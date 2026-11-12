package edc

import (
	"bytes"
	"context"
	"flag"
	"net"
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
