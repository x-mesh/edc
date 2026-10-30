//go:build linux || darwin

package edc

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

type duHang struct {
	at, calls int
	release   chan struct{}
}

// duHangs는 duBeforeRead를 한 번만 바꾼다. duWalk가 남겨 둔 goroutine이 test가 끝난 뒤에도 duBeforeRead를 읽는다.
var duHangs = struct {
	once  sync.Once
	mu    sync.Mutex
	paths map[string]*duHang
}{paths: map[string]*duHang{}}

// duHangAt은 path에서 at번째 duBeforeRead 호출을 test가 끝날 때까지 막는다. 1번째는 open, 2번째는 첫 목록 읽기다.
func duHangAt(t *testing.T, path string, at int) {
	duHangs.once.Do(func() {
		duBeforeRead = func(path string) {
			duHangs.mu.Lock()
			hang := duHangs.paths[path]
			var release chan struct{}
			if hang != nil {
				hang.calls++
				if hang.calls == hang.at {
					release = hang.release
				}
			}
			duHangs.mu.Unlock()
			if release != nil {
				<-release
			}
		}
	})
	hang := &duHang{at: at, release: make(chan struct{})}
	duHangs.mu.Lock()
	duHangs.paths[path] = hang
	duHangs.mu.Unlock()
	t.Cleanup(func() {
		duHangs.mu.Lock()
		delete(duHangs.paths, path)
		duHangs.mu.Unlock()
		close(hang.release)
	})
}

func TestDuWalkReturnsAtDeadlineWhenACallHangs(t *testing.T) {
	const (
		open = 1
		read = 2
	)
	cases := []struct {
		name     string
		child    bool
		at       int
		workers  int
		minDirs  int64
		minBytes uint64
	}{
		{"child open parallel", true, open, 4, 2, 64 << 10},
		{"child open sequential", true, open, 1, 1, 0},
		{"child read parallel", true, read, 4, 2, 64 << 10},
		{"child read sequential", true, read, 1, 1, 0},
		{"root open parallel", false, open, 4, 0, 0},
		{"root open sequential", false, open, 1, 0, 0},
	}
	sources, _ := hostDuSources()
	const timeout = 100 * time.Millisecond
	for _, test := range cases {
		root := filepath.Join(t.TempDir(), "tree")
		for _, dir := range []string{"hang", "ok"} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, dir, "file"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		hung := root
		if test.child {
			hung = filepath.Join(root, "hang")
		}
		duHangAt(t, hung, test.at)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		started := time.Now()
		done := make(chan duScan, 1)
		go func() {
			scan, err := duWalk(ctx, duOptions{root: root, workers: test.workers, rows: 20}, sources)
			if err != nil {
				t.Error(err)
			}
			done <- scan
		}()
		select {
		case scan := <-done:
			elapsed := time.Since(started)
			// 시간이 다 된 뒤 유예 시간만 더 기다린다. 1초 여유는 CI runner의 지연 몫이다.
			if limit := timeout + duAbandonGrace + time.Second; elapsed > limit {
				t.Errorf("%s: returned after %s, limit %s", test.name, elapsed, limit)
			}
			warning := T("du.warning.abandoned", 1)
			if !scan.partial || scan.directories < test.minDirs || scan.root.total < test.minBytes || scan.walker != nil ||
				!slices.Contains(scan.warnings, warning) {
				t.Errorf("%s: partial=%v dirs=%d total=%d walker=%v warnings=%v", test.name, scan.partial, scan.directories,
					scan.root.total, scan.walker != nil, scan.warnings)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: duWalk did not return after the deadline", test.name)
		}
		cancel()
	}
}

func TestDuBufferKeepReleasesWideDirectories(t *testing.T) {
	var buffer duBuffer
	small := make([]duEntry, 3, 8)
	small[0] = duEntry{name: "a", path: "/a", skip: &duMount{Path: "/a"}}
	buffer.keep(small, 8)
	if cap(buffer.entries) != 8 || len(buffer.entries) != 0 {
		t.Fatalf("small array: len=%d cap=%d", len(buffer.entries), cap(buffer.entries))
	}
	if kept := buffer.entries[:1][0]; kept.name != "" || kept.path != "" || kept.skip != nil {
		t.Fatalf("the kept array still refers to %+v", kept)
	}
	buffer.keep(make([]duEntry, 9), 8)
	if buffer.entries != nil {
		t.Fatalf("wide array kept: cap=%d", cap(buffer.entries))
	}
}
