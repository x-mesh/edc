package edc

import (
	"strings"
	"testing"
	"time"
)

func TestViewExceptionHead(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{"java.lang.IllegalArgumentException: Invalid URI path", "IllegalArgumentException: Invalid URI path"},
		{"java.lang.NullPointerException", "NullPointerException"},
		{"java.lang.IllegalStateException:", "IllegalStateException"},
		{"Caused by: java.net.SocketTimeoutException: Read timed out", "SocketTimeoutException: Read timed out"},
		{`Exception in thread "main" java.lang.OutOfMemoryError: Java heap space`, "OutOfMemoryError: Java heap space"},
		{"foundation.icon.icx.transport.jsonrpc.RpcError: ERROR -32602 invalid params", "RpcError: ERROR -N invalid params"},
		{"com.example.Outer$InnerException: boom", "Outer$InnerException: boom"},
		{"ValueError: invalid literal for int() with base 10: 'x'", "ValueError: invalid literal for int() with base N: 'x'"},
		{"TypeError: Cannot read properties of undefined", "TypeError: Cannot read properties of undefined"},
		{"java.net.ConnectException: connect to 10.0.0.12:8080   failed after 3000ms", "ConnectException: connect to N.N.N.N:N failed after N"},
		{"java.lang.IllegalArgumentException: user 550e8400-e29b-41d4-a716-446655440000 missing", "IllegalArgumentException: user N-N-N-N-N missing"},
	} {
		got, ok := viewExceptionHead(test.text)
		if !ok || got != test.want {
			t.Errorf("%q = %q %v, want %q", test.text, got, ok, test.want)
		}
	}
	long, _ := viewExceptionHead("java.lang.RuntimeException: " + strings.Repeat("a ", 100))
	if message := strings.TrimPrefix(long, "RuntimeException: "); len([]rune(message)) != viewExceptionMessageLimit || !strings.HasSuffix(message, "…") {
		t.Errorf("long message %q", message)
	}
	for _, text := range []string{
		"\tat com.example.Api.call(Api.java:10)",
		"... 12 more",
		"Error occurred while syncing",
		"the Exception: x",
		"com.example.service: boom",
		"lowercaseerror: x",
		"com..BadException: x",
		"1Exception: x",
		"",
	} {
		if key, ok := viewExceptionHead(text); ok {
			t.Errorf("%q is not an exception line: %q", text, key)
		}
	}
}

func TestViewSpringParts(t *testing.T) {
	for _, test := range []struct{ rest, thread, logger, message string }{
		{" --- [cheduled-pool 8]       c.i.m.i.v.VcScoreService : ---> Vc Score", "cheduled-pool 8", "c.i.m.i.v.VcScoreService", "---> Vc Score"},
		{" 4242 --- [app] [main] o.s.b.SpringApplication : Started in 2.1 seconds", "main", "o.s.b.SpringApplication", "Started in 2.1 seconds"},
		{"  --- [main] o.s.b.SpringApplication :", "main", "o.s.b.SpringApplication", ""},
	} {
		thread, logger, message, ok := viewSpringParts(test.rest)
		if !ok || thread != test.thread || logger != test.logger || message != test.message {
			t.Errorf("%q = %q %q %q %v", test.rest, thread, logger, message, ok)
		}
	}
	for _, rest := range []string{" [http-7] c.e.Api - request failed", " --- [main] noColon here", " --- [unclosed"} {
		if _, _, _, ok := viewSpringParts(rest); ok {
			t.Errorf("%q is not the Spring Boot layout", rest)
		}
	}
}

// Spring Boot 로그는 level 형식으로 읽고, logger를 상위 프로그램으로, stack trace의 첫 예외를 그 event의 예외로 센다.
func TestViewScreenCountsExceptions(t *testing.T) {
	model := viewScreenWith(t, "spring.log", 140, 40)
	if model.format != viewFormatLevel {
		t.Fatalf("format %q", model.format)
	}
	first, _ := model.line(1)
	if first.program != "o.s.s.s.TaskUtils$LoggingErrorHandler" || first.message != "Unexpected error occurred in scheduled task" || first.exception != "IllegalArgumentException: Invalid URI path" {
		t.Fatalf("event line %+v", first)
	}
	for _, number := range []int{2, 3, 4} {
		if line, _ := model.line(number); !line.cont || line.exception != first.exception {
			t.Fatalf("line %d must continue the event with its exception: %+v", number, line)
		}
	}
	want := map[string]int{
		"IllegalArgumentException: Invalid URI path": 2,
		// 메시지의 블록 번호가 달라도 한 열쇠다. 뒤의 "Caused by: RpcError"는 따로 세지 않는다.
		"ScoreSyncerException: block N not found": 2,
	}
	if len(model.stats.exceptions) != len(want) {
		t.Fatalf("exceptions %v", model.stats.exceptions)
	}
	for key, count := range want {
		if model.stats.exceptions[key] != count {
			t.Fatalf("exceptions %v", model.stats.exceptions)
		}
	}
	if model.stats.programs["c.i.m.i.c.IconServiceManager"] != 3 || model.stats.failures != 5 {
		t.Fatalf("programs %v failures %d", model.stats.programs, model.stats.failures)
	}
	panel, _ := model.panelColumn(model.panelWidth(), false)
	joined := strings.Join(panel, "\n")
	// 예외 항목은 패널 폭에서 두 줄로 나뉜다. LAST ERROR는 logger 앞의 정렬 공백을 줄인다.
	for _, want := range []string{"TOP EXCEPTIONS", "      2 IllegalArgumentException:\n        Invalid URI path", "      2 ScoreSyncerException: block N\n        not found", "TOP PROGRAMS", "c.i.m.i.c.IconServiceManager"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("panel misses %q:\n%s", want, joined)
		}
	}
	if last := viewPanelError(model.stats.lastError, model.format); last != "2026-11-12 13:51:15.000 ERROR --- [main] c.i.m.i.Application : shutdown requested" {
		t.Fatalf("last error %q", last)
	}
	if strings.Index(joined, "TOP EXCEPTIONS") > strings.Index(joined, "TOP PROGRAMS") {
		t.Fatal("exceptions come before the other top lists")
	}
	model.detail = 1
	if detail := strings.Join(model.detailView(false), "\n"); !strings.Contains(detail, "exception  IllegalArgumentException: Invalid URI path") || !strings.Contains(detail, "logger     o.s.s.s.TaskUtils$LoggingErrorHandler") {
		t.Fatalf("detail:\n%s", detail)
	}
}

// exception: filter는 예외가 난 event와 그 stack trace를 함께 남긴다. 패널의 TOP EXCEPTIONS 항목으로도 건다.
func TestViewScreenFiltersByException(t *testing.T) {
	model := viewScreenWith(t, "spring.log", 140, 40)
	if err := model.applyFilter("exception:ScoreSyncer*"); err != nil {
		t.Fatal(err)
	}
	if shown := model.shown(); len(shown) != 8 || shown[0] != 10 || shown[len(shown)-1] != 17 {
		t.Fatalf("shown %v", shown)
	}
	if stats := model.panelStats(); stats.lines != 2 || stats.exceptions["ScoreSyncerException: block N not found"] != 2 || len(stats.exceptions) != 1 {
		t.Fatalf("filtered stats %+v", stats)
	}
	model = viewPress(model, "x", "tab")
	picks := model.panelPicks()
	at := -1
	for index, pick := range picks {
		if pick == (viewPanelPick{"exception", "IllegalArgumentException: Invalid URI path"}) {
			at = index
		}
	}
	if at < 0 {
		t.Fatalf("picks %v", picks)
	}
	model.pick = picks[at]
	model = viewPress(model, "enter")
	if model.filter != `exception:"IllegalArgumentException: Invalid URI path"` || model.panelStats().lines != 2 || len(model.shown()) != 7 {
		t.Fatalf("pick filter %q lines %d shown %v", model.filter, model.panelStats().lines, model.shown())
	}
}

// 예외 줄이 event와 다른 묶음으로 들어와도 그 event에 붙는다. filter가 걸린 동안 들어와도 패널 수가 맞다. event가 밀려나면
// 그 예외도 빠진다.
func TestViewScreenAttachesALateExceptionLine(t *testing.T) {
	model := newViewScreenModel("app", 4)
	model.ingest([]string{"2026-11-12 13:51:11.258 ERROR --- [main] c.e.Job : failed"}, time.Now(), false)
	if err := model.applyFilter("exception:*"); err != nil {
		t.Fatal(err)
	}
	if model.panelStats().lines != 0 {
		t.Fatal("no exception yet")
	}
	model.ingest([]string{"java.lang.IllegalStateException: ERROR state", "\tat c.e.Job.run(Job.java:1)"}, time.Now(), false)
	if line, _ := model.line(1); !line.cont || !line.exceptionHead {
		t.Fatalf("an exception line with ERROR in its message continues the event: %+v", line)
	}
	if stats := model.panelStats(); stats.lines != 1 || stats.exceptions["IllegalStateException: ERROR state"] != 1 || model.stats.exceptions["IllegalStateException: ERROR state"] != 1 {
		t.Fatalf("late exception: matched %+v all %v", stats, model.stats.exceptions)
	}
	if len(model.shown()) != 3 {
		t.Fatalf("shown %v", model.shown())
	}
	model.ingest([]string{"2026-11-12 13:51:12.000 INFO --- [main] c.e.Job : ok", "2026-11-12 13:51:13.000 INFO --- [main] c.e.Job : ok"}, time.Now(), false)
	if len(model.stats.exceptions) != 0 || model.panelStats().lines != 0 {
		t.Fatalf("evicted event keeps its exception: %v %+v", model.stats.exceptions, model.panelStats())
	}
}

// 이어지는 줄은 event의 exception을 물려받지만 통계에 세지 않는다. 예외는 addException으로 event당 한 번만 센다.
func TestViewStatsCountAnExceptionOncePerEvent(t *testing.T) {
	stats := newViewStats()
	event := viewLine{text: "2026-11-12 13:51:11.258 ERROR --- [main] c.e.Job : failed", level: "error"}
	head := viewLine{text: "java.lang.IllegalStateException: boom", cont: true, level: "error", exception: "IllegalStateException: boom", exceptionHead: true}
	frame := viewLine{text: "\tat c.e.Job.run(Job.java:1)", cont: true, level: "error", exception: head.exception}
	stats.add(event)
	stats.addException(head.exception)
	stats.add(head)
	stats.add(frame)
	if stats.lines != 1 || stats.exceptions[head.exception] != 1 || len(stats.exceptions) != 1 {
		t.Fatalf("lines %d exceptions %v", stats.lines, stats.exceptions)
	}
	event.exception = head.exception
	stats.remove(head)
	stats.remove(frame)
	stats.remove(event)
	if stats.lines != 0 || len(stats.exceptions) != 0 {
		t.Fatalf("after remove: lines %d exceptions %v", stats.lines, stats.exceptions)
	}
	// --stats와 화면이 같은 수를 낸다. 예외 줄과 stack trace 줄이 있어도 event 수만큼이다.
	summary, err := viewSummarize(strings.NewReader(event.text + "\n" + head.text + "\n" + frame.text + "\n" + frame.text + "\n"))
	if err != nil || summary.stats.exceptions[head.exception] != 1 {
		t.Fatalf("--stats exceptions %v %v", summary.stats.exceptions, err)
	}
}
