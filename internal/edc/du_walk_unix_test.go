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

// duOpenHang은 duOpen을 한 번만 바꾼다. duWalk가 남겨 둔 goroutine이 test가 끝난 뒤에도 duOpen을 읽는다.
var duOpenHang = struct {
	once  sync.Once
	mu    sync.Mutex
	paths map[string]chan struct{}
}{paths: map[string]chan struct{}{}}

func duHangOpen(t *testing.T, paths ...string) {
	duOpenHang.once.Do(func() {
		original := duOpen
		duOpen = func(path string, mode int, perm uint32) (int, error) {
			duOpenHang.mu.Lock()
			release := duOpenHang.paths[path]
			duOpenHang.mu.Unlock()
			if release != nil {
				<-release
			}
			return original(path, mode, perm)
		}
	})
	release := make(chan struct{})
	duOpenHang.mu.Lock()
	for _, path := range paths {
		duOpenHang.paths[path] = release
	}
	duOpenHang.mu.Unlock()
	t.Cleanup(func() {
		duOpenHang.mu.Lock()
		for _, path := range paths {
			delete(duOpenHang.paths, path)
		}
		duOpenHang.mu.Unlock()
		close(release)
	})
}

func TestDuWalkReturnsAtDeadlineWhenOpenHangs(t *testing.T) {
	parent := t.TempDir()
	tree := func(name string) string {
		root := filepath.Join(parent, name)
		for _, dir := range []string{"hang", "ok"} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, dir, "file"), make([]byte, 64<<10), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	child, whole := tree("child"), tree("whole")
	// macOS의 TCC 보호 경로는 open이 돌아오지 않는다.
	duHangOpen(t, filepath.Join(child, "hang"), whole)
	sources, _ := hostDuSources()
	cases := []struct {
		root    string
		workers int
		dirs    int64
	}{
		{child, 4, 2},
		{whole, 4, 0},
		{whole, 1, 0},
	}
	for _, test := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		done := make(chan duScan, 1)
		go func() {
			scan, err := duWalk(ctx, duOptions{root: test.root, workers: test.workers, rows: 20}, sources)
			if err != nil {
				t.Error(err)
			}
			done <- scan
		}()
		select {
		case scan := <-done:
			warning := T("du.warning.abandoned", 1)
			if !scan.partial || scan.directories != test.dirs || scan.walker != nil || !slices.Contains(scan.warnings, warning) {
				t.Errorf("%s workers %d: partial=%v dirs=%d walker=%v warnings=%v", test.root, test.workers, scan.partial,
					scan.directories, scan.walker != nil, scan.warnings)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s workers %d: duWalk did not return after the deadline", test.root, test.workers)
		}
		cancel()
	}
}
