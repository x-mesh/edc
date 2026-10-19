package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
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

// 지워졌지만 열린 파일을 찾지 않은 실행은 JSON에서 deleted_open을 빼서 0건과 구분한다. macOS가 그렇다(#266).
func TestDuResultLeavesOutDeletedOpenThatWasNotChecked(t *testing.T) {
	scan := duScan{root: duTestDir("/data", 4, duTestFile("a", 10))}
	duTotal(scan.root)
	options := duOptions{root: "/data", workers: 4, rows: 5}
	if _, ok := duResult(Result{StartedAt: time.Now()}, options, scan, time.Minute, false).Metrics["deleted_open"]; ok {
		t.Fatal("deleted_open without a check")
	}
	scan.deletedChecked = true
	if _, ok := duResult(Result{StartedAt: time.Now()}, options, scan, time.Minute, false).Metrics["deleted_open"]; !ok {
		t.Fatal("no deleted_open after a check")
	}
}

// 부분 결과의 경고는 긴 트리 위에 있어 화면 밖으로 밀린다. 파일 시스템 사용량 아래, 출력 끝에서도 알린다.
func TestDuPrintEndsAPartialResultWithTheNotice(t *testing.T) {
	scan := duScan{root: duTestDir("/data", 4, duTestFile("a", 10)), partial: true, filesystem: duFilesystem{Mount: "/data", FSType: "xfs", UsedBytes: 900, SizeBytes: 1000}}
	duTotal(scan.root)
	options := duOptions{root: "/data", workers: 4, rows: 5}
	print := func(stopped bool) []string {
		var output strings.Builder
		duPrint(&output, commonOptions{timeout: time.Minute}, duResult(Result{StartedAt: time.Now()}, options, scan, time.Minute, stopped), false, false, 0)
		return strings.Split(strings.TrimRight(output.String(), "\n"), "\n")
	}
	lines := print(false)
	if last := lines[len(lines)-2:]; !strings.HasPrefix(last[0], "PARTIAL: edc stopped at the 1m0s time limit") || last[1] != "  to count the whole tree: edc du --timeout 4m0s /data" {
		t.Fatalf("time limit: %q", last)
	}
	lines = print(true)
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "PARTIAL: you stopped edc") {
		t.Fatalf("stopped: %q", last)
	}
	scan.partial = false
	if lines := print(false); strings.Contains(strings.Join(lines, "\n"), "PARTIAL") {
		t.Fatalf("whole scan: %q", lines)
	}
}

func TestDuLargestKeepsTheBiggestDirectoriesBelowTheRoot(t *testing.T) {
	root := duTestDir("/data", 4,
		duTestDir("a", 4, duTestFile("big", 60), duTestDir("nested", 4, duTestFile("x", 26))),
		duTestDir("b", 50),
		duTestFile("c", 10),
	)
	duTotal(root)
	all := duLargest(root, 10)
	if !all.Complete || len(all.Dirs) != 3 || all.Dirs[0] != (duLargeDir{Path: "a", Bytes: 94, Entries: 4}) || all.Dirs[2].Path != "a/nested" {
		t.Fatalf("all = %+v", all)
	}
	top := duLargest(root, 2)
	if top.Complete || len(top.Dirs) != 2 || top.Dirs[1].Path != "b" {
		t.Fatalf("top = %+v", top)
	}
}

func duTestDirMetrics(path string, complete bool, dirs ...duLargeDir) map[string]interface{} {
	return map[string]interface{}{"path": path, "partial": false, "largest_dirs": duLargestList{Complete: complete, Dirs: dirs}}
}

func TestDuCompareDirsShowsTheDeepestGrowthAndBounds(t *testing.T) {
	before := duTestDirMetrics("/var", false,
		duLargeDir{Path: "lib", Bytes: 300}, duLargeDir{Path: "lib/docker", Bytes: 250}, duLargeDir{Path: "lib/docker/overlay2", Bytes: 200},
		duLargeDir{Path: "log", Bytes: 100}, duLargeDir{Path: "cache", Bytes: 50})
	after := duTestDirMetrics("/var", false,
		duLargeDir{Path: "lib", Bytes: 340}, duLargeDir{Path: "lib/docker", Bytes: 290}, duLargeDir{Path: "lib/docker/overlay2", Bytes: 240},
		duLargeDir{Path: "log", Bytes: 102}, duLargeDir{Path: "new", Bytes: 80})
	deltas, note := duCompareDirs(before, after)
	if note != "" {
		t.Fatalf("note %q", note)
	}
	// lib와 lib/docker는 overlay2가 증가를 모두 설명하므로 빠진다. cache는 after 목록의 하한(80)보다 작았을 수 있어
	// 줄었는지 알 수 없다. new는 before 목록에 없어 before가 50 이하다.
	want := []duDirDelta{
		{Path: "lib/docker/overlay2", Before: 200, After: 240, Delta: 40},
		{Path: "new", Before: 50, After: 80, Delta: 30, Bound: "before"},
		{Path: "log", Before: 100, After: 102, Delta: 2},
	}
	if fmt.Sprint(deltas) != fmt.Sprint(want) {
		t.Fatalf("deltas = %+v", deltas)
	}
	lines := formatDuDirDeltas(deltas)
	if lines[0] != "     +40 B  lib/docker/overlay2  200 B → 240 B" || lines[1] != "     +30 B  new                  ≤50 B → 80 B" {
		t.Fatalf("lines = %q", lines)
	}
}

func TestDuCompareDirsWithCompleteListsCountsMissingAsZero(t *testing.T) {
	before := duTestDirMetrics("/srv", true, duLargeDir{Path: "old", Bytes: 70}, duLargeDir{Path: "keep", Bytes: 10})
	after := duTestDirMetrics("/srv", true, duLargeDir{Path: "keep", Bytes: 10})
	deltas, _ := duCompareDirs(before, after)
	if len(deltas) != 1 || deltas[0] != (duDirDelta{Path: "old", Before: 70, After: 0, Delta: -70}) {
		t.Fatalf("deltas = %+v", deltas)
	}
	if lines := formatDuDirDeltas(deltas); lines[0] != "     -70 B  old  70 B → 0 B" {
		t.Fatalf("lines = %q", lines)
	}
}

func TestDuCompareDirsSaysWhyItDidNotCompare(t *testing.T) {
	var_ := duTestDirMetrics("/var", true, duLargeDir{Path: "a", Bytes: 1})
	usr := duTestDirMetrics("/usr", true, duLargeDir{Path: "a", Bytes: 2})
	if deltas, note := duCompareDirs(var_, usr); deltas != nil || note != "directories not compared, the paths differ: /var → /usr" {
		t.Fatalf("deltas %v note %q", deltas, note)
	}
	partial := duTestDirMetrics("/var", true, duLargeDir{Path: "a", Bytes: 2})
	partial["partial"] = true
	if deltas, note := duCompareDirs(var_, partial); deltas != nil || !strings.Contains(note, "stopped before the end") {
		t.Fatalf("deltas %v note %q", deltas, note)
	}
	// 3단계까지의 report에는 목록이 없다. 이유 없이 scalar 차이만 남긴다.
	if deltas, note := duCompareDirs(map[string]interface{}{"path": "/var"}, var_); deltas != nil || note != "" {
		t.Fatalf("deltas %v note %q", deltas, note)
	}
}

func TestDuCompareDirsKeepsTheContainerLabel(t *testing.T) {
	before := duTestDirMetrics("/var/lib/docker", true, duLargeDir{Path: "containers/aaaa", Bytes: 10, Label: "container api-1"})
	after := duTestDirMetrics("/var/lib/docker", true, duLargeDir{Path: "containers/aaaa", Bytes: 90, Label: "container api-1"})
	deltas, _ := duCompareDirs(before, after)
	if len(deltas) != 1 || deltas[0].Label != "container api-1" {
		t.Fatalf("deltas = %+v", deltas)
	}
	if line := formatDuDirDeltas(deltas)[0]; !strings.HasSuffix(line, "10 B → 90 B  container api-1") {
		t.Fatalf("line = %q", line)
	}
}

func TestDuLargestMatchesFullSortWithTiesAndLimits(t *testing.T) {
	random := rand.New(rand.NewSource(19))
	root := duTestDir("/data", 0)
	for _, index := range random.Perm(1200) {
		child := duTestDir(fmt.Sprintf("dir-%04d", index), uint64(index%13))
		if index%7 == 0 {
			child.children = append(child.children, duTestDir("nested", uint64(index%5)))
		}
		root.children = append(root.children, child)
	}
	duTotal(root)
	var expected []duLargeDir
	var collect func(*duNode, string)
	collect = func(node *duNode, prefix string) {
		for _, child := range node.children {
			if child.dir {
				path := prefix + child.name
				expected = append(expected, duLargeDir{Path: path, Bytes: child.total, Entries: child.count})
				collect(child, path+"/")
			}
		}
	}
	collect(root, "")
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Bytes != expected[j].Bytes {
			return expected[i].Bytes > expected[j].Bytes
		}
		return expected[i].Path < expected[j].Path
	})
	for _, limit := range []int{0, 1, 2, 200, len(expected), len(expected) + 1} {
		result := duLargest(root, limit)
		wanted := expected[:min(limit, len(expected))]
		if result.Complete != (len(expected) <= limit) || !reflect.DeepEqual(result.Dirs, wanted) {
			t.Fatalf("limit %d: result differs from full sort", limit)
		}
	}
	if result := duLargest(duTestDir("/empty", 0), 200); !result.Complete || result.Dirs != nil {
		t.Fatalf("empty=%+v", result)
	}
}

func BenchmarkDuLargestManyDirectories(b *testing.B) {
	root := duTestDir("/data", 0)
	for index := range 10000 {
		root.children = append(root.children, duTestDir(fmt.Sprintf("dir-%05d", index), uint64(index%17)))
	}
	duTotal(root)
	b.ReportAllocs()
	for b.Loop() {
		duLargest(root, duLargestDirs)
	}
}

// 색 터미널의 트리는 평문과 같은 글자에 크기 색과, -B일 때 가장 큰 줄에 맞춘 차분한 막대를 더한다.
func TestRichDuRowsColorSizesAndScaleBarsToTheLargestEntry(t *testing.T) {
	rows := []duRow{{Path: "lib", Bytes: 75 << 30, Dir: true, Label: "container api"}, {Path: "lib/file", Bytes: 25, Depth: 1}}
	other := duOther{Bytes: 25 << 20, Entries: 2}
	text := richDuRows(rows, other, 0, true)
	for _, want := range []string{
		richPaint("  75.0 GB", richRed, true) + "  " + richFill(richBar(1, 1, duBarWidth), richRed) + "  lib/  " + richDim("container api"),
		richPaint("     25 B", richGreen, false) + "  " + richFill(richBar(25, 75<<30, duBarWidth), richGreen) + "    file",
		richDim("  25.0 MB") + "  " + richFill(richBar(25<<20, 75<<30, duBarWidth), richGray) + "  " + richDim("(2 other entries)"),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%q", want, text)
		}
	}
	if strings.Contains(text, "░") {
		t.Fatal("the bar keeps the plain track")
	}
	for _, line := range strings.Split(strings.TrimRight(richDuRows(rows, duOther{}, 34, true), "\n"), "\n") {
		if liveWidth(line) > 34 {
			t.Fatalf("wide graph row: %q", line)
		}
	}
	// 막대가 없으면 평문과 같은 글자다. 막대를 둘 줄이 하나뿐이면 늘 가득 차므로 뺀다.
	if got := ansi.Strip(richDuRows(rows, other, 0, false)); got != formatDuRows(rows, other, 0) {
		t.Fatalf("stripped rows =\n%s", got)
	}
	if text := richDuRows([]duRow{{Path: "only", Bytes: 10, Dir: true}}, duOther{}, 0, true); strings.Contains(text, "█") {
		t.Fatalf("a single row keeps its bar: %q", text)
	}
}

func TestRichDuMountsAndFilesystemKeepThePlainText(t *testing.T) {
	var mounts []duMount
	for range duMountRows + 3 {
		mounts = append(mounts, duMount{Path: "/data/disk", FSType: "ext4", UsedBytes: 2 << 30, Label: "mounted in db"})
	}
	text := richDuMounts(mounts, false, 0)
	for _, want := range []string{
		richPanelTop("skipped mounts", "", ""),
		richPaint("2.0 GB", richYellow, false) + richDim(" used"),
		richPaint("-v", richCyan, true),
		richPanelBottom(""),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%q", want, text)
		}
	}
	if stripped := ansi.Strip(text); !strings.Contains(stripped, "│ /data/disk  ext4  2.0 GB used  mounted in db\n") || !strings.Contains(stripped, "│ 3 more, -v shows all\n") {
		t.Fatalf("stripped mounts =\n%s", stripped)
	}
	deleted := duDeletedOpen{Bytes: 3 << 20, Files: 2, Top: []duDeletedFile{{Path: "/var/log/app.log", Bytes: 3 << 20, PID: 42, Comm: "app"}}}
	filesystem := richDuFilesystem(duFilesystem{Mount: "/var", FSType: "xfs", SizeBytes: 1 << 30, UsedBytes: 10 << 20, Whole: true}, deleted, 6<<20, 1<<20, 0)
	if !strings.HasPrefix(filesystem, "\n"+richChip("file system")+" "+richBold("/var")+"  "+richDim("xfs")+"  "+richBold("10.0 MB")+richDim(" used of ")+"1.0 GB\n") {
		t.Fatalf("file system title = %q", filesystem)
	}
	stripped := ansi.Strip(filesystem)
	for _, want := range []string{
		"  counted        6.0 MB\n",
		"  deleted, open  3.0 MB in 2 files\n",
		"  not found      1.0 MB  files under a mount point, or file system metadata\n",
		"       3.0 MB  pid 42 app  /var/log/app.log\n",
	} {
		if !strings.Contains(stripped, want) {
			t.Fatalf("missing %q in\n%s", want, stripped)
		}
	}
	if richDuFilesystem(duFilesystem{}, deleted, 0, 0, 0) != "" {
		t.Fatal("unknown file system printed")
	}
}

// 색 터미널에서는 PARTIAL 머리를 노란 배지로, 다시 실행할 명령을 굵은 청록으로 칠한다. 줄은 터미널 폭을 넘지 않는다.
func TestDuPartialNoticeIsAYellowBadgeOnColorTerminals(t *testing.T) {
	result := Result{Next: "edc du --timeout 4m0s /data", Metrics: map[string]interface{}{"partial": true, "bytes": uint64(10)}}
	text := formatDuPartial(result, time.Minute, true, 60)
	if !strings.HasPrefix(text, "\n"+richBadge("PARTIAL", richYellow)+" edc stopped at the 1m0s time limit.") {
		t.Fatalf("badge line = %q", text)
	}
	if !strings.Contains(text, richDim("to count the whole tree: ")+richPaint("edc du --timeout 4m0s /data", richCyan, true)) {
		t.Fatalf("next line = %q", text)
	}
	if strings.Contains(text, "\033[1;33m") {
		t.Fatalf("raw escape in %q", text)
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if liveWidth(line) > 60 {
			t.Fatalf("wide line %q", line)
		}
	}
	// 줄을 바꾼 본문은 배지 뒤에서 이어진다.
	if lines := strings.Split(ansi.Strip(text), "\n"); !strings.HasPrefix(lines[2], strings.Repeat(" ", liveWidth(richBadge("PARTIAL", richYellow))+1)) {
		t.Fatalf("wrapped body = %q", lines)
	}
}

// 좁은 터미널에서도 다시 실행할 명령은 한 줄에 그대로 남는다. 복사하면 그대로 실행되어야 한다.
func TestDuPartialKeepsTheRerunCommandWhole(t *testing.T) {
	command := "edc du --timeout 4m0s /very/long/path/to/the/data/directory"
	result := Result{Next: command, Metrics: map[string]interface{}{"partial": true, "bytes": uint64(10)}}
	text := formatDuPartial(result, time.Minute, true, 40)
	whole := false
	for _, line := range strings.Split(ansi.Strip(text), "\n") {
		if strings.Contains(line, command) {
			whole = true
		}
	}
	if !whole || !strings.Contains(text, richPaint(command, richCyan, true)) {
		t.Fatalf("the command is broken in %q", ansi.Strip(text))
	}
	if !strings.Contains(text, "  "+richDim("to count the whole tree:")+"\n") {
		t.Fatalf("the hint lost its label: %q", text)
	}
}

func TestDuMeasureLeavesHostWideSourcesAlone(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "file"), make([]byte, 32<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	sources, ok := hostDuSources()
	if !ok {
		t.Skip("du is not supported on this platform")
	}
	var deletedCalls, labelCalls int
	sources.deletedOpen = func() (map[uint64]*deletedOpenSummary, bool) {
		deletedCalls++
		return nil, false
	}
	sources.containerLabels = func(context.Context) map[string]string {
		labelCalls++
		return nil
	}
	options := duOptions{root: root, workers: duDefaultWorkers}
	walked, err := duWalk(context.Background(), options, sources)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := 1
	if runtime.GOOS != "linux" {
		wantCalls = 0
	}
	if deletedCalls != wantCalls || labelCalls != wantCalls {
		t.Fatalf("duWalk calls: deletedOpen %d, containerLabels %d, want %d each", deletedCalls, labelCalls, wantCalls)
	}
	deletedCalls, labelCalls = 0, 0
	measured, err := duMeasure(context.Background(), options, sources)
	if err != nil {
		t.Fatal(err)
	}
	if deletedCalls != 0 || labelCalls != 0 {
		t.Fatalf("duMeasure calls: deletedOpen %d, containerLabels %d, want 0", deletedCalls, labelCalls)
	}
	if measured.root.total != walked.root.total || measured.root.count != walked.root.count {
		t.Fatalf("duMeasure total %d count %d, duWalk total %d count %d",
			measured.root.total, measured.root.count, walked.root.total, walked.root.count)
	}
	if measured.filesystem.Mount != walked.filesystem.Mount {
		t.Fatalf("duMeasure filesystem %q, duWalk %q", measured.filesystem.Mount, walked.filesystem.Mount)
	}
}

func TestDuWorkersShortFlag(t *testing.T) {
	sources, ok := hostDuSources()
	if !ok {
		t.Skip("du is not supported on this platform")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-w", "--workers"} {
		jsonPath := filepath.Join(t.TempDir(), "out.json")
		stdout, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		if code := runDuWith([]string{flag, "3", "--json", jsonPath, root}, "test", sources, ok, stdout); code != 0 {
			t.Fatalf("%s: exit %d", flag, code)
		}
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			t.Fatal(err)
		}
		var report interface{}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if got, found := duFindNumber(report, "workers"); !found || got != 3 {
			t.Errorf("%s: workers = %v, found %v", flag, got, found)
		}
	}
}

func duFindNumber(value interface{}, key string) (float64, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if number, ok := typed[key].(float64); ok {
			return number, true
		}
		for _, child := range typed {
			if number, ok := duFindNumber(child, key); ok {
				return number, true
			}
		}
	case []interface{}:
		for _, child := range typed {
			if number, ok := duFindNumber(child, key); ok {
				return number, true
			}
		}
	}
	return 0, false
}
