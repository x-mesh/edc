package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// clean은 알려진 개발 캐시 위치를 찾아 사용량만 읽어서 보인다. 아무것도 지우거나 바꾸지 않는다.
// 사용량과 회수 가능량은 다르다. APFS clone·snapshot과 hard link 때문에 회수량은 사용량보다 작을 수 있어
// 회수 가능량은 내지 않는다.

// 도구 하나의 위치 조회 제한이다. 시험이 줄여 쓴다.
var cleanLookupTimeout = 5 * time.Second

const (
	cleanProbeID     = "clean.scan"
	cleanOutputLimit = 64 << 10
	cleanStderrLimit = 4 << 10
	// cleanDerivedGroups는 터미널에 보이는 DerivedData 묶음 수다.
	cleanDerivedGroups = 5
	cleanDetailRunes   = 80
	cleanDerivedID     = "xcode-derived-data"
)

const (
	cleanMeasured        = "measured"
	cleanPartial         = "partial"
	cleanAbsent          = "absent"
	cleanToolAbsent      = "tool_absent"
	cleanUnknownLocation = "unknown_location"
	cleanUnreadable      = "unreadable"
	cleanRejected        = "rejected"
	cleanDuplicate       = "duplicate"
	cleanNotScanned      = "not_scanned"
)

const (
	cleanReasonTimeout        = "timeout"
	cleanReasonStopped        = "stopped"
	cleanReasonToolTimeout    = "tool_timeout"
	cleanReasonToolFailed     = "tool_failed"
	cleanReasonInvalidOutput  = "invalid_output"
	cleanReasonPermission     = "permission_denied"
	cleanReasonStatError      = "stat_error"
	cleanReasonRootUnreadable = "root_unreadable"
	cleanReasonCoversUserData = "covers_user_data"
	cleanReasonSameAs         = "same_as"
	cleanReasonUnreadableDirs = "unreadable_dirs"
	cleanReasonSkippedMounts  = "skipped_mounts"
)

// 번역 키를 조합하는 값의 전체 목록이다. 시험이 세 언어에서 모두 번역이 있는지 확인한다.
var (
	cleanStatuses = []string{cleanMeasured, cleanPartial, cleanAbsent, cleanToolAbsent, cleanUnknownLocation,
		cleanUnreadable, cleanRejected, cleanDuplicate, cleanNotScanned}
	cleanReasons = []string{cleanReasonTimeout, cleanReasonStopped, cleanReasonToolTimeout, cleanReasonToolFailed,
		cleanReasonInvalidOutput, cleanReasonPermission, cleanReasonStatError, cleanReasonRootUnreadable,
		cleanReasonCoversUserData, cleanReasonSameAs, cleanReasonUnreadableDirs, cleanReasonSkippedMounts}
	cleanCategories  = []string{"build_cache", "package_cache"}
	cleanCosts       = []string{"low", "medium"}
	cleanSpecIDs     = []string{"go-build", "go-mod", "npm", "pnpm", "uv", "homebrew", cleanDerivedID, "swiftpm"}
	cleanNotMeasured = []cleanNotMeasuredItem{
		{"docker_engine", "engine_accounting"},
		{"vm_disks_volumes_db", "user_data"},
		{"app_caches_logs", "per_app_rules"},
		{"worktrees", "unsaved_work"},
		{"trash_downloads", "user_files"},
		{"project_build_dirs", "project_root"},
		{"sdk_simulators_models", "costly_to_restore"},
	}
)

type cleanNotMeasuredItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// cleanSpec은 카탈로그의 후보 하나다. fixed가 있으면 고정 경로이고, 없으면 tool에 위치를 묻는다.
type cleanSpec struct {
	id        string
	category  string
	fixed     string
	tool      string
	args      []string
	env       []string
	lines     int
	line      int
	cost      string
	cleanup   string
	groupApps bool
}

// cleanCatalog는 OS별 후보 목록이다. Linux 전용 고정 경로는 아직 없다.
func cleanCatalog(goos, home string) []cleanSpec {
	goEnv := []string{"GOTOOLCHAIN=local"}
	specs := []cleanSpec{
		{id: "go-build", category: "build_cache", tool: "go", args: []string{"env", "GOCACHE", "GOMODCACHE"}, env: goEnv,
			lines: 2, line: 0, cost: "low", cleanup: "go clean -cache"},
		{id: "go-mod", category: "package_cache", tool: "go", args: []string{"env", "GOCACHE", "GOMODCACHE"}, env: goEnv,
			lines: 2, line: 1, cost: "medium", cleanup: "go clean -modcache"},
		{id: "npm", category: "package_cache", tool: "npm", args: []string{"config", "get", "cache"},
			env: []string{"npm_config_update_notifier=false"}, lines: 1, cost: "medium", cleanup: "npm cache clean --force"},
		{id: "pnpm", category: "package_cache", tool: "pnpm", args: []string{"store", "path"}, lines: 1,
			cost: "medium", cleanup: "pnpm store prune"},
		{id: "uv", category: "package_cache", tool: "uv", args: []string{"cache", "dir"}, lines: 1,
			cost: "medium", cleanup: "uv cache clean"},
		{id: "homebrew", category: "package_cache", tool: "brew", args: []string{"--cache"}, lines: 1,
			cost: "medium", cleanup: "brew cleanup -s"},
	}
	if goos == "darwin" {
		specs = append(specs,
			cleanSpec{id: cleanDerivedID, category: "build_cache", fixed: filepath.Join(home, "Library/Developer/Xcode/DerivedData"),
				cost: "medium", groupApps: true},
			cleanSpec{id: "swiftpm", category: "package_cache", fixed: filepath.Join(home, "Library/Caches/org.swift.swiftpm"),
				cost: "medium", cleanup: "swift package purge-cache"},
		)
	}
	return specs
}

// cleanSources는 호스트 원천에서 마운트 표와 statfs만 복사한다. deletedOpen(/proc 전체)과 containerLabels(docker)는
// 후보마다 되풀이하면 비싸고 clean이 쓰지도 않으므로 nil로 둔다. clean이 duWalk를 거치면 Linux에서 nil 호출로 바로 드러난다.
func cleanSources(host duSources) duSources {
	return duSources{mountinfo: host.mountinfo, statfs: host.statfs}
}

type cleanToolSpec struct {
	Dir  string
	Env  []string
	Name string
	Args []string
}

type cleanToolOutput struct {
	Stdout    string
	Truncated bool
	Stderr    string
	ExitCode  int
	Err       error
}

var errCleanToolTimeout = errors.New("tool lookup timed out")

type cleanDeps struct {
	home     string
	lookPath func(string) (string, error)
	run      func(ctx context.Context, spec cleanToolSpec) cleanToolOutput
	measure  func(ctx context.Context, options duOptions, sources duSources) (duScan, error)
	sources  duSources
}

func hostCleanDeps(home string, host duSources) cleanDeps {
	return cleanDeps{home: home, lookPath: exec.LookPath, run: runCleanTool, measure: duMeasure, sources: cleanSources(host)}
}

// runCleanTool은 stdout만 제한 버퍼로 읽는다. stderr의 경고가 경로에 섞이지 않게 따로 받는다.
func runCleanTool(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
	lookup, cancel := context.WithTimeout(ctx, cleanLookupTimeout)
	defer cancel()
	command := exec.CommandContext(lookup, spec.Name, spec.Args...)
	command.Dir = spec.Dir
	command.Env = append(os.Environ(), spec.Env...)
	configureLogProcess(command)
	command.Cancel = func() error { return signalLogProcess(command, syscall.SIGKILL) }
	// 자식 process가 stdout pipe를 쥔 채 남으면 Run이 끝나지 않는다.
	command.WaitDelay = time.Second
	stdout := &remoteLimitedBuffer{limit: cleanOutputLimit}
	stderr := &remoteLimitedBuffer{limit: cleanStderrLimit}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		_ = signalLogProcess(command, syscall.SIGKILL)
	}
	output := cleanToolOutput{Stdout: stdout.String(), Truncated: stdout.Truncated(), Stderr: stderr.String(), Err: err}
	if command.ProcessState != nil {
		output.ExitCode = command.ProcessState.ExitCode()
	}
	if err != nil && ctx.Err() == nil && (errors.Is(lookup.Err(), context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay)) {
		output.Err = errCleanToolTimeout
	}
	return output
}

type cleanFilesystemRef struct {
	Mount  string `json:"mount"`
	FSType string `json:"fstype"`
}

type cleanGroup struct {
	Name       string `json:"name"`
	Folders    int    `json:"folders"`
	UsageBytes uint64 `json:"usage_bytes"`
}

// cleanItem은 후보 하나의 결과다. usage_bytes는 measured와 partial에만 있다. 회수 가능량 필드는 없다.
type cleanItem struct {
	ID              string              `json:"id"`
	Category        string              `json:"category"`
	Priority        string              `json:"priority"`
	Status          string              `json:"status"`
	Reasons         []string            `json:"reasons,omitempty"`
	Detail          string              `json:"detail,omitempty"`
	LocatedBy       string              `json:"located_by"`
	ReportedPath    string              `json:"reported_path,omitempty"`
	ResolvedPath    string              `json:"resolved_path,omitempty"`
	SameAs          string              `json:"same_as,omitempty"`
	Inside          string              `json:"inside,omitempty"`
	UsageBytes      *uint64             `json:"usage_bytes,omitempty"`
	LowerBound      bool                `json:"lower_bound,omitempty"`
	Entries         int64               `json:"entries,omitempty"`
	MeasuredAt      *time.Time          `json:"measured_at,omitempty"`
	ElapsedMS       *int64              `json:"elapsed_ms,omitempty"`
	Filesystem      *cleanFilesystemRef `json:"filesystem,omitempty"`
	Unreadable      int64               `json:"unreadable,omitempty"`
	UnreadablePaths []string            `json:"unreadable_paths,omitempty"`
	SkippedMounts   int                 `json:"skipped_mounts,omitempty"`
	Regenerable     bool                `json:"regenerable"`
	RegenCost       string              `json:"regen_cost"`
	Cleanup         string              `json:"cleanup,omitempty"`
	Groups          []cleanGroup        `json:"groups,omitempty"`
}

func (item cleanItem) counted() bool {
	return item.UsageBytes != nil && (item.Status == cleanMeasured || item.Status == cleanPartial)
}

// cleanFilesystem은 파일 시스템 하나의 사용량 합이다. 후보가 실제로 회수하는 양이 아니다.
type cleanFilesystem struct {
	Mount      string   `json:"mount"`
	FSType     string   `json:"fstype"`
	SizeBytes  uint64   `json:"size_bytes"`
	UsedBytes  uint64   `json:"used_bytes"`
	FreeBytes  uint64   `json:"free_bytes"`
	UsageBytes uint64   `json:"usage_bytes"`
	LowerBound bool     `json:"lower_bound,omitempty"`
	Candidates []string `json:"candidates"`
	// SharedBlocks는 APFS clone과 snapshot이 합계를 회수량보다 크게 만들 수 있다는 표시다.
	SharedBlocks bool `json:"shared_blocks,omitempty"`
}

type cleanQuery struct {
	lines  []string
	status string
	reason string
	detail string
}

type cleanScanner struct {
	deps    cleanDeps
	home    string
	queries map[string]cleanQuery
}

func cleanScan(ctx context.Context, deps cleanDeps, specs []cleanSpec) []cleanItem {
	scanner := &cleanScanner{deps: deps, home: deps.home, queries: map[string]cleanQuery{}}
	resolvedHome := deps.home
	if resolved, err := filepath.EvalSymlinks(deps.home); err == nil {
		resolvedHome = resolved
	}
	items := make([]cleanItem, 0, len(specs))
	seen := map[string]string{}
	for _, spec := range specs {
		item := cleanItem{ID: spec.id, Category: spec.category, Priority: "P1", Regenerable: true, RegenCost: spec.cost, Cleanup: spec.cleanup}
		if err := ctx.Err(); err != nil {
			item.Status, item.Reasons = cleanNotScanned, []string{cleanContextReason(err)}
			items = append(items, item)
			continue
		}
		scanner.measureOne(ctx, spec, &item, resolvedHome, seen)
		items = append(items, item)
	}
	cleanMarkInside(items)
	return items
}

func cleanContextReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return cleanReasonTimeout
	}
	return cleanReasonStopped
}

func (scanner *cleanScanner) measureOne(ctx context.Context, spec cleanSpec, item *cleanItem, resolvedHome string, seen map[string]string) {
	reported, ok := scanner.locate(ctx, spec, item)
	if !ok {
		return
	}
	item.ReportedPath = reported
	resolved, err := duResolveRoot(reported)
	if err != nil {
		cleanPathFailure(item, err)
		return
	}
	item.ResolvedPath = resolved
	// 도구 설정 오류로 홈이나 그 상위가 보고되면 사용자 데이터 전체를 캐시로 재게 된다.
	if pathUnder(scanner.home, resolved) || pathUnder(resolvedHome, resolved) {
		item.Status, item.Reasons = cleanRejected, []string{cleanReasonCoversUserData}
		return
	}
	if first, dup := seen[resolved]; dup {
		item.Status, item.Reasons, item.SameAs = cleanDuplicate, []string{cleanReasonSameAs}, first
		return
	}
	seen[resolved] = item.ID
	started := time.Now()
	scan, err := scanner.deps.measure(ctx, duOptions{root: resolved, workers: duDefaultWorkers}, scanner.deps.sources)
	if err != nil {
		cleanPathFailure(item, err)
		return
	}
	at := started.UTC()
	elapsed := scan.elapsed.Milliseconds()
	item.MeasuredAt, item.ElapsedMS = &at, &elapsed
	if scan.filesystem.Mount != "" {
		item.Filesystem = &cleanFilesystemRef{Mount: scan.filesystem.Mount, FSType: scan.filesystem.FSType}
	}
	if scan.root.unreadable {
		item.Status, item.Reasons = cleanUnreadable, []string{cleanReasonRootUnreadable}
		return
	}
	usage := scan.root.total
	item.UsageBytes, item.Entries = &usage, scan.root.count
	item.Unreadable, item.SkippedMounts = scan.unreadableCount, len(scan.mounts)
	item.UnreadablePaths = scan.unreadable
	item.Status = cleanMeasured
	if scan.partial {
		item.Reasons = append(item.Reasons, cleanContextReason(ctx.Err()))
	}
	if scan.unreadableCount > 0 {
		item.Reasons = append(item.Reasons, cleanReasonUnreadableDirs)
	}
	if len(scan.mounts) > 0 {
		item.Reasons = append(item.Reasons, cleanReasonSkippedMounts)
	}
	if len(item.Reasons) > 0 {
		item.Status, item.LowerBound = cleanPartial, true
	}
	if spec.groupApps {
		item.Groups = cleanGroupDerivedData(scan.root)
	}
}

// locate는 후보의 보고된 경로를 낸다. 찾지 못하면 item에 상태를 적고 false를 낸다. 경로를 추측하지 않는다.
func (scanner *cleanScanner) locate(ctx context.Context, spec cleanSpec, item *cleanItem) (string, bool) {
	if spec.fixed != "" {
		item.LocatedBy = "fixed_path"
		return spec.fixed, true
	}
	item.LocatedBy = strings.Join(append([]string{spec.tool}, spec.args...), " ")
	if _, err := scanner.deps.lookPath(spec.tool); err != nil {
		item.Status = cleanToolAbsent
		return "", false
	}
	key := item.LocatedBy
	query, cached := scanner.queries[key]
	if !cached {
		query = scanner.query(ctx, spec)
		scanner.queries[key] = query
	}
	if query.status != "" {
		item.Status, item.Detail = query.status, query.detail
		if query.reason != "" {
			item.Reasons = []string{query.reason}
		}
		return "", false
	}
	return query.lines[spec.line], true
}

func (scanner *cleanScanner) query(ctx context.Context, spec cleanSpec) cleanQuery {
	output := scanner.deps.run(ctx, cleanToolSpec{Dir: scanner.home, Env: spec.env, Name: spec.tool, Args: spec.args})
	fail := func(reason, detail string) cleanQuery {
		return cleanQuery{status: cleanUnknownLocation, reason: reason, detail: detail}
	}
	if output.Err != nil {
		switch {
		case ctx.Err() != nil:
			return fail(cleanContextReason(ctx.Err()), "")
		case errors.Is(output.Err, errCleanToolTimeout):
			return fail(cleanReasonToolTimeout, "")
		}
		detail := fmt.Sprintf("exit %d", output.ExitCode)
		if line := cleanFirstLine(output.Stderr); line != "" {
			detail += ": " + line
		}
		if output.ExitCode == 0 {
			detail = output.Err.Error()
		}
		return fail(cleanReasonToolFailed, detail)
	}
	text := strings.TrimSuffix(output.Stdout, "\n")
	lines := strings.Split(text, "\n")
	if output.Truncated || len(lines) != spec.lines {
		return fail(cleanReasonInvalidOutput, cleanFirstLine(output.Stdout))
	}
	for _, line := range lines {
		if line == "" || !filepath.IsAbs(line) {
			return fail(cleanReasonInvalidOutput, cleanFirstLine(output.Stdout))
		}
	}
	return cleanQuery{lines: lines}
}

func cleanFirstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	runes := []rune(line)
	if len(runes) > cleanDetailRunes {
		return string(runes[:cleanDetailRunes]) + "…"
	}
	return line
}

func cleanPathFailure(item *cleanItem, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		item.Status = cleanAbsent
	case errors.Is(err, fs.ErrPermission):
		item.Status, item.Reasons = cleanUnreadable, []string{cleanReasonPermission}
	default:
		item.Status, item.Reasons, item.Detail = cleanUnreadable, []string{cleanReasonStatError}, err.Error()
	}
}

// cleanMarkInside는 다른 후보 안에 있는 후보를 표시한다. 파일 시스템 합계에서 한 번만 세려는 것이다.
func cleanMarkInside(items []cleanItem) {
	for index := range items {
		if !items[index].counted() {
			continue
		}
		for other := range items {
			if other == index || !items[other].counted() {
				continue
			}
			if items[index].ResolvedPath != items[other].ResolvedPath && pathUnder(items[index].ResolvedPath, items[other].ResolvedPath) {
				items[index].Inside = items[other].ID
				break
			}
		}
	}
}

var cleanDerivedName = regexp.MustCompile(`^(.+)-[a-z]{28}$`)

// cleanGroupDerivedData는 DerivedData의 1단계 폴더를 앱 이름별로 묶는다. 이름 규칙에 맞지 않는 폴더는
// 앱으로 추정하지 않고 이름 그대로 한 묶음으로 둔다.
func cleanGroupDerivedData(root *duNode) []cleanGroup {
	byName := map[string]*cleanGroup{}
	for _, child := range root.children {
		if !child.dir {
			continue
		}
		name := child.name
		if match := cleanDerivedName.FindStringSubmatch(name); match != nil {
			name = match[1]
		}
		group := byName[name]
		if group == nil {
			group = &cleanGroup{Name: name}
			byName[name] = group
		}
		group.Folders++
		group.UsageBytes += child.total
	}
	groups := make([]cleanGroup, 0, len(byName))
	for _, group := range byName {
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].UsageBytes != groups[j].UsageBytes {
			return groups[i].UsageBytes > groups[j].UsageBytes
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

// cleanFilesystems는 파일 시스템별로 사용량을 더한다. 다른 후보 안에 있는 후보는 빼고, partial이 섞이면 하한이다.
func cleanFilesystems(items []cleanItem, sources duSources) []cleanFilesystem {
	var list []cleanFilesystem
	index := map[string]int{}
	for _, item := range items {
		if !item.counted() || item.Inside != "" {
			continue
		}
		mount, fstype := "", ""
		if item.Filesystem != nil {
			mount, fstype = item.Filesystem.Mount, item.Filesystem.FSType
		}
		position, found := index[mount]
		if !found {
			entry := cleanFilesystem{Mount: mount, FSType: fstype, SharedBlocks: fstype == "apfs"}
			if mount != "" && sources.statfs != nil {
				if usage, ok := sources.statfs(mount); ok {
					entry.SizeBytes, entry.UsedBytes = usage.Size, usage.Used
					if usage.Size > usage.Used {
						entry.FreeBytes = usage.Size - usage.Used
					}
				}
			}
			position = len(list)
			index[mount] = position
			list = append(list, entry)
		}
		entry := &list[position]
		entry.UsageBytes += *item.UsageBytes
		entry.LowerBound = entry.LowerBound || item.LowerBound
		entry.Candidates = append(entry.Candidates, item.ID)
	}
	return list
}

func runClean(args []string, version string) int {
	host, ok := hostDuSources()
	home, err := os.UserHomeDir()
	if err == nil && home == "" {
		err = errors.New("empty home directory")
	}
	deps := hostCleanDeps(home, host)
	return runCleanWith(args, version, deps, ok, err, os.Stdout)
}

func runCleanWith(args []string, version string, deps cleanDeps, ok bool, homeErr error, stdout io.Writer) int {
	options := configuredCommon(duDefaultTimeout)
	set := flag.NewFlagSet("clean", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	if set.NArg() > 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc clean [options]"))
		return 2
	}
	started := time.Now()
	target := map[string]interface{}{"goos": runtime.GOOS}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(cleanProbeID, T("clean.skip.unsupported"))}, options.redact))
	}
	file, _ := stdout.(*os.File)
	terminal := file != nil && isTerminal(file)
	// du와 같이 사람이 보는 터미널에서만 spinner를 띄운다. pipe, --json, NO_COLOR는 그대로 출력한다.
	live := options.jsonPath == "" && terminal && liveTerminal()
	lineShown := false
	result := Result{Probe: cleanProbeID, StartedAt: started.UTC()}
	if homeErr != nil {
		result.Status, result.Summary = StatusFail, T("clean.error.home")
		result.Error = &DiagnosticError{Kind: "home", Message: homeErr.Error()}
	} else {
		base, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, deadline := context.WithTimeout(base, options.timeout)
		defer deadline()
		scan := func() Result {
			items := cleanScan(ctx, deps, cleanCatalog(runtime.GOOS, deps.home))
			return cleanResult(result, items, cleanFilesystems(items, deps.sources), options.timeout, ctx.Err())
		}
		if live {
			// 화면에서 Ctrl-C는 신호가 아니라 키로 들어오므로 stop으로 탐색을 멈춘다.
			result, lineShown = cleanScanLive(scan, options.redact, stop)
		} else {
			result = scan()
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, target, []Result{result}, options.redact))
	}
	width := 0
	if terminal {
		width = terminalWidth()
	}
	return cleanPrint(stdout, options, result, terminal && os.Getenv("NO_COLOR") == "", width, lineShown)
}

// cleanScanLive는 탐색하는 동안 spinner 한 줄을 띄운다. probeLiveDelay 안에 끝나면 화면을 띄우지 않는다.
// 화면을 띄웠으면 마지막 화면에 결과 줄이 남으므로 두 번째 값이 true다.
func cleanScanLive(scan func() Result, redact bool, cancel context.CancelFunc) (Result, bool) {
	finished := make(chan Result, 1)
	go func() { finished <- scan() }()
	select {
	case result := <-finished:
		return result, false
	case <-time.After(probeLiveDelay):
	}
	model := newProbeModel(cleanProbeID, "", "", true, redact, cancel)
	live, err := startLiveProgram(model, cancel, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.live.start_failed", err))
		return <-finished, false
	}
	result := <-finished
	if _, err := live.finish(probeResultMsg{result: result}); err != nil {
		fmt.Fprintln(os.Stderr, T("observe.live.exit_error", err))
	}
	return result, true
}

func cleanResult(result Result, items []cleanItem, filesystems []cleanFilesystem, timeout time.Duration, ctxErr error) Result {
	measured, quiet := 0, true
	for _, item := range items {
		switch item.Status {
		case cleanMeasured:
			measured++
		case cleanAbsent, cleanToolAbsent, cleanDuplicate:
		default:
			quiet = false
		}
	}
	result.Status = StatusPass
	if !quiet {
		result.Status = StatusWarn
	}
	result.Summary = T("clean.summary", len(items), measured, len(items)-measured)
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		result.Status = StatusWarn
		result.Warnings = append(result.Warnings, T("clean.warning.timeout"))
		next := 4 * timeout
		if next < duDefaultTimeout {
			next = duDefaultTimeout
		}
		result.Next = fmt.Sprintf("edc clean --timeout %s", next)
	case ctxErr != nil:
		result.Status = StatusWarn
		result.Warnings = append(result.Warnings, T("clean.warning.stopped"))
	}
	result.Metrics = map[string]interface{}{"candidates": items, "filesystems": filesystems, "not_measured": cleanNotMeasured}
	return result
}

func cleanPad(text string, width int) string {
	if gap := width - ansi.StringWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

func cleanValue(item cleanItem) string {
	size := ""
	if item.counted() {
		size = formatSizeShort(*item.UsageBytes)
		if item.LowerBound {
			size = T("clean.lower_bound", size)
		}
		return size
	}
	word := T("clean.status." + item.Status)
	switch item.Status {
	case cleanToolAbsent:
		return T("clean.tool_absent", word, strings.Fields(item.LocatedBy)[0])
	case cleanDuplicate:
		return T("clean.duplicate", word, item.SameAs)
	}
	if len(item.Reasons) > 0 {
		return T("clean.with_reason", word, T("clean.reason."+item.Reasons[0]))
	}
	return word
}

// lineShown은 spinner 화면이 마지막에 결과 줄을 남겼다는 뜻이다. 그때는 결과 줄을 다시 쓰지 않는다.
func cleanPrint(stdout io.Writer, options commonOptions, result Result, color bool, width int, lineShown bool) int {
	var output io.Writer = stdout
	var buffer strings.Builder
	if options.redact {
		output = &buffer
	}
	if !lineShown {
		fmt.Fprint(output, formatResultLine(result, color))
	}
	printResultDetail(output, result, options.verbose, color)
	if result.Status != StatusFail {
		items, _ := result.Metrics["candidates"].([]cleanItem)
		filesystems, _ := result.Metrics["filesystems"].([]cleanFilesystem)
		fmt.Fprint(output, "\n"+formatCleanItems(items, options.verbose, width))
		fmt.Fprint(output, formatCleanFilesystems(filesystems, width))
		fmt.Fprint(output, formatCleanNotMeasured(options.verbose, width))
	}
	if options.redact {
		fmt.Fprint(stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode([]Result{result})
}

func formatCleanItems(items []cleanItem, verbose bool, width int) string {
	var categoryWidth, nameWidth, valueWidth int
	for _, item := range items {
		categoryWidth = max(categoryWidth, ansi.StringWidth(T("clean.category."+item.Category)))
		nameWidth = max(nameWidth, ansi.StringWidth(T("clean.candidate."+item.ID)))
		valueWidth = max(valueWidth, ansi.StringWidth(cleanValue(item)))
	}
	var builder strings.Builder
	for _, item := range items {
		location := "-"
		if item.ReportedPath != "" {
			location = item.ReportedPath
			if item.ResolvedPath != "" && item.ResolvedPath != item.ReportedPath {
				location = T("clean.resolved", item.ReportedPath, item.ResolvedPath)
			}
		}
		line := fmt.Sprintf("%s  %s  %s  %s  %s", item.Priority, cleanPad(T("clean.category."+item.Category), categoryWidth),
			cleanPad(T("clean.candidate."+item.ID), nameWidth), cleanPad(cleanValue(item), valueWidth), location)
		builder.WriteString(duFit(line, width) + "\n")
		if item.Inside != "" {
			builder.WriteString(duFit("      "+T("clean.inside", item.Inside), width) + "\n")
		}
		if item.counted() {
			builder.WriteString(formatCleanGroups(item.Groups, verbose, width))
		}
		if verbose {
			builder.WriteString(formatCleanDetail(item, width))
		}
	}
	return builder.String()
}

func formatCleanGroups(groups []cleanGroup, verbose bool, width int) string {
	shown := groups
	if !verbose && len(shown) > cleanDerivedGroups {
		shown = shown[:cleanDerivedGroups]
	}
	var builder strings.Builder
	for _, group := range shown {
		builder.WriteString(duFit("      "+T("clean.group", group.Name, group.Folders, formatSizeShort(group.UsageBytes)), width) + "\n")
	}
	if len(shown) < len(groups) {
		builder.WriteString("      " + T("clean.group_more", len(groups)-len(shown)) + "\n")
	}
	return builder.String()
}

func formatCleanDetail(item cleanItem, width int) string {
	var builder strings.Builder
	line := func(text string) { builder.WriteString(duFit("      "+text, width) + "\n") }
	if item.Detail != "" {
		line(item.Detail)
	}
	if len(item.Reasons) > 1 {
		reasons := make([]string, 0, len(item.Reasons))
		for _, reason := range item.Reasons {
			reasons = append(reasons, T("clean.reason."+reason))
		}
		line(strings.Join(reasons, ", "))
	}
	if item.Unreadable > 0 {
		line(T("clean.unreadable", item.Unreadable))
		for _, path := range item.UnreadablePaths {
			line("  " + path)
		}
	}
	if item.counted() {
		line(T("clean.regen", T("clean.cost."+item.RegenCost)))
	}
	if item.Cleanup != "" {
		line(T("clean.cleanup", item.Cleanup))
	}
	return builder.String()
}

func formatCleanFilesystems(filesystems []cleanFilesystem, width int) string {
	var builder strings.Builder
	for _, entry := range filesystems {
		usage := formatSizeShort(entry.UsageBytes)
		if entry.LowerBound {
			usage = T("clean.lower_bound", usage)
		}
		mount := entry.Mount
		if mount == "" {
			mount = "-"
		}
		builder.WriteString("\n" + duFit(T("clean.fs.title", mount, entry.FSType, usage, formatSizeShort(entry.FreeBytes)), width) + "\n")
		builder.WriteString("  " + duFit(T("clean.fs.not_reclaim"), width) + "\n")
		if entry.SharedBlocks {
			builder.WriteString("  " + duFit(T("clean.fs.apfs"), width) + "\n")
		}
	}
	return builder.String()
}

func formatCleanNotMeasured(verbose bool, width int) string {
	if !verbose {
		names := make([]string, 0, len(cleanNotMeasured))
		for _, item := range cleanNotMeasured {
			names = append(names, T("clean.not_measured."+item.ID))
		}
		return "\n" + duFit(T("clean.not_measured_line", strings.Join(names, "; ")), width) + "\n"
	}
	var builder strings.Builder
	builder.WriteString("\n" + T("clean.not_measured_title") + "\n")
	for _, item := range cleanNotMeasured {
		builder.WriteString(duFit("  "+T("clean.not_measured_item", T("clean.not_measured."+item.ID), T("clean.not_measured_reason."+item.Reason)), width) + "\n")
	}
	return builder.String()
}
