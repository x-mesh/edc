package edc

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func topDeltaModel(t *testing.T) topModel {
	t.Helper()
	model := topFixtureModel(nil)
	filter, err := parseTopProcessFilter("42")
	if err != nil {
		t.Fatal(err)
	}
	model.processFilter, model.view, model.detail = filter, topViewProcess, true
	model.rows = topDeltaRows(100<<20, 120<<20, 150<<20)
	model.selected, model.width, model.height = 2, 80, 24
	model.limits.color = false
	return model
}

func TestTopDeltaDetailShowsObservedChangeAndUnavailableWarmup(t *testing.T) {
	model := topDeltaModel(t)
	for _, width := range []int{80, 140} {
		model.width = width
		text := ansi.Strip(model.View().Content)
		for _, want := range []string{"RSS pid 42: 150.0M", "+50.0 MB", "2m0s", "3 observed samples"} {
			if !strings.Contains(text, want) {
				t.Fatalf("width %d lost %s: %s", width, want, text)
			}
		}
		if strings.Contains(strings.ToLower(text), "leak") {
			t.Fatalf("endpoint change became a leak verdict: %s", text)
		}
		for _, line := range strings.Split(text, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("line exceeds %d: %s", width, line)
			}
		}
	}
	model.rows = model.rows[:1]
	model.selected = 0
	text := model.View().Content
	if !strings.Contains(text, "change unavailable") || strings.Contains(text, "0 B over") {
		t.Fatalf("warmup invented zero change: %s", text)
	}
}

func TestTopDeltaDetailFollowsSelectedHistoricalRow(t *testing.T) {
	model := topDeltaModel(t)
	model.selected = 1
	model.follow = false
	text := model.View().Content
	if !strings.Contains(text, "RSS pid 42: 120.0M") || !strings.Contains(text, "+20.0 MB") || !strings.Contains(text, "2 observed samples") || strings.Contains(text, "+50.0 MB") {
		t.Fatalf("historical delta mixed future data: %s", text)
	}
	model.rows[1].filter = "other"
	text = model.View().Content
	if !strings.Contains(text, "RSS change unavailable") || strings.Contains(text, "+20.0 MB") {
		t.Fatalf("other filter shown as current observations: %s", text)
	}
}

func TestTopDeltaRowsPreserveProcessObservationTimestamp(t *testing.T) {
	model := topFixtureModel(nil)
	at := model.previous.TakenAt.Add(time.Second)
	observedAt := at.Add(-100 * time.Millisecond)
	snapshot := resourceSnapshot{TakenAt: at, ProcessesAt: observedAt, ProcessesValid: true, Processes: []topProcess{{PID: 42, Started: at.Add(-time.Hour), RSS: 100}}}
	next, _ := model.Update(topSampleMsg{snapshot: snapshot})
	model = next.(topModel)
	if len(model.rows) != 1 || !model.rows[0].processesAt.Equal(observedAt) {
		t.Fatalf("observation timestamp lost: %+v", model.rows)
	}
}
