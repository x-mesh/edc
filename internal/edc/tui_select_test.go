package edc

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func pressKey(model tea.Model, key string) (tea.Model, tea.Cmd) {
	return model.Update(tea.KeyPressMsg{Code: keyCode(key), Text: keyText(key)})
}

func keyCode(key string) rune {
	switch key {
	case "up":
		return tea.KeyUp
	case "down":
		return tea.KeyDown
	case "enter":
		return tea.KeyEnter
	case "esc":
		return tea.KeyEscape
	default:
		return rune(key[0])
	}
}

func keyText(key string) string {
	if len(key) == 1 {
		return key
	}
	return ""
}

func selectAfter(t *testing.T, model selectModel, keys ...string) selectModel {
	t.Helper()
	var current tea.Model = model
	for _, key := range keys {
		current, _ = pressKey(current, key)
	}
	updated, ok := current.(selectModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated
}

func TestSelectModelCursorWraps(t *testing.T) {
	model := newSelectModel(selectGroupTitle, selectGroupLabel, selectItemsFromValues([]string{"daily", "weekly", "monthly"}))
	if got := selectAfter(t, model, "down", "down").cursor; got != 2 {
		t.Fatalf("cursor = %d, want 2", got)
	}
	if got := selectAfter(t, model, "up").cursor; got != 2 {
		t.Fatalf("up from the first item must wrap to the last, got %d", got)
	}
	if got := selectAfter(t, model, "down", "down", "down").cursor; got != 0 {
		t.Fatalf("down from the last item must wrap to the first, got %d", got)
	}
	if got := selectAfter(t, model, "j", "k").cursor; got != 0 {
		t.Fatalf("j and k must move the cursor, got %d", got)
	}
}

func TestSelectModelEnterChooses(t *testing.T) {
	model := newSelectModel(selectGroupTitle, selectGroupLabel, selectItemsFromValues([]string{"daily", "weekly"}))
	updated, cmd := pressKey(selectAfter(t, model, "down"), "enter")
	final, ok := updated.(selectModel)
	if !ok || !final.done {
		t.Fatalf("model = %#v", updated)
	}
	if cmd == nil {
		t.Fatal("enter must quit the program")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatalf("command = %#v", cmd())
	}
	group, err := final.choice()
	if err != nil || group != "weekly" {
		t.Fatalf("choice = %q, %v", group, err)
	}
	// 목록은 그 자리에 남고 첫 줄이 답으로 바뀐다. 줄 수는 그대로여야 한다.
	view := final.View()
	if view.AltScreen {
		t.Fatal("selector must stay inline")
	}
	if !strings.HasPrefix(view.Content, selectGroupLabel+"\n") {
		t.Fatalf("final view = %q", view.Content)
	}
	if !strings.Contains(view.Content, liveSelectedBar+" weekly") {
		t.Fatalf("final view must mark the chosen item: %q", view.Content)
	}
	// 고른 뒤에는 반전을 걷어 기록으로만 남긴다.
	if strings.Contains(view.Content, liveReverse) {
		t.Fatalf("final view must not stay reversed: %q", view.Content)
	}
	if liveLineCount(view.Content) != final.frameHeight() {
		t.Fatalf("frame height changed: %d, want %d", liveLineCount(view.Content), final.frameHeight())
	}
}

func TestSelectModelCancels(t *testing.T) {
	model := newSelectModel(selectGroupTitle, selectGroupLabel, selectItemsFromValues([]string{"daily", "weekly"}))
	for _, key := range []string{"esc", "q", "ctrl+c"} {
		updated, cmd := model.Update(tea.KeyPressMsg{Code: cancelCode(key), Mod: cancelMod(key)})
		final, ok := updated.(selectModel)
		if !ok || !final.cancelled {
			t.Fatalf("%s did not cancel: %#v", key, updated)
		}
		if cmd == nil {
			t.Fatalf("%s must quit the program", key)
		}
		if _, err := final.choice(); !errors.Is(err, errRemoteCancelled) {
			t.Fatalf("%s choice error = %v", key, err)
		}
		content := final.View().Content
		if !strings.HasPrefix(content, selectGroupLabel+": 취소\n") {
			t.Fatalf("%s view = %q", key, content)
		}
		if strings.Contains(content, liveSelectedBar) || strings.Contains(content, liveReverse) {
			t.Fatalf("%s must clear the highlight: %q", key, content)
		}
		if liveLineCount(content) != final.frameHeight() {
			t.Fatalf("%s changed the frame height: %q", key, content)
		}
	}
}

func cancelCode(key string) rune {
	switch key {
	case "esc":
		return tea.KeyEscape
	case "q":
		return 'q'
	default:
		return 'c'
	}
}

func cancelMod(key string) tea.KeyMod {
	if key == "ctrl+c" {
		return tea.ModCtrl
	}
	return 0
}

func TestSelectModelViewListsItems(t *testing.T) {
	model := newSelectModel(selectGroupTitle, selectGroupLabel, selectItemsFromValues([]string{"daily", "weekly"}))
	view := model.View().Content
	// 고른 줄은 막대와 반전으로, 나머지는 그대로 그린다.
	for _, expected := range []string{selectGroupTitle + "\n", liveReverse + liveSelectedBar + " daily", "  weekly\n"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view %q does not contain %q", view, expected)
		}
	}
	if strings.Count(view, liveReverse) != 1 {
		t.Fatalf("한 줄만 반전되어야 한다: %q", view)
	}
	if !strings.HasSuffix(view, "\n") {
		t.Fatalf("view must end with a newline: %q", view)
	}
}
