package edc

import (
	"bytes"
	"strings"
	"sync"
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
