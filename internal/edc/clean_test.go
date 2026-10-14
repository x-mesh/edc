package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	return cleanSpec{id: id, category: "build_cache", priority: "P1", fixed: path, regenerable: true, cost: "low"}
}

func cleanToolSpecFor(id string, lines, line int) cleanSpec {
	return cleanSpec{id: id, category: "package_cache", priority: "P1", tool: id, args: []string{"where"}, lines: lines, line: line,
		regenerable: true, cost: "medium"}
}

func cleanStdout(text string) func(context.Context, cleanToolSpec) cleanToolOutput {
	return func(context.Context, cleanToolSpec) cleanToolOutput { return cleanToolOutput{Stdout: text} }
}

func cleanTestDeps(home string, run func(context.Context, cleanToolSpec) cleanToolOutput) cleanDeps {
	host, _ := hostDuSources()
	return cleanDeps{
		home: home, run: run, measure: duMeasure, sources: cleanSources(host),
		lookupEnv: func(string) (string, bool) { return "", false }, deviceOf: cleanDeviceOf,
		// 실행 중인 docker에 연결하면 결과가 호스트마다 달라진다. docker를 찾지 못하게 하고 연결하면 시험을 멈춘다.
		lookPath: func(name string) (string, error) {
			if name == "docker" {
				return "", errors.New("not found")
			}
			return "/bin/" + name, nil
		},
		dockerDial: func(context.Context, string) (net.Conn, error) { panic("a test dialed the Docker socket") },
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
	shared := []string{"cargo-registry", "cargo-git", "zig", "bun", "pip", "playwright", "puppeteer", "node-gyp"}
	linux := append(append(append([]string{}, common...), shared...), "huggingface", "rustup-toolchains")
	if got := ids("linux"); !reflect.DeepEqual(got, linux) {
		t.Fatalf("linux = %v", got)
	}
	darwin := append(append([]string{}, common...), "xcode-derived-data", "swiftpm")
	darwin = append(append(darwin, shared...), "electron", "xcode-device-support", "coresimulator-caches", "app-updates", "app-logs",
		"huggingface", "rustup-toolchains", "ollama", "app-caches", "dot-cache")
	if got := ids("darwin"); !reflect.DeepEqual(got, darwin) {
		t.Fatalf("darwin = %v", got)
	}
	if !reflect.DeepEqual(darwin, cleanSpecIDs) {
		t.Fatalf("cleanSpecIDs = %v", cleanSpecIDs)
	}
	cleanups := map[string]string{}
	tools := map[string]string{}
	for _, goos := range []string{"darwin", "linux"} {
		for _, spec := range cleanCatalog(goos, "/Users/me") {
			cleanups[spec.id] = spec.cleanup
			if spec.tool != "" {
				tools[spec.id] = spec.tool
			}
			if spec.fixed != "" {
				if !strings.HasPrefix(spec.fixed, "/Users/me/") {
					t.Fatalf("%s default path = %q", spec.id, spec.fixed)
				}
				if goos == "linux" && strings.Contains(spec.fixed, "/Library/") {
					t.Fatalf("linux %s default path = %q", spec.id, spec.fixed)
				}
			}
			if spec.fixed == "" && spec.tool == "" {
				t.Fatalf("%s has no location", spec.id)
			}
		}
	}
	want := map[string]string{
		"go-build": "go clean -cache", "go-mod": "go clean -modcache", "npm": "npm cache clean --force",
		"pnpm": "pnpm store prune", "uv": "uv cache clean", "homebrew": "brew cleanup -s",
		"xcode-derived-data": "", "swiftpm": "swift package purge-cache",
		"cargo-registry": "", "cargo-git": "", "zig": "", "bun": "bun pm cache rm", "pip": "pip3 cache purge",
		"playwright": "", "puppeteer": "", "node-gyp": "", "electron": "", "coresimulator-caches": "",
		"huggingface": "hf cache delete", "rustup-toolchains": "rustup toolchain uninstall <toolchain>", "ollama": "",
		"xcode-device-support": "", "app-updates": "", "app-logs": "", "app-caches": "", "dot-cache": "",
	}
	if !reflect.DeepEqual(cleanups, want) {
		t.Fatalf("cleanups = %v", cleanups)
	}
	// 새 후보 중 도구에 위치를 묻는 것은 pip뿐이다. bun, zig, rustup은 조회만으로 폴더를 만든다.
	wantTools := map[string]string{"go-build": "go", "go-mod": "go", "npm": "npm", "pnpm": "pnpm", "uv": "uv", "homebrew": "brew", "pip": "pip3"}
	if !reflect.DeepEqual(tools, wantTools) {
		t.Fatalf("tools = %v", tools)
	}
}

func cleanEnvironment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, set := values[name]
		return value, set
	}
}

func TestCleanLocatesFromEnvironmentInOrder(t *testing.T) {
	home := cleanTestHome(t)
	other := filepath.Join(t.TempDir(), "x")
	catalog := func(goos string) map[string]cleanSpec {
		specs := map[string]cleanSpec{}
		for _, spec := range cleanCatalog(goos, home) {
			specs[spec.id] = spec
		}
		return specs
	}
	locate := func(spec cleanSpec, values map[string]string) (cleanItem, int) {
		var calls atomic.Int32
		deps := cleanTestDeps(home, nil)
		deps.lookupEnv = cleanEnvironment(values)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
			calls.Add(1)
			return cleanFakeScan(4096), nil
		}
		item := cleanScan(context.Background(), deps, []cleanSpec{spec})[0]
		return item, int(calls.Load())
	}
	tests := []struct {
		goos, id string
		values   map[string]string
		path     string
		by       string
	}{
		{"linux", "cargo-registry", nil, filepath.Join(home, ".cargo/registry"), "default_path"},
		{"linux", "cargo-registry", map[string]string{"CARGO_HOME": other}, filepath.Join(other, "registry"), "env:CARGO_HOME"},
		{"linux", "cargo-git", map[string]string{"CARGO_HOME": other}, filepath.Join(other, "git"), "env:CARGO_HOME"},
		{"linux", "zig", nil, filepath.Join(home, ".cache/zig"), "default_path"},
		{"linux", "zig", map[string]string{"XDG_CACHE_HOME": other}, filepath.Join(other, "zig"), "env:XDG_CACHE_HOME"},
		{"linux", "zig", map[string]string{"XDG_CACHE_HOME": other, "ZIG_GLOBAL_CACHE_DIR": other + "/z"}, other + "/z", "env:ZIG_GLOBAL_CACHE_DIR"},
		{"linux", "bun", nil, filepath.Join(home, ".bun/install/cache"), "default_path"},
		{"linux", "bun", map[string]string{"BUN_INSTALL": other, "XDG_CACHE_HOME": other + "/xdg"}, filepath.Join(other, "install/cache"), "env:BUN_INSTALL"},
		{"linux", "bun", map[string]string{"XDG_CACHE_HOME": other}, filepath.Join(other, ".bun/install/cache"), "env:XDG_CACHE_HOME"},
		{"linux", "bun", map[string]string{"BUN_INSTALL_CACHE_DIR": other + "/c", "BUN_INSTALL": other + "/i"}, other + "/c", "env:BUN_INSTALL_CACHE_DIR"},
		{"linux", "playwright", nil, filepath.Join(home, ".cache/ms-playwright"), "default_path"},
		{"linux", "playwright", map[string]string{"XDG_CACHE_HOME": other}, filepath.Join(other, "ms-playwright"), "env:XDG_CACHE_HOME"},
		{"darwin", "playwright", nil, filepath.Join(home, "Library/Caches/ms-playwright"), "default_path"},
		{"darwin", "playwright", map[string]string{"XDG_CACHE_HOME": other}, filepath.Join(home, "Library/Caches/ms-playwright"), "default_path"},
		{"darwin", "playwright", map[string]string{"PLAYWRIGHT_BROWSERS_PATH": other}, other, "env:PLAYWRIGHT_BROWSERS_PATH"},
		{"linux", "puppeteer", nil, filepath.Join(home, ".cache/puppeteer"), "default_path"},
		{"linux", "puppeteer", map[string]string{"PUPPETEER_CACHE_DIR": other}, other, "env:PUPPETEER_CACHE_DIR"},
		{"linux", "node-gyp", map[string]string{"XDG_CACHE_HOME": other}, filepath.Join(other, "node-gyp"), "env:XDG_CACHE_HOME"},
		{"darwin", "node-gyp", nil, filepath.Join(home, "Library/Caches/node-gyp"), "default_path"},
		{"darwin", "node-gyp", map[string]string{"npm_config_devdir": other}, other, "env:npm_config_devdir"},
		{"darwin", "electron", nil, filepath.Join(home, "Library/Caches/electron"), "default_path"},
		{"darwin", "electron", map[string]string{"electron_config_cache": other}, other, "env:electron_config_cache"},
		{"darwin", "coresimulator-caches", map[string]string{"HOME": other}, filepath.Join(home, "Library/Developer/CoreSimulator/Caches"), "fixed_path"},
		{"linux", "huggingface", nil, filepath.Join(home, ".cache/huggingface/hub"), "default_path"},
		{"linux", "huggingface", map[string]string{"HF_HOME": other}, filepath.Join(other, "hub"), "env:HF_HOME"},
		{"linux", "huggingface", map[string]string{"HF_HOME": other, "HF_HUB_CACHE": other + "/h"}, other + "/h", "env:HF_HUB_CACHE"},
		{"linux", "huggingface", map[string]string{"HF_HOME": other, "HUGGINGFACE_HUB_CACHE": other + "/h"}, other + "/h", "env:HUGGINGFACE_HUB_CACHE"},
		{"linux", "huggingface", map[string]string{"XDG_CACHE_HOME": other}, filepath.Join(other, "huggingface/hub"), "env:XDG_CACHE_HOME"},
		{"linux", "rustup-toolchains", map[string]string{"RUSTUP_HOME": other}, filepath.Join(other, "toolchains"), "env:RUSTUP_HOME"},
		{"darwin", "ollama", nil, filepath.Join(home, ".ollama/models"), "default_path"},
		{"darwin", "ollama", map[string]string{"OLLAMA_MODELS": other}, other, "env:OLLAMA_MODELS"},
	}
	for _, test := range tests {
		t.Run(test.goos+"/"+test.id+"/"+test.by, func(t *testing.T) {
			spec, ok := catalog(test.goos)[test.id]
			if !ok {
				t.Fatalf("%s has no %s", test.goos, test.id)
			}
			item, _ := locate(spec, test.values)
			if item.ReportedPath != test.path || item.LocatedBy != test.by {
				t.Fatalf("reported %q by %q, want %q by %q", item.ReportedPath, item.LocatedBy, test.path, test.by)
			}
		})
	}
	invalid := []struct {
		id     string
		values map[string]string
		name   string
	}{
		{"playwright", map[string]string{"PLAYWRIGHT_BROWSERS_PATH": "0"}, "PLAYWRIGHT_BROWSERS_PATH"},
		{"huggingface", map[string]string{"HF_HOME": "~/hf"}, "HF_HOME"},
		{"zig", map[string]string{"ZIG_GLOBAL_CACHE_DIR": ""}, "ZIG_GLOBAL_CACHE_DIR"},
		{"bun", map[string]string{"BUN_INSTALL": "rel", "XDG_CACHE_HOME": other}, "BUN_INSTALL"},
		{"huggingface", map[string]string{"HF_HUB_CACHE": "", "HF_HOME": other}, "HF_HUB_CACHE"},
	}
	for _, test := range invalid {
		t.Run("invalid "+test.id+" "+test.name, func(t *testing.T) {
			item, calls := locate(catalog("linux")[test.id], test.values)
			if item.Status != cleanUnknownLocation || len(item.Reasons) != 1 || item.Reasons[0] != cleanReasonInvalidEnv || calls != 0 {
				t.Fatalf("item = %+v, measure calls %d", item, calls)
			}
			if want := test.name + "=" + test.values[test.name]; item.Detail != want || item.LocatedBy != "env:"+test.name {
				t.Fatalf("detail %q located by %q", item.Detail, item.LocatedBy)
			}
			data, err := json.Marshal(item)
			if err != nil || strings.Contains(string(data), "usage_bytes") || strings.Contains(string(data), "reported_path") {
				t.Fatalf("JSON = %s (%v)", data, err)
			}
		})
	}
	t.Run("a variable that covers the home directory is rejected", func(t *testing.T) {
		item, calls := locate(catalog("linux")["puppeteer"], map[string]string{"PUPPETEER_CACHE_DIR": home})
		if item.Status != cleanRejected || item.Reasons[0] != cleanReasonCoversUserData || calls != 0 {
			t.Fatalf("item = %+v, measure calls %d", item, calls)
		}
		item, calls = locate(catalog("linux")["huggingface"], map[string]string{"HF_HUB_CACHE": filepath.Dir(home)})
		if item.Status != cleanRejected || calls != 0 {
			t.Fatalf("item = %+v, measure calls %d", item, calls)
		}
	})
	t.Run("pip asks the tool and does not guess when it fails", func(t *testing.T) {
		deps := cleanTestDeps(home, func(context.Context, cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Err: errors.New("exit status 1"), ExitCode: 1, Stderr: "ERROR: pip cache commands can not function since cache is disabled.\n"}
		})
		item := cleanScan(context.Background(), deps, []cleanSpec{catalog("linux")["pip"]})[0]
		if item.Status != cleanUnknownLocation || item.Reasons[0] != cleanReasonToolFailed || !strings.HasPrefix(item.Detail, "exit 1: ERROR") ||
			item.ReportedPath != "" || item.LocatedBy != "pip3 cache dir" {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("the catalog never runs xcrun", func(t *testing.T) {
		for _, spec := range cleanCatalog("darwin", home) {
			if spec.tool == "xcrun" {
				t.Fatalf("%s asks xcrun", spec.id)
			}
		}
		for _, item := range cleanNotMeasured {
			if item.ID == "simulators" {
				return
			}
		}
		t.Fatal("not_measured lacks simulators")
	})
}

func TestCleanRunnerGetsHomeDirectoryAndQuietEnvironment(t *testing.T) {
	home := cleanTestHome(t)
	var specs []cleanToolSpec
	run := func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
		specs = append(specs, spec)
		return cleanToolOutput{Stdout: filepath.Join(home, "x", "a") + "\n" + filepath.Join(home, "x", "b") + "\n"}
	}
	items := cleanScan(context.Background(), cleanTestDeps(home, run), cleanCatalog("linux", home))
	if want := len(cleanCatalog("linux", home)); len(items) != want {
		t.Fatalf("items = %d, want %d", len(items), want)
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

// cleanRecorder는 measure가 받은 root를 순서대로 모은다.
type cleanRecorder struct {
	mu    sync.Mutex
	roots []string
}

func (recorder *cleanRecorder) measure(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
	recorder.mu.Lock()
	recorder.roots = append(recorder.roots, options.root)
	recorder.mu.Unlock()
	return duMeasure(ctx, options, sources)
}

// touching은 root와 같거나 root를 품은 measure 호출 수다.
func (recorder *cleanRecorder) touching(root string) int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	count := 0
	for _, measured := range recorder.roots {
		if pathUnder(root, measured) {
			count++
		}
	}
	return count
}

func cleanWriteFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cleanAbsentTools는 모든 도구가 홈 아래 없는 폴더를 보고하게 한다.
func cleanAbsentTools(home string) func(context.Context, cleanToolSpec) cleanToolOutput {
	return func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
		path := filepath.Join(home, "absent", spec.Name)
		if spec.Name == "go" {
			return cleanToolOutput{Stdout: path + "-build\n" + path + "-mod\n"}
		}
		return cleanToolOutput{Stdout: path + "\n"}
	}
}

func cleanItemByID(t *testing.T, items []cleanItem, id string) cleanItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("no candidate %s", id)
	return cleanItem{}
}

func cleanGroupNames(item cleanItem) []string {
	var names []string
	for _, group := range item.Groups {
		names = append(names, group.Name)
	}
	sort.Strings(names)
	return names
}

func TestCleanBoundaryUserDataIsNeverACache(t *testing.T) {
	home := cleanTestHome(t)
	library := filepath.Join(home, "Library")
	files := []string{
		"Developer/Xcode/Archives/a/f", "Developer/Xcode/UserData/u/f", "Developer/Xcode/DerivedData/d/f",
		"Developer/Xcode/iOS DeviceSupport/s/f", "Developer/Xcode/watchOS Notes/n",
		"Developer/CoreSimulator/Devices/x/f", "Application Support/app/f", "Containers/c/f", "Group Containers/g/f",
		"Caches/appA/f", "Logs/l/f",
	}
	for _, name := range files {
		cleanWriteFile(t, filepath.Join(library, name), 8<<10)
	}
	if err := os.MkdirAll(filepath.Join(library, "Developer/CoreSimulator/Caches"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(library, "Caches/link")
	if err := os.Symlink("../Application Support", link); err != nil {
		t.Fatal(err)
	}
	user := []string{
		filepath.Join(library, "Developer/Xcode/Archives"), filepath.Join(library, "Developer/Xcode/UserData"),
		filepath.Join(library, "Developer/CoreSimulator/Devices"), filepath.Join(library, "Application Support"),
		filepath.Join(library, "Containers"), filepath.Join(library, "Group Containers"),
	}
	assertAvoids := func(t *testing.T, recorder *cleanRecorder) {
		t.Helper()
		for _, root := range recorder.roots {
			if root == link {
				t.Errorf("measured the symlink %s", root)
			}
			for _, forbidden := range user {
				if pathUnder(root, forbidden) || pathUnder(forbidden, root) {
					t.Errorf("measured %s, which is or touches the user data %s", root, forbidden)
				}
			}
			if pathUnder(home, root) {
				t.Errorf("measured %s, which is the home directory or above it", root)
			}
		}
	}
	scan := func(env map[string]string) ([]cleanItem, *cleanRecorder) {
		recorder := &cleanRecorder{}
		deps := cleanTestDeps(home, cleanAbsentTools(home))
		deps.lookupEnv = cleanEnvironment(env)
		deps.measure = recorder.measure
		return cleanScan(context.Background(), deps, cleanCatalog("darwin", home)), recorder
	}
	t.Run("the default catalog", func(t *testing.T) {
		items, recorder := scan(nil)
		assertAvoids(t, recorder)
		if len(recorder.roots) == 0 {
			t.Fatal("nothing was measured")
		}
		support := cleanItemByID(t, items, "xcode-device-support")
		if !reflect.DeepEqual(cleanGroupNames(support), []string{"iOS DeviceSupport"}) || support.Match != "*DeviceSupport" {
			t.Fatalf("device support = %+v", support)
		}
		for _, id := range []string{"app-caches", "dot-cache", "app-updates", "app-logs"} {
			if item := cleanItemByID(t, items, id); item.Regenerable {
				t.Errorf("%s is marked regenerable", id)
			}
		}
		if got := cleanGroupNames(cleanItemByID(t, items, "app-caches")); !reflect.DeepEqual(got, []string{"appA"}) {
			t.Fatalf("app-caches groups = %v", got)
		}
		if state := cleanItemByID(t, items, "coresimulator-caches"); state.Status != cleanMeasured || state.UsageBytes == nil {
			t.Fatalf("coresimulator-caches = %+v", state)
		}
	})
	t.Run("a variable that points at the home directory or above", func(t *testing.T) {
		items, recorder := scan(map[string]string{"PUPPETEER_CACHE_DIR": home, "HF_HUB_CACHE": filepath.Dir(home)})
		assertAvoids(t, recorder)
		for _, id := range []string{"puppeteer", "huggingface"} {
			if item := cleanItemByID(t, items, id); item.Status != cleanRejected || item.Reasons[0] != cleanReasonCoversUserData || item.UsageBytes != nil {
				t.Fatalf("%s = %+v", id, item)
			}
		}
	})
}

func TestCleanBoundaryRemainderSkipsCandidatePaths(t *testing.T) {
	home := cleanTestHome(t)
	caches, dot := filepath.Join(home, "Library/Caches"), filepath.Join(home, ".cache")
	for _, name := range []string{
		"Library/Caches/go-build/f", "Library/Caches/pip/f", "Library/Caches/ms-playwright/f", "Library/Caches/Foo.ShipIt/f",
		"Library/Caches/appA/f", "Library/Caches/appB/sub/f", "Library/Caches/pnpm/f",
		".cache/uv/f", ".cache/zig/f", ".cache/puppeteer/f", ".cache/huggingface/hub/m", ".cache/huggingface/xet/x", ".cache/other/f",
	} {
		cleanWriteFile(t, filepath.Join(home, name), 16<<10)
	}
	run := func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
		switch spec.Name {
		case "go":
			return cleanToolOutput{Stdout: filepath.Join(caches, "go-build") + "\n" + filepath.Join(home, "absent/go-mod") + "\n"}
		case "pip3":
			return cleanToolOutput{Stdout: filepath.Join(caches, "pip") + "\n"}
		case "uv":
			return cleanToolOutput{Stdout: filepath.Join(dot, "uv") + "\n"}
		}
		return cleanToolOutput{Stdout: filepath.Join(home, "absent", spec.Name) + "\n"}
	}
	recorder := &cleanRecorder{}
	deps := cleanTestDeps(home, run)
	deps.measure = recorder.measure
	items := cleanScan(context.Background(), deps, cleanCatalog("darwin", home))
	candidates := map[string]string{
		"go-build": filepath.Join(caches, "go-build"), "pip": filepath.Join(caches, "pip"),
		"playwright": filepath.Join(caches, "ms-playwright"), "app-updates": filepath.Join(caches, "Foo.ShipIt"),
		"uv": filepath.Join(dot, "uv"), "zig": filepath.Join(dot, "zig"), "puppeteer": filepath.Join(dot, "puppeteer"),
		"huggingface": filepath.Join(dot, "huggingface/hub"),
	}
	for id, path := range candidates {
		if calls := recorder.touching(path); calls != 1 {
			t.Errorf("%s at %s is measured by %d calls, want 1", id, path, calls)
		}
		if inside := cleanItemByID(t, items, id).Inside; inside != "" {
			t.Errorf("%s is inside %s", id, inside)
		}
	}
	if recorder.touching(filepath.Join(caches, "pnpm")) != 1 {
		t.Error("the pnpm folder of the app caches is not measured once")
	}
	appCaches, dotCache := cleanItemByID(t, items, "app-caches"), cleanItemByID(t, items, "dot-cache")
	if got := cleanGroupNames(appCaches); !reflect.DeepEqual(got, []string{"appA", "appB", "pnpm"}) {
		t.Fatalf("app-caches groups = %v", got)
	}
	if got := cleanGroupNames(dotCache); !reflect.DeepEqual(got, []string{"huggingface", "other"}) {
		t.Fatalf("dot-cache groups = %v", got)
	}
	wantExcludes := []string{"app-updates", "go-build", "pip", "playwright"}
	if !reflect.DeepEqual(appCaches.Excludes, wantExcludes) {
		t.Fatalf("app-caches excludes = %v, want %v", appCaches.Excludes, wantExcludes)
	}
	if want := []string{"huggingface", "puppeteer", "uv", "zig"}; !reflect.DeepEqual(dotCache.Excludes, want) {
		t.Fatalf("dot-cache excludes = %v, want %v", dotCache.Excludes, want)
	}
	if appCaches.Status != cleanMeasured || dotCache.Status != cleanMeasured {
		t.Fatalf("statuses = %s, %s", appCaches.Status, dotCache.Status)
	}
	sources := cleanSources(mustHostSources(t))
	whole := func(root string) uint64 {
		scan, err := duMeasure(context.Background(), duOptions{root: root, workers: duDefaultWorkers}, sources)
		if err != nil {
			t.Fatal(err)
		}
		return scan.root.total
	}
	own := func(root string) uint64 {
		info, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		return cleanBlocks(info)
	}
	for _, test := range []struct {
		root       string
		candidates []string
		remainder  cleanItem
	}{
		{caches, []string{"go-build", "pip", "playwright", "app-updates"}, appCaches},
		{dot, []string{"uv", "zig", "puppeteer", "huggingface"}, dotCache},
	} {
		sum := own(test.root) + *test.remainder.UsageBytes
		for _, id := range test.candidates {
			sum += *cleanItemByID(t, items, id).UsageBytes
		}
		if whole := whole(test.root); sum != whole {
			t.Errorf("%s: own + candidates + remainder = %d, whole = %d", test.root, sum, whole)
		}
	}
	var counted uint64
	for _, item := range items {
		if item.counted() && item.Inside == "" && item.Filesystem != nil {
			counted += *item.UsageBytes
		}
	}
	var summed uint64
	for _, filesystem := range cleanFilesystems(items, deps.sources) {
		summed += filesystem.UsageBytes
	}
	if summed != counted {
		t.Fatalf("file system sum = %d, candidates = %d", summed, counted)
	}
	t.Run("a second remainder that is part of the first is a duplicate", func(t *testing.T) {
		home := cleanTestHome(t)
		cleanWriteFile(t, filepath.Join(home, "Library/Caches/appA/f"), 8<<10)
		cleanWriteFile(t, filepath.Join(home, "Library/Caches/appB/sub/f"), 8<<10)
		if err := os.Symlink(filepath.Join(home, "Library/Caches/appB"), filepath.Join(home, ".cache")); err != nil {
			t.Fatal(err)
		}
		recorder := &cleanRecorder{}
		deps := cleanTestDeps(home, cleanAbsentTools(home))
		deps.measure = recorder.measure
		items := cleanScan(context.Background(), deps, cleanCatalog("darwin", home))
		dotCache := cleanItemByID(t, items, "dot-cache")
		if dotCache.Status != cleanDuplicate || dotCache.SameAs != "app-caches" || dotCache.UsageBytes != nil {
			t.Fatalf("dot-cache = %+v", dotCache)
		}
		if calls := recorder.touching(filepath.Join(home, "Library/Caches/appB")); calls != 1 {
			t.Fatalf("appB is measured by %d calls", calls)
		}
	})
}

func cleanChildrenSpec(parent string) cleanSpec {
	return cleanSpec{id: "app-caches", category: "app_cache", priority: "P2", fixed: parent, children: &cleanChildren{}, cost: "unknown"}
}

func cleanChildrenTree(t *testing.T) (string, string) {
	t.Helper()
	home := cleanTestHome(t)
	parent := filepath.Join(home, "Library/Caches")
	for _, name := range []string{"a", "b", "c"} {
		cleanWriteFile(t, filepath.Join(parent, name, "f"), 8<<10)
	}
	return home, parent
}

func TestCleanChildrenSkipsOtherDevicesAndStopsBetweenEntries(t *testing.T) {
	home, parent := cleanChildrenTree(t)
	var running, peak atomic.Int32
	var calls []string
	deps := cleanTestDeps(home, nil)
	deps.measure = func(_ context.Context, options duOptions, _ duSources) (duScan, error) {
		now := running.Add(1)
		if now > peak.Load() {
			peak.Store(now)
		}
		calls = append(calls, filepath.Base(options.root))
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		return cleanFakeScan(4096), nil
	}
	t.Run("another device is counted and not measured", func(t *testing.T) {
		calls = nil
		deps.deviceOf = func(path string) (uint64, error) {
			if filepath.Base(path) == "b" {
				return 2, nil
			}
			return 1, nil
		}
		item := cleanScan(context.Background(), deps, []cleanSpec{cleanChildrenSpec(parent)})[0]
		if !reflect.DeepEqual(calls, []string{"a", "c"}) || peak.Load() != 1 {
			t.Fatalf("measured %v with %d at a time", calls, peak.Load())
		}
		if item.Status != cleanPartial || !item.LowerBound || item.SkippedMounts != 1 || !reflect.DeepEqual(item.Reasons, []string{cleanReasonSkippedMounts}) {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("a stop between entries measures nothing more", func(t *testing.T) {
		calls = nil
		deps.deviceOf = func(string) (uint64, error) { return 1, nil }
		ctx, cancel := context.WithCancel(context.Background())
		deps := deps
		inner := deps.measure
		deps.measure = func(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
			scan, err := inner(ctx, options, sources)
			if len(calls) == 2 {
				cancel()
				scan.partial = true
			}
			return scan, err
		}
		item := cleanScan(ctx, deps, []cleanSpec{cleanChildrenSpec(parent)})[0]
		if len(calls) != 2 || item.Status != cleanPartial || !item.LowerBound || item.Reasons[0] != cleanReasonStopped || item.UsageBytes == nil {
			t.Fatalf("calls %v, item = %+v", calls, item)
		}
	})
	t.Run("an expired context leaves the candidate not scanned", func(t *testing.T) {
		calls = nil
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		item := cleanScan(ctx, deps, []cleanSpec{cleanChildrenSpec(parent)})[0]
		if item.Status != cleanNotScanned || item.Reasons[0] != cleanReasonTimeout || len(calls) != 0 {
			t.Fatalf("calls %v, item = %+v", calls, item)
		}
	})
}

func TestCleanChildrenFailuresAreCounted(t *testing.T) {
	home, parent := cleanChildrenTree(t)
	deps := cleanTestDeps(home, nil)
	deps.measure = func(_ context.Context, options duOptions, _ duSources) (duScan, error) {
		switch filepath.Base(options.root) {
		case "a":
			return duScan{}, &fs.PathError{Op: "open", Path: options.root, Err: syscall.ENOENT}
		case "b":
			return duScan{}, &fs.PathError{Op: "open", Path: options.root, Err: syscall.EACCES}
		}
		return cleanFakeScan(4096), nil
	}
	item := cleanScan(context.Background(), deps, []cleanSpec{cleanChildrenSpec(parent)})[0]
	if item.Status != cleanPartial || !item.LowerBound || item.Unreadable != 1 || !reflect.DeepEqual(item.Reasons, []string{cleanReasonUnreadableDirs}) {
		t.Fatalf("item = %+v", item)
	}
	if want := []string{filepath.Join(parent, "b")}; !reflect.DeepEqual(item.UnreadablePaths, want) {
		t.Fatalf("unreadable paths = %v", item.UnreadablePaths)
	}
	if item.UsageBytes == nil || *item.UsageBytes != 4096 || !reflect.DeepEqual(cleanGroupNames(item), []string{"c"}) {
		t.Fatalf("item = %+v", item)
	}
}

func TestCleanPatternWithoutMatchIsAbsent(t *testing.T) {
	home := cleanTestHome(t)
	cleanWriteFile(t, filepath.Join(home, "Library/Developer/Xcode/DerivedData/d/f"), 8<<10)
	var support cleanSpec
	for _, spec := range cleanCatalog("darwin", home) {
		if spec.id == "xcode-device-support" {
			support = spec
		}
	}
	var measured atomic.Int32
	deps := cleanTestDeps(home, nil)
	deps.measure = func(context.Context, duOptions, duSources) (duScan, error) {
		measured.Add(1)
		return cleanFakeScan(4096), nil
	}
	item := cleanScan(context.Background(), deps, []cleanSpec{support})[0]
	data, err := json.Marshal(item)
	if err != nil || item.Status != cleanAbsent || item.UsageBytes != nil || strings.Contains(string(data), "usage_bytes") || measured.Load() != 0 {
		t.Fatalf("item = %s (%v), measured %d", data, err, measured.Load())
	}
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
	if strings.Contains(string(data), "deleted_open") {
		t.Fatal("JSON contains deleted_open")
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
	for _, key := range []string{"candidates", "filesystems", "docker", "vm_disks", "worktrees", "not_measured"} {
		if _, ok := result.Metrics[key]; !ok {
			t.Fatalf("metrics lack %q", key)
		}
	}
	// 회수 가능량은 Docker 엔진이 보고하는 값만 낸다. 후보와 파일 시스템 합계에는 없다.
	for _, key := range []string{"candidates", "filesystems"} {
		if strings.Contains(string(result.Metrics[key]), "reclaimable") {
			t.Fatalf("%s contain a reclaimable value", key)
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
	if !strings.Contains(text, "\nnot found: ") || !strings.Contains(text, "not measured:") {
		t.Fatalf("text = %s", text)
	}
	if !strings.HasPrefix(text, "PASS") {
		t.Fatalf("a run with only absent candidates should pass:\n%s", text)
	}
}

func cleanSizedItem(id, category, priority string, bytes uint64) cleanItem {
	return cleanItem{ID: id, Category: category, Priority: priority, Status: cleanMeasured, UsageBytes: &bytes,
		ReportedPath: "/h/" + id, ResolvedPath: "/h/" + id, RegenCost: "low"}
}

func TestCleanTextCollapsesQuietStatusesAndSortsByPriority(t *testing.T) {
	items := []cleanItem{
		cleanSizedItem("huggingface", "model", "P3", 3<<30),
		{ID: "playwright", Category: "downloaded_binary", Priority: "P2", Status: cleanAbsent, ReportedPath: "/h/pw"},
		cleanSizedItem("go-build", "build_cache", "P1", 1<<30),
		{ID: "pip", Category: "package_cache", Priority: "P1", Status: cleanToolAbsent, LocatedBy: "pip3 cache dir"},
		cleanSizedItem("puppeteer", "downloaded_binary", "P2", 2<<30),
		{ID: "dot-cache", Category: "app_cache", Priority: "P2", Status: cleanDuplicate, SameAs: "app-caches", ReportedPath: "/h/.cache"},
		{ID: "zig", Category: "build_cache", Priority: "P1", Status: cleanUnknownLocation, Reasons: []string{cleanReasonInvalidEnv}},
		cleanSizedItem("coresimulator-caches", "xcode_support", "P2", 0),
	}
	text := formatCleanItems(items, false, 0)
	order := []string{"Go build cache", "Zig cache", "Puppeteer browsers", "CoreSimulator caches", "Hugging Face models"}
	last := -1
	for _, name := range order {
		index := strings.Index(text, name)
		if index <= last {
			t.Fatalf("%q is out of order in:\n%s", name, text)
		}
		last = index
	}
	for _, want := range []string{"not found: Playwright browsers\n", "tool not found: pip cache (pip3)\n",
		"duplicate: other ~/.cache folders = other app caches\n", "environment variable is not an absolute path"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if lines := strings.Count(text, "\n"); lines != 8 {
		t.Fatalf("default output has %d lines:\n%s", lines, text)
	}
	if !strings.Contains(text, " 0 B ") {
		t.Fatalf("a measured 0 B should show:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "0 B") && !strings.Contains(line, "CoreSimulator") {
			t.Fatalf("%q shows 0 B", line)
		}
	}
	verbose := formatCleanItems(items, true, 0)
	for _, name := range []string{"Playwright browsers", "pip cache", "other ~/.cache folders"} {
		if !strings.Contains(verbose, "  "+name+" ") && !strings.Contains(verbose, name+"  ") {
			t.Fatalf("-v lacks a line for %q:\n%s", name, verbose)
		}
	}
	if strings.Contains(verbose, "not found: ") {
		t.Fatalf("-v folds the quiet candidates:\n%s", verbose)
	}
	if items[0].ID != "huggingface" {
		t.Fatal("formatting reordered the caller's slice")
	}
}

func TestCleanTextCapsGroups(t *testing.T) {
	item := cleanSizedItem("app-caches", "app_cache", "P2", 1<<30)
	for index := 0; index < 40; index++ {
		item.Groups = append(item.Groups, cleanGroup{Name: fmt.Sprintf("app%02d", index), Folders: 1, UsageBytes: uint64(40-index) << 20})
	}
	count := func(text string) int { return strings.Count(text, "folder(s)") }
	normal := formatCleanItems([]cleanItem{item}, false, 0)
	if count(normal) != 5 || !strings.Contains(normal, "35 more, -v shows more") {
		t.Fatalf("default output:\n%s", normal)
	}
	verbose := formatCleanItems([]cleanItem{item}, true, 0)
	if count(verbose) != 30 || !strings.Contains(verbose, "10 more, --json shows all") {
		t.Fatalf("-v output:\n%s", verbose)
	}
	if len(item.Groups) != 40 {
		t.Fatal("capping changed the groups")
	}
}

func TestCleanTextWrapsInsteadOfCutting(t *testing.T) {
	squeeze := func(text string) string { return strings.Join(strings.Fields(text), "") }
	text := formatCleanNotMeasured(false, 40)
	for _, item := range cleanNotMeasured {
		if !strings.Contains(squeeze(text), squeeze(T("clean.not_measured."+item.ID))) {
			t.Errorf("not measured lost %s:\n%s", item.ID, text)
		}
	}
	if len(strings.Split(strings.TrimSpace(text), "\n")) < 2 {
		t.Fatalf("a narrow screen should wrap:\n%s", text)
	}
	var absent []cleanItem
	for _, spec := range cleanCatalog("darwin", "/h") {
		absent = append(absent, cleanItem{ID: spec.id, Priority: spec.priority, Status: cleanAbsent})
	}
	folded := formatCleanItems(absent, false, 40)
	for _, spec := range cleanCatalog("darwin", "/h") {
		if !strings.Contains(squeeze(folded), squeeze(T("clean.candidate."+spec.id))) {
			t.Errorf("the folded line lost %s:\n%s", spec.id, folded)
		}
	}
	if wide := formatCleanNotMeasured(false, 0); strings.Count(wide, "\n") != 2 {
		t.Fatalf("a pipe should get one line:\n%s", wide)
	}
}

func TestCleanPrintSkipsTheResultLineTheSpinnerLeft(t *testing.T) {
	items := []cleanItem{{ID: "go-build", Category: "build_cache", Priority: "P1", Status: cleanAbsent}}
	result := cleanResult(Result{Probe: cleanProbeID}, cleanReport{candidates: items}, time.Minute, nil)
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
	timed := cleanResult(Result{}, cleanReport{candidates: items}, time.Minute, context.DeadlineExceeded)
	if timed.Status != StatusWarn || timed.Next != "edc clean --timeout 4m0s" {
		t.Fatalf("timed = %+v", timed)
	}
	stopped := cleanResult(Result{}, cleanReport{candidates: items}, time.Minute, context.Canceled)
	if stopped.Status != StatusWarn || stopped.Next != "" {
		t.Fatalf("stopped = %+v", stopped)
	}
}

func TestCleanMeasurePathMapsScanStates(t *testing.T) {
	measure := func(ctx context.Context, scan duScan, err error) (cleanMeasurement, error) {
		deps := cleanTestDeps(t.TempDir(), nil)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) { return scan, err }
		return cleanMeasurePath(ctx, deps, "/x")
	}
	t.Run("measured", func(t *testing.T) {
		got, err := measure(context.Background(), cleanFakeScan(4096), nil)
		if err != nil || got.status != cleanMeasured || len(got.reasons) != 0 || got.lowerBound || got.usage == nil || *got.usage != 4096 {
			t.Fatalf("got = %+v (%v)", got, err)
		}
		if got.filesystem == nil || got.filesystem.Mount != "/data" || got.measuredAt.IsZero() {
			t.Fatalf("got = %+v", got)
		}
	})
	t.Run("an unreadable root has no usage", func(t *testing.T) {
		scan := cleanFakeScan(4096)
		scan.root.unreadable = true
		got, err := measure(context.Background(), scan, nil)
		if err != nil || got.status != cleanUnreadable || got.reasons[0] != cleanReasonRootUnreadable || got.usage != nil {
			t.Fatalf("got = %+v (%v)", got, err)
		}
	})
	t.Run("a scan stopped by the time limit or by you is partial", func(t *testing.T) {
		timed, cancelTimed := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancelTimed()
		<-timed.Done()
		stopped, cancelStopped := context.WithCancel(context.Background())
		cancelStopped()
		for ctx, reason := range map[context.Context]string{timed: cleanReasonTimeout, stopped: cleanReasonStopped} {
			scan := cleanFakeScan(4096)
			scan.partial = true
			got, err := measure(ctx, scan, nil)
			if err != nil || got.status != cleanPartial || !got.lowerBound || got.reasons[0] != reason || got.usage == nil {
				t.Fatalf("%s: got = %+v (%v)", reason, got, err)
			}
		}
	})
	t.Run("unreadable directories and skipped mounts are lower bounds", func(t *testing.T) {
		scan := cleanFakeScan(4096)
		scan.unreadableCount, scan.unreadable = 2, []string{"/x/a"}
		scan.mounts = []duMount{{Path: "/x/m", FSType: "ext4"}}
		got, err := measure(context.Background(), scan, nil)
		want := []string{cleanReasonUnreadableDirs, cleanReasonSkippedMounts}
		if err != nil || got.status != cleanPartial || !got.lowerBound || !reflect.DeepEqual(got.reasons, want) ||
			got.unreadable != 2 || got.skippedMounts != 1 || !reflect.DeepEqual(got.unreadablePaths, []string{"/x/a"}) {
			t.Fatalf("got = %+v (%v)", got, err)
		}
		var item cleanItem
		got.applyTo(&item)
		if item.Status != cleanPartial || !item.LowerBound || item.UsageBytes == nil || item.Unreadable != 2 || item.SkippedMounts != 1 || item.MeasuredAt == nil {
			t.Fatalf("item = %+v", item)
		}
	})
	t.Run("a measure error is returned", func(t *testing.T) {
		if _, err := measure(context.Background(), duScan{}, fs.ErrPermission); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("failures map to a status and reasons", func(t *testing.T) {
		tests := []struct {
			err     error
			status  string
			reasons []string
			detail  bool
		}{
			{fs.ErrNotExist, cleanAbsent, nil, false},
			{fs.ErrPermission, cleanUnreadable, []string{cleanReasonPermission}, false},
			{errors.New("boom"), cleanUnreadable, []string{cleanReasonStatError}, true},
		}
		for _, test := range tests {
			status, reasons, detail := cleanFailure(test.err)
			if status != test.status || !reflect.DeepEqual(reasons, test.reasons) || (detail != "") != test.detail {
				t.Fatalf("%v: %q %v %q", test.err, status, reasons, detail)
			}
		}
	})
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
	for _, kind := range cleanDockerTypes {
		keys = append(keys, "clean.docker.type."+kind)
	}
	for _, spec := range append(cleanVMDisks("darwin", "/h"), cleanVMDisks("linux", "/h")...) {
		keys = append(keys, "clean.vm.disk."+spec.id)
	}
	for _, verdict := range cleanWorktreeVerdicts {
		keys = append(keys, "clean.worktree.verdict."+verdict)
	}
	for _, source := range cleanWorktreeBaseSources {
		keys = append(keys, "clean.worktree.base_source."+source)
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
	for _, goos := range []string{"darwin", "linux"} {
		for _, spec := range cleanCatalog(goos, "/h") {
			if !declared[spec.id] || !contains(cleanCategories, spec.category) || !contains(cleanCosts, spec.cost) {
				t.Errorf("%s catalog entry %s uses a value that has no message", goos, spec.id)
			}
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
