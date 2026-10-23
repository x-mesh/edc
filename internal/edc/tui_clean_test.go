package edc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func cleanProgressFixture() cleanProgressModel {
	progress := newCleanProgress(io.Discard, "/home/demo", false)
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	progress.started = now
	progress.now = func() time.Time { return now }
	end := progress.begin("caches", "")
	now = now.Add(13400 * time.Millisecond)
	end()
	end = progress.begin("projects", "")
	now = now.Add(7900 * time.Millisecond)
	end()
	progress.begin("docker", "")
	progress.begin("docker_request", "/v1.41/system/df?type=volume")
	now = now.Add(2200 * time.Millisecond)
	model := newCleanProgressModel(progress, 10, nil)
	model.color = false
	return model
}

func TestCleanProgressBoardGolden(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	model := cleanProgressFixture()
	actual := model.View().Content
	path := filepath.Join("testdata", "clean-progress.golden")
	if os.Getenv("EDC_UPDATE_CLEAN_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(actual), 0644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(expected) {
		t.Fatalf("clean progress frame differs:\n%s", actual)
	}
	if model.View().AltScreen {
		t.Fatal("progress must stay in shell scrollback")
	}
}

func TestCleanProgressBoardResizesAndPreservesUnicode(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		model := cleanProgressFixture()
		mixed := "~/项目/e\u0301/👩‍💻/🇰🇷/خدمة/資料"
		model.snapshot.active[0].target = mixed
		for _, width := range []int{1, 20, 39, 40, 59, 60, 80, 120, 180} {
			for _, height := range []int{1, 4, 5, 10, 15, 24} {
				next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: height})
				resized := next.(cleanProgressModel)
				view := resized.View().Content
				if liveLineCount(view) > height {
					t.Fatalf("%s %dx%d height overflow: %q", language, width, height, view)
				}
				for _, line := range strings.Split(view, "\n") {
					if ansi.StringWidth(line) > width {
						t.Fatalf("%s width %d overflow: %q", language, width, line)
					}
				}
			}
		}
		model.width = 180
		if !strings.Contains(model.View().Content, mixed) {
			t.Fatalf("logical Unicode text changed: %s", model.View().Content)
		}
	}
}

func TestCleanProgressBoardCancelsOnceAndIgnoresPaste(t *testing.T) {
	model := cleanProgressFixture()
	cancelled := 0
	model.cancel = func() { cancelled++ }
	next, _ := model.Update(tea.PasteMsg{Content: "q\x1b"})
	model = next.(cleanProgressModel)
	if cancelled != 0 {
		t.Fatal("paste cancelled the scan")
	}
	next, _ = model.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	model = next.(cleanProgressModel)
	if !model.help {
		t.Fatal("help did not open")
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: tea.KeyEscape}} {
		next, cmd := model.Update(key)
		model = next.(cleanProgressModel)
		if cmd != nil {
			t.Fatal("cancel must wait for partial results")
		}
	}
	if cancelled != 1 || !model.cancelling {
		t.Fatalf("cancelled %d times", cancelled)
	}
	next, cmd := model.Update(cleanProgressDone{snapshot: model.snapshot, result: Result{Status: StatusWarn}})
	model = next.(cleanProgressModel)
	if !model.done || cmd == nil {
		t.Fatal("completed scan did not close")
	}
	if !strings.Contains(model.View().Content, T("clean.ui.stopped")) {
		t.Fatal("cancelled state missing")
	}
}

func TestCleanProgressBoardSnapshotsAreIndependentAndRedacted(t *testing.T) {
	progress := newCleanProgress(io.Discard, "/home/demo", true)
	end := progress.begin("caches", "")
	progress.begin("measure", "/home/demo/資料/192.0.2.10\x1b[31m")
	before := progress.snapshot()
	end()
	after := progress.snapshot()
	if !before.stages["caches"].finished.IsZero() || after.stages["caches"].finished.IsZero() {
		t.Fatal("snapshot shares mutable stage state")
	}
	if target := after.active[0].target; strings.Contains(target, "192.0.2.10") || strings.Contains(target, "\x1b") || !strings.Contains(target, "~/資料/") {
		t.Fatalf("unsafe target %q", target)
	}
}

func TestCleanProgressBoardFinishStatesAndSuspend(t *testing.T) {
	for _, status := range []Status{StatusPass, StatusWarn, StatusFail} {
		model := cleanProgressFixture()
		next, cmd := model.Update(cleanProgressDone{snapshot: model.snapshot, result: Result{Status: status}})
		model = next.(cleanProgressModel)
		key := "clean.ui.finished"
		if status == StatusWarn {
			key = "clean.ui.partial"
		}
		if status == StatusFail {
			key = "clean.ui.failed"
		}
		if cmd == nil || !strings.Contains(model.View().Content, T(key)) {
			t.Fatalf("missing finish state %v", status)
		}
		_, cmd = model.Update(cleanProgressTick{snapshot: model.snapshot})
		if cmd != nil {
			t.Fatal("finished board scheduled a redraw")
		}
	}
	model := cleanProgressFixture()
	_, cmd := model.Update(tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("Ctrl-Z did not suspend")
	}
	if _, ok := cmd().(tea.SuspendMsg); !ok {
		t.Fatal("Ctrl-Z was repurposed")
	}
}

func TestCleanProgressBoardDoesNotCallCancelledStagesFinished(t *testing.T) {
	progress := newCleanProgress(io.Discard, "/home/demo", false)
	progress.cancelled = func() bool { return true }
	progress.begin("worktrees", "")()
	model := newCleanProgressModel(progress, 4, nil)
	model.color = false
	next, _ := model.Update(cleanProgressDone{snapshot: progress.snapshot(), result: Result{Status: StatusWarn}})
	model = next.(cleanProgressModel)
	if !model.snapshot.stages["worktrees"].skipped || !strings.Contains(model.View().Content, T("clean.ui.skipped")) {
		t.Fatal("cancelled stage shown as finished")
	}
}
