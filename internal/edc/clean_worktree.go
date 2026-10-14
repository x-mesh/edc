package edc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// worktree 섹션은 git-kit이 만든 worktree의 상태와 용량을 읽기만 한다. 사용 여부는 수정 시각이 아니라
// git 상태로 판정하고, 수정 시각은 읽지도 내지도 않는다. git에는 아래 읽기 하위 명령만 쓴다.

const (
	cleanWorktreeRootPath = ".gk/worktree"
	cleanWorktreeDepth    = 5
	cleanWorktreeLimit    = 256
	cleanWorktreeShown    = 10
	cleanWorktreeCleanup  = "git-kit worktree cleanup"
	cleanGitFileLimit     = 4 << 10
)

const (
	cleanWorktreeLinked     = "worktree"
	cleanWorktreeRepository = "repository"
	cleanWorktreeUnlinked   = "unlinked"
)

const (
	cleanVerdictUnlinked    = "unlinked"
	cleanVerdictHasChanges  = "has_changes"
	cleanVerdictStateUnk    = "state_unknown"
	cleanVerdictLocked      = "locked"
	cleanVerdictNotMerged   = "not_merged"
	cleanVerdictBaseUnknown = "base_unknown"
	cleanVerdictMergedClean = "merged_clean"
)

const (
	cleanBaseGKParent   = "gk_parent"
	cleanBaseOriginHead = "origin_head"
	cleanBaseNone       = "none"
)

const (
	cleanReasonGitOutputTruncated = "git_output_truncated"
	cleanReasonBaseMissing        = "base_missing"
	cleanReasonGitdirMissing      = "gitdir_missing"
	cleanReasonWorktreeLimit      = "worktree_limit"
)

// 번역 키를 조합하는 값의 전체 목록이다. verdict는 우선순위 순서이고 판정이 위에서부터 첫 일치를 고른다.
var (
	cleanWorktreeVerdicts = []string{cleanVerdictUnlinked, cleanVerdictHasChanges, cleanVerdictStateUnk, cleanVerdictLocked,
		cleanVerdictNotMerged, cleanVerdictBaseUnknown, cleanVerdictMergedClean}
	cleanWorktreeBaseSources = []string{cleanBaseGKParent, cleanBaseOriginHead}
)

// cleanGitEnv는 git이 index를 다시 쓰거나(GIT_OPTIONAL_LOCKS) 입력을 기다리지 않게 한다. 일반 git status는 index의
// 수정 시각을 바꾼다.
var cleanGitEnv = []string{"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}

// cleanWorktree는 worktree 하나의 결과다. 알 수 없는 값은 0이 아니라 null이다.
type cleanWorktree struct {
	Path             string     `json:"path"`
	Kind             string     `json:"kind"`
	Repository       string     `json:"repository,omitempty"`
	Branch           string     `json:"branch,omitempty"`
	Head             string     `json:"head,omitempty"`
	Registered       *bool      `json:"registered"`
	Locked           bool       `json:"locked"`
	Prunable         bool       `json:"prunable"`
	Changes          *int       `json:"changes"`
	Untracked        *int       `json:"untracked"`
	CountsLowerBound bool       `json:"counts_lower_bound,omitempty"`
	Base             string     `json:"base,omitempty"`
	BaseSource       string     `json:"base_source"`
	Merged           *bool      `json:"merged"`
	Ahead            *int       `json:"ahead"`
	LastCommitAt     *time.Time `json:"last_commit_at"`
	HeadMovedAt      *time.Time `json:"head_moved_at"`
	UsageBytes       *uint64    `json:"usage_bytes,omitempty"`
	LowerBound       bool       `json:"lower_bound,omitempty"`
	// BuildDirs는 --project로 찾은 빌드 폴더 수다. 빌드 폴더를 찾지 않았으면 0이 아니라 null이다.
	BuildDirs *int     `json:"build_dirs"`
	Verdict   string   `json:"verdict"`
	Reasons   []string `json:"reasons,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	// failed는 git 명령이 하나라도 답하지 못했다는 뜻이다. 이 경우 merged_clean이 될 수 없다.
	failed bool
}

type cleanWorktreeLeftover struct {
	UsageBytes uint64 `json:"usage_bytes"`
}

// cleanWorktrees는 worktree 루트 섹션이다. 어떤 합계에도 넣지 않는다.
type cleanWorktrees struct {
	Status     string                 `json:"status"`
	Reasons    []string               `json:"reasons,omitempty"`
	Detail     string                 `json:"detail,omitempty"`
	Root       string                 `json:"root"`
	UsageBytes *uint64                `json:"usage_bytes,omitempty"`
	LowerBound bool                   `json:"lower_bound,omitempty"`
	Truncated  bool                   `json:"truncated,omitempty"`
	Leftover   *cleanWorktreeLeftover `json:"leftover,omitempty"`
	Cleanup    string                 `json:"cleanup,omitempty"`
	MeasuredAt *time.Time             `json:"measured_at,omitempty"`
	ElapsedMS  *int64                 `json:"elapsed_ms,omitempty"`
	Items      []cleanWorktree        `json:"items"`
}

func (section cleanWorktrees) counted() bool {
	return section.Status == cleanMeasured || section.Status == cleanPartial
}

func cleanAddReason(reasons []string, reason string) []string {
	if slices.Contains(reasons, reason) {
		return reasons
	}
	return append(reasons, reason)
}

// cleanWorktreesQuiet는 확인할 일이 없는 상태다. 루트가 없는 것은 worktree를 쓰지 않는 호스트의 정상 상태다.
func cleanWorktreesQuiet(section cleanWorktrees) bool {
	return section.Status == cleanMeasured || section.Status == cleanAbsent
}

// cleanWorktreeScan은 worktree 루트를 걸어 worktree를 찾고, 루트를 한 번 재서 worktree별 용량을 읽고,
// worktree마다 읽기 전용 git으로 상태를 판정한다.
func cleanWorktreeScan(ctx context.Context, deps cleanDeps) cleanWorktrees {
	section := cleanWorktrees{Root: filepath.Join(deps.home, cleanWorktreeRootPath), Items: []cleanWorktree{}}
	if err := ctx.Err(); err != nil {
		section.Status, section.Reasons = cleanNotScanned, []string{cleanContextReason(err)}
		return section
	}
	resolved, err := duResolveRoot(section.Root)
	if err != nil {
		section.Status, section.Reasons, section.Detail = cleanFailure(err)
		return section
	}
	section.Root = resolved
	if _, err := deps.lookPath("git"); err != nil {
		section.Status = cleanToolAbsent
		return section
	}
	device, err := deps.deviceOf(resolved)
	if err != nil {
		section.Status, section.Reasons, section.Detail = cleanFailure(err)
		return section
	}
	walk := &cleanWorktreeWalk{ctx: ctx, deps: deps, root: resolved, device: device}
	if err := walk.readRoot(); err != nil {
		section.Status, section.Reasons, section.Detail = cleanFailure(err)
		return section
	}
	section.Items, section.Truncated = walk.items, walk.truncated
	section.Status = cleanMeasured
	reasons := []string(nil)
	if walk.truncated {
		reasons = append(reasons, cleanReasonWorktreeLimit)
	}
	if walk.stopped {
		reasons = cleanAddReason(reasons, cleanContextReason(ctx.Err()))
	}
	if walk.unreadable > 0 {
		reasons = cleanAddReason(reasons, cleanReasonUnreadableDirs)
	}
	if walk.mounts > 0 {
		reasons = cleanAddReason(reasons, cleanReasonSkippedMounts)
	}

	measurement, err := cleanMeasurePath(ctx, deps, resolved)
	switch {
	case err != nil:
		status, failure, detail := cleanFailure(err)
		if status == cleanAbsent {
			failure = []string{cleanReasonStatError}
		}
		reasons, section.Detail = append(reasons, failure...), detail
	case measurement.usage == nil:
		reasons = append(reasons, measurement.reasons...)
	default:
		at, elapsed := measurement.measuredAt, measurement.elapsedMS
		section.MeasuredAt, section.ElapsedMS = &at, &elapsed
		section.UsageBytes, section.LowerBound = measurement.usage, measurement.lowerBound
		for _, reason := range measurement.reasons {
			reasons = cleanAddReason(reasons, reason)
		}
		cleanWorktreeUsage(&section, measurement.scan.root)
	}

	lists := map[string]*cleanWorktreeList{}
	for index := range section.Items {
		item := &section.Items[index]
		if item.Kind != cleanWorktreeUnlinked {
			if err := ctx.Err(); err != nil {
				item.failed, item.Reasons = true, cleanAddReason(item.Reasons, cleanContextReason(err))
				reasons = cleanAddReason(reasons, cleanContextReason(err))
			} else {
				cleanWorktreeInspect(ctx, deps, item, lists)
			}
		}
		item.Verdict = cleanWorktreeVerdict(*item)
	}
	if err := ctx.Err(); err != nil {
		reasons = cleanAddReason(reasons, cleanContextReason(err))
	}
	section.Reasons = reasons
	if len(reasons) > 0 {
		section.Status = cleanPartial
	}
	section.Cleanup = cleanWorktreeCleanup
	return section
}

// cleanWorktreeWalk는 루트 아래에서 .git이 있는 디렉터리를 찾는다. worktree 아래로는 내려가지 않는다.
type cleanWorktreeWalk struct {
	ctx        context.Context
	deps       cleanDeps
	root       string
	device     uint64
	items      []cleanWorktree
	truncated  bool
	stopped    bool
	unreadable int
	mounts     int
}

func (walk *cleanWorktreeWalk) readRoot() error {
	entries, err := os.ReadDir(walk.root)
	if err != nil {
		return err
	}
	walk.descend(walk.root, entries, 1)
	return nil
}

func (walk *cleanWorktreeWalk) descend(dir string, entries []os.DirEntry, depth int) {
	for _, entry := range entries {
		if walk.truncated || walk.stopped {
			return
		}
		if walk.ctx.Err() != nil {
			walk.stopped = true
			return
		}
		// symlink는 따라가지 않는다. 다른 장치의 디렉터리도 건너뛰고 센다.
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		device, err := walk.deps.deviceOf(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				walk.unreadable++
			}
			continue
		}
		if device != walk.device {
			walk.mounts++
			continue
		}
		walk.visit(path, depth)
	}
}

func (walk *cleanWorktreeWalk) visit(path string, depth int) {
	info, err := os.Lstat(filepath.Join(path, ".git"))
	switch {
	case err == nil:
		if len(walk.items) >= cleanWorktreeLimit {
			walk.truncated = true
			return
		}
		walk.items = append(walk.items, cleanWorktreeClassify(path, info))
		return
	case !errors.Is(err, fs.ErrNotExist):
		walk.unreadable++
		return
	}
	if depth >= cleanWorktreeDepth {
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			walk.unreadable++
		}
		return
	}
	walk.descend(path, entries, depth+1)
}

// cleanWorktreeClassify는 .git만 보고 종류를 정한다. gitfile이 가리키는 gitdir이 없으면 git을 부르지 않는다.
// main 저장소가 사라진 worktree에서 git은 128로 실패하는데, 이를 변경 없음으로 읽으면 안 되기 때문이다.
func cleanWorktreeClassify(path string, info fs.FileInfo) cleanWorktree {
	item := cleanWorktree{Path: path, Kind: cleanWorktreeLinked, BaseSource: cleanBaseNone}
	if info.IsDir() {
		item.Kind = cleanWorktreeRepository
		return item
	}
	unlinked := func(detail string) cleanWorktree {
		item.Kind, item.Reasons, item.Detail = cleanWorktreeUnlinked, []string{cleanReasonGitdirMissing}, detail
		return item
	}
	file, err := os.Open(filepath.Join(path, ".git"))
	if err != nil {
		item.failed, item.Reasons, item.Detail = true, []string{cleanReasonStatError}, err.Error()
		return item
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cleanGitFileLimit))
	if err != nil {
		item.failed, item.Reasons, item.Detail = true, []string{cleanReasonStatError}, err.Error()
		return item
	}
	target, found := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	target = strings.TrimSpace(target)
	if !found || target == "" {
		return unlinked(cleanFirstLine(string(data)))
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		return unlinked(target)
	}
	return item
}

// cleanWorktreeUsage는 du 트리에서 worktree별 용량을 읽고 루트 합에서 뺀 나머지를 낸다. 루트 측정이 partial이면
// 모든 값이 하한이다.
func cleanWorktreeUsage(section *cleanWorktrees, tree *duNode) {
	var sum uint64
	for index := range section.Items {
		item := &section.Items[index]
		node := tree
		rel, err := filepath.Rel(section.Root, item.Path)
		if err != nil {
			continue
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			node = cleanWorktreeChild(node, part)
			if node == nil {
				break
			}
		}
		if node == nil || node.unreadable {
			continue
		}
		total := node.total
		item.UsageBytes, item.LowerBound = &total, section.LowerBound
		sum += total
	}
	leftover := uint64(0)
	if *section.UsageBytes > sum {
		leftover = *section.UsageBytes - sum
	}
	section.Leftover = &cleanWorktreeLeftover{UsageBytes: leftover}
}

func cleanWorktreeChild(node *duNode, name string) *duNode {
	for _, child := range node.children {
		if child.dir && child.name == name {
			return child
		}
	}
	return nil
}

// cleanWorktreeVerdict는 우선순위 순서로 첫 일치를 고른다. merged_clean은 확인된 사실이 모두 있어야 한다.
func cleanWorktreeVerdict(item cleanWorktree) string {
	changed := item.Changes != nil && *item.Changes > 0 || item.Untracked != nil && *item.Untracked > 0
	switch {
	case item.Kind == cleanWorktreeUnlinked:
		return cleanVerdictUnlinked
	case changed:
		return cleanVerdictHasChanges
	case item.failed || item.Changes == nil || item.Untracked == nil:
		return cleanVerdictStateUnk
	case item.Locked:
		return cleanVerdictLocked
	case item.Merged != nil && !*item.Merged:
		return cleanVerdictNotMerged
	case item.Merged == nil:
		return cleanVerdictBaseUnknown
	}
	return cleanVerdictMergedClean
}

// cleanGit은 읽기 전용 git을 실행하는 유일한 길이다. fsmonitor hook은 worktree 안의 설정이 실행하는 명령이라 끈다.
func cleanGit(ctx context.Context, deps cleanDeps, dir string, args ...string) cleanToolOutput {
	return deps.run(ctx, cleanToolSpec{Dir: dir, Env: cleanGitEnv, Name: "git", Args: append([]string{"-c", "core.fsmonitor=false"}, args...)})
}

// cleanWorktreeList는 저장소 하나의 worktree list 결과다. 공통 디렉터리마다 한 번만 부른다.
type cleanWorktreeList struct {
	entries map[string]cleanWorktreeEntry
	reason  string
	detail  string
}

type cleanWorktreeEntry struct {
	locked   bool
	prunable bool
}

type cleanWorktreeProbe struct {
	ctx  context.Context
	deps cleanDeps
	item *cleanWorktree
	// lastReason은 가장 최근에 기록한 실패다. 저장소의 worktree list가 실패하면 같은 저장소의 다른 worktree도 같은 이유를 받는다.
	lastReason string
}

// fail은 답하지 못한 git 명령을 기록한다. 이 worktree는 merged_clean이 될 수 없다.
func (probe *cleanWorktreeProbe) fail(reason, detail string) {
	probe.item.failed, probe.lastReason = true, reason
	probe.item.Reasons = cleanAddReason(probe.item.Reasons, reason)
	if probe.item.Detail == "" {
		probe.item.Detail = detail
	}
}

// run은 종료 코드를 돌려준다. 종료 코드가 뜻을 가지는 명령(merge-base, config --get)이 직접 해석하도록 0이 아닌 코드도
// 실패로 만들지 않는다. 실행하지 못했거나 시간 초과·중단이면 실패로 기록하고 ok=false를 낸다.
func (probe *cleanWorktreeProbe) run(args ...string) (output cleanToolOutput, code int, ok bool) {
	output = cleanGit(probe.ctx, probe.deps, probe.item.Path, args...)
	switch {
	case output.Err == nil:
		return output, 0, true
	case probe.ctx.Err() != nil:
		probe.fail(cleanContextReason(probe.ctx.Err()), "")
	case errors.Is(output.Err, errCleanToolTimeout):
		probe.fail(cleanReasonToolTimeout, "")
	case output.ExitCode > 0:
		return output, output.ExitCode, true
	default:
		probe.fail(cleanReasonToolFailed, output.Err.Error())
	}
	return output, 0, false
}

// succeed는 종료 코드 0과 잘리지 않은 출력을 요구한다. 아니면 실패로 기록한다.
func (probe *cleanWorktreeProbe) succeed(args ...string) (string, bool) {
	output, code, ok := probe.run(args...)
	if !ok {
		return "", false
	}
	if code != 0 {
		detail := fmt.Sprintf("exit %d", code)
		if line := cleanFirstLine(output.Stderr); line != "" {
			detail += ": " + line
		}
		probe.fail(cleanReasonToolFailed, detail)
		return "", false
	}
	if output.Truncated {
		probe.fail(cleanReasonGitOutputTruncated, "")
		return "", false
	}
	return strings.TrimSpace(output.Stdout), true
}

// cleanWorktreeInspect는 worktree 하나의 상태를 읽는다. 명령 하나가 실패해도 나머지는 계속 읽어 변경 여부를 놓치지 않는다.
func cleanWorktreeInspect(ctx context.Context, deps cleanDeps, item *cleanWorktree, lists map[string]*cleanWorktreeList) {
	probe := &cleanWorktreeProbe{ctx: ctx, deps: deps, item: item}
	detached := false
	if text, ok := probe.succeed("rev-parse", "--git-common-dir", "--abbrev-ref", "HEAD"); ok {
		lines := strings.Split(text, "\n")
		if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
			probe.fail(cleanReasonInvalidOutput, cleanFirstLine(text))
		} else {
			common := lines[0]
			if !filepath.IsAbs(common) {
				common = filepath.Join(item.Path, common)
			}
			item.Repository = filepath.Clean(common)
			if lines[1] == "HEAD" {
				detached = true
			} else {
				item.Branch = lines[1]
			}
		}
	}
	probe.status()
	if item.Kind == cleanWorktreeLinked && item.Repository != "" {
		probe.register(lists)
	}
	if item.Repository != "" && (item.Branch != "" || detached) {
		probe.base()
	}
	if item.Base != "" {
		probe.merge()
	}
	probe.times()
}

// status는 바뀐 파일과 추적하지 않는 파일을 센다. git status는 stat이 바뀐 파일을 저장소가 정한 clean filter(git-lfs 같은)로
// 읽어 내용을 비교하므로, 검사가 저장소 설정의 program을 실행하고 느린 filter가 시간 한도를 다 쓴다. 그래서 filter를 부르지
// 않는 세 명령으로 센다. diff-files는 stat만 비교하므로 touch만 한 파일도 바뀐 것으로 센다. 이 오차는 정리 대상에서 빠지는
// 쪽으로만 난다.
func (probe *cleanWorktreeProbe) status() {
	item := probe.item
	failed, truncated := false, false
	names := func(args ...string) []string {
		output, code, ok := probe.run(args...)
		if !ok {
			failed = true
			return nil
		}
		if code != 0 {
			failed = true
			probe.fail(cleanReasonToolFailed, fmt.Sprintf("exit %d: %s", code, cleanFirstLine(output.Stderr)))
			return nil
		}
		lines := strings.Split(strings.TrimRight(output.Stdout, "\n"), "\n")
		if output.Truncated {
			// 마지막 줄은 중간에서 잘렸을 수 있다. 개수는 하한이고 변경이 있다는 사실은 유지한다.
			lines, truncated = lines[:len(lines)-1], true
		}
		return slices.DeleteFunc(lines, func(line string) bool { return line == "" })
	}
	// 커밋이 없는 저장소는 HEAD가 없고(종료 코드 1), index의 항목이 모두 staged다. HEAD를 읽지 못하면 staged는 세지 않는다.
	var staged []string
	output, code, ok := probe.run("rev-parse", "--verify", "--quiet", "HEAD")
	switch {
	case !ok:
		failed = true
	case code == 0:
		head := strings.TrimSpace(output.Stdout)
		item.Head = head[:min(len(head), 12)]
		staged = names("diff-index", "--cached", "--name-only", "HEAD")
	case code == 1:
		staged = names("ls-files")
	default:
		failed = true
		probe.fail(cleanReasonToolFailed, fmt.Sprintf("exit %d: %s", code, cleanFirstLine(output.Stderr)))
	}
	changed := map[string]bool{}
	for _, name := range append(staged, names("diff-files", "--name-only")...) {
		changed[name] = true
	}
	untracked := len(names("ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory"))
	if truncated {
		item.CountsLowerBound = true
		probe.item.Reasons = cleanAddReason(probe.item.Reasons, cleanReasonGitOutputTruncated)
	}
	changes := len(changed)
	switch {
	case !failed && !truncated, changes > 0 || untracked > 0:
		// 한 명령이 실패하거나 잘려도, 다른 명령이 확인한 변경은 변경이 있다는 사실로 남긴다.
		item.Changes, item.Untracked = &changes, &untracked
		item.CountsLowerBound = item.CountsLowerBound || failed
	case truncated && !failed:
		// 개수를 하나도 읽지 못했다. 변경 없음으로 보이면 안 된다.
		probe.fail(cleanReasonGitOutputTruncated, "")
	}
}

func (probe *cleanWorktreeProbe) register(lists map[string]*cleanWorktreeList) {
	item := probe.item
	list, cached := lists[item.Repository]
	if !cached {
		list = probe.list()
		lists[item.Repository] = list
	}
	if list.reason != "" {
		probe.fail(list.reason, list.detail)
		return
	}
	entry, found := list.entries[cleanWorktreeKey(item.Path)]
	registered := found
	item.Registered, item.Locked, item.Prunable = &registered, entry.locked, entry.prunable
}

// list는 이 worktree에서 저장소의 worktree list를 읽는다. 저장소의 어느 worktree에서 불러도 같은 답이다.
func (probe *cleanWorktreeProbe) list() *cleanWorktreeList {
	list := &cleanWorktreeList{entries: map[string]cleanWorktreeEntry{}}
	output, code, ok := probe.run("worktree", "list", "--porcelain")
	switch {
	case !ok:
		list.reason, list.detail = probe.lastReason, probe.item.Detail
		return list
	case code != 0:
		list.reason, list.detail = cleanReasonToolFailed, fmt.Sprintf("exit %d: %s", code, cleanFirstLine(output.Stderr))
	case output.Truncated:
		list.reason = cleanReasonGitOutputTruncated
	default:
		cleanWorktreeParseList(output.Stdout, list.entries)
	}
	return list
}

func cleanWorktreeKey(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func cleanWorktreeParseList(text string, entries map[string]cleanWorktreeEntry) {
	var path string
	var entry cleanWorktreeEntry
	flush := func() {
		if path != "" {
			entries[cleanWorktreeKey(path)] = entry
		}
		path, entry = "", cleanWorktreeEntry{}
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			path = strings.TrimPrefix(line, "worktree ")
		case line == "locked" || strings.HasPrefix(line, "locked "):
			entry.locked = true
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			entry.prunable = true
		}
	}
	flush()
}

// base는 비교할 기준 브랜치를 정한다. git-kit이 적은 부모(gk-parent), origin/HEAD 순서이고 둘 다 없으면 기준이 없다.
// 기준을 main으로 가정하면 develop에 병합된 브랜치를 미병합으로 잘못 본다.
func (probe *cleanWorktreeProbe) base() {
	item := probe.item
	var base, source string
	if item.Branch != "" {
		output, code, ok := probe.run("config", "--get", "branch."+item.Branch+".gk-parent")
		switch {
		case !ok:
			return
		case code == 0:
			base, source = strings.TrimSpace(output.Stdout), cleanBaseGKParent
		case code != 1:
			probe.fail(cleanReasonToolFailed, fmt.Sprintf("exit %d: %s", code, cleanFirstLine(output.Stderr)))
			return
		}
	}
	if source == "" {
		output, code, ok := probe.run("symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		switch {
		case !ok:
			return
		case code == 0:
			base, source = strings.TrimSpace(output.Stdout), cleanBaseOriginHead
		case code != 1 && code != 128:
			probe.fail(cleanReasonToolFailed, fmt.Sprintf("exit %d: %s", code, cleanFirstLine(output.Stderr)))
			return
		}
	}
	if source == "" {
		return
	}
	item.Base, item.BaseSource = base, source
	// 옵션으로 읽히는 이름은 git에 넘기지 않는다.
	if base == "" || strings.HasPrefix(base, "-") {
		item.Base = ""
		item.Reasons = cleanAddReason(item.Reasons, cleanReasonBaseMissing)
		return
	}
	_, code, ok := probe.run("rev-parse", "--verify", "--quiet", base+"^{commit}")
	switch {
	case !ok:
		item.Base = ""
	case code == 1:
		item.Base = ""
		item.Reasons = cleanAddReason(item.Reasons, cleanReasonBaseMissing)
	case code != 0:
		item.Base = ""
		probe.fail(cleanReasonToolFailed, fmt.Sprintf("rev-parse exit %d", code))
	}
}

func (probe *cleanWorktreeProbe) merge() {
	item := probe.item
	_, code, ok := probe.run("merge-base", "--is-ancestor", "HEAD", item.Base)
	if ok {
		switch code {
		case 0, 1:
			merged := code == 0
			item.Merged = &merged
		default:
			probe.fail(cleanReasonToolFailed, fmt.Sprintf("merge-base exit %d", code))
		}
	}
	if text, ok := probe.succeed("rev-list", "--count", item.Base+"..HEAD"); ok {
		if ahead, err := strconv.Atoi(text); err == nil && ahead >= 0 {
			item.Ahead = &ahead
		} else {
			probe.fail(cleanReasonInvalidOutput, cleanFirstLine(text))
		}
	}
}

var cleanReflogTime = regexp.MustCompile(`@\{(\d+)\}$`)

// times는 마지막 커밋 시각과 HEAD가 마지막으로 움직인 시각을 읽는다. 파일의 수정 시각은 읽지 않는다.
func (probe *cleanWorktreeProbe) times() {
	item := probe.item
	if text, ok := probe.succeed("log", "-1", "--format=%ct"); ok {
		if seconds, err := strconv.ParseInt(text, 10, 64); err == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			item.LastCommitAt = &at
		} else {
			probe.fail(cleanReasonInvalidOutput, cleanFirstLine(text))
		}
	}
	if text, ok := probe.succeed("reflog", "-1", "--date=unix", "--format=%gd"); ok && text != "" {
		match := cleanReflogTime.FindStringSubmatch(text)
		seconds, err := int64(0), errors.New("no match")
		if match != nil {
			seconds, err = strconv.ParseInt(match[1], 10, 64)
		}
		if err != nil {
			probe.fail(cleanReasonInvalidOutput, cleanFirstLine(text))
			return
		}
		at := time.Unix(seconds, 0).UTC()
		item.HeadMovedAt = &at
	}
}

func formatCleanWorktrees(section cleanWorktrees, verbose bool, width int, home string) string {
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(duFit(indent+text, width) + "\n") }
	switch section.Status {
	case cleanAbsent:
		builder.WriteString("\n" + cleanWrap(T("clean.worktree.fold_absent", cleanHomePath(section.Root, home)), width))
		return builder.String()
	case cleanMeasured, cleanPartial:
	default:
		line("\n", T("clean.worktree.state", cleanStateText(section.Status, section.Reasons)))
		if section.Detail != "" {
			line("  ", section.Detail)
		}
		return builder.String()
	}
	line("\n", T("clean.worktree.title", cleanHomePath(section.Root, home)))
	if section.Status == cleanPartial {
		line("  ", T("clean.worktree.state", cleanStateText(section.Status, section.Reasons)))
	}
	if len(section.Items) == 0 {
		line("  ", T("clean.worktree.none"))
	}
	counts, sizes, measured, lower := map[string]int{}, map[string]uint64{}, map[string]int{}, map[string]bool{}
	for _, item := range section.Items {
		counts[item.Verdict]++
		if item.UsageBytes == nil {
			lower[item.Verdict] = true
			continue
		}
		sizes[item.Verdict] += *item.UsageBytes
		measured[item.Verdict]++
		lower[item.Verdict] = lower[item.Verdict] || item.LowerBound
	}
	type summaryRow struct{ size, verdict, count string }
	var summary []summaryRow
	for _, verdict := range cleanWorktreeVerdicts {
		if counts[verdict] == 0 {
			continue
		}
		// 크기를 하나도 모르는 묶음은 0 B가 아니라 크기 없음으로 보인다.
		size := cleanNoSize
		if measured[verdict] > 0 {
			size = cleanSize(sizes[verdict], lower[verdict])
		}
		summary = append(summary, summaryRow{size, T("clean.worktree.verdict." + verdict), T("clean.worktree.count", counts[verdict])})
	}
	sorted := append([]cleanWorktree(nil), section.Items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := sorted[i].UsageBytes, sorted[j].UsageBytes
		switch {
		case left == nil || right == nil:
			return left != nil
		}
		return *left > *right
	})
	shown := sorted
	if !verbose && len(shown) > cleanWorktreeShown {
		shown = shown[:cleanWorktreeShown]
	}
	// 요약 줄과 worktree 줄은 크기와 판정 칸을 함께 맞춘다.
	var columns cleanWorktreeColumns
	var countWidth int
	for _, row := range summary {
		columns.size = max(columns.size, liveWidth(row.size))
		columns.verdict = max(columns.verdict, liveWidth(row.verdict))
		countWidth = max(countWidth, liveWidth(row.count))
	}
	for _, item := range shown {
		columns.size = max(columns.size, liveWidth(cleanWorktreeSize(item)))
		columns.verdict = max(columns.verdict, liveWidth(T("clean.worktree.verdict."+item.Verdict)))
	}
	for _, row := range summary {
		line("  ", cleanPadLeft(row.size, columns.size)+"  "+cleanPad(row.verdict, columns.verdict)+"  "+cleanPadLeft(row.count, countWidth))
	}
	for _, item := range shown {
		builder.WriteString(formatCleanWorktree(item, section.Root, verbose, width, columns, home))
	}
	if len(shown) < len(sorted) {
		line(strings.Repeat(" ", 2+columns.size+2), T("clean.group_more", len(sorted)-len(shown)))
	}
	if section.Leftover != nil {
		size := formatSizeShort(section.Leftover.UsageBytes)
		if section.LowerBound {
			size = T("clean.lower_bound", size)
		}
		line("  ", T("clean.worktree.leftover", size))
	}
	if section.Cleanup != "" {
		line("  ", T("clean.cleanup", section.Cleanup))
	}
	return builder.String()
}

type cleanWorktreeColumns struct {
	size    int
	verdict int
}

func cleanWorktreeSize(item cleanWorktree) string {
	if item.UsageBytes == nil {
		return cleanNoSize
	}
	return cleanSize(*item.UsageBytes, item.LowerBound)
}

func formatCleanWorktree(item cleanWorktree, root string, verbose bool, width int, columns cleanWorktreeColumns, home string) string {
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(duFit(indent+text, width) + "\n") }
	// 자세한 줄은 판정 칸보다 두 칸 안쪽에 둔다.
	detail := strings.Repeat(" ", 2+columns.size+4)
	name, err := filepath.Rel(root, item.Path)
	if err != nil {
		name = item.Path
	}
	if item.Branch != "" {
		name += " (" + item.Branch + ")"
	}
	text := cleanPadLeft(cleanWorktreeSize(item), columns.size) + "  " + cleanPad(T("clean.worktree.verdict."+item.Verdict), columns.verdict) + "  " + name
	if item.Changes != nil && item.Untracked != nil && (*item.Changes > 0 || *item.Untracked > 0) {
		counts := T("clean.worktree.counts", *item.Changes, *item.Untracked)
		if item.CountsLowerBound {
			counts = T("clean.lower_bound", counts)
		}
		text += "  " + counts
	}
	if item.BuildDirs != nil && *item.BuildDirs > 0 {
		text += "  " + T("clean.worktree.build_dirs", *item.BuildDirs)
	}
	line("  ", text)
	if !verbose {
		return builder.String()
	}
	if item.Base != "" {
		ahead := "-"
		if item.Ahead != nil {
			ahead = strconv.Itoa(*item.Ahead)
		}
		line(detail, T("clean.worktree.base", item.Base, T("clean.worktree.base_source."+item.BaseSource), ahead))
	}
	if item.LastCommitAt != nil || item.HeadMovedAt != nil {
		date := func(at *time.Time) string {
			if at == nil {
				return "-"
			}
			return at.Format("2006-01-02")
		}
		line(detail, T("clean.worktree.activity", date(item.LastCommitAt), date(item.HeadMovedAt)))
	}
	if len(item.Reasons) > 0 {
		reasons := make([]string, 0, len(item.Reasons))
		for _, reason := range item.Reasons {
			reasons = append(reasons, T("clean.reason."+reason))
		}
		line(detail, strings.Join(reasons, ", "))
	}
	if item.Detail != "" {
		line(detail, cleanHomePath(item.Detail, home))
	}
	return builder.String()
}
