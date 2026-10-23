package edc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// 프로젝트 빌드 폴더 섹션은 --project로 받은 루트에서 표지 파일이 있는 빌드 폴더만 찾아 잰다.
// 폴더 이름만으로는 소스 폴더와 구분되지 않으므로 표지가 없으면 재지 않고, 저장소 안이면 추적 파일이 없어야
// 재생성 가능으로 본다. git에는 읽기 하위 명령만 쓰고 cleanGit을 거친다.

const (
	cleanProjectDepth      = 6
	cleanProjectVisitLimit = 300000
	cleanProjectItemLimit  = 256
	cleanProjectShown      = 10
	// cleanProjectFlag는 fold_none 문장 안의 flag다. 세 언어의 문장이 모두 이 글자를 그대로 쓴다.
	cleanProjectFlag = "--project DIR"
)

const (
	cleanNoProjectRoot      = "no_project_root"
	cleanReasonNotDirectory = "not_directory"
	cleanReasonProjectLimit = "project_limit"
	cleanReasonTrackedFiles = "tracked_files"
	cleanReasonGitUnknown   = "git_unknown"
)

const (
	cleanGitNone       = "none"
	cleanGitIgnored    = "ignored"
	cleanGitNotIgnored = "not_ignored"
	cleanGitUnknown    = "unknown"
	// cleanGitInside는 JSON에 나가지 않는 중간 값이다. 저장소 안이라는 뜻이다.
	cleanGitInside = "repository"
)

// 상한은 시험이 줄여 쓴다.
var (
	cleanProjectDepthLimit = cleanProjectDepth
	cleanProjectVisits     = cleanProjectVisitLimit
	cleanProjectItems      = cleanProjectItemLimit
)

// cleanProjectKind는 빌드 폴더 종류다. markers 중 하나가 같은 부모에 있어야 한다.
type cleanProjectKind struct {
	id      string
	dir     string
	markers []string
}

var cleanProjectKinds = []cleanProjectKind{
	{"zig_cache", ".zig-cache", []string{"build.zig"}},
	{"zig_out", "zig-out", []string{"build.zig"}},
	{"cargo_target", "target", []string{"Cargo.toml"}},
	{"node_modules", "node_modules", []string{"package.json"}},
	{"swiftpm_build", ".build", []string{"Package.swift"}},
	{"gradle_build", "build", []string{"build.gradle", "build.gradle.kts"}},
}

// 번역 키를 조합하는 값의 전체 목록이다.
var cleanProjectGitStates = []string{cleanGitNone, cleanGitIgnored, cleanGitNotIgnored, cleanGitUnknown}

// cleanProjects는 반복할 수 있는 --project 값이다.
type cleanProjects []string

func (values *cleanProjects) String() string { return strings.Join(*values, ",") }

func (values *cleanProjects) Set(value string) error {
	// 빈 값은 filepath.Abs에서 현재 디렉터리가 되어 의도하지 않은 폴더를 걷게 된다.
	if value == "" {
		return errors.New(T("clean.error.project_empty"))
	}
	*values = append(*values, value)
	return nil
}

type cleanProjectRoot struct {
	Path          string   `json:"path"`
	ResolvedPath  string   `json:"resolved_path,omitempty"`
	Status        string   `json:"status"`
	Reasons       []string `json:"reasons,omitempty"`
	Detail        string   `json:"detail,omitempty"`
	SameAs        string   `json:"same_as,omitempty"`
	DirsVisited   *int     `json:"dirs_visited"`
	Truncated     bool     `json:"truncated,omitempty"`
	Unmarked      *int     `json:"unmarked"`
	SkippedMounts int      `json:"skipped_mounts,omitempty"`
	Unreadable    int      `json:"unreadable,omitempty"`
	device        uint64
}

// cleanBuildDir는 빌드 폴더 하나의 결과다. 알 수 없는 값은 0이 아니라 null이거나 없다.
type cleanBuildDir struct {
	Kind           string              `json:"kind"`
	Path           string              `json:"path"`
	ResolvedPath   string              `json:"resolved_path"`
	Project        string              `json:"project"`
	Marker         string              `json:"marker"`
	Git            string              `json:"git,omitempty"`
	TrackedFiles   *bool               `json:"tracked_files,omitempty"`
	Regenerable    bool                `json:"regenerable"`
	Status         string              `json:"status"`
	Reasons        []string            `json:"reasons,omitempty"`
	Detail         string              `json:"detail,omitempty"`
	UsageBytes     *uint64             `json:"usage_bytes,omitempty"`
	LowerBound     bool                `json:"lower_bound,omitempty"`
	Entries        int64               `json:"entries,omitempty"`
	MeasuredAt     *time.Time          `json:"measured_at,omitempty"`
	ElapsedMS      *int64              `json:"elapsed_ms,omitempty"`
	Filesystem     *cleanFilesystemRef `json:"filesystem,omitempty"`
	SameAs         string              `json:"same_as,omitempty"`
	Inside         string              `json:"inside,omitempty"`
	InsideWorktree string              `json:"inside_worktree,omitempty"`
}

func (dir cleanBuildDir) counted() bool {
	return dir.UsageBytes != nil && (dir.Status == cleanMeasured || dir.Status == cleanPartial)
}

// cleanProjectBuildDirs는 빌드 폴더 섹션이다. 재생성 가능하고 다른 곳에서 세지 않은 폴더만 파일 시스템 합계에 들어간다.
type cleanProjectBuildDirs struct {
	Status   string             `json:"status"`
	Reasons  []string           `json:"reasons,omitempty"`
	MaxDepth int                `json:"max_depth"`
	Roots    []cleanProjectRoot `json:"roots"`
	Items    []cleanBuildDir    `json:"items"`
}

// cleanProjectQuiet는 확인할 일이 없는 상태다. 루트를 주지 않은 것은 정상이다.
func cleanProjectQuiet(section cleanProjectBuildDirs) bool {
	return section.Status == cleanNoProjectRoot || section.Status == cleanMeasured
}

func cleanProjectKindOf(name string) (cleanProjectKind, bool) {
	for _, kind := range cleanProjectKinds {
		if kind.dir == name {
			return kind, true
		}
	}
	return cleanProjectKind{}, false
}

// cleanProjectScan은 루트마다 빌드 폴더를 찾고, 찾은 순서대로 git 근거를 읽고 잰다.
func cleanProjectScan(ctx context.Context, deps cleanDeps, projects []string, catalog []cleanItem) cleanProjectBuildDirs {
	section := cleanProjectBuildDirs{MaxDepth: cleanProjectDepthLimit, Roots: []cleanProjectRoot{}, Items: []cleanBuildDir{}}
	if len(projects) == 0 {
		section.Status = cleanNoProjectRoot
		return section
	}
	scanner := &cleanProjectScanner{ctx: ctx, deps: deps}
	// 측정하지 못한 후보의 폴더는 합계에 들어가지 않았으므로 기준으로 쓰지 않는다.
	for _, item := range catalog {
		if item.counted() {
			for _, root := range item.roots {
				scanner.catalog = append(scanner.catalog, cleanRoot{path: root, id: item.ID})
			}
		}
	}
	section.Roots = scanner.resolve(projects)
	for index := range section.Roots {
		done := deps.begin("discover", section.Roots[index].Path)
		scanner.walkRoot(&section.Roots[index])
		done()
	}
	section.Items = scanner.measure()
	cleanProjectSummarize(&section)
	return section
}

type cleanProjectScanner struct {
	ctx     context.Context
	deps    cleanDeps
	catalog []cleanRoot
	found   []cleanBuildDir
	visits  int
	limited bool
	stopped bool
	// 아래는 걷는 중인 루트의 값이다.
	device     uint64
	visited    int
	unmarked   int
	mounts     int
	unreadable int
}

// resolve는 루트를 실제 경로로 풀고 걸을 수 없는 루트의 상태를 정한다. 같은 루트와 다른 루트 안의 루트는 한 번만 걷는다.
func (scanner *cleanProjectScanner) resolve(projects []string) []cleanProjectRoot {
	roots := make([]cleanProjectRoot, len(projects))
	walkable := make([]bool, len(projects))
	for index, given := range projects {
		root := &roots[index]
		root.Path = given
		if absolute, err := filepath.Abs(given); err == nil {
			root.Path = absolute
		}
		resolved, err := scanner.deps.resolvePath(given)
		if err != nil {
			root.Status, root.Reasons, root.Detail = cleanFailure(err)
			continue
		}
		root.ResolvedPath = resolved
		info, err := os.Stat(resolved)
		if err != nil {
			root.Status, root.Reasons, root.Detail = cleanFailure(err)
			continue
		}
		if !info.IsDir() {
			root.Status, root.Reasons = cleanRejected, []string{cleanReasonNotDirectory}
			continue
		}
		device, err := scanner.deps.deviceOf(resolved)
		if err != nil {
			root.Status, root.Reasons, root.Detail = cleanFailure(err)
			continue
		}
		root.device, walkable[index] = device, true
	}
	for index := range roots {
		if !walkable[index] {
			continue
		}
		for other := range roots {
			if other == index || !walkable[other] {
				continue
			}
			same := roots[other].ResolvedPath == roots[index].ResolvedPath
			if (same && other < index) || (!same && pathUnder(roots[index].ResolvedPath, roots[other].ResolvedPath)) {
				roots[index].Status, roots[index].Reasons, roots[index].SameAs = cleanDuplicate, []string{cleanReasonSameAs}, roots[other].Path
				break
			}
		}
	}
	return roots
}

func (scanner *cleanProjectScanner) walkRoot(root *cleanProjectRoot) {
	if root.Status != "" {
		return
	}
	switch {
	case scanner.ctx.Err() != nil:
		root.Status, root.Reasons = cleanNotScanned, []string{cleanContextReason(scanner.ctx.Err())}
		return
	case scanner.limited:
		root.Status, root.Reasons = cleanNotScanned, []string{cleanReasonProjectLimit}
		return
	}
	scanner.device, scanner.visited, scanner.unmarked, scanner.mounts, scanner.unreadable = root.device, 0, 0, 0, 0
	if err := scanner.visit(root, root.ResolvedPath, 0); err != nil {
		root.Status, root.Reasons, root.Detail = cleanFailure(err)
		return
	}
	visited, unmarked := scanner.visited, scanner.unmarked
	root.DirsVisited, root.Unmarked = &visited, &unmarked
	root.SkippedMounts, root.Unreadable = scanner.mounts, scanner.unreadable
	root.Status = cleanMeasured
	if scanner.stopped {
		root.Reasons = append(root.Reasons, cleanContextReason(scanner.ctx.Err()))
	}
	if scanner.limited {
		root.Truncated = true
		root.Reasons = append(root.Reasons, cleanReasonProjectLimit)
	}
	if scanner.unreadable > 0 {
		root.Reasons = append(root.Reasons, cleanReasonUnreadableDirs)
	}
	if scanner.mounts > 0 {
		root.Reasons = append(root.Reasons, cleanReasonSkippedMounts)
	}
	if len(root.Reasons) > 0 {
		root.Status = cleanPartial
	}
}

// visit은 디렉터리 하나를 읽고 이름이 맞는 빌드 폴더를 모은다. 빌드 폴더와 .git 아래로는 내려가지 않고,
// symlink는 따르지 않는다. 읽지 못한 오류는 호출한 쪽이 센다.
func (scanner *cleanProjectScanner) visit(root *cleanProjectRoot, dir string, depth int) error {
	if err := scanner.ctx.Err(); err != nil {
		scanner.stopped = true
		return nil
	}
	if scanner.visits >= cleanProjectVisits {
		scanner.limited = true
		return nil
	}
	scanner.visits++
	scanner.visited++
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			files[entry.Name()] = true
		}
	}
	for _, entry := range entries {
		if scanner.stopped || scanner.limited {
			return nil
		}
		if !entry.IsDir() || entry.Name() == ".git" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		device, err := scanner.deps.deviceOf(path)
		if err != nil {
			scanner.skip(err)
			continue
		}
		if device != scanner.device {
			scanner.mounts++
			continue
		}
		// 표지가 없는 같은 이름의 폴더는 소스 폴더일 수 있으므로, 재지 않고 다른 폴더처럼 안으로 들어간다.
		if kind, named := cleanProjectKindOf(entry.Name()); named && scanner.candidate(root, path, kind, files) {
			continue
		}
		if scanner.underCatalog(path) || depth+1 >= cleanProjectDepthLimit {
			continue
		}
		if err := scanner.visit(root, path, depth+1); err != nil {
			scanner.skip(err)
		}
	}
	return nil
}

// skip은 읽지 못한 디렉터리를 센다. 목록을 만든 뒤 사라진 항목은 건너뛴다.
func (scanner *cleanProjectScanner) skip(err error) {
	if !errors.Is(err, fs.ErrNotExist) {
		scanner.unreadable++
	}
}

func (scanner *cleanProjectScanner) underCatalog(path string) bool {
	return slices.ContainsFunc(scanner.catalog, func(root cleanRoot) bool { return pathUnder(path, root.path) })
}

// candidate는 표지가 있는 폴더만 모으고, 모았는지를 돌려준다. 표지가 없으면 소스 폴더일 수 있으므로 재지 않고 센다.
func (scanner *cleanProjectScanner) candidate(root *cleanProjectRoot, path string, kind cleanProjectKind, files map[string]bool) bool {
	marker := ""
	for _, name := range kind.markers {
		if files[name] {
			marker = name
			break
		}
	}
	if marker == "" {
		scanner.unmarked++
		return false
	}
	if len(scanner.found) >= cleanProjectItems {
		scanner.limited = true
		return true
	}
	relative, err := filepath.Rel(root.ResolvedPath, path)
	if err != nil {
		relative = path
	}
	reported := filepath.Join(root.Path, relative)
	scanner.found = append(scanner.found, cleanBuildDir{Kind: kind.id, Path: reported, ResolvedPath: path,
		Project: filepath.Dir(reported), Marker: marker})
	return true
}

// measure는 찾은 순서대로 git 근거를 읽고 잰다.
func (scanner *cleanProjectScanner) measure() []cleanBuildDir {
	dirs := scanner.found
	if dirs == nil {
		return []cleanBuildDir{}
	}
	repositories := map[string]string{}
	for index := range dirs {
		dir := &dirs[index]
		if err := scanner.ctx.Err(); err != nil {
			dir.Status, dir.Reasons = cleanNotScanned, []string{cleanContextReason(err)}
			continue
		}
		duplicate := false
		for _, root := range scanner.catalog {
			switch {
			case root.path == dir.ResolvedPath:
				dir.Status, dir.Reasons, dir.SameAs = cleanDuplicate, []string{cleanReasonSameAs}, root.id
				duplicate = true
			case dir.Inside == "" && pathUnder(dir.ResolvedPath, root.path):
				dir.Inside = root.id
			}
			if duplicate {
				break
			}
		}
		if duplicate {
			dir.Inside = ""
			continue
		}
		keep := scanner.classify(dir, repositories)
		if err := scanner.ctx.Err(); err != nil {
			dir.Status, dir.Reasons = cleanNotScanned, []string{cleanContextReason(err)}
			continue
		}
		measurement, err := cleanMeasurePath(scanner.ctx, scanner.deps, dir.ResolvedPath)
		if err != nil {
			dir.Status, dir.Reasons, dir.Detail = cleanFailure(err)
		} else {
			at, elapsed := measurement.measuredAt, measurement.elapsedMS
			dir.MeasuredAt, dir.ElapsedMS, dir.Filesystem = &at, &elapsed, measurement.filesystem
			dir.Status, dir.Reasons, dir.LowerBound = measurement.status, measurement.reasons, measurement.lowerBound
			dir.UsageBytes, dir.Entries = measurement.usage, measurement.entries
		}
		dir.Reasons = append(dir.Reasons, keep...)
	}
	return dirs
}

// classify는 git 근거로 재생성 가능 여부를 정하고 재생성 가능하지 않은 이유를 낸다.
// 저장소 밖은 표지만으로 재생성 가능이다. 판정하지 못하면 재생성 가능이라고 하지 않는다.
func (scanner *cleanProjectScanner) classify(dir *cleanBuildDir, repositories map[string]string) []string {
	project, name := filepath.Dir(dir.ResolvedPath), filepath.Base(dir.ResolvedPath)
	repository, cached := repositories[project]
	if !cached {
		repository = scanner.repository(dir, project)
		repositories[project] = repository
	}
	switch repository {
	case cleanGitNone:
		dir.Git, dir.Regenerable = cleanGitNone, true
		return nil
	case cleanGitUnknown:
		dir.Git = cleanGitUnknown
		return []string{cleanReasonGitUnknown}
	}
	dir.Git = cleanGitUnknown
	code, ok := scanner.git(dir, project, "check-ignore", "-q", "--", name)
	switch {
	case ok && code == 0:
		dir.Git = cleanGitIgnored
	case ok && code == 1:
		dir.Git = cleanGitNotIgnored
	default:
		return []string{cleanReasonGitUnknown}
	}
	code, ok = scanner.git(dir, project, "ls-files", "--error-unmatch", "--", name)
	if !ok || code > 1 {
		dir.Git = cleanGitUnknown
		return []string{cleanReasonGitUnknown}
	}
	tracked := code == 0
	dir.TrackedFiles = &tracked
	if tracked {
		return []string{cleanReasonTrackedFiles}
	}
	dir.Regenerable = true
	return nil
}

// repository는 폴더가 git 저장소 안인지 본다. 종료 코드 128과 'not a git repository'일 때만 저장소 밖이다.
// 소유자 불일치나 시간 초과 같은 실패는 저장소 밖으로 읽지 않는다.
func (scanner *cleanProjectScanner) repository(dir *cleanBuildDir, project string) string {
	output := cleanGit(scanner.ctx, scanner.deps, project, "rev-parse", "--show-toplevel")
	switch {
	case output.Err == nil && !output.Truncated && filepath.IsAbs(strings.TrimSpace(output.Stdout)):
		return cleanGitInside
	case output.Err != nil && output.ExitCode == 128 && scanner.ctx.Err() == nil && strings.Contains(output.Stderr, "not a git repository"):
		return cleanGitNone
	}
	if dir.Detail == "" {
		dir.Detail = cleanProjectGitDetail(output)
	}
	return cleanGitUnknown
}

// git은 종료 코드가 뜻을 가지는 명령을 실행한다. 실행하지 못했거나 시간 초과, 중단이면 ok가 false다.
func (scanner *cleanProjectScanner) git(dir *cleanBuildDir, project string, args ...string) (code int, ok bool) {
	output := cleanGit(scanner.ctx, scanner.deps, project, args...)
	switch {
	case output.Err == nil:
		return 0, true
	case scanner.ctx.Err() == nil && !errors.Is(output.Err, errCleanToolTimeout) && output.ExitCode > 0:
		if output.ExitCode > 1 && dir.Detail == "" {
			dir.Detail = cleanProjectGitDetail(output)
		}
		return output.ExitCode, true
	}
	if dir.Detail == "" {
		dir.Detail = cleanProjectGitDetail(output)
	}
	return 0, false
}

func cleanProjectGitDetail(output cleanToolOutput) string {
	if line := cleanFirstLine(output.Stderr); line != "" {
		return line
	}
	if output.Err != nil {
		return output.Err.Error()
	}
	return ""
}

// cleanProjectSummarize는 루트와 폴더의 상태를 섹션 상태 하나로 합친다. 걷지 못한 루트나 재지 못한 폴더가 있으면 partial이다.
func cleanProjectSummarize(section *cleanProjectBuildDirs) {
	walked, scanned, incomplete := 0, 0, false
	var reasons []string
	for _, root := range section.Roots {
		if root.Status == cleanMeasured || root.Status == cleanPartial {
			walked++
		}
		if root.Status != cleanNotScanned {
			scanned++
		}
		if root.Status != cleanMeasured && root.Status != cleanDuplicate {
			incomplete = true
			for _, reason := range root.Reasons {
				reasons = cleanAddReason(reasons, reason)
			}
		}
	}
	for _, dir := range section.Items {
		if dir.Status == cleanMeasured || dir.Status == cleanDuplicate || dir.Status == cleanAbsent {
			continue
		}
		incomplete = true
		for _, reason := range dir.Reasons {
			if reason != cleanReasonTrackedFiles && reason != cleanReasonGitUnknown {
				reasons = cleanAddReason(reasons, reason)
			}
		}
	}
	section.Reasons = reasons
	switch {
	case walked == 0 && scanned == 0:
		section.Status = cleanNotScanned
	case walked == 0:
		section.Status = cleanFailed
	case incomplete:
		section.Status = cleanPartial
	default:
		section.Status = cleanMeasured
	}
}

// cleanLinkWorktrees는 worktree 안의 빌드 폴더를 서로 표시한다. worktree는 합계에 넣지 않으므로 폴더를 두 번 세지 않고,
// 개수는 빌드 폴더를 찾은 경우에만 낸다.
func cleanLinkWorktrees(projects *cleanProjectBuildDirs, worktrees *cleanWorktrees) {
	if projects.Status != cleanMeasured && projects.Status != cleanPartial {
		return
	}
	for index := range worktrees.Items {
		count := 0
		worktrees.Items[index].BuildDirs = &count
	}
	for index := range projects.Items {
		dir := &projects.Items[index]
		for position := range worktrees.Items {
			item := &worktrees.Items[position]
			if pathUnder(dir.ResolvedPath, item.Path) {
				dir.InsideWorktree = item.Path
				*item.BuildDirs++
				break
			}
		}
	}
}

func formatCleanProject(section cleanProjectBuildDirs, verbose bool, width int, home string, style cleanStyle) string {
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(style.row(indent, text, width)) }
	switch section.Status {
	case cleanNoProjectRoot:
		return "\n" + cleanProjectFoldNone(width, style)
	case cleanMeasured, cleanPartial:
		builder.WriteString(style.open(T("clean.project.title"), T("clean.project.panel"), T("clean.project.note"), width))
		if section.Status == cleanPartial {
			line("  ", style.warn(T("clean.project.state", cleanStateText(section.Status, section.Reasons))))
		}
	default:
		state := cleanStateText(section.Status, section.Reasons)
		builder.WriteString(style.open(T("clean.project.state", state), T("clean.project.panel"), state, width))
	}
	for _, root := range section.Roots {
		if root.DirsVisited == nil {
			state := cleanStateText(root.Status, root.Reasons)
			if root.SameAs != "" {
				state = T("clean.duplicate", T("clean.status."+root.Status), root.SameAs)
			}
			line("  ", T("clean.project.root_state", cleanHomePath(root.Path, home), style.state(state, root.Status)))
			if root.Detail != "" {
				line("    ", style.dim(root.Detail))
			}
			continue
		}
		line("  ", style.dim(T("clean.project.root", cleanHomePath(root.Path, home), *root.DirsVisited, *root.Unmarked)))
		if root.Status == cleanPartial {
			line("    ", style.warn(cleanStateText(root.Status, root.Reasons)))
		}
		if root.Unreadable > 0 {
			line("    ", style.warn(T("clean.unreadable", root.Unreadable)))
		}
		if root.SkippedMounts > 0 {
			line("    ", style.dim(T("clean.skipped_mounts", root.SkippedMounts)))
		}
	}
	if section.Status != cleanMeasured && section.Status != cleanPartial {
		return builder.String() + style.close()
	}
	if len(section.Items) == 0 {
		line("  ", style.dim(T("clean.project.none")))
		return builder.String() + style.close()
	}
	sorted := append([]cleanBuildDir(nil), section.Items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := sorted[i], sorted[j]
		if left.counted() != right.counted() {
			return left.counted()
		}
		return left.counted() && *left.UsageBytes > *right.UsageBytes
	})
	shown := sorted
	if !verbose && len(shown) > cleanProjectShown {
		shown = shown[:cleanProjectShown]
	}
	var columns cleanBuildDirColumns
	for _, dir := range shown {
		columns.size = max(columns.size, liveWidth(cleanBuildDirSize(dir)))
		columns.kind = max(columns.kind, liveWidth(T("clean.project.kind."+dir.Kind)))
	}
	for _, dir := range shown {
		builder.WriteString(formatCleanBuildDir(dir, width, columns, home, style))
	}
	if len(shown) < len(sorted) {
		line(strings.Repeat(" ", 2+columns.size+2), style.dim(T("clean.group_more", len(sorted)-len(shown))))
	}
	return builder.String() + style.close()
}

// cleanProjectFoldNone은 --project를 주지 않았다는 줄이다. 색을 쓰면 문장 안의 flag를 chip으로 보인다.
func cleanProjectFoldNone(width int, style cleanStyle) string {
	text := T("clean.project.fold_none")
	before, after, found := strings.Cut(text, cleanProjectFlag)
	if !bool(style) || !found {
		return style.wrap(text, width)
	}
	return cleanWrap(richDim(before)+richChip(cleanProjectFlag)+richDim(after), width)
}

type cleanBuildDirColumns struct {
	size int
	kind int
}

func cleanBuildDirSize(dir cleanBuildDir) string {
	if dir.counted() {
		return cleanSize(*dir.UsageBytes, dir.LowerBound)
	}
	return cleanNoSize
}

func formatCleanBuildDir(dir cleanBuildDir, width int, columns cleanBuildDirColumns, home string, style cleanStyle) string {
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(style.row(indent, text, width)) }
	place := style.dim(cleanHomePath(dir.Path, home))
	var bytes uint64
	switch {
	case dir.counted():
		bytes = *dir.UsageBytes
	case dir.Status == cleanDuplicate:
		place = style.dim(T("clean.duplicate", T("clean.status.duplicate"), dir.SameAs)) + "  " + place
	default:
		place = style.state(cleanStateText(dir.Status, dir.Reasons), dir.Status) + "  " + place
	}
	line("  ", style.size(cleanPadLeft(cleanBuildDirSize(dir), columns.size), bytes, true)+"  "+cleanPad(T("clean.project.kind."+dir.Kind), columns.kind)+"  "+place)
	if dir.Git == "" {
		return builder.String()
	}
	// 자세한 줄은 종류 칸보다 두 칸 안쪽에 둔다.
	detail := strings.Repeat(" ", 2+columns.size+4)
	verdict := T("clean.project.recreatable")
	if !dir.Regenerable {
		for _, reason := range dir.Reasons {
			if reason == cleanReasonTrackedFiles || reason == cleanReasonGitUnknown {
				verdict = T("clean.project.kept", T("clean.reason."+reason))
				break
			}
		}
	}
	line(detail, style.dim(T("clean.project.detail", dir.Marker, T("clean.project.git."+dir.Git), verdict)))
	if dir.Inside != "" {
		line(detail, style.dim(T("clean.inside", dir.Inside)))
	}
	if dir.InsideWorktree != "" {
		line(detail, style.dim(T("clean.project.in_worktree", cleanHomePath(dir.InsideWorktree, home))))
	}
	return builder.String()
}
