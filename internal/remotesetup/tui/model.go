// Package tui draws the remotesetup editor with Bubble Tea. It reaches the host application
// only through remotesetup.Messages and Styler, so another program can supply its own.
package tui

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/x-mesh/edc/internal/remotesetup"
)

// Styler paints text. A key hint shows the key in a strong color and its words dim,
// and the selected row has its own background.
type Styler interface {
	Key(text string) string
	Dim(text string) string
	Selected(text string) string
	Error(text string) string
}

type PlainStyler struct{}

func (PlainStyler) Key(text string) string      { return text }
func (PlainStyler) Dim(text string) string      { return text }
func (PlainStyler) Selected(text string) string { return "> " + text }
func (PlainStyler) Error(text string) string    { return text }

type Options struct {
	Messages remotesetup.Messages
	Styler   Styler
	// Location is shown in the title, for example the path of the file.
	Location string
}

type mode int

const (
	modeHosts mode = iota
	modeGroups
	modeForm
	modeGroupName
	modePick
	modeNumber
	modeConfirm
)

type confirmKind int

const (
	confirmDeleteHost confirmKind = iota
	confirmDeleteGroup
	confirmDiscard
)

const (
	fieldName = iota
	fieldTarget
	fieldTags
	fieldCount
)

type Model struct {
	editor   *remotesetup.Editor
	messages remotesetup.Messages
	style    Styler
	location string

	mode          mode
	width, height int
	hostCursor    int
	groupCursor   int

	status      string
	statusError bool
	saved       bool

	fields      [fieldCount]textinput.Model
	fieldFocus  int
	editingHost int

	input        textinput.Model
	numberGroup  string
	pendingGroup string
	pickGroup    string
	pickCursor   int
	pickChecked  map[string]bool

	confirm       confirmKind
	confirmTarget string
	returnMode    mode
}

func New(editor *remotesetup.Editor, options Options) Model {
	model := Model{editor: editor, messages: options.Messages, style: options.Styler, location: options.Location, width: 80, height: 24, editingHost: -1}
	if model.style == nil {
		model.style = PlainStyler{}
	}
	for index := range model.fields {
		model.fields[index] = newInput()
	}
	model.input = newInput()
	return model
}

func newInput() textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.SetWidth(40)
	return input
}

// Saved reports whether the inventory was saved at least once during the session.
func (model Model) Saved() bool { return model.saved }

func (model Model) text(code string, args ...any) string { return model.messages.Text(code, args...) }

func (model Model) Init() tea.Cmd { return nil }

func (model Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		return model, nil
	case tea.KeyPressMsg:
		if value.String() == "ctrl+c" {
			return model.requestQuit()
		}
		switch model.mode {
		case modeHosts:
			return model.updateHosts(value)
		case modeGroups:
			return model.updateGroups(value)
		case modeForm:
			return model.updateForm(value)
		case modeGroupName:
			return model.updateGroupName(value)
		case modePick:
			return model.updatePick(value)
		case modeNumber:
			return model.updateNumber(value)
		case modeConfirm:
			return model.updateConfirm(value)
		}
	}
	return model.forward(message)
}

// forward gives non-key messages, such as the cursor blink, to the input in use.
func (model Model) forward(message tea.Msg) (tea.Model, tea.Cmd) {
	var command tea.Cmd
	switch model.mode {
	case modeForm:
		model.fields[model.fieldFocus], command = model.fields[model.fieldFocus].Update(message)
	case modeGroupName, modeNumber:
		model.input, command = model.input.Update(message)
	}
	return model, command
}

func (model Model) requestQuit() (tea.Model, tea.Cmd) {
	if !model.editor.Dirty() {
		return model, tea.Quit
	}
	model.returnMode = model.mode
	model.mode = modeConfirm
	model.confirm = confirmDiscard
	return model, nil
}

func (model Model) setStatus(text string, failed bool) Model {
	model.status, model.statusError = text, failed
	return model
}

func (model Model) issueText(err error) string {
	var issue remotesetup.Issue
	if errors.As(err, &issue) {
		return model.text(issue.Code, issue.Args...)
	}
	return err.Error()
}

func move(cursor, delta, length int) int {
	if length == 0 {
		return 0
	}
	return (cursor + delta + length) % length
}

func (model Model) updateHosts(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	inventory := model.editor.Inventory()
	switch key.String() {
	case "up", "k":
		model.hostCursor = move(model.hostCursor, -1, len(inventory.Hosts))
	case "down", "j":
		model.hostCursor = move(model.hostCursor, 1, len(inventory.Hosts))
	case "a":
		return model.startForm(-1)
	case "e", "enter":
		if len(inventory.Hosts) > 0 {
			return model.startForm(model.hostCursor)
		}
	case "d":
		if len(inventory.Hosts) > 0 {
			model.mode, model.returnMode, model.confirm = modeConfirm, modeHosts, confirmDeleteHost
			model.confirmTarget = inventory.Hosts[model.hostCursor].Name
		}
	case "g", "tab":
		model.mode = modeGroups
		model.status = ""
	case "s":
		return model.save()
	case "q", "esc":
		return model.requestQuit()
	}
	return model, nil
}

func (model Model) updateGroups(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	inventory := model.editor.Inventory()
	switch key.String() {
	case "up", "k":
		model.groupCursor = move(model.groupCursor, -1, len(inventory.Groups))
	case "down", "j":
		model.groupCursor = move(model.groupCursor, 1, len(inventory.Groups))
	case "a":
		if len(inventory.Hosts) == 0 {
			return model.setStatus(model.text("remotesetup.tui.status.add_host_first"), true), nil
		}
		model.mode = modeGroupName
		model.input.SetValue("")
		model.input.Placeholder = ""
		model.status = ""
		return model, model.input.Focus()
	case "e", "enter":
		if len(inventory.Groups) > 0 {
			return model.startPick(inventory.Groups[model.groupCursor].Name, inventory.Groups[model.groupCursor].Members), nil
		}
	case "d":
		if len(inventory.Groups) > 0 {
			model.mode, model.returnMode, model.confirm = modeConfirm, modeGroups, confirmDeleteGroup
			model.confirmTarget = inventory.Groups[model.groupCursor].Name
		}
	case "p":
		return model.startNumber("", inventory.Parallel)
	case "o":
		if len(inventory.Groups) > 0 {
			name := inventory.Groups[model.groupCursor].Name
			return model.startNumber(name, inventory.GroupParallel[name])
		}
	case "h", "tab":
		model.mode = modeHosts
		model.status = ""
	case "s":
		return model.save()
	case "q", "esc":
		return model.requestQuit()
	}
	return model, nil
}

func (model Model) startForm(index int) (tea.Model, tea.Cmd) {
	model.mode, model.editingHost, model.fieldFocus, model.status = modeForm, index, 0, ""
	var host remotesetup.Host
	if index >= 0 {
		host = model.editor.Inventory().Hosts[index]
	}
	model.fields[fieldName].SetValue(host.Name)
	model.fields[fieldTarget].SetValue(host.Target)
	model.fields[fieldTags].SetValue(strings.Join(host.Tags, ", "))
	for position := range model.fields {
		model.fields[position].Blur()
	}
	return model, model.fields[fieldName].Focus()
}

func (model Model) focusField(position int) (Model, tea.Cmd) {
	model.fields[model.fieldFocus].Blur()
	model.fieldFocus = position
	return model, model.fields[position].Focus()
}

func (model Model) updateForm(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		model.mode = modeHosts
		model.status = ""
		return model, nil
	case "tab", "down":
		return model.focusField((model.fieldFocus + 1) % fieldCount)
	case "shift+tab", "up":
		return model.focusField((model.fieldFocus + fieldCount - 1) % fieldCount)
	case "enter":
		if model.fieldFocus < fieldCount-1 {
			return model.focusField(model.fieldFocus + 1)
		}
		return model.submitHost()
	}
	var command tea.Cmd
	model.fields[model.fieldFocus], command = model.fields[model.fieldFocus].Update(key)
	return model, command
}

func splitTags(value string) []string {
	var tags []string
	for _, tag := range strings.Split(value, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func (model Model) submitHost() (tea.Model, tea.Cmd) {
	host := remotesetup.Host{
		Name:   model.fields[fieldName].Value(),
		Target: model.fields[fieldTarget].Value(),
		Tags:   splitTags(model.fields[fieldTags].Value()),
	}
	var err error
	if model.editingHost >= 0 {
		err = model.editor.UpdateHost(model.editingHost, host)
	} else {
		err = model.editor.AddHost(host)
	}
	if err != nil {
		return model.setStatus(model.issueText(err), true), nil
	}
	if model.editingHost < 0 {
		model.hostCursor = len(model.editor.Inventory().Hosts) - 1
	}
	model.mode = modeHosts
	return model.setStatus("", false), nil
}

func (model Model) updateGroupName(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		model.mode = modeGroups
		model.status = ""
		return model, nil
	case "enter":
		name := strings.TrimSpace(model.input.Value())
		if name == "" {
			return model.setStatus(model.text(remotesetup.IssueGroupNameEmpty), true), nil
		}
		if model.editor.Inventory().GroupIndex(name) >= 0 {
			return model.setStatus(model.text(remotesetup.IssueGroupDuplicate, name), true), nil
		}
		model.pendingGroup = name
		return model.startPick("", nil), nil
	}
	var command tea.Cmd
	model.input, command = model.input.Update(key)
	return model, command
}

// startPick opens the member list. An empty group name means a new group named pendingGroup.
func (model Model) startPick(group string, members []string) Model {
	model.mode, model.pickGroup, model.pickCursor, model.status = modePick, group, 0, ""
	model.pickChecked = make(map[string]bool, len(members))
	for _, member := range members {
		model.pickChecked[member] = true
	}
	return model
}

func (model Model) updatePick(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	hosts := model.editor.Inventory().Hosts
	switch key.String() {
	case "up", "k":
		model.pickCursor = move(model.pickCursor, -1, len(hosts))
	case "down", "j":
		model.pickCursor = move(model.pickCursor, 1, len(hosts))
	case " ", "space", "x":
		if len(hosts) > 0 {
			name := hosts[model.pickCursor].Name
			model.pickChecked[name] = !model.pickChecked[name]
		}
	case "esc":
		model.mode = modeGroups
		model.status = ""
	case "enter":
		var members []string
		for _, host := range hosts {
			if model.pickChecked[host.Name] {
				members = append(members, host.Name)
			}
		}
		var err error
		if model.pickGroup == "" {
			err = model.editor.AddGroup(model.pendingGroup, members)
		} else {
			err = model.editor.SetMembers(model.pickGroup, members)
		}
		if err != nil {
			return model.setStatus(model.issueText(err), true), nil
		}
		if model.pickGroup == "" {
			model.groupCursor = len(model.editor.Inventory().Groups) - 1
		}
		model.mode = modeGroups
		return model.setStatus("", false), nil
	}
	return model, nil
}

func (model Model) startNumber(group string, current int) (tea.Model, tea.Cmd) {
	model.mode, model.numberGroup, model.status = modeNumber, group, ""
	model.input.SetValue(strconv.Itoa(current))
	return model, model.input.Focus()
}

func (model Model) updateNumber(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		model.mode = modeGroups
		model.status = ""
		return model, nil
	case "enter":
		value, err := strconv.Atoi(strings.TrimSpace(model.input.Value()))
		if err != nil {
			return model.setStatus(model.text("remotesetup.tui.status.not_a_number"), true), nil
		}
		if model.numberGroup == "" {
			err = model.editor.SetParallel(value)
		} else {
			err = model.editor.SetGroupParallel(model.numberGroup, value)
		}
		if err != nil {
			return model.setStatus(model.issueText(err), true), nil
		}
		model.mode = modeGroups
		return model.setStatus("", false), nil
	}
	var command tea.Cmd
	model.input, command = model.input.Update(key)
	return model, command
}

func (model Model) updateConfirm(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "y", "Y":
		return model.applyConfirm()
	case "n", "N", "esc", "q":
		model.mode = model.returnMode
	}
	return model, nil
}

func (model Model) applyConfirm() (tea.Model, tea.Cmd) {
	model.mode = model.returnMode
	switch model.confirm {
	case confirmDiscard:
		return model, tea.Quit
	case confirmDeleteHost:
		inventory := model.editor.Inventory()
		if index := inventory.HostIndex(model.confirmTarget); index >= 0 {
			removed := model.editor.RemoveHost(index)
			model.hostCursor = min(model.hostCursor, max(len(inventory.Hosts)-2, 0))
			model.groupCursor = min(model.groupCursor, max(len(model.editor.Inventory().Groups)-1, 0))
			if len(removed) > 0 {
				return model.setStatus(model.text("remotesetup.tui.status.groups_removed", strings.Join(removed, ", ")), false), nil
			}
		}
	case confirmDeleteGroup:
		model.editor.RemoveGroup(model.confirmTarget)
		model.groupCursor = min(model.groupCursor, max(len(model.editor.Inventory().Groups)-1, 0))
	}
	return model.setStatus("", false), nil
}

func (model Model) save() (tea.Model, tea.Cmd) {
	if !model.editor.Dirty() {
		return model.setStatus(model.text("remotesetup.tui.status.no_changes"), false), nil
	}
	problems, err := model.editor.Save()
	switch {
	case errors.Is(err, remotesetup.ErrStale):
		return model.setStatus(model.text("remotesetup.tui.status.stale"), true), nil
	case err != nil:
		return model.setStatus(model.text("remotesetup.tui.status.save_failed", model.issueText(err)), true), nil
	case len(problems) > 0:
		first := model.text(problems[0].Code, problems[0].Args...)
		return model.setStatus(model.text("remotesetup.tui.status.blocked", len(problems), first), true), nil
	}
	model.saved = true
	return model.setStatus(model.text("remotesetup.tui.status.saved"), false), nil
}

// Run shows the editor until the user quits. It reports whether anything was saved.
func Run(editor *remotesetup.Editor, options Options, input io.Reader, output io.Writer) (bool, error) {
	final, err := tea.NewProgram(New(editor, options), tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return false, err
	}
	model, ok := final.(Model)
	return ok && model.saved, nil
}

func (model Model) hint(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for index := 0; index+1 < len(pairs); index += 2 {
		parts = append(parts, model.style.Key(pairs[index])+" "+model.style.Dim(model.text("remotesetup.tui.key."+pairs[index+1])))
	}
	return strings.Join(parts, model.style.Dim(" · "))
}

func (model Model) hints() string {
	switch model.mode {
	case modeHosts:
		return model.hint("↑↓", "move", "a", "add", "e", "edit", "d", "delete", "g", "groups", "s", "save", "q", "quit")
	case modeGroups:
		return model.hint("↑↓", "move", "a", "add", "e", "members", "d", "delete", "p", "parallel", "o", "group_parallel", "h", "hosts", "s", "save", "q", "quit")
	case modeForm:
		return model.hint("tab", "next", "enter", "confirm", "esc", "cancel")
	case modePick:
		return model.hint("↑↓", "move", "space", "toggle", "enter", "confirm", "esc", "cancel")
	case modeConfirm:
		return model.hint("y", "yes", "n", "no")
	default:
		return model.hint("enter", "confirm", "esc", "cancel")
	}
}

func (model Model) View() tea.View {
	var builder strings.Builder
	title := model.text("remotesetup.tui.title", model.location)
	if model.editor.Dirty() {
		title += " *"
	}
	builder.WriteString(ansi.Truncate(title, max(model.width, 20), "…") + "\n")
	builder.WriteString(model.tabs() + "\n\n")
	builder.WriteString(model.body())
	builder.WriteString("\n")
	builder.WriteString(model.statusLine() + "\n")
	builder.WriteString(model.hints() + "\n")
	view := tea.NewView(builder.String())
	view.AltScreen = true
	return view
}

func (model Model) tabs() string {
	hosts, groups := model.text("remotesetup.tui.screen.hosts"), model.text("remotesetup.tui.screen.groups")
	switch model.mode {
	case modeGroups, modeGroupName, modePick, modeNumber:
		return model.style.Dim(hosts) + "  " + model.style.Selected(groups)
	}
	return model.style.Selected(hosts) + "  " + model.style.Dim(groups)
}

func (model Model) statusLine() string {
	if model.mode == modeConfirm {
		switch model.confirm {
		case confirmDeleteHost:
			return model.text("remotesetup.tui.confirm.delete_host", model.confirmTarget)
		case confirmDeleteGroup:
			return model.text("remotesetup.tui.confirm.delete_group", model.confirmTarget)
		default:
			return model.text("remotesetup.tui.confirm.discard")
		}
	}
	if model.statusError {
		return model.style.Error(model.status)
	}
	return model.style.Dim(model.status)
}

func (model Model) rows() int { return max(model.height-8, 3) }

func window(cursor, total, rows int) (int, int) {
	if total <= rows {
		return 0, total
	}
	start := min(max(cursor-rows/2, 0), total-rows)
	return start, start + rows
}

func (model Model) cell(text string, width int) string {
	if gap := width - ansi.StringWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

func (model Model) listRows(lines []string, cursor int, empty string) string {
	if len(lines) == 0 {
		return model.style.Dim(empty) + "\n"
	}
	var builder strings.Builder
	start, end := window(cursor, len(lines), model.rows())
	for index := start; index < end; index++ {
		line := ansi.Truncate(lines[index], max(model.width-4, 20), "…")
		if index == cursor {
			builder.WriteString(model.style.Selected(line) + "\n")
		} else {
			builder.WriteString("  " + line + "\n")
		}
	}
	return builder.String()
}

func (model Model) body() string {
	inventory := model.editor.Inventory()
	switch model.mode {
	case modeForm:
		return model.formBody()
	case modeGroupName:
		return model.text("remotesetup.tui.prompt.group_name") + "\n" + model.input.View() + "\n"
	case modeNumber:
		if model.numberGroup == "" {
			return model.text("remotesetup.tui.prompt.parallel") + "\n" + model.input.View() + "\n"
		}
		return model.text("remotesetup.tui.prompt.group_parallel", model.numberGroup) + "\n" + model.input.View() + "\n"
	case modePick:
		return model.pickBody(inventory)
	case modeGroups:
		return model.groupsBody(inventory)
	case modeConfirm:
		if model.returnMode == modeGroups || model.confirm == confirmDeleteGroup {
			return model.groupsBody(inventory)
		}
	}
	return model.hostsBody(inventory)
}

func (model Model) hostsBody(inventory remotesetup.Inventory) string {
	nameWidth, targetWidth := 0, 0
	for _, host := range inventory.Hosts {
		nameWidth = max(nameWidth, ansi.StringWidth(host.Name))
		targetWidth = max(targetWidth, ansi.StringWidth(host.Target))
	}
	lines := make([]string, len(inventory.Hosts))
	for index, host := range inventory.Hosts {
		tags := ""
		if len(host.Tags) > 0 {
			tags = "[" + strings.Join(host.Tags, ",") + "]"
		}
		lines[index] = strings.TrimRight(model.cell(host.Name, nameWidth)+"  "+model.cell(host.Target, targetWidth)+"  "+tags, " ")
	}
	return model.listRows(lines, model.hostCursor, model.text("remotesetup.tui.hosts_empty"))
}

func (model Model) groupsBody(inventory remotesetup.Inventory) string {
	var builder strings.Builder
	builder.WriteString(model.style.Dim(model.text("remotesetup.tui.parallel", inventory.Parallel)) + "\n")
	nameWidth := 0
	for _, group := range inventory.Groups {
		nameWidth = max(nameWidth, ansi.StringWidth(group.Name))
	}
	lines := make([]string, len(inventory.Groups))
	for index, group := range inventory.Groups {
		line := model.cell(group.Name, nameWidth) + "  " + strings.Join(group.Members, ", ")
		if value := inventory.GroupParallel[group.Name]; value > 0 {
			line += "  " + model.text("remotesetup.tui.group_parallel", value)
		}
		lines[index] = line
	}
	builder.WriteString(model.listRows(lines, model.groupCursor, model.text("remotesetup.tui.groups_empty")))
	return builder.String()
}

func (model Model) pickBody(inventory remotesetup.Inventory) string {
	name := model.pickGroup
	if name == "" {
		name = model.pendingGroup
	}
	lines := make([]string, len(inventory.Hosts))
	for index, host := range inventory.Hosts {
		box := "[ ]"
		if model.pickChecked[host.Name] {
			box = "[x]"
		}
		lines[index] = box + " " + host.Name
	}
	return model.text("remotesetup.tui.pick.title", name) + "\n" + model.listRows(lines, model.pickCursor, model.text("remotesetup.tui.hosts_empty"))
}

func (model Model) formBody() string {
	title := "remotesetup.tui.form.add"
	if model.editingHost >= 0 {
		title = "remotesetup.tui.form.edit"
	}
	labels := [fieldCount]string{
		model.text("remotesetup.tui.field.name"),
		model.text("remotesetup.tui.field.target"),
		model.text("remotesetup.tui.field.tags"),
	}
	width := 0
	for _, label := range labels {
		width = max(width, ansi.StringWidth(label))
	}
	var builder strings.Builder
	builder.WriteString(model.text(title) + "\n\n")
	for index, label := range labels {
		marker := "  "
		if index == model.fieldFocus {
			marker = model.style.Key("> ")
		}
		builder.WriteString(fmt.Sprintf("%s%s  %s\n", marker, model.cell(label, width), model.fields[index].View()))
	}
	return builder.String()
}
