package edc

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func confirmAfter(t *testing.T, model confirmModel, keys ...tea.KeyPressMsg) (confirmModel, tea.Cmd) {
	t.Helper()
	var current tea.Model = model
	var cmd tea.Cmd
	for _, key := range keys {
		current, cmd = current.Update(key)
	}
	updated, ok := current.(confirmModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated, cmd
}

func TestConfirmModelMovesAndAccepts(t *testing.T) {
	model := newConfirmModel("실행할까요?", false)
	if model.yes {
		t.Fatal("default must be no")
	}
	moved, _ := confirmAfter(t, model, tea.KeyPressMsg{Code: tea.KeyLeft})
	if !moved.yes {
		t.Fatal("left must move to yes")
	}
	accepted, cmd := confirmAfter(t, moved, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !accepted.done || !accepted.yes || cmd == nil {
		t.Fatalf("model = %#v", accepted)
	}
	if !strings.HasPrefix(accepted.View().Content, "실행할까요? "+confirmYesLabel()+"\n") {
		t.Fatalf("final view = %q", accepted.View().Content)
	}
	if liveLineCount(accepted.View().Content) != accepted.frameHeight() {
		t.Fatalf("final view height = %d, want %d", liveLineCount(accepted.View().Content), accepted.frameHeight())
	}
}

func TestConfirmModelShortcuts(t *testing.T) {
	yes, cmd := confirmAfter(t, newConfirmModel("실행할까요?", false), tea.KeyPressMsg{Code: 'y', Text: "y"})
	if !yes.done || !yes.yes || cmd == nil {
		t.Fatalf("y = %#v", yes)
	}
	no, cmd := confirmAfter(t, newConfirmModel("실행할까요?", true), tea.KeyPressMsg{Code: 'n', Text: "n"})
	if !no.done || no.yes || cmd == nil {
		t.Fatalf("n = %#v", no)
	}
	if !strings.HasPrefix(no.View().Content, "실행할까요? "+confirmNoLabel()+"\n") {
		t.Fatalf("final view = %q", no.View().Content)
	}
}

func TestConfirmModelCancels(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: 'q', Text: "q"}, {Code: 'c', Mod: tea.ModCtrl}} {
		model, cmd := confirmAfter(t, newConfirmModel("실행할까요?", false), key)
		if !model.cancelled || cmd == nil {
			t.Fatalf("%s did not cancel: %#v", key.String(), model)
		}
		if strings.TrimSpace(model.View().Content) != "" {
			t.Fatalf("cancelled view = %q", model.View().Content)
		}
	}
}

func TestConfirmModelViewShowsBothOptions(t *testing.T) {
	view := newConfirmModel("실행할까요?", false).View().Content
	// 질문과 선택지, 키 안내가 한 줄에 있고 고른 쪽만 반전된다.
	for _, expected := range []string{"실행할까요?", confirmYesLabel(), liveReverse + liveSelectedBar + " " + confirmNoLabel(), confirmHelp()} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view %q does not contain %q", view, expected)
		}
	}
	if liveLineCount(view) != 1 {
		t.Fatalf("확인은 한 줄이어야 한다: %q", view)
	}
	if strings.Count(view, liveReverse) != 1 {
		t.Fatalf("한 쪽만 반전되어야 한다: %q", view)
	}
}
