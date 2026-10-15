package edc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresAndRedisAreRegisteredLikeTheOtherDatabases(t *testing.T) {
	for _, protocol := range []string{"postgres", "redis"} {
		spec := traceProtocols[protocol]
		if !spec.serverSide || !spec.linuxOnly || !spec.hideTraffic || spec.newSummarizer == nil || spec.scrollLabels == nil {
			t.Fatalf("%s spec = %+v", protocol, spec)
		}
		var titles []string
		for _, column := range spec.groupColumns {
			titles = append(titles, column.screenTitle)
		}
		if strings.Join(titles, " ") != "CMD RSP ERR NOANS AVGms MAXms" {
			t.Fatalf("%s columns = %q", protocol, titles)
		}
		for _, test := range []struct{ side, want string }{{"", protocol}, {traceClientSide, protocol + " --side client"}, {traceServerSide, protocol + " --side server"}} {
			if got := traceLabel(protocol, test.side); got != test.want {
				t.Fatalf("traceLabel(%s, %q) = %q, want %q", protocol, test.side, got, test.want)
			}
		}
		if !traceScreenPairs(protocol) {
			t.Fatalf("%s rows are not paired", protocol)
		}
		for _, args := range [][]string{{protocol, "--port", "0"}, {protocol, "--payload"}, {protocol, "--http-port", "8123"}, {protocol, "--tls"}, {protocol, "--side", "both"}, {protocol, "--slow", "0s"}} {
			if code := runTrace(args); code != 2 {
				t.Fatalf("trace %q exit = %d, want 2", args, code)
			}
		}
	}
}

func pgFixtureEvents(t *testing.T, name string, run int, server bool) []captureEvent {
	t.Helper()
	return replayPostgres(loadPostgresFixture(t, name), server, false, 0)[run]
}

func TestPostgresScrollLabelsAndScreenPairs(t *testing.T) {
	events := pgFixtureEvents(t, "error", 0, true)
	var labels []string
	for _, event := range events {
		destination, label := traceScrollLabels(event)
		labels = append(labels, destination+" | "+strings.Fields(label)[0])
	}
	want := []string{
		"server: edc edc | postgres_connect",
		"server: ↳ ready 16.15 (Ubuntu 16.15-1.pgdg24.04+2) | postgres_result",
	}
	if labels[0] != want[0] || labels[1] != want[1] || !strings.HasPrefix(labels[3], "server: ↳ ERROR 42601 syntax error") {
		t.Fatalf("labels = %q", labels)
	}
	model := newTraceScreenModel("postgres", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 160, 20
	model.events = events
	rows := traceVisibleScreenRows(model)
	if len(rows) != len(events)/2 {
		t.Fatalf("rows = %+v for %d events", rows, len(events))
	}
	for _, row := range rows {
		if row.response < 0 {
			t.Fatalf("unpaired row %+v", row)
		}
	}
	_, destination, _, ok := model.displayRowLabels(rows[len(rows)-1])
	if !ok || !strings.Contains(destination, "→ ERROR 22012") {
		t.Fatalf("last row = %q", destination)
	}
	detail := model.buildDetail(rows[len(rows)-1].primary)
	if text := strings.Join(detail.raw, "\n"); !strings.Contains(text, "response:\nevent: postgres_error\nresult: ERROR 22012") || !strings.Contains(text, "rows: 499") || !strings.Contains(text, "sql:\nSELECT") {
		t.Fatalf("detail = %s", text)
	}
	tls := captureEvent{Protocol: "postgres", Event: postgresEventTLS, Side: traceClientSide, Postgres: &tracePostgresEvent{Encryption: "ssl"}}
	if destination, _ := traceScrollLabels(tls); destination != "client: tls ssl" {
		t.Fatalf("tls label = %q", destination)
	}
	if _, _, pending, _ := tracePairOf(tls); pending {
		t.Fatal("postgres_tls must not wait for a response")
	}
}

func TestPostgresShapeIgnoresValues(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT * FROM t WHERE id = 7 AND v = 'x'":            "SELECT * FROM t WHERE id = ? AND v = ?",
		"SELECT  *\n FROM t WHERE id = 8 AND v = '?'":         "SELECT * FROM t WHERE id = ? AND v = ?",
		"SELECT $1::int, t2.c FROM t2 WHERE x > 1.5e3":        "SELECT $1::int, t2.c FROM t2 WHERE x > ?",
		"SELECT $$secret$$, E'a\\'b', \"Col 1\" -- 5\n":       "SELECT $$?$$, E?, \"Col 1\" -- 5",
		"SELECT repeat('?',100) FROM generate_series(1,1000)": "SELECT repeat(?,?) FROM generate_series(?,?)",
	} {
		if got := tracePostgresShape(sql); got != want {
			t.Errorf("shape(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestPostgresTraceSummaryCountsFixtureRuns(t *testing.T) {
	var events []captureEvent
	for _, server := range []bool{true, false} {
		for _, name := range []string{"simple", "error", "extended", "ssl"} {
			for _, run := range replayPostgres(loadPostgresFixture(t, name), server, false, 0) {
				events = append(events, run...)
			}
		}
	}
	summarizer := newPostgresTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 1}, time.Second).(postgresTraceReport)
	if report.Client == nil || report.Server == nil || report.Client.Commands != report.Server.Commands || report.TLSConnections != 2 || report.Errors == 0 {
		t.Fatalf("report = %+v client %+v server %+v", report.tracePostgresCounts, report.Client, report.Server)
	}
	// 처음부터 본 fixture의 요청은 모두 답을 받는다. CancelRequest 연결은 요청 event를 내지 않는다.
	if report.Unanswered != 0 || report.Responses != report.Commands {
		t.Fatalf("unanswered = %d, responses %d of %d", report.Unanswered, report.Responses, report.Commands)
	}
	shapes := map[string]bool{}
	for _, row := range report.Statements {
		shapes[row.Side+" "+row.Command+" "+row.Shape] = true
	}
	for _, want := range []string{"server connect (connect)", "client query SELECT ?", "server execute SELECT length($1::text) AS n;", "client function (function call)"} {
		if !shapes[want] {
			t.Fatalf("rows %v have no %q", shapes, want)
		}
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"client":{"commands":`, `"statements":[`, `"lost_events":1`, `"tls_connections":2`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	output := traceCaptureOutput(t, &os.Stdout, func() { report.print(false) })
	for _, text := range []string{"PostgreSQL trace: 1s\n", "Client side (requests that this host sent):\n  Commands: ", "\nSIDE\tCOMMAND\tSQL\tCOUNT\tERRORS\tUNANSWERED\tAVG\tMAX\tPROCESS\n", "Lost events: 1\n"} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	for _, group := range summarizeTraceGroups("postgres", traceGroupByEvent, events, captureSummary{}, time.Second, "", "").Groups {
		if (group.Group == postgresEventQuery || group.Group == postgresEventConnect) && group.Postgres.Unanswered != 0 {
			t.Fatalf("event row %s (server %t) = %+v", group.Group, group.Server, *group.Postgres)
		}
	}
}

func TestRedisScrollLabelsSummaryAndScreenPairs(t *testing.T) {
	var events []captureEvent
	for _, name := range []string{"pipeline", "multi", "pubsub", "errors"} {
		runs, _ := replayRedis(loadRedisFixture(t, name), true, 1024, false)
		for run := 0; run < len(runs); run++ {
			events = append(events, runs[run]...)
		}
	}
	var labels []string
	for _, event := range events[:4] {
		destination, label := traceScrollLabels(event)
		labels = append(labels, destination+" | "+strings.Fields(label)[0])
	}
	if labels[0] != "server: AUTH | redis_command" || labels[1] != "server: ↳ simple | redis_reply" {
		t.Fatalf("labels = %q", labels)
	}
	pushes := 0
	for _, event := range events {
		destination, _ := traceScrollLabels(event)
		if strings.Contains(destination, "edc-fixture-pass") {
			t.Fatalf("label %q shows the password", destination)
		}
		if event.Event == redisEventPush {
			pushes++
			if !strings.HasPrefix(destination, "server: push ") {
				t.Fatalf("push label = %q", destination)
			}
		}
	}
	summarizer := newRedisTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(redisTraceReport)
	if report.Side != traceServerSide || report.Commands == 0 || report.Replies == 0 || report.Errors == 0 || report.Pushes != uint64(pushes) || pushes == 0 {
		t.Fatalf("report = %+v", report.traceRedisCounts)
	}
	byName := map[string]redisTraceRow{}
	for _, row := range report.Rows {
		byName[row.Command] = row
	}
	if set := byName["SET"]; set.Commands < 16 || set.Unanswered != 0 || set.LatencyAvgMS == nil {
		t.Fatalf("SET row = %+v", set)
	}
	output := traceCaptureOutput(t, &os.Stdout, func() { report.print(false) })
	for _, text := range []string{"Redis server trace: 1s\n", "\nCOMMAND\tCOUNT\tERRORS\tUNANSWERED\tAVG\tMAX\tPROCESS\n", "Pushes: "} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	model := newTraceScreenModel("redis", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 160, 20
	model.events = events
	paired := 0
	for _, row := range traceVisibleScreenRows(model) {
		if row.response >= 0 {
			paired++
			if events[row.primary].Redis.Command != events[row.response].Redis.Command {
				t.Fatalf("row pairs %s with %s", events[row.primary].Redis.Command, events[row.response].Redis.Command)
			}
		}
	}
	if uint64(paired) != report.Replies {
		t.Fatalf("paired rows = %d, replies %d", paired, report.Replies)
	}
	for _, group := range summarizeTraceGroups("redis", traceGroupByEvent, events, captureSummary{}, time.Second, "", "").Groups {
		if group.Group == redisEventCommand && group.Redis.Unanswered != report.Unanswered {
			t.Fatalf("command row = %+v, report unanswered %d", *group.Redis, report.Unanswered)
		}
	}
}

func TestRedisDetailShowsTheFieldsAndHidesSecrets(t *testing.T) {
	for _, show := range []bool{false, true} {
		found := map[string]bool{}
		for _, name := range []string{"secrets", "pipeline", "errors", "pubsub", "large_get"} {
			runs, _ := replayRedis(loadRedisFixture(t, name), true, 1024, show)
			for run := 0; run < len(runs); run++ {
				for _, event := range runs[run] {
					lines := newTraceDetail(event, "", nil, 1, 160).raw
					if lines[0] != "event: "+event.Event {
						t.Fatalf("%s detail starts with %q", event.Event, lines[0])
					}
					text := strings.Join(lines, "\n")
					if strings.Contains(text, "edc-fixture-pass") {
						t.Fatalf("show=%v: detail shows the password:\n%s", show, text)
					}
					found["edc-secret-value"] = found["edc-secret-value"] || strings.Contains(text, "edc-secret-value")
					for _, line := range []string{"command: AUTH", "error: ", "push_kind: ", "latency: ", "elements: ", "bulk_length: ", "value: ", "arguments:"} {
						found[line] = found[line] || strings.Contains(text, "\n"+line)
					}
				}
			}
		}
		// 값과 인자 preview는 --show-secrets일 때만 생긴다. 그때만 secrets fixture의 값이 상세에 보여야 한다.
		for line, want := range map[string]bool{"command: AUTH": true, "error: ": true, "push_kind: ": true, "latency: ": true, "elements: ": true, "bulk_length: ": true, "value: ": show, "arguments:": show, "edc-secret-value": show} {
			if found[line] != want {
				t.Errorf("show=%v: Redis detail has %q = %v, want %v", show, line, found[line], want)
			}
		}
	}
	// Redis 정보가 없는 연결 event는 다른 protocol처럼 JSON으로 보인다.
	if lines := newTraceDetail(captureEvent{Protocol: "redis", Event: "close"}, "", nil, 1, 160).raw; lines[0] != "{" {
		t.Fatalf("event without Redis fields: %q", lines)
	}
}
