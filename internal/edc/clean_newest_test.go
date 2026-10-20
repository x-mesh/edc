package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cleanTestMeasured(id string, roots ...string) cleanItem {
	usage := uint64(1 << 20)
	return cleanItem{ID: id, Status: cleanMeasured, UsageBytes: &usage, roots: roots, Priority: "P1", Category: "package_cache"}
}

func TestCleanAddNewestWriteFillsItemsAndGroups(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2025, 1, 2, 3, 0, 0, 0, time.UTC)
	mid := time.Date(2025, 6, 7, 3, 0, 0, 0, time.UTC)
	new := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	cleanTestWriteFile(t, filepath.Join(root, "ProjA-1", "x"), "x", old)
	cleanTestWriteFile(t, filepath.Join(root, "ProjA-2", "y"), "y", mid)
	cleanTestWriteFile(t, filepath.Join(root, "ProjB-1", "z"), "z", new)
	cleanTestWriteFile(t, filepath.Join(root, "loose.txt"), "l", old)

	item := cleanTestMeasured("derived", root)
	item.Groups = []cleanGroup{
		{Name: "ProjB", Folders: 1, paths: []string{filepath.Join(root, "ProjB-1")}},
		{Name: "ProjA", Folders: 2, paths: []string{filepath.Join(root, "ProjA-1"), filepath.Join(root, "ProjA-2")}},
		{Name: "NoPaths", Folders: 1},
	}
	absent := cleanItem{ID: "absent", Status: cleanAbsent, roots: []string{root}}
	items := []cleanItem{item, absent}
	cleanAddNewestWrite(context.Background(), items)

	if got := items[0].NewestWrite; got == nil || cleanDockerDate(got) != "2026-09-12" || items[0].NewestPartial {
		t.Fatalf("item newest = %v partial %v", got, items[0].NewestPartial)
	}
	if got := items[0].Groups[0].NewestWrite; got == nil || cleanDockerDate(got) != "2026-09-12" {
		t.Errorf("ProjB newest = %v", got)
	}
	if got := items[0].Groups[1].NewestWrite; got == nil || cleanDockerDate(got) != "2025-06-07" {
		t.Errorf("a group with two folders must take the newest of both, got %v", got)
	}
	if items[0].Groups[2].NewestWrite != nil {
		t.Errorf("a group without paths got a date: %v", items[0].Groups[2].NewestWrite)
	}
	if items[1].NewestWrite != nil {
		t.Errorf("a candidate that was not measured got a date: %v", items[1].NewestWrite)
	}
}

func TestCleanAddNewestWriteMarksAnUnreadableFolderAsPartial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every folder")
	}
	root := t.TempDir()
	cleanTestWriteFile(t, filepath.Join(root, "open", "a"), "a", time.Date(2025, 2, 3, 0, 0, 0, 0, time.UTC))
	cleanTestWriteFile(t, filepath.Join(root, "locked", "b"), "b", time.Now())
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	items := []cleanItem{cleanTestMeasured("cache", root)}
	cleanAddNewestWrite(context.Background(), items)
	if items[0].NewestWrite == nil || cleanDockerDate(items[0].NewestWrite) != "2025-02-03" || !items[0].NewestPartial {
		t.Fatalf("newest %v partial %v", items[0].NewestWrite, items[0].NewestPartial)
	}
}

func TestCleanAddNewestWriteStopsOnACancelledContext(t *testing.T) {
	root := t.TempDir()
	cleanTestWriteFile(t, filepath.Join(root, "a"), "a", time.Now())
	items := []cleanItem{cleanTestMeasured("cache", root)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanAddNewestWrite(ctx, items)
	if items[0].NewestWrite != nil || !items[0].NewestPartial {
		t.Fatalf("newest %v partial %v", items[0].NewestWrite, items[0].NewestPartial)
	}
}

func TestCleanAddNewestWriteHandlesManyCandidatesAtOnce(t *testing.T) {
	var items []cleanItem
	for index := 0; index < cleanNewestParallel*5; index++ {
		root := t.TempDir()
		cleanTestWriteFile(t, filepath.Join(root, "f"), "f", time.Date(2025, 1, 1+index, 0, 0, 0, 0, time.UTC))
		item := cleanTestMeasured(fmt.Sprintf("c%02d", index), root)
		item.Groups = []cleanGroup{{Name: "g", Folders: 1, paths: []string{root}}}
		items = append(items, item)
	}
	cleanAddNewestWrite(context.Background(), items)
	for index, item := range items {
		want := fmt.Sprintf("2025-01-%02d", 1+index)
		if item.NewestWrite == nil || cleanDockerDate(item.NewestWrite) != want || item.Groups[0].NewestWrite == nil || cleanDockerDate(item.Groups[0].NewestWrite) != want {
			t.Fatalf("%s: item %v group %v, want %s", item.ID, item.NewestWrite, item.Groups[0].NewestWrite, want)
		}
	}
}

func TestFormatCleanShowsTheNewestFileDate(t *testing.T) {
	at := time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)
	item := cleanTestMeasured("go-build", "/h/go-build")
	item.ReportedPath, item.NewestWrite = "/h/go-build", &at
	item.RegenCost = "low"
	item.Groups = []cleanGroup{{Name: "ProjA", Folders: 1, UsageBytes: 1 << 20, NewestWrite: &at}, {Name: "ProjB", Folders: 2, UsageBytes: 1 << 19, NewestWrite: &at, NewestPartial: true}}
	item2 := item
	item2.ID, item2.NewestPartial = "npm", true
	verbose := formatCleanItems([]cleanItem{item, item2}, true, 0, "/h")
	for _, want := range []string{"newest file 2025-03-04", "≥ newest file 2025-03-04", "ProjA  2025-03-04", "2025-03-04"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, verbose)
		}
	}
	if !strings.Contains(verbose, "ProjB  2 folders  ≥ 2025-03-04") {
		t.Errorf("a partial group date lacks the marker:\n%s", verbose)
	}
	none := cleanTestMeasured("uv", "/h/uv")
	none.ReportedPath = "/h/uv"
	if text := formatCleanItems([]cleanItem{none}, true, 0, "/h"); strings.Contains(text, "newest file") {
		t.Errorf("a candidate without a date shows one:\n%s", text)
	}
}

func TestRunCleanFindsTheNewestFileOnlyWithVerbose(t *testing.T) {
	home := cleanTestHome(t)
	cleanTestWriteFile(t, filepath.Join(home, ".cache", "zig", "old"), "o", time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC))
	cleanTestWriteFile(t, filepath.Join(home, ".cache", "zig", "sub", "newer"), "n", time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC))
	newest := func(t *testing.T, args ...string) (string, bool) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "out.json")
		var output bytes.Buffer
		if code := runCleanWith(append(args, "--json", path), "test", cleanRunDeps(t, home), true, nil, &output); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Results []struct {
				Metrics struct {
					Candidates []struct {
						ID          string `json:"id"`
						NewestWrite string `json:"newest_write"`
					} `json:"candidates"`
				} `json:"metrics"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		for _, candidate := range report.Results[0].Metrics.Candidates {
			if candidate.ID == "zig" {
				return candidate.NewestWrite, true
			}
		}
		t.Fatal("zig candidate missing")
		return "", false
	}
	if got, _ := newest(t, "-v"); !strings.HasPrefix(got, "2026-08-09") {
		t.Errorf("with -v: newest_write = %q", got)
	}
	if got, _ := newest(t); got != "" {
		t.Errorf("without -v the candidate got a date: %q", got)
	}
}
