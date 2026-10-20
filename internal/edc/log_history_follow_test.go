package edc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func followFixture(key string, started time.Time, outcome logHistoryOutcome) logHistoryAttempt {
	row := logHistoryAttempt{Key: key, Command: []string{"make", "test"}, CWD: "/tmp", Path: "/logs/job.log", Started: started, Outcome: outcome, Attempt: 1}
	if outcome != historyOutcomeRunning && outcome != historyOutcomeUnknown {
		exit := 0
		if outcome == historyOutcomeFail {
			exit = 2
		}
		row.Exit, row.Ended, row.Duration = &exit, started.Add(time.Second), time.Second
	}
	return row
}

func runFollowFixture(t *testing.T, options logHistoryOptions, snapshots [][]logHistoryAttempt) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	load := func() (logHistorySnapshot, error) {
		rows := snapshots[min(calls, len(snapshots)-1)]
		calls++
		if calls >= len(snapshots) {
			cancel()
		}
		return logHistorySnapshot{Rows: rows}, nil
	}
	var stdout, stderr strings.Builder
	code := streamLogHistoryFollow(ctx, &stdout, &stderr, time.Millisecond, options, false, load)
	return stdout.String(), stderr.String(), code
}

func TestLogHistoryFollowEmitsStartAndEndOnce(t *testing.T) {
	key, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	base := time.Now().Add(-time.Minute)
	old := followFixture(other, base.Add(-time.Hour), historyOutcomeSuccess)
	running := followFixture(key, base, historyOutcomeRunning)
	finished := followFixture(key, base, historyOutcomeFail)
	fresh := followFixture(other, base.Add(time.Second), historyOutcomeSuccess)
	output, stderr, code := runFollowFixture(t, logHistoryOptions{JSON: "-"}, [][]logHistoryAttempt{
		{running, old},
		{running, old},
		{fresh, finished, old},
		{fresh, finished, old},
	})
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var events []logHistoryFollowEvent
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var event logHistoryFollowEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	want := []string{"start " + key, "end " + key, "start " + other, "end " + other, "summary "}
	if len(events) != len(want) {
		t.Fatalf("events: %s", output)
	}
	for index, event := range events {
		if event.Type+" "+event.Key != want[index] {
			t.Fatalf("event %d = %s %s; output %s", index, event.Type, event.Key, output)
		}
	}
	if events[1].Outcome != "FAIL" || events[1].Exit == nil || *events[1].Exit != 2 || events[1].DurationMS == nil {
		t.Fatalf("end event lost its result: %+v", events[1])
	}
	if !strings.Contains(output, `"started":2,"ended":2,"failed":1`) {
		t.Fatalf("summary: %s", output)
	}
}

func TestLogHistoryFollowWaitsBeforeReportingACrash(t *testing.T) {
	key := strings.Repeat("c", 64)
	started := time.Now()
	running := followFixture(key, started, historyOutcomeRunning)
	lost := followFixture(key, started, historyOutcomeUnknown)
	output, _, _ := runFollowFixture(t, logHistoryOptions{}, [][]logHistoryAttempt{{running}, {lost}, {running}, {lost}, {lost}})
	unknown := T("cli.log_history.outcome.unknown")
	if strings.Count(output, unknown) != 1 || strings.Count(output, T("cli.log_history.follow_start")) != 1 {
		t.Fatalf("crash reported early or twice:\n%s", output)
	}
}

func TestLogHistoryFollowFiltersFailuresAndKeys(t *testing.T) {
	key, other := strings.Repeat("d", 64), strings.Repeat("e", 64)
	started := time.Now()
	output, _, _ := runFollowFixture(t, logHistoryOptions{Failed: true, Key: key[:12]}, [][]logHistoryAttempt{
		{},
		{followFixture(key, started, historyOutcomeSuccess), followFixture(key, started.Add(time.Second), historyOutcomeFail), followFixture(other, started, historyOutcomeFail)},
	})
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], T("cli.log_history.outcome.fail")) || !strings.Contains(lines[0], "exit 2") {
		t.Fatalf("filtered output:\n%s", output)
	}
}

func TestLogHistoryJSONRequiresFollow(t *testing.T) {
	var stderr strings.Builder
	if _, err := parseLogHistoryOptions([]string{"--json", "-"}, &stderr); err == nil {
		t.Fatal("--json without --follow was accepted")
	}
	options, err := parseLogHistoryOptions([]string{"--follow", "--json", "-", "--command", "make"}, &stderr)
	if err != nil || !options.Follow || options.JSON != "-" || options.Name != "make" {
		t.Fatalf("options=%+v err=%v", options, err)
	}
}
