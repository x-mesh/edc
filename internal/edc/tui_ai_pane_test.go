package edc

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func aiPaneTestModel(width int) aiModel {
	now := time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	return aiModel{
		now: now, poll: time.Minute, rowSize: time.Minute, width: width, height: 30, scanned: true,
		sessionFeed: &aiSessionFeed{}, sessionsRead: true, sessionsHaveEvents: true,
		sessions: []aiSession{
			{Tool: "claude", State: aiStateNeeds, Reason: "permission", Detail: "Bash: rm -rf target", Cwd: "/w/edc", Since: now.Add(-time.Minute)},
			{Tool: "codex", State: aiStateWorking, Cwd: "/w/local", Since: now.Add(-2 * time.Minute),
				Activity: &aiActivity{Kind: "tool", Text: "exec_command: go test ./...", At: now.Add(-30 * time.Second), Running: true}},
		},
	}
}

func TestAIDashboardShowsSessionsBesideTheTableWhenWide(t *testing.T) {
	view := ansi.Strip(aiPaneTestModel(150).View().Content)
	lines := strings.Split(view, "\n")
	if !strings.Contains(lines[0], "│ sessions · ! 1 · ▸ 1") {
		t.Fatalf("the first line must carry the pane title:\n%s", view)
	}
	if !strings.Contains(view, "! cc edc") || !strings.Contains(view, "30s · exec_comm") {
		t.Fatalf("the pane must list the sessions:\n%s", view)
	}
	for index, line := range lines {
		if width := ansi.StringWidth(line); width > 150 {
			t.Fatalf("line %d is %d wide", index, width)
		}
	}
	if os.Getenv("EDC_SHOW_VIEW") != "" {
		t.Log("\n" + view)
	}
}

func TestAIDashboardShowsOnlyACountWhenNarrow(t *testing.T) {
	view := ansi.Strip(aiPaneTestModel(100).View().Content)
	if strings.Contains(view, "sessions ·") || !strings.Contains(strings.Split(view, "\n")[0], "! 1 needs") {
		t.Fatalf("a narrow screen keeps the table and counts the waiting sessions in the title:\n%s", view)
	}
}

// 세션 칸도 세션 줄 아래에 그 세션에서 도는 작업을 들여 쓴다.
func TestAIDashboardPaneShowsRunningChildren(t *testing.T) {
	model := aiPaneTestModel(150)
	model.sessions[0].Tasks = []aiTask{{Kind: "shell", ID: "bjk7nsvut", Depth: 1, Description: "full test run", Started: model.now.Add(-3 * time.Minute)}}
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "└ $ shell") || !strings.Contains(view, "full test run") {
		t.Fatalf("the pane must show the running shell under its session:\n%s", view)
	}
	for index, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > 150 {
			t.Fatalf("line %d is %d wide", index, width)
		}
	}
}
