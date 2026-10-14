//go:build linux

package edc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

func hostDuSources() (duSources, bool) {
	return duSources{
		mountinfo: func() (string, error) {
			data, err := os.ReadFile("/proc/self/mountinfo")
			return string(data), err
		},
		statfs: duStatfs,
		deletedOpen: func() (map[uint64]*deletedOpenSummary, bool) {
			return scanDeletedOpen("/proc", duDeletedOpenStat, infoDeletedOpenFDLimit), duProcPartial("/proc/1/fd")
		},
	}, true
}

// duProcPartial은 다른 process의 fd link를 읽을 수 없는지 본다. scanDeletedOpen은 파일을 찾은 장치에만 Partial을
// 적으므로, 하나도 찾지 못했을 때도 값이 하한인지 알려고 PID 1의 link를 읽어 본다. CAP_SYS_PTRACE가 없으면
// 목록은 읽혀도 link는 EACCES이므로 목록이 아니라 link를 읽는다.
func duProcPartial(fdDir string) bool {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		_, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err == nil {
			return false
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// duStatfs의 Used는 df의 Used와 같은 값이다. root 예약 block은 사용 중으로 세지 않는다.
func duStatfs(path string) (duUsage, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return duUsage{}, false
	}
	size := uint64(stat.Frsize)
	if size == 0 {
		size = uint64(stat.Bsize)
	}
	return duUsage{Size: stat.Blocks * size, Used: (stat.Blocks - stat.Bfree) * size}, true
}

// duDeletedOpenStat은 info와 달리 파일 길이가 아니라 할당된 block을 크기로 준다. df의 사용량, du의 합과
// 같은 단위여야 차이를 맞춰 볼 수 있다.
func duDeletedOpenStat(path string) (deletedOpenStat, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return deletedOpenStat{}, err
	}
	return deletedOpenStat{
		Dev: stat.Dev, Inode: stat.Ino, Size: uint64(stat.Blocks) * 512, Links: uint64(stat.Nlink),
		Regular: stat.Mode&unix.S_IFMT == unix.S_IFREG,
	}, nil
}

type duInode struct{ dev, ino uint64 }

type duDir struct {
	node *duNode
	path string
}

type duWalker struct {
	ctx         context.Context
	options     duOptions
	sources     duSources
	rootDev     uint64
	mounts      []diskMount
	mountPoints map[string]diskMount
	files       duFiles
	directories *atomic.Int64
	stopped     atomic.Bool

	mu              sync.Mutex
	seen            map[duInode]struct{}
	skipped         []duMount
	unreadable      []string
	unreadableCount int64
}

func duWalk(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
	started := time.Now()
	var stat unix.Stat_t
	if err := unix.Stat(options.root, &stat); err != nil {
		return duScan{}, err
	}
	own := uint64(stat.Blocks) * 512
	root := &duNode{name: options.root, dir: stat.Mode&unix.S_IFMT == unix.S_IFDIR, bytes: own, own: own, entries: 1}
	if !root.dir {
		duFinish(root, nil)
		return duScan{root: root, elapsed: time.Since(started)}, nil
	}
	walker := &duWalker{
		ctx: ctx, options: options, sources: sources, rootDev: stat.Dev,
		mountPoints: map[string]diskMount{}, files: duFiles{limit: max(options.rows, options.keepFiles)}, seen: map[duInode]struct{}{},
		directories: options.progress,
	}
	if walker.directories == nil {
		walker.directories = new(atomic.Int64)
	}
	var warnings []string
	text, err := sources.mountinfo()
	walker.mounts = parseMountInfo(text)
	if !options.crossMounts {
		// 장치 번호만 보면 같은 파일 시스템을 다시 붙인 bind mount를 지나쳐 같은 파일을 두 번 센다.
		if err != nil {
			warnings = append(warnings, T("du.warning.no_mountinfo", err.Error()))
		}
		for _, mount := range walker.mounts {
			if mount.Point != options.root && pathUnder(mount.Point, options.root) && !duSelfBind(walker.mounts, mount) {
				walker.mountPoints[mount.Point] = mount
			}
		}
	}
	start := duDir{node: root, path: options.root}
	if options.workers <= 1 {
		walker.sequential(start)
	} else {
		walker.parallel(start, options.workers)
	}
	duFinish(root, walker.files.heap)
	sort.Slice(walker.skipped, func(i, j int) bool { return walker.skipped[i].Path < walker.skipped[j].Path })
	sort.Strings(walker.unreadable)
	scan := duScan{
		root: root, directories: walker.directories.Load(), mounts: walker.skipped, unreadable: walker.unreadable,
		unreadableCount: walker.unreadableCount, partial: walker.stopped.Load(), warnings: warnings,
	}
	duReadFilesystem(&scan, options, sources, walker.mounts, stat.Dev)
	scan.elapsed = time.Since(started)
	return scan, nil
}

// duReadFilesystem은 경로가 속한 파일 시스템의 크기, 사용량, 지워졌지만 열린 파일을 채운다. 사용량과 센 크기의
// 차이는 경로가 파일 시스템의 root를 붙인 마운트 지점이고, 모든 디렉터리와 fd를 읽었을 때만 계산한다.
func duReadFilesystem(scan *duScan, options duOptions, sources duSources, mounts []diskMount, dev uint64) {
	mount, found := findDiskMount(mounts, options.root)
	usage, ok := sources.statfs(options.root)
	if !found || !ok {
		return
	}
	scan.filesystem = duFilesystem{
		Mount: mount.Point, FSType: mount.FSType, Source: mount.Source, Root: mount.Root, SizeBytes: usage.Size, UsedBytes: usage.Used,
	}
	summaries, partial := sources.deletedOpen()
	scan.deleted.Partial = partial
	if summary := summaries[dev]; summary != nil {
		scan.deleted.Bytes, scan.deleted.Files = summary.Bytes, summary.Files
		scan.deleted.Partial = partial || summary.Partial
		for _, file := range summary.Top {
			scan.deleted.Top = append(scan.deleted.Top, duDeletedFile{Path: file.Path, Bytes: file.Size, PID: file.PID, Comm: file.Comm})
		}
	}
	// overlay의 statfs는 upper 파일 시스템 전체를 답하고, Root가 /가 아닌 마운트는 파일 시스템의 일부만 붙인 것이다.
	sameDevice := mount.Major == int(unix.Major(dev)) && mount.Minor == int(unix.Minor(dev))
	scan.filesystem.Whole = mount.Point == options.root && mount.Root == "/" && mount.FSType != "overlay" && sameDevice &&
		!options.crossMounts && !scan.partial && scan.unreadableCount == 0 && !scan.deleted.Partial
	if scan.filesystem.Whole {
		if accounted := scan.root.total + scan.deleted.Bytes; usage.Used > accounted {
			scan.notFound = usage.Used - accounted
		}
	}
}

// sequential은 worker가 하나일 때 goroutine 없이 탐색한다. 디렉터리마다 다른 thread로 차례를 넘기면
// futex 호출이 디렉터리 수만큼 늘어 du보다 느려졌다.
func (walker *duWalker) sequential(start duDir) {
	stack := []duDir{start}
	for len(stack) > 0 {
		if walker.ctx.Err() != nil {
			walker.stopped.Store(true)
			return
		}
		dir := stack[len(stack)-1]
		stack = append(stack[:len(stack)-1], walker.scan(dir)...)
	}
}

func (walker *duWalker) parallel(start duDir, workers int) {
	queue := newDuQueue()
	queue.push([]duDir{start})
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				dir, ok := queue.pop()
				if !ok {
					return
				}
				// 시간이 다 되면 남은 디렉터리는 읽지 않고 queue에서 비우기만 한다.
				if walker.ctx.Err() != nil {
					walker.stopped.Store(true)
				} else {
					queue.push(walker.scan(dir))
				}
				queue.done()
			}
		}()
	}
	group.Wait()
}

// scan은 디렉터리 하나를 읽고 그 안의 하위 디렉터리를 돌려준다. 이름은 디렉터리 fd를 기준으로 stat한다.
// 항목마다 전체 경로를 다시 해석하면 CPU 시간이 약 25% 늘었다.
func (walker *duWalker) scan(dir duDir) []duDir {
	fd, err := unix.Open(dir.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		dir.node.unreadable = walker.unreadableDir(dir.path, err)
		return nil
	}
	file := os.NewFile(uintptr(fd), dir.path)
	defer file.Close()
	// 목록을 끝까지 읽지 못해도 읽은 이름은 센다. 디렉터리 하나는 한 번만 읽지 못한 것으로 센다.
	names, readErr := file.Readdirnames(-1)
	walker.directories.Add(1)
	var subdirs []duDir
	for _, name := range names {
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			// 읽는 사이에 지워진 항목은 세지 않는다.
			if readErr == nil && !errors.Is(err, unix.ENOENT) {
				readErr = err
			}
			continue
		}
		bytes := uint64(stat.Blocks) * 512
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			path := duJoin(dir.path, name)
			if walker.boundary(path, stat.Dev) {
				continue
			}
			child := &duNode{name: name, parent: dir.node, dir: true, bytes: bytes, own: bytes, entries: 1}
			dir.node.children = append(dir.node.children, child)
			subdirs = append(subdirs, duDir{node: child, path: path})
			continue
		}
		if stat.Nlink > 1 && walker.counted(stat.Dev, stat.Ino) {
			continue
		}
		dir.node.bytes += bytes
		dir.node.entries++
		walker.files.offer(dir.node, name, bytes)
	}
	if readErr != nil {
		dir.node.unreadable = walker.unreadableDir(dir.path, readErr)
	}
	return subdirs
}

// duSelfBind는 디렉터리를 자기 자신 위에 bind mount했는지 본다. systemd는 /run/credentials처럼 마운트 옵션을
// 바꾸려고 이렇게 붙인다. 이 파일은 다른 곳에 없으므로 건너뛰면 어디에서도 세지 않는다.
func duSelfBind(mounts []diskMount, mount diskMount) bool {
	for _, base := range mounts {
		if base.Major == mount.Major && base.Minor == mount.Minor && base.Root == "/" && base.Point != mount.Point &&
			filepath.Join(base.Point, mount.Root) == mount.Point {
			return true
		}
	}
	return false
}

func duJoin(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// boundary는 path가 다른 마운트라 세지 않을 디렉터리인지 본다. 건너뛴 지점은 따로 모은다.
func (walker *duWalker) boundary(path string, dev uint64) bool {
	if walker.options.crossMounts {
		return false
	}
	mount, listed := walker.mountPoints[path]
	if !listed && dev == walker.rootDev {
		return false
	}
	if !listed {
		mount, _ = findDiskMount(walker.mounts, path)
	}
	skipped := duMount{Path: path, FSType: mount.FSType}
	// bind mount는 밑의 파일 시스템과 장치 번호가 같고, overlay의 statfs는 upper 파일 시스템 전체를 답한다.
	// 마운트 표에 없는데 장치 번호만 바뀌는 btrfs subvolume도 statfs가 파일 시스템 전체를 답한다.
	rootMajor, rootMinor := int(unix.Major(walker.rootDev)), int(unix.Minor(walker.rootDev))
	skipped.Shared = !listed || mount.FSType == "overlay" || (mount.Major == rootMajor && mount.Minor == rootMinor)
	if !skipped.Shared {
		usage, ok := walker.sources.statfs(path)
		skipped.UsedBytes, skipped.Shared = usage.Used, !ok
	}
	if skipped.FSType == "" {
		skipped.FSType = "-"
	}
	walker.mu.Lock()
	walker.skipped = append(walker.skipped, skipped)
	walker.mu.Unlock()
	return true
}

// counted는 hard link가 여럿인 파일을 처음 볼 때만 false를 돌려준다. link 수가 1인 파일은 표에 넣지 않는다.
func (walker *duWalker) counted(dev, ino uint64) bool {
	key := duInode{dev: dev, ino: ino}
	walker.mu.Lock()
	defer walker.mu.Unlock()
	if _, ok := walker.seen[key]; ok {
		return true
	}
	walker.seen[key] = struct{}{}
	return false
}

// unreadableDir는 path를 읽지 못한 디렉터리로 세고, 센 경우에 true를 돌려준다.
func (walker *duWalker) unreadableDir(path string, err error) bool {
	// 읽는 사이에 지워졌거나 디렉터리가 아닌 것으로 바뀐 항목은 읽지 못한 것이 아니다.
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return false
	}
	walker.mu.Lock()
	defer walker.mu.Unlock()
	walker.unreadableCount++
	if len(walker.unreadable) < duUnreadableKeep {
		walker.unreadable = append(walker.unreadable, path)
	}
	return true
}

// duQueue는 worker가 함께 쓰는 작업 목록이다. pending은 queue에 있거나 읽는 중인 디렉터리 수다.
// pending이 0이 되면 더 나올 디렉터리가 없으므로 모든 worker가 끝난다.
type duQueue struct {
	mu      sync.Mutex
	ready   *sync.Cond
	items   []duDir
	pending int
}

func newDuQueue() *duQueue {
	queue := &duQueue{}
	queue.ready = sync.NewCond(&queue.mu)
	return queue
}

func (queue *duQueue) push(dirs []duDir) {
	if len(dirs) == 0 {
		return
	}
	queue.mu.Lock()
	queue.items = append(queue.items, dirs...)
	queue.pending += len(dirs)
	queue.mu.Unlock()
	if len(dirs) == 1 {
		queue.ready.Signal()
	} else {
		queue.ready.Broadcast()
	}
}

// pop은 나중에 넣은 디렉터리부터 꺼낸다. 깊이 우선으로 읽으면 queue에 쌓이는 경로 수가 적다.
func (queue *duQueue) pop() (duDir, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for len(queue.items) == 0 && queue.pending > 0 {
		queue.ready.Wait()
	}
	if len(queue.items) == 0 {
		return duDir{}, false
	}
	last := len(queue.items) - 1
	dir := queue.items[last]
	queue.items = queue.items[:last]
	return dir, true
}

func (queue *duQueue) done() {
	queue.mu.Lock()
	queue.pending--
	finished := queue.pending == 0
	queue.mu.Unlock()
	if finished {
		queue.ready.Broadcast()
	}
}
