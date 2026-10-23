package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCleanProgressShowsActiveTaskAndElapsedTime(t *testing.T) {
	var output bytes.Buffer
	progress := newCleanProgress(&output, "/home/demo", true)
	now := time.Unix(100, 0)
	progress.started = now
	progress.now = func() time.Time { return now }
	endStage := progress.begin("projects", "")
	endScan := progress.begin("measure", "/home/demo/work/192.0.2.10\x1b[31m\n")
	now = now.Add(3 * time.Second)
	progress.heartbeat()
	endScan()
	endStage()
	text := output.String()
	for _, want := range []string{"START", "RUN", "END", "3.0s", "(3s)", "~/work/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "192.0.2.10") || strings.Contains(text, "\x1b") || strings.Contains(text, "/home/demo") {
		t.Fatalf("unsafe progress: %q", text)
	}
	if len(progress.tasks) != 0 {
		t.Fatalf("completed tasks retained: %v", progress.tasks)
	}
}

func TestCleanProgressCoalescesFastDetailsButKeepsStages(t *testing.T) {
	var output bytes.Buffer
	progress := newCleanProgress(&output, "/home/demo", false)
	now := time.Unix(100, 0)
	progress.started = now
	progress.now = func() time.Time { return now }
	for range 100 {
		progress.begin("tool", "git")()
	}
	progress.begin("docker", "")()
	if got := strings.Count(output.String(), "START"); got != 2 {
		t.Fatalf("START count %d: %s", got, output.String())
	}
}

func TestRunCleanProgressKeepsJSONSeparate(t *testing.T) {
	home := cleanTestHome(t)
	cleanTestTree(t, filepath.Join(home, "npm"), 4096)
	deps := cleanRunDeps(t, home)
	log, err := os.CreateTemp(t.TempDir(), "progress")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	old := os.Stderr
	os.Stderr = log
	defer func() { os.Stderr = old }()
	path := filepath.Join(t.TempDir(), "result.json")
	var output bytes.Buffer
	if code := runCleanWith([]string{"--progress", "--json", path}, "test", deps, true, nil, &output); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("invalid report: %v", err)
	}
	recorded, err := os.ReadFile(log.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "START") || !strings.Contains(string(recorded), "END") {
		t.Fatalf("missing progress: %s", recorded)
	}
	if bytes.Contains(data, []byte("START")) || output.Len() != 0 {
		t.Fatal("progress leaked into result stream")
	}
	if err := log.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if code := runCleanWith([]string{"--progress=false", "--json", path}, "test", deps, true, nil, &output); code != 0 {
		t.Fatalf("disabled exit %d", code)
	}
	info, err := log.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("disabled progress wrote logs: %v", err)
	}
}

func TestCleanStallGuardSkipsMeasureThatNeverReturns(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stuck := func(context.Context, duOptions, duSources) (duScan, error) {
		<-release // 커널 안의 open처럼 취소를 보지 않는다.
		return duScan{}, nil
	}
	var begun, ended []string
	begin := func(stage, target string) func() {
		begun = append(begun, stage+" "+target)
		return func() { ended = append(ended, stage+" "+target) }
	}
	stalls := &cleanStalls{}
	measure := cleanStallGuard(stuck, begin, stalls, 20*time.Millisecond, 80*time.Millisecond)
	started := time.Now()
	_, err := measure(context.Background(), duOptions{root: "/cache/music"}, duSources{})
	if !errors.Is(err, errCleanStalled) || time.Since(started) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(started))
	}
	if len(begun) != 1 || begun[0] != "stalled /cache/music" || len(ended) != 1 {
		t.Fatalf("begun %v ended %v", begun, ended)
	}
	if warnings := stalls.warnings(10 * time.Second); len(warnings) != 1 || !strings.Contains(warnings[0], "/cache/music") {
		t.Fatalf("warnings = %v", warnings)
	}
	if status, reasons, _ := cleanFailure(err); status != cleanUnreadable || len(reasons) != 1 || reasons[0] != cleanReasonNoResponse {
		t.Fatalf("failure = %s %v", status, reasons)
	}
}

func TestCleanStallGuardWaitsForSlowMeasureThatKeepsWorking(t *testing.T) {
	slow := func(ctx context.Context, options duOptions, _ duSources) (duScan, error) {
		for range 40 {
			time.Sleep(5 * time.Millisecond)
			options.activity.Add(1)
		}
		return duScan{root: &duNode{total: 42}}, nil
	}
	stalls := &cleanStalls{}
	scan, err := cleanStallGuard(slow, nil, stalls, 20*time.Millisecond, 60*time.Millisecond)(context.Background(), duOptions{root: "/big"}, duSources{})
	if err != nil || scan.root == nil || scan.root.total != 42 || len(stalls.warnings(time.Second)) != 0 {
		t.Fatalf("scan = %+v err = %v", scan, err)
	}
}

func TestDuMeasureCountsFinishedCalls(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	sources := mustHostSources(t)
	for _, workers := range []int{1, 4} {
		activity := new(atomic.Int64)
		if _, err := duMeasure(context.Background(), duOptions{root: root, workers: workers, activity: activity}, sources); err != nil {
			t.Fatal(err)
		}
		if activity.Load() < 3 {
			t.Fatalf("workers %d: activity = %d", workers, activity.Load())
		}
	}
}

func TestCleanResolveGuardSkipsPathThatNeverResolves(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stuck := func(string) (string, error) {
		<-release
		return "", nil
	}
	var begun []string
	begin := func(stage, target string) func() {
		begun = append(begun, stage+" "+target)
		return func() {}
	}
	stalls := &cleanStalls{}
	_, err := cleanResolveGuard(stuck, begin, stalls, 20*time.Millisecond, 80*time.Millisecond)("/mnt/hang/cache")
	if !errors.Is(err, errCleanStalled) || len(begun) != 1 || begun[0] != "stalled /mnt/hang/cache" {
		t.Fatalf("err = %v begun = %v", err, begun)
	}
	resolved, err := cleanResolveGuard(func(path string) (string, error) { return path + "/real", nil }, nil, stalls,
		20*time.Millisecond, 80*time.Millisecond)("/ok")
	if err != nil || resolved != "/ok/real" {
		t.Fatalf("resolved = %q err = %v", resolved, err)
	}
}

func TestCleanStallsGroupNetworkMountsAndSkipTheirOtherPaths(t *testing.T) {
	listed := 0
	stalls := &cleanStalls{listMounts: func() ([]diskMount, error) {
		listed++
		return []diskMount{{Point: "/", FSType: "ext4"}, {Point: "/mnt/hang", FSType: "fuse.sshfs"}}, nil
	}}
	stalls.add("/mnt/hang/pip")
	stalls.add("/home/demo/Library/Caches/com.apple.Music")
	if !stalls.skip("/mnt/hang/zig") || stalls.skip("/home/demo/.cache/zig") {
		t.Fatal("only paths on the stalled network mount must be skipped")
	}
	resolve := cleanResolveGuard(func(string) (string, error) {
		t.Fatal("a path on a stalled mount was resolved again")
		return "", nil
	}, nil, stalls, time.Hour, time.Hour)
	if _, err := resolve("/mnt/hang/bun"); !errors.Is(err, errCleanStalled) {
		t.Fatalf("resolve err = %v", err)
	}
	measure := cleanStallGuard(func(context.Context, duOptions, duSources) (duScan, error) {
		t.Fatal("a path on a stalled mount was measured again")
		return duScan{}, nil
	}, nil, stalls, time.Hour, time.Hour)
	if _, err := measure(context.Background(), duOptions{root: "/mnt/hang/hf"}, duSources{}); !errors.Is(err, errCleanStalled) {
		t.Fatalf("measure err = %v", err)
	}
	warnings := stalls.warnings(10 * time.Second)
	want := []string{
		T("clean.warning.stalled", "/home/demo/Library/Caches/com.apple.Music", 10*time.Second),
		T("clean.warning.stalled_mount", "/mnt/hang", "fuse.sshfs", 10*time.Second, 4),
	}
	if strings.Join(warnings, "\n") != strings.Join(want, "\n") || listed != 1 {
		t.Fatalf("warnings = %q listed = %d, want %q", warnings, listed, want)
	}
}

func TestCleanRemoteFSType(t *testing.T) {
	for _, fstype := range []string{"fuse", "fuseblk", "fuse.sshfs", "nfs4", "cifs", "smbfs", "macfuse"} {
		if !cleanRemoteFSType(fstype) {
			t.Errorf("%s is not treated as remote", fstype)
		}
	}
	for _, fstype := range []string{"ext4", "apfs", "xfs", "btrfs", "overlay", "tmpfs"} {
		if cleanRemoteFSType(fstype) {
			t.Errorf("%s is treated as remote", fstype)
		}
	}
}
