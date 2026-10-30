//go:build darwin

package edc

import (
	"context"
	"encoding/binary"
	"sort"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	duDarwinFDReserveDivisor = 4
	duDefaultWorkers         = 4
	duDarwinParentFDLimit    = 32
	duDarwinBufferSize       = 64 << 10
	duDarwinDirectory        = 2
	duDarwinDirAllocSize     = 0x8
	duDarwinDirMountStatus   = 0x4
	duDarwinRecordSize       = 68
)

var duDarwinAttrs = unix.Attrlist{
	Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
	Commonattr: unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_ERROR | unix.ATTR_CMN_NAME |
		unix.ATTR_CMN_DEVID | unix.ATTR_CMN_OBJTYPE | unix.ATTR_CMN_FLAGS | unix.ATTR_CMN_FILEID,
	Dirattr:  duDarwinDirMountStatus | duDarwinDirAllocSize,
	Fileattr: unix.ATTR_FILE_LINKCOUNT | unix.ATTR_FILE_ALLOCSIZE,
}

func hostDuSources() (duSources, bool) {
	return duSources{statfs: duStatfs}, true
}

func duStatfs(path string) (duUsage, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return duUsage{}, false
	}
	return duUsage{Size: stat.Blocks * uint64(stat.Bsize), Used: (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)}, true
}

func duDarwinMounts() ([]diskMount, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	stats := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	if n >= len(stats) {
		return nil, unix.EOVERFLOW
	}
	mounts := make([]diskMount, 0, n)
	for _, stat := range stats[:n] {
		mounts = append(mounts, diskMount{Point: unix.ByteSliceToString(stat.Mntonname[:]),
			FSType: unix.ByteSliceToString(stat.Fstypename[:]), Source: unix.ByteSliceToString(stat.Mntfromname[:]), Root: "/"})
	}
	return mounts, nil
}

// duHostMounts는 MNT_NOWAIT로 읽으므로 응답하지 않는 파일 시스템을 기다리지 않는다.
func duHostMounts() ([]diskMount, error) { return duDarwinMounts() }

// duMeasure는 duWalk에 맡긴다. darwin의 duWalk는 stat, getfsstat, statfs만 읽고 호스트 전체 원천이 없다.
func duMeasure(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
	return duWalk(ctx, options, sources)
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
	mounts, err := duDarwinMounts()
	if err != nil {
		return duScan{}, err
	}
	walker := &duWalker{ctx: ctx, options: options, sources: sources, rootDev: uint64(stat.Dev), mounts: mounts,
		mountPoints: map[string]diskMount{}, files: duFiles{limit: max(options.rows, options.keepFiles)},
		seen: map[duInode]*duLink{}, directories: options.progress, activity: options.activity}
	if walker.directories == nil {
		walker.directories = new(atomic.Int64)
	}
	if walker.activity == nil {
		walker.activity = new(atomic.Int64)
	}
	for _, mount := range mounts {
		if mount.Point != options.root && pathUnder(mount.Point, options.root) {
			walker.mountPoints[mount.Point] = mount
		}
	}
	start := duDir{node: root, path: options.root}
	if options.workers <= 1 {
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			return duScan{}, err
		}
		parentFDLimit := max(1, min(duDarwinParentFDLimit, int(limit.Cur/duDarwinFDReserveDivisor)))
		walker.walk(func() { walker.sequentialDarwin(start, parentFDLimit) })
	} else {
		walker.walk(func() { walker.parallel(start, options.workers) })
	}
	duFinish(root, walker.files.heap)
	sort.Slice(walker.skipped, func(i, j int) bool { return walker.skipped[i].Path < walker.skipped[j].Path })
	sort.Strings(walker.unreadable)
	scan := duScan{root: root, directories: walker.directories.Load(), mounts: walker.skipped,
		unreadable: walker.unreadable, unreadableCount: walker.unreadableCount, partial: walker.stopped.Load(), walker: walker}
	// 남겨 둔 goroutine이 walker를 읽으므로 화면 갱신에 다시 쓰지 않는다.
	if walker.abandoned {
		scan.walker = nil
		scan.warnings = walker.abandonWarning()
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(options.root, &fs); err == nil {
		scan.filesystem = duFilesystem{Mount: unix.ByteSliceToString(fs.Mntonname[:]), FSType: unix.ByteSliceToString(fs.Fstypename[:]),
			Source: unix.ByteSliceToString(fs.Mntfromname[:]), Root: "/", SizeBytes: fs.Blocks * uint64(fs.Bsize),
			UsedBytes: (fs.Blocks - fs.Bfree) * uint64(fs.Bsize)}
	}
	// APFS 공유 블록과 스냅샷은 탐색 합계로 설명할 수 없어 Whole과 notFound를 계산하지 않는다.
	scan.elapsed = time.Since(started)
	return scan, nil
}

func duReadBuffer() *duBuffer { return &duBuffer{read: make([]byte, duDarwinBufferSize)} }

func duDarwinRead(fd int, buffer []byte) (int, error) {
	// x/sys에는 getattrlistbulk wrapper가 없다. 두 포인터는 syscall 동안만 사용하며 buffer는 비어 있지 않다.
	n, _, errno := syscall.Syscall6(unix.SYS_GETATTRLISTBULK, uintptr(fd), uintptr(unsafe.Pointer(&duDarwinAttrs)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), unix.FSOPT_PACK_INVAL_ATTRS, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

type duDarwinEntry struct {
	name     []byte
	dev      uint64
	ino      uint64
	bytes    uint64
	links    uint32
	dir      bool
	firmlink bool
	stat     bool
}

func duDarwinParse(record []byte) (duDarwinEntry, error) {
	if len(record) < duDarwinRecordSize {
		return duDarwinEntry{}, unix.EIO
	}
	u32 := binary.LittleEndian.Uint32
	u64 := binary.LittleEndian.Uint64
	if errno := u32(record[24:]); errno != 0 {
		return duDarwinEntry{}, syscall.Errno(errno)
	}
	common := u32(record[4:])
	if common&unix.ATTR_CMN_NAME == 0 {
		return duDarwinEntry{}, unix.EIO
	}
	offset := int(int32(u32(record[28:]))) + 28
	length := int(u32(record[32:]))
	if offset < duDarwinRecordSize || length < 2 || offset > len(record)-length || record[offset+length-1] != 0 {
		return duDarwinEntry{}, unix.EIO
	}
	entry := duDarwinEntry{name: record[offset : offset+length-1], dev: uint64(u32(record[36:])),
		ino: u64(record[48:]), dir: u32(record[40:]) == duDarwinDirectory,
		firmlink: u32(record[44:])&unix.SF_FIRMLINK != 0}
	entry.stat = common&duDarwinAttrs.Commonattr != duDarwinAttrs.Commonattr
	if entry.dir {
		entry.bytes = u64(record[60:])
		entry.stat = entry.stat || u32(record[12:])&duDarwinAttrs.Dirattr != duDarwinAttrs.Dirattr || entry.firmlink
	} else {
		entry.links, entry.bytes = u32(record[56:]), u64(record[60:])
		entry.stat = entry.stat || u32(record[16:])&duDarwinAttrs.Fileattr != duDarwinAttrs.Fileattr
	}
	return entry, nil
}

func (walker *duWalker) sequentialDarwin(start duDir, parentFDLimit int) {
	type parentFD struct {
		fd, remaining int
		used          uint64
	}
	parents := make(map[*duNode]parentFD)
	defer func() {
		for _, parent := range parents {
			_ = unix.Close(parent.fd)
		}
	}()
	buffer := duReadBuffer()
	stack := []duDir{start}
	var clock uint64
	for len(stack) > 0 {
		if walker.ctx.Err() != nil {
			walker.stopped.Store(true)
			return
		}
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		clock++
		var fd int
		var err error
		parent, cached := parents[dir.node.parent]
		flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		walker.scanning.Add(1)
		duBeforeRead(dir.path)
		if cached {
			fd, err = unix.Openat(parent.fd, dir.node.name, flags, 0)
			parent.remaining--
			if parent.remaining == 0 {
				_ = unix.Close(parent.fd)
				delete(parents, dir.node.parent)
			} else {
				parent.used = clock
				parents[dir.node.parent] = parent
			}
		} else {
			fd, err = unix.Open(dir.path, flags, 0)
		}
		walker.advance()
		var children []duDir
		if err != nil {
			walker.apply(func() { dir.node.unreadable = walker.unreadableDir(dir.path, err) })
		} else {
			children = walker.scanDarwinFD(dir, buffer, fd)
		}
		walker.scanning.Add(-1)
		if err != nil {
			continue
		}
		if len(children) == 0 {
			_ = unix.Close(fd)
			continue
		}
		// 깊거나 넓은 트리에서도 부모 fd를 모두 보관해 open file 제한에 도달하지 않도록 상한을 둔다.
		if len(parents) == parentFDLimit {
			var oldest *duNode
			var used uint64 = ^uint64(0)
			for node, candidate := range parents {
				if candidate.used < used {
					oldest, used = node, candidate.used
				}
			}
			_ = unix.Close(parents[oldest].fd)
			delete(parents, oldest)
		}
		parents[dir.node] = parentFD{fd: fd, remaining: len(children), used: clock}
		stack = append(stack, children...)
	}
}

func (walker *duWalker) scan(dir duDir, buffer *duBuffer) []duDir {
	walker.scanning.Add(1)
	defer walker.scanning.Add(-1)
	duBeforeRead(dir.path)
	fd, err := unix.Open(dir.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	walker.advance()
	if err != nil {
		walker.apply(func() { dir.node.unreadable = walker.unreadableDir(dir.path, err) })
		return nil
	}
	defer unix.Close(fd)
	return walker.scanDarwinFD(dir, buffer, fd)
}

func (walker *duWalker) scanDarwinFD(dir duDir, buffer *duBuffer, fd int) []duDir {
	walker.directories.Add(1)
	var subdirs []duDir
	for {
		if walker.ctx.Err() != nil {
			walker.stopped.Store(true)
			break
		}
		duBeforeRead(dir.path)
		n, err := duDarwinRead(fd, buffer.read)
		walker.advance()
		if err != nil {
			walker.apply(func() { walker.markUnreadable(dir, err) })
			break
		}
		if n == 0 {
			break
		}
		var batch duDarwinBatch
		buffer.entries, batch = walker.readDarwinBatch(dir, buffer.read, n, fd, buffer.entries[:0])
		applied := walker.apply(func() {
			subdirs = walker.addEntries(dir, buffer.entries, subdirs)
			if batch.broken {
				dir.node.unreadable = walker.unreadableDir(dir.path, unix.EIO)
			} else if batch.failed != nil {
				walker.markUnreadable(dir, batch.failed)
			}
		})
		if !applied || batch.broken {
			break
		}
	}
	return subdirs
}

// duDarwinBatch는 bulk 조회 한 번의 오류다. broken이면 남은 기록을 읽을 수 없고, failed는 항목 하나를 읽지 못한 첫 오류다.
type duDarwinBatch struct {
	broken bool
	failed error
}

// readDarwinBatch는 bulk 조회 한 번에 받은 n개 기록을 entries에 모은다. 트리는 고치지 않는다.
func (walker *duWalker) readDarwinBatch(dir duDir, buffer []byte, n, fd int, entries []duEntry) ([]duEntry, duDarwinBatch) {
	var batch duDarwinBatch
	remaining := buffer
	for range n {
		if len(remaining) < 4 {
			batch.broken = true
			return entries, batch
		}
		length := int(binary.LittleEndian.Uint32(remaining))
		if length < duDarwinRecordSize || length > len(remaining) {
			batch.broken = true
			return entries, batch
		}
		entry, err := duDarwinParse(remaining[:length])
		remaining = remaining[length:]
		if err != nil {
			if batch.failed == nil {
				batch.failed = err
			}
			continue
		}
		if entry.stat {
			// bulk 조회는 firmlink 원본과 일부 파일의 불완전한 속성을 준다. fd 기준 stat으로 대상 값을 읽는다.
			var stat unix.Stat_t
			err := unix.Fstatat(fd, string(entry.name), &stat, unix.AT_SYMLINK_NOFOLLOW)
			walker.advance()
			if err != nil {
				if batch.failed == nil {
					batch.failed = err
				}
				continue
			}
			entry.dev, entry.ino, entry.bytes, entry.links = uint64(stat.Dev), stat.Ino, uint64(stat.Blocks)*512, uint32(stat.Nlink)
			entry.dir = stat.Mode&unix.S_IFMT == unix.S_IFDIR
		}
		item := duEntry{dev: entry.dev, ino: entry.ino, bytes: entry.bytes, links: uint64(entry.links), dir: entry.dir}
		// name은 다음 bulk 조회가 덮어쓰는 buffer를 가리킨다. 쓰는 항목만 복사해 파일마다 할당하지 않는다.
		switch {
		case entry.dir:
			item.name = string(entry.name)
			item.path = duJoin(dir.path, item.name)
			item.skip = walker.mountSkip(item.path)
		case entry.bytes > walker.files.floor.Load():
			item.name = string(entry.name)
		}
		entries = append(entries, item)
	}
	return entries, batch
}

// mountSkip은 path가 건너뛸 마운트 지점이면 그 기록을 돌려준다. statfs가 막힐 수 있어 apply 밖에서 부른다.
func (walker *duWalker) mountSkip(path string) *duMount {
	if walker.options.crossMounts {
		return nil
	}
	mount, listed := walker.mountPoints[path]
	if !listed {
		return nil
	}
	skipped := duMount{Path: path, FSType: mount.FSType, Shared: mount.FSType == "apfs"}
	if !skipped.Shared {
		usage, ok := walker.sources.statfs(path)
		skipped.UsedBytes, skipped.Shared = usage.Used, !ok
	}
	return &skipped
}
