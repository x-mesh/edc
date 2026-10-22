//go:build linux

package edc

import (
	"encoding/binary"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var fsWatchTestDrivers = []string{fsWatchDriverInotify, fsWatchDriverFanotify}

var fsWatchFanotifyProbe struct {
	once sync.Once
	err  error
}

// fsWatchFanotifyUnavailable은 이 환경에서 fanotify를 시작하지 못하는 이유다. 일반 사용자로 돌리면 권한이 없다.
func fsWatchFanotifyUnavailable(t *testing.T) error {
	t.Helper()
	fsWatchFanotifyProbe.once.Do(func() {
		root, err := os.MkdirTemp("", "edc-fanotify")
		if err != nil {
			fsWatchFanotifyProbe.err = err
			return
		}
		defer os.RemoveAll(root)
		if root, err = filepath.EvalSymlinks(root); err != nil {
			fsWatchFanotifyProbe.err = err
			return
		}
		source, err := newFanotifyWatchSource(fsWatchOptions{root: root, recursive: true})
		if err == nil {
			source.close()
		}
		fsWatchFanotifyProbe.err = err
	})
	return fsWatchFanotifyProbe.err
}

// fsWatchSkipUnavailableDriver는 fanotify를 쓸 수 없으면 fanotify test를 건너뛴다. CI는 root로 돌릴 때
// EDC_TEST_FANOTIFY=required를 주어 건너뛰지 않고 실패하게 한다.
func fsWatchSkipUnavailableDriver(t *testing.T, driver string) {
	t.Helper()
	if driver != fsWatchDriverFanotify {
		return
	}
	if err := fsWatchFanotifyUnavailable(t); err != nil {
		if os.Getenv("EDC_TEST_FANOTIFY") == "required" {
			t.Fatalf("fanotify: %v", err)
		}
		t.Skipf("fanotify: %v", err)
	}
}

func TestFSWatchAutoDriverPrefersFanotifyForRecursiveWatches(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, recursive := range []bool{false, true} {
		source, err := newFSWatchSource(fsWatchOptions{root: root, recursive: recursive, driver: fsWatchDriverAuto})
		if err != nil {
			t.Fatal(err)
		}
		source.close()
		want := fsWatchDriverInotify
		if recursive && fsWatchFanotifyUnavailable(t) == nil {
			want = fsWatchDriverFanotify
		}
		if source.driver != want {
			t.Fatalf("recursive=%t driver = %q, want %q", recursive, source.driver, want)
		}
	}
}

// filesystem mark는 마운트 경계를 넘지 않으므로 루트 아래의 마운트마다 mark를 건다. sysfs처럼 mark를 걸 수 없는
// 마운트와 루트 밖 디렉터리를 bind mount한 지점은 빼고 알린다.
func TestFSWatchFanotifyWatchesTheMountsBelowTheRoot(t *testing.T) {
	fsWatchSkipUnavailableDriver(t, fsWatchDriverFanotify)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	mount := func(name, source, kind string, flags uintptr) {
		t.Helper()
		point := filepath.Join(root, name)
		if err := os.Mkdir(point, 0700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(source, point, kind, flags, ""); err != nil {
			t.Skipf("mount %s: %v", kind, err)
		}
		t.Cleanup(func() { unix.Unmount(point, unix.MNT_DETACH) })
	}
	mount("cache", "tmpfs", "tmpfs", 0)
	mount("sys", "sysfs", "sysfs", 0)
	mount("shared", outside, "", unix.MS_BIND)
	var notice strings.Builder
	previous := fsWatchNotice.swap(&notice)
	defer fsWatchNotice.swap(previous)
	source := fsWatchTestSource(t, root, true, fsWatchDriverAuto)
	if source.driver != fsWatchDriverFanotify {
		t.Fatalf("driver = %q, want fanotify", source.driver)
	}
	for _, want := range []string{filepath.Join(root, "sys"), filepath.Join(root, "shared")} {
		if !strings.Contains(notice.String(), want) {
			t.Fatalf("notice %q does not name %s", notice.String(), want)
		}
	}
	if strings.Contains(notice.String(), filepath.Join(root, "cache")) {
		t.Fatalf("notice %q names the tmpfs mount", notice.String())
	}
	if err := os.WriteFile(filepath.Join(root, "cache", "inside.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "create", "cache/inside.txt")
}

func TestFSWatchPlansTheMountsBelowTheRoot(t *testing.T) {
	mounts := parseFSWatchMounts(strings.Join([]string{
		"1 0 8:3 / / rw - xfs /dev/sda3 rw",
		"2 1 8:3 /etc /etc ro - xfs /dev/sda3 rw",
		"3 1 0:5 / /srv/data/cache rw - tmpfs tmpfs rw",
		"4 1 0:6 / /srv/data/cache rw - tmpfs tmpfs rw",
		"5 1 0:7 / /srv/data/sys rw - sysfs sysfs rw",
		"6 5 0:8 / /srv/data/sys/kernel/debug rw - debugfs debugfs rw",
		"7 1 8:3 /mnt/other /srv/data/shared rw - xfs /dev/sda3 rw",
		"8 1 8:3 /srv/data/real /srv/data/alias rw - xfs /dev/sda3 rw",
		"9 1 0:9 / /srv/data/build rw - tmpfs tmpfs rw",
		`10 1 0:10 / /srv/data/with\040space rw - tmpfs tmpfs rw`,
		"11 1 0:11 / /elsewhere rw - tmpfs tmpfs rw",
	}, "\n"))
	excluded := func(relative string) bool { return relative == "build" }
	filesystems, skipped := planFSWatchMounts(mounts, "/srv/data", true, excluded)
	var points [][]string
	for _, filesystem := range filesystems {
		points = append(points, filesystem.points)
	}
	want := [][]string{{"/srv/data", "/srv/data/alias"}, {"/srv/data/cache"}, {"/srv/data/sys"}, {"/srv/data/with space"}, {"/srv/data/sys/kernel/debug"}}
	if !slices.EqualFunc(points, want, slices.Equal) {
		t.Fatalf("points = %v, want %v", points, want)
	}
	if filesystems[0].mount.point != "/" || filesystems[1].mount.device != "0:6" {
		t.Fatalf("filesystems = %+v", filesystems)
	}
	if !slices.Equal(skipped, []string{"/srv/data/shared"}) {
		t.Fatalf("skipped = %v", skipped)
	}
	if filesystems, skipped := planFSWatchMounts(mounts, "/srv/data", false, excluded); len(filesystems) != 1 || len(skipped) != 0 {
		t.Fatalf("non-recursive plan = %+v, %v", filesystems, skipped)
	}
	if got := fsWatchOutermostPaths([]string{"/sys/kernel/debug", "/proc", "/sys", "/system"}); !slices.Equal(got, []string{"/proc", "/sys", "/system"}) {
		t.Fatalf("outermost = %v", got)
	}
	if got := fsWatchShortList([]string{"/a", "/b", "/c", "/d", "/e", "/f", "/g"}); got != "/a, /b, /c, /d, /e … (+2)" {
		t.Fatalf("short list = %q", got)
	}
}

func fsWatchFanotifyTestKey(name string) string {
	return "testfsid" + fsWatchFanotifyHandleKey(unix.NewFileHandle(1, []byte(name)))
}

// fsWatchFanotifyTestEvent는 커널과 같은 배치로 이벤트 하나를 만든다. info record 길이는 4 byte 단위로 맞춘다.
func fsWatchFanotifyTestEvent(mask uint64, parent, name, target string) []byte {
	record := func(kind byte, key, name string) []byte {
		body := make([]byte, fsWatchFanotifyHandleOffset)
		body[0] = kind
		copy(body[fsWatchFanotifyFSIDOffset:], key[:fsWatchFanotifyFSIDSize])
		binary.NativeEndian.PutUint32(body[12:16], uint32(len(key)-fsWatchFanotifyFSIDSize-4))
		body = append(body, key[fsWatchFanotifyFSIDSize:]...)
		if kind == unix.FAN_EVENT_INFO_TYPE_DFID_NAME {
			body = append(append(body, name...), 0)
		}
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
		binary.NativeEndian.PutUint16(body[2:4], uint16(len(body)))
		return body
	}
	var info []byte
	if parent != "" {
		info = record(unix.FAN_EVENT_INFO_TYPE_DFID_NAME, parent, name)
	}
	if target != "" {
		info = append(info, record(unix.FAN_EVENT_INFO_TYPE_FID, target, "")...)
	}
	event := make([]byte, fsWatchFanotifyMetadataSize)
	binary.NativeEndian.PutUint32(event[0:4], uint32(len(event)+len(info)))
	event[4] = unix.FANOTIFY_METADATA_VERSION
	binary.NativeEndian.PutUint16(event[6:8], fsWatchFanotifyMetadataSize)
	binary.NativeEndian.PutUint64(event[8:16], mask)
	binary.NativeEndian.PutUint32(event[16:20], 0xffffffff)
	return append(event, info...)
}

func TestFSWatchFanotifyParsesEventRecords(t *testing.T) {
	parent, target := fsWatchFanotifyTestKey("parent"), fsWatchFanotifyTestKey("child-dir")
	buffer := slices.Concat(
		fsWatchFanotifyTestEvent(unix.FAN_DELETE|unix.FAN_ONDIR, parent, "gone", target),
		fsWatchFanotifyTestEvent(unix.FAN_MODIFY, parent, "file.txt", ""),
		fsWatchFanotifyTestEvent(unix.FAN_Q_OVERFLOW, "", "", ""),
	)
	events, err := parseFSWatchFanotify(buffer)
	if err != nil {
		t.Fatal(err)
	}
	want := []fsWatchFanotifyEvent{
		{mask: unix.FAN_DELETE | unix.FAN_ONDIR, parent: parent, name: "gone", target: target},
		{mask: unix.FAN_MODIFY, parent: parent, name: "file.txt"},
		{mask: unix.FAN_Q_OVERFLOW},
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	version := slices.Clone(buffer)
	version[4]++
	for name, broken := range map[string][]byte{
		"truncated": buffer[:len(buffer)-1],
		"short":     buffer[:fsWatchFanotifyMetadataSize-1],
		"version":   version,
	} {
		if _, err := parseFSWatchFanotify(broken); !errors.Is(err, errFSWatchFanotifyMalformed) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestFSWatchFanotifyOrdersMergedMasks(t *testing.T) {
	for _, test := range []struct {
		mask          uint64
		exists, isDir bool
		want          []string
	}{
		{unix.FAN_CREATE | unix.FAN_MODIFY, true, false, []string{"create", "modify"}},
		{unix.FAN_MODIFY | unix.FAN_MOVED_FROM, false, false, []string{"modify", "rename"}},
		{unix.FAN_MOVED_TO | unix.FAN_DELETE, false, false, []string{"create", "remove"}},
		{unix.FAN_DELETE | unix.FAN_CREATE, true, false, []string{"remove", "create"}},
		{unix.FAN_DELETE | unix.FAN_MODIFY, true, false, []string{"modify", "remove"}},
		{unix.FAN_CREATE | unix.FAN_MODIFY, true, true, []string{"create"}},
	} {
		if got := fsWatchFanotifyOperations(test.mask, test.exists, test.isDir); !slices.Equal(got, test.want) {
			t.Fatalf("mask %#x exists=%t dir=%t = %v, want %v", test.mask, test.exists, test.isDir, got, test.want)
		}
	}
}

// fsWatchFanotifyTestWatch는 handle을 paths에 있는 경로로만 여는 감시다. 나머지 handle은 지워진 디렉터리처럼 ESTALE이다.
// 경로 ""는 감시 루트, ".."는 그 부모 디렉터리다.
func fsWatchFanotifyTestWatch(t *testing.T, paths map[string]string) (*fsWatchFanotify, *[]string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for key, path := range paths {
		switch path {
		case "":
			paths[key] = root
		case "..":
			paths[key] = filepath.Dir(root)
		}
	}
	var emitted []string
	lookup := func(key string) (string, error) {
		if path, ok := paths[key]; ok {
			return path, nil
		}
		return "", unix.ESTALE
	}
	watch := newFSWatchFanotify(fsWatchOptions{root: root, recursive: true}, lookup, func(event fsWatchEvent) bool {
		emitted = append(emitted, event.Event+" "+event.Path)
		return true
	})
	return watch, &emitted
}

func TestFSWatchFanotifyJoinsTheEntriesOfARemovedTree(t *testing.T) {
	root, tree, a, deep := fsWatchFanotifyTestKey("root"), fsWatchFanotifyTestKey("tree"), fsWatchFanotifyTestKey("a"), fsWatchFanotifyTestKey("deep")
	watch, emitted := fsWatchFanotifyTestWatch(t, map[string]string{root: ""})
	for _, event := range []fsWatchFanotifyEvent{
		{mask: unix.FAN_DELETE, parent: deep, name: "one.txt"},
		{mask: unix.FAN_DELETE | unix.FAN_ONDIR, parent: a, name: "deep", target: deep},
		{mask: unix.FAN_DELETE, parent: a, name: "two.txt"},
		{mask: unix.FAN_DELETE | unix.FAN_ONDIR, parent: tree, name: "a", target: a},
	} {
		if err := watch.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(*emitted) != 0 || watch.waitingCount != 4 {
		t.Fatalf("emitted %v with %d waiting before the tree was removed", *emitted, watch.waitingCount)
	}
	if err := watch.handle(fsWatchFanotifyEvent{mask: unix.FAN_DELETE | unix.FAN_ONDIR, parent: root, name: "tree", target: tree}); err != nil {
		t.Fatal(err)
	}
	want := []string{"remove tree/a/deep/one.txt", "remove tree/a/deep", "remove tree/a/two.txt", "remove tree/a", "remove tree"}
	if !slices.Equal(*emitted, want) {
		t.Fatalf("emitted %v, want %v", *emitted, want)
	}
	if watch.waitingCount != 0 || len(watch.waiting) != 0 || len(watch.stale) != 0 {
		t.Fatalf("left %d waiting, %d stale", watch.waitingCount, len(watch.stale))
	}
}

// 커널은 아직 읽지 않은 디렉터리의 생성과 삭제를 한 이벤트로 합쳐 그 안의 이벤트보다 앞에 둔다.
func TestFSWatchFanotifyJoinsTheEntriesOfAMergedDirectory(t *testing.T) {
	root, a, b := fsWatchFanotifyTestKey("root"), fsWatchFanotifyTestKey("a"), fsWatchFanotifyTestKey("b")
	watch, emitted := fsWatchFanotifyTestWatch(t, map[string]string{root: ""})
	for _, event := range []fsWatchFanotifyEvent{
		{mask: unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_ONDIR, parent: root, name: "a", target: a},
		{mask: unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_ONDIR, parent: a, name: "b", target: b},
		{mask: unix.FAN_CREATE | unix.FAN_MODIFY | unix.FAN_DELETE, parent: b, name: "f.txt"},
	} {
		if err := watch.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"create a", "remove a", "create a/b", "remove a/b", "create a/b/f.txt", "modify a/b/f.txt", "remove a/b/f.txt"}
	if !slices.Equal(*emitted, want) {
		t.Fatalf("emitted %v, want %v", *emitted, want)
	}
	if watch.waitingCount != 0 {
		t.Fatalf("left %d waiting", watch.waitingCount)
	}
}

// reader는 적게 읽은 뒤 다음 read까지 기다린다. 그동안 생겼다 지워진 트리는 커널이 디렉터리 이벤트를 합쳐 보낸다.
func TestFSWatchFanotifyReportsATreeRemovedBeforeItWasRead(t *testing.T) {
	fsWatchSkipUnavailableDriver(t, fsWatchDriverFanotify)
	root := t.TempDir()
	source := fsWatchTestSource(t, root, true, fsWatchDriverFanotify)
	if err := os.WriteFile(filepath.Join(root, "first.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	fsWatchWaitEvent(t, source, "create", "first.txt")
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "f.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	missing := map[string]bool{}
	for _, want := range []string{"create a", "create a/b", "create a/b/f.txt", "modify a/b/f.txt", "remove a/b/f.txt", "remove a/b", "remove a"} {
		missing[want] = true
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(missing) > 0 {
		select {
		case event, ok := <-source.events:
			if !ok {
				t.Fatal("event source closed")
			}
			delete(missing, event.Event+" "+event.Path)
		case err := <-source.errors:
			t.Fatalf("watch error: %v", err)
		case <-timer.C:
			t.Fatalf("missing %v", slices.Sorted(maps.Keys(missing)))
		}
	}
}

// 옮긴 뒤 지운 디렉터리는 옮기기 전의 이벤트를 이전 경로에, 옮긴 뒤의 이벤트를 새 경로에 붙인다.
func TestFSWatchFanotifyJoinsEventsAcrossADirectoryMove(t *testing.T) {
	root, moved := fsWatchFanotifyTestKey("root"), fsWatchFanotifyTestKey("moved")
	watch, emitted := fsWatchFanotifyTestWatch(t, map[string]string{root: ""})
	for _, event := range []fsWatchFanotifyEvent{
		{mask: unix.FAN_CREATE, parent: moved, name: "before.txt"},
		{mask: unix.FAN_MOVED_FROM | unix.FAN_ONDIR, parent: root, name: "old", target: moved},
		{mask: unix.FAN_MOVED_TO | unix.FAN_ONDIR, parent: root, name: "new", target: moved},
		{mask: unix.FAN_CREATE, parent: moved, name: "after.txt"},
		{mask: unix.FAN_DELETE | unix.FAN_ONDIR, parent: root, name: "new", target: moved},
	} {
		if err := watch.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"create old/before.txt", "rename old", "create new", "create new/after.txt", "remove new"}
	if !slices.Equal(*emitted, want) {
		t.Fatalf("emitted %v, want %v", *emitted, want)
	}
}

// 감시 루트 밖에서 지운 트리의 이벤트도 경로가 이어질 때까지 기다리고, 이어지면 알리지 않고 버린다. 지운
// 디렉터리에서 아직 열려 있는 파일에 쓴 이벤트도 기다리지 않고 버린다.
func TestFSWatchFanotifyDropsRemovedTreesOutsideTheRoot(t *testing.T) {
	root, outside, build := fsWatchFanotifyTestKey("root"), fsWatchFanotifyTestKey("outside"), fsWatchFanotifyTestKey("build")
	watch, emitted := fsWatchFanotifyTestWatch(t, map[string]string{root: "", outside: "/elsewhere"})
	for _, event := range []fsWatchFanotifyEvent{
		{mask: unix.FAN_DELETE, parent: build, name: "out.o"},
		{mask: unix.FAN_DELETE | unix.FAN_ONDIR, parent: outside, name: "build", target: build},
		{mask: unix.FAN_MODIFY, parent: build, name: "build.log"},
	} {
		if err := watch.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(*emitted) != 0 || watch.waitingCount != 0 {
		t.Fatalf("emitted %v with %d waiting", *emitted, watch.waitingCount)
	}
}

func TestFSWatchFanotifyReportsRootRemovalAndOverflow(t *testing.T) {
	parent := fsWatchFanotifyTestKey("parent")
	watch, _ := fsWatchFanotifyTestWatch(t, map[string]string{parent: ".."})
	err := watch.handle(fsWatchFanotifyEvent{mask: unix.FAN_MOVED_FROM | unix.FAN_ONDIR, parent: parent, name: filepath.Base(watch.options.root)})
	if err == nil || !strings.Contains(err.Error(), watch.options.root) {
		t.Fatalf("root move error = %v", err)
	}
	if err := watch.handle(fsWatchFanotifyEvent{mask: unix.FAN_Q_OVERFLOW}); err == nil || err.Error() != T("watchfs.overflow") {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestFSWatchUnescapeMount(t *testing.T) {
	for text, want := range map[string]string{
		"/mnt/plain":            "/mnt/plain",
		`/mnt/with\040space`:    "/mnt/with space",
		`/mnt/back\134slash`:    `/mnt/back\slash`,
		`/mnt/trailing\04`:      `/mnt/trailing\04`,
		`/mnt/tab\011and\012nl`: "/mnt/tab\tand\nnl",
	} {
		if got := fsWatchUnescapeMount(text); got != want {
			t.Fatalf("unescape %q = %q, want %q", text, got, want)
		}
	}
}

// auto가 inotify를 고르면 이유가 source에 남는다. 재귀 감시가 아니면 not_recursive, 직접 고르거나 fanotify면 비어 있다.
func TestFSWatchSourceRecordsWhyAutoChoseInotify(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name       string
		options    fsWatchOptions
		wantDriver string
		wantReason string
	}{
		{"not recursive", fsWatchOptions{root: root, driver: fsWatchDriverAuto}, fsWatchDriverInotify, fsWatchReasonNotRecursive},
		{"chosen by the user", fsWatchOptions{root: root, recursive: true, driver: fsWatchDriverInotify}, fsWatchDriverInotify, ""},
	} {
		source, err := newFSWatchSource(test.options)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if source.driver != test.wantDriver || source.driverReason != test.wantReason {
			t.Errorf("%s: driver %q reason %q, want %q and %q", test.name, source.driver, source.driverReason, test.wantDriver, test.wantReason)
		}
		source.close()
	}
}

// fanotify를 시작하지 못하면 auto는 inotify로 넘어가고 이유를 fanotify_unavailable로 남긴다. /proc은 fanotify가 쓰는
// file handle을 지원하지 않고, root가 아니면 fanotify_init 자체가 거부되므로 어느 쪽으로 실행해도 이 분기를 탄다.
func TestFSWatchSourceRecordsWhenAutoFellBackToInotify(t *testing.T) {
	root := "/proc/self/attr"
	if _, err := os.Stat(root); err != nil {
		t.Skip(err)
	}
	// 건너뛰는 것은 이 환경의 inotify가 그 경로를 아예 못 보는 경우뿐이다. 그 밖의 오류는 폴백이 깨진 것이므로 실패다.
	direct, err := newFSNotifyWatchSource(fsWatchOptions{root: root, recursive: true})
	if err != nil {
		t.Skipf("inotify cannot watch %s here: %v", root, err)
	}
	direct.close()
	source, err := newFSWatchSource(fsWatchOptions{root: root, recursive: true, driver: fsWatchDriverAuto})
	if err != nil {
		t.Fatalf("auto did not fall back to inotify for %s: %v", root, err)
	}
	defer source.close()
	if source.driver != fsWatchDriverInotify || source.driverReason != fsWatchReasonFanotifyDown {
		t.Fatalf("driver %q reason %q, want %q and %q", source.driver, source.driverReason, fsWatchDriverInotify, fsWatchReasonFanotifyDown)
	}
}

// pid는 metadata의 20번째 byte부터 4 byte다(event_len, vers, reserved, metadata_len, mask, fd, pid 순서).
func TestFSWatchFanotifyParsesThePIDOfTheProcessThatCausedTheEvent(t *testing.T) {
	parent := fsWatchFanotifyTestKey("parent")
	event := fsWatchFanotifyTestEvent(unix.FAN_MODIFY, parent, "file.txt", "")
	binary.NativeEndian.PutUint32(event[20:24], 4242)
	events, err := parseFSWatchFanotify(event)
	if err != nil || len(events) != 1 || events[0].pid != 4242 || events[0].name != "file.txt" {
		t.Fatalf("events = %+v, err %v", events, err)
	}
}

func TestFSWatchFanotifyProcessNamesAreCachedWithinABatch(t *testing.T) {
	watch := newFSWatchFanotify(fsWatchOptions{root: "/"}, nil, nil)
	calls := map[int32]int{}
	names := map[int32]string{10: "proc10", 77: ""}
	watch.processName = func(pid int32) string {
		calls[pid]++
		return names[pid]
	}
	for _, pid := range []int32{0, -1} {
		if name := watch.process(pid); name != "" || len(calls) != 0 {
			t.Fatalf("pid %d: name %q, lookups %v", pid, name, calls)
		}
	}
	for range 5 {
		if name := watch.process(10); name != "proc10" {
			t.Fatalf("name = %q", name)
		}
		watch.process(77)
	}
	if calls[10] != 1 || calls[77] != 1 {
		t.Errorf("lookups = %v, want one for each pid in a batch, also for the process that is gone", calls)
	}
	// 새 묶음에서는 다시 찾는다. 그 사이에 pid가 다른 process로 재사용되었을 수 있다.
	names[10] = "reused"
	watch.nextBatch()
	if name := watch.process(10); name != "reused" || calls[10] != 2 {
		t.Errorf("after a new batch: name %q, lookups %v", name, calls)
	}
	// 항목이 한도를 넘으면 비우고 계속 동작한다.
	for pid := int32(1000); pid < 1000+fsWatchProcessEntries+10; pid++ {
		watch.process(pid)
	}
	if len(watch.processes) > fsWatchProcessEntries {
		t.Errorf("cache holds %d entries, limit %d", len(watch.processes), fsWatchProcessEntries)
	}
}

// 실제 fanotify에서 이 test를 돌리는 process가 파일을 쓰면, 이벤트가 그 pid와 이름을 담아야 한다.
func TestFSWatchFanotifyReportsTheProcessThatChangedAFile(t *testing.T) {
	fsWatchSkipUnavailableDriver(t, fsWatchDriverFanotify)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := fsWatchTestSource(t, root, true, fsWatchDriverFanotify)
	if err := os.WriteFile(filepath.Join(root, "who.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(func() string { data, _ := os.ReadFile("/proc/self/comm"); return string(data) }())
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-source.events:
			if event.Path != "who.txt" {
				continue
			}
			if event.PID != os.Getpid() || event.Process != want || want == "" {
				t.Fatalf("event = %+v, want pid %d and process %q", event, os.Getpid(), want)
			}
			return
		case err := <-source.errors:
			t.Fatalf("watch error: %v", err)
		case <-timer.C:
			t.Fatal("no event for who.txt")
		}
	}
}
