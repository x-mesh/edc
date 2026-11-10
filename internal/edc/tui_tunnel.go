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
	// tunnelHostShown은 호스트 후보를 한 줄에 보이는 개수다. 거르는 대상은 후보 전체다.
	tunnelHostShown = 8
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
	// details는 notice 아래에 이어 적을 줄이다. 폼만 여는 화면이 끝난 뒤 터미널에 남긴다.
	details []string
	err     error
	file    string
	specs   map[string]tunnelSpec
}

const (
	tunnelFieldHost = iota
	tunnelFieldKind
	tunnelFieldPort
	tunnelFieldDestination
	tunnelFieldTTL
	tunnelFieldName
	tunnelFieldSave
)

var tunnelKinds = []tunnelKind{tunnelLocal, tunnelReverse, tunnelSocks}

type tunnelForm struct {
	hosts    []string
	hostPick int
	// hostQuery는 사용자가 직접 친 글자다. ←/→는 이 글자가 들어간 후보 안에서만 돈다.
	hostQuery string
	// hostPrefilled는 host가 폼이 미리 채운 첫 후보라는 뜻이다. 이때 첫 입력은 덧붙이지 않고 바꿔 쓴다.
	hostPrefilled bool
	// host에는 호스트 이름 대신 "jump1 db.internal:5432" 같은 한 줄 입력이 올 수 있다. 값을 읽을 때는 expanded를 거친다.
	host        string
	kind        tunnelKind
	port        string
	destination string
	ttl         string
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
	// quick은 edc tunnel up이 목록 없이 폼만 여는 화면이다. 시작에 성공하면 done에 결과를 담고 끝난다.
	quick bool
	done  *tunnelDoneMsg
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

// runTunnelForm은 폼만 열어 터널 하나를 시작하고 끝난다. 결과는 화면이 닫힌 뒤 터미널에 남긴다.
func runTunnelForm(environment tunnelEnvironment, timeout time.Duration, name string, ttl time.Duration, save bool) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := newTunnelScreen(environment, timeout, os.Getenv("NO_COLOR") == "")
	screen.ctx, screen.quick, screen.mode = ctx, true, tunnelModeForm
	screen.form = screen.newForm()
	screen.form.name, screen.form.save = name, save
	if ttl > 0 {
		screen.form.ttl = ttl.String()
	}
	final, err := tea.NewProgram(screen, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if finished, ok := final.(tunnelScreen); ok && finished.done != nil {
		fmt.Println(finished.done.notice)
		for _, line := range finished.done.details {
			fmt.Println(line)
		}
		if finished.done.warning != "" {
			fmt.Fprintln(os.Stderr, finished.done.warning)
		}
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
	if screen.quick {
		return nil
	}
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
		if screen.quick {
			// 시작이 실패하면 입력을 그대로 둔 채 폼에 머문다. 한 번 쓰는 화면이라 돌아갈 목록이 없다.
			if value.err != nil {
				screen.form.problem = value.err.Error()
				return screen, nil
			}
			screen.done = &value
			return screen, tea.Quit
		}
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
	case tea.PasteMsg:
		// 붙여 넣은 글은 KeyPressMsg가 아니라 이 메시지로 온다. 받지 않으면 붙여 넣은 ssh 명령이 사라진다.
		if screen.mode == tunnelModeForm && !(screen.quick && screen.busy) {
			screen.form = screen.form.insert(strings.Join(strings.Fields(value.Content), " "))
			screen.form.problem = ""
		}
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
	case "enter", "r", "x", "s":
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
	case action == "s":
		if row.Defined {
			return screen, nil
		}
		screen.busy, screen.notice = true, ""
		return screen, screen.saveCmd(row.Name)
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
		return screen, screen.startCmd(row.Name, screen.environment.specs[row.Name], "", false, 0)
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

func (screen tunnelScreen) startCmd(name string, spec tunnelSpec, savePath string, save bool, ttl time.Duration) tea.Cmd {
	environment, timeout, base := screen.environment, screen.timeout, screen.baseContext()
	display := screen.displayPath(savePath)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		row, err := environment.manager.start(ctx, name, spec, ttl)
		if err != nil {
			return tunnelDoneMsg{err: err}
		}
		done := tunnelDoneMsg{notice: T("tunnel.up.done", row.Name, tunnelListen(row), tunnelDestinationText(row)),
			details: []string{"  $ " + row.Command}}
		if row.Expires != nil {
			done.details = append(done.details, "  "+tunnelExpiryText(row, environment.manager.now()))
		}
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
			done.details = append(done.details, T("tunnel.up.saved", name, display))
		}
		done.warning = tunnelJoinWarnings(row, name, specs, environment.manager.targets)
		return done
	}
}

// saveCmd는 정의 없이 실행 중인 터널을 정의 파일에 올린다. 터널은 건드리지 않는다.
func (screen tunnelScreen) saveCmd(name string) tea.Cmd {
	environment, savePath := screen.environment, screen.savePath()
	display := screen.displayPath(savePath)
	return func() tea.Msg {
		record, found := environment.manager.store.find(name)
		if !found {
			return tunnelDoneMsg{err: tunnelError("tunnel.error.not_running", name)}
		}
		if err := saveTunnelDefinition(savePath, name, record.definition()); err != nil {
			return tunnelDoneMsg{err: err}
		}
		specs, err := loadTunnelDefinitions(savePath, environment.manager.targets)
		if err != nil {
			return tunnelDoneMsg{err: err}
		}
		return tunnelDoneMsg{notice: T("tunnel.up.saved", name, display), specs: specs, file: savePath}
	}
}

// stopCmd는 터널을 끝낸다. restart면 같은 정의와 같은 유지 시간으로 다시 시작한다.
func (screen tunnelScreen) stopCmd(name string, restart bool) tea.Cmd {
	environment, timeout, base := screen.environment, screen.timeout, screen.baseContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		previous, _ := environment.manager.store.find(name)
		if _, err := environment.manager.down(ctx, name); err != nil {
			return tunnelDoneMsg{err: err}
		}
		if !restart {
			return tunnelDoneMsg{notice: T("tunnel.down.done", name)}
		}
		row, err := environment.manager.start(ctx, name, environment.specs[name], previous.TTL)
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
	// 폼만 여는 화면은 한 번 쓰려는 것이므로 저장하지 않는 쪽이 기본이다.
	form := tunnelForm{hosts: hosts, kind: tunnelLocal, port: tunnelAutoPort, save: !screen.quick}
	if len(hosts) > 0 {
		form.host, form.hostPrefilled = hosts[0], true
	}
	return form
}

// hostMatches는 친 글자가 들어간 후보다. 한 줄 입력을 쓰는 동안에는 고를 후보가 없다.
func (form tunnelForm) hostMatches() []string {
	if strings.ContainsAny(form.host, " \t") {
		return nil
	}
	query := strings.ToLower(form.hostQuery)
	if query == "" {
		return form.hosts
	}
	var matches []string
	for _, host := range form.hosts {
		if strings.Contains(strings.ToLower(host), query) {
			matches = append(matches, host)
		}
	}
	return matches
}

// line은 호스트 칸이 한 줄 입력인지와 그 해석을 돌려준다. 호스트 이름에는 공백이 없으므로 공백이 있으면 한 줄 입력이다.
func (form tunnelForm) line() (tunnelSpec, bool, error) {
	text := strings.TrimSpace(form.host)
	if !strings.ContainsAny(text, " \t") {
		return tunnelSpec{}, false, nil
	}
	spec, err := parseTunnelLine(text)
	return spec, true, err
}

// expanded는 한 줄 입력을 항목별 값으로 푼 폼이다. 한 줄 입력이 아니거나 아직 해석되지 않으면 그대로다.
func (form tunnelForm) expanded() tunnelForm {
	spec, isLine, err := form.line()
	if !isLine || err != nil {
		return form
	}
	form.host, form.hostQuery, form.hostPick, form.hostPrefilled = spec.Host, "", -1, false
	switch {
	case spec.Reverse != "":
		form.kind = tunnelReverse
		form.port, form.destination, _ = strings.Cut(spec.Reverse, ":")
	case spec.Socks != "":
		form.kind, form.port, form.destination = tunnelSocks, string(spec.Socks), ""
	default:
		form.kind, form.port, form.destination = tunnelLocal, string(spec.Local), spec.Remote
	}
	return form
}

// settled는 한 줄 입력을 항목에 옮겨 적는다. 해석하지 못하면 problem에 이유를 적고 false를 돌려준다.
func (form tunnelForm) settled() (tunnelForm, bool) {
	if _, isLine, err := form.line(); isLine && err != nil {
		form.problem = err.Error()
		return form, false
	}
	return form.expanded(), true
}

// insert는 지금 고른 칸에 글자를 넣는다. 키 입력과 붙여 넣기가 함께 쓴다.
func (form tunnelForm) insert(text string) tunnelForm {
	if form.field == tunnelFieldHost && form.hostPrefilled {
		form.host = ""
	}
	target := form.text()
	if target == nil {
		return form
	}
	*target += text
	return form.hostEdited()
}

func (form tunnelForm) hostEdited() tunnelForm {
	if form.field == tunnelFieldHost {
		form.hostQuery, form.hostPick, form.hostPrefilled = form.host, -1, false
	}
	return form
}

// lifetime은 유지 시간 칸의 값이다. 비어 있으면 0이고 터널은 직접 끝낼 때까지 남는다.
func (form tunnelForm) lifetime() (time.Duration, error) {
	text := strings.TrimSpace(form.ttl)
	if text == "" {
		return 0, nil
	}
	duration, err := parseFlagDuration(text)
	if err != nil || duration <= 0 {
		return 0, tunnelError("tunnel.error.ttl_invalid", text)
	}
	return duration, nil
}

// tunnelHostChoices는 inventory의 host 이름과 ~/.ssh/config의 Host 별칭이다. 와일드카드 패턴은 뺀다.
func tunnelHostChoices(targets map[string]string) []string {
	seen := make(map[string]struct{})
	var names []string
	add := func(name string) {
		if _, exists := seen[name]; exists || name == "" || strings.HasPrefix(name, "-") {
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
	return append(fields, tunnelFieldTTL, tunnelFieldName, tunnelFieldSave)
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
	form = form.expanded()
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
	form = form.expanded()
	if _, err := form.lifetime(); err != nil {
		return err
	}
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
	case tunnelFieldTTL:
		return &form.ttl
	case tunnelFieldName:
		return &form.name
	}
	return nil
}

func (screen tunnelScreen) updateForm(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	form := screen.form
	if screen.quick && screen.busy {
		if key.String() == "ctrl+c" {
			return screen, tea.Quit
		}
		return screen, nil
	}
	switch key.String() {
	case "esc", "ctrl+c":
		if screen.quick {
			return screen, tea.Quit
		}
		screen.mode = tunnelModeList
		return screen, nil
	case "tab", "down", "shift+tab", "up":
		settled, ok := form.settled()
		if !ok {
			screen.form = settled
			return screen, nil
		}
		step := 1
		if key.String() == "shift+tab" || key.String() == "up" {
			step = -1
		}
		form = settled.moveField(step)
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
			if matches := form.hostMatches(); len(matches) > 0 {
				if form.hostPick < 0 && step < 0 {
					form.hostPick = 0
				}
				form.hostPick = (form.hostPick + step + len(matches)) % len(matches)
				form.host, form.hostPrefilled = matches[form.hostPick], false
			}
		}
	case "backspace":
		if text := form.text(); text != nil {
			runes := []rune(*text)
			if len(runes) > 0 {
				*text = string(runes[:len(runes)-1])
			}
			form = form.hostEdited()
		}
	case "ctrl+u":
		if text := form.text(); text != nil {
			*text = ""
			form = form.hostEdited()
		}
	case "ctrl+t":
		return screen.startCheck(form)
	case "enter":
		return screen.submitForm(form)
	default:
		if key.Text != "" {
			form = form.insert(key.Text)
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
	form, ok := form.settled()
	if !ok {
		screen.form = form
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
	form, ok := form.settled()
	if !ok {
		screen.form = form
		return screen, nil
	}
	save := form.save
	name, spec := form.spec()
	ttl, _ := form.lifetime()
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
	screen.busy, screen.notice, screen.form = true, "", form
	if !screen.quick {
		screen.mode = tunnelModeList
	}
	return screen, screen.startCmd(name, spec, savePath, save, ttl)
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
	if row, ok := screen.selected(); ok {
		if row.Defined {
			hints = append(hints, T("tunnel.hint.restart"), T("tunnel.hint.remove"))
		} else {
			hints = append(hints, T("tunnel.hint.save"))
		}
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
		T("tunnel.column.host"), T("tunnel.column.state"), T("tunnel.column.reach"), T("tunnel.column.uptime"), T("tunnel.column.expires"), T("tunnel.column.saved")}
	cells := make([][]string, 0, len(screen.rows)+1)
	cells = append(cells, headers)
	for _, row := range screen.rows {
		destination := row.Destination
		if destination == "" {
			destination = "-"
		}
		cells = append(cells, []string{row.Name, string(row.Kind), tunnelListen(row), destination, row.Host,
			string(row.State), row.Reach, tunnelUptime(row.Started, screen.now()), tunnelRemaining(row.Expires, screen.now()), tunnelSavedMark(row.Defined)})
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
	// 호스트 칸은 친 글자를 그대로 보이고, 나머지 칸은 한 줄 입력을 푼 값을 보인다.
	typed := screen.form.host
	form := screen.form.expanded()
	var builder strings.Builder
	builder.WriteString(liveTitleHints(T("tunnel.screen.form_title"), screen.color) + "\n\n")
	portLabel, destinationLabel := T("tunnel.form.local_port"), T("tunnel.form.destination")
	if form.kind == tunnelReverse {
		portLabel, destinationLabel = T("tunnel.form.remote_port"), T("tunnel.form.local_target")
	}
	labels := map[int]string{
		tunnelFieldHost: T("tunnel.form.host"), tunnelFieldKind: T("tunnel.form.kind"), tunnelFieldPort: portLabel,
		tunnelFieldDestination: destinationLabel, tunnelFieldTTL: T("tunnel.form.ttl"), tunnelFieldName: T("tunnel.form.name"),
		tunnelFieldSave: T("tunnel.form.save"),
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
			value = typed
		case tunnelFieldTTL:
			value = form.ttl
			if value == "" {
				hint := liveMuted(T("tunnel.form.hint_ttl"), screen.color)
				if field == form.field {
					hint = "▏" + hint
				}
				builder.WriteString(bar + " " + screen.fieldLabel(form, field, labels[field], width) + "  " + hint + "\n")
				continue
			}
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
	if screen.quick && screen.busy {
		builder.WriteString("\n  " + liveMuted(T("tunnel.form.starting"), screen.color) + "\n")
	}
	return builder.String()
}

// hostCandidates는 호스트 칸 아래에 고를 수 있는 후보를 한 줄로 보인다. 지금 값과 같은 후보는 선택 행처럼 칠한다.
func (screen tunnelScreen) hostCandidates(form tunnelForm) string {
	matches := form.hostMatches()
	if len(matches) == 0 {
		return ""
	}
	first := min(max(form.hostPick-tunnelHostShown+1, 0), max(len(matches)-tunnelHostShown, 0))
	shown := matches[first:min(first+tunnelHostShown, len(matches))]
	parts := make([]string, len(shown))
	for index, host := range shown {
		parts[index] = liveMuted(host, screen.color)
		switch {
		case host != form.host:
		case liveRich(screen.color):
			parts[index] = liveSelected(" "+host+" ", true)
		default:
			parts[index] = "[" + host + "]"
		}
	}
	line := strings.Join(parts, "  ")
	if hidden := len(matches) - len(shown); hidden > 0 {
		line += liveMuted(fmt.Sprintf("  +%d", hidden), screen.color)
	}
	return line
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
	if _, isLine, err := form.line(); isLine && err != nil {
		return "  " + liveMuted(err.Error(), screen.color)
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
	case tunnelFieldTTL:
		lines = TList("tunnel.help.ttl")
	case tunnelFieldName:
		lines = TList("tunnel.help.name")
	case tunnelFieldSave:
		lines = TList("tunnel.help.save")
	}
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString("  " + liveMuted(line, screen.color) + "\n")
	}
	if form.field == tunnelFieldHost {
		if candidates := screen.hostCandidates(screen.form); candidates != "" {
			builder.WriteString("\n  " + candidates + "\n")
		}
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
	form = form.expanded()
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
