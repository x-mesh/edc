package edc

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTraceRawWriterFlushesWithoutNewLines(t *testing.T) {
	var out lockedBuffer
	raw := newTraceRawWriter(&out)
	raw.Write([]byte("one\n"))
	deadline := time.Now().Add(5 * time.Second)
	for out.String() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if out.String() != "one\n" {
		t.Fatalf("output after the interval = %q", out.String())
	}
	raw.Write([]byte("two\n"))
	raw.Close()
	if got := out.String(); got != "one\ntwo\n" {
		t.Fatalf("output after Close = %q", strings.TrimSpace(got))
	}
}

type failingRawOutput struct{ calls atomic.Int32 }

func (out *failingRawOutput) Write([]byte) (int, error) {
	out.calls.Add(1)
	return 0, syscall.ENOSPC
}

// 마지막으로 비울 때 난 쓰기 오류와, 주기적으로 비우다 난 오류를 Close가 돌려준다. --raw가 잃은 출력을 성공으로 끝내지 않는다.
func TestTraceRawWriterReportsAWriteFailure(t *testing.T) {
	var last failingRawOutput
	raw := newTraceRawWriter(&last)
	raw.Write([]byte("one\n"))
	if err := raw.Close(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Close after a failed final flush = %v", err)
	}
	if err := raw.Close(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("second Close = %v", err)
	}
	var periodic failingRawOutput
	raw = newTraceRawWriter(&periodic)
	raw.Write([]byte("one\n"))
	deadline := time.Now().Add(5 * time.Second)
	for periodic.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if periodic.calls.Load() == 0 {
		t.Fatal("the interval did not flush")
	}
	if err := raw.Close(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Close after a failed periodic flush = %v", err)
	}
}
