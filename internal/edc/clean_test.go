package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func cleanTestHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func cleanTestTree(t *testing.T, root string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cleanFixedSpec(id, path string) cleanSpec {
	return cleanSpec{id: id, category: "build_cache", fixed: path, cost: "low"}
}

func cleanToolSpecFor(id string, lines, line int) cleanSpec {
	return cleanSpec{id: id, category: "package_cache", tool: id, args: []string{"where"}, lines: lines, line: line, cost: "medium"}
}

func cleanStdout(text string) func(context.Context, cleanToolSpec) cleanToolOutput {
	return func(context.Context, cleanToolSpec) cleanToolOutput { return cleanToolOutput{Stdout: text} }
}

func cleanTestDeps(home string, run func(context.Context, cleanToolSpec) cleanToolOutput) cleanDeps {
	host, _ := hostDuSources()
	return cleanDeps{
		home: home, run: run, measure: duMeasure, sources: cleanSources(host),
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
	}
}

func cleanStatusOf(items []cleanItem) map[string]string {
	statuses := map[string]string{}
	for _, item := range items {
		statuses[item.ID] = item.Status
	}
	return statuses
}

func TestCleanCatalogSplitsByPlatform(t *testing.T) {
	ids := func(goos string) []string {
		var list []string
		for _, spec := range cleanCatalog(goos, "/home/me") {
			list = append(list, spec.id)
		}
		return list
	}
	common := []string{"go-build", "go-mod", "npm", "pnpm", "uv", "homebrew"}
	if got := ids("linux"); !reflect.DeepEqual(got, common) {
		t.Fatalf("linux = %v", got)
	}
	if got, want := ids("darwin"), append(append([]string{}, common...), "xcode-derived-data", "swiftpm"); !reflect.DeepEqual(got, want) {
		t.Fatalf("darwin = %v", got)
	}
	cleanups := map[string]string{}
	for _, spec := range cleanCatalog("darwin", "/Users/me") {
		cleanups[spec.id] = spec.cleanup
		if spec.fixed != "" && !strings.HasPrefix(spec.fixed, "/Users/me/Library/") {
			t.Fatalf("%s fixed path = %q", spec.id, spec.fixed)
		}
	}
	want := map[string]string{
		"go-build": "go clean -cache", "go-mod": "go clean -modcache", "npm": "npm cache clean --force",
		"pnpm": "pnpm store prune", "uv": "uv cache clean", "homebrew": "brew cleanup -s",
		"xcode-derived-data": "", "swiftpm": "swift package purge-cache",
	}
	if !reflect.DeepEqual(cleanups, want) {
		t.Fatalf("cleanups = %v", cleanups)
	}
}

func TestCleanRunnerGetsHomeDirectoryAndQuietEnvironment(t *testing.T) {
	home := cleanTestHome(t)
	var specs []cleanToolSpec
	run := func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
		specs = append(specs, spec)
		return cleanToolOutput{Stdout: filepath.Join(home, "x", "a") + "\n" + filepath.Join(home, "x", "b") + "\n"}
	}
	items := cleanScan(context.Background(), cleanTestDeps(home, run), cleanCatalog("linux", home))
	if len(items) != 6 {
		t.Fatalf("items = %d", len(items))
	}
	env := map[string][]string{}
	for _, spec := range specs {
		if spec.Dir != home {
			t.Fatalf("%s runs in %q, want the home directory", spec.Name, spec.Dir)
		}
		env[spec.Name] = spec.Env
	}
	if !reflect.DeepEqual(env["go"], []string{"GOTOOLCHAIN=local"}) || !reflect.DeepEqual(env["npm"], []string{"npm_config_update_notifier=false"}) {
		t.Fatalf("env = %v", env)
	}
	// go env 한 번이 go-build와 go-mod 두 후보에 답한다.
	goRuns := 0
	for _, spec := range specs {
		if spec.Name == "go" {
			goRuns++
		}
	}
	if goRuns != 1 {
		t.Fatalf("go env ran %d times", goRuns)
	}
}

func TestCleanParsesToolOutput(t *testing.T) {
	home := cleanTestHome(t)
	abs := filepath.Join(home, "cache")
	cases := []struct {
		name   string
		spec   cleanSpec
		output cleanToolOutput
		status string
		reason string
	}{
		{"one absolute line", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Stdout: abs + "\n"}, cleanAbsent, ""},
		{"truncated", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Stdout: abs + "\n", Truncated: true}, cleanUnknownLocation, cleanReasonInvalidOutput},
		{"two lines for one", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Stdout: abs + "\n" + abs + "\n"}, cleanUnknownLocation, cleanReasonInvalidOutput},
		{"one line for two", cleanToolSpecFor("go", 2, 0), cleanToolOutput{Stdout: abs + "\n"}, cleanUnknownLocation, cleanReasonInvalidOutput},
		{"empty", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Stdout: "\n"}, cleanUnknownLocation, cleanReasonInvalidOutput},
		{"relative", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Stdout: "cache\n"}, cleanUnknownLocation, cleanReasonInvalidOutput},
		{"off", cleanToolSpecFor("go", 2, 0), cleanToolOutput{Stdout: "off\n" + abs + "\n"}, cleanUnknownLocation, cleanReasonInvalidOutput},
		{"timeout", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Err: errCleanToolTimeout}, cleanUnknownLocation, cleanReasonToolTimeout},
		{"failure", cleanToolSpecFor("npm", 1, 0), cleanToolOutput{Err: errors.New("exit status 3"), ExitCode: 3, Stderr: "boom\nmore"}, cleanUnknownLocation, cleanReasonToolFailed},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			deps := cleanTestDeps(home, func(context.Context, cleanToolSpec) cleanToolOutput { return test.output })
			item := cleanScan(context.Background(), deps, []cleanSpec{test.spec})[0]
			if item.Status != test.status {
				t.Fatalf("status = %q, want %q (%+v)", item.Status, test.status, item)
			}
			if test.reason != "" && (len(item.Reasons) != 1 || item.Reasons[0] != test.reason) {
				t.Fatalf("reasons = %v, want %s", item.Reasons, test.reason)
			}
			if test.status == cleanUnknownLocation && (item.ReportedPath != "" || item.UsageBytes != nil) {
				t.Fatalf("a candidate with an unknown location carries a path or a size: %+v", item)
			}
		})
	}
	failure := cleanScan(context.Background(), cleanTestDeps(home, func(context.Context, cleanToolSpec) cleanToolOutput {
		return cleanToolOutput{Err: errors.New("x"), ExitCode: 3, Stderr: "boom\nmore"}
	}),
		[]cleanSpec{cleanToolSpecFor("npm", 1, 0)})[0]
	if failure.Detail != "exit 3: boom" {
		t.Fatalf("detail = %q", failure.Detail)
	}
}

func TestCleanTakesTheSecondLineForTheSecondCandidate(t *testing.T) {
	home := cleanTestHome(t)
	first, second := filepath.Join(home, "one"), filepath.Join(home, "two")
	cleanTestTree(t, second, 8<<10)
	deps := cleanTestDeps(home, cleanStdout(first+"\n"+second+"\n"))
	items := cleanScan(context.Background(), deps, []cleanSpec{cleanToolSpecFor("go", 2, 0), cleanToolSpecFor("go", 2, 1)})
	if items[0].ReportedPath != first || items[0].Status != cleanAbsent || items[1].ReportedPath != second || items[1].Status != cleanMeasured {
		t.Fatalf("items = %+v", items)
	}
}

func cleanWriteScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCleanToolReadsOnlyStdoutWithinLimits(t *testing.T) {
	t.Run("stderr stays apart", func(t *testing.T) {
		script := cleanWriteScript(t, `echo "npm WARN old" >&2; echo /cache/path`)
		output := runCleanTool(context.Background(), cleanToolSpec{Dir: t.TempDir(), Name: script})
		if output.Err != nil || output.Stdout != "/cache/path\n" || !strings.Contains(output.Stderr, "WARN") {
			t.Fatalf("output = %+v", output)
		}
	})
	t.Run("long output is truncated", func(t *testing.T) {
		script := cleanWriteScript(t, `head -c 70000 /dev/zero | tr '\0' a`)
		output := runCleanTool(context.Background(), cleanToolSpec{Dir: t.TempDir(), Name: script})
		if !output.Truncated || len(output.Stdout) != cleanOutputLimit {
			t.Fatalf("truncated %v, %d bytes", output.Truncated, len(output.Stdout))
		}
	})
	t.Run("a slow tool times out", func(t *testing.T) {
		previous := cleanLookupTimeout
		cleanLookupTimeout = 200 * time.Millisecond
		defer func() { cleanLookupTimeout = previous }()
		script := cleanWriteScript(t, `sleep 30`)
		started := time.Now()
		output := runCleanTool(context.Background(), cleanToolSpec{Dir: t.TempDir(), Name: script})
		if !errors.Is(output.Err, errCleanToolTimeout) || time.Since(started) > 5*time.Second {
			t.Fatalf("err %v after %s", output.Err, time.Since(started))
		}
	})
	t.Run("a child that holds the pipe cannot hang the lookup", func(t *testing.T) {
		previous := cleanLookupTimeout
		cleanLookupTimeout = 200 * time.Millisecond
		defer func() { cleanLookupTimeout = previous }()
		script := cleanWriteScript(t, `(sleep 30 &) ; echo /cache`)
		started := time.Now()
		output := runCleanTool(context.Background(), cleanToolSpec{Dir: t.TempDir(), Name: script})
		if time.Since(started) > 5*time.Second || output.Err == nil {
			t.Fatalf("err %v after %s", output.Err, time.Since(started))
		}
	})
	t.Run("the overall limit wins over the lookup limit", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		output := runCleanTool(ctx, cleanToolSpec{Dir: t.TempDir(), Name: cleanWriteScript(t, `sleep 30`)})
		if output.Err == nil || errors.Is(output.Err, errCleanToolTimeout) {
			t.Fatalf("err = %v", output.Err)
		}
	})
}

func TestCleanRejectsLocationsThatCoverUserData(t *testing.T) {
	home := cleanTestHome(t)
	for name, reported := range map[string]string{"root": "/", "home": home, "parent": filepath.Dir(home)} {
		t.Run(name, func(t *testing.T) {
			var measured atomic.Int32
			deps := cleanTestDeps(home, cleanStdout(reported+"\n"))
			deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
				measured.Add(1)
				return duScan{}, nil
			}
			item := cleanScan(context.Background(), deps, []cleanSpec{cleanToolSpecFor("npm", 1, 0)})[0]
			if item.Status != cleanRejected || item.Reasons[0] != cleanReasonCoversUserData || item.UsageBytes != nil || measured.Load() != 0 {
				t.Fatalf("item = %+v, measured %d", item, measured.Load())
			}
		})
	}
}

func TestCleanMeasuresTheTargetOfASymlink(t *testing.T) {
	home := cleanTestHome(t)
	target := filepath.Join(home, "real")
	cleanTestTree(t, target, 64<<10)
	link := filepath.Join(home, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	item := cleanScan(context.Background(), cleanTestDeps(home, nil), []cleanSpec{cleanFixedSpec("link", link)})[0]
	direct, err := duMeasure(context.Background(), duOptions{root: target, workers: duDefaultWorkers}, cleanSources(mustHostSources(t)))
	if err != nil {
		t.Fatal(err)
	}
	if item.ReportedPath != link || item.ResolvedPath != target || item.UsageBytes == nil || *item.UsageBytes != direct.root.total {
		t.Fatalf("item = %+v, target total %d", item, direct.root.total)
	}
}

func mustHostSources(t *testing.T) duSources {
	t.Helper()
	sources, ok := hostDuSources()
	if !ok {
		t.Skip("du is not supported on this platform")
	}
	return sources
}

func TestCleanCountsAnOverlappingCandidateOnce(t *testing.T) {
	home := cleanTestHome(t)
	outer := filepath.Join(home, "outer")
	inner := filepath.Join(outer, "inner")
	cleanTestTree(t, inner, 64<<10)
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(outer, alias); err != nil {
		t.Fatal(err)
	}
	items := cleanScan(context.Background(), cleanTestDeps(home, nil), []cleanSpec{
		cleanFixedSpec("inner", inner), cleanFixedSpec("outer", outer), cleanFixedSpec("alias", alias),
	})
	if items[0].Inside != "outer" || items[1].Inside != "" {
		t.Fatalf("inside = %q, %q", items[0].Inside, items[1].Inside)
	}
	if items[2].Status != cleanDuplicate || items[2].SameAs != "outer" || items[2].UsageBytes != nil {
		t.Fatalf("alias = %+v", items[2])
	}
	filesystems := cleanFilesystems(items, cleanSources(mustHostSources(t)))
	if len(filesystems) != 1 || filesystems[0].UsageBytes != *items[1].UsageBytes || !reflect.DeepEqual(filesystems[0].Candidates, []string{"outer"}) {
		t.Fatalf("filesystems = %+v", filesystems)
	}
}

func TestCleanFilesystemSumsFlagSharedBlocksAndLowerBounds(t *testing.T) {
	size := func(value uint64) *uint64 { return &value }
	items := []cleanItem{
		{ID: "a", Status: cleanMeasured, UsageBytes: size(10), Filesystem: &cleanFilesystemRef{Mount: "/data", FSType: "apfs"}},
		{ID: "b", Status: cleanPartial, LowerBound: true, UsageBytes: size(5), Filesystem: &cleanFilesystemRef{Mount: "/data", FSType: "apfs"}},
		{ID: "c", Status: cleanMeasured, UsageBytes: size(7), Filesystem: &cleanFilesystemRef{Mount: "/other", FSType: "ext4"}},
		{ID: "d", Status: cleanAbsent},
	}
	filesystems := cleanFilesystems(items, duSources{})
	if len(filesystems) != 2 {
		t.Fatalf("filesystems = %+v", filesystems)
	}
	if first := filesystems[0]; first.UsageBytes != 15 || !first.LowerBound || !first.SharedBlocks {
		t.Fatalf("first = %+v", first)
	}
	if second := filesystems[1]; second.UsageBytes != 7 || second.LowerBound || second.SharedBlocks {
		t.Fatalf("second = %+v", second)
	}
}

func TestCleanGroupsDerivedDataByApp(t *testing.T) {
	suffix := func(letter string) string { return strings.Repeat(letter, 28) }
	node := func(name string, total uint64) *duNode { return &duNode{name: name, dir: true, total: total} }
	root := &duNode{dir: true, children: []*duNode{
		node("Shop-"+suffix("a"), 30), node("Shop-"+suffix("b"), 40), node("Shop-"+suffix("c"), 5),
		node("Mail-"+suffix("d"), 100), node("CompilationCache.noindex", 20), node("Weird-abc", 1),
		{name: "loose", total: 999},
	}}
	got := cleanGroupDerivedData(root)
	want := []cleanGroup{{"Mail", 1, 100}, {"Shop", 3, 75}, {"CompilationCache.noindex", 1, 20}, {"Weird-abc", 1, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %+v", got)
	}
}

func TestCleanBoundaryHostWideSourcesStayUntouched(t *testing.T) {
	host := mustHostSources(t)
	var deletedCalls, labelCalls atomic.Int32
	counting := host
	counting.deletedOpen = func() (map[uint64]*deletedOpenSummary, bool) {
		deletedCalls.Add(1)
		return nil, false
	}
	counting.containerLabels = func(context.Context) map[string]string {
		labelCalls.Add(1)
		return nil
	}
	cleaned := cleanSources(counting)
	if cleaned.deletedOpen != nil || cleaned.containerLabels != nil {
		t.Fatal("cleanSources copies a host-wide source")
	}
	if clean := cleanSources(host); clean.deletedOpen != nil || clean.containerLabels != nil {
		t.Fatal("cleanSources copies a host-wide source of the real host")
	}
	home := cleanTestHome(t)
	first, second := filepath.Join(home, "first"), filepath.Join(home, "second")
	cleanTestTree(t, first, 16<<10)
	cleanTestTree(t, second, 16<<10)
	deps := cleanTestDeps(home, nil)
	deps.sources = cleaned
	items := cleanScan(context.Background(), deps, []cleanSpec{cleanFixedSpec("first", first), cleanFixedSpec("second", second)})
	if items[0].Status != cleanMeasured || items[1].Status != cleanMeasured {
		t.Fatalf("items = %+v", items)
	}
	if deletedCalls.Load() != 0 || labelCalls.Load() != 0 {
		t.Fatalf("clean called deletedOpen %d times and containerLabels %d times", deletedCalls.Load(), labelCalls.Load())
	}
	// 양성 대조: duWalk는 Linux에서 두 원천을 한 번씩 부른다.
	if _, err := duWalk(context.Background(), duOptions{root: first, workers: duDefaultWorkers}, counting); err != nil {
		t.Fatal(err)
	}
	want := int32(0)
	if runtime.GOOS == "linux" {
		want = 1
	}
	if deletedCalls.Load() != want || labelCalls.Load() != want {
		t.Fatalf("duWalk calls: deletedOpen %d, containerLabels %d, want %d each", deletedCalls.Load(), labelCalls.Load(), want)
	}
	deletedCalls.Store(0)
	labelCalls.Store(0)
	// 두 필드를 지우지 않은 원천을 duMeasure에 바로 넘겨도 호스트 전체 원천을 부르지 않는다.
	if _, err := duMeasure(context.Background(), duOptions{root: first, workers: duDefaultWorkers}, counting); err != nil {
		t.Fatal(err)
	}
	if deletedCalls.Load() != 0 || labelCalls.Load() != 0 {
		t.Fatalf("duMeasure calls: deletedOpen %d, containerLabels %d", deletedCalls.Load(), labelCalls.Load())
	}
}

func cleanFakeScan(total uint64) duScan {
	return duScan{root: &duNode{dir: true, total: total, count: 1}, filesystem: duFilesystem{Mount: "/data", FSType: "ext4"}}
}

func TestCleanBoundaryFailuresAreNotZeroBytes(t *testing.T) {
	home := cleanTestHome(t)
	present := filepath.Join(home, "present")
	cleanTestTree(t, present, 8<<10)
	fake := func(scan duScan, err error) func(context.Context, duOptions, duSources) (duScan, error) {
		return func(context.Context, duOptions, duSources) (duScan, error) { return scan, err }
	}
	single := func(t *testing.T, deps cleanDeps, spec cleanSpec) cleanItem {
		t.Helper()
		return cleanScan(context.Background(), deps, []cleanSpec{spec})[0]
	}
	assertNoUsage := func(t *testing.T, item cleanItem) {
		t.Helper()
		if item.UsageBytes != nil {
			t.Fatalf("%s carries a size: %+v", item.Status, item)
		}
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "usage_bytes") {
			t.Fatalf("%s JSON has usage_bytes: %s", item.Status, data)
		}
	}
	t.Run("a missing path is absent", func(t *testing.T) {
		item := single(t, cleanTestDeps(home, nil), cleanFixedSpec("x", filepath.Join(home, "missing")))
		if item.Status != cleanAbsent {
			t.Fatalf("status = %q", item.Status)
		}
		assertNoUsage(t, item)
	})
	t.Run("a denied path is unreadable", func(t *testing.T) {
		deps := cleanTestDeps(home, nil)
		deps.measure = fake(duScan{}, &fs.PathError{Op: "open", Path: present, Err: syscall.EACCES})
		item := single(t, deps, cleanFixedSpec("x", present))
		if item.Status != cleanUnreadable || item.Reasons[0] != cleanReasonPermission {
			t.Fatalf("item = %+v", item)
		}
		assertNoUsage(t, item)
	})
	t.Run("an unreadable root is unreadable", func(t *testing.T) {
		scan := cleanFakeScan(4096)
		scan.root.unreadable = true
		deps := cleanTestDeps(home, nil)
		deps.measure = fake(scan, nil)
		item := single(t, deps, cleanFixedSpec("x", present))
		if item.Status != cleanUnreadable {
			t.Fatalf("status = %q", item.Status)
		}
		assertNoUsage(t, item)
	})
	t.Run("unreadable directories make a lower bound", func(t *testing.T) {
		scan := cleanFakeScan(4096)
		scan.unreadableCount = 2
		deps := cleanTestDeps(home, nil)
		deps.measure = fake(scan, nil)
		item := single(t, deps, cleanFixedSpec("x", present))
		if item.Status != cleanPartial || !item.LowerBound || item.UsageBytes == nil || item.Reasons[0] != cleanReasonUnreadableDirs {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("skipped mounts make a lower bound", func(t *testing.T) {
		scan := cleanFakeScan(4096)
		scan.mounts = []duMount{{Path: "/x/mnt", FSType: "ext4"}}
		deps := cleanTestDeps(home, nil)
		deps.measure = fake(scan, nil)
		item := single(t, deps, cleanFixedSpec("x", present))
		if item.Status != cleanPartial || !item.LowerBound || item.Reasons[0] != cleanReasonSkippedMounts || item.SkippedMounts != 1 {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("a scan that stops at the time limit is partial", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		deps := cleanTestDeps(home, nil)
		deps.measure = func(ctx context.Context, _ duOptions, _ duSources) (duScan, error) {
			<-ctx.Done()
			scan := cleanFakeScan(4096)
			scan.partial = true
			return scan, nil
		}
		item := cleanScan(ctx, deps, []cleanSpec{cleanFixedSpec("x", present)})[0]
		if item.Status != cleanPartial || !item.LowerBound || item.Reasons[0] != cleanReasonTimeout {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("a scan that you stop is partial with stopped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		deps := cleanTestDeps(home, nil)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
			cancel()
			scan := cleanFakeScan(4096)
			scan.partial = true
			return scan, nil
		}
		item := cleanScan(ctx, deps, []cleanSpec{cleanFixedSpec("x", present)})[0]
		if item.Status != cleanPartial || item.Reasons[0] != cleanReasonStopped {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("a missing tool is tool_absent", func(t *testing.T) {
		deps := cleanTestDeps(home, nil)
		deps.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		item := single(t, deps, cleanToolSpecFor("npm", 1, 0))
		if item.Status != cleanToolAbsent || item.ReportedPath != "" {
			t.Fatalf("item = %+v", item)
		}
		assertNoUsage(t, item)
	})
	t.Run("a tool that times out has an unknown location", func(t *testing.T) {
		item := single(t, cleanTestDeps(home, func(context.Context, cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Err: errCleanToolTimeout}
		}), cleanToolSpecFor("npm", 1, 0))
		if item.Status != cleanUnknownLocation || item.Reasons[0] != cleanReasonToolTimeout {
			t.Fatalf("item = %+v", item)
		}
		assertNoUsage(t, item)
	})
	t.Run("bad output has an unknown location", func(t *testing.T) {
		item := single(t, cleanTestDeps(home, cleanStdout("relative\n")), cleanToolSpecFor("npm", 1, 0))
		if item.Status != cleanUnknownLocation || item.Reasons[0] != cleanReasonInvalidOutput {
			t.Fatalf("item = %+v", item)
		}
		assertNoUsage(t, item)
	})
	t.Run("an expired context leaves every candidate not scanned", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		specs := cleanCatalog("darwin", home)
		items := cleanScan(ctx, cleanTestDeps(home, cleanStdout("/x\n")), specs)
		if len(items) != len(specs) {
			t.Fatalf("items = %d, want %d", len(items), len(specs))
		}
		for _, item := range items {
			if item.Status != cleanNotScanned || item.Reasons[0] != cleanReasonTimeout {
				t.Fatalf("item = %+v", item)
			}
			assertNoUsage(t, item)
		}
	})
	t.Run("a cancelled context says stopped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		items := cleanScan(ctx, cleanTestDeps(home, nil), cleanCatalog("linux", home))
		for _, item := range items {
			if item.Status != cleanNotScanned || item.Reasons[0] != cleanReasonStopped {
				t.Fatalf("item = %+v", item)
			}
		}
	})
	t.Run("a real unreadable subdirectory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every directory")
		}
		root := filepath.Join(home, "locked")
		cleanTestTree(t, root, 8<<10)
		locked := filepath.Join(root, "sub")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(locked, 0o755)
		item := single(t, cleanTestDeps(home, nil), cleanFixedSpec("x", root))
		if item.Status != cleanPartial || !item.LowerBound || item.Unreadable != 1 {
			t.Fatalf("item = %+v", item)
		}
	})
}

func TestCleanMeasuresOneCandidateAtATime(t *testing.T) {
	home := cleanTestHome(t)
	var running, peak, calls atomic.Int32
	deps := cleanTestDeps(home, nil)
	deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		running.Add(-1)
		return cleanFakeScan(4096), nil
	}
	var specs []cleanSpec
	for _, name := range []string{"a", "b", "c", "d"} {
		path := filepath.Join(home, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, cleanFixedSpec(name, path))
	}
	cleanScan(context.Background(), deps, specs)
	if calls.Load() != 4 || peak.Load() != 1 {
		t.Fatalf("calls %d, peak %d", calls.Load(), peak.Load())
	}
}

func cleanRunDeps(t *testing.T, home string) cleanDeps {
	t.Helper()
	run := func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
		switch spec.Name {
		case "go":
			return cleanToolOutput{Stdout: filepath.Join(home, "go-build") + "\n" + filepath.Join(home, "go-mod") + "\n"}
		default:
			return cleanToolOutput{Stdout: filepath.Join(home, spec.Name) + "\n"}
		}
	}
	return cleanTestDeps(home, run)
}

func TestRunCleanRejectsPositionalArguments(t *testing.T) {
	home := cleanTestHome(t)
	var output bytes.Buffer
	if code := runCleanWith([]string{"somewhere"}, "test", cleanRunDeps(t, home), true, nil, &output); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunCleanJSONHasTheThreeMetricsAndNoReclaimableKey(t *testing.T) {
	home := cleanTestHome(t)
	cleanTestTree(t, filepath.Join(home, "npm"), 8<<10)
	path := filepath.Join(t.TempDir(), "out.json")
	var output bytes.Buffer
	if code := runCleanWith([]string{"--json", path}, "test", cleanRunDeps(t, home), true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"deleted_open", "reclaimable"} {
		if strings.Contains(string(data), key) {
			t.Fatalf("JSON contains %q", key)
		}
	}
	var report struct {
		Results []struct {
			Probe   string                     `json:"probe"`
			Status  string                     `json:"status"`
			Metrics map[string]json.RawMessage `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if result.Probe != "clean.scan" {
		t.Fatalf("probe = %q", result.Probe)
	}
	for _, key := range []string{"candidates", "filesystems", "not_measured"} {
		if _, ok := result.Metrics[key]; !ok {
			t.Fatalf("metrics lack %q", key)
		}
	}
	var candidates []map[string]interface{}
	if err := json.Unmarshal(result.Metrics["candidates"], &candidates); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		for _, key := range []string{"id", "category", "priority", "status", "located_by", "regenerable", "regen_cost"} {
			if _, ok := candidate[key]; !ok {
				t.Fatalf("candidate %v lacks %q", candidate["id"], key)
			}
		}
		_, hasUsage := candidate["usage_bytes"]
		measured := candidate["status"] == cleanMeasured || candidate["status"] == cleanPartial
		if hasUsage != measured {
			t.Fatalf("candidate %v: usage_bytes %v with status %v", candidate["id"], hasUsage, candidate["status"])
		}
		if measured {
			for _, key := range []string{"measured_at", "elapsed_ms", "entries", "filesystem", "reported_path", "resolved_path"} {
				if _, ok := candidate[key]; !ok {
					t.Fatalf("measured candidate %v lacks %q", candidate["id"], key)
				}
			}
		}
	}
	var notMeasured []cleanNotMeasuredItem
	if err := json.Unmarshal(result.Metrics["not_measured"], &notMeasured); err != nil || !reflect.DeepEqual(notMeasured, cleanNotMeasured) {
		t.Fatalf("not_measured = %+v (%v)", notMeasured, err)
	}
}

func TestRunCleanTextNeverShowsAbsentAsZeroBytes(t *testing.T) {
	home := cleanTestHome(t)
	var output bytes.Buffer
	if code := runCleanWith(nil, "test", cleanRunDeps(t, home), true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	text := output.String()
	if strings.Contains(text, "0 B") {
		t.Fatalf("text shows 0 B:\n%s", text)
	}
	if !strings.Contains(text, "not found") || !strings.Contains(text, "not measured:") {
		t.Fatalf("text = %s", text)
	}
	if !strings.HasPrefix(text, "PASS") {
		t.Fatalf("a run with only absent candidates should pass:\n%s", text)
	}
}

func TestCleanPrintSkipsTheResultLineTheSpinnerLeft(t *testing.T) {
	items := []cleanItem{{ID: "go-build", Category: "build_cache", Priority: "P1", Status: cleanAbsent}}
	result := cleanResult(Result{Probe: cleanProbeID}, items, nil, time.Minute, nil)
	var plain, shown bytes.Buffer
	cleanPrint(&plain, commonOptions{}, result, false, 0, false)
	cleanPrint(&shown, commonOptions{}, result, false, 0, true)
	line := formatResultLine(result, false)
	if !strings.HasPrefix(plain.String(), line) {
		t.Fatalf("plain output should start with the result line:\n%s", plain.String())
	}
	if strings.Contains(shown.String(), line) || shown.String() != strings.TrimPrefix(plain.String(), line) {
		t.Fatalf("output after the spinner should keep everything but the result line:\n%s", shown.String())
	}
}

func TestRunCleanFailsWithoutAHomeDirectory(t *testing.T) {
	var output bytes.Buffer
	deps := cleanRunDeps(t, t.TempDir())
	if code := runCleanWith(nil, "test", deps, true, errors.New("no home"), &output); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestCleanResultWarnsAndOffersALongerLimitOnlyForATimeout(t *testing.T) {
	items := []cleanItem{{ID: "a", Status: cleanNotScanned, Reasons: []string{cleanReasonTimeout}}}
	timed := cleanResult(Result{}, items, nil, time.Minute, context.DeadlineExceeded)
	if timed.Status != StatusWarn || timed.Next != "edc clean --timeout 4m0s" {
		t.Fatalf("timed = %+v", timed)
	}
	stopped := cleanResult(Result{}, items, nil, time.Minute, context.Canceled)
	if stopped.Status != StatusWarn || stopped.Next != "" {
		t.Fatalf("stopped = %+v", stopped)
	}
}

func TestCleanHelpSaysItOnlyReads(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		var output strings.Builder
		if !printCommandHelp(&output, "clean") {
			t.Fatalf("clean help is missing for %s", language)
		}
		notes := TList("command.clean.notes")
		if len(notes) == 0 || !strings.Contains(output.String(), notes[0]) {
			t.Fatalf("%s help lacks the read-only note: %q", language, output.String())
		}
		setLanguage(defaultLanguage)
		if first := TList("command.clean.notes")[0]; first != "It reads only. It changes and deletes nothing." {
			t.Fatalf("first note = %q", first)
		}
		setLanguage(language)
	}
}

func TestCleanMessagesCoverEveryCode(t *testing.T) {
	var keys []string
	for _, status := range cleanStatuses {
		keys = append(keys, "clean.status."+status)
	}
	for _, reason := range cleanReasons {
		keys = append(keys, "clean.reason."+reason)
	}
	for _, category := range cleanCategories {
		keys = append(keys, "clean.category."+category)
	}
	for _, cost := range cleanCosts {
		keys = append(keys, "clean.cost."+cost)
	}
	for _, id := range cleanSpecIDs {
		keys = append(keys, "clean.candidate."+id)
	}
	for _, item := range cleanNotMeasured {
		keys = append(keys, "clean.not_measured."+item.ID, "clean.not_measured_reason."+item.Reason)
	}
	for _, language := range supportedLanguages {
		for _, key := range keys {
			if !hasMessage(language, key) {
				t.Errorf("%s lacks %s", language, key)
			}
		}
	}
	declared := map[string]bool{}
	for _, id := range cleanSpecIDs {
		declared[id] = true
	}
	for _, spec := range cleanCatalog("darwin", "/h") {
		if !declared[spec.id] || !contains(cleanCategories, spec.category) || !contains(cleanCosts, spec.cost) {
			t.Errorf("catalog entry %s uses a value that has no message", spec.id)
		}
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
