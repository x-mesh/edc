package edc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"runtime/debug"
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

// --raw는 GC를 늦추지만, 사용자가 GOMEMLIMIT으로 정한 더 낮은 한도는 그대로 둔다.
func TestTraceRawGCKeepsALowerMemoryLimit(t *testing.T) {
	gcPercent, limit := debug.SetGCPercent(100), debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetGCPercent(gcPercent); debug.SetMemoryLimit(limit) })
	debug.SetMemoryLimit(64 << 20)
	setTraceRawGC()
	if got := debug.SetMemoryLimit(-1); got != 64<<20 {
		t.Fatalf("a lower limit was replaced: %d", got)
	}
	debug.SetMemoryLimit(math.MaxInt64)
	setTraceRawGC()
	if got := debug.SetMemoryLimit(-1); got != traceRawMemoryLimit {
		t.Fatalf("no limit was set: %d", got)
	}
}

func BenchmarkTraceRawWriter(b *testing.B) {
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Process: "service", Source: "10.0.0.1:40000", Destination: "10.0.0.2:443", Method: "POST", Target: "api.example", Path: "/orders/42", Payload: "POST /orders/42 HTTP/1.1\r\nAuthorization: Bearer secret\r\n\r\n{\"order\":42}"}
	b.Run("encode", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			if _, err := json.Marshal(event); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("encode_buffered", func(b *testing.B) {
		raw := newTraceRawWriter(io.Discard)
		defer raw.Close()
		encoder := json.NewEncoder(raw)
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			if err := encoder.Encode(event); err != nil {
				b.Fatal(err)
			}
		}
	})
}
