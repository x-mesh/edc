package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func writeAIFixture(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendAIFixture(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func aiFixtureTime(now time.Time) string {
	return now.Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
}

// Claude Code는 응답 하나를 content block마다 한 줄씩 쓰고, 앞선 줄의 output은 덜 찬 값이다.
func TestAIClaudeScanCountsEachResponseOnce(t *testing.T) {
	claudeDir, now := t.TempDir(), time.Now()
	at := aiFixtureTime(now)
	writeAIFixture(t, filepath.Join(claudeDir, "projects", "app", "session.jsonl"),
		`{"timestamp":"`+at+`","requestId":"r1","message":{"id":"m1","usage":{"input_tokens":10,"cache_read_input_tokens":100,"output_tokens":1}}}`+"\n",
		`{"timestamp":"`+at+`","requestId":"r1","message":{"id":"m1","usage":{"input_tokens":10,"cache_read_input_tokens":100,"output_tokens":50}}}`+"\n",
		`{"timestamp":"`+at+`","requestId":"r2","message":{"id":"m2","usage":{"input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":5}}}`+"\n",
		`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n",
	)
	usage := newAITokenScanner(claudeDir, t.TempDir()).scan(now)
	want := aiUsage{Requests: 2, Input: 18, Output: 55, Cache: 100}
	if got := aiSumBuckets(usage, "claude"); got != want {
		t.Fatalf("claude usage = %+v, want %+v", got, want)
	}
}

func aiSumBuckets(snapshot aiUsageSnapshot, provider string) aiUsage {
	var total aiUsage
	for _, usage := range snapshot.Buckets[provider] {
		total.add(usage)
	}
	return total
}

func TestAICodexScanAddsTheGrowthOfTheTotal(t *testing.T) {
	codexDir, now := t.TempDir(), time.Now()
	at := aiFixtureTime(now)
	event := func(input, cached, output, total string) string {
		return `{"timestamp":"` + at + `","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":` + input +
			`,"cached_input_tokens":` + cached + `,"output_tokens":` + output + `,"total_tokens":` + total + `}}}}` + "\n"
	}
	writeAIFixture(t, filepath.Join(codexDir, "sessions", "2026", "10", "06", "rollout.jsonl"),
		`{"timestamp":"`+at+`","type":"event_msg","payload":{"type":"token_count","info":null}}`+"\n",
		event("80", "0", "20", "100"), event("80", "0", "20", "100"), event("200", "50", "50", "250"),
	)
	usage := newAITokenScanner(t.TempDir(), codexDir).scan(now)
	// cached_input_tokens는 input_tokens 안에 있으므로 in에서 빠진다. 같은 누적값을 다시 적은 이벤트는 요청으로 세지 않는다.
	want := aiUsage{Requests: 2, Input: 150, Output: 50, Cache: 50}
	if got := aiSumBuckets(usage, "codex"); got != want || got.total() != 250 {
		t.Fatalf("codex usage = %+v (total %d), want %+v", got, got.total(), want)
	}
}

// 기록 중인 마지막 줄을 반만 읽으면 그 응답을 잃는다. 줄바꿈이 붙은 다음 scan에서 읽어야 한다.
func TestAIScanWaitsForTheLineToEnd(t *testing.T) {
	codexDir, now := t.TempDir(), time.Now()
	at := aiFixtureTime(now)
	path := filepath.Join(codexDir, "sessions", "rollout.jsonl")
	first := `{"timestamp":"` + at + `","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"output_tokens":10,"total_tokens":40}}}}` + "\n"
	partial := `{"timestamp":"` + at + `","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":70,"output_tokens":20,"total_tokens":90}}}}`
	writeAIFixture(t, path, first, partial)
	scanner := newAITokenScanner(t.TempDir(), codexDir)
	if got := aiSumBuckets(scanner.scan(now), "codex").total(); got != 40 {
		t.Fatalf("before the newline: codex tokens = %d, want 40", got)
	}
	appendAIFixture(t, path, "\n")
	if got := aiSumBuckets(scanner.scan(now), "codex").total(); got != 90 {
		t.Fatalf("after the newline: codex tokens = %d, want 90", got)
	}
}

func TestParseAIClaudeUsageKeepsKnownWindows(t *testing.T) {
	windows, err := parseAIClaudeUsage([]byte(`{
		"five_hour": {"utilization": 22, "resets_at": "2026-09-30T08:59:59.843792+00:00"},
		"seven_day": {"utilization": 18, "resets_at": "2026-10-06T07:59:59.843821+00:00"},
		"seven_day_opus": null,
		"iguana_necktie": {"utilization": 0, "resets_at": "2026-11-05T07:59:00+00:00", "limit_dollars": 250}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []aiWindow{
		{Name: "5h", Used: 22, ResetsAt: time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)},
		{Name: "7d", Used: 18, ResetsAt: time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)},
	}
	if len(windows) != len(want) {
		t.Fatalf("windows = %+v, want %+v", windows, want)
	}
	for index := range want {
		if windows[index].Name != want[index].Name || windows[index].Used != want[index].Used || !windows[index].ResetsAt.Equal(want[index].ResetsAt) {
			t.Errorf("window %d = %+v, want %+v", index, windows[index], want[index])
		}
	}
}

func TestParseAICodexRateLimitsNamesWindowsByDuration(t *testing.T) {
	plan, windows, err := parseAICodexRateLimits([]byte(`{
		"rateLimits": {"limitId": "codex", "planType": "prolite",
			"primary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 1791850925}, "secondary": null},
		"rateLimitsByLimitId": {
			"codex": {"limitId": "codex", "planType": "prolite",
				"primary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 1791850925}},
			"base_model_inference": {"limitId": "base_model_inference", "limitName": "gpt-reserve",
				"primary": {"usedPercent": 3, "windowDurationMins": 300, "resetsAt": 1791855328}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan != "prolite" {
		t.Errorf("plan = %q, want prolite", plan)
	}
	if len(windows) != 2 || windows[0].Name != "7d" || windows[1].Name != "5h gpt-reserve" {
		t.Fatalf("windows = %+v", windows)
	}
	if !windows[0].ResetsAt.Equal(time.Unix(1791850925, 0)) || windows[0].Used != 12 {
		t.Errorf("first window = %+v", windows[0])
	}
}

func TestDetectAIResetsNeedsALaterResetTime(t *testing.T) {
	now := time.Date(2026, 10, 6, 9, 0, 5, 0, time.UTC)
	before := aiProvider{Name: "claude", Windows: []aiWindow{{Name: "5h", Used: 64, ResetsAt: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}}}
	jitter := aiProvider{Name: "claude", Windows: []aiWindow{{Name: "5h", Used: 65, ResetsAt: time.Date(2026, 10, 6, 9, 0, 30, 0, time.UTC)}}}
	if events := detectAIResets(before, jitter, now); len(events) != 0 {
		t.Fatalf("a reset time that moved 30s is the same window: %+v", events)
	}
	after := aiProvider{Name: "claude", Windows: []aiWindow{{Name: "5h", Used: 1, ResetsAt: time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)}}}
	events := detectAIResets(before, after, now)
	if len(events) != 1 || events[0].Window != "5h" || events[0].UsedBefore != 64 || events[0].UsedAfter != 1 || !events[0].ResetAt.Equal(before.Windows[0].ResetsAt) {
		t.Fatalf("events = %+v", events)
	}
}

// 쓰지 않은 Codex 창은 리셋 시각이 매번 "지금 + 7일"로 밀린다. 리셋으로 기록하면 조회마다 한 줄씩 쌓인다.
func TestDetectAIResetsIgnoresAnUnusedWindowThatSlides(t *testing.T) {
	now := time.Date(2026, 10, 6, 2, 1, 44, 0, time.UTC)
	before := aiProvider{Name: "codex", Windows: []aiWindow{{Name: "7d gpt-reserve", Used: 0, ResetsAt: now.Add(7*24*time.Hour - time.Minute - time.Second)}}}
	slid := aiProvider{Name: "codex", Windows: []aiWindow{{Name: "7d gpt-reserve", Used: 0, ResetsAt: now.Add(7 * 24 * time.Hour)}}}
	if events := detectAIResets(before, slid, now.Add(time.Minute)); len(events) != 0 {
		t.Fatalf("a sliding unused window is no reset: %+v", events)
	}
	used := aiProvider{Name: "codex", Windows: []aiWindow{{Name: "7d", Used: 40, ResetsAt: now.Add(3 * 24 * time.Hour)}}}
	credit := aiProvider{Name: "codex", Windows: []aiWindow{{Name: "7d", Used: 0, ResetsAt: now.Add(7 * 24 * time.Hour)}}}
	if events := detectAIResets(used, credit, now); len(events) != 1 {
		t.Fatalf("an early reset that drops the usage is a reset: %+v", events)
	}
}

func TestAIResetLogKeepsTheLatestResetPerProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", aiResetLogName)
	first := aiResetEvent{Provider: "claude", Window: "5h", ResetAt: time.Date(2026, 10, 6, 4, 0, 0, 0, time.UTC)}
	second := aiResetEvent{Provider: "claude", Window: "5h", ResetAt: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	codex := aiResetEvent{Provider: "codex", Window: "7d", ResetAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if err := appendAIResets(path, []aiResetEvent{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := appendAIResets(path, []aiResetEvent{codex}); err != nil {
		t.Fatal(err)
	}
	last := loadAILastResets(path)
	if !last["claude"].ResetAt.Equal(second.ResetAt) || !last["codex"].ResetAt.Equal(codex.ResetAt) {
		t.Fatalf("last resets = %+v", last)
	}
}

func TestReadAIClaudeTokenRefusesAnExpiredToken(t *testing.T) {
	dir, now := t.TempDir(), time.Now()
	write := func(expires time.Time) {
		writeAIFixture(t, filepath.Join(dir, ".credentials.json"),
			`{"claudeAiOauth":{"accessToken":"test-token","expiresAt":`+strconv.FormatInt(expires.UnixMilli(), 10)+`,"subscriptionType":"max"}}`)
	}
	write(now.Add(-time.Minute))
	if _, plan, err := readAIClaudeToken(dir, now); !errors.Is(err, errAIAuthExpired) || plan != "max" {
		t.Fatalf("expired token: plan %q, err %v", plan, err)
	}
	write(now.Add(time.Hour))
	if token, _, err := readAIClaudeToken(dir, now); err != nil || token != "test-token" {
		t.Fatalf("valid token: %q, %v", token, err)
	}
}

func TestAICountdownShowsSeconds(t *testing.T) {
	for _, test := range []struct {
		left time.Duration
		want string
	}{
		{2*time.Hour + 13*time.Minute + 45*time.Second, "02:13:45"},
		{6*24*time.Hour + 6*time.Hour + 18*time.Minute + 11*time.Second, "6d 06:18:11"},
		{-time.Second, "00:00:00"},
	} {
		if got := aiCountdown(test.left); got != test.want {
			t.Errorf("aiCountdown(%s) = %q, want %q", test.left, got, test.want)
		}
	}
}

func aiDashboardFixture(now time.Time) aiModel {
	model := aiModel{poll: time.Minute, width: 80, height: 24, now: now, rowSize: time.Minute, polled: true, scanned: true,
		usage: aiUsageSnapshot{Buckets: map[string]map[time.Time]aiUsage{
			"claude": {now.Truncate(aiBucket): {Requests: 3, Input: 4100, Output: 1200, Cache: 412_000}},
			"codex":  {now.Add(-2 * time.Minute).Truncate(aiBucket): {Requests: 1, Input: 900, Output: 120}},
		}},
		providers: []aiProvider{
			{Name: "claude", Plan: "max", FetchedAt: now, Windows: []aiWindow{
				{Name: "5h", Used: 22, ResetsAt: now.Add(2 * time.Hour)},
				{Name: "7d", Used: 97, ResetsAt: now.Add(6 * 24 * time.Hour)},
			}},
			{Name: "codex", Plan: "prolite", FetchedAt: now, Err: "codex app-server exited", Windows: []aiWindow{
				{Name: "7d", Used: 0, ResetsAt: now.Add(7 * 24 * time.Hour)},
				{Name: "5h gpt-reserve", Used: 3, ResetsAt: now.Add(time.Hour)},
			}},
		},
	}
	return model
}

func TestAIDashboardFitsAnEightyColumnTerminal(t *testing.T) {
	model := aiDashboardFixture(time.Date(2026, 10, 6, 1, 0, 30, 0, time.Local))
	lines := strings.Split(model.View().Content, "\n")
	if len(lines) != model.height {
		t.Errorf("%d lines, want the %d-line terminal filled", len(lines), model.height)
	}
	for index, line := range lines {
		if width := ansi.StringWidth(line); width > model.width {
			t.Errorf("line %d is %d columns: %q", index+1, width, line)
		}
	}
	text := ansi.Strip(model.View().Content)
	for _, want := range []string{"▸01:00:00│   3│  4.1K│  1.2K│  412K│   417K│", "│00:58:00│   0│", "in 02:00:00", "in 6d 00:00:00", "! codex app-server exited", "│   Σ 10m│   3│  4.1K│  1.2K│  412K│   417K│   1│   900│   120│     0│   1.0K│", "- 10s [1m] 5m 1h +"} {
		if !strings.Contains(text, want) {
			t.Errorf("dashboard misses %q", want)
		}
	}
}

func TestAIRowsSumBucketsIntoRows(t *testing.T) {
	end := time.Date(2026, 10, 6, 2, 41, 7, 0, time.Local)
	snapshot := aiUsageSnapshot{Buckets: map[string]map[time.Time]aiUsage{"claude": {
		time.Date(2026, 10, 6, 2, 40, 0, 0, time.Local):  {Requests: 1, Output: 10},
		time.Date(2026, 10, 6, 2, 40, 50, 0, time.Local): {Requests: 2, Output: 20},
		time.Date(2026, 10, 6, 2, 41, 0, 0, time.Local):  {Requests: 1, Output: 5},
		time.Date(2026, 10, 6, 1, 59, 50, 0, time.Local): {Requests: 4, Output: 40},
	}}}
	minutes := snapshot.rows(end, time.Minute, 2)
	if minutes[0].Claude.Output != 30 || minutes[1].Claude.Output != 5 || !minutes[1].Start.Equal(time.Date(2026, 10, 6, 2, 41, 0, 0, time.Local)) {
		t.Fatalf("minute rows = %+v", minutes)
	}
	hours := snapshot.rows(end, time.Hour, 2)
	if hours[0].Claude.Requests != 4 || hours[1].Claude.Requests != 4 {
		t.Fatalf("hour rows = %+v", hours)
	}
}

func TestAIDashboardKeysMoveLikeTop(t *testing.T) {
	model := aiDashboardFixture(time.Now())
	model = model.updateKey("+")
	if model.rowSize != 5*time.Minute || !strings.Contains(ansi.Strip(model.statusLine(80)), "- 10s 1m [5m] 1h +") {
		t.Fatalf("+ gave rows %s, hint %q", model.rowSize, ansi.Strip(model.statusLine(80)))
	}
	model = model.updateKey("-").updateKey("-").updateKey("-")
	if model.rowSize != aiBucket {
		t.Fatalf("- stops at the shortest rows, got %s", model.rowSize)
	}
	model = model.updateKey("up").updateKey("up")
	if model.back != 2 || !strings.Contains(model.title(80), "history -2") {
		t.Fatalf("↑ twice: back %d, title %q", model.back, model.title(80))
	}
	if strings.Contains(ansi.Strip(model.View().Content), "▸") {
		t.Error("a history view must not mark a live row")
	}
	model = model.updateKey("end")
	if model.back != 0 {
		t.Fatalf("End must return live, back %d", model.back)
	}
	model.rowSize = time.Hour
	for range 100 {
		model = model.updateKey("up")
	}
	if model.back != model.maxBack() || model.back > 24 {
		t.Fatalf("history must stop inside 24h: back %d, max %d", model.back, model.maxBack())
	}
}

func TestAIClaudeIntervalGrowsOnRateLimitAndStays(t *testing.T) {
	collector, now := &aiCollector{}, time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)
	collector.scheduleClaude(false, now, time.Minute)
	if every := collector.claudeEvery(time.Minute); every != aiClaudeMinInterval || collector.claudeBackoff != 0 || !collector.claudeNext.Equal(now.Add(aiClaudeMinInterval)) {
		t.Fatalf("first success: every %s, backoff %s, next %s", every, collector.claudeBackoff, collector.claudeNext)
	}
	collector.scheduleClaude(true, now, time.Minute)
	if collector.claudeBackoff != 2*aiClaudeMinInterval || !collector.claudeNext.Equal(now.Add(2*aiClaudeMinInterval)) {
		t.Fatalf("after 429: backoff %s, next %s", collector.claudeBackoff, collector.claudeNext)
	}
	collector.scheduleClaude(false, now, time.Minute)
	if collector.claudeBackoff != 2*aiClaudeMinInterval {
		t.Fatalf("success after 429 must keep %s, got %s", 2*aiClaudeMinInterval, collector.claudeBackoff)
	}
	for range 10 {
		collector.scheduleClaude(true, now, time.Minute)
	}
	if collector.claudeBackoff != aiMaxBackoff {
		t.Fatalf("backoff %s, want the %s cap", collector.claudeBackoff, aiMaxBackoff)
	}
	// --poll 하한은 간격을 늘리지만 백오프가 아니다.
	if fresh := (&aiCollector{}); fresh.claudeEvery(10*time.Minute) != 10*time.Minute || fresh.claudeBackoff != 0 {
		t.Fatalf("poll floor: every %s, backoff %s", fresh.claudeEvery(10*time.Minute), fresh.claudeBackoff)
	}
	if fresh := (&aiCollector{}); fresh.claudeEvery(30*time.Second) != aiClaudeMinInterval {
		t.Fatalf("short poll must keep Claude at %s, got %s", aiClaudeMinInterval, fresh.claudeEvery(30*time.Second))
	}
}

func TestAIClaudeStateKeepsTheValuesWithoutTheError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, aiClaudeStateName)
	fetched := time.Date(2026, 10, 6, 2, 17, 40, 0, time.UTC)
	saved := aiProvider{Name: "claude", Plan: "max", FetchedAt: fetched, Err: "rate limited",
		Windows: []aiWindow{{Name: "5h", Used: 23, ResetsAt: time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC)}}}
	if err := saveAIClaudeState(path, aiClaudeState{aiProvider: saved}); err != nil {
		t.Fatal(err)
	}
	loaded, ok := loadAIClaudeState(path)
	if !ok || loaded.Err != "" || loaded.Plan != "max" || !loaded.FetchedAt.Equal(fetched) || len(loaded.Windows) != 1 || loaded.Windows[0].Used != 23 {
		t.Fatalf("loaded %+v, ok %t", loaded, ok)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the temporary file stayed behind: %d entries", len(entries))
	}
}

// 직전 실행이 방금 조회했다면 다시 열어도 저장한 값을 보이고 최소 간격이 지날 때까지 부르지 않는다.
func TestAICollectorStartsFromTheSavedClaudeValues(t *testing.T) {
	now := time.Date(2026, 10, 6, 2, 20, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		fetched  time.Time
		wantNext time.Time
	}{
		{"recent", now.Add(-2 * time.Minute), now.Add(3 * time.Minute)},
		{"old", now.Add(-time.Hour), now.Add(-time.Hour + aiClaudeMinInterval)},
		{"future", now.Add(time.Hour), time.Time{}},
	} {
		stateDir := t.TempDir()
		saved := aiProvider{Name: "claude", FetchedAt: test.fetched, Windows: []aiWindow{{Name: "7d", Used: 61}}}
		if err := saveAIClaudeState(filepath.Join(stateDir, aiClaudeStateName), aiClaudeState{aiProvider: saved}); err != nil {
			t.Fatal(err)
		}
		collector := newAICollector(t.TempDir(), t.TempDir(), stateDir, now)
		if shown := collector.shown["claude"]; len(shown.Windows) != 1 || shown.Windows[0].Used != 61 {
			t.Errorf("%s: shown %+v", test.name, shown)
		}
		if !collector.claudeNext.Equal(test.wantNext) {
			t.Errorf("%s: next Claude call %s, want %s", test.name, collector.claudeNext, test.wantNext)
		}
	}
}

func TestAICollectorCallsClaudeAtOnceWhenTheStateIsBroken(t *testing.T) {
	stateDir := t.TempDir()
	writeAIFixture(t, filepath.Join(stateDir, aiClaudeStateName), "{not json")
	collector := newAICollector(t.TempDir(), t.TempDir(), stateDir, time.Now())
	if _, ok := collector.shown["claude"]; ok || !collector.claudeNext.IsZero() {
		t.Fatalf("a broken state must be ignored: shown %+v, next %s", collector.shown["claude"], collector.claudeNext)
	}
}

// Σ 10m은 10초 구간 경계에서 시작한다. 진행 중인 구간을 넣은 10초 행 60개의 합과 같아야 한다.
func TestAITotalsMatchTheTenSecondRows(t *testing.T) {
	now := time.Date(2026, 10, 6, 2, 41, 7, 0, time.Local)
	buckets := map[time.Time]aiUsage{}
	for offset := time.Duration(0); offset < 2*time.Hour; offset += 70 * time.Second {
		buckets[now.Add(-offset).Truncate(aiBucket)] = aiUsage{Requests: 1, Input: int64(offset / time.Second), Output: 7, Cache: 100}
	}
	snapshot := aiUsageSnapshot{Buckets: map[string]map[time.Time]aiUsage{"claude": buckets, "codex": {}}}
	totals := snapshot.totals(now)
	for index, span := range []time.Duration{10 * time.Minute, time.Hour} {
		var want aiUsage
		for _, row := range snapshot.rows(now, aiBucket, int(span/aiBucket)) {
			want.add(row.Claude)
		}
		if totals[index].Claude != want {
			t.Errorf("Σ %s = %+v, want %+v", totals[index].Window, totals[index].Claude, want)
		}
	}
	if totals[2].Window != "24h" || totals[2].Claude.Requests != int64(len(buckets)) {
		t.Errorf("Σ 24h = %+v, want every bucket", totals[2])
	}
}

func TestAICompactKeepsAboutThreeDigits(t *testing.T) {
	for value, want := range map[int64]string{
		999: "999", 4100: "4.1K", 412_000: "412K", 1_020_000: "1.02M", 25_100_000: "25.1M", 121_400_000: "121M", 2_100_000_000: "2.10B",
	} {
		if got := aiCompact(value); got != want {
			t.Errorf("aiCompact(%d) = %q, want %q", value, got, want)
		}
	}
}

// aiStubTransport는 Claude 사용량 API 대신 정해 둔 상태 코드와 본문을 돌려준다.
type aiStubTransport struct {
	status int
	body   string
}

func (stub aiStubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: stub.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stub.body)), Request: request}, nil
}

// aiUsageFixture는 성공한 Claude 사용량 응답이다.
const aiUsageFixture = `{"five_hour":{"utilization":22,"resets_at":"2026-10-06T08:59:59Z"}}`

func writeAIClaudeCredentials(t *testing.T, dir string) {
	t.Helper()
	writeAIFixture(t, filepath.Join(dir, ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"test-token","expiresAt":`+strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)+`,"subscriptionType":"max"}}`)
}

// 429로 늘린 간격은 다시 실행해도 이어진다. 한 번도 성공하지 못했으면 상자는 비지 않고 미룬 이유와 다음 조회 시각을 보인다.
func TestAIPollKeepsTheClaudeBackoffAcrossRuns(t *testing.T) {
	// codex를 찾지 못하게 해 시험이 실제 app-server를 띄우지 않는다.
	t.Setenv("PATH", t.TempDir())
	claudeDir, stateDir := t.TempDir(), t.TempDir()
	writeAIClaudeCredentials(t, claudeDir)
	rateLimited := &http.Client{Transport: aiStubTransport{status: http.StatusTooManyRequests, body: "{}"}}
	collector := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	collector.http = rateLimited
	collector.poll(context.Background(), time.Minute)
	if collector.claudeBackoff != 2*aiClaudeMinInterval {
		t.Fatalf("after 429: backoff %s", collector.claudeBackoff)
	}
	restarted := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	// 복원이 틀려 바로 조회하게 되어도 실제 API를 부르지 않는다.
	restarted.http = rateLimited
	if !restarted.claudeNext.Equal(collector.claudeNext) || restarted.claudeBackoff != collector.claudeBackoff {
		t.Fatalf("restart: next %s backoff %s, want %s and %s", restarted.claudeNext, restarted.claudeBackoff, collector.claudeNext, collector.claudeBackoff)
	}
	claude := restarted.poll(context.Background(), time.Minute).providers[0]
	if claude.Name != "claude" || len(claude.Windows) != 0 || !strings.Contains(claude.Err, "rate limited · next try ") {
		t.Errorf("restart without a success: %+v", claude)
	}
}

// 429 뒤에 늘어난 백오프는 성공해도 줄지 않는다. 성공한 뒤 다시 실행해도 그 간격을 쓴다.
func TestAIPollKeepsTheBackedOffIntervalAfterASuccess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	claudeDir, stateDir := t.TempDir(), t.TempDir()
	writeAIClaudeCredentials(t, claudeDir)
	collector := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	collector.claudeBackoff = 2 * aiClaudeMinInterval
	collector.http = &http.Client{Transport: aiStubTransport{status: http.StatusOK, body: aiUsageFixture}}
	if claude := collector.poll(context.Background(), time.Minute).providers[0]; claude.Err != "" || len(claude.Windows) != 1 {
		t.Fatalf("success: %+v", claude)
	}
	restarted := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	if !restarted.claudeNext.Equal(collector.claudeNext) || restarted.claudeBackoff != 2*aiClaudeMinInterval {
		t.Errorf("restart: next %s backoff %s, want %s and %s", restarted.claudeNext, restarted.claudeBackoff, collector.claudeNext, 2*aiClaudeMinInterval)
	}
	if shown := restarted.shown["claude"]; shown.Err != "" || len(shown.Windows) != 1 {
		t.Errorf("restart after a success shows %+v", shown)
	}
}

// --poll 10m으로 성공한 뒤 기본 --poll로 다시 실행하면 5분 간격으로 돌아간다. 하한은 백오프가 아니라서 남기지 않는다.
func TestAIPollDoesNotCarryThePollFloorAcrossRuns(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	claudeDir := t.TempDir()
	writeAIClaudeCredentials(t, claudeDir)
	for _, test := range []struct {
		name    string
		backoff time.Duration
		poll    time.Duration
		// wantAfter는 성공한 시각에서 다시 실행한 쪽의 다음 조회까지 걸리는 시간이다.
		wantAfter time.Duration
	}{
		{"no backoff", 0, 10 * time.Minute, aiClaudeMinInterval},
		{"poll above the backoff", 10 * time.Minute, 12 * time.Minute, 10 * time.Minute},
	} {
		stateDir := t.TempDir()
		collector := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
		collector.claudeBackoff = test.backoff
		collector.http = &http.Client{Transport: aiStubTransport{status: http.StatusOK, body: aiUsageFixture}}
		if claude := collector.poll(context.Background(), test.poll).providers[0]; claude.Err != "" {
			t.Fatalf("%s: success: %+v", test.name, claude)
		}
		fetched := collector.last["claude"].FetchedAt
		restarted := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
		if restarted.claudeBackoff != test.backoff || !restarted.claudeNext.Equal(fetched.Add(test.wantAfter)) {
			t.Errorf("%s: restart backoff %s next %s, want %s and %s", test.name, restarted.claudeBackoff, restarted.claudeNext, test.backoff, fetched.Add(test.wantAfter))
		}
	}
}

// --poll이 백오프 상한보다 길어도 429 뒤에 다시 실행하면 백오프를 이어 받는다. 저장한 시각에 하한이 섞이면 복원 범위를 벗어난다.
func TestAIPollKeepsTheBackoffWithAPollAboveTheCap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	claudeDir, stateDir := t.TempDir(), t.TempDir()
	writeAIClaudeCredentials(t, claudeDir)
	rateLimited := &http.Client{Transport: aiStubTransport{status: http.StatusTooManyRequests, body: "{}"}}
	collector := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	collector.http = rateLimited
	collector.poll(context.Background(), 20*time.Minute)
	if collector.claudeBackoff != aiMaxBackoff {
		t.Fatalf("after 429 under --poll 20m: backoff %s", collector.claudeBackoff)
	}
	restarted := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	restarted.http = rateLimited
	// 백오프 상한이 --poll보다 길면 다음 실행도 백오프가 끝날 때까지 기다린다.
	if want := collector.claudeNext; restarted.claudeBackoff != aiMaxBackoff || !restarted.claudeNext.Equal(want) {
		t.Fatalf("restart: backoff %s next %s, want %s and %s", restarted.claudeBackoff, restarted.claudeNext, aiMaxBackoff, want)
	}
	if claude := restarted.poll(context.Background(), time.Minute).providers[0]; !strings.Contains(claude.Err, "rate limited · next try ") {
		t.Errorf("restart without a success: %+v", claude)
	}
}

func TestAICollectorResumesTheSavedClaudeBackoff(t *testing.T) {
	now := time.Date(2026, 10, 6, 2, 20, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		fetched     time.Time
		nextTry     time.Time
		backoff     time.Duration
		wantNext    time.Time
		wantBackoff time.Duration
		// wantAfter429는 이어 받은 백오프에서 429를 다시 받은 뒤의 백오프다. 0이면 확인하지 않는다.
		wantAfter429 time.Duration
	}{
		{"still ahead", now.Add(-2 * time.Minute), now.Add(8 * time.Minute), 10 * time.Minute, now.Add(8 * time.Minute), 10 * time.Minute, 0},
		{"before the minimum interval", now.Add(-2 * time.Minute), now.Add(time.Minute), 10 * time.Minute, now.Add(3 * time.Minute), 10 * time.Minute, 0},
		// --count 1을 되풀이하면 다음 실행은 미룬 시각이 지난 뒤에 온다. 한 간격 안이면 백오프를 이어 받아 다시 늘린다.
		{"passed within one backoff", now.Add(-20 * time.Minute), now.Add(-time.Minute), 10 * time.Minute, now.Add(-time.Minute), 10 * time.Minute, 20 * time.Minute},
		// 한 간격 넘게 쉬었으면 백오프는 끝났다. 늘린 간격을 영구 하한으로 남기지 않는다.
		{"passed long ago", now.Add(-time.Hour), now.Add(-50 * time.Minute), 10 * time.Minute, now.Add(-time.Hour + aiClaudeMinInterval), 0, 0},
		{"beyond the backoff cap", now.Add(-2 * time.Minute), now.Add(time.Hour), 10 * time.Minute, now.Add(3 * time.Minute), 0, 0},
		// 백오프 없이 저장한 다음 조회 시각은 --poll 하한일 수 있어 따르지 않는다.
		{"no backoff", now.Add(-2 * time.Minute), now.Add(8 * time.Minute), 0, now.Add(3 * time.Minute), 0, 0},
	} {
		stateDir := t.TempDir()
		saved := aiClaudeState{aiProvider: aiProvider{FetchedAt: test.fetched, Windows: []aiWindow{{Name: "7d", Used: 61}}}, NextTry: test.nextTry, Backoff: test.backoff}
		if err := saveAIClaudeState(filepath.Join(stateDir, aiClaudeStateName), saved); err != nil {
			t.Fatal(err)
		}
		collector := newAICollector(t.TempDir(), t.TempDir(), stateDir, now)
		if !collector.claudeNext.Equal(test.wantNext) || collector.claudeBackoff != test.wantBackoff {
			t.Errorf("%s: next %s backoff %s, want %s and %s", test.name, collector.claudeNext, collector.claudeBackoff, test.wantNext, test.wantBackoff)
		}
		if shown := collector.shown["claude"]; len(shown.Windows) != 1 {
			t.Errorf("%s: the saved values are not shown: %+v", test.name, shown)
		}
		// 이어 받은 백오프에서 429를 다시 받으면 상한까지 계속 늘어난다.
		if test.wantAfter429 > 0 {
			collector.scheduleClaude(true, now, time.Minute)
			if collector.claudeBackoff != test.wantAfter429 {
				t.Errorf("%s: 429 after a resumed backoff: %s, want %s", test.name, collector.claudeBackoff, test.wantAfter429)
			}
		}
	}
}

func TestAIJSONOmitsUnknownTimes(t *testing.T) {
	data, err := json.Marshal(aiProvider{Name: "codex", Windows: []aiWindow{{Name: "5h", Used: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); strings.Contains(text, "resets_at") || strings.Contains(text, "fetched_at") || strings.Contains(text, "0001-01-01") {
		t.Errorf("unknown times leaked into JSON: %s", text)
	}
	at := time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC)
	data, err = json.Marshal(aiProvider{Name: "codex", FetchedAt: at, Windows: []aiWindow{{Name: "5h", ResetsAt: at}}})
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); !strings.Contains(text, `"resets_at":"2026-10-06T03:00:00Z"`) || !strings.Contains(text, `"fetched_at":"2026-10-06T03:00:00Z"`) {
		t.Errorf("known times are missing: %s", text)
	}
}

func TestReadAIClaudeTokenAsksForALoginWithoutCredentials(t *testing.T) {
	_, _, err := readAIClaudeToken(t.TempDir(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "log in with Claude Code") {
		t.Fatalf("missing credentials: %v", err)
	}
}

func TestAIDashboardAsksForTheHeightItNeeds(t *testing.T) {
	model := aiDashboardFixture(time.Date(2026, 10, 6, 1, 0, 30, 0, time.Local))
	minHeight := model.minHeight()
	if minHeight <= 12 {
		t.Fatalf("the fixture needs %d rows; the old 12-row check would pass", minHeight)
	}
	model.height = minHeight - 1
	if text := model.View().Content; !strings.Contains(text, "terminal too small") || !strings.Contains(text, fmt.Sprintf("40×%d", minHeight)) {
		t.Errorf("%d rows: %q", model.height, text)
	}
	model.height = minHeight
	if lines := strings.Split(model.View().Content, "\n"); len(lines) > model.height || strings.Contains(lines[0], "terminal too small") {
		t.Errorf("%d rows: %d lines, first %q", model.height, len(lines), lines[0])
	}
}

// aiStatuslineFixture는 문서에 나온 statusline 입력의 한도 부분이다. spend_limit은 gateway 전용이라 읽지 않는다.
const aiStatuslineFixture = `{"model":{"display_name":"Opus"},"rate_limits":{` +
	`"five_hour":{"used_percentage":23.5,"resets_at":1791362400},` +
	`"seven_day":{"used_percentage":41.2,"resets_at":1791880800},` +
	`"spend_limit":{"used_percentage":62.8,"resets_at":1793000000}}}`

func TestParseAIStatuslineInputKeepsTheLimitWindows(t *testing.T) {
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	provider, ok := parseAIStatuslineInput([]byte(aiStatuslineFixture), now)
	if !ok || provider.Name != "claude" || !provider.FetchedAt.Equal(now) || len(provider.Windows) != 2 {
		t.Fatalf("parsed %+v, ok %t", provider, ok)
	}
	if window := provider.Windows[0]; window.Name != "5h" || window.Used != 23.5 || !window.ResetsAt.Equal(time.Unix(1791362400, 0)) {
		t.Errorf("5h window %+v", window)
	}
	if window := provider.Windows[1]; window.Name != "7d" || window.Used != 41.2 {
		t.Errorf("7d window %+v", window)
	}
	if text := formatAIStatusline(provider); text != "5h 24% · 7d 41%" {
		t.Errorf("status line %q", text)
	}
	for _, input := range []string{`{"model":{}}`, `{"rate_limits":{}}`, `not json`, ``} {
		if _, ok := parseAIStatuslineInput([]byte(input), now); ok {
			t.Errorf("%q must not count as usage", input)
		}
	}
}

func TestReadAIClaudeSnapshotDropsPassedWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, aiClaudeSnapshotName)
	if provider := readAIClaudeSnapshot(path, time.Now()); provider.Err != errAIStatuslineMissing.Error() {
		t.Fatalf("missing snapshot: %+v", provider)
	}
	writeAIFixture(t, path, "{not json")
	if provider := readAIClaudeSnapshot(path, time.Now()); provider.Err == "" || provider.Err == errAIStatuslineMissing.Error() {
		t.Fatalf("broken snapshot: %+v", provider)
	}
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	saved := aiProvider{Name: "claude", FetchedAt: now.Add(-time.Hour), Windows: []aiWindow{
		{Name: "5h", Used: 90, ResetsAt: now.Add(-time.Minute)},
		{Name: "7d", Used: 41, ResetsAt: now.Add(time.Hour)},
	}}
	if err := saveAIClaudeState(path, aiClaudeState{aiProvider: saved}); err != nil {
		t.Fatal(err)
	}
	provider := readAIClaudeSnapshot(path, now)
	if provider.Err != "" || !provider.FetchedAt.Equal(saved.FetchedAt) || len(provider.Windows) != 1 || provider.Windows[0].Name != "7d" {
		t.Errorf("snapshot %+v", provider)
	}
}

// 스냅샷을 읽는 수집기는 사용량 API를 부르지 않고, 조회 간격도 늘리지 않으며, API 상태 파일도 쓰지 않는다.
func TestAIPollReadsTheStatuslineSnapshotInsteadOfTheAPI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	claudeDir, stateDir := t.TempDir(), t.TempDir()
	writeAIClaudeCredentials(t, claudeDir)
	snapshot := filepath.Join(stateDir, aiClaudeSnapshotName)
	provider, _ := parseAIStatuslineInput([]byte(aiStatuslineFixture), time.Now().Add(-time.Minute))
	if err := saveAIClaudeState(snapshot, aiClaudeState{aiProvider: provider}); err != nil {
		t.Fatal(err)
	}
	collector := newAICollector(claudeDir, t.TempDir(), stateDir, time.Now())
	collector.claudeSnapshot = snapshot
	collector.http = &http.Client{Transport: aiStubTransport{status: http.StatusTeapot, body: "{}"}}
	result := collector.poll(context.Background(), time.Minute)
	if claude := result.providers[0]; claude.Err != "" || len(claude.Windows) != 2 || !claude.FetchedAt.Equal(provider.FetchedAt) {
		t.Fatalf("claude %+v", claude)
	}
	if result.claudeInterval != time.Minute {
		t.Errorf("claude interval %s, want the poll", result.claudeInterval)
	}
	if _, err := os.Stat(filepath.Join(stateDir, aiClaudeStateName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the API state file must stay untouched: %v", err)
	}
}

func TestRunAIStatuslinePassesTheInputToTheCommand(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	input := filepath.Join(t.TempDir(), "input.json")
	writeAIFixture(t, input, aiStatuslineFixture)
	stdin, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved }()

	copied := filepath.Join(t.TempDir(), "copied.json")
	if code := runAIStatusline([]string{"--", "sh", "-c", `cat > "$0"; exit 3`, copied}); code != 3 {
		t.Errorf("exit code %d, want the command's 3", code)
	}
	if data, err := os.ReadFile(copied); err != nil || string(data) != aiStatuslineFixture {
		t.Errorf("the command got %q, %v", data, err)
	}
	provider := readAIClaudeSnapshot(filepath.Join(stateHome, "edc", aiClaudeSnapshotName), time.Now())
	if provider.Err != "" || len(provider.Windows) != 2 {
		t.Errorf("snapshot %+v", provider)
	}
}

func TestEditAIStatuslineWrapsAndRestoresTheCommand(t *testing.T) {
	const executable = "/opt/edc/bin/edc"
	for _, test := range []struct {
		name, settings, wrapped string
	}{
		{"plain", `{"model":"opus","statusLine":{"type":"command","command":"cship","padding":0},"env":{"A":"1"}}`,
			"/opt/edc/bin/edc ai statusline -- cship"},
		{"shell", `{"statusLine":{"type":"command","command":"jq -r '\"[\\(.model.display_name)]\"' | head -1 && echo ok"}}`,
			`/opt/edc/bin/edc ai statusline -- sh -c 'jq -r '\''"[\(.model.display_name)]"'\'' | head -1 && echo ok'`},
		{"env", `{"statusLine":{"type":"command","command":"NO_COLOR=1 cship"}}`,
			`/opt/edc/bin/edc ai statusline -- sh -c 'NO_COLOR=1 cship'`},
		{"words", `{"statusLine":{"type":"command","command":"bunx ccstatusline"}}`,
			`/opt/edc/bin/edc ai statusline -- sh -c 'bunx ccstatusline'`},
		{"home", `{"statusLine":{"type":"command","command":"~/bin/status"}}`,
			"/opt/edc/bin/edc ai statusline -- ~/bin/status"},
		{"absent", `{"model":"opus"}`, "/opt/edc/bin/edc ai statusline"},
		{"empty file", ``, "/opt/edc/bin/edc ai statusline"},
	} {
		installed, err := editAIStatusline([]byte(test.settings), executable, true)
		if err != nil || !installed.changed || installed.after != test.wrapped {
			t.Fatalf("%s: install %+v, %v", test.name, installed, err)
		}
		again, err := editAIStatusline(installed.settings, executable, true)
		if err != nil || again.changed {
			t.Errorf("%s: a second install must not wrap again: %+v, %v", test.name, again, err)
		}
		removed, err := editAIStatusline(installed.settings, executable, false)
		if err != nil || !removed.changed || removed.after != installed.before {
			t.Fatalf("%s: uninstall %+v, %v", test.name, removed, err)
		}
		if !aiJSONEqual(t, removed.settings, test.settings) {
			t.Errorf("%s: uninstall left\n%s\nwant %s", test.name, removed.settings, test.settings)
		}
	}
	installed, _ := editAIStatusline([]byte(`{"model":"opus","statusLine":{"type":"command","command":"cship","padding":0},"env":{"A":"1"}}`), executable, true)
	if text := string(installed.settings); strings.Index(text, `"model"`) > strings.Index(text, `"statusLine"`) || strings.Index(text, `"statusLine"`) > strings.Index(text, `"env"`) || !strings.Contains(text, `"padding": 0`) {
		t.Errorf("the other keys moved or vanished:\n%s", text)
	}
	if again, err := editAIStatusline([]byte(`{"statusLine":{"command":"cship"}}`), executable, false); err != nil || again.changed {
		t.Errorf("uninstall without edc: %+v, %v", again, err)
	}
	if _, err := editAIStatusline([]byte(`[1]`), executable, true); err == nil {
		t.Error("a settings file that is not an object must fail")
	}
}

func TestWriteAIClaudeSettingsKeepsASymbolicLink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "dotfiles", "settings.json")
	link := filepath.Join(directory, "claude", "settings.json")
	for _, dir := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	backup, err := writeAIClaudeSettings(link, []byte(`{"a":1}`), []byte(`{"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link became %v, %v", info, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != `{"a":2}` {
		t.Errorf("target %q, %v", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("target mode %v, %v", info, err)
	}
	if data, err := os.ReadFile(backup); backup != link+aiStatuslineBackupSuffix || err != nil || string(data) != `{"a":1}` {
		t.Errorf("backup %s %q, %v", backup, data, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAIClaudeSettings(link, nil, []byte(`{"a":3}`)); err == nil {
		t.Error("a link to a missing file must fail")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the dangling link changed: %v, %v", info, err)
	}
}

func aiJSONEqual(t *testing.T, got []byte, want string) bool {
	t.Helper()
	if strings.TrimSpace(want) == "" {
		want = "{}"
	}
	var left, right any
	if json.Unmarshal(got, &left) != nil || json.Unmarshal([]byte(want), &right) != nil {
		return false
	}
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}
