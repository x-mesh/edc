package edc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
