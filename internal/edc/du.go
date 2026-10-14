package edc

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// du는 디렉터리 아래의 디스크 사용량을 읽기만 해서 보인다. du처럼 할당된 block을 세고 hard link는 한 번만 센다.
// 기본으로 한 파일 시스템 안에서만 세고, 그 아래의 마운트는 건너뛴 지점으로 알린다.

const (
	duProbeID = "du.scan"
	// 캐시가 빈 회전 디스크에서는 큰 트리를 읽는 데 오래 걸린다. 시간이 다 되면 그때까지 센 값을 partial로 보인다.
	duDefaultTimeout = 60 * time.Second
	duPipedRows      = 20
	// duMountRows는 터미널에 보이는 건너뛴 마운트 수다. container가 많은 호스트는 overlay 마운트가 수십 개다.
	duMountRows = 10
	// duUnreadableKeep은 읽지 못한 경로를 담는 상한이다. 개수는 모두 센다.
	duUnreadableKeep = 1000
	// duLargestDirs는 report diff가 두 시점을 맞춰 보도록 JSON에 남기는 큰 디렉터리 수다. 화면용 rows는 고른
	// 줄이라 두 시점의 줄이 서로 맞지 않는다.
	duLargestDirs = 200
	// duDiffDirs는 report diff가 보여 주는 디렉터리 변화 수다.
	duDiffDirs = 10
	// duBrowseFiles는 탐색 화면을 열 때 남기는 큰 파일 수다. 화면은 디렉터리마다 그 안의 큰 파일을 보여야 하므로
	// 출력 줄 수보다 많이 남긴다. 나머지 파일은 디렉터리마다 한 줄로 합친다.
	duBrowseFiles = 10000
)

type duOptions struct {
	root        string
	workers     int
	rows        int
	crossMounts bool
	// keepFiles는 남길 큰 파일 수다. rows보다 작으면 rows를 쓴다.
	keepFiles int
	// progress는 읽은 디렉터리 수를 탐색 중에 바깥에서 읽게 한다. nil이면 탐색기가 따로 센다.
	progress *atomic.Int64
}

// duNode는 디렉터리 하나, 또는 화면에 오를 수 있는 큰 파일 하나다. bytes와 entries는 자기 자신과
// children으로 따로 두지 않은 파일의 합이다. total과 count는 탐색이 끝난 뒤 하위 전체를 더한 값이다.
type duNode struct {
	name     string
	parent   *duNode
	dir      bool
	bytes    uint64
	entries  int64
	children []*duNode
	total    uint64
	count    int64
	// own은 디렉터리 자신의 block이다. bytes에서 빼면 children으로 따로 두지 않은 파일의 합이 남는다.
	own        uint64
	unreadable bool
}

type duMount struct {
	Path      string `json:"path"`
	FSType    string `json:"fstype"`
	UsedBytes uint64 `json:"used_bytes,omitempty"`
	// Shared는 statfs가 이 마운트가 아니라 밑에 있는 파일 시스템 전체를 답하는 경우다. bind mount와 overlay가 그렇다.
	// 그 값을 마운트의 크기처럼 보이면 같은 숫자가 마운트마다 되풀이된다.
	Shared bool `json:"shared,omitempty"`
	// Label은 이 경로를 쓰는 container다. docker가 있을 때만 붙는다.
	Label string `json:"label,omitempty"`
}

type duScan struct {
	root            *duNode
	directories     int64
	mounts          []duMount
	unreadable      []string
	unreadableCount int64
	partial         bool
	warnings        []string
	filesystem      duFilesystem
	deleted         duDeletedOpen
	// deletedChecked는 지워졌지만 열린 파일을 찾았다는 뜻이다. Linux는 경로의 파일 시스템을 찾았을 때만 찾고, macOS는
	// 찾지 않는다. 찾지 않았으면 JSON에서 deleted_open을 빼서 0건과 구분한다.
	deletedChecked bool
	// notFound는 filesystem.Whole일 때만 뜻이 있다. df의 사용량에서 센 크기와 지워졌지만 열린 파일을 뺀 값이다.
	notFound uint64
	// labels는 절대 경로마다 그 경로를 쓰는 container다. docker가 없으면 비어 있다.
	labels map[string]string
	// elapsed는 탐색에 걸린 시간이다. 화면을 닫은 뒤 결과를 만들므로 화면을 본 시간을 넣지 않으려고 따로 잰다.
	elapsed time.Duration
}

type duUsage struct{ Size, Used uint64 }

// duFilesystem은 경로가 속한 파일 시스템이다. 크기와 사용량은 df와 같이 statfs에서 읽는다.
type duFilesystem struct {
	Mount  string `json:"mount"`
	FSType string `json:"fstype"`
	// Source와 Root는 mountinfo의 장치와, 그 장치에서 이 마운트에 붙인 디렉터리다. Root가 /가 아니면 bind mount라
	// 크기와 사용량이 마운트가 아니라 장치 전체의 값이다.
	Source    string `json:"source"`
	Root      string `json:"root"`
	SizeBytes uint64 `json:"size_bytes"`
	UsedBytes uint64 `json:"used_bytes"`
	// Whole은 경로가 파일 시스템의 root를 붙인 마운트 지점이고 모두 읽었다는 뜻이다. 그때만 사용량과 센 크기의
	// 차이가 "찾지 못한 공간"이 된다. 하위 디렉터리나 bind mount를 재면 차이는 당연히 크다.
	Whole bool `json:"whole"`
}

// duDeletedOpen은 같은 파일 시스템에서 지워졌지만 process가 아직 열고 있는 파일이다. 경로 아래만이 아니라
// 파일 시스템 전체의 값이다. Partial이면 다른 사용자의 process를 읽지 못해 값이 하한이다.
type duDeletedOpen struct {
	Bytes   uint64          `json:"bytes"`
	Files   uint64          `json:"files"`
	Partial bool            `json:"partial"`
	Top     []duDeletedFile `json:"top,omitempty"`
}

type duDeletedFile struct {
	Path  string `json:"path"`
	Bytes uint64 `json:"bytes"`
	PID   int    `json:"pid"`
	Comm  string `json:"comm"`
}

type duRow struct {
	Path    string `json:"path"`
	Bytes   uint64 `json:"bytes"`
	Entries int64  `json:"entries"`
	Dir     bool   `json:"dir"`
	Depth   int    `json:"depth"`
	Label   string `json:"label,omitempty"`
}

type duOther struct {
	Bytes   uint64 `json:"bytes"`
	Entries int64  `json:"entries"`
}

// duSources는 탐색이 읽는 호스트 원천이다. 테스트는 마운트 표를 가짜로 넣어 실제 마운트를 만들지 않는다.
type duSources struct {
	mountinfo func() (string, error)
	statfs    func(path string) (duUsage, bool)
	// deletedOpen의 bool은 다른 사용자의 process를 읽지 못해 값이 하한이라는 뜻이다.
	deletedOpen func() (map[uint64]*deletedOpenSummary, bool)
	// containerLabels는 docker 저장 경로의 이름표다. nil이면 이름표를 붙이지 않는다.
	containerLabels func(ctx context.Context) map[string]string
}

func runDu(args []string, version string) int {
	sources, ok := hostDuSources()
	return runDuWith(args, version, sources, ok, os.Stdout)
}

func runDuWith(args []string, version string, sources duSources, ok bool, stdout *os.File) int {
	options := configuredCommon(duDefaultTimeout)
	set := flag.NewFlagSet("du", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	workers := set.Int("workers", duDefaultWorkers, T("command.du.option.workers"))
	rows := set.Int("top", 0, T("command.du.option.top"))
	crossMounts := set.Bool("cross-mounts", false, T("command.du.option.cross_mounts"))
	noBrowse := set.Bool("no-browse", false, T("command.du.option.no_browse"))
	set.BoolVar(noBrowse, "B", false, T("command.du.option.no_browse"))
	ordered, err := reorderFSWatchArgs(set, args)
	if err == nil {
		err = set.Parse(ordered)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	if set.NArg() > 1 || *workers < 1 || *rows < 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc du [options] [path]"))
		return 2
	}
	path := "."
	if set.NArg() == 1 {
		path = set.Arg(0)
	}
	started := time.Now()
	target := map[string]interface{}{"path": path}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(duProbeID, T("du.skip.unsupported"))}, options.redact))
	}
	terminal := options.jsonPath == "" && isTerminal(stdout)
	// svc, ctr과 같이 사람이 보는 터미널에서만 화면을 연다. pipe, --json, NO_COLOR는 그대로 출력한다.
	live := terminal && liveTerminal()
	browse := live && !*noBrowse
	scanOptions := duOptions{workers: *workers, rows: *rows, crossMounts: *crossMounts}
	if scanOptions.rows == 0 {
		scanOptions.rows = duDefaultRows(stdout, terminal)
	}
	if browse {
		scanOptions.keepFiles = duBrowseFiles
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	result := Result{Probe: duProbeID, StartedAt: started.UTC()}
	// 넘긴 경로가 symlink면 링크 자체가 아니라 가리키는 디렉터리를 잰다. 링크 하나의 크기는 쓸모가 없다.
	scanOptions.root, err = duResolveRoot(path)
	var scan duScan
	if err == nil && live {
		scan, err = duBrowse(ctx, cancel, scanOptions, sources, started, *noBrowse)
	} else if err == nil {
		scan, err = duWalk(ctx, scanOptions, sources)
	}
	if err != nil {
		result = duPathFailure(result, path, err)
	} else {
		// 화면에서 q로 멈추면 base context가 취소된다. 시간 초과와 구분해 다음 명령을 권하지 않는다.
		stopped := scan.partial && errors.Is(ctx.Err(), context.Canceled)
		result = duResult(result, scanOptions, scan, options.timeout, stopped)
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, target, []Result{result}, options.redact))
	}
	// pipe로 넘기는 출력은 자르지 않는다. 받는 쪽이 경로 전체를 읽어야 한다.
	width := 0
	if terminal {
		width = terminalWidth()
	}
	return duPrint(stdout, options, result, terminal && os.Getenv("NO_COLOR") == "", live && *noBrowse, width)
}

func duResolveRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// duDefaultRows는 결과 줄, 나머지 줄, 건너뛴 마운트 몇 줄, 파일 시스템 줄이 화면에 함께 들어가게 남긴다.
func duDefaultRows(stdout *os.File, terminal bool) int {
	if !terminal {
		return duPipedRows
	}
	_, height, err := term.GetSize(int(stdout.Fd()))
	if err != nil || height <= 0 {
		return duPipedRows
	}
	return max(5, height-12)
}

func duResult(result Result, options duOptions, scan duScan, timeout time.Duration, stopped bool) Result {
	root := scan.root
	rows, other := duSelect(root, options.rows)
	elapsed := scan.elapsed.Round(time.Millisecond)
	result.Status = StatusPass
	result.Summary = T("du.summary", formatSizeShort(root.total), options.root, root.count, elapsed, options.workers)
	result.Warnings = append(result.Warnings, scan.warnings...)
	if scan.partial && stopped {
		result.Status = StatusWarn
		result.Summary = T("du.summary_stopped", formatSizeShort(root.total), options.root, root.count)
		result.Warnings = append(result.Warnings, T("du.warning.stopped"))
	} else if scan.partial {
		result.Status = StatusWarn
		result.Summary = T("du.summary_partial", formatSizeShort(root.total), options.root, root.count, timeout)
		result.Warnings = append(result.Warnings, T("du.warning.partial"))
		next := 4 * timeout
		if next < duDefaultTimeout {
			next = duDefaultTimeout
		}
		result.Next = fmt.Sprintf("edc du --timeout %s %s", next, quoteLocalShellWord(options.root))
	}
	if scan.unreadableCount > 0 {
		result.Status = StatusWarn
		if os.Geteuid() != 0 {
			result.Warnings = append(result.Warnings, T("du.warning.unreadable_root", scan.unreadableCount))
		} else {
			result.Warnings = append(result.Warnings, T("du.warning.unreadable", scan.unreadableCount))
		}
		result.Evidence = append(result.Evidence, Evidence{Label: T("du.evidence.unreadable"), Value: strings.Join(scan.unreadable, "\n")})
	}
	// info.disk와 snapshot과 같은 기준이다. 읽지 못한 fd가 있어 하한이어도 그것만으로는 warn하지 않는다.
	// root가 아니면 늘 하한이라 모든 실행이 warn이 된다.
	filesystem, deleted := scan.filesystem, scan.deleted
	if filesystem.SizeBytes > 0 && len(deleted.Top) > 0 && deleted.Bytes*100 >= filesystem.SizeBytes*infoDeletedOpenWarnPercent {
		result.Status = StatusWarn
		top := deleted.Top[0]
		result.Warnings = append(result.Warnings, T("du.warning.deleted_open", formatSizeShort(deleted.Bytes), filesystem.Mount, top.Comm, top.PID))
	}
	largest := duLargest(root, duLargestDirs)
	if len(scan.labels) > 0 {
		for index := range rows {
			rows[index].Label = scan.labels[filepath.Join(options.root, rows[index].Path)]
		}
		for index := range scan.mounts {
			scan.mounts[index].Label = scan.labels[scan.mounts[index].Path]
		}
		for index := range largest.Dirs {
			largest.Dirs[index].Label = scan.labels[filepath.Join(options.root, largest.Dirs[index].Path)]
		}
	}
	result.Metrics = map[string]interface{}{
		"path": options.root, "bytes": root.total, "entries": root.count, "directories": scan.directories,
		"workers": options.workers, "partial": scan.partial, "unreadable": scan.unreadableCount,
		"rows": rows, "other": other, "skipped_mounts": scan.mounts,
		"filesystem": filesystem, "largest_dirs": largest,
	}
	if scan.deletedChecked {
		result.Metrics["deleted_open"] = deleted
	}
	if filesystem.Whole {
		result.Metrics["not_found_bytes"] = scan.notFound
	}
	return result
}

func duPrint(stdout io.Writer, options commonOptions, result Result, color, graph bool, width int) int {
	var output io.Writer = stdout
	var buffer strings.Builder
	if options.redact {
		output = &buffer
	}
	fmt.Fprint(output, formatResultLine(result, color))
	printResultDetail(output, result, options.verbose, color)
	if result.Status != StatusFail {
		rows, _ := result.Metrics["rows"].([]duRow)
		other, _ := result.Metrics["other"].(duOther)
		counted, _ := result.Metrics["bytes"].(uint64)
		fmt.Fprint(output, "\n"+formatDuRows(rows, other, width, graph, counted))
		mounts, _ := result.Metrics["skipped_mounts"].([]duMount)
		fmt.Fprint(output, formatDuMounts(mounts, options.verbose, width))
		filesystem, _ := result.Metrics["filesystem"].(duFilesystem)
		deleted, _ := result.Metrics["deleted_open"].(duDeletedOpen)
		notFound, _ := result.Metrics["not_found_bytes"].(uint64)
		fmt.Fprint(output, formatDuFilesystem(filesystem, deleted, counted, notFound, width))
	}
	if options.redact {
		fmt.Fprint(stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode([]Result{result})
}

// duSelect는 큰 항목부터 골라 펼친다. 디렉터리를 고르면 그 안의 항목이 후보에 오른다. 조상은 언제나 자손보다
// 먼저 뽑히므로, 보이는 파일보다 큰 파일은 모두 보인다. 그래서 탐색은 전체에서 큰 파일 limit개만 남기면 된다.
// 크기가 0인 항목은 /dev처럼 줄만 차지하므로 나머지 줄에 넣는다.
func duSelect(root *duNode, limit int) ([]duRow, duOther) {
	chosen := map[*duNode]bool{}
	candidates := &duHeap{}
	push := func(node *duNode) {
		for _, child := range node.children {
			if child.total > 0 {
				heap.Push(candidates, child)
			}
		}
	}
	push(root)
	for candidates.Len() > 0 && len(chosen) < limit {
		node := heap.Pop(candidates).(*duNode)
		chosen[node] = true
		push(node)
	}
	var rows []duRow
	other := duOther{Bytes: root.total, Entries: root.count - 1}
	var visit func(node *duNode, prefix string, depth int)
	visit = func(node *duNode, prefix string, depth int) {
		children := append([]*duNode(nil), node.children...)
		sort.Slice(children, func(i, j int) bool { return duLarger(children[i], children[j]) })
		for _, child := range children {
			if !chosen[child] {
				continue
			}
			path := prefix + child.name
			rows = append(rows, duRow{Path: path, Bytes: child.total, Entries: child.count, Dir: child.dir, Depth: depth})
			if depth == 0 {
				other.Bytes -= child.total
				other.Entries -= child.count
			}
			visit(child, path+"/", depth+1)
		}
	}
	visit(root, "", 0)
	return rows, other
}

func duLarger(left, right *duNode) bool {
	if left.total != right.total {
		return left.total > right.total
	}
	return left.name < right.name
}

type duHeap []*duNode

func (h duHeap) Len() int           { return len(h) }
func (h duHeap) Less(i, j int) bool { return duLarger(h[i], h[j]) }
func (h duHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *duHeap) Push(value any)    { *h = append(*h, value.(*duNode)) }
func (h *duHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// duFit는 width가 0이면 자르지 않는다.
func duFit(text string, width int) string {
	if width <= 0 {
		return text
	}
	return ansi.Truncate(text, width, "…")
}

func formatDuRows(rows []duRow, other duOther, width int, graph bool, total uint64) string {
	var builder strings.Builder
	line := func(bytes uint64, label string) {
		if graph {
			label = duBar(bytes, total) + "  " + label
		}
		builder.WriteString(duFit(fmt.Sprintf("  %9s  %s", formatSizeShort(bytes), label), width))
		builder.WriteString("\n")
	}
	for _, row := range rows {
		name := row.Path[strings.LastIndex(row.Path, "/")+1:]
		if row.Dir {
			name += "/"
		}
		if row.Label != "" {
			name += "  " + row.Label
		}
		line(row.Bytes, strings.Repeat("  ", row.Depth)+name)
	}
	if other.Entries > 0 {
		line(other.Bytes, T("du.other", other.Entries))
	}
	return builder.String()
}

func formatDuMounts(mounts []duMount, verbose bool, width int) string {
	if len(mounts) == 0 {
		return ""
	}
	shown := mounts
	if !verbose && len(shown) > duMountRows {
		shown = shown[:duMountRows]
	}
	pathWidth, typeWidth := 0, 0
	for _, mount := range shown {
		pathWidth = max(pathWidth, liveWidth(mount.Path))
		typeWidth = max(typeWidth, liveWidth(mount.FSType))
	}
	var builder strings.Builder
	builder.WriteString("\n" + T("du.mounts.title") + "\n")
	for _, mount := range shown {
		used := "-"
		if !mount.Shared {
			used = T("du.mounts.used", formatSizeShort(mount.UsedBytes))
		}
		text := "  " + liveCell(mount.Path, pathWidth+2) + liveCell(mount.FSType, typeWidth+2) + used
		if mount.Label != "" {
			text += "  " + mount.Label
		}
		builder.WriteString(duFit(text, width) + "\n")
	}
	if hidden := len(mounts) - len(shown); hidden > 0 {
		builder.WriteString("  " + T("du.mounts.more", hidden) + "\n")
	}
	return builder.String()
}

// duMountLabel은 bind mount를 findmnt처럼 장치[디렉터리]로 적어 사용량이 마운트가 아니라 장치 전체의 값임을 보인다.
func duMountLabel(filesystem duFilesystem) string {
	if filesystem.Root != "" && filesystem.Root != "/" {
		return fmt.Sprintf("%s (%s[%s])", filesystem.Mount, filesystem.Source, filesystem.Root)
	}
	return filesystem.Mount
}

// formatDuFilesystem은 경로가 속한 파일 시스템을 보인다. 경로가 파일 시스템 전체일 때만 df의 사용량과 센 크기를
// 맞춰 보고, 하위 디렉터리를 쟀으면 지워졌지만 열린 파일이 있을 때만 그 줄을 보인다.
func formatDuFilesystem(filesystem duFilesystem, deleted duDeletedOpen, counted, notFound uint64, width int) string {
	if filesystem.Mount == "" {
		return ""
	}
	var builder strings.Builder
	title := T("du.fs.title", duMountLabel(filesystem), filesystem.FSType, formatSizeShort(filesystem.UsedBytes), formatSizeShort(filesystem.SizeBytes))
	builder.WriteString("\n" + duFit(title, width) + "\n")
	var labels, values []string
	if filesystem.Whole {
		labels, values = append(labels, T("du.fs.counted")), append(values, formatSizeShort(counted))
	}
	// 하나도 찾지 못했어도 하한이면 줄을 남긴다. 줄이 없으면 지워진 파일이 없다고 읽힌다.
	if filesystem.Whole || deleted.Files > 0 || deleted.Partial {
		value := formatSizeShort(deleted.Bytes)
		if deleted.Partial {
			value = "≥" + value
		}
		if deleted.Files > 0 {
			value += " " + T("du.fs.files", deleted.Files)
		}
		if deleted.Partial {
			value += "  " + T("du.fs.partial_hint")
		}
		labels, values = append(labels, T("du.fs.deleted")), append(values, value)
	}
	if filesystem.Whole {
		labels, values = append(labels, T("du.fs.not_found")), append(values, formatSizeShort(notFound)+"  "+T("du.fs.not_found_hint"))
	}
	labelWidth := 0
	for _, label := range labels {
		labelWidth = max(labelWidth, liveWidth(label))
	}
	for index, label := range labels {
		builder.WriteString(duFit("  "+liveCell(label, labelWidth+2)+values[index], width) + "\n")
	}
	for _, file := range deleted.Top {
		builder.WriteString(duFit(fmt.Sprintf("    %9s  pid %d %s  %s", formatSizeShort(file.Bytes), file.PID, file.Comm, file.Path), width) + "\n")
	}
	return builder.String()
}

// duFiles는 전체에서 가장 큰 파일 limit개만 남긴다. 나머지 파일은 부모 디렉터리의 합에만 들어간다.
// 그래서 메모리는 파일 수가 아니라 디렉터리 수를 따라 는다.
type duFiles struct {
	limit int
	// floor는 가득 찬 heap의 가장 작은 값이다. 대부분의 파일은 lock 없이 여기서 걸러진다.
	floor atomic.Uint64
	mu    sync.Mutex
	heap  duFileHeap
}

type duFile struct {
	parent *duNode
	name   string
	bytes  uint64
}

func (files *duFiles) offer(parent *duNode, name string, bytes uint64) {
	if files.limit <= 0 || bytes == 0 || bytes <= files.floor.Load() {
		return
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	heap.Push(&files.heap, duFile{parent: parent, name: name, bytes: bytes})
	if files.heap.Len() > files.limit {
		heap.Pop(&files.heap)
	}
	if files.heap.Len() == files.limit {
		files.floor.Store(files.heap[0].bytes)
	}
}

type duFileHeap []duFile

func (h duFileHeap) Len() int           { return len(h) }
func (h duFileHeap) Less(i, j int) bool { return h[i].bytes < h[j].bytes }
func (h duFileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *duFileHeap) Push(value any)    { *h = append(*h, value.(duFile)) }
func (h *duFileHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// duFinish는 남긴 큰 파일을 부모의 children으로 옮기고 하위 합을 계산한다.
func duFinish(root *duNode, files []duFile) {
	for _, file := range files {
		file.parent.bytes -= file.bytes
		file.parent.entries--
		file.parent.children = append(file.parent.children, &duNode{name: file.name, parent: file.parent, bytes: file.bytes, entries: 1})
	}
	duTotal(root)
}

func duTotal(node *duNode) {
	node.total, node.count = node.bytes, node.entries
	for _, child := range node.children {
		duTotal(child)
		node.total += child.total
		node.count += child.count
	}
}

// duLargestList는 깊이와 관계없이 가장 큰 디렉터리다. Complete면 모든 디렉터리가 들어 있어, 목록에 없는 디렉터리는
// 없던 것이다. 아니면 목록에 없는 디렉터리는 가장 작은 항목 이하다.
type duLargestList struct {
	Complete bool         `json:"complete"`
	Dirs     []duLargeDir `json:"dirs"`
}

type duLargeDir struct {
	Path    string `json:"path"`
	Bytes   uint64 `json:"bytes"`
	Entries int64  `json:"entries"`
	Label   string `json:"label,omitempty"`
}

// duLargest는 root를 뺀 디렉터리 가운데 큰 것 limit개를 크기순으로 고른다. 경로는 root 기준이다.
func duLargest(root *duNode, limit int) duLargestList {
	var dirs []duLargeDir
	count := 0
	offer := func(candidate duLargeDir) {
		if limit == 0 {
			return
		}
		if len(dirs) < limit {
			if dirs == nil {
				dirs = make([]duLargeDir, 0, limit)
			}
			dirs = append(dirs, candidate)
			for index := len(dirs) - 1; index > 0; {
				parent := (index - 1) / 2
				if !duLargeBefore(dirs[parent], dirs[index]) {
					break
				}
				dirs[parent], dirs[index] = dirs[index], dirs[parent]
				index = parent
			}
			return
		}
		if !duLargeBefore(candidate, dirs[0]) {
			return
		}
		dirs[0] = candidate
		for index := 0; ; {
			child := 2*index + 1
			if child >= len(dirs) {
				break
			}
			if child+1 < len(dirs) && duLargeBefore(dirs[child], dirs[child+1]) {
				child++
			}
			if !duLargeBefore(dirs[index], dirs[child]) {
				break
			}
			dirs[index], dirs[child] = dirs[child], dirs[index]
			index = child
		}
	}
	var visit func(node *duNode, prefix string)
	visit = func(node *duNode, prefix string) {
		for _, child := range node.children {
			if !child.dir {
				continue
			}
			count++
			path := prefix + child.name
			offer(duLargeDir{Path: path, Bytes: child.total, Entries: child.count})
			visit(child, path+"/")
		}
	}
	visit(root, "")
	if limit == 0 && count > 0 {
		dirs = []duLargeDir{}
	}
	sort.Slice(dirs, func(i, j int) bool { return duLargeBefore(dirs[i], dirs[j]) })
	return duLargestList{Complete: count <= limit, Dirs: dirs}
}

func duLargeBefore(a, b duLargeDir) bool {
	if a.Bytes != b.Bytes {
		return a.Bytes > b.Bytes
	}
	return a.Path < b.Path
}

// duDirDelta는 report diff의 디렉터리 하나다. Bound가 있으면 그쪽 report의 목록에 이 디렉터리가 없어, 그쪽 값은
// 그 목록의 가장 작은 크기 이하라는 상한이다. 그때 Delta는 변화의 하한이다.
type duDirDelta struct {
	Path   string `json:"path"`
	Before uint64 `json:"before_bytes"`
	After  uint64 `json:"after_bytes"`
	Delta  int64  `json:"delta_bytes"`
	Bound  string `json:"bound,omitempty"`
	Label  string `json:"label,omitempty"`
}

// duCompareDirs는 두 du.scan 결과의 큰 디렉터리 목록을 경로로 맞춘다. 비교하지 않으면 그 이유를 돌려준다.
// 목록이 없는 이전 report는 이유 없이 건너뛴다.
func duCompareDirs(before, after map[string]interface{}) ([]duDirDelta, string) {
	old, oldOK := duLargestOf(before)
	current, currentOK := duLargestOf(after)
	if !oldOK || !currentOK {
		return nil, ""
	}
	oldPath, _ := before["path"].(string)
	currentPath, _ := after["path"].(string)
	if oldPath != currentPath {
		return nil, T("du.diff.path_differs", oldPath, currentPath)
	}
	// 일부만 센 쪽은 빠진 디렉터리가 줄어든 것처럼 보인다.
	if partial, _ := before["partial"].(bool); partial {
		return nil, T("du.diff.partial")
	}
	if partial, _ := after["partial"].(bool); partial {
		return nil, T("du.diff.partial")
	}
	oldBytes, oldBound := duLargestIndex(old)
	currentBytes, currentBound := duLargestIndex(current)
	// 이름표는 나중 report의 것을 쓴다. 지운 디렉터리는 앞 report에만 이름표가 있다.
	labels := map[string]string{}
	for _, list := range []duLargestList{old, current} {
		for _, dir := range list.Dirs {
			if dir.Label != "" {
				labels[dir.Path] = dir.Label
			}
		}
	}
	var deltas []duDirDelta
	for path := range duPathUnion(oldBytes, currentBytes) {
		delta := duDirDelta{Path: path, Label: labels[path]}
		var hadBefore, hasAfter bool
		delta.Before, hadBefore = oldBytes[path]
		delta.After, hasAfter = currentBytes[path]
		if !hadBefore {
			delta.Before = oldBound
			if !old.Complete {
				delta.Bound = "before"
			}
		}
		if !hasAfter {
			delta.After = currentBound
			if !current.Complete {
				delta.Bound = "after"
			}
		}
		delta.Delta = int64(delta.After) - int64(delta.Before)
		// 상한과 비교해 변화가 없거나 반대로 나오면 실제 변화의 방향을 알 수 없다.
		if delta.Delta == 0 || (delta.Bound == "before" && delta.Delta < 0) || (delta.Bound == "after" && delta.Delta > 0) {
			continue
		}
		deltas = append(deltas, delta)
	}
	return duTopDeltas(duFoldAncestors(deltas), duDiffDirs), ""
}

func duLargestOf(metrics map[string]interface{}) (duLargestList, bool) {
	value, ok := metrics["largest_dirs"]
	if !ok {
		return duLargestList{}, false
	}
	// 파일에서 읽은 report는 map이고, 같은 process에서 만든 결과는 duLargestList다. JSON으로 한 번 맞춘다.
	data, err := json.Marshal(value)
	if err != nil {
		return duLargestList{}, false
	}
	var list duLargestList
	if err := json.Unmarshal(data, &list); err != nil {
		return duLargestList{}, false
	}
	return list, true
}

// duLargestIndex는 경로별 크기와, 목록에 없는 디렉터리의 크기를 돌려준다. 목록이 완전하면 없는 디렉터리는 0이다.
func duLargestIndex(list duLargestList) (map[string]uint64, uint64) {
	index := make(map[string]uint64, len(list.Dirs))
	var smallest uint64
	for position, dir := range list.Dirs {
		index[dir.Path] = dir.Bytes
		if position == 0 || dir.Bytes < smallest {
			smallest = dir.Bytes
		}
	}
	if list.Complete {
		return index, 0
	}
	return index, smallest
}

func duPathUnion(first, second map[string]uint64) map[string]struct{} {
	union := make(map[string]struct{}, len(first)+len(second))
	for path := range first {
		union[path] = struct{}{}
	}
	for path := range second {
		union[path] = struct{}{}
	}
	return union
}

// duFoldAncestors는 같은 방향의 변화를 90% 이상 설명하는 하위 디렉터리가 있으면 상위 디렉터리를 뺀다.
// 그대로 두면 /var, /var/lib, /var/lib/docker가 같은 증가로 줄을 모두 차지한다.
func duFoldAncestors(deltas []duDirDelta) []duDirDelta {
	var kept []duDirDelta
	for _, delta := range deltas {
		explained := false
		for _, other := range deltas {
			if strings.HasPrefix(other.Path, delta.Path+"/") && (other.Delta > 0) == (delta.Delta > 0) &&
				duAbs(other.Delta)*10 >= duAbs(delta.Delta)*9 {
				explained = true
				break
			}
		}
		if !explained {
			kept = append(kept, delta)
		}
	}
	return kept
}

func duTopDeltas(deltas []duDirDelta, limit int) []duDirDelta {
	sort.Slice(deltas, func(i, j int) bool {
		if duAbs(deltas[i].Delta) != duAbs(deltas[j].Delta) {
			return duAbs(deltas[i].Delta) > duAbs(deltas[j].Delta)
		}
		return deltas[i].Path < deltas[j].Path
	})
	if len(deltas) > limit {
		deltas = deltas[:limit]
	}
	return deltas
}

func duAbs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// formatDuDirDeltas는 report diff의 상세 줄이다. 한쪽 목록에 없던 디렉터리는 그쪽 값 앞에 ≤를 붙인다.
func formatDuDirDeltas(deltas []duDirDelta) []string {
	pathWidth := 0
	for _, delta := range deltas {
		pathWidth = min(max(pathWidth, liveWidth(delta.Path)), 40)
	}
	lines := make([]string, 0, len(deltas))
	for _, delta := range deltas {
		sign := "+"
		if delta.Delta < 0 {
			sign = "-"
		}
		before, after := formatSizeShort(delta.Before), formatSizeShort(delta.After)
		switch delta.Bound {
		case "before":
			before = "≤" + before
		case "after":
			after = "≤" + after
		}
		path := ansi.Truncate(delta.Path, pathWidth, "…")
		line := fmt.Sprintf("%10s  %s%s → %s", sign+formatSizeShort(uint64(duAbs(delta.Delta))), liveCell(path, pathWidth+2), before, after)
		if delta.Label != "" {
			line += "  " + delta.Label
		}
		lines = append(lines, line)
	}
	return lines
}

// duSnapshotResult는 edc snapshot --du의 결과다. edc du와 같은 탐색과 판정을 쓰고, snapshot의 제한 시간에 걸리면
// 그때까지 센 값을 partial로 남긴다.
func duSnapshotResult(ctx context.Context, path string, timeout time.Duration) Result {
	started := time.Now()
	sources, ok := hostDuSources()
	if !ok {
		return unsupported(duProbeID, T("du.skip.unsupported"))
	}
	result := Result{Probe: duProbeID, StartedAt: started.UTC()}
	options := duOptions{workers: duDefaultWorkers, rows: duPipedRows}
	root, err := duResolveRoot(path)
	var scan duScan
	if err == nil {
		options.root = root
		scan, err = duWalk(ctx, options, sources)
	}
	if err != nil {
		result = duPathFailure(result, path, err)
	} else {
		result = duResult(result, options, scan, timeout, false)
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

func duPathFailure(result Result, path string, err error) Result {
	result.Status, result.Summary = StatusFail, T("du.error.path", path)
	result.Error = &DiagnosticError{Kind: "path", Message: err.Error()}
	return result
}
