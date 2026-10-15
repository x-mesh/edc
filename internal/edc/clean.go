package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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
	// cleanDerivedGroups는 기본 출력에 보이는 묶음 수이고, cleanVerboseGroups는 -v에 보이는 묶음 수다.
	cleanDerivedGroups = 3
	cleanVerboseGroups = 30
	cleanDetailRunes   = 80
	cleanDerivedID     = "xcode-derived-data"
	// cleanNoSize는 크기를 모르는 행의 크기 칸이다. 0 B로 쓰면 비어 있다고 읽힌다.
	cleanNoSize = "-"
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
	cleanReasonInvalidEnv     = "invalid_env"
)

// 번역 키를 조합하는 값의 전체 목록이다. 시험이 세 언어에서 모두 번역이 있는지 확인한다.
var (
	cleanStatuses = []string{cleanMeasured, cleanPartial, cleanAbsent, cleanToolAbsent, cleanUnknownLocation,
		cleanUnreadable, cleanRejected, cleanDuplicate, cleanNotScanned, cleanNotRunning, cleanUnreachable, cleanUnsupported,
		cleanFailed, cleanNotReported, cleanNoProjectRoot}
	cleanReasons = []string{cleanReasonTimeout, cleanReasonStopped, cleanReasonToolTimeout, cleanReasonToolFailed,
		cleanReasonInvalidOutput, cleanReasonPermission, cleanReasonStatError, cleanReasonRootUnreadable,
		cleanReasonCoversUserData, cleanReasonSameAs, cleanReasonUnreadableDirs, cleanReasonSkippedMounts,
		cleanReasonInvalidEnv, cleanReasonEndpointScheme, cleanReasonSocketMissing, cleanReasonConnectionRefused,
		cleanReasonEngineTimeout, cleanReasonAPIVersion, cleanReasonHTTPStatus, cleanReasonRequestFailed,
		cleanReasonGitOutputTruncated, cleanReasonBaseMissing, cleanReasonGitdirMissing, cleanReasonWorktreeLimit,
		cleanReasonNotDirectory, cleanReasonProjectLimit, cleanReasonTrackedFiles, cleanReasonGitUnknown}
	cleanCategories = []string{"build_cache", "package_cache", "downloaded_binary", "xcode_support", "app_cache", "logs", "model", "toolchain"}
	cleanCosts      = []string{"low", "medium", "high", "unknown", "lost"}
	cleanSpecIDs    = []string{"go-build", "go-mod", "npm", "pnpm", "uv", "homebrew", cleanDerivedID, "swiftpm",
		"cargo-registry", "cargo-git", "zig", "bun", "pip", "playwright", "puppeteer", "node-gyp", "electron",
		"xcode-device-support", "coresimulator-caches", "app-updates", "app-logs", "huggingface", "rustup-toolchains", "ollama",
		"app-caches", "dot-cache"}
	cleanNotMeasured = []cleanNotMeasuredItem{
		{"other_vm_disks_db", "user_data"},
		{"other_worktrees", "outside_root"},
		{"trash_downloads", "user_files"},
		{"simulators", "device_data"},
		{"sdks", "costly_to_restore"},
		{"app_data", "user_data"},
		{"other_tool_caches", "unverified_location"},
	}
)

type cleanNotMeasuredItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// cleanEnv는 위치를 정하는 환경변수 하나다. 값에 suffix를 이어 붙인 경로가 캐시 폴더다.
type cleanEnv struct {
	name   string
	suffix string
}

// cleanSpec은 카탈로그의 후보 하나다. tool이 있으면 도구에 위치를 묻고, 없으면 envs를 순서대로 보다가
// 환경변수가 하나도 없을 때 fixed를 쓴다.
type cleanSpec struct {
	id       string
	category string
	priority string
	fixed    string
	envs     []cleanEnv
	// emptyUnset은 값이 비어 있으면 없는 것처럼 보는 변수다. 도구가 그렇게 읽는다고 확인한 변수만 넣는다.
	emptyUnset    []string
	tool          string
	args          []string
	env           []string
	lines         int
	line          int
	regenerable   bool
	cost          string
	cleanup       string
	groupApps     bool
	groupChildren bool
	children      *cleanChildren
}

// cleanChildren은 fixed 폴더의 1단계 항목을 따로 재는 묶음 후보다. suffix가 있으면 이름 끝이 맞는 항목만 재고,
// 없으면 앞선 후보를 뺀 나머지 항목을 모두 잰다.
type cleanChildren struct {
	suffix string
}

// cleanCatalog는 OS별 후보 목록이다. Linux 기본 경로는 소스, man page, 문서로 확인한 것만 둔다.
func cleanCatalog(goos, home string) []cleanSpec {
	at := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	darwin := goos == "darwin"
	goEnv := []string{"GOTOOLCHAIN=local"}
	specs := []cleanSpec{
		{id: "go-build", category: "build_cache", priority: "P1", tool: "go", args: []string{"env", "GOCACHE", "GOMODCACHE"}, env: goEnv,
			lines: 2, line: 0, regenerable: true, cost: "low", cleanup: "go clean -cache"},
		{id: "go-mod", category: "package_cache", priority: "P1", tool: "go", args: []string{"env", "GOCACHE", "GOMODCACHE"}, env: goEnv,
			lines: 2, line: 1, regenerable: true, cost: "medium", cleanup: "go clean -modcache"},
		{id: "npm", category: "package_cache", priority: "P1", tool: "npm", args: []string{"config", "get", "cache"},
			env: []string{"npm_config_update_notifier=false"}, lines: 1, regenerable: true, cost: "medium", cleanup: "npm cache clean --force"},
		{id: "pnpm", category: "package_cache", priority: "P1", tool: "pnpm", args: []string{"store", "path"}, lines: 1,
			regenerable: true, cost: "medium", cleanup: "pnpm store prune"},
		{id: "uv", category: "package_cache", priority: "P1", tool: "uv", args: []string{"cache", "dir"}, lines: 1,
			regenerable: true, cost: "medium", cleanup: "uv cache clean"},
		{id: "homebrew", category: "package_cache", priority: "P1", tool: "brew", args: []string{"--cache"}, lines: 1,
			regenerable: true, cost: "medium", cleanup: "brew cleanup -s"},
	}
	if darwin {
		specs = append(specs,
			cleanSpec{id: cleanDerivedID, category: "build_cache", priority: "P1", fixed: at("Library/Developer/Xcode/DerivedData"),
				regenerable: true, cost: "medium", groupApps: true},
			cleanSpec{id: "swiftpm", category: "package_cache", priority: "P1", fixed: at("Library/Caches/org.swift.swiftpm"),
				regenerable: true, cost: "medium", cleanup: "swift package purge-cache"},
		)
	}
	// 도구 설정 파일(bunfig.toml, .npmrc, .puppeteerrc)은 읽지 않는다. 기본 경로로 잰 값과 보고 경로로 차이를 볼 수 있다.
	specs = append(specs,
		cleanSpec{id: "cargo-registry", category: "package_cache", priority: "P1", fixed: at(".cargo/registry"),
			envs: []cleanEnv{{"CARGO_HOME", "registry"}}, regenerable: true, cost: "medium"},
		cleanSpec{id: "cargo-git", category: "package_cache", priority: "P1", fixed: at(".cargo/git"),
			envs: []cleanEnv{{"CARGO_HOME", "git"}}, regenerable: true, cost: "medium"},
		cleanSpec{id: "zig", category: "build_cache", priority: "P1", fixed: at(".cache/zig"),
			envs: []cleanEnv{{"ZIG_GLOBAL_CACHE_DIR", ""}, {"XDG_CACHE_HOME", "zig"}}, regenerable: true, cost: "medium",
			// zig의 resolveGlobalCacheDir(src/introspect.zig)는 빈 XDG_CACHE_HOME을 건너뛰고 ~/.cache/zig를 쓴다.
			emptyUnset: []string{"XDG_CACHE_HOME"}},
		cleanSpec{id: "bun", category: "package_cache", priority: "P1", fixed: at(".bun/install/cache"),
			envs:        []cleanEnv{{"BUN_INSTALL_CACHE_DIR", ""}, {"BUN_INSTALL", "install/cache"}, {"XDG_CACHE_HOME", ".bun/install/cache"}},
			regenerable: true, cost: "medium", cleanup: "bun pm cache rm"},
		cleanSpec{id: "pip", category: "package_cache", priority: "P1", tool: "pip3", args: []string{"cache", "dir"}, lines: 1,
			regenerable: true, cost: "medium", cleanup: "pip3 cache purge"},
	)
	playwright := cleanSpec{id: "playwright", category: "downloaded_binary", priority: "P2", fixed: at(".cache/ms-playwright"),
		envs:        []cleanEnv{{"PLAYWRIGHT_BROWSERS_PATH", ""}, {"XDG_CACHE_HOME", "ms-playwright"}},
		regenerable: true, cost: "high"}
	nodeGyp := cleanSpec{id: "node-gyp", category: "downloaded_binary", priority: "P2", fixed: at(".cache/node-gyp"),
		envs: []cleanEnv{{"npm_config_devdir", ""}, {"XDG_CACHE_HOME", "node-gyp"}}, regenerable: true, cost: "medium"}
	if darwin {
		// macOS의 Playwright와 node-gyp는 XDG_CACHE_HOME을 보지 않고 ~/Library/Caches를 쓴다.
		playwright.fixed, playwright.envs = at("Library/Caches/ms-playwright"), playwright.envs[:1]
		nodeGyp.fixed, nodeGyp.envs = at("Library/Caches/node-gyp"), nodeGyp.envs[:1]
	}
	specs = append(specs, playwright,
		cleanSpec{id: "puppeteer", category: "downloaded_binary", priority: "P2", fixed: at(".cache/puppeteer"),
			envs: []cleanEnv{{"PUPPETEER_CACHE_DIR", ""}}, regenerable: true, cost: "high"},
		nodeGyp)
	if darwin {
		specs = append(specs,
			cleanSpec{id: "electron", category: "downloaded_binary", priority: "P2", fixed: at("Library/Caches/electron"),
				envs: []cleanEnv{{"electron_config_cache", ""}}, regenerable: true, cost: "medium"},
			cleanSpec{id: "xcode-device-support", category: "xcode_support", priority: "P2", fixed: at("Library/Developer/Xcode"),
				children: &cleanChildren{suffix: "DeviceSupport"}, regenerable: true, cost: "high"},
			cleanSpec{id: "coresimulator-caches", category: "xcode_support", priority: "P2", fixed: at("Library/Developer/CoreSimulator/Caches"),
				regenerable: true, cost: "low"},
			// 앱이 남긴 폴더는 무엇이 들었는지 모르므로 다시 만들 수 있다고 하지 않는다.
			cleanSpec{id: "app-updates", category: "app_cache", priority: "P2", fixed: at("Library/Caches"),
				children: &cleanChildren{suffix: ".ShipIt"}, cost: "unknown"},
			cleanSpec{id: "app-logs", category: "logs", priority: "P2", fixed: at("Library/Logs"), groupChildren: true, cost: "lost"},
		)
	}
	specs = append(specs,
		cleanSpec{id: "huggingface", category: "model", priority: "P3", fixed: at(".cache/huggingface/hub"),
			envs:        []cleanEnv{{"HF_HUB_CACHE", ""}, {"HUGGINGFACE_HUB_CACHE", ""}, {"HF_HOME", "hub"}, {"XDG_CACHE_HOME", "huggingface/hub"}},
			regenerable: true, cost: "high", cleanup: "hf cache delete"},
		cleanSpec{id: "rustup-toolchains", category: "toolchain", priority: "P3", fixed: at(".rustup/toolchains"),
			envs: []cleanEnv{{"RUSTUP_HOME", "toolchains"}}, regenerable: true, cost: "high", groupChildren: true,
			cleanup: "rustup toolchain uninstall <toolchain>"},
	)
	if darwin {
		specs = append(specs, cleanSpec{id: "ollama", category: "model", priority: "P3", fixed: at(".ollama/models"),
			envs: []cleanEnv{{"OLLAMA_MODELS", ""}}, regenerable: true, cost: "high"})
		// 나머지 묶음은 앞선 모든 후보의 측정 경로를 알아야 하므로 카탈로그 끝에 둔다.
		specs = append(specs,
			cleanSpec{id: "app-caches", category: "app_cache", priority: "P2", fixed: at("Library/Caches"), children: &cleanChildren{}, cost: "unknown"},
			cleanSpec{id: "dot-cache", category: "app_cache", priority: "P2", fixed: at(".cache"), children: &cleanChildren{}, cost: "unknown"},
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
	home      string
	lookPath  func(string) (string, error)
	run       func(ctx context.Context, spec cleanToolSpec) cleanToolOutput
	lookupEnv func(string) (string, bool)
	deviceOf  func(string) (uint64, error)
	// dockerDial은 Docker API socket에 연결한다. 시험이 서버에 연결하도록 바꾼다.
	dockerDial func(ctx context.Context, path string) (net.Conn, error)
	measure    func(ctx context.Context, options duOptions, sources duSources) (duScan, error)
	sources    duSources
}

func hostCleanDeps(home string, host duSources) cleanDeps {
	return cleanDeps{home: home, lookPath: exec.LookPath, run: runCleanTool, lookupEnv: os.LookupEnv, deviceOf: cleanDeviceOf,
		dockerDial: cleanDockerDial, measure: duMeasure, sources: cleanSources(host)}
}

func cleanDockerDial(ctx context.Context, path string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", path)
}

// cleanDeviceOf는 경로가 있는 장치 번호다. symlink는 따라가지 않는다.
func cleanDeviceOf(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no device number")
	}
	return uint64(stat.Dev), nil
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
	Match           string              `json:"match,omitempty"`
	Excludes        []string            `json:"excludes,omitempty"`
	// roots는 실제로 잰 경로다. 단일 경로 후보는 하나이고 묶음 후보는 잰 항목마다 하나다. 포함 판정이 이것을 쓴다.
	roots   []string
	grouped bool
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

type cleanRoot struct {
	path string
	id   string
}

type cleanScanner struct {
	deps    cleanDeps
	home    string
	queries map[string]cleanQuery
	// roots는 앞선 후보가 잰 경로다. 같은 경로를 다시 재거나 나머지 묶음이 다시 탐색하지 않으려고 모은다.
	roots []cleanRoot
}

func cleanScan(ctx context.Context, deps cleanDeps, specs []cleanSpec) []cleanItem {
	scanner := &cleanScanner{deps: deps, home: deps.home, queries: map[string]cleanQuery{}}
	resolvedHome := deps.home
	if resolved, err := filepath.EvalSymlinks(deps.home); err == nil {
		resolvedHome = resolved
	}
	items := make([]cleanItem, 0, len(specs))
	for _, spec := range specs {
		item := cleanItem{ID: spec.id, Category: spec.category, Priority: spec.priority, Regenerable: spec.regenerable, RegenCost: spec.cost, Cleanup: spec.cleanup}
		if err := ctx.Err(); err != nil {
			item.Status, item.Reasons = cleanNotScanned, []string{cleanContextReason(err)}
			items = append(items, item)
			continue
		}
		scanner.measureOne(ctx, spec, &item, resolvedHome)
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

func (scanner *cleanScanner) measureOne(ctx context.Context, spec cleanSpec, item *cleanItem, resolvedHome string) {
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
	if spec.children != nil {
		scanner.measureChildren(ctx, spec, item, resolved)
		return
	}
	for _, root := range scanner.roots {
		if root.path == resolved {
			item.Status, item.Reasons, item.SameAs = cleanDuplicate, []string{cleanReasonSameAs}, root.id
			return
		}
	}
	scanner.roots = append(scanner.roots, cleanRoot{path: resolved, id: item.ID})
	item.roots = []string{resolved}
	measurement, err := cleanMeasurePath(ctx, scanner.deps, resolved)
	if err != nil {
		cleanPathFailure(item, err)
		return
	}
	measurement.applyTo(item)
	if item.UsageBytes == nil {
		return
	}
	switch {
	case spec.groupApps:
		item.Groups = cleanGroupDerivedData(measurement.scan.root)
	case spec.groupChildren:
		item.Groups = cleanGroupFolders(measurement.scan.root, nil)
	}
}

// cleanMeasurement는 경로 하나를 잰 결과다. 후보뿐 아니라 다른 섹션도 같은 상태 규칙으로 쓴다.
// 사용량이 없으면 root를 읽지 못한 것이다.
type cleanMeasurement struct {
	scan            duScan
	status          string
	reasons         []string
	lowerBound      bool
	measuredAt      time.Time
	elapsedMS       int64
	filesystem      *cleanFilesystemRef
	usage           *uint64
	entries         int64
	unreadable      int64
	unreadablePaths []string
	skippedMounts   int
}

// cleanMeasurePath는 resolved 경로의 block 사용량을 잰다. 시간 초과와 중단, 읽지 못한 디렉터리, 건너뛴 마운트는
// partial과 하한으로 남긴다.
func cleanMeasurePath(ctx context.Context, deps cleanDeps, resolved string) (cleanMeasurement, error) {
	started := time.Now()
	scan, err := deps.measure(ctx, duOptions{root: resolved, workers: duDefaultWorkers}, deps.sources)
	if err != nil {
		return cleanMeasurement{}, err
	}
	measurement := cleanMeasurement{scan: scan, measuredAt: started.UTC(), elapsedMS: scan.elapsed.Milliseconds()}
	if scan.filesystem.Mount != "" {
		measurement.filesystem = &cleanFilesystemRef{Mount: scan.filesystem.Mount, FSType: scan.filesystem.FSType}
	}
	if scan.root.unreadable {
		measurement.status, measurement.reasons = cleanUnreadable, []string{cleanReasonRootUnreadable}
		return measurement, nil
	}
	usage := scan.root.total
	measurement.usage, measurement.entries = &usage, scan.root.count
	measurement.unreadable, measurement.skippedMounts = scan.unreadableCount, len(scan.mounts)
	measurement.unreadablePaths = scan.unreadable
	measurement.status = cleanMeasured
	if scan.partial {
		measurement.reasons = append(measurement.reasons, cleanContextReason(ctx.Err()))
	}
	if scan.unreadableCount > 0 {
		measurement.reasons = append(measurement.reasons, cleanReasonUnreadableDirs)
	}
	if len(scan.mounts) > 0 {
		measurement.reasons = append(measurement.reasons, cleanReasonSkippedMounts)
	}
	if len(measurement.reasons) > 0 {
		measurement.status, measurement.lowerBound = cleanPartial, true
	}
	return measurement, nil
}

func (measurement cleanMeasurement) applyTo(item *cleanItem) {
	at, elapsed := measurement.measuredAt, measurement.elapsedMS
	item.MeasuredAt, item.ElapsedMS = &at, &elapsed
	item.Filesystem = measurement.filesystem
	item.Status, item.Reasons, item.LowerBound = measurement.status, measurement.reasons, measurement.lowerBound
	item.UsageBytes, item.Entries = measurement.usage, measurement.entries
	item.Unreadable, item.UnreadablePaths, item.SkippedMounts = measurement.unreadable, measurement.unreadablePaths, measurement.skippedMounts
}

// measureChildren은 부모 폴더의 1단계 항목을 하나씩 잰다. 앞선 후보가 잰 경로는 다시 재지 않고 excludes에 남긴다.
// 그 경로를 품은 폴더는 자신의 block만 더하고 하위로 내려가 품지 않은 부분만 잰다.
func (scanner *cleanScanner) measureChildren(ctx context.Context, spec cleanSpec, item *cleanItem, parent string) {
	item.grouped = true
	for _, root := range scanner.roots {
		if pathUnder(parent, root.path) {
			item.Status, item.Reasons, item.SameAs = cleanDuplicate, []string{cleanReasonSameAs}, root.id
			return
		}
	}
	device, err := scanner.deps.deviceOf(parent)
	if err != nil {
		cleanPathFailure(item, err)
		return
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		cleanPathFailure(item, err)
		return
	}
	if spec.children.suffix != "" {
		item.Match = "*" + spec.children.suffix
		matched := entries[:0:0]
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), spec.children.suffix) {
				matched = append(matched, entry)
			}
		}
		if len(matched) == 0 {
			item.Status = cleanAbsent
			return
		}
		entries = matched
	}
	walk := &cleanChildWalk{scanner: scanner, device: device, excluded: map[string]string{}, hit: map[string]bool{},
		groups: map[string]*cleanGroup{}}
	for _, root := range scanner.roots {
		if root.path != parent && pathUnder(root.path, parent) {
			walk.excluded[root.path] = root.id
		}
	}
	started := time.Now()
	walk.visit(ctx, parent, entries, "")
	at := started.UTC()
	elapsed := time.Since(started).Milliseconds()
	item.MeasuredAt, item.ElapsedMS = &at, &elapsed
	if walk.filesystem.Mount != "" {
		item.Filesystem = &cleanFilesystemRef{Mount: walk.filesystem.Mount, FSType: walk.filesystem.FSType}
	}
	usage := walk.usage
	item.UsageBytes, item.Entries = &usage, walk.entries
	item.Unreadable, item.UnreadablePaths, item.SkippedMounts = walk.unreadable, walk.unreadablePaths, walk.mounts
	item.roots = walk.roots
	for _, root := range walk.roots {
		scanner.roots = append(scanner.roots, cleanRoot{path: root, id: item.ID})
	}
	for id := range walk.hit {
		item.Excludes = append(item.Excludes, id)
	}
	sort.Strings(item.Excludes)
	for _, group := range walk.groups {
		item.Groups = append(item.Groups, *group)
	}
	sort.Slice(item.Groups, func(i, j int) bool {
		if item.Groups[i].UsageBytes != item.Groups[j].UsageBytes {
			return item.Groups[i].UsageBytes > item.Groups[j].UsageBytes
		}
		return item.Groups[i].Name < item.Groups[j].Name
	})
	item.Status = cleanMeasured
	if walk.stopped {
		item.Reasons = append(item.Reasons, cleanContextReason(ctx.Err()))
	}
	if walk.unreadable > 0 {
		item.Reasons = append(item.Reasons, cleanReasonUnreadableDirs)
	}
	if walk.mounts > 0 {
		item.Reasons = append(item.Reasons, cleanReasonSkippedMounts)
	}
	if len(item.Reasons) > 0 {
		item.Status, item.LowerBound = cleanPartial, true
	}
}

// cleanChildWalk는 묶음 후보 하나를 재는 동안의 상태다.
type cleanChildWalk struct {
	scanner         *cleanScanner
	device          uint64
	excluded        map[string]string
	hit             map[string]bool
	groups          map[string]*cleanGroup
	roots           []string
	usage           uint64
	entries         int64
	mounts          int
	unreadable      int64
	unreadablePaths []string
	filesystem      duFilesystem
	stopped         bool
}

func (walk *cleanChildWalk) visit(ctx context.Context, dir string, entries []os.DirEntry, group string) {
	for _, entry := range entries {
		if walk.stopped || ctx.Err() != nil {
			walk.stopped = true
			return
		}
		path := filepath.Join(dir, entry.Name())
		name := group
		if name == "" {
			name = entry.Name()
		}
		info, err := os.Lstat(path)
		if err != nil {
			walk.fail(path, err)
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if id, skip := walk.excluded[path]; skip {
			walk.hit[id] = true
			continue
		}
		device, err := walk.scanner.deps.deviceOf(path)
		if err != nil {
			walk.fail(path, err)
			continue
		}
		if device != walk.device {
			walk.mounts++
			continue
		}
		if info.IsDir() && walk.holdsExcluded(path) {
			walk.add(name, cleanBlocks(info), 1)
			children, err := os.ReadDir(path)
			if err != nil {
				walk.fail(path, err)
				continue
			}
			walk.visit(ctx, path, children, name)
			continue
		}
		walk.measure(ctx, path, name)
	}
}

func (walk *cleanChildWalk) holdsExcluded(dir string) bool {
	for path := range walk.excluded {
		if pathUnder(path, dir) {
			return true
		}
	}
	return false
}

func (walk *cleanChildWalk) measure(ctx context.Context, path, name string) {
	scanner := walk.scanner
	scan, err := scanner.deps.measure(ctx, duOptions{root: path, workers: duDefaultWorkers}, scanner.deps.sources)
	if err != nil {
		walk.fail(path, err)
		return
	}
	if walk.filesystem.Mount == "" {
		walk.filesystem = scan.filesystem
	}
	if scan.root.unreadable {
		walk.unreadable++
		walk.keep(path)
		return
	}
	walk.roots = append(walk.roots, path)
	walk.add(name, scan.root.total, scan.root.count)
	walk.unreadable += scan.unreadableCount
	for _, unreadable := range scan.unreadable {
		walk.keep(unreadable)
	}
	walk.mounts += len(scan.mounts)
	walk.stopped = walk.stopped || scan.partial
}

func (walk *cleanChildWalk) add(name string, bytes uint64, entries int64) {
	group := walk.groups[name]
	if group == nil {
		group = &cleanGroup{Name: name, Folders: 1}
		walk.groups[name] = group
	}
	group.UsageBytes += bytes
	walk.usage += bytes
	walk.entries += entries
}

// fail은 측정하지 못한 항목을 센다. 목록을 만든 뒤 사라진 항목은 건너뛴다.
func (walk *cleanChildWalk) fail(path string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	walk.unreadable++
	walk.keep(path)
}

func (walk *cleanChildWalk) keep(path string) {
	if len(walk.unreadablePaths) < duUnreadableKeep {
		walk.unreadablePaths = append(walk.unreadablePaths, path)
	}
}

func cleanBlocks(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Blocks) * 512
	}
	return uint64(info.Size())
}

// locate는 후보의 보고된 경로를 낸다. 찾지 못하면 item에 상태를 적고 false를 낸다. 경로를 추측하지 않는다.
func (scanner *cleanScanner) locate(ctx context.Context, spec cleanSpec, item *cleanItem) (string, bool) {
	if spec.tool == "" {
		return scanner.locateByPath(spec, item)
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

// locateByPath는 처음으로 환경에 있는 변수만 쓴다. 값이 비었거나 절대 경로가 아니면 다음 변수나 기본 경로로 넘어가지 않고
// 위치를 모른다고 둔다. 설정이 잘못됐는데 다른 폴더를 그 도구의 캐시로 재면 안 되기 때문이다.
func (scanner *cleanScanner) locateByPath(spec cleanSpec, item *cleanItem) (string, bool) {
	for _, candidate := range spec.envs {
		value, set := scanner.deps.lookupEnv(candidate.name)
		if !set || value == "" && slices.Contains(spec.emptyUnset, candidate.name) {
			continue
		}
		item.LocatedBy = "env:" + candidate.name
		if !filepath.IsAbs(value) {
			item.Status, item.Reasons = cleanUnknownLocation, []string{cleanReasonInvalidEnv}
			item.Detail = cleanFirstLine(candidate.name + "=" + value)
			return "", false
		}
		return filepath.Join(value, candidate.suffix), true
	}
	item.LocatedBy = "fixed_path"
	if len(spec.envs) > 0 {
		item.LocatedBy = "default_path"
	}
	return spec.fixed, true
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

// cleanFailure는 경로를 읽지 못한 오류를 상태와 이유로 바꾼다. 없는 경로는 이유가 없다.
func cleanFailure(err error) (status string, reasons []string, detail string) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cleanAbsent, nil, ""
	case errors.Is(err, fs.ErrPermission):
		return cleanUnreadable, []string{cleanReasonPermission}, ""
	default:
		return cleanUnreadable, []string{cleanReasonStatError}, err.Error()
	}
}

func cleanPathFailure(item *cleanItem, err error) {
	item.Status, item.Reasons, item.Detail = cleanFailure(err)
}

// cleanMarkInside는 다른 후보 안에 있는 후보를 표시한다. 파일 시스템 합계에서 한 번만 세려는 것이다.
// 포함 기준은 후보가 실제로 잰 경로뿐이다. 묶음 후보의 부모 폴더는 기준으로 쓰지 않는다.
func cleanMarkInside(items []cleanItem) {
	for index := range items {
		if !items[index].counted() {
			continue
		}
		for other := range items {
			if other != index && items[other].counted() && cleanInsideRoots(items[index], items[other]) {
				items[index].Inside = items[other].ID
				break
			}
		}
	}
}

func cleanInsideRoots(item, other cleanItem) bool {
	for _, root := range other.roots {
		// 묶음 후보는 부모 폴더 자체가 다른 후보의 측정 경로일 때도 그 안에 있다.
		if pathUnder(item.ResolvedPath, root) && (item.grouped || item.ResolvedPath != root) {
			return true
		}
	}
	return false
}

var cleanDerivedName = regexp.MustCompile(`^(.+)-[a-z]{28}$`)

// cleanGroupDerivedData는 DerivedData의 1단계 폴더를 앱 이름별로 묶는다. 이름 규칙에 맞지 않는 폴더는
// 앱으로 추정하지 않고 이름 그대로 한 묶음으로 둔다.
func cleanGroupDerivedData(root *duNode) []cleanGroup {
	return cleanGroupFolders(root, cleanDerivedName)
}

// cleanGroupFolders는 root의 1단계 폴더를 묶는다. appName이 있으면 그 규칙에 맞는 폴더를 앱 이름별로 합치고,
// nil이면 폴더 이름마다 한 묶음이다.
func cleanGroupFolders(root *duNode, appName *regexp.Regexp) []cleanGroup {
	byName := map[string]*cleanGroup{}
	for _, child := range root.children {
		if !child.dir {
			continue
		}
		name := child.name
		if appName != nil {
			if match := appName.FindStringSubmatch(name); match != nil {
				name = match[1]
			}
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
	return cleanFilesystemsWith(items, nil, sources)
}

// cleanFilesystemsWith는 후보 합계에 프로젝트 빌드 폴더를 더한다. 재생성 가능하다고 확인했고 다른 곳에서 세지 않은 폴더만
// 넣는다. 표지나 git 근거가 없는 폴더는 후보가 아니므로 합계에 들어가지 않는다.
func cleanFilesystemsWith(items []cleanItem, dirs []cleanBuildDir, sources duSources) []cleanFilesystem {
	type term struct {
		id         string
		usage      uint64
		lowerBound bool
		filesystem *cleanFilesystemRef
	}
	var terms []term
	for _, item := range items {
		// 잴 항목이 없어 파일 시스템을 모르는 묶음은 합계에 넣지 않는다.
		if item.counted() && item.Inside == "" && item.Filesystem != nil {
			terms = append(terms, term{item.ID, *item.UsageBytes, item.LowerBound, item.Filesystem})
		}
	}
	for _, dir := range dirs {
		if dir.counted() && dir.Regenerable && dir.Inside == "" && dir.Filesystem != nil {
			terms = append(terms, term{dir.Path, *dir.UsageBytes, dir.LowerBound, dir.Filesystem})
		}
	}
	var list []cleanFilesystem
	index := map[string]int{}
	for _, item := range terms {
		mount, fstype := item.filesystem.Mount, item.filesystem.FSType
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
		entry.UsageBytes += item.usage
		entry.LowerBound = entry.LowerBound || item.lowerBound
		entry.Candidates = append(entry.Candidates, item.id)
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
	var projects cleanProjects
	set.Var(&projects, "project", T("command.clean.option.project"))
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
			return cleanResult(result, cleanCollect(ctx, deps, runtime.GOOS, projects...), options.timeout, ctx.Err())
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
	return cleanPrint(stdout, options, result, terminal && os.Getenv("NO_COLOR") == "", width, lineShown, deps.home)
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

// cleanReport는 한 번의 탐색이 만든 결과 전체다. 섹션은 후보 밖에 따로 붙인다.
type cleanReport struct {
	candidates       []cleanItem
	filesystems      []cleanFilesystem
	projectBuildDirs cleanProjectBuildDirs
	docker           cleanDocker
	vmDisks          []cleanVMDisk
	worktrees        cleanWorktrees
}

// cleanCollect는 섹션을 카탈로그, 프로젝트 빌드 폴더, Docker 엔진, VM 디스크, worktree 순서로 실행한다.
// 하나의 ctx를 순차로 나눠 쓴다. 파일 시스템 합계는 후보와 재생성 가능한 빌드 폴더만 더한다. 엔진 수치와 VM 디스크는
// 같은 데이터를 다른 방법으로 본 값이고, worktree는 안의 빌드 폴더와 겹칠 수 있어 더하지 않는다.
func cleanCollect(ctx context.Context, deps cleanDeps, goos string, projects ...string) cleanReport {
	items := cleanScan(ctx, deps, cleanCatalog(goos, deps.home))
	report := cleanReport{candidates: items}
	report.projectBuildDirs = cleanProjectScan(ctx, deps, projects, items)
	report.filesystems = cleanFilesystemsWith(items, report.projectBuildDirs.Items, deps.sources)
	report.docker = cleanDockerScan(ctx, deps)
	report.vmDisks = cleanVMScan(ctx, deps, goos)
	report.worktrees = cleanWorktreeScan(ctx, deps)
	cleanLinkWorktrees(&report.projectBuildDirs, &report.worktrees)
	return report
}

func cleanResult(result Result, report cleanReport, timeout time.Duration, ctxErr error) Result {
	items := report.candidates
	measured, quiet := 0, true
	for _, item := range items {
		if item.counted() {
			measured++
		}
		switch item.Status {
		case cleanMeasured, cleanAbsent, cleanToolAbsent, cleanDuplicate:
		default:
			quiet = false
		}
	}
	result.Status = StatusPass
	if !quiet || !cleanSectionsQuiet(report) || !cleanWorktreesQuiet(report.worktrees) ||
		!cleanProjectQuiet(report.projectBuildDirs) {
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
	result.Metrics = map[string]interface{}{"candidates": items, "filesystems": report.filesystems,
		"project_build_dirs": report.projectBuildDirs, "docker": report.docker, "vm_disks": report.vmDisks,
		"worktrees": report.worktrees, "not_measured": cleanNotMeasured}
	return result
}

func cleanPad(text string, width int) string {
	if gap := width - ansi.StringWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

// cleanPadLeft는 크기 칸을 du처럼 오른쪽에 맞춰 자릿수를 위아래로 비교하게 한다.
func cleanPadLeft(text string, width int) string {
	if gap := width - ansi.StringWidth(text); gap > 0 {
		return strings.Repeat(" ", gap) + text
	}
	return text
}

// cleanSize는 크기 칸의 값이다. 하한은 du처럼 ≥를 붙여 칸 폭을 문장 길이만큼 늘리지 않는다.
func cleanSize(bytes uint64, lowerBound bool) string {
	size := formatSizeShort(bytes)
	if lowerBound {
		return "≥" + size
	}
	return size
}

// cleanHomePath는 홈 아래 경로를 ~로 줄인다. 좁은 화면에서 줄 끝이 잘릴 때 폴더 이름이 먼저 사라지지 않게 한다.
func cleanHomePath(path, home string) string {
	home = strings.TrimSuffix(home, "/")
	if home == "" || path == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}

func cleanItemSize(item cleanItem) string {
	if item.counted() {
		return cleanSize(*item.UsageBytes, item.LowerBound)
	}
	return cleanNoSize
}

// cleanItemState는 크기가 없는 후보의 상태 문구다.
func cleanItemState(item cleanItem) string {
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
func cleanPrint(stdout io.Writer, options commonOptions, result Result, color bool, width int, lineShown bool, home string) int {
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
		projects, _ := result.Metrics["project_build_dirs"].(cleanProjectBuildDirs)
		docker, _ := result.Metrics["docker"].(cleanDocker)
		disks, _ := result.Metrics["vm_disks"].([]cleanVMDisk)
		worktrees, _ := result.Metrics["worktrees"].(cleanWorktrees)
		style := cleanStyle(color)
		if color {
			fmt.Fprint(output, "\n"+richCleanItems(items, options.verbose, width, home))
		} else {
			fmt.Fprint(output, "\n"+formatCleanItems(items, options.verbose, width, home))
		}
		fmt.Fprint(output, formatCleanProject(projects, options.verbose, width, home, style))
		fmt.Fprint(output, formatCleanDocker(docker, options.verbose, width, style))
		fmt.Fprint(output, formatCleanVMDisks(disks, docker.Inside, width, home, style))
		fmt.Fprint(output, formatCleanWorktrees(worktrees, options.verbose, width, home, style))
		fmt.Fprint(output, formatCleanNotMeasured(options.verbose, width, style))
		// 합계는 맨 끝에 둔다. 출력이 화면보다 길면 명령이 끝난 뒤 남는 것은 마지막 줄이다.
		fmt.Fprint(output, formatCleanFilesystems(filesystems, width, style))
	}
	if options.redact {
		fmt.Fprint(stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode([]Result{result})
}

// cleanQuiet는 기본 출력에서 상태별 한 줄로 접는 상태다. 크기가 없고 확인할 것도 없다.
var cleanQuiet = []string{cleanAbsent, cleanToolAbsent, cleanDuplicate}

// cleanWrap은 width가 0보다 크면 단어 사이에서 줄을 바꾸고, 0이면 한 줄로 낸다. 이름이 잘려 사라지지 않게 한다.
// 한 단어가 width보다 길 때만 단어 안에서 바꾼다.
func cleanWrap(text string, width int) string {
	if width > 0 {
		text = ansi.Wrap(text, width, "")
	}
	return text + "\n"
}

// formatCleanItems는 후보마다 우선순위, 크기, 이름, 위치를 한 줄에 보인다. 분류는 이름과 겹치므로 JSON에만 둔다.
// 우선순위 안에서는 큰 후보부터 보이고, 크기가 없는 후보는 그 우선순위의 끝에 둔다.
func formatCleanItems(items []cleanItem, verbose bool, width int, home string) string {
	shown, folded := cleanShownItems(items, verbose)
	limit, _ := cleanGroupLimit(verbose)
	var sizeWidth, nameWidth int
	for _, item := range shown {
		sizeWidth = max(sizeWidth, ansi.StringWidth(cleanItemSize(item)))
		nameWidth = max(nameWidth, ansi.StringWidth(T("clean.candidate."+item.ID)))
		if item.counted() {
			for _, group := range item.Groups[:min(len(item.Groups), limit)] {
				sizeWidth = max(sizeWidth, ansi.StringWidth(formatSizeShort(group.UsageBytes)))
			}
		}
	}
	var builder strings.Builder
	for _, item := range shown {
		lead := ansi.StringWidth(item.Priority) + 2
		place := cleanLocation(item, home)
		if !item.counted() {
			place = cleanItemState(item)
			if item.ReportedPath != "" {
				place += "  " + cleanLocation(item, home)
			}
		}
		line := fmt.Sprintf("%s  %s  %s  %s", item.Priority, cleanPadLeft(cleanItemSize(item), sizeWidth),
			cleanPad(T("clean.candidate."+item.ID), nameWidth), place)
		builder.WriteString(duFit(line, width) + "\n")
		// 묶음 이름, 포함 표시, 자세한 줄은 모두 이름 칸보다 두 칸 안쪽에 둔다.
		indent := strings.Repeat(" ", lead+sizeWidth+4)
		if item.Inside != "" {
			builder.WriteString(duFit(indent+T("clean.inside", item.Inside), width) + "\n")
		}
		if item.counted() {
			builder.WriteString(formatCleanGroups(item.Groups, verbose, width, lead, sizeWidth))
		}
		if verbose {
			builder.WriteString(formatCleanDetail(item, width, indent, home, false))
		}
	}
	builder.WriteString(formatCleanFolded(folded, width, false))
	return builder.String()
}

func formatCleanFolded(folded map[string][]string, width int, style cleanStyle) string {
	var builder strings.Builder
	for _, status := range cleanQuiet {
		if names := folded[status]; len(names) > 0 {
			builder.WriteString(style.wrap(T("clean.fold."+status, strings.Join(names, ", ")), width))
		}
	}
	return builder.String()
}

// richCleanItems는 색을 쓰는 터미널의 후보 목록이다. 우선순위마다 배지 제목을 두고, 행은 크기, 막대, 이름, 위치 순이다.
// 기본 출력은 묶음을 이름 칸 아래 한 줄로 접는다.
func richCleanItems(items []cleanItem, verbose bool, width int, home string) string {
	style := cleanStyle(true)
	shown, folded := cleanShownItems(items, verbose)
	limit, more := cleanGroupLimit(verbose)
	var sizeWidth, nameWidth int
	var largest uint64
	for _, item := range shown {
		sizeWidth = max(sizeWidth, liveWidth(cleanItemSize(item)))
		nameWidth = max(nameWidth, liveWidth(T("clean.candidate."+item.ID)))
		if !item.counted() {
			continue
		}
		if *item.UsageBytes > largest {
			largest = *item.UsageBytes
		}
		if verbose {
			for _, group := range item.Groups[:min(len(item.Groups), limit)] {
				sizeWidth = max(sizeWidth, liveWidth(formatSizeShort(group.UsageBytes)))
			}
		}
	}
	barWidth := cleanBarWidth(width)
	bar := func(bytes uint64) string {
		if barWidth == 0 {
			return ""
		}
		return style.size(richBar(float64(bytes), float64(largest), barWidth), bytes, false) + "  "
	}
	blank := ""
	if barWidth > 0 {
		blank = strings.Repeat(" ", barWidth+2)
	}
	nameAt := 1 + sizeWidth + 2 + len(blank)
	// 묶음, 포함 표시, 자세한 줄의 글자는 평문처럼 이름 칸보다 두 칸 안쪽에서 시작한다. 접은 묶음 줄은 그 두 칸에 ▸를 둔다.
	indent := strings.Repeat(" ", nameAt+2)
	line := func(text string) string { return duFit(text, width) + "\n" }
	var builder strings.Builder
	priority := ""
	for _, item := range shown {
		if item.Priority != priority {
			priority = item.Priority
			builder.WriteString(line(richBadge(priority, richMagenta) + "  " + richBold(T("clean.priority."+priority))))
		}
		name := cleanPad(T("clean.candidate."+item.ID), nameWidth)
		size := cleanPadLeft(cleanItemSize(item), sizeWidth)
		if !item.counted() {
			place := style.state(cleanItemState(item), item.Status)
			if item.ReportedPath != "" {
				place += "  " + richDim(cleanLocation(item, home))
			}
			builder.WriteString(line(" " + richDim(size) + "  " + blank + name + "  " + place))
		} else {
			bytes := *item.UsageBytes
			builder.WriteString(line(" " + style.size(size, bytes, true) + "  " + bar(bytes) + name + "  " + richDim(cleanLocation(item, home))))
		}
		if item.Inside != "" {
			builder.WriteString(line(indent + richDim(T("clean.inside", item.Inside))))
		}
		if item.counted() && len(item.Groups) > 0 {
			if verbose {
				for _, group := range item.Groups[:min(len(item.Groups), limit)] {
					text := " " + style.size(cleanPadLeft(formatSizeShort(group.UsageBytes), sizeWidth), group.UsageBytes, false) + "  " + blank + "  " + group.Name
					if group.Folders > 1 {
						text += "  " + richDim(T("clean.group_folders", group.Folders))
					}
					builder.WriteString(line(text))
				}
				if len(item.Groups) > limit {
					builder.WriteString(line(indent + richDim(T(more, len(item.Groups)-limit))))
				}
			} else {
				builder.WriteString(line(strings.Repeat(" ", nameAt) + richDim(cleanCollapsedGroups(item.Groups, limit, width-nameAt))))
			}
		}
		if verbose {
			builder.WriteString(formatCleanDetail(item, width, indent, home, style))
		}
	}
	builder.WriteString(formatCleanFolded(folded, width, style))
	return builder.String()
}

// cleanCollapsedGroups는 묶음을 "▸ 이름 크기 · 이름 크기 · N more" 한 줄로 접는다. room 칸에 들어가는 묶음만 보이고
// 나머지는 N more에 센다. 줄 끝을 자르면 남은 개수가 사라진다. room이 0 이하이면 폭을 보지 않는다.
func cleanCollapsedGroups(groups []cleanGroup, limit, room int) string {
	tail := func(rest int) string {
		if rest == 0 {
			return ""
		}
		return " · " + T("clean.group_more_short", rest)
	}
	text := "▸"
	shown := 0
	for _, group := range groups[:min(len(groups), limit)] {
		separator := " "
		if shown > 0 {
			separator = " · "
		}
		next := text + separator + group.Name + " " + formatSizeShort(group.UsageBytes)
		if room > 0 && shown > 0 && liveWidth(next+tail(len(groups)-shown-1)) > room {
			break
		}
		text = next
		shown++
	}
	return text + tail(len(groups)-shown)
}

// cleanBarWidth는 터미널 폭에 맞춘 막대 칸 수다. 좁은 화면에서는 경로가 남도록 막대를 줄이고, 70칸보다 좁으면 뺀다.
// 폭을 모르면(0) 가장 넓은 막대를 쓴다.
func cleanBarWidth(width int) int {
	switch {
	case width <= 0 || width >= 100:
		return 20
	case width < 70:
		return 0
	}
	return 8 + (width-70)*12/30
}

// cleanShownItems는 후보를 우선순위, 크기 순으로 정렬하고, 기본 출력에서는 조용한 상태의 후보를 상태별 이름 목록으로 접는다.
func cleanShownItems(items []cleanItem, verbose bool) ([]cleanItem, map[string][]string) {
	sorted := append([]cleanItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := sorted[i], sorted[j]
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		if left.counted() != right.counted() {
			return left.counted()
		}
		return left.counted() && *left.UsageBytes > *right.UsageBytes
	})
	var shown []cleanItem
	folded := map[string][]string{}
	for _, item := range sorted {
		if !verbose && slices.Contains(cleanQuiet, item.Status) {
			folded[item.Status] = append(folded[item.Status], cleanFoldedName(item))
			continue
		}
		shown = append(shown, item)
	}
	return shown, folded
}

func cleanFoldedName(item cleanItem) string {
	name := T("clean.candidate." + item.ID)
	switch item.Status {
	case cleanToolAbsent:
		return T("clean.fold.tool_item", name, strings.Fields(item.LocatedBy)[0])
	case cleanDuplicate:
		return T("clean.fold.duplicate_item", name, T("clean.candidate."+item.SameAs))
	}
	return name
}

// cleanLocation은 후보의 위치 칸이다. 묶음 후보는 부모 폴더에 이름 규칙이나 제외 표시를 붙인다.
func cleanLocation(item cleanItem, home string) string {
	if item.ReportedPath == "" {
		return "-"
	}
	location := cleanHomePath(item.ReportedPath, home)
	if item.ResolvedPath != "" && item.ResolvedPath != item.ReportedPath {
		location = T("clean.resolved", location, cleanHomePath(item.ResolvedPath, home))
	}
	switch {
	case item.Match != "":
		return location + "/" + item.Match
	case item.grouped:
		return T("clean.remainder", location)
	}
	return location
}

func cleanGroupLimit(verbose bool) (int, string) {
	if verbose {
		return cleanVerboseGroups, "clean.group_more_json"
	}
	return cleanDerivedGroups, "clean.group_more"
}

// formatCleanGroups는 묶음의 크기를 후보와 같은 크기 칸에 두고 이름만 들여 쓴다. edc du가 하위 항목을 보이는 방식이다.
// 폴더 수는 한 묶음에 폴더가 둘 이상일 때만 쓴다. 폴더 이름별 묶음은 늘 하나다.
func formatCleanGroups(groups []cleanGroup, verbose bool, width, lead, sizeWidth int) string {
	limit, more := cleanGroupLimit(verbose)
	shown := groups
	if len(shown) > limit {
		shown = shown[:limit]
	}
	var builder strings.Builder
	for _, group := range shown {
		label := group.Name
		if group.Folders > 1 {
			label += "  " + T("clean.group_folders", group.Folders)
		}
		builder.WriteString(duFit(strings.Repeat(" ", lead)+cleanPadLeft(formatSizeShort(group.UsageBytes), sizeWidth)+"    "+label, width) + "\n")
	}
	if len(shown) < len(groups) {
		builder.WriteString(duFit(strings.Repeat(" ", lead+sizeWidth+4)+T(more, len(groups)-len(shown)), width) + "\n")
	}
	return builder.String()
}

func formatCleanDetail(item cleanItem, width int, indent, home string, style cleanStyle) string {
	var builder strings.Builder
	write := func(text string) { builder.WriteString(duFit(indent+text, width) + "\n") }
	line := func(text string) { write(style.dim(text)) }
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
		write(style.warn(T("clean.unreadable", item.Unreadable)))
		for _, path := range item.UnreadablePaths {
			line("  " + cleanHomePath(path, home))
		}
	}
	if item.counted() {
		line(T("clean.regen", T("clean.cost."+item.RegenCost)))
	}
	if len(item.Excludes) > 0 {
		names := make([]string, 0, len(item.Excludes))
		for _, id := range item.Excludes {
			names = append(names, T("clean.candidate."+id))
		}
		line(T("clean.excludes", strings.Join(names, ", ")))
	}
	if item.SkippedMounts > 0 {
		line(T("clean.skipped_mounts", item.SkippedMounts))
	}
	if item.Cleanup != "" {
		write(style.phrase("clean.cleanup", item.Cleanup, richCyan))
	}
	return builder.String()
}

func formatCleanFilesystems(filesystems []cleanFilesystem, width int, style cleanStyle) string {
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
		title := T("clean.fs.title", mount, entry.FSType, usage, formatSizeShort(entry.FreeBytes))
		if style {
			title = richChip(T("clean.fs.label")) + " " + richBold(mount) + "  " + richDim(entry.FSType) + "   " +
				richDim(T("clean.fs.usage")) + " " + richBold(usage) + "   " + richDim(T("clean.fs.free")) + " " +
				richPaint(formatSizeShort(entry.FreeBytes), richGreen, true)
		}
		builder.WriteString("\n" + duFit(title, width) + "\n")
		note := func(text string) {
			if style {
				builder.WriteString(duFit("  "+richDim(text), width) + "\n")
				return
			}
			builder.WriteString("  " + duFit(text, width) + "\n")
		}
		note(T("clean.fs.not_reclaim"))
		if entry.SharedBlocks {
			note(T("clean.fs.apfs"))
		}
	}
	return builder.String()
}

func formatCleanNotMeasured(verbose bool, width int, style cleanStyle) string {
	if !verbose {
		names := make([]string, 0, len(cleanNotMeasured))
		for _, item := range cleanNotMeasured {
			names = append(names, T("clean.not_measured."+item.ID))
		}
		return "\n" + style.wrap(T("clean.not_measured_line", strings.Join(names, "; ")), width)
	}
	var builder strings.Builder
	builder.WriteString("\n" + style.dim(T("clean.not_measured_title")) + "\n")
	for _, item := range cleanNotMeasured {
		builder.WriteString(duFit("  "+style.dim(T("clean.not_measured_item", T("clean.not_measured."+item.ID), T("clean.not_measured_reason."+item.Reason))), width) + "\n")
	}
	return builder.String()
}

// cleanStyle은 색을 쓰는 터미널에서만 글자를 칠한다. false면 모든 메서드가 받은 글자를 그대로 돌려줘 평문이 바뀌지 않는다.
type cleanStyle bool

func (style cleanStyle) dim(text string) string {
	if style {
		return richDim(text)
	}
	return text
}

func (style cleanStyle) warn(text string) string {
	if style {
		return richPaint(text, richYellow, false)
	}
	return text
}

func (style cleanStyle) size(text string, bytes uint64, bold bool) string {
	if style {
		return richPaint(text, richSizeColor(int64(bytes)), bold)
	}
	return text
}

// cleanProblems는 다시 확인해야 하는 상태다. 노랑으로 칠하고, 나머지 크기 없는 상태는 회색으로 칠한다.
var cleanProblems = []string{cleanUnknownLocation, cleanUnreadable, cleanRejected, cleanNotScanned, cleanUnreachable,
	cleanUnsupported, cleanFailed, cleanNotReported}

func (style cleanStyle) state(text, status string) string {
	if slices.Contains(cleanProblems, status) {
		return style.warn(text)
	}
	return style.dim(text)
}

// phrase는 key의 문장을 회색으로, %s 자리의 value를 color로 칠한다. 문장 전체를 회색으로 감싸면 value의 reset 뒤가
// 회색을 잃는다.
func (style cleanStyle) phrase(key, value, color string) string {
	if !style {
		return T(key, value)
	}
	before, after, _ := strings.Cut(T(key, "\x00"), "\x00")
	return richDim(before) + richPaint(value, color, false) + richDim(after)
}

// wrap은 cleanWrap처럼 줄을 바꾼 뒤 줄마다 회색으로 칠한다. 칠한 뒤에 바꾸면 둘째 줄부터 색이 빠진다.
func (style cleanStyle) wrap(text string, width int) string {
	wrapped := strings.TrimSuffix(cleanWrap(text, width), "\n")
	if !style {
		return wrapped + "\n"
	}
	lines := strings.Split(wrapped, "\n")
	for index := range lines {
		lines[index] = richDim(lines[index])
	}
	return strings.Join(lines, "\n") + "\n"
}

// open은 섹션 제목이다. 평문은 빈 줄 뒤의 title 한 줄이고, 색을 쓰면 panel 제목과 detail로 회색 패널을 연다.
// detail은 패널 윗줄이 width를 넘지 않게 자른다.
func (style cleanStyle) open(title, panel, detail string, width int) string {
	if !style {
		return "\n" + duFit(title, width) + "\n"
	}
	if width > 0 {
		room := width - liveWidth("╭─ "+panel+"  "+" ───")
		if room < 8 {
			detail = ""
		}
		detail = ansi.Truncate(detail, max(room, 0), "…")
	}
	return "\n" + duFit(richPanelTop(panel, detail, ""), width) + "\n"
}

// row는 섹션 안의 한 줄이다. 색을 쓰면 패널 테두리 뒤에 두고, 칸이 맞도록 모든 줄을 평문보다 한 칸씩 안쪽에 둔다.
func (style cleanStyle) row(indent, text string, width int) string {
	if !style {
		return duFit(indent+text, width) + "\n"
	}
	return duFit(richPanelLine(indent[1:]+text, ""), width) + "\n"
}

func (style cleanStyle) close() string {
	if !style {
		return ""
	}
	return richPanelBottom("") + "\n"
}
