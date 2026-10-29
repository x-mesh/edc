//go:build darwin

package edc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func duDarwinTestTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for d := range 4 {
		dir := filepath.Join(root, fmt.Sprint(d))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := range 300 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(f)), make([]byte, 4096+f), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := unix.Setxattr(filepath.Join(root, "0", "2"), "com.apple.ResourceFork", make([]byte, 9000), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "0", "1"), filepath.Join(root, "1", "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "0"), filepath.Join(root, "directory-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "broken-link")); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(root, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDuDarwinMatchesSystemDu(t *testing.T) {
	root := duDarwinTestTree(t)
	output, err := exec.Command("/usr/bin/du", "-skx", root).Output()
	if err != nil {
		t.Fatal(err)
	}
	kib, err := strconv.ParseUint(strings.Fields(string(output))[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var want uint64
	seen := map[duInode]bool{}
	err = filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return err
		}
		key := duInode{dev: uint64(stat.Dev), ino: stat.Ino}
		if !seen[key] {
			seen[key] = true
			want += uint64(stat.Blocks) * 512
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sources, _ := hostDuSources()
	for _, workers := range []int{1, 4} {
		scan, err := duWalk(context.Background(), duOptions{root: root, workers: workers, rows: 20}, sources)
		if err != nil {
			t.Fatal(err)
		}
		if scan.root.total != want || (scan.root.total+1023)/1024 != kib || scan.root.count != 1208 || scan.directories != 5 || scan.partial || scan.unreadableCount != 0 {
			t.Fatalf("workers %d: bytes=%d stat=%d du=%d entries=%d dirs=%d partial=%v unreadable=%d", workers, scan.root.total, want, kib, scan.root.count, scan.directories, scan.partial, scan.unreadableCount)
		}
		if scan.filesystem.Whole || scan.notFound != 0 {
			t.Fatalf("APFS accounting: %+v", scan.filesystem)
		}
	}
}

func TestDuDarwinCancellationAndUnreadableDirectory(t *testing.T) {
	root := duDarwinTestTree(t)
	sources, _ := hostDuSources()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, workers := range []int{1, 4} {
		scan, err := duWalk(ctx, duOptions{root: root, workers: workers, rows: 20}, sources)
		if err != nil {
			t.Fatal(err)
		}
		if !scan.partial || scan.directories != 0 {
			t.Fatalf("workers %d: partial=%v dirs=%d", workers, scan.partial, scan.directories)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read directories without permissions")
	}
	blocked := filepath.Join(root, "0")
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 4, rows: 20}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if scan.unreadableCount != 1 || len(scan.unreadable) != 1 || scan.unreadable[0] != blocked {
		t.Fatalf("unreadable=%d paths=%v", scan.unreadableCount, scan.unreadable)
	}
}

func TestDuDarwinMountBoundaryAllowsFirmlinkDevice(t *testing.T) {
	walker := duWalker{rootDev: 1, mountPoints: map[string]diskMount{"/data/mounted": {FSType: "apfs"}}}
	if walker.mountSkip("/data/firmlink") != nil {
		t.Fatal("device change alone must not skip a firmlink")
	}
	if skip := walker.mountSkip("/data/mounted"); skip == nil || !skip.Shared {
		t.Fatalf("mount=%+v", skip)
	}
	walker.options.crossMounts = true
	if walker.mountSkip("/data/mounted") != nil {
		t.Fatal("cross mounts must include mounted directories")
	}
}

func TestDuDarwinBulkParserRejectsInvalidRecords(t *testing.T) {
	record := make([]byte, 80)
	binary.LittleEndian.PutUint32(record[4:], duDarwinAttrs.Commonattr)
	binary.LittleEndian.PutUint32(record[16:], duDarwinAttrs.Fileattr)
	binary.LittleEndian.PutUint32(record[28:], 40)
	binary.LittleEndian.PutUint32(record[32:], 5)
	copy(record[68:], "file\x00")
	binary.LittleEndian.PutUint32(record[40:], 1)
	binary.LittleEndian.PutUint32(record[56:], 1)
	binary.LittleEndian.PutUint64(record[60:], 4096)
	entry, err := duDarwinParse(record)
	if err != nil || string(entry.name) != "file" || entry.bytes != 4096 || entry.stat {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 0xffffffff) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[32:], 1000) },
		func(b []byte) { b[72] = 1 },
		func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 0) },
	} {
		broken := append([]byte(nil), record...)
		mutate(broken)
		if _, err := duDarwinParse(broken); !errors.Is(err, unix.EIO) {
			t.Fatalf("invalid record: %v", err)
		}
	}
	if _, err := duDarwinParse(record[:4]); !errors.Is(err, unix.EIO) {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(record[24:], uint32(unix.EACCES))
	if _, err := duDarwinParse(record); !errors.Is(err, unix.EACCES) {
		t.Fatal(err)
	}
}

func TestDuDarwinSnapshotMeasuresFiles(t *testing.T) {
	root := duDarwinTestTree(t)
	result := duSnapshotResult(context.Background(), root, duDefaultTimeout)
	if result.Status != StatusPass || result.Metrics["bytes"].(uint64) == 0 {
		t.Fatalf("result=%+v", result)
	}
}

func duDarwinOpenFDCount(t *testing.T) int {
	t.Helper()
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	count := 0
	for fd := range int(min(limit.Cur, 4096)) {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			count++
		}
	}
	return count
}

func duDarwinDeepTestTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := root
	buffer := duReadBuffer()
	for range duDarwinParentFDLimit + 12 {
		for _, name := range []string{"a", "z"} {
			if err := os.Mkdir(filepath.Join(path, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(path, "file"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		var last string
		for {
			n, err := duDarwinRead(fd, buffer)
			if err != nil {
				unix.Close(fd)
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			remaining := buffer
			for range n {
				length := int(binary.LittleEndian.Uint32(remaining))
				entry, err := duDarwinParse(remaining[:length])
				if err != nil {
					unix.Close(fd)
					t.Fatal(err)
				}
				if entry.dir {
					last = string(entry.name)
				}
				remaining = remaining[length:]
			}
		}
		unix.Close(fd)
		if last == "" {
			t.Fatal("no child directory")
		}
		path = filepath.Join(path, last)
	}
	return root
}

func TestDuDarwinParentFDCacheEvictionAndClosure(t *testing.T) {
	root := duDarwinDeepTestTree(t)
	sources, _ := hostDuSources()
	before := duDarwinOpenFDCount(t)
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 1, rows: 20}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if scan.unreadableCount != 0 || scan.directories != int64(1+2*(duDarwinParentFDLimit+12)) || scan.partial {
		t.Fatalf("dirs=%d unreadable=%d partial=%v", scan.directories, scan.unreadableCount, scan.partial)
	}
	if after := duDarwinOpenFDCount(t); after != before {
		t.Fatalf("open fd before=%d after=%d", before, after)
	}
	parallel, err := duWalk(context.Background(), duOptions{root: root, workers: 4, rows: 20}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if scan.root.total != parallel.root.total || scan.root.count != parallel.root.count {
		t.Fatal("parent-fd walk differs from path-based walk")
	}
}

type duCancelAtProgress struct {
	context.Context
	cancel   context.CancelFunc
	progress *atomic.Int64
}

func (ctx duCancelAtProgress) Err() error {
	if ctx.progress.Load() >= 8 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestDuDarwinCancellationClosesCachedParents(t *testing.T) {
	root := duDarwinDeepTestTree(t)
	var progress atomic.Int64
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := duCancelAtProgress{Context: base, cancel: cancel, progress: &progress}
	sources, _ := hostDuSources()
	before := duDarwinOpenFDCount(t)
	scan, err := duWalk(ctx, duOptions{root: root, workers: 1, rows: 20, progress: &progress}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.partial || scan.directories < 8 {
		t.Fatalf("partial=%v dirs=%d", scan.partial, scan.directories)
	}
	if after := duDarwinOpenFDCount(t); after != before {
		t.Fatalf("open fd before=%d after=%d", before, after)
	}
}

func TestDuDarwinParentFDWithLowOpenFileLimit(t *testing.T) {
	const childEnv = "EDC_TEST_DU_LOW_FD"
	if os.Getenv(childEnv) != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestDuDarwinParentFDWithLowOpenFileLimit$", "-test.count=1")
		command.Env = append(os.Environ(), childEnv+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("low fd child: %v\n%s", err, output)
		}
		return
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	original := limit
	defer func() {
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Error(err)
		}
	}()
	limit.Cur = min(limit.Cur, 24)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	root := duDarwinDeepTestTree(t)
	sources, _ := hostDuSources()
	before := duDarwinOpenFDCount(t)
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 1, rows: 20}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if scan.unreadableCount != 0 || scan.directories != int64(1+2*(duDarwinParentFDLimit+12)) || scan.partial {
		t.Fatalf("dirs=%d unreadable=%d partial=%v", scan.directories, scan.unreadableCount, scan.partial)
	}
	if after := duDarwinOpenFDCount(t); after != before {
		t.Fatalf("open fd before=%d after=%d", before, after)
	}
}
