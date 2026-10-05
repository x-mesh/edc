package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFSWatchGlob(t *testing.T) {
	for _, test := range []struct {
		pattern, path string
		want          bool
	}{
		{"text.txt", "text.txt", true}, {"text.txt", "sub/text.txt", false},
		{"**/*.go", "main.go", true}, {"**/*.go", "src/sub/main.go", true},
		{"*.go", "src/main.go", false}, {"src/**/test?.[ch]", "src/test1.c", true},
		{"src/**/test?.[ch]", "src/a/b/test2.h", true}, {"src/**/test?.[ch]", "src/a/b/test22.h", false},
		{".git/**", ".git", true}, {".git/**", ".git/objects/x", true}, {"**", "a/b", true},
	} {
		if err := validateFSWatchGlob(test.pattern); err != nil {
			t.Fatal(err)
		}
		if got := matchFSWatchGlob(test.pattern, test.path); got != test.want {
			t.Fatalf("%s %s = %v", test.pattern, test.path, got)
		}
	}
	for _, pattern := range []string{"", "/a", "../a", "a/../b", "a//b", "[", "a/"} {
		if err := validateFSWatchGlob(pattern); err == nil {
			t.Fatalf("invalid glob accepted: %q", pattern)
		}
	}
}

func TestFSWatchOptionsAndStrictRules(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "watch.yaml")
	contents := "directory: .\nrecursive: true\nrules:\n  - name: pull\n    events: [create]\n    match: text.txt\n    command: [git-kit, pull]\n    debounce: 0s\n    timeout: 2s\n"
	if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	options, err := parseFSWatchOptions([]string{root, "--rules", config, "--recursive=false", "--exclude", "build/**"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	if options.root != realRoot || options.recursive || len(options.rules) != 1 || options.rules[0].debounce != 0 || options.rules[0].timeout != 2*time.Second || options.rules[0].CWD != realRoot {
		t.Fatalf("options = %+v", options)
	}
	if len(options.exclude) != 2 {
		t.Fatalf("excludes = %v", options.exclude)
	}
	cli, err := parseFSWatchOptions([]string{root, "--event", "create", "--match", "text.txt", "--exec", "git-kit pull"}, io.Discard)
	if err != nil || len(cli.rules) != 1 || cli.rules[0].Command[2] != "git-kit pull" {
		t.Fatalf("CLI = %+v, %v", cli, err)
	}
	for _, args := range [][]string{
		{root, "--event", "read"}, {root, "--debounce=-1s"}, {root, "--timeout=0s"}, {root, "--duration=-1s"},
		{root, "--match", "["}, {root, "extra"}, {root, "--exec", "true", "--rules", config},
		{root, "--json", config, "--rules", config}, {root, "--wat"}, {root, "--exec"},
	} {
		if _, err := parseFSWatchOptions(args, io.Discard); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	alias := filepath.Join(root, "alias.yaml")
	if err := os.Symlink(config, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFSWatchOptions([]string{"--rules", config, "--json", alias}, io.Discard); err == nil {
		t.Fatal("rules file could be overwritten via symlink")
	}
	for _, invalid := range []string{
		"recursive: true\nrulez: []\n", "rules: []\n", contents + "---\nrules: []\n",
		"rules: [{command: echo}]\n", "rules: [{command: []}]\n",
		"rules: [{name: dup, command: [true]}, {name: dup, command: [true]}]\n",
		"rules: [{command: [true], events: [read]}]\n",
	} {
		if err := os.WriteFile(config, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseFSWatchOptions([]string{"--rules", config}, io.Discard); err == nil {
			t.Fatalf("invalid rules accepted: %s", invalid)
		}
	}
}

func fsWatchWaitEvent(t *testing.T, source fsWatchSource, kind, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-source.events:
			if !ok {
				t.Fatal("event source closed")
			}
			if event.Event == kind && event.Path == path {
				return
			}
		case err := <-source.errors:
			t.Fatalf("watch error: %v", err)
		case <-timer.C:
			t.Fatalf("missing %s %s", kind, path)
		}
	}
}

func fsWatchTestSource(t *testing.T, root string, recursive bool) fsWatchSource {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newFSWatchSource(fsWatchOptions{root: root, recursive: recursive, exclude: []string{".git/**"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(source.close)
	return source
}

func TestFSWatchNativeFileLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	source := fsWatchTestSource(t, root, false)
	file := filepath.Join(root, "text.txt")
	if err := os.WriteFile(file, []byte("created"), 0600); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "create", "text.txt")
	if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "modify", "text.txt")
	renamed := filepath.Join(root, "renamed.txt")
	if err := os.Rename(file, renamed); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "rename", "text.txt")
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "remove", "renamed.txt")
}

func TestFSWatchRecursiveNewTreeAndRename(t *testing.T) {
	root := t.TempDir()
	source := fsWatchTestSource(t, root, true)
	sub := filepath.Join(root, "new", "deep")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "text.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "create", "new/deep/text.txt")
	if err := os.Rename(filepath.Join(root, "new"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "create", "moved/deep/text.txt")
	if err := os.WriteFile(filepath.Join(root, "moved", "deep", "later.txt"), []byte("later"), 0600); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "create", "moved/deep/later.txt")
}

func TestFSWatchNoInitialOrExcludedEvents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	source := fsWatchTestSource(t, root, false)
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "child"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-source.events:
		t.Fatalf("unexpected event: %+v", event)
	case err := <-source.errors:
		t.Fatal(err)
	case <-time.After(150 * time.Millisecond):
	}
}

type fsWatchTestSink struct {
	mutex   sync.Mutex
	data    bytes.Buffer
	records chan fsWatchRecord
}

func newFSWatchTestSink() *fsWatchTestSink {
	return &fsWatchTestSink{records: make(chan fsWatchRecord, 100)}
}
func (sink *fsWatchTestSink) Write(data []byte) (int, error) {
	sink.mutex.Lock()
	defer sink.mutex.Unlock()
	var record fsWatchRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	sink.records <- record
	return sink.data.Write(data)
}
func (sink *fsWatchTestSink) wait(t *testing.T, kind string) fsWatchRecord {
	t.Helper()
	for {
		select {
		case record := <-sink.records:
			if record.Type == kind {
				return record
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("missing record: %s", kind)
		}
	}
}

func TestFSWatchSerialActionsCoalesceWhileRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan fsWatchEvent, 20)
	source := fsWatchSource{events: events, errors: make(chan error)}
	rule := fsWatchRule{Name: "test", Events: []string{"modify"}, Match: "**/*.go", Command: []string{"test"}}
	sink := newFSWatchTestSink()
	starts := make(chan fsWatchEvent, 5)
	release := make(chan struct{})
	runner := func(ctx context.Context, rule fsWatchRule, event fsWatchEvent, root string) fsWatchRecord {
		starts <- event
		select {
		case <-release:
		case <-ctx.Done():
		}
		return fsWatchRecord{Type: "action_result", Rule: rule.Name, Path: event.Path, Status: "success"}
	}
	done := make(chan int, 1)
	go func() {
		code, err := streamFSWatch(ctx, sink, source, fsWatchOptions{root: ".", events: []string{"modify"}, match: "**", rules: []fsWatchRule{rule}, jsonPath: "-"}, runner)
		if err != nil {
			done <- 99
		} else {
			done <- code
		}
	}()
	sink.wait(t, "ready")
	events <- fsWatchEvent{Event: "modify", Path: "first.go"}
	select {
	case <-starts:
	case <-time.After(3 * time.Second):
		t.Fatal("first action did not start")
	}
	sink.wait(t, "event")
	for _, name := range []string{"one.go", "two.go", "last.go"} {
		events <- fsWatchEvent{Event: "modify", Path: name}
		sink.wait(t, "event")
	}
	select {
	case <-starts:
		t.Fatal("actions ran concurrently")
	default:
	}
	release <- struct{}{}
	select {
	case event := <-starts:
		if event.Path != "last.go" {
			t.Fatalf("queued event=%+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending action missing")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop")
	}
	count := 0
	decoder := json.NewDecoder(&sink.data)
	for {
		var record fsWatchRecord
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record.Type == "action_result" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("actions=%d", count)
	}
}

func TestFSWatchDryRunAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan fsWatchEvent, 1)
	sink := newFSWatchTestSink()
	done := make(chan int, 1)
	options := fsWatchOptions{root: ".", events: []string{"create"}, match: "**", jsonPath: "-", dryRun: true, rules: []fsWatchRule{{Name: "dry", Events: []string{"create"}, Match: "text.txt", Command: []string{"should-not-run"}}}}
	go func() {
		code, _ := streamFSWatch(ctx, sink, fsWatchSource{events: events, errors: make(chan error)}, options, func(context.Context, fsWatchRule, fsWatchEvent, string) fsWatchRecord { panic("dry-run executed") })
		done <- code
	}()
	sink.wait(t, "ready")
	events <- fsWatchEvent{Event: "create", Path: "text.txt"}
	if record := sink.wait(t, "action_result"); record.Status != "dry_run" {
		t.Fatalf("result=%+v", record)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop timed out")
	}
	already, cancelAlready := context.WithCancel(context.Background())
	cancelAlready()
	if code, err := streamFSWatch(already, &bytes.Buffer{}, fsWatchSource{}, fsWatchOptions{}, nil); code != 0 || err != nil {
		t.Fatalf("already cancelled: %d %v", code, err)
	}
}

func TestFSWatchActionEnvironmentFailuresAndTimeout(t *testing.T) {
	root := t.TempDir()
	event := fsWatchEvent{Event: "create", Path: "text'; touch injected; '.txt"}
	rule := fsWatchRule{Name: "env", Command: []string{"/bin/sh", "-c", `printf '%s:%s' "$EDC_WATCH_PATH" "$EDC_WATCH_EVENT"`}, CWD: root, timeout: time.Second}
	record := executeFSWatchAction(context.Background(), rule, event, root)
	if record.Status != "success" || record.ExitCode == nil || *record.ExitCode != 0 || record.Output != event.Path+":create" {
		t.Fatalf("result=%+v", record)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("event path executed as shell text")
	}
	rule.Command = []string{"/bin/sh", "-c", "exit 7"}
	record = executeFSWatchAction(context.Background(), rule, event, root)
	if record.Status != "failed" || record.ExitCode == nil || *record.ExitCode != 7 {
		t.Fatalf("failure=%+v", record)
	}
	rule.Command = []string{"/does-not-exist/edc-watch-test"}
	record = executeFSWatchAction(context.Background(), rule, event, root)
	if record.Status != "failed" || record.ExitCode != nil {
		t.Fatalf("start failure=%+v", record)
	}
	rule.Command = []string{"/bin/sh", "-c", `(sleep 0.2; printf bad > "$EDC_WATCH_ROOT/leaked.txt") & wait`}
	rule.timeout = 30 * time.Millisecond
	record = executeFSWatchAction(context.Background(), rule, event, root)
	if record.Status != "timeout" {
		t.Fatalf("timeout=%+v", record)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, "leaked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("timed-out descendant survived")
	}
}

func TestFSWatchActionOutputBoundedAndWatcherErrors(t *testing.T) {
	var output fsWatchActionOutput
	if n, err := output.Write([]byte(strings.Repeat("x", fsWatchOutputLimit+1))); n != fsWatchOutputLimit+1 || err != nil {
		t.Fatalf("write=%d %v", n, err)
	}
	if len(output.text()) != fsWatchOutputLimit || !output.truncated {
		t.Fatal("action output not bounded")
	}
	failures := make(chan error, 1)
	failures <- errors.New("overflow")
	var writer bytes.Buffer
	code, err := streamFSWatch(context.Background(), &writer, fsWatchSource{errors: failures}, fsWatchOptions{jsonPath: "-"}, nil)
	if code != 1 || err == nil || !strings.Contains(writer.String(), "watch_error") {
		t.Fatalf("watch error=%d %v %s", code, err, &writer)
	}
	if code, err := streamFSWatch(context.Background(), watchRejectSummary{}, fsWatchSource{}, fsWatchOptions{jsonPath: "-"}, nil); code != 2 || err == nil {
		t.Fatalf("write error=%d %v", code, err)
	}
}

func TestFSWatchDebounceAndFailureKeepsWatching(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan fsWatchEvent, 10)
	sink := newFSWatchTestSink()
	starts := make(chan fsWatchEvent, 5)
	rule := fsWatchRule{Name: "debounce", Events: []string{"modify"}, Match: "**", Command: []string{"test"}, debounce: 40 * time.Millisecond}
	done := make(chan int, 1)
	go func() {
		code, _ := streamFSWatch(ctx, sink, fsWatchSource{events: events, errors: make(chan error)}, fsWatchOptions{root: ".", events: rule.Events, match: "**", rules: []fsWatchRule{rule}, jsonPath: "-"}, func(ctx context.Context, rule fsWatchRule, event fsWatchEvent, root string) fsWatchRecord {
			starts <- event
			return fsWatchRecord{Type: "action_result", Rule: rule.Name, Path: event.Path, Status: "failed"}
		})
		done <- code
	}()
	sink.wait(t, "ready")
	for _, name := range []string{"one", "two", "last"} {
		events <- fsWatchEvent{Event: "modify", Path: name}
		sink.wait(t, "event")
	}
	select {
	case event := <-starts:
		if event.Path != "last" {
			t.Fatalf("debounce=%+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("debounced action missing")
	}
	sink.wait(t, "action_result")
	events <- fsWatchEvent{Event: "modify", Path: "after-failure"}
	select {
	case event := <-starts:
		if event.Path != "after-failure" {
			t.Fatalf("continued action=%+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failure stopped watching")
	}
	sink.wait(t, "action_result")
	cancel()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("failure exit=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop timed out")
	}
}

func TestFSWatchNativeOutputIsExcludedAndRootRenameFails(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	output := filepath.Join(root, "events.jsonl")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	source, err := newFSWatchSource(fsWatchOptions{root: root, outputPath: output, outputInfo: info})
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	if _, err := file.WriteString("output\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-source.events:
		t.Fatalf("self output event: %+v", event)
	case err := <-source.errors:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	select {
	case err := <-source.errors:
		if err == nil || !strings.Contains(err.Error(), root) {
			t.Fatalf("root error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("root rename was not reported")
	}
}

func TestFSWatchDirectorySymlinkNotFollowed(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	source := fsWatchTestSource(t, root, true)
	if err := os.WriteFile(filepath.Join(outside, "text.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-source.events:
		t.Fatalf("symlink subtree followed: %+v", event)
	case err := <-source.errors:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFSWatchSubcommandHelpStaysSpecific(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, mode := range []string{"http", "fs"} {
			var output bytes.Buffer
			printWatchSubcommandHelp(&output, mode)
			text := output.String()
			if mode == "fs" && (strings.Contains(text, "--expect-status") || strings.Contains(text, "--redact") || strings.Contains(text, "--interval")) {
				t.Fatalf("HTTP flags in fs help: %s", text)
			}
			if mode == "http" && (strings.Contains(text, "--exec") || strings.Contains(text, "--recursive")) {
				t.Fatalf("fs flags in HTTP help: %s", text)
			}
			for _, line := range strings.Split(text, "\n") {
				if liveWidth(line) > 80 {
					t.Fatalf("%s %s help too wide: %s", language, mode, line)
				}
			}
		}
	}
}

func TestTerminalJSONKeepsShellOperators(t *testing.T) {
	const command = "echo ok > out.txt && cat < in.txt"
	if got := terminalJSON(command); got != `"`+command+`"` {
		t.Fatalf("terminalJSON = %s", got)
	}
	if got := terminalJSON("a\x1b[31m한"); got != `"a\u001b[31m\ud55c"` {
		t.Fatalf("terminalJSON control and non-ASCII = %q", got)
	}
	if got := asciiJSON(command); !strings.Contains(got, `\u003e`) {
		t.Fatalf("asciiJSON changed the log header format: %s", got)
	}
}

func TestFSWatchTerminalLines(t *testing.T) {
	var buffer bytes.Buffer
	start := fsWatchRecord{Type: "action_start", Time: time.Now(), Rule: "command", Path: "text.txt", Command: []string{"/bin/sh", "-c", "echo ok > out.txt"}}
	if err := writeFSWatchRecord(&buffer, start, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "echo ok > out.txt") {
		t.Fatalf("start line = %q", buffer.String())
	}
	for reason, key := range map[string]string{"duration": "watchfs.stop.duration", "cancelled": "watchfs.stop.cancelled", "watch_error": "watchfs.stop.watch_error"} {
		buffer.Reset()
		if err := writeFSWatchRecord(&buffer, fsWatchRecord{Type: "summary", Time: time.Now(), StopReason: reason}, false); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(strings.TrimSpace(buffer.String()), T(key)) {
			t.Fatalf("%s summary = %q", reason, buffer.String())
		}
	}
	buffer.Reset()
	if err := writeFSWatchRecord(&buffer, fsWatchRecord{Type: "summary", Time: time.Now(), StopReason: "duration"}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), `"stop_reason":"duration"`) {
		t.Fatalf("JSON summary changed: %s", buffer.String())
	}
}
