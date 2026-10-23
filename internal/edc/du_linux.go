//go:build linux

package edc

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// 12코어 호스트의 /usr(캐시가 채워진 상태)에서 worker 4개가 du보다 3배 빨랐고 순간 사용량은 약 5코어였다.
	// 코어 수를 넘기면 더 빨라지지 않았다. 운영 호스트에서 CPU를 덜 쓰는 쪽을 기본으로 둔다.
	duDefaultWorkers = 4
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
		containerLabels: duHostContainerLabels,
	}, true
}

// duHostMounts는 /proc에서 마운트 표만 읽으므로 응답하지 않는 파일 시스템을 건드리지 않는다.
func duHostMounts() ([]diskMount, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	return parseMountInfo(string(data)), err
}

// duHostContainerLabels는 docker에 container를 묻는다. docker가 없거나 답하지 않으면 이름표 없이 둔다.
func duHostContainerLabels(ctx context.Context) map[string]string {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, duDockerTimeout)
	defer cancel()
	ids, err := commandOutput(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc")
	if err != nil || strings.TrimSpace(ids) == "" {
		return nil
	}
	// ID는 docker ps가 낸 16진수라 option으로 읽힐 일이 없다. 목록을 읽은 뒤 지운 container가 있으면 inspect는
	// 실패하지만 나머지 줄은 낸다.
	inspect, _ := commandOutput(ctx, "docker", append([]string{"inspect", "--type", "container", "--format", duDockerFormat}, strings.Fields(ids)...)...)
	mountinfo, _ := os.ReadFile("/proc/self/mountinfo")
	return duResolveLabelPaths(duContainerLabels(inspect, string(mountinfo)))
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
	// docker 조회는 탐색과 함께 돌려 시간을 더하지 않는다.
	var labels chan map[string]string
	if sources.containerLabels != nil {
		labels = make(chan map[string]string, 1)
		go func() { labels <- sources.containerLabels(ctx) }()
	}
	scan, mounts := duWalkTree(ctx, options, sources, root, stat.Dev)
	duReadFilesystem(&scan, options, sources, mounts, stat.Dev)
	if labels != nil {
		scan.labels = <-labels
	}
	scan.elapsed = time.Since(started)
	return scan, nil
}

// duMeasure는 경로 하나의 사용량과 파일 시스템만 잰다. duWalk와 달리 지워졌지만 열린 파일(/proc 전체)과 docker
// 이름표를 읽지 않으므로 호출자가 경로를 여럿 재도 호스트 전체 조회가 되풀이되지 않는다.
func duMeasure(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
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
	scan, mounts := duWalkTree(ctx, options, sources, root, stat.Dev)
	scan.filesystem, _, _, _ = duFilesystemOf(options, sources, mounts)
	scan.elapsed = time.Since(started)
	return scan, nil
}

// duWalkTree는 디렉터리 root 아래를 세어 duScan을 만든다. 파일 시스템 정보와 지워졌지만 열린 파일은 채우지 않는다.
const duActivityBatch = 256

func duWalkTree(ctx context.Context, options duOptions, sources duSources, root *duNode, dev uint64) (duScan, []diskMount) {
	walker := &duWalker{
		ctx: ctx, options: options, sources: sources, rootDev: dev,
		mountPoints: map[string]diskMount{}, files: duFiles{limit: max(options.rows, options.keepFiles)}, seen: map[duInode]struct{}{},
		directories: options.progress, activity: options.activity,
	}
	if walker.directories == nil {
		walker.directories = new(atomic.Int64)
	}
	if walker.activity == nil {
		walker.activity = new(atomic.Int64)
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
	return scan, walker.mounts
}

// duFilesystemOf는 경로가 속한 마운트를 찾아 크기와 사용량을 읽는다. 마운트를 찾지 못하거나 statfs가 실패하면 ok가 false다.
func duFilesystemOf(options duOptions, sources duSources, mounts []diskMount) (duFilesystem, diskMount, duUsage, bool) {
	mount, found := findDiskMount(mounts, options.root)
	usage, ok := sources.statfs(options.root)
	if !found || !ok {
		return duFilesystem{}, diskMount{}, duUsage{}, false
	}
	return duFilesystem{
		Mount: mount.Point, FSType: mount.FSType, Source: mount.Source, Root: mount.Root, SizeBytes: usage.Size, UsedBytes: usage.Used,
	}, mount, usage, true
}

// duReadFilesystem은 경로가 속한 파일 시스템의 크기, 사용량, 지워졌지만 열린 파일을 채운다. 사용량과 센 크기의
// 차이는 경로가 파일 시스템의 root를 붙인 마운트 지점이고, 모든 디렉터리와 fd를 읽었을 때만 계산한다.
func duReadFilesystem(scan *duScan, options duOptions, sources duSources, mounts []diskMount, dev uint64) {
	filesystem, mount, usage, ok := duFilesystemOf(options, sources, mounts)
	if !ok {
		return
	}
	scan.filesystem = filesystem
	summaries, partial := sources.deletedOpen()
	scan.deletedChecked = true
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

// scan은 디렉터리 하나를 읽고 그 안의 하위 디렉터리를 돌려준다. 이름은 디렉터리 fd를 기준으로 stat한다.
// 항목마다 전체 경로를 다시 해석하면 CPU 시간이 약 25% 늘었다.
func (walker *duWalker) scan(dir duDir, _ []byte) []duDir {
	fd, err := unix.Open(dir.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	walker.advance()
	if err != nil {
		dir.node.unreadable = walker.unreadableDir(dir.path, err)
		return nil
	}
	file := os.NewFile(uintptr(fd), dir.path)
	defer file.Close()
	// 목록을 끝까지 읽지 못해도 읽은 이름은 센다. 디렉터리 하나는 한 번만 읽지 못한 것으로 센다.
	var names []string
	var readErr error
	for {
		batch, err := file.Readdirnames(duActivityBatch * 16)
		walker.advance()
		names = append(names, batch...)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	walker.directories.Add(1)
	var subdirs []duDir
	for index, name := range names {
		// 항목마다 세면 worker들이 같은 counter를 두고 다툰다. 멈춤을 알아보는 데는 묶음 단위로 충분하다.
		if index%duActivityBatch == duActivityBatch-1 {
			walker.advance()
		}
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

func duReadBuffer() []byte { return nil }
