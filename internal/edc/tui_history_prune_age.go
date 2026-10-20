package edc

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var historyPruneAges = []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 90 * 24 * time.Hour, 180 * 24 * time.Hour, 365 * 24 * time.Hour}

type historyPruneAgePicker struct {
	selected  int
	input     bool
	text, err string
}

func historyPruneAgeLabel(age time.Duration) string {
	switch {
	case age%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", age/(24*time.Hour))
	case age%time.Hour == 0:
		return fmt.Sprintf("%dh", age/time.Hour)
	case age%time.Minute == 0:
		return fmt.Sprintf("%dm", age/time.Minute)
	default:
		return age.String()
	}
}

func pruneAgeInputText(text string) string {
	var runes []rune
	for _, r := range text {
		if !unicode.IsControl(r) && len(runes) < 64 {
			runes = append(runes, r)
		}
	}
	return string(runes)
}

func (model *historyReviewBrowser) openPruneAgePicker() {
	picker := &historyPruneAgePicker{selected: len(historyPruneAges), text: historyPruneAgeLabel(model.pruneOptions.olderThan)}
	for i, age := range historyPruneAges {
		if age == model.pruneOptions.olderThan {
			picker.selected = i
			break
		}
	}
	model.agePicker = picker
}

func (model historyReviewBrowser) applyPruneAge(age time.Duration) (tea.Model, tea.Cmd) {
	if model.loadPrune == nil {
		return model, nil
	}
	model.pruneOptions.olderThan = age
	model.seq++
	model.agePicker = nil
	model.expanded, model.confirm, model.selected = false, false, 0
	model.checked = map[string]bool{}
	model.data = historyReviewData{summary: T("history.review.age_loading", historyPruneAgeLabel(age)), empty: T("history.review.age_loading", historyPruneAgeLabel(age)), pruneOptions: model.pruneOptions}
	model.notice = ""
	model.resize()
	return model, model.reload()
}

func (model historyReviewBrowser) updatePruneAge(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	picker := model.agePicker
	if key.String() == "ctrl+c" {
		return model, tea.Quit
	}
	if key.String() == "esc" {
		model.agePicker = nil
		return model, nil
	}
	if picker.input {
		switch key.String() {
		case "enter":
			age, err := parseLogAge(strings.TrimSpace(picker.text))
			if err != nil || age <= 0 {
				picker.err = T("history.review.age_invalid")
				return model, nil
			}
			return model.applyPruneAge(age)
		case "backspace", "ctrl+h":
			runes := []rune(picker.text)
			if len(runes) > 0 {
				picker.text = string(runes[:len(runes)-1])
			}
		case "ctrl+u":
			picker.text = ""
		default:
			picker.text = pruneAgeInputText(picker.text + key.Text)
		}
		picker.err = ""
		return model, nil
	}
	switch key.String() {
	case "q":
		return model, tea.Quit
	case "up", "k":
		picker.selected = max(0, picker.selected-1)
	case "down", "j":
		picker.selected = min(len(historyPruneAges), picker.selected+1)
	case "home":
		picker.selected = 0
	case "end":
		picker.selected = len(historyPruneAges)
	case "enter":
		if picker.selected == len(historyPruneAges) {
			picker.input = true
			picker.text = ""
			return model, nil
		}
		return model.applyPruneAge(historyPruneAges[picker.selected])
	}
	return model, nil
}

func (model historyReviewBrowser) pruneAgeView() tea.View {
	if model.width < 40 || model.height < 10 {
		view := tea.NewView(ansi.Truncate(T("cli.log_history.small"), max(1, model.width), "…"))
		view.AltScreen = true
		return view
	}
	picker := model.agePicker
	lines := []string{historyStrong(T("history.review.age_title"), richCyan, model.color), historyTint(T("history.review.age_current", historyPruneAgeLabel(model.pruneOptions.olderThan)), richGray, model.color), ""}
	key := "history.review.age_help"
	var body []string
	selected := picker.selected
	if picker.input {
		key = "history.review.age_input_help"
		body = []string{T("history.review.age_custom"), "", historyStrong(historyTerminalText(picker.text)+"█", richCyan, model.color)}
		if picker.err != "" {
			body = append(body, "", historyTint(picker.err, richYellow, model.color))
		}
		selected = 0
	} else {
		for i, age := range historyPruneAges {
			text := T("history.review.age_option", historyPruneAgeLabel(age))
			if i == picker.selected {
				text = historySelect(text, model.color)
			} else {
				text = "  " + text
			}
			body = append(body, text)
		}
		text := T("history.review.age_custom")
		if picker.selected == len(historyPruneAges) {
			text = historySelect(text, model.color)
		} else {
			text = "  " + text
		}
		body = append(body, text)
	}
	footer := historyReviewKeyHints(T(key), model.width, model.color)
	available := max(1, model.height-len(lines)-len(footer))
	start := max(0, selected-available+1)
	end := min(len(body), start+available)
	lines = append(lines, body[start:end]...)
	for len(lines)+len(footer) < model.height {
		lines = append(lines, "")
	}
	lines = append(lines, footer...)
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], model.width, "…")
	}
	view := tea.NewView(strings.Join(lines[:min(len(lines), model.height)], "\n"))
	view.AltScreen = true
	return view
}
