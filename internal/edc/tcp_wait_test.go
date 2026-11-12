package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTCPWaitLimitFlag(t *testing.T) {
	for _, test := range []struct {
		args  []string
		on    bool
		limit time.Duration
	}{
		{[]string{"-w", "host:1"}, true, 0},
		{[]string{"--wait", "host:1"}, true, 0},
		{[]string{"-w=5m", "host:1"}, true, 5 * time.Minute},
		{[]string{"host:1", "--wait=90"}, true, 90 * time.Second},
		{[]string{"-w=0", "host:1"}, true, 0},
	} {
		var wait tcpWaitLimit
		set := flag.NewFlagSet("test", flag.ContinueOnError)
		set.Var(&wait, "w", "")
		set.Var(&wait, "wait", "")
		positional, err := parseInterspersed(set, test.args)
		if err != nil || wait.on != test.on || wait.limit != test.limit || len(positional) != 1 || positional[0] != "host:1" {
			t.Errorf("%v: wait %+v positional %v err %v", test.args, wait, positional, err)
		}
	}
	for _, value := range []string{"-1s", "soon"} {
		var wait tcpWaitLimit
		if err := wait.Set(value); err == nil {
			t.Errorf("%q must be rejected", value)
		}
	}
	// runTCP는 -w가 어떤 모양이든 wait로 넘긴다.
	for _, args := range [][]string{{"-w", "h:1"}, {"h:1", "-w=5m"}, {"--wait", "h:1"}, {"--wait=1m", "h:1"}} {
		if !hasValueFlag(args, "w") && !hasValueFlag(args, "wait") {
			t.Errorf("%v is not seen as -w", args)
		}
	}
}

// 나중에 뜨는 서버를 기다렸다가 연결한다. 결과에는 기다린 시간과 시도 수가 붙는다.
func TestWaitTCPConnectsWhenTheServerStarts(t *testing.T) {
	address := tcpWaitFreeAddress(t)
	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		listener, err := net.Listen("tcp", address)
		if err != nil {
			listening <- nil
			return
		}
		listening <- listener
	}()
	defer func() {
		if listener := <-listening; listener != nil {
			listener.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var causes []string
	result := waitTCP(ctx, address, time.Second, 100*time.Millisecond, func(_ int, row Result) { causes = append(causes, tcpWaitCause(row)) })
	if result.Status == StatusFail || result.Metrics["attempts"].(int) < 2 || !strings.Contains(result.Summary, "connected") || !strings.Contains(result.Summary, "attempt ") {
		t.Fatalf("result %+v", result)
	}
	if causes[0] != "connection refused" || causes[len(causes)-1] != "connected" {
		t.Fatalf("causes %v", causes)
	}
	if waited := result.Metrics["waited_ms"].(int64); waited < 500 || result.DurationMS != waited {
		t.Fatalf("waited %d duration %d", waited, result.DurationMS)
	}
}

// 기다린 시간이 지나면 마지막 실패의 원인을 그대로 남기고 끝난다. 기한이 끝낸 시도의 취소가 원인을 덮지 않는다.
func TestWaitTCPGivesUpWithTheLastCause(t *testing.T) {
	address := tcpWaitFreeAddress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	attempts := 0
	result := waitTCP(ctx, address, time.Second, 100*time.Millisecond, func(int, Result) { attempts++ })
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "connection_refused" {
		t.Fatalf("result %+v", result)
	}
	if got := result.Metrics["attempts"].(int); got < 3 || got != attempts || !strings.Contains(result.Summary, "not connected · waited") {
		t.Fatalf("attempts %d reported %d summary %q", got, attempts, result.Summary)
	}
}

// pipe에서는 원인이 바뀔 때만 한 줄 쓴다. 터미널에서는 한 줄을 고쳐 쓰고, 끝나면 지운다.
func TestTCPWaitProgressWritesOnChange(t *testing.T) {
	refused := Result{Status: StatusFail, Error: &DiagnosticError{Kind: "connection_refused"}}
	timeout := Result{Status: StatusFail, Error: &DiagnosticError{Kind: "timeout"}}
	var out bytes.Buffer
	progress := &tcpWaitProgress{out: &out, address: "h:1", started: time.Now()}
	for attempt, row := range []Result{refused, refused, timeout, timeout, refused, {Status: StatusPass}} {
		progress.attempted(attempt+1, row)
	}
	progress.finish()
	want := "edc tcp check: h:1 attempt 1: connection refused\nedc tcp check: h:1 attempt 3: timeout\nedc tcp check: h:1 attempt 5: connection refused\nedc tcp check: h:1 attempt 6: connected\n"
	if out.String() != want {
		t.Fatalf("progress:\n%s", out.String())
	}
	out.Reset()
	live := &tcpWaitProgress{out: &out, live: true, address: "h:1", started: time.Now()}
	live.attempted(1, refused)
	live.attempted(2, refused)
	live.finish()
	if lines := strings.Count(out.String(), "\n"); lines != 0 || strings.Count(out.String(), "\r\033[K") != 3 || !strings.Contains(out.String(), "attempt 2 · connection refused") {
		t.Fatalf("live progress %q", out.String())
	}
	// 명령줄의 대상에 든 제어 문자는 진행 줄에 그대로 나오지 않는다. 줄을 고쳐 쓰는 \r\033[K만 남는다.
	for _, live := range []bool{false, true} {
		out.Reset()
		progress := &tcpWaitProgress{out: &out, live: live, address: "h\x1b[2J\x07:1", started: time.Now()}
		progress.attempted(1, refused)
		if text := strings.ReplaceAll(out.String(), "\r\033[K", ""); strings.ContainsAny(text, "\x1b\x07") {
			t.Errorf("live %v: %q", live, out.String())
		}
	}
}

func tcpWaitFreeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

// -w는 마지막 값을 따른다. --wait=false면 꺼지고 그 flag를 빼서 보통 check로 넘긴다. -w=0은 끝없이 기다리는 값이다.
func TestTCPWaitRequested(t *testing.T) {
	for _, test := range []struct {
		args []string
		on   bool
		rest string
	}{
		{[]string{"-w", "h:1"}, true, "h:1"},
		{[]string{"--wait=false", "h:1"}, false, "h:1"},
		{[]string{"-w", "--wait=false", "h:1"}, false, "h:1"},
		{[]string{"--wait=false", "-w=5m", "h:1"}, true, "h:1"},
		{[]string{"-w=0", "h:1"}, true, "h:1"},
		{[]string{"--count", "3", "-wait=false", "h:1"}, false, "--count 3 h:1"},
		{[]string{"h:1", "--", "-w"}, false, "h:1 -- -w"},
		{[]string{"-v", "h:1"}, false, "-v h:1"},
	} {
		if got := tcpWaitRequested(test.args); got != test.on {
			t.Errorf("%v: requested %v", test.args, got)
		}
		if rest := strings.Join(tcpWithoutWait(test.args), " "); !test.on && rest != test.rest {
			t.Errorf("%v: without -w %q, want %q", test.args, rest, test.rest)
		}
	}
}

// runTCP부터 실제 명령 경로로 실행한다. -w=2500ms는 닫힌 포트에 1초마다 시도하다 2.5초 뒤 실패하고, -w는 늦게 뜬 서버에 연결하며,
// --wait=false는 기다리지 않고 한 번만 시도한다.
func TestRunTCPWaitCommand(t *testing.T) {
	run := func(args ...string) (int, map[string]interface{}, time.Duration, string) {
		t.Helper()
		previousOut, previousErr := os.Stdout, os.Stderr
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer devNull.Close()
		os.Stdout, os.Stderr = write, devNull
		output := make(chan []byte, 1)
		go func() {
			data, _ := io.ReadAll(read)
			output <- data
		}()
		started := time.Now()
		code := runTCP(append([]string{"check", "--json", "-"}, args...), "test")
		elapsed := time.Since(started)
		os.Stdout, os.Stderr = previousOut, previousErr
		write.Close()
		data := <-output
		var report struct {
			Results []struct {
				Metrics map[string]interface{} `json:"metrics"`
				Error   struct {
					Kind string `json:"kind"`
				} `json:"error"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &report); err != nil || len(report.Results) != 1 {
			t.Fatalf("%v: %v %s", args, err, data)
		}
		return code, report.Results[0].Metrics, elapsed, report.Results[0].Error.Kind
	}
	closed := tcpWaitFreeAddress(t)
	if code, metrics, elapsed, _ := run("-w=2500ms", closed); code != 1 || elapsed < 2500*time.Millisecond || metrics["attempts"].(float64) < 3 {
		t.Fatalf("-w=2500ms: code %d elapsed %v metrics %v", code, elapsed, metrics)
	}
	if code, metrics, elapsed, _ := run("--wait=false", closed); code != 1 || elapsed > 900*time.Millisecond || metrics["attempts"] != nil {
		t.Fatalf("--wait=false: code %d elapsed %v metrics %v", code, elapsed, metrics)
	}
	// 연결할 수 없는 port는 기다려도 연결되지 않는다. 재시도하지 않고 입력 오류로 바로 끝난다.
	for _, target := range []string{"127.0.0.1:", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:no-such-service"} {
		if code, metrics, elapsed, kind := run("-w=5s", target); code != 1 || kind != "input" || elapsed > 900*time.Millisecond || metrics["attempts"] != nil {
			t.Fatalf("%s: code %d kind %q elapsed %v metrics %v", target, code, kind, elapsed, metrics)
		}
	}
	late := tcpWaitFreeAddress(t)
	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		listener, _ := net.Listen("tcp", late)
		listening <- listener
	}()
	defer func() {
		if listener := <-listening; listener != nil {
			listener.Close()
		}
	}()
	if code, metrics, _, _ := run("-w=10s", late); code != 0 || metrics["attempts"].(float64) < 2 {
		t.Fatalf("-w: code %d metrics %v", code, metrics)
	}
}
