//go:build linux

package edc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// duTestTree는 크기가 다른 파일, 하위 디렉터리, hard link, 밖을 가리키는 symlink를 만든다.
func duTestTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	outside := filepath.Join(filepath.Dir(root), "outside")
	for path, size := range map[string]int{
		"small": 10, "a/big": 300 << 10, "a/b/c/deep": 70 << 10, "d/one": 1, "d/two": 5 << 10,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	// 두 이름을 같은 디렉터리에 둔다. 어느 이름을 먼저 읽든 디렉터리별 합이 같아야 결과가 흔들리지 않는다.
	if err := os.Link(filepath.Join(root, "d/two"), filepath.Join(root, "d/two-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Fatal(err)
	}
	return root
}

// duExpectedBytes는 du처럼 block을 더한다. symlink는 따라가지 않고 같은 inode는 한 번만 센다.
func duExpectedBytes(t *testing.T, root string, skip string) uint64 {
	t.Helper()
	seen := map[uint64]bool{}
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip != "" && path == skip {
			return filepath.SkipDir
		}
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return err
		}
		if !seen[stat.Ino] {
			seen[stat.Ino] = true
			total += uint64(stat.Blocks) * 512
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func duTestDevice(t *testing.T, path string) string {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d", unix.Major(stat.Dev), unix.Minor(stat.Dev))
}

func duNoMounts() duSources {
	return duSources{
		mountinfo:   func() (string, error) { return "", nil },
		statfs:      func(string) (duUsage, bool) { return duUsage{}, false },
		deletedOpen: func() (map[uint64]*deletedOpenSummary, bool) { return nil, false },
	}
}

func TestDuWalkMatchesTheBlocksThatDuCounts(t *testing.T) {
	root := duTestTree(t)
	want := duExpectedBytes(t, root, "")
	for _, workers := range []int{1, 4} {
		scan, err := duWalk(context.Background(), duOptions{root: root, workers: workers, rows: 3}, duNoMounts())
		if err != nil {
			t.Fatal(err)
		}
		// 항목은 root, small, a, a/big, a/b, a/b/c, a/b/c/deep, d, d/one, d/two, outside-link다. two-link는 한 번 더 세지 않는다.
		if scan.root.total != want || scan.root.count != 11 || scan.directories != 5 || scan.partial {
			t.Fatalf("workers %d: total %d (want %d), count %d, dirs %d, partial %v", workers, scan.root.total, want, scan.root.count, scan.directories, scan.partial)
		}
		rows, _ := duSelect(scan.root, 3)
		if got := duRowPaths(rows); got != "a >a/big >a/b" {
			t.Fatalf("workers %d: rows %q", workers, got)
		}
	}
}

func TestDuWalkSkipsMountsButNotSelfBinds(t *testing.T) {
	root := duTestTree(t)
	device := duTestDevice(t, root)
	// a는 다른 곳을 원본으로 하는 bind mount다. d는 자기 자신 위에 붙인 bind mount라 그대로 센다.
	mountinfo := strings.Join([]string{
		fmt.Sprintf("30 1 %s / %s rw shared:1 - xfs /dev/test rw", device, filepath.Dir(root)),
		fmt.Sprintf("31 30 %s /elsewhere %s rw shared:1 - xfs /dev/test rw", device, filepath.Join(root, "a")),
		fmt.Sprintf("32 30 %s /tree/d %s rw - xfs /dev/test rw", device, filepath.Join(root, "d")),
		fmt.Sprintf("33 30 0:99 / %s rw - tmpfs tmpfs rw", filepath.Join(root, "missing")),
	}, "\n")
	sources := duSources{
		mountinfo: func() (string, error) { return mountinfo, nil },
		statfs: func(path string) (duUsage, bool) {
			if path != root {
				t.Errorf("statfs of the shared mount %s", path)
			}
			return duUsage{}, false
		},
	}
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 2, rows: 5}, sources)
	if err != nil {
		t.Fatal(err)
	}
	want := duExpectedBytes(t, root, filepath.Join(root, "a"))
	if scan.root.total != want {
		t.Fatalf("total %d, want %d", scan.root.total, want)
	}
	if len(scan.mounts) != 1 || scan.mounts[0].Path != filepath.Join(root, "a") || scan.mounts[0].FSType != "xfs" || !scan.mounts[0].Shared {
		t.Fatalf("mounts = %+v", scan.mounts)
	}

	crossed, err := duWalk(context.Background(), duOptions{root: root, workers: 2, rows: 5, crossMounts: true}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if crossed.root.total != duExpectedBytes(t, root, "") || len(crossed.mounts) != 0 {
		t.Fatalf("cross mounts: total %d, mounts %+v", crossed.root.total, crossed.mounts)
	}
}

func TestDuWalkReportsTheUsageOfASeparateFileSystem(t *testing.T) {
	root := duTestTree(t)
	mountinfo := fmt.Sprintf("40 1 0:99 / %s rw - ext4 /dev/other rw", filepath.Join(root, "d"))
	sources := duSources{
		mountinfo: func() (string, error) { return mountinfo, nil },
		statfs:    func(path string) (duUsage, bool) { return duUsage{Used: 42 << 20}, path == filepath.Join(root, "d") },
	}
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 1, rows: 5}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.mounts) != 1 || scan.mounts[0].FSType != "ext4" || scan.mounts[0].Shared || scan.mounts[0].UsedBytes != 42<<20 {
		t.Fatalf("mounts = %+v", scan.mounts)
	}
}

func TestDuWalkMarksAStoppedScanAsPartial(t *testing.T) {
	root := duTestTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, workers := range []int{1, 4} {
		scan, err := duWalk(ctx, duOptions{root: root, workers: workers, rows: 3}, duNoMounts())
		if err != nil {
			t.Fatal(err)
		}
		if !scan.partial || scan.root.count != 1 {
			t.Fatalf("workers %d: partial %v, count %d", workers, scan.partial, scan.root.count)
		}
	}
}

func TestDuWalkCountsUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory with mode 000")
	}
	root := duTestTree(t)
	locked := filepath.Join(root, "a/b")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 2, rows: 3}, duNoMounts())
	if err != nil {
		t.Fatal(err)
	}
	if scan.unreadableCount != 1 || len(scan.unreadable) != 1 || scan.unreadable[0] != locked {
		t.Fatalf("unreadable %d %v", scan.unreadableCount, scan.unreadable)
	}
}

func TestDuWalkWarnsWithoutTheMountTable(t *testing.T) {
	root := duTestTree(t)
	sources := duNoMounts()
	sources.mountinfo = func() (string, error) { return "", os.ErrPermission }
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 1, rows: 3}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.warnings) != 1 || !strings.Contains(scan.warnings[0], "bind mounts") {
		t.Fatalf("warnings = %q", scan.warnings)
	}
}

func TestRunDuPrintsTheTreeAndRejectsBadOptions(t *testing.T) {
	root := duTestTree(t)
	output, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	if code := runDuWith([]string{root, "--top", "2"}, "test", duNoMounts(), true, output); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "PASS  du.scan") || !strings.Contains(text, "  a/\n") || !strings.Contains(text, "    big\n") || !strings.Contains(text, "other entries)") {
		t.Fatalf("output =\n%s", text)
	}
	for _, args := range [][]string{{"--workers", "0", root}, {root, root}, {"--top", "-1"}} {
		if code := runDuWith(args, "test", duNoMounts(), true, output); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

// duWholeSources는 root를 파일 시스템의 마운트 지점처럼 보이게 한다. 실제 마운트는 만들지 않는다.
func duWholeSources(t *testing.T, root, mountRoot string, used uint64, deleted *deletedOpenSummary, partial bool) duSources {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	mountinfo := fmt.Sprintf("50 1 %s %s %s rw - xfs /dev/test rw", duTestDevice(t, root), mountRoot, root)
	return duSources{
		mountinfo: func() (string, error) { return mountinfo, nil },
		statfs:    func(string) (duUsage, bool) { return duUsage{Size: 1 << 30, Used: used}, true },
		deletedOpen: func() (map[uint64]*deletedOpenSummary, bool) {
			if deleted == nil {
				return nil, partial
			}
			return map[uint64]*deletedOpenSummary{stat.Dev: deleted}, partial
		},
	}
}

func TestDuWalkComparesTheUsageOfAWholeFileSystem(t *testing.T) {
	root := duTestTree(t)
	counted := duExpectedBytes(t, root, "")
	deleted := &deletedOpenSummary{Files: 1, Bytes: 3 << 20, Top: []deletedOpenFile{{Path: "/data/app.log", Size: 3 << 20, PID: 42, Comm: "app"}}}
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 2, rows: 3}, duWholeSources(t, root, "/", counted+deleted.Bytes+(1<<20), deleted, false))
	if err != nil {
		t.Fatal(err)
	}
	if !scan.filesystem.Whole || scan.filesystem.Mount != root || scan.filesystem.FSType != "xfs" || scan.notFound != 1<<20 {
		t.Fatalf("filesystem %+v, not found %d", scan.filesystem, scan.notFound)
	}
	if scan.deleted.Bytes != 3<<20 || len(scan.deleted.Top) != 1 || scan.deleted.Top[0].PID != 42 || scan.deleted.Partial {
		t.Fatalf("deleted %+v", scan.deleted)
	}
}

func TestDuWalkDoesNotCompareAPartOfAFileSystem(t *testing.T) {
	root := duTestTree(t)
	counted := duExpectedBytes(t, root, "")
	for name, test := range map[string]struct {
		mountRoot string
		partial   bool
		ctx       func() context.Context
	}{
		"bind mount":         {mountRoot: "/srv/tree", ctx: context.Background},
		"unreadable fds":     {mountRoot: "/", partial: true, ctx: context.Background},
		"stopped by timeout": {mountRoot: "/", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }},
	} {
		scan, err := duWalk(test.ctx(), duOptions{root: root, workers: 1, rows: 3}, duWholeSources(t, root, test.mountRoot, counted+(5<<20), nil, test.partial))
		if err != nil {
			t.Fatal(err)
		}
		if scan.filesystem.Whole || scan.notFound != 0 || scan.filesystem.UsedBytes != counted+(5<<20) || scan.deleted.Partial != test.partial {
			t.Errorf("%s: filesystem %+v, not found %d, deleted %+v", name, scan.filesystem, scan.notFound, scan.deleted)
		}
	}
}

// 이 process가 연 채로 지운 파일을 실제 /proc에서 찾는다. 크기는 파일 길이가 아니라 할당된 block이다.
func TestDuDeletedOpenStatFindsAnUnlinkedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(make([]byte, 256<<10)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	summary := scanDeletedOpen("/proc", duDeletedOpenStat, infoDeletedOpenFDLimit)[stat.Dev]
	if summary == nil {
		t.Fatal("no deleted file on the device of the temporary directory")
	}
	for _, found := range summary.Top {
		if found.Path == path && found.PID == os.Getpid() {
			if found.Size != uint64(stat.Blocks)*512 {
				t.Fatalf("size %d, want %d blocks", found.Size, stat.Blocks)
			}
			return
		}
	}
	t.Fatalf("%s is not in %+v", path, summary.Top)
}

func TestDuProcPartialReadsALinkNotOnlyTheList(t *testing.T) {
	fdDir := t.TempDir()
	if duProcPartial(fdDir) {
		t.Fatal("an empty fd list is not partial")
	}
	if err := os.Symlink("/dev/null", filepath.Join(fdDir, "0")); err != nil {
		t.Fatal(err)
	}
	if duProcPartial(fdDir) {
		t.Fatal("a readable link is not partial")
	}
	if !duProcPartial(filepath.Join(fdDir, "missing")) {
		t.Fatal("an unreadable fd list is partial")
	}
}
