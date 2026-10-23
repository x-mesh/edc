package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// cleanProjectNoGit는 모든 폴더를 git 저장소 밖으로 답한다.
func cleanProjectNoGit(context.Context, cleanToolSpec) cleanToolOutput {
	return cleanToolOutput{ExitCode: 128, Stderr: "fatal: not a git repository (or any of the parent directories): .git", Err: errors.New("exit status 128")}
}

func cleanProjectDeps(t *testing.T, home string, run func(context.Context, cleanToolSpec) cleanToolOutput) cleanDeps {
	t.Helper()
	if run == nil {
		run = cleanProjectNoGit
	}
	return cleanTestDeps(home, run)
}

// cleanProjectGitLab은 실제 git으로 판정하는 시험을 위해 임시 홈과 환경을 만든다. 홈 위의 저장소가 섞이지 않게 막는다.
func cleanProjectGitLab(t *testing.T) (string, cleanDeps) {
	t.Helper()
	cleanGitEnvironment(t)
	home := cleanTestHome(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	return home, cleanProjectDeps(t, home, runCleanTool)
}

func cleanProjectTouch(t *testing.T, path string) {
	t.Helper()
	cleanWriteFile(t, path, 0)
}

func cleanProjectByPath(t *testing.T, section cleanProjectBuildDirs) map[string]cleanBuildDir {
	t.Helper()
	byPath := map[string]cleanBuildDir{}
	for _, dir := range section.Items {
		byPath[dir.Path] = dir
	}
	return byPath
}

func cleanProjectPaths(section cleanProjectBuildDirs) []string {
	var paths []string
	for _, dir := range section.Items {
		paths = append(paths, dir.Path)
	}
	sort.Strings(paths)
	return paths
}

func TestCleanProjectFlagRepeatsAndAnEmptyValueIsRejected(t *testing.T) {
	home := cleanTestHome(t)
	first, second := filepath.Join(home, "a"), filepath.Join(home, "absent")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.json")
	var output bytes.Buffer
	if code := runCleanWith([]string{"--project", first, "--project", second, "--json", path}, "test", cleanRunDeps(t, home), true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Status  string `json:"status"`
			Metrics struct {
				Project cleanJSONProject `json:"project_build_dirs"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	project := report.Results[0].Metrics.Project
	if len(project.Roots) != 2 || project.Roots[0].Status != cleanMeasured || project.Roots[1].Status != cleanAbsent {
		t.Fatalf("roots = %+v", project.Roots)
	}
	if project.Status != cleanPartial || report.Results[0].Status != string(StatusWarn) {
		t.Fatalf("a missing root must not look complete: section %q, result %q", project.Status, report.Results[0].Status)
	}

	var rejected bytes.Buffer
	if code := runCleanWith([]string{"--project", ""}, "test", cleanRunDeps(t, home), true, nil, &rejected); code != 2 {
		t.Fatalf("an empty --project exits %d, want 2", code)
	}
	if rejected.Len() != 0 {
		t.Fatalf("output = %q", rejected.String())
	}
}

type cleanJSONProject struct {
	Status string `json:"status"`
	Roots  []struct {
		Status string `json:"status"`
	} `json:"roots"`
	Items []map[string]interface{} `json:"items"`
}

func TestCleanProjectWithoutARootDoesNotSearch(t *testing.T) {
	home := cleanTestHome(t)
	path := filepath.Join(t.TempDir(), "out.json")
	var output bytes.Buffer
	if code := runCleanWith([]string{"--json", path}, "test", cleanRunDeps(t, home), true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Status  string `json:"status"`
			Metrics struct {
				Project json.RawMessage `json:"project_build_dirs"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	var compact bytes.Buffer
	if err := json.Compact(&compact, result.Metrics.Project); err != nil {
		t.Fatal(err)
	}
	if want := `{"status":"no_project_root","max_depth":6,"roots":[],"items":[]}`; compact.String() != want {
		t.Fatalf("project_build_dirs = %s", compact.String())
	}
	if result.Status != string(StatusPass) {
		t.Fatalf("no root is a quiet state, status = %q", result.Status)
	}
	var text bytes.Buffer
	if code := runCleanWith(nil, "test", cleanRunDeps(t, home), true, nil, &text); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(text.String(), "add --project DIR") {
		t.Fatalf("text does not say how to search:\n%s", text.String())
	}
	if len(cleanNotMeasured) != 7 {
		t.Fatalf("not measured = %d items, want 7", len(cleanNotMeasured))
	}
	for _, item := range cleanNotMeasured {
		if item.ID == "project_build_dirs" {
			t.Fatal("project build folders are still listed as not measured")
		}
	}
}

func TestCleanProjectRootsAreWalkedOnce(t *testing.T) {
	home := cleanTestHome(t)
	outer := filepath.Join(home, "outer")
	cleanProjectTouch(t, filepath.Join(outer, "proj", "package.json"))
	cleanWriteFile(t, filepath.Join(outer, "proj", "node_modules", "f"), 4<<10)
	file := filepath.Join(home, "file")
	cleanProjectTouch(t, file)
	recorder := &cleanRecorder{}
	deps := cleanProjectDeps(t, home, nil)
	deps.measure = recorder.measure
	projects := []string{outer, filepath.Join(outer, "proj"), outer, file, filepath.Join(home, "absent")}
	section := cleanProjectScan(context.Background(), deps, projects, nil)
	statuses := make([]string, len(section.Roots))
	for index, root := range section.Roots {
		statuses[index] = root.Status
	}
	// 같은 루트와 다른 루트 안의 루트는 한 번만 걷는다. 걷는 루트는 가장 바깥의 첫 루트 하나다.
	want := []string{cleanMeasured, cleanDuplicate, cleanDuplicate, cleanRejected, cleanAbsent}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("root statuses = %v, want %v", statuses, want)
	}
	if section.Roots[1].SameAs != outer || section.Roots[2].SameAs != outer {
		t.Fatalf("same_as = %q, %q", section.Roots[1].SameAs, section.Roots[2].SameAs)
	}
	if !reflect.DeepEqual(section.Roots[3].Reasons, []string{cleanReasonNotDirectory}) {
		t.Fatalf("file root = %+v", section.Roots[3])
	}
	if section.Roots[1].DirsVisited != nil || section.Roots[4].DirsVisited != nil {
		t.Fatal("a root that was not walked carries a visit count")
	}
	if len(section.Items) != 1 || recorder.touching(filepath.Join(outer, "proj", "node_modules")) != 1 {
		t.Fatalf("items = %+v, measure calls = %d", section.Items, recorder.touching(filepath.Join(outer, "proj", "node_modules")))
	}
	if section.Status != cleanPartial {
		t.Fatalf("a rejected and a missing root leave the section %q", section.Status)
	}
}

// 이름만 맞고 표지가 없는 폴더는 재지 않지만, 그 안의 project는 찾는다(#267).
func TestCleanProjectLooksInsideAnUnmarkedBuildFolder(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	cleanProjectTouch(t, filepath.Join(root, "build", "inner", "Cargo.toml"))
	cleanWriteFile(t, filepath.Join(root, "build", "inner", "target", "f"), 4<<10)
	recorder := &cleanRecorder{}
	deps := cleanProjectDeps(t, home, nil)
	deps.measure = recorder.measure
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	byPath := cleanProjectByPath(t, section)
	if dir, ok := byPath[filepath.Join(root, "build", "inner", "target")]; !ok || dir.Kind != "cargo_target" || dir.Marker != "Cargo.toml" {
		t.Fatalf("items = %v", cleanProjectPaths(section))
	}
	if section.Roots[0].Unmarked == nil || *section.Roots[0].Unmarked != 1 {
		t.Fatalf("unmarked = %v, want 1", section.Roots[0].Unmarked)
	}
	if build, target := recorder.touching(filepath.Join(root, "build")), recorder.touching(filepath.Join(root, "build", "inner", "target")); build != 0 || target != 1 {
		t.Fatalf("build was measured %d times and its target %d times, want 0 and 1", build, target)
	}
}

func TestCleanProjectMarkerTable(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	tests := []struct {
		project, dir, marker, kind string
	}{
		{"zig", ".zig-cache", "build.zig", "zig_cache"},
		{"zigout", "zig-out", "build.zig", "zig_out"},
		{"rust", "target", "Cargo.toml", "cargo_target"},
		{"node", "node_modules", "package.json", "node_modules"},
		{"swift", ".build", "Package.swift", "swiftpm_build"},
		{"gradle", "build", "build.gradle", "gradle_build"},
		{"kotlin", "build", "build.gradle.kts", "gradle_build"},
	}
	for _, test := range tests {
		cleanProjectTouch(t, filepath.Join(root, test.project, test.marker))
		cleanWriteFile(t, filepath.Join(root, test.project, test.dir, "f"), 4<<10)
	}
	// 이름만 맞는 폴더는 소스 폴더일 수 있다. 표지가 없으면 재지 않고 센다.
	for _, bare := range []string{"zig/target", "rust/build", "node/.build", "swift/node_modules", "gradle/zig-out", "kotlin/.zig-cache"} {
		cleanWriteFile(t, filepath.Join(root, bare, "f"), 4<<10)
	}
	// .git 안과 찾은 빌드 폴더 안은 보지 않는다.
	cleanProjectTouch(t, filepath.Join(root, "zig", ".git", "inner", "package.json"))
	cleanWriteFile(t, filepath.Join(root, "zig", ".git", "inner", "node_modules", "f"), 4<<10)
	cleanProjectTouch(t, filepath.Join(root, "node", "node_modules", "dep", "package.json"))
	cleanWriteFile(t, filepath.Join(root, "node", "node_modules", "dep", "node_modules", "f"), 4<<10)
	recorder := &cleanRecorder{}
	deps := cleanProjectDeps(t, home, nil)
	deps.measure = recorder.measure
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	byPath := cleanProjectByPath(t, section)
	if len(section.Items) != len(tests) {
		t.Fatalf("items = %v, want %d", cleanProjectPaths(section), len(tests))
	}
	for _, test := range tests {
		dir, ok := byPath[filepath.Join(root, test.project, test.dir)]
		if !ok || dir.Kind != test.kind || dir.Marker != test.marker || dir.Status != cleanMeasured || dir.UsageBytes == nil {
			t.Errorf("%s/%s = %+v", test.project, test.dir, dir)
		}
	}
	if section.Roots[0].Unmarked == nil || *section.Roots[0].Unmarked != 6 {
		t.Fatalf("unmarked = %v, want 6", section.Roots[0].Unmarked)
	}
	for _, bare := range []string{"zig/target", "rust/build", "zig/.git"} {
		if calls := recorder.touching(filepath.Join(root, bare)); calls != 0 {
			t.Errorf("%s was measured %d times", bare, calls)
		}
	}
	if section.Status != cleanMeasured {
		t.Fatalf("status = %q", section.Status)
	}
}

func TestCleanProjectDoesNotFollowSymlinks(t *testing.T) {
	home := cleanTestHome(t)
	outside := filepath.Join(home, "outside")
	cleanProjectTouch(t, filepath.Join(outside, "proj", "package.json"))
	cleanWriteFile(t, filepath.Join(outside, "proj", "node_modules", "f"), 4<<10)
	root := filepath.Join(home, "root")
	if err := os.MkdirAll(filepath.Join(root, "own"), 0o755); err != nil {
		t.Fatal(err)
	}
	cleanProjectTouch(t, filepath.Join(root, "own", "package.json"))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// 빌드 폴더 이름의 symlink도 폴더로 보지 않는다.
	if err := os.Symlink(filepath.Join(outside, "proj", "node_modules"), filepath.Join(root, "own", "node_modules")); err != nil {
		t.Fatal(err)
	}
	section := cleanProjectScan(context.Background(), cleanProjectDeps(t, home, nil), []string{root}, nil)
	if len(section.Items) != 0 || section.Status != cleanMeasured {
		t.Fatalf("section = %+v", section)
	}
}

func TestCleanProjectSkipsOtherDevices(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	for _, name := range []string{"mnt", "ok"} {
		cleanProjectTouch(t, filepath.Join(root, name, "proj", "package.json"))
		cleanWriteFile(t, filepath.Join(root, name, "proj", "node_modules", "f"), 4<<10)
	}
	deps := cleanProjectDeps(t, home, nil)
	deps.deviceOf = func(path string) (uint64, error) {
		if pathUnder(path, filepath.Join(root, "mnt")) {
			return 2, nil
		}
		return 1, nil
	}
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	if want := []string{filepath.Join(root, "ok", "proj", "node_modules")}; !reflect.DeepEqual(cleanProjectPaths(section), want) {
		t.Fatalf("items = %v", cleanProjectPaths(section))
	}
	got := section.Roots[0]
	if got.SkippedMounts != 1 || got.Status != cleanPartial || !reflect.DeepEqual(got.Reasons, []string{cleanReasonSkippedMounts}) {
		t.Fatalf("root = %+v", got)
	}
}

func TestCleanProjectLimits(t *testing.T) {
	build := func(t *testing.T, root string, names ...string) {
		for _, name := range names {
			cleanProjectTouch(t, filepath.Join(root, name, "package.json"))
			cleanWriteFile(t, filepath.Join(root, name, "node_modules", "f"), 1<<10)
		}
	}
	t.Run("depth", func(t *testing.T) {
		home := cleanTestHome(t)
		root := filepath.Join(home, "root")
		six := filepath.Join(root, "a", "b", "c", "d", "e")
		seven := filepath.Join(root, "a", "b", "c", "d", "e", "f")
		build(t, six, ".")
		build(t, seven, ".")
		section := cleanProjectScan(context.Background(), cleanProjectDeps(t, home, nil), []string{root}, nil)
		if want := []string{filepath.Join(six, "node_modules")}; !reflect.DeepEqual(cleanProjectPaths(section), want) {
			t.Fatalf("items = %v, want only the depth 6 folder", cleanProjectPaths(section))
		}
	})
	t.Run("visits", func(t *testing.T) {
		home := cleanTestHome(t)
		root := filepath.Join(home, "root")
		build(t, root, "a", "b", "c", "d")
		saved := cleanProjectVisits
		cleanProjectVisits = 3
		defer func() { cleanProjectVisits = saved }()
		section := cleanProjectScan(context.Background(), cleanProjectDeps(t, home, nil), []string{root}, nil)
		got := section.Roots[0]
		if !got.Truncated || got.Status != cleanPartial || !reflect.DeepEqual(got.Reasons, []string{cleanReasonProjectLimit}) || *got.DirsVisited != 3 {
			t.Fatalf("root = %+v", got)
		}
		if section.Status != cleanPartial || !reflect.DeepEqual(section.Reasons, []string{cleanReasonProjectLimit}) {
			t.Fatalf("section = %q %v", section.Status, section.Reasons)
		}
	})
	t.Run("candidates", func(t *testing.T) {
		home := cleanTestHome(t)
		first, second := filepath.Join(home, "first"), filepath.Join(home, "second")
		build(t, first, "a", "b", "c")
		build(t, second, "d")
		saved := cleanProjectItems
		cleanProjectItems = 2
		defer func() { cleanProjectItems = saved }()
		section := cleanProjectScan(context.Background(), cleanProjectDeps(t, home, nil), []string{first, second}, nil)
		if len(section.Items) != 2 || !section.Roots[0].Truncated || section.Roots[0].Status != cleanPartial {
			t.Fatalf("section = %+v", section)
		}
		// 상한에서 멈추면 뒤 루트는 걷지 않았다고 보인다. 빈 결과로 보이지 않는다.
		if second := section.Roots[1]; second.Status != cleanNotScanned || !reflect.DeepEqual(second.Reasons, []string{cleanReasonProjectLimit}) || second.DirsVisited != nil {
			t.Fatalf("second root = %+v", second)
		}
	})
}

func TestCleanProjectCountsDirectoriesItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	cleanProjectTouch(t, filepath.Join(root, "ok", "package.json"))
	cleanWriteFile(t, filepath.Join(root, "ok", "node_modules", "f"), 1<<10)
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	section := cleanProjectScan(context.Background(), cleanProjectDeps(t, home, nil), []string{root}, nil)
	got := section.Roots[0]
	if got.Unreadable != 1 || got.Status != cleanPartial || !reflect.DeepEqual(got.Reasons, []string{cleanReasonUnreadableDirs}) {
		t.Fatalf("root = %+v", got)
	}
	if len(section.Items) != 1 || section.Status != cleanPartial {
		t.Fatalf("section = %+v", section)
	}
}

func TestCleanProjectStopsWhenTheTimeIsUp(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	for _, name := range []string{"a", "b"} {
		cleanProjectTouch(t, filepath.Join(root, name, "package.json"))
		cleanWriteFile(t, filepath.Join(root, name, "node_modules", "f"), 1<<10)
	}
	t.Run("before the walk", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		section := cleanProjectScan(ctx, cleanProjectDeps(t, home, nil), []string{root}, nil)
		if section.Status != cleanNotScanned || section.Roots[0].Status != cleanNotScanned ||
			!reflect.DeepEqual(section.Roots[0].Reasons, []string{cleanReasonTimeout}) || len(section.Items) != 0 {
			t.Fatalf("section = %+v", section)
		}
	})
	t.Run("between measurements", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		deps := cleanProjectDeps(t, home, nil)
		inner := deps.measure
		deps.measure = func(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
			scan, err := inner(ctx, options, sources)
			cancel()
			return scan, err
		}
		section := cleanProjectScan(ctx, deps, []string{root}, nil)
		if len(section.Items) != 2 {
			t.Fatalf("items = %+v", section.Items)
		}
		second := section.Items[1]
		if second.Status != cleanNotScanned || second.UsageBytes != nil || !reflect.DeepEqual(second.Reasons, []string{cleanReasonStopped}) {
			t.Fatalf("second = %+v", second)
		}
		if section.Status != cleanPartial {
			t.Fatalf("status = %q", section.Status)
		}
	})
}

func TestCleanProjectGitEvidence(t *testing.T) {
	home, deps := cleanProjectGitLab(t)
	repo := func(name string) string {
		dir := filepath.Join(home, "root", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cleanGitRun(t, dir, "init", "-q", "-b", "main")
		return dir
	}
	ignored := repo("ignored")
	cleanProjectTouch(t, filepath.Join(ignored, "build.zig"))
	if err := os.WriteFile(filepath.Join(ignored, ".gitignore"), []byte(".zig-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanWriteFile(t, filepath.Join(ignored, ".zig-cache", "f"), 4<<10)

	tracked := repo("tracked")
	cleanProjectTouch(t, filepath.Join(tracked, "build.gradle"))
	cleanWriteFile(t, filepath.Join(tracked, "build", "Source.java"), 4<<10)
	cleanGitRun(t, tracked, "add", "build/Source.java")

	both := repo("both")
	cleanProjectTouch(t, filepath.Join(both, "Cargo.toml"))
	if err := os.WriteFile(filepath.Join(both, ".gitignore"), []byte("target/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanWriteFile(t, filepath.Join(both, "target", "kept"), 4<<10)
	cleanGitRun(t, both, "add", "-f", "target/kept")

	notIgnored := repo("not-ignored")
	cleanProjectTouch(t, filepath.Join(notIgnored, "package.json"))
	cleanWriteFile(t, filepath.Join(notIgnored, "node_modules", "f"), 4<<10)

	outside := filepath.Join(home, "root", "plain")
	cleanProjectTouch(t, filepath.Join(outside, "Cargo.toml"))
	cleanWriteFile(t, filepath.Join(outside, "target", "f"), 4<<10)

	section := cleanProjectScan(context.Background(), deps, []string{filepath.Join(home, "root")}, nil)
	byPath := cleanProjectByPath(t, section)
	flag := func(value bool) *bool { return &value }
	tests := []struct {
		path        string
		git         string
		tracked     *bool
		regenerable bool
		reasons     []string
	}{
		{filepath.Join(ignored, ".zig-cache"), cleanGitIgnored, flag(false), true, nil},
		{filepath.Join(tracked, "build"), cleanGitNotIgnored, flag(true), false, []string{cleanReasonTrackedFiles}},
		// git은 추적 파일이 든 폴더를 무시된 것으로 답하지 않는다. 무시 규칙이 있어도 추적 파일이 우선이다.
		{filepath.Join(both, "target"), cleanGitNotIgnored, flag(true), false, []string{cleanReasonTrackedFiles}},
		{filepath.Join(notIgnored, "node_modules"), cleanGitNotIgnored, flag(false), true, nil},
		{filepath.Join(outside, "target"), cleanGitNone, nil, true, nil},
	}
	if len(section.Items) != len(tests) {
		t.Fatalf("items = %v", cleanProjectPaths(section))
	}
	for _, test := range tests {
		dir := byPath[test.path]
		if dir.Git != test.git || !reflect.DeepEqual(dir.TrackedFiles, test.tracked) || dir.Regenerable != test.regenerable ||
			!reflect.DeepEqual(dir.Reasons, test.reasons) || dir.Status != cleanMeasured {
			t.Errorf("%s = %+v", test.path, dir)
		}
	}
}

func TestCleanProjectGitFailureIsNotRecreatable(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	cleanProjectTouch(t, filepath.Join(root, "p", "package.json"))
	cleanWriteFile(t, filepath.Join(root, "p", "node_modules", "f"), 1<<10)
	fail := func(output cleanToolOutput) func(context.Context, cleanToolSpec) cleanToolOutput {
		return func(context.Context, cleanToolSpec) cleanToolOutput { return output }
	}
	inside := func(sub string, output cleanToolOutput) func(context.Context, cleanToolSpec) cleanToolOutput {
		return func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
			if cleanGitSub(spec) == sub {
				return output
			}
			return cleanToolOutput{Stdout: root + "/p\n"}
		}
	}
	tests := []struct {
		name string
		run  func(context.Context, cleanToolSpec) cleanToolOutput
	}{
		{"dubious ownership", fail(cleanToolOutput{ExitCode: 128, Stderr: "fatal: detected dubious ownership in repository", Err: errors.New("exit status 128")})},
		{"a time out", fail(cleanToolOutput{Err: errCleanToolTimeout})},
		{"no git binary", fail(cleanToolOutput{Err: errors.New("executable file not found")})},
		{"a truncated answer", fail(cleanToolOutput{Stdout: "/x", Truncated: true})},
		{"check-ignore fails", inside("check-ignore", cleanToolOutput{ExitCode: 128, Stderr: "fatal: bad", Err: errors.New("exit status 128")})},
		{"ls-files fails", inside("ls-files", cleanToolOutput{ExitCode: 128, Stderr: "fatal: bad", Err: errors.New("exit status 128")})},
	}
	for _, test := range tests {
		section := cleanProjectScan(context.Background(), cleanProjectDeps(t, home, test.run), []string{root}, nil)
		if len(section.Items) != 1 {
			t.Fatalf("%s: items = %+v", test.name, section.Items)
		}
		dir := section.Items[0]
		if dir.Git != cleanGitUnknown || dir.Regenerable || !reflect.DeepEqual(dir.Reasons, []string{cleanReasonGitUnknown}) {
			t.Errorf("%s: %+v", test.name, dir)
		}
		// 측정은 했지만 재생성 가능이 아니므로 합계에 들어가지 않는다.
		if dir.UsageBytes == nil || len(cleanFilesystemsWith(nil, section.Items, cleanSources(mustHostSources(t)))) != 0 {
			t.Errorf("%s: the folder is summed or not measured", test.name)
		}
	}
}

func TestCleanProjectCatalogRoots(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	cache := filepath.Join(root, "cache")
	cleanProjectTouch(t, filepath.Join(cache, "deep", "package.json"))
	cleanWriteFile(t, filepath.Join(cache, "deep", "node_modules", "f"), 1<<10)
	same := filepath.Join(root, "proj", "node_modules")
	cleanProjectTouch(t, filepath.Join(root, "proj", "package.json"))
	cleanWriteFile(t, filepath.Join(same, "f"), 1<<10)
	usage := uint64(1)
	catalog := []cleanItem{
		{ID: "pnpm", Status: cleanMeasured, UsageBytes: &usage, roots: []string{cache}},
		{ID: "npm", Status: cleanMeasured, UsageBytes: &usage, roots: []string{same}},
		{ID: "uv", Status: cleanUnreadable, roots: []string{filepath.Join(root, "uv")}},
	}
	recorder := &cleanRecorder{}
	deps := cleanProjectDeps(t, home, nil)
	deps.measure = recorder.measure
	section := cleanProjectScan(context.Background(), deps, []string{root}, catalog)
	if len(section.Items) != 1 {
		t.Fatalf("a folder under a measured candidate is found: %v", cleanProjectPaths(section))
	}
	dir := section.Items[0]
	if dir.Path != same || dir.Status != cleanDuplicate || dir.SameAs != "npm" || dir.UsageBytes != nil || recorder.touching(same) != 0 {
		t.Fatalf("dir = %+v", dir)
	}

	// 루트가 후보 안에 있으면 폴더를 찾지만 합계에서 뺀다.
	section = cleanProjectScan(context.Background(), cleanProjectDeps(t, home, nil), []string{filepath.Join(cache, "deep")}, catalog)
	if len(section.Items) != 1 || section.Items[0].Inside != "pnpm" || section.Items[0].UsageBytes == nil || !section.Items[0].Regenerable {
		t.Fatalf("section = %+v", section)
	}
	if sums := cleanFilesystemsWith(nil, section.Items, cleanSources(mustHostSources(t))); len(sums) != 0 {
		t.Fatalf("a folder inside a candidate is summed again: %+v", sums)
	}
}

func TestCleanProjectLinksBuildDirsToWorktrees(t *testing.T) {
	lab := newCleanWorktreeLab(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(lab.home))
	repo := lab.repo("main")
	inside := lab.add(repo, "w", "work")
	other := lab.add(repo, "x", "other")
	cleanProjectTouch(t, filepath.Join(inside, "package.json"))
	cleanWriteFile(t, filepath.Join(inside, "node_modules", "f"), 4<<10)
	cleanProjectTouch(t, filepath.Join(inside, "pkg", "Cargo.toml"))
	cleanWriteFile(t, filepath.Join(inside, "pkg", "target", "f"), 4<<10)
	deps := lab.deps(nil)
	projects := cleanProjectScan(context.Background(), deps, []string{lab.root, repo}, nil)
	worktrees := cleanWorktreeScan(context.Background(), deps)
	if len(worktrees.Items) != 2 {
		t.Fatalf("worktrees = %+v", worktrees.Items)
	}
	for _, item := range worktrees.Items {
		if item.BuildDirs != nil {
			t.Fatalf("a count exists before the link: %+v", item)
		}
	}
	cleanLinkWorktrees(&projects, &worktrees)
	counts := map[string]int{}
	for _, item := range worktrees.Items {
		if item.BuildDirs == nil {
			t.Fatalf("no count for %s", item.Path)
		}
		counts[item.Path] = *item.BuildDirs
	}
	if counts[inside] != 2 || counts[other] != 0 {
		t.Fatalf("counts = %v", counts)
	}
	linked := 0
	for _, dir := range projects.Items {
		if dir.InsideWorktree != "" {
			linked++
			if dir.InsideWorktree != inside {
				t.Errorf("%s is inside %s", dir.Path, dir.InsideWorktree)
			}
		}
	}
	if linked != 2 {
		t.Fatalf("linked = %d, items = %+v", linked, projects.Items)
	}

	// 폴더를 찾지 않았으면 0이 아니라 null이다.
	unsearched := cleanWorktrees{Items: []cleanWorktree{{Path: "/w"}}}
	cleanLinkWorktrees(&cleanProjectBuildDirs{Status: cleanNoProjectRoot}, &unsearched)
	data, err := json.Marshal(unsearched)
	if err != nil || !strings.Contains(string(data), `"build_dirs":null`) {
		t.Fatalf("JSON = %s (%v)", data, err)
	}
}

func TestRunCleanTextAndJSONShowProjectBuildFolders(t *testing.T) {
	cleanGitEnvironment(t)
	home := cleanTestHome(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	root := filepath.Join(home, "work")
	cleanProjectTouch(t, filepath.Join(root, "ultrasync", "Cargo.toml"))
	cleanWriteFile(t, filepath.Join(root, "ultrasync", "target", "f"), 64<<10)
	cleanProjectTouch(t, filepath.Join(root, "web", "package.json"))
	cleanWriteFile(t, filepath.Join(root, "web", "node_modules", "f"), 8<<10)
	cleanWriteFile(t, filepath.Join(root, "docs", "build", "f"), 8<<10)
	deps := cleanRunDeps(t, home)
	tools := deps.run
	deps.run = func(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
		if spec.Name == "git" {
			return runCleanTool(ctx, spec)
		}
		return tools(ctx, spec)
	}
	var text bytes.Buffer
	if code := runCleanWith([]string{"--project", root, "-v"}, "test", deps, true, nil, &text); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	for _, want := range []string{"Project build folders", "Cargo target", "node_modules", "marker Cargo.toml, git: not a git repository, can be recreated",
		filepath.Join("~", "work", "ultrasync", "target"), "1 name matches without a marker file not measured"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text lacks %q:\n%s", want, text.String())
		}
	}
	path := filepath.Join(t.TempDir(), "out.json")
	if code := runCleanWith([]string{"--project", root, "--json", path}, "test", deps, true, nil, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Metrics struct {
				Project     cleanJSONProject `json:"project_build_dirs"`
				Filesystems []struct {
					Candidates []string `json:"candidates"`
				} `json:"filesystems"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	metrics := report.Results[0].Metrics
	if metrics.Project.Status != cleanMeasured || len(metrics.Project.Items) != 2 {
		t.Fatalf("project = %+v", metrics.Project)
	}
	for _, item := range metrics.Project.Items {
		for _, key := range []string{"kind", "path", "resolved_path", "project", "marker", "git", "regenerable", "status", "usage_bytes", "measured_at", "filesystem"} {
			if _, ok := item[key]; !ok {
				t.Errorf("item lacks %q: %v", key, item)
			}
		}
	}
	summed := 0
	for _, filesystem := range metrics.Filesystems {
		for _, candidate := range filesystem.Candidates {
			if strings.HasPrefix(candidate, root) {
				summed++
			}
		}
	}
	if summed != 2 {
		t.Fatalf("the filesystem sums include %d build folders, want 2", summed)
	}
}

func TestCleanBoundaryBuildDirNeedsMarkerAndGitEvidence(t *testing.T) {
	home, deps := cleanProjectGitLab(t)
	root := filepath.Join(home, "root")
	repo := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cleanGitRun(t, dir, "init", "-q", "-b", "main")
		return dir
	}
	// 표지가 없는 node_modules는 소스 폴더일 수 있다.
	unmarked := repo("unmarked")
	cleanWriteFile(t, filepath.Join(unmarked, "node_modules", "f"), 64<<10)
	// 추적 파일이 있는 build.gradle 옆의 build는 소스다.
	gradle := repo("gradle")
	cleanProjectTouch(t, filepath.Join(gradle, "build.gradle"))
	cleanWriteFile(t, filepath.Join(gradle, "build", "Source.java"), 64<<10)
	cleanGitRun(t, gradle, "add", "build/Source.java")
	// git이 답하지 못한 폴더는 재생성 가능하다고 하지 않는다.
	unknown := filepath.Join(root, "unknown")
	cleanProjectTouch(t, filepath.Join(unknown, "package.json"))
	cleanWriteFile(t, filepath.Join(unknown, "node_modules", "f"), 64<<10)
	// 근거가 확인된 폴더만 합계에 들어간다.
	zig := repo("zig")
	cleanProjectTouch(t, filepath.Join(zig, "build.zig"))
	if err := os.WriteFile(filepath.Join(zig, ".gitignore"), []byte(".zig-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanWriteFile(t, filepath.Join(zig, ".zig-cache", "f"), 16<<10)
	rust := filepath.Join(root, "rust")
	cleanProjectTouch(t, filepath.Join(rust, "Cargo.toml"))
	cleanWriteFile(t, filepath.Join(rust, "target", "f"), 16<<10)

	run := deps.run
	deps.run = func(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
		if spec.Dir == unknown {
			return cleanToolOutput{ExitCode: 128, Stderr: "fatal: detected dubious ownership in repository", Err: errors.New("exit status 128")}
		}
		return run(ctx, spec)
	}
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	byPath := cleanProjectByPath(t, section)
	if _, found := byPath[filepath.Join(unmarked, "node_modules")]; found {
		t.Fatal("a folder without a marker file is listed")
	}
	if section.Roots[0].Unmarked == nil || *section.Roots[0].Unmarked != 1 {
		t.Fatalf("unmarked = %v", section.Roots[0].Unmarked)
	}
	for _, path := range []string{filepath.Join(gradle, "build"), filepath.Join(unknown, "node_modules")} {
		dir, found := byPath[path]
		if !found || dir.Regenerable {
			t.Errorf("%s = %+v", path, dir)
		}
	}
	want := []string{filepath.Join(rust, "target"), filepath.Join(zig, ".zig-cache")}
	for _, path := range want {
		if dir := byPath[path]; !dir.Regenerable || !dir.counted() {
			t.Errorf("%s = %+v", path, dir)
		}
	}
	sums := cleanFilesystemsWith(nil, section.Items, cleanSources(mustHostSources(t)))
	var candidates []string
	var usage uint64
	for _, sum := range sums {
		candidates = append(candidates, sum.Candidates...)
		usage += sum.UsageBytes
	}
	sort.Strings(candidates)
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("the sums hold %v, want %v", candidates, want)
	}
	if want := *byPath[want[0]].UsageBytes + *byPath[want[1]].UsageBytes; usage != want {
		t.Fatalf("usage = %d, want %d", usage, want)
	}
}

func TestCleanProjectDoesNotStatNetworkMounts(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	for _, name := range []string{"mnt", "ok"} {
		cleanProjectTouch(t, filepath.Join(root, name, "proj", "package.json"))
		cleanWriteFile(t, filepath.Join(root, name, "proj", "node_modules", "f"), 4<<10)
	}
	mnt := filepath.Join(root, "mnt")
	deps := cleanProjectDeps(t, home, nil)
	deps.mounts = func() ([]diskMount, error) { return []diskMount{{Point: mnt, FSType: "fuse.sshfs"}}, nil }
	deviceOf := deps.deviceOf
	deps.deviceOf = func(path string) (uint64, error) {
		if pathUnder(path, mnt) {
			t.Fatalf("stat on the network mount %s", path)
		}
		return deviceOf(path)
	}
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	if want := []string{filepath.Join(root, "ok", "proj", "node_modules")}; !reflect.DeepEqual(cleanProjectPaths(section), want) {
		t.Fatalf("items = %v", cleanProjectPaths(section))
	}
	if got := section.Roots[0]; got.SkippedMounts != 1 || got.Status != cleanPartial {
		t.Fatalf("root = %+v", got)
	}
}

func TestCleanProjectSkipsADirectoryThatStopsAnswering(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "root")
	cleanProjectTouch(t, filepath.Join(root, "ok", "package.json"))
	cleanWriteFile(t, filepath.Join(root, "ok", "node_modules", "f"), 1<<10)
	stuck := filepath.Join(root, "stuck")
	if err := os.MkdirAll(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	stalls := &cleanStalls{}
	deps := cleanProjectDeps(t, home, nil)
	deps.readDir = cleanReadDirGuard(func(path string) ([]os.DirEntry, error) {
		if path == stuck {
			<-release // macOS 권한 창처럼 응답할 때까지 돌아오지 않는다.
		}
		return os.ReadDir(path)
	}, nil, stalls, 20*time.Millisecond, 80*time.Millisecond)
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	if got := section.Roots[0]; got.Unreadable != 1 || got.Status != cleanPartial {
		t.Fatalf("root = %+v", got)
	}
	if want := []string{filepath.Join(root, "ok", "node_modules")}; !reflect.DeepEqual(cleanProjectPaths(section), want) {
		t.Fatalf("items = %v", cleanProjectPaths(section))
	}
	if warnings := stalls.warnings(10 * time.Second); len(warnings) != 1 || !strings.Contains(warnings[0], stuck) {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestCleanProjectSkipsARootThatStopsAnswering(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "nfs")
	cleanProjectTouch(t, filepath.Join(root, "proj", "package.json"))
	release := make(chan struct{})
	defer close(release)
	deps := cleanProjectDeps(t, home, nil)
	deps.deviceOf = func(string) (uint64, error) {
		<-release // 응답하지 않는 네트워크 마운트의 루트처럼 돌아오지 않는다.
		return 1, nil
	}
	deps.wait = cleanCallGuard(nil, &cleanStalls{}, 20*time.Millisecond, 80*time.Millisecond)
	section := cleanProjectScan(context.Background(), deps, []string{root}, nil)
	got := section.Roots[0]
	if got.Status != cleanUnreadable || !reflect.DeepEqual(got.Reasons, []string{cleanReasonNoResponse}) || len(section.Items) != 0 {
		t.Fatalf("root = %+v items = %v", got, section.Items)
	}
}
