//go:build linux

package edc

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// fanotify는 파일시스템 전체에 mark 하나를 걸므로 디렉터리를 훑지 않고, inotify watch 한도도 받지 않는다.
// 대신 CAP_SYS_ADMIN과 FAN_REPORT_TARGET_FID(Linux 5.17)가 필요하다.
const (
	fsWatchFanotifyMask = unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_MOVED_FROM | unix.FAN_MOVED_TO | unix.FAN_MODIFY | unix.FAN_ONDIR
	// fsWatchFanotifyPathEntries는 디렉터리 handle에서 찾은 경로를 기억할 수다. 감시 루트 밖의 이벤트도 경로를
	// 찾아야 거를 수 있으므로, 자주 바뀌는 디렉터리를 다시 찾지 않게 둔다.
	fsWatchFanotifyPathEntries = 16384
	// fsWatchFanotifyWaitingLimit는 경로를 아직 모르는 이벤트 수다. 지워진 트리의 이벤트는 그 트리의 맨 위
	// 디렉터리가 지워졌다는 이벤트가 올 때까지 기다린다.
	fsWatchFanotifyWaitingLimit = 1 << 18
	// fsWatchFanotifyBatchDelay는 적게 읽었을 때 다음 read까지 기다리는 시간이다. 파일시스템 전체의 이벤트를 몇 개씩
	// 읽으면 깨어나는 비용이 이벤트 처리보다 크다. 감시 루트 밖에서 파일을 15만 번 바꾸는 동안 기다리지 않으면
	// edc가 CPU를 0.93초, 기다리면 0.24초 썼다.
	fsWatchFanotifyBatchDelay   = 5 * time.Millisecond
	fsWatchFanotifyBufferSize   = 256 << 10
	fsWatchFanotifyMetadataSize = 24
	// fsWatchFanotifyHandleOffset은 info record에서 handle_type이 시작하는 위치다. header 4, fsid 8, handle_bytes 4 byte 뒤에 온다.
	fsWatchFanotifyHandleOffset = 16
	fsWatchFanotifyFSIDOffset   = 4
	fsWatchFanotifyFSIDSize     = 8
	// fsWatchSkippedShown은 건너뛴 마운트 안내에 이름을 보일 수다.
	fsWatchSkippedShown = 5
)

// newFSWatchSource는 --driver에 맞는 Linux 감시를 만든다. auto는 재귀 감시에서 fanotify를 먼저 쓰고, 쓸 수
// 없으면 그 이유를 알린 뒤 inotify로 디렉터리마다 watch를 등록한다. 비재귀 감시는 watch 하나라 inotify로 충분하다.
func newFSWatchSource(options fsWatchOptions) (fsWatchSource, error) {
	switch options.driver {
	case fsWatchDriverInotify:
		return newFSNotifyWatchSource(options)
	case fsWatchDriverFanotify:
		source, err := newFanotifyWatchSource(options)
		if err != nil {
			return source, errors.New(T("watchfs.fanotify.unavailable", err))
		}
		return source, nil
	}
	if !options.recursive {
		return newFSNotifyWatchSource(options)
	}
	source, err := newFanotifyWatchSource(options)
	if err == nil {
		return source, nil
	}
	if isTerminal(os.Stderr) {
		fmt.Fprintln(fsWatchNotice, T("watchfs.fanotify.fallback", err))
	}
	return newFSNotifyWatchSource(options)
}

// fsWatchFanotifyEvent는 FAN_REPORT_DFID_NAME_TARGET 이벤트 하나다. parent는 name 항목이 든 디렉터리, target은 그
// 항목 자신의 file handle이고, 둘 다 fsid, handle_type과 handle을 그대로 이은 key다.
type fsWatchFanotifyEvent struct {
	mask   uint64
	parent string
	name   string
	target string
}

// fsWatchFanotifyWaiting은 부모 디렉터리가 이미 지워져 경로를 아직 모르는 이벤트다. seq는 읽은 순서다.
type fsWatchFanotifyWaiting struct {
	seq   uint64
	event fsWatchFanotifyEvent
}

type fsWatchFanotifyResolved struct {
	seq       uint64
	directory string
	event     fsWatchFanotifyEvent
}

// fsWatchFanotify는 fanotify 이벤트를 edc event로 바꾸는 상태다. 이벤트 goroutine 하나만 쓴다.
type fsWatchFanotify struct {
	options             fsWatchOptions
	rootPrefix          string
	rootParent          string
	ignored             func(string, fs.DirEntry) bool
	lookup              func(key string) (string, error)
	emit                func(fsWatchEvent) bool
	paths               map[string]string
	stale               map[string]bool
	waiting             map[string][]fsWatchFanotifyWaiting
	waitingCount        int
	seq                 uint64
	excludedDirectories map[string]bool
}

func newFSWatchFanotify(options fsWatchOptions, lookup func(string) (string, error), emit func(fsWatchEvent) bool) *fsWatchFanotify {
	rootPrefix := options.root + string(filepath.Separator)
	if options.root == string(filepath.Separator) {
		rootPrefix = options.root
	}
	return &fsWatchFanotify{
		options: options, rootPrefix: rootPrefix, rootParent: filepath.Dir(options.root),
		ignored: fsWatchIgnored(options), lookup: lookup, emit: emit,
		paths: map[string]string{}, stale: map[string]bool{}, waiting: map[string][]fsWatchFanotifyWaiting{},
		excludedDirectories: map[string]bool{},
	}
}

func newFanotifyWatchSource(options fsWatchOptions) (fsWatchSource, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|unix.FAN_REPORT_DFID_NAME_TARGET, unix.O_RDONLY)
	if err != nil {
		return fsWatchSource{}, fsWatchFanotifyReason(err)
	}
	mountFDs := map[string]int{}
	release := func() {
		unix.Close(fd)
		for _, mountFD := range mountFDs {
			unix.Close(mountFD)
		}
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		release()
		return fsWatchSource{}, err
	}
	excludedDirectories := map[string]bool{}
	excluded := func(relative string) bool {
		return fsWatchExcluded(options.exclude, filepath.ToSlash(relative)) || fsWatchUnderExcluded(options.exclude, excludedDirectories, relative)
	}
	filesystems, skipped := planFSWatchMounts(parseFSWatchMounts(string(mountinfo)), options.root, options.recursive, excluded)
	for index, filesystem := range filesystems {
		point := filesystem.mount.point
		if index == 0 {
			point = options.root
		}
		err := fsWatchFanotifyAdd(fd, point, mountFDs)
		if err == nil {
			continue
		}
		// 감시 루트의 파일시스템을 볼 수 없으면 fanotify를 쓸 이유가 없다. 그 아래 sysfs나 devpts 같은 마운트는 빼고 감시한다.
		if index == 0 {
			release()
			return fsWatchSource{}, err
		}
		skipped = append(skipped, filesystem.points...)
	}
	lookup := func(key string) (string, error) {
		mountFD, ok := mountFDs[key[:min(len(key), fsWatchFanotifyFSIDSize)]]
		if !ok {
			return "", unix.ESTALE
		}
		return fsWatchOpenHandle(mountFD, key[fsWatchFanotifyFSIDSize:])
	}
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		release()
		return fsWatchSource{}, err
	}
	if skipped = fsWatchOutermostPaths(skipped); len(skipped) > 0 {
		fmt.Fprintln(fsWatchNotice, T("watchfs.fanotify.skipped", len(skipped), fsWatchShortList(skipped)))
	}
	events := make(chan fsWatchEvent, 256)
	failures := make(chan error, 1)
	done, finished := make(chan struct{}), make(chan struct{})
	watch := newFSWatchFanotify(options, lookup, func(event fsWatchEvent) bool {
		select {
		case events <- event:
			return true
		case <-done:
			return false
		}
	})
	go func() {
		defer close(finished)
		defer close(events)
		defer close(failures)
		fail := func(err error) {
			select {
			case failures <- err:
			case <-done:
			}
		}
		buffer := make([]byte, fsWatchFanotifyBufferSize)
		for {
			n, err := fsWatchFanotifyRead(fd, wake, buffer)
			if errors.Is(err, fs.ErrClosed) {
				return
			}
			if err != nil {
				fail(err)
				return
			}
			batch, err := parseFSWatchFanotify(buffer[:n])
			if err != nil {
				fail(err)
				return
			}
			for _, event := range batch {
				if err := watch.handle(event); err != nil {
					if !errors.Is(err, fs.ErrClosed) {
						fail(err)
					}
					return
				}
			}
			if n < len(buffer)/2 {
				time.Sleep(fsWatchFanotifyBatchDelay)
			}
		}
	}()
	var once sync.Once
	return fsWatchSource{events: events, errors: failures, driver: fsWatchDriverFanotify, close: func() {
		once.Do(func() {
			close(done)
			unix.Write(wake, binary.NativeEndian.AppendUint64(nil, 1))
			<-finished
			unix.Close(wake)
			release()
		})
	}}, nil
}

// fsWatchFanotifyRead는 이벤트가 오거나 wake가 울릴 때까지 기다렸다가 읽는다. fd를 Go netpoller에 맡기면 이벤트가
// 들어올 때마다 runtime이 깨어난다. 감시 루트 밖에서 파일을 15만 번 바꾸는 동안 epoll_pwait을 5만 번 넘게 불렀다.
func fsWatchFanotifyRead(fd, wake int, buffer []byte) (int, error) {
	descriptors := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(descriptors, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, err
		}
		if descriptors[1].Revents != 0 {
			return 0, fs.ErrClosed
		}
		n, err := unix.Read(fd, buffer)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		return n, err
	}
}

// fsWatchFanotifyReason은 fanotify를 시작하지 못한 errno를 사용자가 고칠 수 있는 이유로 바꾼다.
func fsWatchFanotifyReason(err error) error {
	switch {
	case errors.Is(err, unix.EPERM):
		return errors.New(T("watchfs.fanotify.permission"))
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return errors.New(T("watchfs.fanotify.kernel"))
	case errors.Is(err, unix.ENODEV), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EXDEV), errors.Is(err, unix.ESTALE):
		return errors.New(T("watchfs.fanotify.filesystem"))
	}
	return err
}

// fsWatchFanotifyAdd는 point가 있는 파일시스템에 mark를 걸고, 그 파일시스템의 handle을 열 mount fd를 fsid로 둔다.
// mark를 받아도 handle을 경로로 되돌리지 못하는 파일시스템이 있으므로, point의 handle을 먼저 다시 열어 본다.
func fsWatchFanotifyAdd(fd int, point string, mountFDs map[string]int) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(point, &stat); err != nil {
		return err
	}
	fsid := string(binary.NativeEndian.AppendUint32(binary.NativeEndian.AppendUint32(nil, uint32(stat.Fsid.Val[0])), uint32(stat.Fsid.Val[1])))
	// 이벤트는 fsid로만 파일시스템을 가린다. 같은 fsid가 둘이면 어느 마운트로 경로를 찾을지 알 수 없다.
	if _, ok := mountFDs[fsid]; ok {
		return unix.EXDEV
	}
	// open_by_handle_at은 O_PATH로 연 mount fd를 받지 않는다.
	mountFD, err := unix.Open(point, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	handle, _, err := unix.NameToHandleAt(unix.AT_FDCWD, point, 0)
	var resolved string
	if err == nil {
		resolved, err = fsWatchOpenHandle(mountFD, fsWatchFanotifyHandleKey(handle))
	}
	if err == nil && resolved != point {
		err = errors.New(T("watchfs.fanotify.path", resolved))
	} else if err == nil {
		err = unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, fsWatchFanotifyMask, unix.AT_FDCWD, point)
	}
	if err != nil {
		unix.Close(mountFD)
		return fsWatchFanotifyReason(err)
	}
	mountFDs[fsid] = mountFD
	return nil
}

// fsWatchMount는 /proc/self/mountinfo의 한 줄이다. source는 파일시스템 안에서 이 마운트가 보여 주는 경로다.
type fsWatchMount struct {
	device string
	source string
	point  string
}

// fsWatchFilesystem은 mark를 걸 파일시스템 하나다. mount는 handle을 경로로 바꿀 때 쓰는 마운트이고, points는 감시
// 루트 아래에서 이 파일시스템을 마운트한 지점이다.
type fsWatchFilesystem struct {
	mount  fsWatchMount
	points []string
}

func parseFSWatchMounts(mountinfo string) []fsWatchMount {
	var mounts []fsWatchMount
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mounts = append(mounts, fsWatchMount{device: fields[2], source: fsWatchUnescapeMount(fields[3]), point: fsWatchUnescapeMount(fields[4])})
	}
	return mounts
}

// planFSWatchMounts는 mark를 걸 파일시스템을 고른다. 첫 항목은 감시 루트가 있는 파일시스템이다. 하나의 mark는
// 마운트 경계를 넘지 않으므로 재귀 감시는 루트 아래의 다른 파일시스템마다 mark를 건다. 이미 고른 파일시스템을
// 다시 마운트한 지점의 이벤트는 처음 고른 마운트를 거친 경로로 나온다. 그 경로가 감시 루트 밖이면 알릴 수 없으므로
// skipped에 넣는다. excluded는 --exclude에 걸린 마운트를 거른다.
func planFSWatchMounts(mounts []fsWatchMount, root string, recursive bool, excluded func(string) bool) ([]fsWatchFilesystem, []string) {
	// 같은 지점에 다시 마운트하면 나중 줄이 앞의 마운트를 가린다.
	visible := map[string]fsWatchMount{}
	for _, mount := range mounts {
		visible[mount.point] = mount
	}
	var top fsWatchMount
	var below []string
	for point, mount := range visible {
		if fsWatchPathUnder(point, root) && len(point) >= len(top.point) {
			top = mount
		}
		if point != root && fsWatchPathUnder(root, point) {
			below = append(below, point)
		}
	}
	filesystems := []fsWatchFilesystem{{mount: top, points: []string{root}}}
	if !recursive {
		return filesystems, nil
	}
	// 얕은 지점부터 보아야 같은 파일시스템의 마운트 중 위쪽을 기준으로 삼는다.
	slices.SortFunc(below, func(first, second string) int {
		return cmp.Or(cmp.Compare(strings.Count(first, "/"), strings.Count(second, "/")), cmp.Compare(first, second))
	})
	index := map[string]int{top.device: 0}
	var skipped []string
	for _, point := range below {
		relative, err := filepath.Rel(root, point)
		if err != nil || excluded(relative) {
			continue
		}
		mount := visible[point]
		at, ok := index[mount.device]
		if !ok {
			index[mount.device] = len(filesystems)
			filesystems = append(filesystems, fsWatchFilesystem{mount: mount, points: []string{point}})
			continue
		}
		base := filesystems[at].mount
		if !fsWatchPathUnder(base.source, mount.source) || !fsWatchPathUnder(root, filepath.Join(base.point, strings.TrimPrefix(mount.source, base.source))) {
			skipped = append(skipped, point)
			continue
		}
		filesystems[at].points = append(filesystems[at].points, point)
	}
	return filesystems, skipped
}

// fsWatchPathUnder는 path가 parent이거나 그 아래에 있는지 본다.
func fsWatchPathUnder(parent, path string) bool {
	return path == parent || parent == string(filepath.Separator) || strings.HasPrefix(path, parent+string(filepath.Separator))
}

// fsWatchOutermostPaths는 다른 경로 아래에 있는 경로를 뺀다. /sys를 건너뛰면 /sys/kernel/debug는 따로 알릴 필요가 없다.
func fsWatchOutermostPaths(paths []string) []string {
	slices.Sort(paths)
	var outermost []string
	for _, path := range paths {
		if len(outermost) == 0 || !fsWatchPathUnder(outermost[len(outermost)-1], path) {
			outermost = append(outermost, path)
		}
	}
	return outermost
}

// fsWatchShortList는 앞의 몇 개만 보이고 나머지는 개수로 줄인다.
func fsWatchShortList(paths []string) string {
	if len(paths) <= fsWatchSkippedShown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s … (+%d)", strings.Join(paths[:fsWatchSkippedShown], ", "), len(paths)-fsWatchSkippedShown)
}

// fsWatchUnescapeMount는 mountinfo가 공백, tab, 줄바꿈과 backslash를 바꿔 쓴 \ooo 8진수를 되돌린다.
func fsWatchUnescapeMount(text string) string {
	if !strings.Contains(text, `\`) {
		return text
	}
	var builder strings.Builder
	for index := 0; index < len(text); index++ {
		if text[index] == '\\' && index+3 < len(text) {
			if value, err := strconv.ParseUint(text[index+1:index+4], 8, 8); err == nil {
				builder.WriteByte(byte(value))
				index += 3
				continue
			}
		}
		builder.WriteByte(text[index])
	}
	return builder.String()
}

// fsWatchFanotifyHandleKey는 handle_type과 handle을 이벤트 record와 같은 배치로 잇는다.
func fsWatchFanotifyHandleKey(handle unix.FileHandle) string {
	key := binary.NativeEndian.AppendUint32(nil, uint32(handle.Type()))
	return string(append(key, handle.Bytes()...))
}

// fsWatchOpenHandle은 디렉터리 file handle을 지금 경로로 바꾼다. 지워진 디렉터리는 ESTALE이 되거나, 아직 열려
// 있으면 경로 뒤에 " (deleted)"가 붙는다. key는 fsid를 뺀 handle_type과 handle이다.
func fsWatchOpenHandle(mountFD int, key string) (string, error) {
	if len(key) < 4 {
		return "", unix.EINVAL
	}
	handle := unix.NewFileHandle(int32(binary.NativeEndian.Uint32([]byte(key[:4]))), []byte(key[4:]))
	fd, err := unix.OpenByHandleAt(mountFD, handle, unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
}

var errFSWatchFanotifyMalformed = errors.New("fanotify: malformed event")

// parseFSWatchFanotify는 read 한 번으로 받은 이벤트를 나눈다. info record는 길이에 padding이 들어 있으므로
// record마다 header의 길이만큼 넘긴다.
func parseFSWatchFanotify(buffer []byte) ([]fsWatchFanotifyEvent, error) {
	var events []fsWatchFanotifyEvent
	for len(buffer) > 0 {
		if len(buffer) < fsWatchFanotifyMetadataSize {
			return nil, errFSWatchFanotifyMalformed
		}
		length := int(binary.NativeEndian.Uint32(buffer[0:4]))
		metadataLength := int(binary.NativeEndian.Uint16(buffer[6:8]))
		if buffer[4] != unix.FANOTIFY_METADATA_VERSION || metadataLength < fsWatchFanotifyMetadataSize || length < metadataLength || length > len(buffer) {
			return nil, errFSWatchFanotifyMalformed
		}
		event := fsWatchFanotifyEvent{mask: binary.NativeEndian.Uint64(buffer[8:16])}
		for info := buffer[metadataLength:length]; len(info) > 0; {
			if len(info) < 4 {
				return nil, errFSWatchFanotifyMalformed
			}
			kind, size := info[0], int(binary.NativeEndian.Uint16(info[2:4]))
			if size < 4 || size > len(info) {
				return nil, errFSWatchFanotifyMalformed
			}
			record := info[:size]
			info = info[size:]
			if kind != unix.FAN_EVENT_INFO_TYPE_DFID_NAME && kind != unix.FAN_EVENT_INFO_TYPE_FID {
				continue
			}
			if size < fsWatchFanotifyHandleOffset+4 {
				return nil, errFSWatchFanotifyMalformed
			}
			end := fsWatchFanotifyHandleOffset + 4 + int(binary.NativeEndian.Uint32(record[12:16]))
			if end > size {
				return nil, errFSWatchFanotifyMalformed
			}
			// 마운트마다 파일시스템이 다르므로 handle 앞에 fsid를 붙여야 서로 겹치지 않는다.
			key := string(record[fsWatchFanotifyFSIDOffset:fsWatchFanotifyFSIDOffset+fsWatchFanotifyFSIDSize]) + string(record[fsWatchFanotifyHandleOffset:end])
			if kind == unix.FAN_EVENT_INFO_TYPE_FID {
				event.target = key
				continue
			}
			event.parent = key
			event.name, _, _ = strings.Cut(string(record[end:]), "\x00")
		}
		events = append(events, event)
		buffer = buffer[length:]
	}
	return events, nil
}

// handle은 이벤트 하나의 경로를 찾아 알린다. 부모 디렉터리가 이미 지워져 경로를 모르면 기다렸다가, 그 디렉터리가
// 지워지거나 옮겨졌다는 이벤트가 오면 그 경로로 잇는다. rm -rf는 안쪽부터 지우므로 맨 위 디렉터리의 삭제
// 이벤트에서 기다리던 이벤트가 모두 이어진다.
func (watch *fsWatchFanotify) handle(event fsWatchFanotifyEvent) error {
	if event.mask&unix.FAN_Q_OVERFLOW != 0 {
		return errors.New(T("watchfs.overflow"))
	}
	if event.parent == "" || event.name == "" {
		return nil
	}
	watch.seq++
	isDir := event.mask&unix.FAN_ONDIR != 0
	// 옮긴 디렉터리 아래의 경로는 기억한 경로와 달라진다. 디렉터리 이름 변경은 드물어서 모두 비운다.
	if isDir && event.mask&(unix.FAN_MOVED_FROM|unix.FAN_MOVED_TO) != 0 {
		clear(watch.paths)
	}
	directory, ok := watch.resolve(event.parent)
	if !ok {
		watch.wait(event)
		return nil
	}
	var resolved []fsWatchFanotifyResolved
	if fsWatchFanotifyLinksTarget(event) && len(watch.waiting[event.target]) > 0 {
		watch.claim(event.target, filepath.Join(directory, event.name), watch.seq, &resolved)
	}
	slices.SortFunc(resolved, func(first, second fsWatchFanotifyResolved) int { return cmp.Compare(first.seq, second.seq) })
	for _, item := range resolved {
		if err := watch.report(item.directory, item.event); err != nil {
			return err
		}
	}
	if watch.waitingCount == 0 {
		clear(watch.stale)
	}
	return watch.report(directory, event)
}

// resolve는 디렉터리 handle의 지금 경로를 찾는다. 지워진 디렉터리는 옮긴 뒤 지웠을 수도 있으므로 " (deleted)"를
// 떼지 않고, 삭제 이벤트로 경로를 잇도록 찾지 못한 것으로 둔다.
func (watch *fsWatchFanotify) resolve(key string) (string, bool) {
	if path, ok := watch.paths[key]; ok {
		return path, true
	}
	if watch.stale[key] {
		return "", false
	}
	path, err := watch.lookup(key)
	if err != nil || strings.HasSuffix(path, " (deleted)") {
		watch.stale[key] = true
		return "", false
	}
	if len(watch.paths) >= fsWatchFanotifyPathEntries {
		clear(watch.paths)
	}
	watch.paths[key] = path
	return path, true
}

// wait는 경로를 모르는 이벤트를 부모 디렉터리 handle별로 둔다. 한도를 넘으면 기다리던 이벤트를 모두 버리고 알린다.
// 이어 줄 삭제 이벤트를 잃은 이벤트가 계속 쌓이지 않게 한다.
func (watch *fsWatchFanotify) wait(event fsWatchFanotifyEvent) {
	if watch.waitingCount >= fsWatchFanotifyWaitingLimit {
		fmt.Fprintln(fsWatchNotice, T("watchfs.fanotify.dropped", watch.waitingCount))
		clear(watch.waiting)
		clear(watch.stale)
		watch.waitingCount = 0
	}
	watch.waiting[event.parent] = append(watch.waiting[event.parent], fsWatchFanotifyWaiting{seq: watch.seq, event: event})
	watch.waitingCount++
}

// claim은 target 디렉터리에서 before보다 먼저 일어나 기다리던 이벤트에 directory 아래 경로를 붙인다. 그 이벤트가
// 지워지거나 옮겨진 디렉터리이면 그 안에서 기다리던 이벤트도 이어서 붙인다. before 뒤의 이벤트는 디렉터리를
// 옮긴 뒤에 일어났으므로 다른 경로에 붙는다.
func (watch *fsWatchFanotify) claim(target, directory string, before uint64, resolved *[]fsWatchFanotifyResolved) {
	list := watch.waiting[target]
	count := 0
	for count < len(list) && list[count].seq < before {
		count++
	}
	if count == 0 {
		return
	}
	if count == len(list) {
		delete(watch.waiting, target)
	} else {
		watch.waiting[target] = list[count:]
	}
	watch.waitingCount -= count
	for _, item := range list[:count] {
		*resolved = append(*resolved, fsWatchFanotifyResolved{seq: item.seq, directory: directory, event: item.event})
		if fsWatchFanotifyLinksTarget(item.event) {
			watch.claim(item.event.target, filepath.Join(directory, item.event.name), item.seq, resolved)
		}
	}
}

// fsWatchFanotifyLinksTarget은 이벤트가 디렉터리의 이전 경로를 알려 주는지 본다. 지우거나 옮긴 디렉터리의 handle은
// 더 이상 그 경로로 열리지 않는다.
func fsWatchFanotifyLinksTarget(event fsWatchFanotifyEvent) bool {
	return event.mask&unix.FAN_ONDIR != 0 && event.target != "" && event.mask&(unix.FAN_DELETE|unix.FAN_MOVED_FROM) != 0
}

// report는 경로를 찾은 이벤트를 감시 범위로 거른 뒤 알린다. 이벤트 대부분은 감시 루트 밖에서 오므로 경로를
// 합치기 전에 디렉터리 접두사로 먼저 거른다. 루트의 부모 디렉터리는 루트 자신의 삭제와 이동을 알린다.
func (watch *fsWatchFanotify) report(directory string, event fsWatchFanotifyEvent) error {
	root := watch.options.root
	if directory != root && directory != watch.rootParent && !strings.HasPrefix(directory, watch.rootPrefix) {
		return nil
	}
	filename := filepath.Join(directory, event.name)
	if filename == root {
		if event.mask&(unix.FAN_DELETE|unix.FAN_MOVED_FROM) != 0 {
			return errors.New(T("watchfs.root_removed", root))
		}
		return nil
	}
	relative, err := filepath.Rel(root, filename)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	if !watch.options.recursive && strings.Contains(relative, string(filepath.Separator)) {
		return nil
	}
	if fsWatchUnderExcluded(watch.options.exclude, watch.excludedDirectories, relative) || watch.ignored(filename, nil) {
		return nil
	}
	isDir := event.mask&unix.FAN_ONDIR != 0
	exists, symlink := false, false
	if info, err := os.Lstat(filename); err == nil {
		exists, symlink = true, info.Mode()&os.ModeSymlink != 0
	}
	for _, name := range fsWatchFanotifyOperations(event.mask, exists, isDir) {
		if symlink && name != "create" {
			continue
		}
		if !watch.emit(fsWatchEvent{Time: time.Now().UTC(), Event: name, Path: filepath.ToSlash(relative), IsDir: isDir}) {
			return fs.ErrClosed
		}
	}
	if event.mask&unix.FAN_MOVED_TO != 0 && isDir && exists && watch.options.recursive {
		return watch.announceTree(filename)
	}
	return nil
}

// fsWatchFanotifyOperations는 mask를 순서 있는 edc event로 바꾼다. 커널은 아직 읽지 않은 같은 경로의 이벤트를 한
// mask로 합치고 순서를 남기지 않는다. 지금 경로가 있고 mask에 생성이 있으면 마지막 변경이 생성이므로 삭제를
// 먼저 알리고, 그렇지 않으면 생성과 수정 뒤에 삭제를 알린다.
func fsWatchFanotifyOperations(mask uint64, exists, isDir bool) []string {
	var removed, created, modified []string
	if mask&unix.FAN_MOVED_FROM != 0 {
		removed = append(removed, "rename")
	}
	if mask&unix.FAN_DELETE != 0 {
		removed = append(removed, "remove")
	}
	if mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO) != 0 {
		created = []string{"create"}
	}
	if mask&unix.FAN_MODIFY != 0 && !isDir {
		modified = []string{"modify"}
	}
	if exists && len(created) > 0 {
		return slices.Concat(removed, created, modified)
	}
	return slices.Concat(created, modified, removed)
}

// announceTree는 옮겨 들어온 디렉터리의 하위 항목을 create로 알린다. fanotify는 그 디렉터리 하나만 보낸다.
func (watch *fsWatchFanotify) announceTree(directory string) error {
	return filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if filename == directory {
			return nil
		}
		if watch.ignored(filename, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(watch.options.root, filename)
		if err != nil {
			return err
		}
		if !watch.emit(fsWatchEvent{Time: time.Now().UTC(), Event: "create", Path: filepath.ToSlash(relative), IsDir: entry.IsDir()}) {
			return fs.ErrClosed
		}
		return nil
	})
}
