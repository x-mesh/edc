//go:build darwin

package edc

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/x-mesh/edc/internal/fsevents"
)

func TestFSWatchClassifyHandlesAccumulatedFlags(t *testing.T) {
	started := time.Unix(1000, 0)
	before, after := started.Add(-time.Second), started.Add(time.Second)
	file := fsWatchPathState{exists: true, inode: 7, size: 1, mtime: after, birth: after}
	grown := file
	grown.size, grown.mtime = 2, after.Add(time.Second)
	old := fsWatchPathState{exists: true, inode: 7, size: 1, mtime: before, birth: before}
	gone := fsWatchPathState{}
	const (
		created  = fsevents.FlagItemCreated
		modified = fsevents.FlagItemModified
		removed  = fsevents.FlagItemRemoved
		renamed  = fsevents.FlagItemRenamed
	)
	for _, test := range []struct {
		name     string
		flags    uint32
		previous fsWatchPathState
		known    bool
		current  fsWatchPathState
		want     []string
	}{
		{"new file", created | modified, gone, false, file, []string{"create"}},
		{"write after create keeps the created flag", created | modified, file, true, grown, []string{"modify"}},
		{"repeat with no change", created | modified, file, true, file, nil},
		{"file from before the watch", created, gone, false, old, nil},
		{"old file written before the watch", modified, gone, false, old, nil},
		{"old file written now", modified, gone, false, grown, []string{"modify"}},
		{"renamed in", renamed, gone, false, file, []string{"create"}},
		{"old file renamed in", renamed, gone, false, old, []string{"create"}},
		{"renamed away after create", created | modified | renamed, file, true, gone, []string{"rename"}},
		{"removed after rename in", renamed | removed, file, true, gone, []string{"remove"}},
		{"created and removed in one batch", created | removed, gone, false, gone, []string{"create", "remove"}},
		{"removed twice", removed, gone, true, gone, nil},
		{"replaced by another inode", modified, file, true, fsWatchPathState{exists: true, inode: 8, mtime: after, birth: after}, []string{"create"}},
		{"directory change", modified, fsWatchPathState{exists: true, isDir: true}, true, fsWatchPathState{exists: true, isDir: true, size: 9}, nil},
	} {
		if got := fsWatchClassify(test.flags, test.previous, test.known, test.current, started); !slices.Equal(got, test.want) {
			t.Errorf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}

func TestFSWatchForgetsGoneAndMovedPaths(t *testing.T) {
	now := time.Unix(2000, 0)
	states := map[string]fsWatchPathState{
		"/r/kept":      {exists: true},
		"/r/recent":    {gone: now.Add(-time.Second)},
		"/r/old":       {gone: now.Add(-2 * fsWatchGoneTTL)},
		"/r/dir/a":     {exists: true},
		"/r/dir/sub/b": {exists: true},
		"/r/dirt":      {exists: true},
	}
	fsWatchForgetGone(states, now)
	fsWatchForgetTree(states, "/r/dir")
	var names []string
	for name := range states {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"/r/dirt", "/r/kept", "/r/recent"}; !slices.Equal(names, want) {
		t.Fatalf("states = %v, want %v", names, want)
	}
}

func TestFSWatchRescansAfterDroppedEvents(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kept.txt", "changed.txt", "gone.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0700); err != nil {
		t.Fatal(err)
	}
	var emitted []string
	options := fsWatchOptions{root: root, recursive: true, exclude: fsWatchDefaultExcludes}
	watch := newFSWatchFSEvents(options, time.Now(), func(event fsWatchEvent) bool {
		emitted = append(emitted, event.Event+" "+event.Path)
		return true
	})
	for _, name := range []string{"kept.txt", "changed.txt", "gone.txt"} {
		if err := watch.update(name, fsevents.FlagItemModified, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "changed.txt"), []byte("after, longer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new", "file.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "pkg", "index.js"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var notice bytes.Buffer
	previous := fsWatchNotice
	fsWatchNotice = &notice
	defer func() { fsWatchNotice = previous }()
	emitted = nil
	batch := []fsevents.Event{
		{Path: filepath.Join(root, "node_modules"), Flags: fsevents.FlagMustScanSubDirs},
		{Path: filepath.Join(root, "new"), Flags: fsevents.FlagMustScanSubDirs | fsevents.FlagUserDropped},
		{Path: root, Flags: fsevents.FlagMustScanSubDirs | fsevents.FlagUserDropped},
		{Path: filepath.Join(root, "new", "file.txt"), Flags: fsevents.FlagItemCreated},
	}
	if err := watch.handle(batch, time.Now()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(emitted)
	if want := []string{"create new", "create new/file.txt", "modify changed.txt", "remove gone.txt"}; !slices.Equal(emitted, want) {
		t.Fatalf("emitted %v, want %v", emitted, want)
	}
	if lines := strings.Count(notice.String(), "\n"); lines != 1 {
		t.Fatalf("notice %q, want one rescan of the root", notice.String())
	}
}
