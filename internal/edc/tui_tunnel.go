package edc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	tunnelRefreshInterval = 2 * time.Second
	tunnelHostCandidates  = 50
)

type tunnelMode int

const (
	tunnelModeList tunnelMode = iota
	tunnelModeForm
	tunnelModeConfirm
)

type tunnelRowsMsg []tunnelRow

type tunnelTickMsg struct{}

// tunnelCheckMsg는 연결 시험의 결과다. signature는 시험한 입력이라, 결과가 오는 사이 입력이 바뀌었는지 가린다.
type tunnelCheckMsg struct {
	signature string
	checks    []tunnelCheck
	err       error
}

// tunnelDoneMsg는 시작, 종료, 저장 같은 동작의 결과다. specs가 있으면 정의 파일을 다시 읽은 값이다.
type tunnelDoneMsg struct {
	notice  string
	warning string
	err     error
	file    string
	specs   map[string]tunnelSpec
}

const (
	tunnelFieldHost = iota
	tunnelFieldKind
	tunnelFieldPort
	tunnelFieldDestination
	tunnelFieldName
	tunnelFieldSave
)

var tunnelKinds = []tunnelKind{tunnelLocal, tunnelReverse, tunnelSocks}

type tunnelForm struct {
	hosts       []string
	hostPick    int
	host        string
	kind        tunnelKind
	port        string
	destination string
	name        string
	save        bool
	field       int
	problem     string
	checking    bool
	checks      []tunnelCheck
	checkedFor  string
}

type tunnelScreen struct {
	// ctx는 화면이 끝나면 취소되어, 아직 도는 ssh 명령도 함께 멈춘다. 비어 있으면 취소 없이 쓴다.
	ctx         context.Context
	environment tunnelEnvironment
	timeout     time.Duration
	color       bool
	mode        tunnelMode
	rows        []tunnelRow
	cursor      int
	form        tunnelForm
	notice      string
	warning     string
	failed      bool
	busy        bool
	now         func() time.Time
}

func runTunnelScreen(environment tunnelEnvironment, timeout time.Duration) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := newTunnelScreen(environment, timeout, os.Getenv("NO_COLOR") == "")
	screen.ctx = ctx
	if _, err := tea.NewProgram(screen, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func (screen tunnelScreen) baseContext() context.Context {
	if screen.ctx != nil {
		return screen.ctx
	}
	return context.Background()
}

func newTunnelScreen(environment tunnelEnvironment, timeout time.Duration, color bool) tunnelScreen {
	return tunnelScreen{environment: environment, timeout: timeout, color: color, now: time.Now}
}

func (screen tunnelScreen) Init() tea.Cmd {
	return tea.Batch(screen.refresh(), screen.tick())
}

func (screen tunnelScreen) tick() tea.Cmd {
	return tea.Tick(tunnelRefreshInterval, func(time.Time) tea.Msg { return tunnelTickMsg{} })
}

func (screen tunnelScreen) refresh() tea.Cmd {
	environment, timeout, base := screen.environment, screen.timeout, screen.baseContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		return tunnelRowsMsg(environment.manager.rows(ctx, environment.specs))
	}
}

func (screen tunnelScreen) selected() (tunnelRow, bool) {
	if screen.cursor < 0 || screen.cursor >= len(screen.rows) {
		return tunnelRow{}, false
	}
	return screen.rows[screen.cursor], true
}

func (screen tunnelScreen) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tunnelRowsMsg:
		screen.rows = value
		screen.cursor = min(screen.cursor, max(len(screen.rows)-1, 0))
	case tunnelTickMsg:
		if screen.busy || screen.mode != tunnelModeList {
			return screen, screen.tick()
		}
		return screen, tea.Batch(screen.refresh(), screen.tick())
	case tunnelCheckMsg:
		screen.form.checking = false
		screen.form.checks, screen.form.checkedFor = value.checks, value.signature
		if value.err != nil {
			screen.form.problem = value.err.Error()
		}
	case tunnelDoneMsg:
		screen.busy = false
		screen.notice, screen.warning, screen.failed = value.notice, value.warning, value.err != nil
		if value.err != nil {
			screen.notice = value.err.Error()
		}
		if value.specs != nil {
			screen.environment.specs = value.specs
		}
		if value.file != "" {
			screen.environment.file = value.file
		}
		screen.mode = tunnelModeList
		return screen, screen.refresh()
	case tea.KeyPressMsg:
		switch screen.mode {
		case tunnelModeForm:
			return screen.updateForm(value)
		case tunnelModeConfirm:
			return screen.updateConfirm(value)
		default:
			return screen.updateList(value)
		}
	}
	return screen, nil
}

func (screen tunnelScreen) updateList(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "esc", "ctrl+c":
		return screen, tea.Quit
	case "up", "k":
		if len(screen.rows) > 0 {
			screen.cursor = (screen.cursor - 1 + len(screen.rows)) % len(screen.rows)
		}
	case "down", "j":
		if len(screen.rows) > 0 {
			screen.cursor = (screen.cursor + 1) % len(screen.rows)
		}
	case "n":
		screen.form = screen.newForm()
		screen.mode, screen.notice, screen.warning = tunnelModeForm, "", ""
	case "enter", "r", "x":
		row, ok := screen.selected()
		if !ok || screen.busy {
			return screen, nil
		}
		return screen.act(key.String(), row)
	}
	return screen, nil
}

func (screen tunnelScreen) act(action string, row tunnelRow) (tea.Model, tea.Cmd) {
	running := row.State == tunnelStateUp || row.State == tunnelStateStale
	switch {
	case action == "x":
		if !row.Defined {
			screen.notice, screen.failed = T("tunnel.notice.not_defined", row.Name), true
			return screen, nil
		}
		screen.mode = tunnelModeConfirm
		return screen, nil
	case action == "enter" && running:
		screen.busy, screen.notice = true, ""
		return screen, screen.stopCmd(row.Name, false)
	case !row.Defined:
		screen.notice, screen.failed = T("tunnel.notice.not_defined", row.Name), true
		return screen, nil
	case action == "r" && running:
		screen.busy, screen.notice = true, ""
		return screen, screen.stopCmd(row.Name, true)
	default:
		screen.busy, screen.notice = true, ""
		return screen, screen.startCmd(row.Name, screen.environment.specs[row.Name], "", false)
	}
}

func (screen tunnelScreen) updateConfirm(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	row, ok := screen.selected()
	switch key.String() {
	case "y":
		screen.mode = tunnelModeList
		if !ok {
			return screen, nil
		}
		screen.busy = true
		return screen, screen.removeCmd(row.Name)
	case "n", "esc", "q", "ctrl+c":
		screen.mode = tunnelModeList
	}
	return screen, nil
}

func (screen tunnelScreen) startCmd(name string, spec tunnelSpec, savePath string, save bool) tea.Cmd {
	environment, timeout, base := screen.environment, screen.timeout, screen.baseContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		row, err := environment.manager.up(ctx, name, spec)
		if err != nil {
			return tunnelDoneMsg{err: err}
		}
		done := tunnelDoneMsg{notice: T("tunnel.up.done", row.Name, tunnelListen(row), tunnelDestinationText(row))}
		specs := environment.specs
		// 시작에 성공한 뒤에만 저장한다. 시작이 실패했는데 정의만 남는 일을 막는다.
		if save {
			if err := saveTunnelDefinition(savePath, name, spec); err != nil {
				return tunnelDoneMsg{err: tunnelError("tunnel.error.save_after_start", name, err.Error())}
			}
			reloaded, err := loadTunnelDefinitions(savePath, environment.manager.targets)
			if err != nil {
				return tunnelDoneMsg{err: tunnelError("tunnel.error.save_after_start", name, err.Error())}
			}
			done.specs, done.file, specs = reloaded, savePath, reloaded
		}
		done.warning = tunnelJoinWarnings(row, name, specs, environment.manager.targets)
		return done
	}
}

// stopCmd는 터널을 끝낸다. restart면 같은 정의로 다시 시작한다.
func (screen tunnelScreen) stopCmd(name string, restart bool) tea.Cmd {
	environment, timeout, base := screen.environment, screen.timeout, screen.baseContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		if _, err := environment.manager.down(ctx, name); err != nil {
			return tunnelDoneMsg{err: err}
		}
		if !restart {
			return tunnelDoneMsg{notice: T("tunnel.down.done", name)}
		}
		row, err := environment.manager.up(ctx, name, environment.specs[name])
		if err != nil {
			return tunnelDoneMsg{err: err}
		}
		return tunnelDoneMsg{notice: T("tunnel.up.done", row.Name, tunnelListen(row), tunnelDestinationText(row)),
			warning: tunnelJoinWarnings(row, name, environment.specs, environment.manager.targets)}
	}
}

func (screen tunnelScreen) removeCmd(name string) tea.Cmd {
	environment := screen.environment
	return func() tea.Msg {
		if err := removeTunnelDefinition(environment.file, name); err != nil {
			return tunnelDoneMsg{err: err}
		}
		specs, err := loadTunnelDefinitions(environment.file, environment.manager.targets)
		if err != nil {
			return tunnelDoneMsg{err: err}
		}
		return tunnelDoneMsg{notice: T("tunnel.notice.removed", name), specs: specs}
	}
}

func tunnelSpecsAfter(current, reloaded map[string]tunnelSpec) map[string]tunnelSpec {
	if reloaded != nil {
		return reloaded
	}
	return current
}

func tunnelJoinWarnings(row tunnelRow, name string, specs map[string]tunnelSpec, targets map[string]string) string {
	warnings := append([]string(nil), row.Warnings...)
	warnings = append(warnings, tunnelSharedWarnings(name, specs, targets)...)
	return strings.Join(warnings, "  ")
}

func (screen tunnelScreen) newForm() tunnelForm {
	hosts := tunnelHostChoices(screen.environment.manager.targets)
	form := tunnelForm{hosts: hosts, kind: tunnelLocal, port: tunnelAutoPort, save: true}
	if len(hosts) > 0 {
		form.host = hosts[0]
	}
	return form
}

// tunnelHostChoices는 inventory의 host 이름과 ~/.ssh/config의 Host 별칭이다. 와일드카드 패턴은 뺀다.
func tunnelHostChoices(targets map[string]string) []string {
	seen := make(map[string]struct{})
	var names []string
	add := func(name string) {
		if _, exists := seen[name]; exists || name == "" || strings.HasPrefix(name, "-") || len(names) >= tunnelHostCandidates {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	inventory := make([]string, 0, len(targets))
	for name := range targets {
		inventory = append(inventory, name)
	}
	sort.Strings(inventory)
	for _, name := range inventory {
		add(name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return names
	}
	file, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return names
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "host") {
			continue
		}
		for _, name := range fields[1:] {
			if !strings.ContainsAny(name, "*?!") {
				add(name)
			}
		}
	}
	return names
}

func (form tunnelForm) visibleFields() []int {
	fields := []int{tunnelFieldHost, tunnelFieldKind, tunnelFieldPort}
	if form.kind != tunnelSocks {
		fields = append(fields, tunnelFieldDestination)
	}
	return append(fields, tunnelFieldName, tunnelFieldSave)
}

func (form tunnelForm) moveField(step int) tunnelForm {
	fields := form.visibleFields()
	index := 0
	for position, field := range fields {
		if field == form.field {
			index = position
		}
	}
	form.field = fields[(index+step+len(fields))%len(fields)]
	return form
}

func (form tunnelForm) spec() (string, tunnelSpec) {
	port := strings.TrimSpace(form.port)
	destination := strings.TrimSpace(form.destination)
	spec := tunnelSpec{Host: strings.TrimSpace(form.host)}
	switch form.kind {
	case tunnelReverse:
		spec.Reverse = port + ":" + destination
	case tunnelSocks:
		spec.Socks = tunnelPort(port)
	default:
		spec.Local, spec.Remote = tunnelPort(port), destination
	}
	name := strings.TrimSpace(form.name)
	if name == "" {
		name = tunnelAdhocName(spec.Host, spec)
	}
	return name, spec
}

func (form tunnelForm) check() error {
	if form.kind != tunnelSocks && strings.TrimSpace(form.destination) == "" {
		return tunnelError("tunnel.error.destination_required")
	}
	return nil
}

func (form *tunnelForm) text() *string {
	switch form.field {
	case tunnelFieldHost:
		return &form.host
	case tunnelFieldPort:
		return &form.port
	case tunnelFieldDestination:
		return &form.destination
	case tunnelFieldName:
		return &form.name
	}
	return nil
}

func (screen tunnelScreen) updateForm(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	form := screen.form
	switch key.String() {
	case "esc", "ctrl+c":
		screen.mode = tunnelModeList
		return screen, nil
	case "tab", "down":
		form = form.moveField(1)
	case "shift+tab", "up":
		form = form.moveField(-1)
	case "left", "right":
		step := 1
		if key.String() == "left" {
			step = -1
		}
		switch form.field {
		case tunnelFieldKind:
			index := 0
			for position, kind := range tunnelKinds {
				if kind == form.kind {
					index = position
				}
			}
			form.kind = tunnelKinds[(index+step+len(tunnelKinds))%len(tunnelKinds)]
			// reverse의 원격 포트는 auto를 쓸 수 없으므로 종류를 바꿀 때 기본값을 맞춘다.
			switch {
			case form.kind == tunnelReverse && form.port == tunnelAutoPort:
				form.port = ""
			case form.kind != tunnelReverse && strings.TrimSpace(form.port) == "":
				form.port = tunnelAutoPort
			}
		case tunnelFieldSave:
			form.save = !form.save
		case tunnelFieldHost:
			if len(form.hosts) > 0 {
				form.hostPick = (form.hostPick + step + len(form.hosts)) % len(form.hosts)
				form.host = form.hosts[form.hostPick]
			}
		}
	case "backspace":
		if text := form.text(); text != nil {
			runes := []rune(*text)
			if len(runes) > 0 {
				*text = string(runes[:len(runes)-1])
			}
		}
	case "ctrl+u":
		if text := form.text(); text != nil {
			*text = ""
		}
	case "ctrl+t":
		return screen.startCheck(form)
	case "enter":
		return screen.submitForm(form)
	default:
		if text := form.text(); text != nil && key.Text != "" {
			*text += key.Text
		}
	}
	form.problem = ""
	screen.form = form
	return screen, nil
}

func (form tunnelForm) signature() string {
	name, spec := form.spec()
	return fmt.Sprintf("%s|%+v", name, spec)
}

// startCheck는 폼의 입력으로 연결 시험을 시작한다. 시험은 호스트에 접속하므로 화면 밖에서 돌고, 결과는 tunnelCheckMsg로 온다.
func (screen tunnelScreen) startCheck(form tunnelForm) (tea.Model, tea.Cmd) {
	if form.checking {
		return screen, nil
	}
	name, spec := form.spec()
	err := form.check()
	if err == nil {
		_, err = buildTunnelPlan(name, spec, screen.environment.manager.targets, func() (int, error) { return 0, nil })
	}
	if err != nil {
		form.problem = err.Error()
		screen.form = form
		return screen, nil
	}
	form.checking, form.checks, form.problem = true, nil, ""
	screen.form = form
	manager, signature, base, timeout := screen.environment.manager, form.signature(), screen.baseContext(), screen.timeout
	return screen, func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		checks, err := manager.check(ctx, name, spec)
		return tunnelCheckMsg{signature: signature, checks: checks, err: err}
	}
}

func (screen tunnelScreen) submitForm(form tunnelForm) (tea.Model, tea.Cmd) {
	save := form.save
	name, spec := form.spec()
	err := form.check()
	if err == nil {
		err = screen.duplicateProblem(form)
	}
	if err == nil {
		_, err = buildTunnelPlan(name, spec, screen.environment.manager.targets, func() (int, error) { return 0, nil })
	}
	if err != nil {
		form.problem = err.Error()
		screen.form = form
		return screen, nil
	}
	savePath := screen.savePath()
	screen.busy, screen.mode, screen.notice = true, tunnelModeList, ""
	return screen, screen.startCmd(name, spec, savePath, save)
}

// savePath는 폼이 저장할 정의 파일이다. 찾은 파일이 없으면 현재 디렉터리의 ./.edc/tunnels.yaml을 새로 만든다.
func (screen tunnelScreen) savePath() string {
	if screen.environment.file != "" {
		return screen.environment.file
	}
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Join(remoteProjectDirectory, tunnelFileName)
	}
	return defaultTunnelFile(cwd)
}

func (screen tunnelScreen) displayPath(path string) string {
	if cwd, err := os.Getwd(); err == nil {
		return remoteDisplayPath(cwd, path)
	}
	return path
}

// duplicateProblem은 저장하려는 이름이 이미 정의에 있을 때의 오류다. 덮어쓰지 않고 폼에서 바로 알린다.
func (screen tunnelScreen) duplicateProblem(form tunnelForm) error {
	if !form.save {
		return nil
	}
	name, _ := form.spec()
	if _, exists := screen.environment.specs[name]; exists {
		return tunnelError("tunnel.error.duplicate", name, screen.displayPath(screen.savePath()))
	}
	return nil
}

func (screen tunnelScreen) paint(text, color string) string {
	if !liveRich(screen.color) {
		return text
	}
	return richPaint(text, color, false)
}

func (screen tunnelScreen) stateCell(state tunnelState, text string) string {
	switch state {
	case tunnelStateUp:
		return screen.paint(text, richGreen)
	case tunnelStateStale:
		return screen.paint(text, richYellow)
	}
	if !liveRich(screen.color) {
		return text
	}
	return richDim(text)
}

func (screen tunnelScreen) View() tea.View {
	var builder strings.Builder
	switch screen.mode {
	case tunnelModeForm:
		builder.WriteString(screen.formView())
	default:
		builder.WriteString(screen.listView())
	}
	view := tea.NewView(builder.String())
	view.AltScreen = true
	return view
}

func (screen tunnelScreen) listView() string {
	var builder strings.Builder
	builder.WriteString(liveTitleHints(screen.listTitle(), screen.color) + "\n\n")
	if len(screen.rows) == 0 {
		builder.WriteString("  " + liveMuted(T("tunnel.list.empty"), screen.color) + "\n")
	} else {
		builder.WriteString(screen.table())
	}
	builder.WriteString("\n")
	if row, ok := screen.selected(); ok && row.Command != "" {
		builder.WriteString("  " + liveMuted("$ "+row.Command, screen.color) + "\n\n")
	}
	switch {
	case screen.mode == tunnelModeConfirm:
		if row, ok := screen.selected(); ok {
			builder.WriteString("  " + liveKeyHints(T("tunnel.screen.confirm_remove", row.Name), screen.color) + "\n")
		}
	case screen.notice != "":
		color := richGreen
		if screen.failed {
			color = richRed
		}
		builder.WriteString("  " + screen.paint(screen.notice, color) + "\n")
		if screen.warning != "" {
			builder.WriteString("  " + screen.paint(screen.warning, richYellow) + "\n")
		}
	}
	return builder.String()
}

// listTitle은 선택한 줄에서 쓸 수 있는 키만 안내한다. 정의가 없는 일회용 터널에는 재시작과 정의 삭제가 없다.
func (screen tunnelScreen) listTitle() string {
	hints := []string{T("tunnel.hint.toggle"), T("tunnel.hint.new")}
	if row, ok := screen.selected(); ok && row.Defined {
		hints = append(hints, T("tunnel.hint.restart"), T("tunnel.hint.remove"))
	}
	hints = append(hints, T("tunnel.hint.quit"))
	return T("tunnel.screen.name") + "   " + strings.Join(hints, "   ")
}

func tunnelSavedMark(defined bool) string {
	if defined {
		return "●"
	}
	return "–"
}

func (screen tunnelScreen) table() string {
	headers := []string{T("tunnel.column.name"), T("tunnel.column.kind"), T("tunnel.column.listen"), T("tunnel.column.destination"),
		T("tunnel.column.host"), T("tunnel.column.state"), T("tunnel.column.reach"), T("tunnel.column.uptime"), T("tunnel.column.saved")}
	cells := make([][]string, 0, len(screen.rows)+1)
	cells = append(cells, headers)
	for _, row := range screen.rows {
		destination := row.Destination
		if destination == "" {
			destination = "-"
		}
		cells = append(cells, []string{row.Name, string(row.Kind), tunnelListen(row), destination, row.Host,
			string(row.State), row.Reach, tunnelUptime(row.Started, screen.now()), tunnelSavedMark(row.Defined)})
	}
	widths := make([]int, len(headers))
	for _, line := range cells {
		for column, cell := range line {
			widths[column] = max(widths[column], liveWidth(cell))
		}
	}
	rowWidth := liveWidth(liveSelectedBar) + 1 + len(widths)*2
	for _, width := range widths {
		rowWidth += width
	}
	var builder strings.Builder
	for index, line := range cells {
		if index == 0 {
			plain := liveIdleBar + " " + joinTunnelCells(line, widths, nil)
			builder.WriteString(liveMuted(plain, screen.color) + "\n")
			continue
		}
		row := screen.rows[index-1]
		if index-1 == screen.cursor {
			plain := liveSelectedBar + " " + joinTunnelCells(line, widths, nil)
			builder.WriteString(liveSelected(liveCell(plain, rowWidth), screen.color) + "\n")
			continue
		}
		paint := func(column int, padded string) string {
			switch column {
			case 5:
				return screen.stateCell(row.State, padded)
			case 6:
				if row.Reach == tunnelReachRefused {
					return screen.paint(padded, richRed)
				}
			}
			return padded
		}
		builder.WriteString(liveIdleBar + " " + joinTunnelCells(line, widths, paint) + "\n")
	}
	return builder.String()
}

func joinTunnelCells(line []string, widths []int, paint func(int, string) string) string {
	parts := make([]string, len(line))
	for column, cell := range line {
		parts[column] = liveCell(cell, widths[column])
		if paint != nil {
			parts[column] = paint(column, parts[column])
		}
	}
	return strings.Join(parts, "  ")
}

func (screen tunnelScreen) formView() string {
	form := screen.form
	var builder strings.Builder
	builder.WriteString(liveTitleHints(T("tunnel.screen.form_title"), screen.color) + "\n\n")
	portLabel, destinationLabel := T("tunnel.form.local_port"), T("tunnel.form.destination")
	if form.kind == tunnelReverse {
		portLabel, destinationLabel = T("tunnel.form.remote_port"), T("tunnel.form.local_target")
	}
	labels := map[int]string{
		tunnelFieldHost: T("tunnel.form.host"), tunnelFieldKind: T("tunnel.form.kind"), tunnelFieldPort: portLabel,
		tunnelFieldDestination: destinationLabel, tunnelFieldName: T("tunnel.form.name"), tunnelFieldSave: T("tunnel.form.save"),
	}
	width := 0
	for _, label := range labels {
		width = max(width, liveWidth(label))
	}
	for _, field := range form.visibleFields() {
		bar := liveIdleBar
		if field == form.field {
			bar = liveSelectedBar
		}
		var value string
		switch field {
		case tunnelFieldKind:
			value = screen.kindChoices(form)
		case tunnelFieldSave:
			value = screen.saveChoices(form)
		case tunnelFieldHost:
			value = form.host
		case tunnelFieldPort:
			value = form.port
			if value == "" {
				value = liveMuted("8080", screen.color)
				if field == form.field {
					value = "▏" + value
				}
				builder.WriteString(bar + " " + screen.fieldLabel(form, field, labels[field], width) + "  " + value + "\n")
				continue
			}
		case tunnelFieldDestination:
			value = form.destination
			if value == "" {
				hint := T("tunnel.form.hint_destination")
				if form.kind == tunnelReverse {
					hint = T("tunnel.form.hint_local_target")
				}
				hint = liveMuted(hint, screen.color)
				if field == form.field {
					hint = "▏" + hint
				}
				builder.WriteString(bar + " " + screen.fieldLabel(form, field, labels[field], width) + "  " + hint + "\n")
				continue
			}
		case tunnelFieldName:
			value = form.name
			if value == "" {
				value = liveMuted(screen.defaultName(form), screen.color)
			}
		}
		if field == form.field && field != tunnelFieldKind && field != tunnelFieldSave {
			value += "▏"
		}
		builder.WriteString(bar + " " + screen.fieldLabel(form, field, labels[field], width) + "  " + value + "\n")
	}
	builder.WriteString("  " + liveMuted(screen.saveTarget(form), screen.color) + "\n")
	builder.WriteString("\n" + screen.helpView(form) + "\n" + screen.previewView(form) + "\n")
	builder.WriteString(screen.checksView(form))
	return builder.String()
}

func (screen tunnelScreen) saveChoices(form tunnelForm) string {
	choices := []struct {
		on    bool
		label string
	}{{true, T("tunnel.form.yes")}, {false, T("tunnel.form.no")}}
	parts := make([]string, len(choices))
	for index, choice := range choices {
		parts[index] = choice.label
		switch {
		case choice.on != form.save:
		case liveRich(screen.color):
			parts[index] = liveSelected(" "+choice.label+" ", true)
		default:
			parts[index] = "[" + choice.label + "]"
		}
	}
	return strings.Join(parts, " ")
}

// saveTarget은 저장 선택 아래에 저장될 곳, 또는 저장되지 않는다는 사실을 한 줄로 보인다.
func (screen tunnelScreen) saveTarget(form tunnelForm) string {
	if form.save {
		return T("tunnel.form.save_to", screen.displayPath(screen.savePath()))
	}
	return T("tunnel.form.save_none")
}

func (screen tunnelScreen) fieldLabel(form tunnelForm, field int, text string, width int) string {
	label := liveCell(text, width)
	if field == form.field && liveRich(screen.color) {
		label = richBold(label)
	}
	return label
}

func (screen tunnelScreen) kindChoices(form tunnelForm) string {
	parts := make([]string, len(tunnelKinds))
	for index, kind := range tunnelKinds {
		parts[index] = string(kind)
		switch {
		case kind != form.kind:
		case liveRich(screen.color):
			parts[index] = liveSelected(" "+string(kind)+" ", true)
		default:
			parts[index] = "[" + string(kind) + "]"
		}
	}
	return strings.Join(parts, " ")
}

func (screen tunnelScreen) defaultName(form tunnelForm) string {
	_, spec := form.spec()
	return tunnelAdhocName(spec.Host, spec)
}

// previewView는 지금 입력으로 실행될 ssh 명령과, 시작하기 전에 알 수 있는 문제를 보인다.
func (screen tunnelScreen) previewView(form tunnelForm) string {
	if form.problem != "" {
		return "  " + screen.paint(form.problem, richRed)
	}
	name, spec := form.spec()
	if err := screen.duplicateProblem(form); err != nil {
		return "  " + screen.paint(err.Error(), richRed)
	}
	err := form.check()
	var plan tunnelPlan
	if err == nil {
		plan, err = buildTunnelPlan(name, spec, screen.environment.manager.targets, func() (int, error) { return 0, nil })
	}
	if err != nil {
		return "  " + liveMuted(err.Error(), screen.color)
	}
	command := plan.Command()
	if plan.Kind != tunnelReverse && plan.Port > 0 {
		if err := screen.environment.manager.available(plan.Port); err != nil {
			return "  " + liveMuted("$ "+command, screen.color) + "\n  " + screen.paint(err.Error(), richYellow)
		}
	}
	return "  " + liveMuted("$ "+command, screen.color)
}

// helpView는 지금 고르는 항목에 무엇을 넣고 어떻게 고르는지 설명한다. 아래 흐름도는 입력한 값으로 트래픽이 지나는 길을 보인다.
func (screen tunnelScreen) helpView(form tunnelForm) string {
	var lines []string
	switch form.field {
	case tunnelFieldHost:
		lines = TList("tunnel.help.host")
	case tunnelFieldKind:
		for _, kind := range tunnelKinds {
			marker := "  "
			if kind == form.kind {
				marker = liveSelectedBar + " "
			}
			lines = append(lines, marker+T("tunnel.help.kind."+string(kind)))
		}
	case tunnelFieldPort:
		lines = TList("tunnel.help.port." + string(form.kind))
	case tunnelFieldDestination:
		lines = TList("tunnel.help.destination." + string(form.kind))
	case tunnelFieldName:
		lines = TList("tunnel.help.name")
	case tunnelFieldSave:
		lines = TList("tunnel.help.save")
	}
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString("  " + liveMuted(line, screen.color) + "\n")
	}
	builder.WriteString("\n  " + screen.diagram(form) + "\n")
	return builder.String()
}

func (screen tunnelScreen) diagram(form tunnelForm) string {
	orPlaceholder := func(value, placeholder string) string {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
		return placeholder
	}
	host, port := orPlaceholder(form.host, "<host>"), orPlaceholder(form.port, "<port>")
	destination := orPlaceholder(form.destination, "<host:port>")
	switch form.kind {
	case tunnelReverse:
		return T("tunnel.diagram.reverse", host, port, destination)
	case tunnelSocks:
		return T("tunnel.diagram.socks", port, host)
	default:
		return T("tunnel.diagram.local", port, host, destination)
	}
}

// checksView는 연결 시험 결과를 보인다. 시험 뒤에 입력이 바뀌었으면 결과를 흐리게 하고 그 사실을 알린다.
func (screen tunnelScreen) checksView(form tunnelForm) string {
	if form.checking {
		return "\n  " + liveMuted(T("tunnel.check.running"), screen.color) + "\n"
	}
	if len(form.checks) == 0 {
		return ""
	}
	stale := form.checkedFor != form.signature()
	var builder strings.Builder
	builder.WriteString("\n")
	for _, check := range form.checks {
		line := "  " + tunnelCheckLine(check)
		switch {
		case stale:
			line = liveMuted(line, screen.color)
		case check.Status == tunnelCheckPass:
			line = screen.paint(line, richGreen)
		case check.Status == tunnelCheckFail:
			line = screen.paint(line, richRed)
		case check.Status == tunnelCheckWarn:
			line = screen.paint(line, richYellow)
		default:
			line = liveMuted(line, screen.color)
		}
		builder.WriteString(line + "\n")
	}
	if stale {
		builder.WriteString("  " + liveMuted(T("tunnel.check.stale"), screen.color) + "\n")
	} else {
		builder.WriteString("  " + liveMuted(tunnelCheckSummary(form.checks), screen.color) + "\n")
	}
	return builder.String()
}
