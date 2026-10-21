package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// screen is the list that the current mode belongs to: hosts, groups, or steps.
func (model Model) screen() mode {
	switch model.mode {
	case modeGroups, modeGroupName, modePick, modeNumber:
		return modeGroups
	case modeSteps, modeRecipeName:
		return modeSteps
	case modeForm:
		if model.form.kind == formStep {
			return modeSteps
		}
		return modeHosts
	case modeConfirm:
		switch model.returnMode {
		case modeGroups, modeGroupName, modePick, modeNumber:
			return modeGroups
		case modeSteps, modeRecipeName:
			return modeSteps
		case modeForm:
			if model.form.kind == formStep {
				return modeSteps
			}
		}
	}
	return modeHosts
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
		if model.recipe != nil {
			return model.hint("↑↓", "move", "a", "add", "e", "edit", "d", "delete", "g", "groups", "r", "steps", "s", "save", "q", "quit")
		}
		return model.hint("↑↓", "move", "a", "add", "e", "edit", "d", "delete", "g", "groups", "s", "save", "q", "quit")
	case modeGroups:
		if model.recipe != nil {
			return model.hint("↑↓", "move", "a", "add", "e", "members", "d", "delete", "p", "parallel", "o", "group_parallel", "h", "hosts", "r", "steps", "s", "save", "q", "quit")
		}
		return model.hint("↑↓", "move", "a", "add", "e", "members", "d", "delete", "p", "parallel", "o", "group_parallel", "h", "hosts", "s", "save", "q", "quit")
	case modeSteps:
		return model.hint("↑↓", "move", "K/J", "reorder", "a", "add", "e", "edit", "d", "delete", "n", "name", "h", "hosts", "g", "groups", "s", "save", "q", "quit")
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
	if model.dirty() {
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
	labels := []struct {
		screen mode
		code   string
	}{{modeHosts, "remotesetup.tui.screen.hosts"}, {modeGroups, "remotesetup.tui.screen.groups"}}
	if model.recipe != nil {
		labels = append(labels, struct {
			screen mode
			code   string
		}{modeSteps, "remotesetup.tui.screen.steps"})
	}
	current := model.screen()
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		if label.screen == current {
			parts = append(parts, model.style.Selected(model.text(label.code)))
		} else {
			parts = append(parts, model.style.Dim(model.text(label.code)))
		}
	}
	return strings.Join(parts, "  ")
}

func (model Model) statusLine() string {
	if model.mode == modeConfirm {
		switch model.confirm {
		case confirmDeleteHost:
			return model.text("remotesetup.tui.confirm.delete_host", model.confirmTarget)
		case confirmDeleteGroup:
			return model.text("remotesetup.tui.confirm.delete_group", model.confirmTarget)
		case confirmDeleteStep:
			return model.text("remotesetup.tui.confirm.delete_step", model.confirmTarget)
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
	switch model.mode {
	case modeForm:
		return model.formBody()
	case modeGroupName:
		return model.text("remotesetup.tui.prompt.group_name") + "\n" + model.input.View() + "\n"
	case modeRecipeName:
		return model.text("remotesetup.tui.prompt.recipe_name") + "\n" + model.input.View() + "\n"
	case modeNumber:
		if model.numberGroup == "" {
			return model.text("remotesetup.tui.prompt.parallel") + "\n" + model.input.View() + "\n"
		}
		return model.text("remotesetup.tui.prompt.group_parallel", model.numberGroup) + "\n" + model.input.View() + "\n"
	case modePick:
		return model.pickBody()
	}
	switch model.screen() {
	case modeGroups:
		return model.groupsBody()
	case modeSteps:
		return model.stepsBody()
	}
	return model.hostsBody()
}

func (model Model) hostsBody() string {
	inventory := model.editor.Inventory()
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

func (model Model) groupsBody() string {
	inventory := model.editor.Inventory()
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

func (model Model) stepsBody() string {
	recipe := model.recipe.Recipe()
	name := recipe.Name
	if name == "" {
		name = model.text("remotesetup.tui.recipe_unnamed")
	}
	var builder strings.Builder
	line := model.text("remotesetup.tui.recipe_line", name)
	if model.recipeLocation != "" {
		line += "  " + model.recipeLocation
	}
	builder.WriteString(model.style.Dim(ansi.Truncate(line, max(model.width, 20), "…")) + "\n")
	nameWidth := 0
	for _, step := range recipe.Steps {
		nameWidth = max(nameWidth, ansi.StringWidth(step.Name))
	}
	lines := make([]string, len(recipe.Steps))
	for index, step := range recipe.Steps {
		action := step.Command
		if step.Upload != nil {
			action = model.text("remotesetup.tui.upload_line", step.Upload.Source, step.Upload.Destination)
		}
		text := model.cell(step.Name, nameWidth) + "  " + action
		if len(step.Tags) > 0 {
			text += "  [" + strings.Join(step.Tags, ",") + "]"
		}
		lines[index] = text
	}
	builder.WriteString(model.listRows(lines, model.stepCursor, model.text("remotesetup.tui.steps_empty")))
	return builder.String()
}

func (model Model) pickBody() string {
	inventory := model.editor.Inventory()
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
	switch {
	case model.form.kind == formStep && model.form.editing >= 0:
		title = "remotesetup.tui.form.step_edit"
	case model.form.kind == formStep:
		title = "remotesetup.tui.form.step_add"
	case model.form.editing >= 0:
		title = "remotesetup.tui.form.edit"
	}
	labels := make([]string, len(model.form.labels))
	width := 0
	for index, code := range model.form.labels {
		labels[index] = model.text(code)
		width = max(width, ansi.StringWidth(labels[index]))
	}
	var builder strings.Builder
	builder.WriteString(model.text(title) + "\n\n")
	for index, label := range labels {
		marker := "  "
		if index == model.form.focus {
			marker = model.style.Key("> ")
		}
		builder.WriteString(fmt.Sprintf("%s%s  %s\n", marker, model.cell(label, width), model.form.inputs[index].View()))
	}
	return builder.String()
}
