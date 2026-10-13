package edc

import (
	"container/heap"
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
	// 12코어 호스트의 /usr(캐시가 채워진 상태)에서 worker 4개가 du보다 3배 빨랐고 순간 사용량은 약 5코어였다.
	// 코어 수를 넘기면 더 빨라지지 않았다. 운영 호스트에서 CPU를 덜 쓰는 쪽을 기본으로 둔다.
	duDefaultWorkers = 4
	// 캐시가 빈 회전 디스크에서는 큰 트리를 읽는 데 오래 걸린다. 시간이 다 되면 그때까지 센 값을 partial로 보인다.
	duDefaultTimeout = 60 * time.Second
	duPipedRows      = 20
	// duMountRows는 터미널에 보이는 건너뛴 마운트 수다. container가 많은 호스트는 overlay 마운트가 수십 개다.
	duMountRows = 10
	// duUnreadableKeep은 읽지 못한 경로를 담는 상한이다. 개수는 모두 센다.
	duUnreadableKeep = 1000
)

type duOptions struct {
	root        string
	workers     int
	rows        int
	crossMounts bool
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
}

type duMount struct {
	Path      string `json:"path"`
	FSType    string `json:"fstype"`
	UsedBytes uint64 `json:"used_bytes,omitempty"`
	// Shared는 statfs가 이 마운트가 아니라 밑에 있는 파일 시스템 전체를 답하는 경우다. bind mount와 overlay가 그렇다.
	// 그 값을 마운트의 크기처럼 보이면 같은 숫자가 마운트마다 되풀이된다.
	Shared bool `json:"shared,omitempty"`
}

type duScan struct {
	root            *duNode
	directories     int64
	mounts          []duMount
	unreadable      []string
	unreadableCount int64
	partial         bool
	warnings        []string
}

type duRow struct {
	Path    string `json:"path"`
	Bytes   uint64 `json:"bytes"`
	Entries int64  `json:"entries"`
	Dir     bool   `json:"dir"`
	Depth   int    `json:"depth"`
}

type duOther struct {
	Bytes   uint64 `json:"bytes"`
	Entries int64  `json:"entries"`
}

// duSources는 탐색이 읽는 호스트 원천이다. 테스트는 마운트 표를 가짜로 넣어 실제 마운트를 만들지 않는다.
type duSources struct {
	mountinfo func() (string, error)
	statfs    func(path string) (uint64, bool)
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
		return emit(options, buildReport(version, started, target, []Result{unsupported(duProbeID, T("du.skip.linux_only"))}, options.redact))
	}
	terminal := options.jsonPath == "" && isTerminal(stdout)
	scanOptions := duOptions{workers: *workers, rows: *rows, crossMounts: *crossMounts}
	if scanOptions.rows == 0 {
		scanOptions.rows = duDefaultRows(stdout, terminal)
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	result := Result{Probe: duProbeID, StartedAt: started.UTC()}
	// 넘긴 경로가 symlink면 링크 자체가 아니라 가리키는 디렉터리를 잰다. 링크 하나의 크기는 쓸모가 없다.
	scanOptions.root, err = duResolveRoot(path)
	var scan duScan
	if err == nil {
		scan, err = duWalk(ctx, scanOptions, sources)
	}
	if err != nil {
		result.Status, result.Summary = StatusFail, T("du.error.path", path)
		result.Error = &DiagnosticError{Kind: "path", Message: err.Error()}
	} else {
		result = duResult(result, scanOptions, scan, options.timeout)
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
	return duPrint(stdout, options, result, terminal && os.Getenv("NO_COLOR") == "", width)
}

func duResolveRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// duDefaultRows는 결과 줄, 빈 줄, 나머지 줄, 건너뛴 마운트 몇 줄이 화면에 함께 들어가게 남긴다.
func duDefaultRows(stdout *os.File, terminal bool) int {
	if !terminal {
		return duPipedRows
	}
	_, height, err := term.GetSize(int(stdout.Fd()))
	if err != nil || height <= 0 {
		return duPipedRows
	}
	return max(5, height-8)
}

func duResult(result Result, options duOptions, scan duScan, timeout time.Duration) Result {
	root := scan.root
	rows, other := duSelect(root, options.rows)
	elapsed := time.Since(result.StartedAt).Round(time.Millisecond)
	result.Status = StatusPass
	result.Summary = T("du.summary", formatSizeShort(root.total), options.root, root.count, elapsed, options.workers)
	result.Warnings = append(result.Warnings, scan.warnings...)
	if scan.partial {
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
	result.Metrics = map[string]interface{}{
		"path": options.root, "bytes": root.total, "entries": root.count, "directories": scan.directories,
		"workers": options.workers, "partial": scan.partial, "unreadable": scan.unreadableCount,
		"rows": rows, "other": other, "skipped_mounts": scan.mounts,
	}
	return result
}

func duPrint(stdout io.Writer, options commonOptions, result Result, color bool, width int) int {
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
		fmt.Fprint(output, "\n"+formatDuRows(rows, other, width))
		mounts, _ := result.Metrics["skipped_mounts"].([]duMount)
		fmt.Fprint(output, formatDuMounts(mounts, options.verbose, width))
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

func formatDuRows(rows []duRow, other duOther, width int) string {
	var builder strings.Builder
	line := func(size, label string) {
		builder.WriteString(duFit(fmt.Sprintf("  %9s  %s", size, label), width))
		builder.WriteString("\n")
	}
	for _, row := range rows {
		name := row.Path[strings.LastIndex(row.Path, "/")+1:]
		if row.Dir {
			name += "/"
		}
		line(formatSizeShort(row.Bytes), strings.Repeat("  ", row.Depth)+name)
	}
	if other.Entries > 0 {
		line(formatSizeShort(other.Bytes), T("du.other", other.Entries))
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
		builder.WriteString(duFit(text, width) + "\n")
	}
	if hidden := len(mounts) - len(shown); hidden > 0 {
		builder.WriteString("  " + T("du.mounts.more", hidden) + "\n")
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
