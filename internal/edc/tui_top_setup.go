package edc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// runTopSetup은 edc top setup이다. 설정을 고르는 동안 아래에 실제 대시보드를 그리고, 저장하면 [defaults.top]만 바꾼다.
// 파일 쓰기는 화면을 닫은 뒤에 한다. edc setup처럼 시작할 때 읽은 파일이 그 사이에 바뀌었으면 쓰지 않는다.
func runTopSetup(args []string, version string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc top setup"))
		return 2
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, T("cli.setup.terminal_required"))
		return 2
	}
	path := configPath()
	if path == "" {
		fmt.Fprintln(os.Stderr, T("cli.setup.path_unavailable"))
		return 2
	}
	snapshot, err := configFileSnapshot(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.setup.input_failed", err))
		return 2
	}
	config, err := loadConfigAt(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.config.invalid", configError(configReadPath(path), err)))
		return 2
	}
	interfaces, err := parseTopInterfaceFilter(configuredString(config.Defaults.Top.Interface, ""))
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.interface_invalid", err))
		return 2
	}
	if interfaces.physical && !topPhysicalSupported {
		interfaces = interfaces.withoutPhysical()
	}
	settings := topSettingsFrom(config.Defaults.Top)
	preview, err := newTopDashboardModel(settings.interval, version, topProcessFilter{}, interfaces)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	final, err := tea.NewProgram(newTopSetupModel(preview, config, path), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	setup, ok := final.(topSetupModel)
	if ok {
		setup.preview.closeAI()
	}
	if err != nil || !ok {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !setup.save {
		if setup.dirty() {
			fmt.Fprintln(os.Stderr, T("cli.setup.cancelled"))
			return 4
		}
		return 0
	}
	data, err := setup.configData()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := writeConfigIfUnchanged(path, data, snapshot); err != nil {
		if errors.Is(err, errConfigChanged) {
			fmt.Fprintln(os.Stderr, T("cli.setup.config_changed"))
			return 2
		}
		fmt.Fprintln(os.Stderr, T("cli.setup.write_failed", err))
		return 2
	}
	fmt.Fprintln(os.Stdout, T("cli.setup.saved", path))
	return 0
}

type topSetupRow int

const (
	topSetupView topSetupRow = iota
	topSetupColumns
	topSetupBoxes
	topSetupPanels
	topSetupSort
	topSetupInterval
	topSetupRowCount
)

// topSetupChrome은 미리보기 말고 화면이 쓰는 줄이다: 제목, 설정 줄, 미리보기 머리줄, 키 안내.
const topSetupChrome = 3 + int(topSetupRowCount)

// topSetupSizes는 z로 바꿔 보는 미리보기 크기다. 0×0은 남은 자리 전체다. 터미널에 들어가지 않는 크기는 건너뛴다.
var topSetupSizes = [][2]int{{0, 0}, {80, 24}, {120, 30}}

var (
	topSetupViews = []topView{"", topViewAll, topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure, topViewSplit}
	topSetupSorts = []string{"", "cpu", "mem", "io"}
)

// topSetupItem은 켜고 끄며 순서를 바꾸는 열 그룹, 박스, 패널이다. detail은 열 그룹에만 쓰는 +다.
type topSetupItem struct {
	name   string
	on     bool
	detail bool
}

// topSettings는 wizard가 고치는 [defaults.top] 값이다. boxesSet과 panelsSet이 false인 목록은 파일에 적지 않아
// 대시보드가 기존 배치를 쓴다. 손대지 않은 값을 기본값으로 바꿔 적으면 나중에 기본값이 바뀌어도 따라가지 못한다.
type topSettings struct {
	view        topView
	columns     []topSetupItem
	columnsSet  bool
	boxes       []topSetupItem
	boxesSet    bool
	panels      []topSetupItem
	panelsSet   bool
	processSort string
	interval    time.Duration
	intervalSet bool
}

// topSetupList는 고른 이름을 적은 순서대로 켜고, 나머지를 기본 순서대로 끈 채 뒤에 붙인다.
func topSetupList(all, chosen []string) []topSetupItem {
	items := make([]topSetupItem, 0, len(all))
	for _, name := range chosen {
		items = append(items, topSetupItem{name: name, on: true})
	}
	for _, name := range all {
		if !slices.Contains(chosen, name) {
			items = append(items, topSetupItem{name: name})
		}
	}
	return items
}

func topSplitNameList() []string {
	names := make([]string, len(topSplitNames))
	for index, entry := range topSplitNames {
		names[index] = entry.name
	}
	return names
}

func topSettingsFrom(config topConfig) topSettings {
	settings := topSettings{interval: configuredDuration(config.Interval, time.Second), intervalSet: config.Interval != nil}
	settings.view = topView(configuredString(config.View, ""))
	settings.processSort = configuredString(config.ProcessSort, "")
	columnNames := make([]string, len(topColumnGroupNames))
	for index, entry := range topColumnGroupNames {
		columnNames[index] = entry.name
	}
	var defaultColumns []string
	for _, group := range topAllColumnGroups() {
		defaultColumns = append(defaultColumns, topColumnGroupName(group.name))
	}
	settings.columns = topSetupList(columnNames, defaultColumns)
	if value := configuredString(config.Columns, ""); value != "" {
		groups, _ := parseTopColumns(value)
		var chosen []string
		for _, group := range groups {
			chosen = append(chosen, topColumnGroupName(group.name))
		}
		if len(chosen) > 0 {
			settings.columns = topSetupList(columnNames, chosen)
			for index, group := range groups {
				settings.columns[index].detail = group.detail
			}
		}
		settings.columnsSet = true
	}
	boxNames := topSplitNameList()
	settings.boxes = topSetupList(boxNames, boxNames)
	if split := configuredString(config.Split, ""); split != "" {
		views, _ := parseTopSplit(split)
		var chosen []string
		for _, view := range views {
			chosen = append(chosen, topSplitName(view))
		}
		settings.boxes, settings.boxesSet = topSetupList(boxNames, chosen), true
	}
	panelNames := make([]string, len(topPanelNames))
	for index, panel := range topPanelNames {
		panelNames[index] = string(panel)
	}
	settings.panels = topSetupList(panelNames, nil)
	if value := configuredString(config.Panels, ""); value != "" {
		panels, _ := parseTopPanels(value)
		var chosen []string
		for _, panel := range panels {
			chosen = append(chosen, string(panel))
		}
		settings.panels, settings.panelsSet = topSetupList(panelNames, chosen), true
	}
	return settings
}

func topSetupOn(items []topSetupItem) []string {
	var names []string
	for _, item := range items {
		if item.on {
			names = append(names, item.name)
		}
	}
	return names
}

// splitValue는 defaults.top.split에 적을 값이다. 다섯 박스를 기본 순서로 모두 켜면 all, 모두 끄면 none이다.
func (settings topSettings) splitValue() string {
	names := topSetupOn(settings.boxes)
	switch {
	case len(names) == 0:
		return "none"
	case slices.Equal(names, topSplitNameList()):
		return "all"
	}
	return strings.Join(names, ",")
}

// columnsValue는 defaults.top.columns에 적을 값이다. 다섯 그룹을 기본 순서로 모두 켜고 +가 없으면 all이다.
func (settings topSettings) columnsValue() string {
	var parts, all []string
	for _, item := range settings.columns {
		if item.on && item.detail {
			parts = append(parts, item.name+"+")
		} else if item.on {
			parts = append(parts, item.name)
		}
	}
	for _, group := range topAllColumnGroups() {
		all = append(all, topColumnGroupName(group.name))
	}
	if slices.Equal(parts, all) {
		return "all"
	}
	return strings.Join(parts, ",")
}

func (settings topSettings) panelsValue() string {
	if names := topSetupOn(settings.panels); len(names) > 0 {
		return strings.Join(names, ",")
	}
	return "none"
}

// topConfig는 original에 wizard 값을 얹는다. wizard가 다루지 않는 count, json 같은 키는 그대로 둔다.
// 뜻이 같은 값은 원래 글자를 그대로 둔다. split = "cpu,mem,disk,net,psi"를 열기만 했는데 all로 바뀌어 저장되면 안 된다.
func (settings topSettings) topConfig(original topConfig) topConfig {
	config := original
	if settings.view != topView(configuredString(original.View, "")) {
		config.View = nil
		if settings.view != "" {
			config.View = stringPointer(string(settings.view))
		}
	}
	if settings.columnsSet && !topSameColumns(configuredString(original.Columns, ""), settings.columnsValue()) {
		config.Columns = stringPointer(settings.columnsValue())
	}
	if settings.boxesSet && !topSameSplit(configuredString(original.Split, ""), settings.splitValue()) {
		config.Split = stringPointer(settings.splitValue())
	}
	if settings.panelsSet && !topSamePanels(configuredString(original.Panels, ""), settings.panelsValue()) {
		config.Panels = stringPointer(settings.panelsValue())
	}
	if settings.processSort != configuredString(original.ProcessSort, "") {
		config.ProcessSort = nil
		if settings.processSort != "" {
			config.ProcessSort = stringPointer(settings.processSort)
		}
	}
	if settings.intervalSet && (original.Interval == nil || original.Interval.Duration != settings.interval) {
		config.Interval = &configDuration{Duration: settings.interval}
	}
	return config
}

func topSameSplit(before, after string) bool {
	if before == "" {
		return false
	}
	beforeViews, beforeErr := parseTopSplit(before)
	afterViews, afterErr := parseTopSplit(after)
	return beforeErr == nil && afterErr == nil && slices.Equal(beforeViews, afterViews)
}

func topSameColumns(before, after string) bool {
	if before == "" {
		return false
	}
	beforeGroups, beforeErr := parseTopColumns(before)
	afterGroups, afterErr := parseTopColumns(after)
	return beforeErr == nil && afterErr == nil && slices.Equal(topColumnGroupsOrAll(beforeGroups), topColumnGroupsOrAll(afterGroups))
}

// topColumnGroupsOrAll은 all(nil)을 다섯 그룹 목록으로 편다. all과 net,cpu,mem,disk,psi는 같은 배치다.
func topColumnGroupsOrAll(groups []topColumnGroup) []topColumnGroup {
	if groups != nil {
		return groups
	}
	return topAllColumnGroups()
}

func topSamePanels(before, after string) bool {
	if before == "" {
		return false
	}
	beforePanels, beforeErr := parseTopPanels(before)
	afterPanels, afterErr := parseTopPanels(after)
	return beforeErr == nil && afterErr == nil && slices.Equal(beforePanels, afterPanels)
}

func (settings topSettings) layout() topLayout {
	layout := topLayout{view: settings.view, panelsSet: settings.panelsSet}
	if settings.panelsSet {
		layout.panels, _ = parseTopPanels(settings.panelsValue())
	}
	if settings.processSort != "" {
		layout.processSort, _ = parseTopProcessSort(settings.processSort)
	}
	if settings.columnsSet {
		layout.columns, _ = parseTopColumns(settings.columnsValue())
	}
	return layout
}

func (settings topSettings) splitViews() []topView {
	if !settings.boxesSet {
		return nil
	}
	views, _ := parseTopSplit(settings.splitValue())
	return views
}

type topSetupModel struct {
	preview  topModel
	config   edcConfig
	path     string
	settings topSettings
	row      topSetupRow
	// item은 박스와 패널 줄에서 고른 항목이다.
	item          int
	width, height int
	// full은 설정 줄을 숨기고 미리보기를 터미널 전체로 그리는지다. size는 topSetupSizes의 자리다.
	full bool
	size int
	// confirmSave와 confirmQuit은 저장할 내용과 버릴 변경을 묻는 중인지다. save는 저장하고 끝내기로 했는지다.
	confirmSave bool
	confirmQuit bool
	save        bool
}

func newTopSetupModel(preview topModel, config edcConfig, path string) topSetupModel {
	model := topSetupModel{preview: preview, config: config, path: path, settings: topSettingsFrom(config.Defaults.Top)}
	model.preview, _ = model.settings.applyTo(model.preview)
	return model
}

// applyTo는 설정을 미리보기 모델에 다시 건다. 보기는 대시보드를 새로 열 때처럼 처음부터 정한다.
func (settings topSettings) applyTo(model topModel) (topModel, tea.Cmd) {
	model.view, model.panelSort, model.notice, model.detail, model.peaks = topViewAll, "", "", false, false
	model = model.withSplit(settings.splitViews()).withLayout(settings.layout())
	updated, tick := model.withInterval(settings.interval)
	model = updated.(topModel)
	var ai tea.Cmd
	if model.aiOn || model.aiTokens {
		model, ai = model.startAI()
	}
	return model, tea.Batch(tick, ai)
}

func (model topSetupModel) Init() tea.Cmd { return model.preview.Init() }

func (model topSetupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		return model, nil
	case tea.KeyPressMsg:
		return model.updateKey(value.String())
	}
	updated, cmd := model.preview.Update(msg)
	model.preview = updated.(topModel)
	return model, cmd
}

func (model topSetupModel) updateKey(key string) (tea.Model, tea.Cmd) {
	if key == "ctrl+c" {
		return model, tea.Quit
	}
	if model.confirmSave {
		switch key {
		case "y", "enter":
			model.save = true
			return model, tea.Quit
		case "n", "esc", "q":
			model.confirmSave = false
		}
		return model, nil
	}
	if model.confirmQuit {
		switch key {
		case "y":
			return model, tea.Quit
		case "n", "esc":
			model.confirmQuit = false
		}
		return model, nil
	}
	switch key {
	case "q", "esc":
		if model.dirty() {
			model.confirmQuit = true
			return model, nil
		}
		return model, tea.Quit
	case "enter":
		model.confirmSave = true
		return model, nil
	case "tab":
		model.full = !model.full
		model.size = model.fitSize(model.size)
		return model, nil
	case "z":
		model.size = model.fitSize(model.size + 1)
		return model, nil
	}
	if model.full {
		return model, nil
	}
	// 목록 항목은 제자리에서 바뀌므로 slice까지 복사해 둬야 바뀐 것을 알아챈다.
	before := model.settings
	before.columns, before.boxes, before.panels = slices.Clone(before.columns), slices.Clone(before.boxes), slices.Clone(before.panels)
	switch key {
	case "up", "k":
		if model.row > topSetupView {
			model.row, model.item = model.row-1, 0
		}
	case "down", "j":
		if model.row < topSetupRowCount-1 {
			model.row, model.item = model.row+1, 0
		}
	case "left", "h":
		model = model.step(-1)
	case "right", "l":
		model = model.step(1)
	case "space", " ":
		model = model.toggle()
	case "+", "=":
		model = model.toggleDetail()
	case "[":
		model = model.move(-1)
	case "]":
		model = model.move(1)
	}
	if reflect.DeepEqual(before, model.settings) {
		return model, nil
	}
	var cmd tea.Cmd
	model.preview, cmd = model.settings.applyTo(model.preview)
	return model, cmd
}

// list는 지금 줄의 항목 목록이다. 박스나 패널 줄이 아니면 nil이다.
func (model *topSetupModel) list() (*[]topSetupItem, *bool) {
	switch model.row {
	case topSetupColumns:
		return &model.settings.columns, &model.settings.columnsSet
	case topSetupBoxes:
		return &model.settings.boxes, &model.settings.boxesSet
	case topSetupPanels:
		return &model.settings.panels, &model.settings.panelsSet
	}
	return nil, nil
}

// step은 ←→다. 값 줄은 값을 바꾸고, 목록 줄은 고른 항목을 옮긴다.
func (model topSetupModel) step(direction int) topSetupModel {
	if items, _ := model.list(); items != nil {
		model.item = min(max(0, model.item+direction), len(*items)-1)
		return model
	}
	switch model.row {
	case topSetupView:
		model.settings.view = topSetupCycle(topSetupViews, model.settings.view, direction)
	case topSetupSort:
		model.settings.processSort = topSetupCycle(topSetupSorts, model.settings.processSort, direction)
	case topSetupInterval:
		model.settings.interval, model.settings.intervalSet = nextTopInterval(model.settings.interval, direction), true
	}
	return model
}

func topSetupCycle[T comparable](values []T, current T, direction int) T {
	index := max(0, slices.Index(values, current))
	return values[(index+direction+len(values))%len(values)]
}

func (model topSetupModel) toggle() topSetupModel {
	items, set := model.list()
	if items == nil {
		return model.step(1)
	}
	item := &(*items)[model.item]
	// 열 그룹이 하나도 없으면 all 보기에 시각과 signal만 남는다. 마지막 그룹은 끌 수 없다.
	if model.row == topSetupColumns && item.on && len(topSetupOn(*items)) == 1 {
		return model
	}
	item.on = !item.on
	*set = true
	return model
}

// toggleDetail은 열 그룹 줄의 +다. +를 붙이면 그룹도 켠다.
func (model topSetupModel) toggleDetail() topSetupModel {
	if model.row != topSetupColumns {
		return model
	}
	item := &model.settings.columns[model.item]
	item.detail = !item.detail
	if item.detail {
		item.on = true
	}
	model.settings.columnsSet = true
	return model
}

// move는 [ ]다. 고른 항목을 앞뒤로 옮기고 선택도 함께 옮긴다. 순서는 박스 배치와 패널 우선순위다.
func (model topSetupModel) move(direction int) topSetupModel {
	items, set := model.list()
	if items == nil {
		return model
	}
	target := model.item + direction
	if target < 0 || target >= len(*items) {
		return model
	}
	(*items)[model.item], (*items)[target] = (*items)[target], (*items)[model.item]
	model.item, *set = target, true
	return model
}

func (model topSetupModel) dirty() bool {
	original := model.config.Defaults.Top
	return !reflect.DeepEqual(model.settings.topConfig(original), original)
}

// configData는 저장할 파일 내용이다. 다른 섹션은 시작할 때 읽은 값을 그대로 쓴다.
func (model topSetupModel) configData() ([]byte, error) {
	config := model.config
	config.Defaults.Top = model.settings.topConfig(model.config.Defaults.Top)
	if err := validateConfig(config); err != nil {
		return nil, errors.New(T("cli.config.invalid", err))
	}
	if filepath.Ext(model.path) == ".toml" {
		return toml.Marshal(config)
	}
	return yaml.Marshal(config)
}

// topSection은 저장 확인 화면에 보이는 [defaults.top]이다.
func (model topSetupModel) topSection() string {
	var section struct {
		Defaults struct {
			Top topConfig `toml:"top"`
		} `toml:"defaults"`
	}
	section.Defaults.Top = model.settings.topConfig(model.config.Defaults.Top)
	data, err := toml.Marshal(section)
	if err != nil {
		return err.Error()
	}
	// 빈 [defaults] 머리줄은 toml이 상위 표를 적은 것이다. 저장할 내용이 아니므로 확인 화면에서 뺀다.
	return strings.TrimSpace(strings.TrimPrefix(string(data), "[defaults]\n"))
}

// area는 미리보기에 쓸 수 있는 크기다.
func (model topSetupModel) area() (int, int) {
	if model.full {
		return model.width, model.height
	}
	return model.width, max(0, model.height-topSetupChrome)
}

// fitSize는 start부터 들어가는 첫 크기의 자리다. 남은 자리 전체인 0은 언제나 들어간다.
func (model topSetupModel) fitSize(start int) int {
	width, height := model.area()
	for offset := range topSetupSizes {
		index := (start + offset) % len(topSetupSizes)
		if size := topSetupSizes[index]; size[0] <= width && size[1] <= height {
			return index
		}
	}
	return 0
}

func (model topSetupModel) previewSize() (int, int) {
	width, height := model.area()
	if size := topSetupSizes[model.size]; size[0] > 0 && size[0] <= width && size[1] <= height {
		return size[0], size[1]
	}
	return width, height
}

// sizedPreview는 미리보기 크기로 맞춘 대시보드다. 전체 화면에서는 안내 줄 첫 줄에 돌아가는 키를 보인다.
// 설정 화면에서는 고른 줄이 바꾸는 보기를 그린다. columns는 all 보기에만, boxes는 박스 화면에만 보이므로 시작 보기가
// 다르면 바꾼 값이 미리보기에 나타나지 않는다. 전체 화면은 저장 후 edc top이 여는 시작 보기를 그대로 그린다.
func (model topSetupModel) sizedPreview() topModel {
	preview := model.preview
	preview.width, preview.height = model.previewSize()
	if model.full {
		preview.notice = "setup preview · Tab back · Enter save · Esc quit"
		return preview
	}
	switch model.row {
	case topSetupColumns:
		preview.view = topViewAll
	case topSetupBoxes:
		preview = preview.enterSplit()
	}
	return preview
}

// previewLabel은 미리보기 머리줄에 적는 지금 그리는 보기다. 시작 보기와 다르면 그 이유를 함께 적는다.
func (model topSetupModel) previewLabel(preview topModel) string {
	name := topSplitViewLabel(preview.view)
	switch {
	case model.full || preview.view == model.preview.view:
		return "start view " + name
	case model.row == topSetupColumns:
		return name + " view for columns · start view " + topSplitViewLabel(model.preview.view)
	}
	return name + " for boxes · start view " + topSplitViewLabel(model.preview.view)
}

func topSplitViewLabel(view topView) string {
	if view == topViewSplit {
		return "boxes"
	}
	return string(view)
}

func (model topSetupModel) View() tea.View {
	color := model.preview.limits.color
	var lines []string
	switch {
	case model.width > 0 && (model.width < 40 || model.height < topSetupChrome+8):
		lines = []string{"terminal too small · resize to at least 40×" + fmt.Sprint(topSetupChrome+8), "Esc quit"}
	case model.confirmSave:
		lines = model.saveLines(color)
	default:
		lines = model.screenLines(color)
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, model.width), "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model topSetupModel) screenLines(color bool) []string {
	preview := model.sizedPreview()
	content := strings.Split(preview.View().Content, "\n")
	content = content[:min(len(content), preview.height)]
	if model.full {
		return content
	}
	// 경로가 길면 오른쪽이 잘리므로 바뀜 표시를 경로 앞에 둔다.
	title := "edc top setup · " + model.path
	if model.dirty() {
		title = "edc top setup · * modified · " + model.path
	}
	lines := []string{topHeading(title, color)}
	for row := range topSetupRowCount {
		lines = append(lines, model.rowLine(row, preview, color))
	}
	width, height := model.area()
	// 크기 뒤에 ai 조회, 숨는 패널, 그려지지 않는 박스 순으로 적는다. 패널 줄은 항목이 많아 80열에서 끝이 잘리므로 여기에 둔다.
	// ai를 켜면 미리보기가 실제로 Claude와 Codex 사용량을 읽으므로 켠 사람이 알도록 가장 앞에 둔다.
	header := fmt.Sprintf("─ preview · %s · %d×%d", model.previewLabel(preview), preview.width, preview.height)
	notes := []string{model.boxesNote(preview), model.panelsNote(preview)}
	switch {
	case preview.aiOn && preview.aiTokens:
		notes = append([]string{"ai polls Claude, Codex and reads their logs"}, notes...)
	case preview.aiOn:
		notes = append([]string{"ai polls Claude, Codex"}, notes...)
	case preview.aiTokens:
		notes = append([]string{"ai reads Claude, Codex logs"}, notes...)
	}
	for _, note := range notes {
		if note != "" {
			header += " · " + note
		}
	}
	if preview.width != model.width || preview.height != model.height {
		header += fmt.Sprintf(" · edc top in this terminal: %d×%d · Tab shows it", model.width, model.height)
	}
	lines = append(lines, topDim(header+" "+strings.Repeat("─", max(0, width-ansi.StringWidth(header)-1)), color))
	lines = append(lines, content...)
	for len(lines) < topSetupChrome-1+height {
		lines = append(lines, "")
	}
	return append(lines, model.hintLine(color))
}

var topSetupLabels = [topSetupRowCount]string{"view", "columns", "boxes", "panels", "sort", "interval"}

func (model topSetupModel) rowLine(row topSetupRow, preview topModel, color bool) string {
	focused := row == model.row
	marker := "  "
	if focused {
		marker = "▸ "
	}
	// 바뀐 줄은 이름 뒤에 *를 붙인다. 저장하면 파일의 이 키가 바뀐다.
	name, mark := topSetupLabels[row], ""
	if model.rowChanged(row) {
		mark = "*"
	}
	padding := strings.Repeat(" ", max(0, 9-len(name)-len(mark)))
	if color {
		if focused {
			name = richBold(name)
		}
		if mark != "" {
			mark = richPaint(mark, richYellow, true)
		}
	}
	label := name + mark + padding
	var value, note string
	switch row {
	case topSetupView:
		value = topSetupValue(topSetupViewName(model.settings.view), focused, color)
	case topSetupColumns:
		value = model.itemsLine(model.settings.columns, focused, color, func(name string) bool { return name == "psi" })
		if model.preview.view != topViewAll {
			note = "all view only · starts in " + topSplitViewLabel(model.preview.view)
		} else if !model.settings.columnsSet {
			note = "not set: all five, more columns as the width allows"
		}
	case topSetupBoxes:
		value = model.itemsLine(model.settings.boxes, focused, color, func(name string) bool { return name == "psi" })
		if !model.settings.boxesSet {
			note = "not set: v shows all five"
		}
	case topSetupPanels:
		value = model.itemsLine(model.settings.panels, focused, color, func(name string) bool {
			return name == string(topPanelNet) || name == string(topPanelPSI)
		})
		if !model.settings.panelsSet {
			note = "not set: net in the network view, events in free space"
		}
	case topSetupSort:
		name := model.settings.processSort
		if name == "" {
			name = "default (follows the view)"
		}
		value = topSetupValue(name, focused, color)
	case topSetupInterval:
		value = topSetupValue(model.settings.interval.String(), focused, color)
	}
	line := marker + label + " " + value
	if note != "" {
		line += "   " + topDim(note, color)
	}
	return line
}

func (model topSetupModel) rowChanged(row topSetupRow) bool {
	original := model.config.Defaults.Top
	config := model.settings.topConfig(original)
	switch row {
	case topSetupView:
		return !reflect.DeepEqual(config.View, original.View)
	case topSetupColumns:
		return !reflect.DeepEqual(config.Columns, original.Columns)
	case topSetupBoxes:
		return !reflect.DeepEqual(config.Split, original.Split)
	case topSetupPanels:
		return !reflect.DeepEqual(config.Panels, original.Panels)
	case topSetupSort:
		return !reflect.DeepEqual(config.ProcessSort, original.ProcessSort)
	}
	return !reflect.DeepEqual(config.Interval, original.Interval)
}

func topSetupViewName(view topView) string {
	if view == "" {
		return "default (all, or boxes when set)"
	}
	return string(view)
}

// topSetupValue는 ←→로 바꾸는 값이다. 고른 줄에서는 바뀔 수 있다는 표시로 ‹ ›를 붙인다.
func topSetupValue(text string, focused, color bool) string {
	if !focused {
		return text
	}
	if color {
		return richPaint("‹ ", richCyan, true) + text + richPaint(" ›", richCyan, true)
	}
	return "‹ " + text + " ›"
}

// itemsLine은 [x] 켜진 항목과 [ ] 꺼진 항목이다. 고른 항목은 키 안내의 켜진 항목과 같은 바탕으로 칠한다.
// linuxOnly 항목은 macOS에서 (Linux)를 붙인다. 한 설정 파일을 두 OS가 함께 쓰므로 고를 수는 있다.
func (model topSetupModel) itemsLine(items []topSetupItem, focused, color bool, linuxOnly func(string) bool) string {
	parts := make([]string, len(items))
	for index, item := range items {
		box := "[ ]"
		if item.on {
			box = "[x]"
		}
		text := box + " " + item.name
		if item.detail {
			text += "+"
		}
		if linuxOnly(item.name) && model.preview.details.System == "darwin" {
			text += " (Linux)"
		}
		switch {
		case focused && index == model.item && color:
			text = topBannerStyle() + text + richReset
		case focused && index == model.item:
			text = ">" + text
		case !item.on:
			text = topDim(text, color)
		}
		parts[index] = text
	}
	return strings.Join(parts, "  ")
}

// boxesNote는 박스 화면이 이 미리보기 크기에 들어가지 않을 때의 안내다.
func (model topSetupModel) boxesNote(preview topModel) string {
	if preview.view != topViewSplit {
		return ""
	}
	if _, _, ok := preview.splitLayout(); !ok {
		return "boxes need more rows · shows " + topSplitName(preview.splitViews()[0]) + " only"
	}
	return ""
}

// panelsNote는 이 미리보기 크기에서 숨는 패널이다. 대시보드와 같은 계산을 쓴다.
func (model topSetupModel) panelsNote(preview topModel) string {
	if !preview.customFooter() {
		return ""
	}
	_, hidden := preview.customFooterPlan()
	if len(hidden) == 0 {
		return ""
	}
	preview.hiddenPanels = hidden
	return preview.hiddenText()
}

func (model topSetupModel) hintLine(color bool) string {
	if model.confirmQuit {
		return topKeyHints{{key: "y", label: "discard changes"}, {key: "n", label: "keep editing"}}.render("  ", color)
	}
	hints := topKeyHints{{key: "↑↓", label: "row"}, {key: "←→", label: "value"}}
	if model.row == topSetupBoxes || model.row == topSetupPanels {
		hints = topKeyHints{{key: "↑↓", label: "row"}, {key: "←→", label: "item"}, {key: "Space", label: "on/off"}, {key: "[ ]", label: "order"}}
	}
	if model.row == topSetupColumns {
		hints = topKeyHints{{key: "↑↓", label: "row"}, {key: "←→", label: "group"}, {key: "Space", label: "on/off"}, {key: "+", label: "more columns"}, {key: "[ ]", label: "order"}}
	}
	sizeLabel := "size"
	if width, height := model.previewSize(); topSetupSizes[model.size][0] > 0 {
		sizeLabel = fmt.Sprintf("size %d×%d", width, height)
	}
	hints = append(hints, topKeyHint{key: "z", label: sizeLabel, on: model.size > 0}, topKeyHint{key: "Tab", label: "full"},
		topKeyHint{key: "Enter", label: "save"}, topKeyHint{key: "Esc", label: "quit"})
	return hints.render("  ", color)
}

func (model topSetupModel) saveLines(color bool) []string {
	lines := []string{topHeading("Save to "+model.path+"?", color), ""}
	lines = append(lines, strings.Split(model.topSection(), "\n")...)
	lines = append(lines, "", topDim("edc rewrites the whole file, so comments in it are not kept. Other sections keep their values.", color), "",
		topKeyHints{{key: "y/Enter", label: "save"}, {key: "n/Esc", label: "back"}}.render("  ", color))
	return lines
}
