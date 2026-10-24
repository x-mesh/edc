package edc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func traceShowWrite(t *testing.T, name string, records ...interface{}) string {
	t.Helper()
	var lines []string
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTraceShowReadsWorstCaseEscapedHTTPPayload(t *testing.T) {
	event := captureEvent{Event: traceHTTPRequestEvent, Protocol: "http", Payload: strings.Repeat("\x01<&", httpMessageMax/3)}
	var raw bytes.Buffer
	if err := json.NewEncoder(&raw).Encode(event); err != nil {
		t.Fatal(err)
	}
	if raw.Len() <= 4<<20 || raw.Len() >= traceShowLineLimit {
		t.Fatalf("encoded line size = %d, limit %d", raw.Len(), traceShowLineLimit)
	}
	file, err := readTraceShowFile(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.events) != 1 || file.events[0].Payload != event.Payload {
		t.Fatalf("round trip payload length = %d", len(file.events[0].Payload))
	}
}

func TestTraceRawSocketsNumberConnectionsFromOne(t *testing.T) {
	sockets := traceRawSockets{}
	if sockets.number(0) != 0 || sockets.number(0xffff8000) != 1 || sockets.number(0xffff9000) != 2 || sockets.number(0xffff8000) != 1 {
		t.Fatalf("numbers = %v", sockets)
	}
}

func TestTraceShowReplaysAnHTTPRawFile(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	tracker := newHTTPTracker("", false, false)
	var events []captureEvent
	for index, packet := range []httpPacket{
		httpTestPacket("GET /a HTTP/1.1\r\nHost: api.example\r\n\r\n", 1_000_000, true),
		httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 3_000_000, false),
		httpTestPacket("GET /b HTTP/1.1\r\nHost: api.example\r\n\r\n", 4_000_000, true),
		httpTestPacket("HTTP/1.1 500 Oops\r\n\r\n", 9_000_000, false),
	} {
		event, ok := tracker.event(packet, 0)
		if !ok {
			t.Fatalf("packet %d made no event", index)
		}
		event.Socket = 1
		events = append(events, event)
	}
	path := traceShowWrite(t, "http.jsonl", events[0], events[1], events[2], events[3], captureSummary{Event: "capture_summary", EventCount: 4, LostEvents: 2})
	output := traceCaptureOutput(t, &os.Stdout, func() {
		if code := runTraceShow([]string{path}); code != 0 {
			t.Errorf("exit = %d", code)
		}
	})
	for _, text := range []string{"Requests: 2", "Responses: 2", "Server errors (5xx): 1", "Lost events: 2"} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	jsonPath := filepath.Join(t.TempDir(), "report.json")
	if code := runTraceShow([]string{"--group-by", "path", "--json", jsonPath, path}); code != 0 {
		t.Fatalf("group-by exit = %d", code)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var report traceGroupReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Protocol != "http" || len(report.Groups) != 2 || report.LostEvents != 2 {
		t.Fatalf("report = %+v", report)
	}
	// duration은 첫 event부터 마지막 event까지다.
	if report.DurationMS != (9_000_000-1_000_000)/int64(time.Millisecond) {
		t.Fatalf("duration = %d", report.DurationMS)
	}
	// --process는 살아 있는 trace와 같은 filter다.
	filtered := traceCaptureOutput(t, &os.Stdout, func() { runTraceShow([]string{"--process", "nobody", path}) })
	if !strings.Contains(filtered, "Requests: 0") {
		t.Fatalf("filtered = %q", filtered)
	}
}

func TestTraceShowReplaysAnExecRawFile(t *testing.T) {
	code1, code0 := int32(1), int32(0)
	short := 50.0
	sender := uint32(1)
	path := traceShowWrite(t, "exec.jsonl",
		execEvent{TimestampNS: 1_000_000_000, Event: execEventExec, PID: 10, Process: "curl", Parent: "cron", Args: "curl -s x"},
		execEvent{TimestampNS: 1_050_000_000, Event: execEventExit, PID: 10, Process: "curl", Parent: "cron", ExitCode: &code1, LifetimeMS: &short},
		execEvent{TimestampNS: 2_000_000_000, Event: execEventSignal, PID: 20, Process: "java", Signal: "SIGTERM", Sender: "systemd", SenderPID: &sender, Result: "delivered"},
		execEvent{TimestampNS: 2_100_000_000, Event: execEventExit, PID: 20, Process: "java", ExitCode: &code0, Signal: "SIGTERM", LifetimeMS: &short},
		execSummary{Event: "capture_summary", EventCount: 4, FilteredSignals: 7})
	output := traceCaptureOutput(t, &os.Stdout, func() {
		if code := runTraceShow([]string{"-d", path}); code != 0 {
			t.Errorf("exit = %d", code)
		}
	})
	for _, text := range []string{"EXEC TRACE  duration 1100ms", "curl", "SIGTERM    systemd", "filtered_signals 7", "events 4"} {
		if !strings.Contains(output, text) {
			t.Fatalf("exec summary %q does not contain %q", output, text)
		}
	}
	jsonPath := filepath.Join(t.TempDir(), "exec.json")
	if code := runTraceShow([]string{path, "--group-by", "parent", "--json", jsonPath}); code != 0 {
		t.Fatalf("group-by exit = %d", code)
	}
	data, _ := os.ReadFile(jsonPath)
	if !strings.Contains(string(data), `"group": "cron"`) || !strings.Contains(string(data), `"short_lived": 1`) {
		t.Fatalf("exec JSON = %s", data)
	}
	if code := runTraceShow([]string{"--group-by", "port", path}); code != 2 {
		t.Fatalf("exec group-by port exit = %d, want 2", code)
	}
}

func TestTraceShowRejectsBadFiles(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for name, content := range map[string]string{
		"empty":        "",
		"not json":     "hello\n",
		"unknown":      `{"event":"x"}` + "\n",
		"mixed":        `{"event":"exec","pid":1,"process":"a"}` + "\n" + `{"event":"connect","protocol":"tcp"}` + "\n",
		"only summary": `{"event":"capture_summary"}` + "\n",
	} {
		path := filepath.Join(t.TempDir(), "trace.jsonl")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := runTraceShow([]string{path}); code != 2 {
			t.Errorf("%s: exit = %d, want 2", name, code)
		}
	}
	if code := runTraceShow([]string{filepath.Join(t.TempDir(), "missing.jsonl")}); code != 2 {
		t.Fatalf("missing file exit = %d", code)
	}
	for _, args := range [][]string{{}, {"a", "b"}, {"--group-by", "nope", traceShowWrite(t, "t.jsonl", captureEvent{Event: "connect", Protocol: "tcp"})}} {
		if code := runTraceShow(args); code != 2 {
			t.Errorf("args %q exit = %d, want 2", args, code)
		}
	}
}
