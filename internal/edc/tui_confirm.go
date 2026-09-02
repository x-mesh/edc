package edc

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

const (
	confirmYesLabel = "예"
	confirmNoLabel  = "아니오"
	confirmHelp     = "←/→ 이동   Enter 선택   y/n 바로 답하기"
)

type confirmModel struct {
	detail    string // 질문 위에 남기는 설명. 확인 뒤에도 화면에 남는다.
	question  string
	yes       bool
	done      bool
	cancelled bool
}

func newConfirmModel(question string, initial bool) confirmModel {
	return confirmModel{question: question, yes: initial}
}

func newDetailedConfirmModel(detail, question string, initial bool) confirmModel {
	return confirmModel{detail: detail, question: question, yes: initial}
}

func (model confirmModel) Init() tea.Cmd { return nil }

func (model confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		return model, nil
	}
	switch key.String() {
	case "left", "h", "right", "l", "tab":
		model.yes = !model.yes
	case "y":
		model.yes = true
		model.done = true
		return model, tea.Quit
	case "n":
		model.yes = false
		model.done = true
		return model, tea.Quit
	case "enter":
		model.done = true
		return model, tea.Quit
	case "esc", "q", "ctrl+c":
		model.cancelled = true
		return model, tea.Quit
	}
	return model, nil
}

func (model confirmModel) View() tea.View {
	if model.cancelled {
		return liveFrame("", model.frameHeight())
	}
	if model.done {
		return liveFrame(model.detail+model.question+" "+model.answerLabel()+"\n", model.frameHeight())
	}
	var builder strings.Builder
	builder.WriteString(model.detail)
	builder.WriteString(confirmPrompt(model.question, model.yes, true) + "\n")
	return liveFrame(builder.String(), model.frameHeight())
}

// frameHeight는 설명과 질문을 모두 보여 줄 때의 줄 수다. 답한 뒤에도 이 높이를 유지한다.
func (model confirmModel) frameHeight() int {
	return liveLineCount(model.detail) + 1
}

// confirmPrompt는 질문과 선택지, 키 안내를 한 줄에 담는다. 고른 쪽은 반전으로 칠한다.
func confirmPrompt(question string, yes, color bool) string {
	return question + "   " + confirmOption(confirmYesLabel, yes, color) +
		"  " + confirmOption(confirmNoLabel, !yes, color) +
		"      " + liveMuted(confirmHelp, color)
}

func (model confirmModel) answerLabel() string {
	if model.yes {
		return confirmYesLabel
	}
	return confirmNoLabel
}

// confirmOption은 선택지 하나를 그린다. 고른 쪽은 막대와 반전으로 표시한다.
func confirmOption(label string, selected, color bool) string {
	if selected {
		return liveSelected(liveSelectedBar+" "+label+" ", color)
	}
	return liveIdleBar + " " + label + " "
}
