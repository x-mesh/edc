package edc

import (
	"strings"
	"testing"
	"time"
)

func duTestDir(name string, bytes uint64, children ...*duNode) *duNode {
	node := &duNode{name: name, dir: true, bytes: bytes, entries: 1, children: children}
	for _, child := range children {
		child.parent = node
	}
	return node
}

func duTestFile(name string, bytes uint64) *duNode {
	return &duNode{name: name, bytes: bytes, entries: 1}
}

func duRowPaths(rows []duRow) string {
	var parts []string
	for _, row := range rows {
		parts = append(parts, strings.Repeat(">", row.Depth)+row.Path)
	}
	return strings.Join(parts, " ")
}

func TestDuSelectOpensTheLargestEntriesFirst(t *testing.T) {
	root := duTestDir("/data", 4,
		duTestDir("a", 4, duTestFile("big", 60), duTestDir("nested", 4, duTestFile("x", 26))),
		duTestDir("b", 50),
		duTestFile("c", 10),
		duTestFile("empty", 0),
	)
	duTotal(root)
	rows, other := duSelect(root, 3)
	if got := duRowPaths(rows); got != "a >a/big b" {
		t.Fatalf("rows = %q", got)
	}
	if rows[0].Bytes != 94 || rows[0].Entries != 4 || !rows[0].Dir || rows[1].Dir {
		t.Fatalf("first rows = %+v", rows[:2])
	}
	// 나머지는 root 자신, c, empty다. 펼친 a 안의 항목은 a 줄에 이미 들어 있다.
	if other.Bytes != 14 || other.Entries != 2 {
		t.Fatalf("other = %+v", other)
	}
}

func TestDuSelectLeavesZeroSizedEntriesInTheRemainder(t *testing.T) {
	root := duTestDir("/dev", 0, duTestDir("pts", 0), duTestFile("null", 0))
	duTotal(root)
	rows, other := duSelect(root, 10)
	if len(rows) != 0 || other.Entries != 2 {
		t.Fatalf("rows = %+v, other = %+v", rows, other)
	}
}

func TestDuFilesKeepTheLargestAndTheTotalsStay(t *testing.T) {
	root := duTestDir("/data", 4)
	files := duFiles{limit: 2}
	for index, size := range []uint64{5, 50, 1, 70, 20, 0} {
		root.bytes += size
		root.entries++
		files.offer(root, string(rune('a'+index)), size)
	}
	duFinish(root, files.heap)
	if root.total != 150 || root.count != 7 {
		t.Fatalf("total %d count %d", root.total, root.count)
	}
	rows, other := duSelect(root, 5)
	if got := duRowPaths(rows); got != "d b" {
		t.Fatalf("rows = %q", got)
	}
	if other.Bytes != 30 || other.Entries != 4 {
		t.Fatalf("other = %+v", other)
	}
}

func TestFormatDuRowsIndentsMarksDirectoriesAndFits(t *testing.T) {
	rows := []duRow{
		{Path: "lib", Bytes: 3 << 30, Dir: true},
		{Path: "lib/firmware-with-a-long-name", Bytes: 600 << 20, Dir: true, Depth: 1},
		{Path: "lib/core", Bytes: 2 << 20, Depth: 1},
	}
	text := formatDuRows(rows, duOther{Bytes: 1 << 10, Entries: 7}, 0)
	want := "     3.0 GB  lib/\n   600.0 MB    firmware-with-a-long-name/\n     2.0 MB    core\n     1.0 KB  (7 other entries)\n"
	if text != want {
		t.Fatalf("text =\n%s", text)
	}
	for _, line := range strings.Split(strings.TrimRight(formatDuRows(rows, duOther{}, 24), "\n"), "\n") {
		if width := liveWidth(line); width > 24 {
			t.Errorf("line %q is %d columns", line, width)
		}
	}
}

func TestFormatDuMountsShowsSharedMountsWithoutASize(t *testing.T) {
	mounts := []duMount{
		{Path: "/data/bind", FSType: "xfs", Shared: true},
		{Path: "/data/disk", FSType: "ext4", UsedBytes: 2 << 30},
	}
	text := formatDuMounts(mounts, false, 0)
	if !strings.Contains(text, "/data/bind  xfs   -\n") || !strings.Contains(text, "/data/disk  ext4  2.0 GB used\n") {
		t.Fatalf("text =\n%s", text)
	}
	var many []duMount
	for range duMountRows + 3 {
		many = append(many, duMount{Path: "/m", FSType: "overlay", Shared: true})
	}
	if text := formatDuMounts(many, false, 0); !strings.Contains(text, "3 more, -v shows all") {
		t.Fatalf("text =\n%s", text)
	}
	if text := formatDuMounts(many, true, 0); strings.Contains(text, "more") {
		t.Fatalf("verbose text =\n%s", text)
	}
}

func TestFormatDuFilesystemComparesOnlyAWholeFileSystem(t *testing.T) {
	deleted := duDeletedOpen{Bytes: 3 << 20, Files: 2, Top: []duDeletedFile{{Path: "/var/log/app.log", Bytes: 3 << 20, PID: 42, Comm: "app"}}}
	whole := formatDuFilesystem(duFilesystem{Mount: "/var", FSType: "xfs", SizeBytes: 1 << 30, UsedBytes: 10 << 20, Whole: true}, deleted, 6<<20, 1<<20, 0)
	for _, want := range []string{
		"\nfile system  /var  xfs  10.0 MB used of 1.0 GB\n",
		"  counted        6.0 MB\n",
		"  deleted, open  3.0 MB in 2 files\n",
		"  not found      1.0 MB  files under a mount point, or file system metadata\n",
		"       3.0 MB  pid 42 app  /var/log/app.log\n",
	} {
		if !strings.Contains(whole, want) {
			t.Fatalf("missing %q in\n%s", want, whole)
		}
	}
	part := formatDuFilesystem(duFilesystem{Mount: "/", FSType: "xfs", SizeBytes: 1 << 30, UsedBytes: 10 << 20}, duDeletedOpen{}, 6<<20, 0, 0)
	if strings.Count(part, "\n") != 2 || strings.Contains(part, "counted") || strings.Contains(part, "deleted") {
		t.Fatalf("part =\n%s", part)
	}
	// 다른 사용자의 process를 읽지 못했으면 찾은 파일이 없어도 하한임을 보인다.
	for files, want := range map[uint64]string{
		0: "  deleted, open  ≥0 B  run as root to read every process\n",
		1: "  deleted, open  ≥1.0 MB in 1 files  run as root to read every process\n",
	} {
		partial := formatDuFilesystem(duFilesystem{Mount: "/", FSType: "xfs"}, duDeletedOpen{Bytes: files << 20, Files: files, Partial: true}, 0, 0, 0)
		if !strings.Contains(partial, want) {
			t.Fatalf("partial =\n%s", partial)
		}
	}
	bind := formatDuFilesystem(duFilesystem{Mount: "/usr", FSType: "xfs", Source: "/dev/sda3", Root: "/usr", SizeBytes: 1 << 30}, duDeletedOpen{}, 0, 0, 0)
	if !strings.HasPrefix(bind, "\nfile system  /usr (/dev/sda3[/usr])  xfs  0 B used of 1.0 GB\n") {
		t.Fatalf("bind =\n%s", bind)
	}
	if formatDuFilesystem(duFilesystem{}, deleted, 0, 0, 0) != "" {
		t.Fatal("unknown file system printed")
	}
}

func TestDuResultWarnsWhenDeletedFilesHoldTheSameShareAsInfo(t *testing.T) {
	scan := duScan{root: duTestDir("/data", 4, duTestFile("a", 10)), filesystem: duFilesystem{Mount: "/data", FSType: "xfs", SizeBytes: 1000}}
	duTotal(scan.root)
	options := duOptions{root: "/data", workers: 4, rows: 5}
	top := []duDeletedFile{{Path: "/data/app.log", Bytes: 100, PID: 42, Comm: "app"}}

	scan.deleted = duDeletedOpen{Bytes: 99, Files: 1, Top: top, Partial: true}
	if result := duResult(Result{StartedAt: time.Now()}, options, scan, time.Minute, false); result.Status != StatusPass || len(result.Warnings) != 0 {
		t.Fatalf("below the share: %s %q", result.Status, result.Warnings)
	}
	scan.deleted = duDeletedOpen{Bytes: 100, Files: 1, Top: top}
	result := duResult(Result{StartedAt: time.Now()}, options, scan, time.Minute, false)
	if result.Status != StatusWarn || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "app (pid 42)") {
		t.Fatalf("at the share: %s %q", result.Status, result.Warnings)
	}
	if _, ok := result.Metrics["not_found_bytes"]; ok {
		t.Fatal("not_found_bytes without a whole file system")
	}
}
