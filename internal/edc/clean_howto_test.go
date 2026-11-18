package edc

import (
	"strings"
	"testing"
)

func TestCleanRemoveCommandKeepsTheTildeOutsideQuotes(t *testing.T) {
	got := cleanRemoveCommand([]string{"/h/a b", "/real/h/x", "/tmp/y z", "/home2/z"}, "/h", "/real/h")
	if want := "rm -rf ~/'a b' ~/x '/tmp/y z' /home2/z"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func cleanHowToFixture() ([]cleanItem, cleanDocker, cleanWorktrees) {
	size := func(bytes uint64) *uint64 { return &bytes }
	items := []cleanItem{
		{ID: "go-build", Priority: "P1", Status: cleanMeasured, UsageBytes: size(40 << 30), Cleanup: "go clean -cache"},
		{ID: cleanDerivedID, Priority: "P1", Status: cleanMeasured, UsageBytes: size(20 << 30),
			Cleanup: "rm -rf ~/Library/Developer/Xcode/DerivedData"},
		{ID: "zig", Priority: "P1", Status: cleanMeasured, UsageBytes: size(0), Cleanup: "rm -rf ~/.cache/zig"},
		{ID: "cargo-git", Priority: "P1", Status: cleanAbsent},
		{ID: "app-caches", Priority: "P2", Status: cleanPartial, LowerBound: true, UsageBytes: size(3 << 30)},
	}
	reclaim := func(bytes int64) *int64 { return &bytes }
	docker := cleanDocker{Status: cleanMeasured, Types: []cleanDockerTypeResult{
		{Type: cleanDockerImages, Status: cleanMeasured, ReclaimableBytes: reclaim(0), Cleanup: "docker image prune -a"},
		{Type: cleanDockerBuildCache, Status: cleanMeasured, ReclaimableBytes: reclaim(1 << 30), Cleanup: "docker builder prune"},
		{Type: cleanDockerVolumes, Status: cleanMeasured},
	}}
	worktrees := cleanWorktrees{Status: cleanMeasured, Cleanup: cleanWorktreeCleanup, Items: []cleanWorktree{
		{Verdict: cleanVerdictMergedClean, UsageBytes: size(1 << 20)},
		{Verdict: cleanVerdictMergedClean, UsageBytes: size(1 << 20)},
		{Verdict: cleanVerdictHasChanges, UsageBytes: size(5 << 30)},
	}}
	return items, docker, worktrees
}

func TestCleanHowToListsMeasuredCandidatesThenDockerAndWorktrees(t *testing.T) {
	entries := cleanHowToEntries(cleanHowToFixture())
	var names []string
	for _, entry := range entries {
		names = append(names, entry.name+"="+entry.size+"|"+entry.command)
	}
	want := []string{
		"Go build cache=40.0 GB|go clean -cache",
		"Xcode DerivedData=20.0 GB|rm -rf ~/Library/Developer/Xcode/DerivedData",
		"other app caches=≥3.0 GB|",
		"Docker build cache=1.0 GB|docker builder prune",
		"Worktrees=2.0 MB|git-kit worktree cleanup",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("entries:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	for _, entry := range entries {
		if entry.note == "" || strings.HasPrefix(entry.note, "clean.") {
			t.Fatalf("%s has no note: %q", entry.name, entry.note)
		}
	}
}

func TestCleanHowToNeverCutsACommand(t *testing.T) {
	entries := cleanHowToEntries(cleanHowToFixture())
	for _, width := range []int{0, 50, 100} {
		text := formatCleanHowTo(entries, width, false)
		if strings.Contains(text, "…") {
			t.Fatalf("width %d cut a line:\n%s", width, text)
		}
		for _, entry := range entries {
			if entry.command == "" {
				continue
			}
			found := false
			for _, line := range strings.Split(text, "\n") {
				found = found || strings.HasSuffix(line, entry.command)
			}
			if !found {
				t.Fatalf("width %d: %q is not whole on one line:\n%s", width, entry.command, text)
			}
		}
		for _, line := range strings.Split(text, "\n") {
			if width > 0 && liveWidth(line) > width && !strings.Contains(line, "rm -rf") {
				t.Fatalf("width %d: only a command may run past the width: %q", width, line)
			}
		}
	}
	if text := formatCleanHowTo(entries, 0, false); !strings.Contains(text, "40.0 GB  Go build cache      go clean -cache\n") {
		t.Fatalf("a command that fits stays on the name line:\n%s", text)
	}
	if formatCleanHowTo(nil, 80, false) != "" {
		t.Fatal("no entries should print nothing")
	}
}
