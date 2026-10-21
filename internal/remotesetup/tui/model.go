// Package tui draws the remotesetup editors with Bubble Tea. It reaches the host application
// only through remotesetup.Messages and Styler, so another program can supply its own.
package tui

import (
	"errors"
	"io"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

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
	// Location is shown in the title, for example the path of the inventory file.
	Location string
	// Recipe adds the steps screen. It stays nil for a program that edits no recipe.
	Recipe         *remotesetup.RecipeEditor
	RecipeLocation string
}

type mode int

const (
	modeHosts mode = iota
	modeGroups
	modeSteps
	modeForm
	modeGroupName
	modeRecipeName
	modePick
	modeNumber
	modeConfirm
)

type confirmKind int

const (
	confirmDeleteHost confirmKind = iota
	confirmDeleteGroup
	confirmDeleteStep
	confirmDiscard
)

type formKind int

const (
	formHost formKind = iota
	formStep
)

// The fields of the host form and of the step form, in screen order.
const (
	hostName = iota
	hostTarget
	hostTags
)

const (
	stepName = iota
	stepCommand
	stepSource
	stepDestination
	stepMode
	stepVerify
	stepTimeout
	stepTags
)

var (
	hostLabels = []string{"remotesetup.tui.field.name", "remotesetup.tui.field.target", "remotesetup.tui.field.tags"}
	stepLabels = []string{
		"remotesetup.tui.field.step_name", "remotesetup.tui.field.step_command", "remotesetup.tui.field.step_source",
		"remotesetup.tui.field.step_destination", "remotesetup.tui.field.step_mode", "remotesetup.tui.field.step_verify",
		"remotesetup.tui.field.step_timeout", "remotesetup.tui.field.step_tags",
	}
)

type formState struct {
	kind    formKind
	labels  []string
	inputs  []textinput.Model
	focus   int
	editing int
}

type Model struct {
	editor         *remotesetup.Editor
	recipe         *remotesetup.RecipeEditor
	messages       remotesetup.Messages
	style          Styler
	location       string
	recipeLocation string

	mode          mode
	width, height int
	hostCursor    int
	groupCursor   int
	stepCursor    int

	status      string
	statusError bool
	saved       bool
	result      Result

	form formState

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
	model := Model{
		editor: editor, recipe: options.Recipe, messages: options.Messages, style: options.Styler,
		location: options.Location, recipeLocation: options.RecipeLocation, width: 80, height: 24,
	}
	if model.style == nil {
		model.style = PlainStyler{}
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

// Result tells which files were saved at least once during the session.
type Result struct{ Inventory, Recipe bool }

func (model Model) Result() Result { return model.result }

func (model Model) dirty() bool {
	return model.editor.Dirty() || model.recipe != nil && model.recipe.Dirty()
}

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
		case modeSteps:
			return model.updateSteps(value)
		case modeForm:
			return model.updateForm(value)
		case modeGroupName:
			return model.updateGroupName(value)
		case modeRecipeName:
			return model.updateRecipeName(value)
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
		model.form.inputs[model.form.focus], command = model.form.inputs[model.form.focus].Update(message)
	case modeGroupName, modeRecipeName, modeNumber:
		model.input, command = model.input.Update(message)
	}
	return model, command
}

func (model Model) requestQuit() (tea.Model, tea.Cmd) {
	if !model.dirty() {
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

// switchScreen handles the keys that change the list. It reports false for any other key.
func (model Model) switchScreen(key string) (Model, bool) {
	screens := []mode{modeHosts, modeGroups}
	if model.recipe != nil {
		screens = append(screens, modeSteps)
	}
	current := model.screen()
	target := mode(-1)
	switch key {
	case "tab":
		for index, screen := range screens {
			if screen == current {
				target = screens[(index+1)%len(screens)]
			}
		}
	case "h":
		target = modeHosts
	case "g":
		target = modeGroups
	case "r":
		target = modeSteps
	default:
		return model, false
	}
	for _, screen := range screens {
		if screen == target && target != current {
			model.mode, model.status = target, ""
			return model, true
		}
	}
	return model, false
}

func (model Model) updateHosts(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, ok := model.switchScreen(key.String()); ok {
		return next, nil
	}
	inventory := model.editor.Inventory()
	switch key.String() {
	case "up", "k":
		model.hostCursor = move(model.hostCursor, -1, len(inventory.Hosts))
	case "down", "j":
		model.hostCursor = move(model.hostCursor, 1, len(inventory.Hosts))
	case "a":
		return model.startHostForm(-1)
	case "e", "enter":
		if len(inventory.Hosts) > 0 {
			return model.startHostForm(model.hostCursor)
		}
	case "d":
		if len(inventory.Hosts) > 0 {
			model.mode, model.returnMode, model.confirm = modeConfirm, modeHosts, confirmDeleteHost
			model.confirmTarget = inventory.Hosts[model.hostCursor].Name
		}
	case "s":
		return model.save()
	case "q", "esc":
		return model.requestQuit()
	}
	return model, nil
}

func (model Model) updateGroups(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, ok := model.switchScreen(key.String()); ok {
		return next, nil
	}
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
	case "s":
		return model.save()
	case "q", "esc":
		return model.requestQuit()
	}
	return model, nil
}

func (model Model) updateSteps(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, ok := model.switchScreen(key.String()); ok {
		return next, nil
	}
	steps := model.recipe.Recipe().Steps
	switch key.String() {
	case "up", "k":
		model.stepCursor = move(model.stepCursor, -1, len(steps))
	case "down", "j":
		model.stepCursor = move(model.stepCursor, 1, len(steps))
	case "K":
		model.stepCursor = model.recipe.MoveStep(model.stepCursor, -1)
	case "J":
		model.stepCursor = model.recipe.MoveStep(model.stepCursor, 1)
	case "a":
		return model.startStepForm(-1)
	case "e", "enter":
		if len(steps) > 0 {
			return model.startStepForm(model.stepCursor)
		}
	case "d":
		if len(steps) > 0 {
			model.mode, model.returnMode, model.confirm = modeConfirm, modeSteps, confirmDeleteStep
			model.confirmTarget = steps[model.stepCursor].Name
		}
	case "n":
		model.mode = modeRecipeName
		model.input.SetValue(model.recipe.Recipe().Name)
		model.status = ""
		return model, model.input.Focus()
	case "s":
		return model.save()
	case "q", "esc":
		return model.requestQuit()
	}
	return model, nil
}

func (model Model) startForm(kind formKind, labels []string, values []string, editing int) (tea.Model, tea.Cmd) {
	model.mode, model.status = modeForm, ""
	model.form = formState{kind: kind, labels: labels, inputs: make([]textinput.Model, len(labels)), editing: editing}
	for index := range model.form.inputs {
		model.form.inputs[index] = newInput()
		model.form.inputs[index].SetValue(values[index])
	}
	return model, model.form.inputs[0].Focus()
}

func (model Model) startHostForm(index int) (tea.Model, tea.Cmd) {
	var host remotesetup.Host
	if index >= 0 {
		host = model.editor.Inventory().Hosts[index]
	}
	return model.startForm(formHost, hostLabels, []string{host.Name, host.Target, strings.Join(host.Tags, ", ")}, index)
}

func (model Model) startStepForm(index int) (tea.Model, tea.Cmd) {
	var step remotesetup.Step
	if index >= 0 {
		step = model.recipe.Recipe().Steps[index]
	}
	var upload remotesetup.Upload
	if step.Upload != nil {
		upload = *step.Upload
	}
	values := []string{step.Name, step.Command, upload.Source, upload.Destination, upload.Mode, step.Verify, step.Timeout, strings.Join(step.Tags, ", ")}
	return model.startForm(formStep, stepLabels, values, index)
}

func (model Model) focusField(position int) (Model, tea.Cmd) {
	model.form.inputs[model.form.focus].Blur()
	model.form.focus = position
	return model, model.form.inputs[position].Focus()
}

func (model Model) updateForm(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	count := len(model.form.inputs)
	switch key.String() {
	case "esc":
		model.mode = model.screen()
		model.status = ""
		return model, nil
	case "tab", "down":
		return model.focusField((model.form.focus + 1) % count)
	case "shift+tab", "up":
		return model.focusField((model.form.focus + count - 1) % count)
	case "enter":
		if model.form.focus < count-1 {
			return model.focusField(model.form.focus + 1)
		}
		if model.form.kind == formStep {
			return model.submitStep()
		}
		return model.submitHost()
	}
	var command tea.Cmd
	model.form.inputs[model.form.focus], command = model.form.inputs[model.form.focus].Update(key)
	return model, command
}

func (model Model) value(field int) string { return model.form.inputs[field].Value() }

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
	host := remotesetup.Host{Name: model.value(hostName), Target: model.value(hostTarget), Tags: splitTags(model.value(hostTags))}
	var err error
	if model.form.editing >= 0 {
		err = model.editor.UpdateHost(model.form.editing, host)
	} else {
		err = model.editor.AddHost(host)
	}
	if err != nil {
		return model.setStatus(model.issueText(err), true), nil
	}
	if model.form.editing < 0 {
		model.hostCursor = len(model.editor.Inventory().Hosts) - 1
	}
	model.mode = modeHosts
	return model.setStatus("", false), nil
}

func (model Model) submitStep() (tea.Model, tea.Cmd) {
	step := remotesetup.Step{
		Name: model.value(stepName), Command: model.value(stepCommand), Verify: model.value(stepVerify),
		Timeout: strings.TrimSpace(model.value(stepTimeout)), Tags: splitTags(model.value(stepTags)),
	}
	upload := remotesetup.Upload{
		Source: strings.TrimSpace(model.value(stepSource)), Destination: strings.TrimSpace(model.value(stepDestination)), Mode: strings.TrimSpace(model.value(stepMode)),
	}
	if upload != (remotesetup.Upload{}) {
		step.Upload = &upload
	}
	step.Command = strings.TrimSpace(step.Command)
	step.Verify = strings.TrimSpace(step.Verify)
	var err error
	if model.form.editing >= 0 {
		err = model.recipe.UpdateStep(model.form.editing, step)
	} else {
		err = model.recipe.AddStep(step)
	}
	if err != nil {
		return model.setStatus(model.issueText(err), true), nil
	}
	if model.form.editing < 0 {
		model.stepCursor = len(model.recipe.Recipe().Steps) - 1
	}
	model.mode = modeSteps
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

func (model Model) updateRecipeName(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		model.mode = modeSteps
		model.status = ""
		return model, nil
	case "enter":
		model.recipe.SetName(model.input.Value())
		model.mode = modeSteps
		return model.setStatus("", false), nil
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
	case confirmDeleteStep:
		if index := model.recipe.Recipe().StepIndex(model.confirmTarget); index >= 0 {
			model.recipe.RemoveStep(index)
			model.stepCursor = min(model.stepCursor, max(len(model.recipe.Recipe().Steps)-1, 0))
		}
	}
	return model.setStatus("", false), nil
}

// saveFailure turns the result of one save into a status. It reports false when the save succeeded.
func (model Model) saveFailure(problems []remotesetup.Issue, err error, blocked string) (string, bool) {
	switch {
	case errors.Is(err, remotesetup.ErrStale):
		return model.text("remotesetup.tui.status.stale"), true
	case err != nil:
		return model.text("remotesetup.tui.status.save_failed", model.issueText(err)), true
	case len(problems) > 0:
		return model.text(blocked, len(problems), model.text(problems[0].Code, problems[0].Args...)), true
	}
	return "", false
}

// save writes the inventory and then the recipe, and stops at the first one that fails.
func (model Model) save() (tea.Model, tea.Cmd) {
	if !model.dirty() {
		return model.setStatus(model.text("remotesetup.tui.status.no_changes"), false), nil
	}
	if model.editor.Dirty() {
		problems, err := model.editor.Save()
		if status, failed := model.saveFailure(problems, err, "remotesetup.tui.status.blocked"); failed {
			return model.setStatus(status, true), nil
		}
		model.saved, model.result.Inventory = true, true
	}
	if model.recipe != nil && model.recipe.Dirty() {
		problems, err := model.recipe.Save()
		if status, failed := model.saveFailure(problems, err, "remotesetup.tui.status.recipe_blocked"); failed {
			return model.setStatus(status, true), nil
		}
		model.saved, model.result.Recipe = true, true
	}
	return model.setStatus(model.text("remotesetup.tui.status.saved"), false), nil
}

// Run shows the editors until the user quits and reports which files were saved.
func Run(editor *remotesetup.Editor, options Options, input io.Reader, output io.Writer) (Result, error) {
	final, err := tea.NewProgram(New(editor, options), tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return Result{}, err
	}
	if model, ok := final.(Model); ok {
		return model.result, nil
	}
	return Result{}, nil
}
