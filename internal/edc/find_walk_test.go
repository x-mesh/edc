package edc

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFindTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(root, file)
		if strings.HasSuffix(file, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func testFindSources(mounts []diskMount) findSources {
	return findSources{mounts: func() ([]diskMount, error) { return mounts, nil }}
}

// collectFind는 순회 결과를 "경로:종류"로 정렬해 돌려준다.
func collectFind(t *testing.T, options findWalkOptions, sources findSources) ([]string, findWalkReport) {
	t.Helper()
	if options.workers == 0 {
		options.workers = 3
	}
	var found []string
	report := findWalk(context.Background(), options, sources, func(batch []findEntry) bool {
		for _, entry := range batch {
			found = append(found, entry.path+":"+string(entry.kind))
		}
		return true
	})
	slices.Sort(found)
	return found, report
}

func TestFindWalkIncludesHiddenNamesAndKeepsTheRootShape(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "a/b.txt", ".env", ".config/app.toml", ".git/HEAD", "sub/.git", "sub/x.go", "empty/")
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	found, report := collectFind(t, findWalkOptions{roots: []string{"."}, noIgnore: true}, testFindSources(nil))
	want := []string{".config:d", ".config/app.toml:f", ".env:f", "a:d", "a/b.txt:f", "empty:d", "link:l", "sub:d", "sub/x.go:f"}
	slices.Sort(want)
	// .git은 디렉터리든 파일(worktree)이든 뺀다. symlink는 따라가지 않는다. 읽은 디렉터리는 시작 경로를 포함해 다섯이다.
	if !reflect.DeepEqual(found, want) || report.Directories != 5 {
		t.Fatalf("found = %v, directories = %d", found, report.Directories)
	}
	for root, first := range map[string]string{"a/": "a/b.txt:f", "./a": "a/b.txt:f", filepath.Join(root, "a"): filepath.Join(root, "a/b.txt") + ":f"} {
		if found, _ := collectFind(t, findWalkOptions{roots: []string{root}, noIgnore: true}, testFindSources(nil)); !reflect.DeepEqual(found, []string{first}) {
			t.Errorf("%s: found = %v", root, found)
		}
	}
	// 파일을 시작 경로로 주면 그 파일만 후보다.
	if found, _ := collectFind(t, findWalkOptions{roots: []string{"a/b.txt"}}, testFindSources(nil)); !reflect.DeepEqual(found, []string{"a/b.txt:f"}) {
		t.Errorf("file root = %v", found)
	}
	if _, report := collectFind(t, findWalkOptions{roots: []string{"missing"}}, testFindSources(nil)); len(report.Problems) != 1 {
		t.Errorf("problems = %v", report.Problems)
	}
}

func TestFindWalkSkipsMountsPseudoFilesystemsAndLayers(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "data/x", "mnt/nfs/y", "proc/1/status", "var/lib/docker/overlay2/l1/etc/nginx.conf",
		"var/lib/docker/containers/c1/c1-json.log", "var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/s/fs/etc/z")
	// 마운트 표는 실제 경로를 적는다. macOS의 임시 디렉터리는 symlink인 /var 아래에 있다.
	real := findTestReal(t, root)
	mounts := []diskMount{
		{Point: real, FSType: "ext4", Root: "/"},
		{Point: filepath.Join(real, "mnt/nfs"), FSType: "nfs4", Root: "/"},
		{Point: filepath.Join(real, "proc"), FSType: "proc", Root: "/"},
	}
	sources := testFindSources(mounts)
	sources.layerPaths = []string{filepath.Join(real, "var/lib/docker/overlay2")}
	sources.layerPrefix = filepath.Join(real, "var/lib/containerd/io.containerd.snapshotter.v1.")
	found, report := collectFind(t, findWalkOptions{roots: []string{root}, noIgnore: true}, sources)
	for _, path := range []string{"mnt/nfs/y", "proc/1/status", "overlay2/l1", "overlayfs/s"} {
		if slices.ContainsFunc(found, func(entry string) bool { return strings.Contains(entry, path) }) {
			t.Errorf("%s must be skipped: %v", path, found)
		}
	}
	// 컨테이너의 로그는 호스트에서 찾는 대상이다.
	if !slices.Contains(found, filepath.Join(root, "var/lib/docker/containers/c1/c1-json.log")+":f") {
		t.Errorf("container logs must stay: %v", found)
	}
	var reasons []string
	for _, skip := range report.Skipped {
		reasons = append(reasons, strings.TrimPrefix(skip.Path, root)+" "+skip.Reason+" "+skip.FSType)
	}
	want := []string{"/mnt/nfs mount nfs4", "/proc pseudo proc", "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs layer ", "/var/lib/docker/overlay2 layer "}
	if !reflect.DeepEqual(reasons, want) {
		t.Errorf("skipped = %q", reasons)
	}
	// --cross-mounts와 --layers를 주어도 가상 파일 시스템에는 들어가지 않는다.
	found, _ = collectFind(t, findWalkOptions{roots: []string{root}, noIgnore: true, crossMounts: true, layers: true}, sources)
	if !slices.Contains(found, filepath.Join(root, "mnt/nfs/y")+":f") || !slices.Contains(found, filepath.Join(root, "var/lib/docker/overlay2/l1/etc/nginx.conf")+":f") ||
		slices.ContainsFunc(found, func(entry string) bool { return strings.Contains(entry, "/proc/1") }) {
		t.Errorf("cross mounts = %v", found)
	}
	// 시작 경로가 그 마운트 안이면 그대로 찾는다.
	if found, _ := collectFind(t, findWalkOptions{roots: []string{filepath.Join(root, "proc")}, noIgnore: true}, sources); len(found) != 2 {
		t.Errorf("inside proc = %v", found)
	}
}

func TestFindWalkNamesPseudoPathsWithoutAMountTable(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "proc/1/status", "etc/hosts")
	sources := findSources{mounts: func() ([]diskMount, error) { return nil, os.ErrPermission }, pseudoPaths: []string{filepath.Join(findTestReal(t, root), "proc")}}
	found, report := collectFind(t, findWalkOptions{roots: []string{root}, noIgnore: true}, sources)
	if report.MountErr == nil || slices.ContainsFunc(found, func(entry string) bool { return strings.Contains(entry, "proc/1") }) {
		t.Errorf("found = %v, err = %v", found, report.MountErr)
	}
}

func TestFindWalkAppliesGitignoreOnlyInsideARepository(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "app/.git/HEAD", "app/.gitignore", "app/logs/app.log", "app/main.go", "app/cache.tmp", "app/sub/.gitignore", "app/sub/keep.txt", "app/sub/drop.bin")
	if err := os.WriteFile(filepath.Join(root, "app/.gitignore"), []byte("logs/\n*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app/sub/.gitignore"), []byte("*.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inside, _ := collectFind(t, findWalkOptions{roots: []string{filepath.Join(root, "app")}}, testFindSources(nil))
	for _, entry := range inside {
		if strings.Contains(entry, "logs") || strings.Contains(entry, "cache.tmp") || strings.Contains(entry, "drop.bin") {
			t.Errorf("ignored path inside the repository: %v", inside)
		}
	}
	// 하위 디렉터리에서 시작해도 저장소 맨 위의 규칙을 따른다.
	if sub, _ := collectFind(t, findWalkOptions{roots: []string{filepath.Join(root, "app/sub")}}, testFindSources(nil)); len(sub) != 2 {
		t.Errorf("sub = %v", sub)
	}
	// 저장소 밖에서 시작하면 배포한 checkout이 무시하는 logs/도 찾아야 한다.
	outside, _ := collectFind(t, findWalkOptions{roots: []string{root}}, testFindSources(nil))
	if !slices.Contains(outside, filepath.Join(root, "app/logs/app.log")+":f") || !slices.Contains(outside, filepath.Join(root, "app/sub/drop.bin")+":f") {
		t.Errorf("outside = %v", outside)
	}
	if all, _ := collectFind(t, findWalkOptions{roots: []string{filepath.Join(root, "app")}, noIgnore: true}, testFindSources(nil)); !slices.Contains(all, filepath.Join(root, "app/logs/app.log")+":f") {
		t.Errorf("no ignore = %v", all)
	}
}

func TestFindWalkReportsAStalledDirectory(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "nfs/a", "local/b", "local/c")
	block := make(chan struct{})
	defer close(block)
	sources := testFindSources(nil)
	sources.beforeRead = func(path string) {
		if strings.HasSuffix(path, "/nfs") {
			<-block
		}
	}
	started := time.Now()
	found, report := collectFind(t, findWalkOptions{roots: []string{root}, noIgnore: true, stall: 50 * time.Millisecond}, sources)
	if !report.Partial || !reflect.DeepEqual(report.Stalled, []string{filepath.Join(root, "nfs")}) {
		t.Fatalf("report = %+v", report)
	}
	if !slices.Contains(found, filepath.Join(root, "local/c")+":f") || time.Since(started) > 5*time.Second {
		t.Errorf("found = %v after %v", found, time.Since(started))
	}
}

func TestFindWalkStopsWhenTheReceiverStops(t *testing.T) {
	root := t.TempDir()
	for index := range 50 {
		writeFindTree(t, root, filepath.Join("d", string(rune('a'+index%26))+strings.Repeat("x", index), "f"))
	}
	batches := 0
	report := findWalk(context.Background(), findWalkOptions{roots: []string{root}, workers: 2, noIgnore: true}, testFindSources(nil), func([]findEntry) bool {
		batches++
		return false
	})
	if !report.Stopped || batches != 1 {
		t.Errorf("stopped %v, batches %d", report.Stopped, batches)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 시간 제한으로 끝난 것은 응답 없는 디렉터리가 아니다.
	if report := findWalk(ctx, findWalkOptions{roots: []string{root}, workers: 2, noIgnore: true}, testFindSources(nil), func([]findEntry) bool { return true }); !report.Partial || report.Stalled != nil {
		t.Errorf("a cancelled walk must be partial without stalled directories: %+v", report)
	}
}

func TestFindWalkStatsOnlyWhenAsked(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "big.log")
	stamp := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "big.log"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, stat := range []bool{false, true} {
		var entries []findEntry
		findWalk(context.Background(), findWalkOptions{roots: []string{root}, workers: 1, stat: stat}, testFindSources(nil), func(batch []findEntry) bool {
			entries = append(entries, batch...)
			return true
		})
		if len(entries) != 1 || entries[0].statted != stat || stat && (entries[0].size != 7 || entries[0].mtime != stamp.UnixNano()) {
			t.Errorf("stat %v: %+v", stat, entries)
		}
	}
}

func TestFindWalkCountsUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permission bits")
	}
	root := t.TempDir()
	writeFindTree(t, root, "locked/secret", "open/file")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	found, report := collectFind(t, findWalkOptions{roots: []string{root}, noIgnore: true}, testFindSources(nil))
	if report.UnreadableCount != 1 || !reflect.DeepEqual(report.Unreadable, []string{locked}) || !slices.Contains(found, locked+":d") {
		t.Errorf("found = %v, report = %+v", found, report)
	}
}

func TestFindArenaKeepsEarlierPaths(t *testing.T) {
	var arena findArena
	first := arena.join("dir", "a")
	var paths []string
	for index := range 5000 {
		paths = append(paths, arena.join("some/long/directory/name/to/fill/the/chunk", strings.Repeat("n", index%300)))
	}
	if first != "dir/a" || arena.join("", "top") != "top" || arena.join("/", "etc") != "/etc" {
		t.Errorf("first = %q", first)
	}
	for index, path := range paths {
		if path != "some/long/directory/name/to/fill/the/chunk/"+strings.Repeat("n", index%300) {
			t.Fatalf("path %d changed: %q", index, path)
		}
	}
}

// 마운트 표는 실제 경로를 적는다. symlink인 시작 경로(/var/run, macOS의 /tmp)에서도 마운트를 알아봐야 한다.
func TestFindWalkResolvesASymlinkedRootForMounts(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "real/nfs/x", "real/local/y")
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, "real"))
	if err != nil {
		t.Fatal(err)
	}
	found, report := collectFind(t, findWalkOptions{roots: []string{link}, noIgnore: true}, testFindSources([]diskMount{{Point: filepath.Join(real, "nfs"), FSType: "nfs4"}}))
	want := []string{link + "/local:d", link + "/local/y:f", link + "/nfs:d"}
	slices.Sort(want)
	if !reflect.DeepEqual(found, want) || len(report.Skipped) != 1 || report.Skipped[0].Path != link+"/nfs" {
		t.Errorf("found = %v, skipped = %+v", found, report.Skipped)
	}
}

// 받는 쪽이 느려 결과를 보내려고 기다리는 worker는 응답 없는 디렉터리가 아니다. 응답 없는 디렉터리가 하나 있어도
// 나머지를 다 받을 때까지 끝내면 안 된다.
func TestFindWalkDoesNotCallASlowReceiverAStall(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "nfs/a")
	for index := range 40 {
		writeFindTree(t, root, filepath.Join("d"+strings.Repeat("x", index), "f"))
	}
	block := make(chan struct{})
	defer close(block)
	sources := testFindSources(nil)
	sources.beforeRead = func(path string) {
		if strings.HasSuffix(path, "/nfs") {
			<-block
		}
	}
	count := 0
	report := findWalk(context.Background(), findWalkOptions{roots: []string{root}, workers: 4, noIgnore: true, stall: 20 * time.Millisecond}, sources, func(batch []findEntry) bool {
		time.Sleep(10 * time.Millisecond)
		count += len(batch)
		return true
	})
	// 시작 경로의 항목 41개와 d*/f 40개다. nfs 아래는 읽지 않는다.
	if count != 81 || !reflect.DeepEqual(report.Stalled, []string{filepath.Join(root, "nfs")}) {
		t.Errorf("count %d, stalled %v", count, report.Stalled)
	}
}

func findTestReal(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
