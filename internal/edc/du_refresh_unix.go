//go:build linux || darwin

package edc

import (
	"context"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

const duMtimeSlack = 2 * time.Second

// duPoll은 트리의 디렉터리 mtime과 남긴 큰 파일의 크기를 다시 본다. since보다 늦게 바뀐 디렉터리는 바로 아래 항목만 다시
// 읽는다. 디렉터리 mtime은 항목이 생기거나 지워질 때만 바뀌므로, 제자리에서 커지는 파일은 큰 파일 줄로 남긴 것만 잡힌다.
func duPoll(ctx context.Context, walker *duWalker, root *duNode, since time.Time, floor uint64) duRefreshResult {
	result := duRefreshResult{started: time.Now()}
	type item struct {
		node  *duNode
		path  string
		depth int
	}
	var changed []item
	var files []duFileChange
	stack := []item{{root, root.name, 0}}
	for len(stack) > 0 {
		if ctx.Err() != nil {
			result.err = ctx.Err()
			return result
		}
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		var stat unix.Stat_t
		err := unix.Lstat(current.path, &stat)
		if !current.node.dir {
			switch {
			case err != nil || stat.Mode&unix.S_IFMT == unix.S_IFDIR:
				files = append(files, duFileChange{node: current.node, gone: true})
			case uint64(stat.Blocks)*512 != current.node.bytes:
				files = append(files, duFileChange{node: current.node, bytes: uint64(stat.Blocks) * 512})
			}
			continue
		}
		// 지워진 디렉터리는 부모의 mtime이 바뀌어 부모를 다시 읽을 때 빠진다.
		if err != nil {
			continue
		}
		// FAT과 HFS+는 mtime을 1~2초 단위로 적는다. 기준보다 조금 앞선 변경도 다시 읽어야 놓치지 않는다.
		if duStatMtime(&stat).After(since.Add(-duMtimeSlack)) {
			changed = append(changed, current)
		}
		for _, child := range current.node.children {
			stack = append(stack, item{child, duJoin(current.path, child.name), current.depth + 1})
		}
	}
	rescanned := map[*duNode]bool{}
	sort.SliceStable(changed, func(i, j int) bool { return changed[i].depth < changed[j].depth })
	for _, dir := range changed {
		rescan, ok := duRescan(ctx, walker, dir.node, dir.path, false, floor)
		if ctx.Err() != nil {
			result.err = ctx.Err()
			return result
		}
		if ok {
			result.dirs = append(result.dirs, rescan)
			rescanned[dir.node] = true
		}
	}
	// 다시 읽은 디렉터리의 큰 파일은 다시 읽기가 크기를 정한다.
	for _, file := range files {
		if !rescanned[file.node.parent] {
			result.files = append(result.files, file)
		}
	}
	result.usage, result.usageOK = duStatfs(root.name)
	result.duration = time.Since(result.started)
	return result
}

// duRescan은 디렉터리 하나를 다시 읽는다. full이 아니면 이름이 같은 하위 디렉터리는 내려가지 않고, 새로 생긴 하위
// 디렉터리만 끝까지 읽는다. 트리는 고치지 않고 새로 읽은 노드를 돌려준다.
func duRescan(ctx context.Context, walker *duWalker, node *duNode, path string, full bool, floor uint64) (duDirRescan, bool) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return duDirRescan{}, false
	}
	own := uint64(stat.Blocks) * 512
	fresh := &duNode{name: node.name, parent: node.parent, dir: true, bytes: own, own: own, entries: 1}
	walker.ctx = ctx
	walker.stopped.Store(false)
	walker.skipped = nil
	walker.files.reset(duBrowseFiles, floor)
	owned := map[*duNode]bool{node: true}
	if full {
		duSubtree(node, owned)
	}
	walker.forget(owned)
	existing := map[string]bool{}
	oldFiles := map[string]bool{}
	if !full {
		for _, child := range node.children {
			if child.dir {
				existing[child.name] = true
			} else {
				oldFiles[child.name] = true
			}
		}
	}
	before := walker.directories.Load()
	for _, subdir := range walker.scan(duDir{node: fresh, path: path}, duReadBuffer()) {
		if !existing[subdir.node.name] {
			walker.sequential(subdir)
		}
	}
	if walker.stopped.Load() {
		return duDirRescan{}, false
	}
	rescan := duDirRescan{node: node, fresh: fresh, full: full, directories: walker.directories.Load() - before}
	for _, file := range walker.files.heap {
		if file.parent == fresh && !full {
			if !oldFiles[file.name] {
				rescan.newBig = append(rescan.newBig, &duNode{name: file.name, bytes: file.bytes, total: file.bytes, entries: 1, count: 1})
			}
			continue
		}
		file.parent.bytes -= file.bytes
		file.parent.entries--
		file.parent.children = append(file.parent.children, &duNode{name: file.name, parent: file.parent, bytes: file.bytes, total: file.bytes, entries: 1, count: 1})
	}
	for _, child := range fresh.children {
		if child.dir && !existing[child.name] {
			duTotal(child)
		}
	}
	if !full {
		rescan.kept = map[*duNode]uint64{}
		for _, child := range node.children {
			if child.dir {
				continue
			}
			var file unix.Stat_t
			if unix.Lstat(duJoin(path, child.name), &file) != nil || file.Mode&unix.S_IFMT == unix.S_IFDIR {
				continue
			}
			// 다른 디렉터리가 센 hard link는 이 디렉터리의 합에 들지 않았다.
			if file.Nlink > 1 && walker.owner(uint64(file.Dev), file.Ino) != fresh {
				continue
			}
			rescan.kept[child] = uint64(file.Blocks) * 512
		}
	}
	return rescan, true
}

// forget은 nodes가 센 hard link 기록을 지운다. 그 디렉터리를 다시 읽으면 같은 파일을 다시 센다.
func (walker *duWalker) forget(nodes map[*duNode]bool) {
	walker.mu.Lock()
	defer walker.mu.Unlock()
	for key, owner := range walker.seen {
		if nodes[owner] {
			delete(walker.seen, key)
		}
	}
}

// reown은 다시 읽을 때 쓴 노드의 hard link 기록을 실제 트리의 노드로 옮긴다.
func (walker *duWalker) reown(from, to *duNode) {
	walker.mu.Lock()
	defer walker.mu.Unlock()
	for key, owner := range walker.seen {
		if owner == from {
			walker.seen[key] = to
		}
	}
}

func (walker *duWalker) owner(dev, ino uint64) *duNode {
	walker.mu.Lock()
	defer walker.mu.Unlock()
	return walker.seen[duInode{dev: dev, ino: ino}]
}

func (walker *duWalker) floor() uint64 { return walker.files.floor.Load() }

// duRescanFull은 r 키로 고른 디렉터리의 하위 전체를 다시 읽는다. 제자리에서 커진 작은 파일은 이렇게만 잡힌다.
func duRescanFull(ctx context.Context, walker *duWalker, node *duNode, path, root string, floor uint64) duRefreshResult {
	result := duRefreshResult{started: time.Now(), manual: true}
	rescan, ok := duRescan(ctx, walker, node, path, true, floor)
	if ctx.Err() != nil {
		result.err = ctx.Err()
		return result
	}
	if ok {
		result.dirs = append(result.dirs, rescan)
	}
	result.usage, result.usageOK = duStatfs(root)
	result.duration = time.Since(result.started)
	return result
}

func duStatMtime(stat *unix.Stat_t) time.Time { return time.Unix(stat.Mtim.Unix()) }
