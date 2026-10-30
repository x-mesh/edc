//go:build linux || darwin

package edc

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// duBeforeRead는 디렉터리를 열거나 목록을 읽기 직전에 부른다. test가 돌아오지 않는 시스템 호출을 이 자리에서 흉내 낸다.
var duBeforeRead = func(string) {}

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
	activity    *atomic.Int64
	stopped     atomic.Bool
	// tree는 탐색 goroutine이 트리를 고치는 구간이다. 시스템 호출은 이 밖에서 부르므로, 시간이 다 되면 walk가 Lock으로
	// 고치는 중인 goroutine만 기다리고 막힌 호출은 기다리지 않는다.
	tree sync.RWMutex
	// abandoned는 시간이 다 된 뒤에도 끝나지 않은 탐색 goroutine을 남겨 두고 돌아왔다는 뜻이다.
	abandoned bool
	// scanning은 읽는 중인 디렉터리 수다. 남겨 둔 goroutine이 몇 디렉터리에서 막혔는지 경고로 알린다.
	scanning atomic.Int64

	mu sync.Mutex
	// seen은 hard link가 여럿인 파일을 어느 디렉터리가 셌는지다. 화면이 디렉터리 하나를 다시 읽을 때 그 디렉터리가 센
	// 파일만 다시 센다.
	seen            map[duInode]*duLink
	skipped         []duMount
	unreadable      []string
	unreadableCount int64
}

// sequential은 worker가 하나일 때 goroutine 없이 탐색한다. 디렉터리마다 다른 thread로 차례를 넘기면
// futex 호출이 디렉터리 수만큼 늘어 du보다 느려졌다.
func (walker *duWalker) sequential(start duDir) {
	buffer := duReadBuffer()
	stack := []duDir{start}
	for len(stack) > 0 {
		if walker.ctx.Err() != nil {
			walker.stopped.Store(true)
			return
		}
		dir := stack[len(stack)-1]
		stack = append(stack[:len(stack)-1], walker.scan(dir, buffer)...)
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
			buffer := duReadBuffer()
			for {
				dir, ok := queue.pop()
				if !ok {
					return
				}
				// 시간이 다 되면 남은 디렉터리는 읽지 않고 queue에서 비우기만 한다.
				if walker.ctx.Err() != nil {
					walker.stopped.Store(true)
				} else {
					queue.push(walker.scan(dir, buffer))
				}
				queue.done()
			}
		}()
	}
	group.Wait()
}

func (walker *duWalker) advance() { walker.activity.Add(1) }

// abandonWarning은 남겨 둔 goroutine이 있을 때의 경고다.
func (walker *duWalker) abandonWarning() []string {
	if !walker.abandoned {
		return nil
	}
	return []string{T("du.warning.abandoned", walker.scanning.Load())}
}

// duAbandonGrace는 시간이 다 된 뒤 탐색 goroutine이 fd를 닫고 끝나기를 기다리는 시간이다. 이 안에 끝나지 않으면 돌아오지
// 않는 시스템 호출에 막힌 것으로 본다.
const duAbandonGrace = time.Second

// walk는 search를 goroutine에서 돌리고, 끝나거나 시간이 다 될 때까지 기다린다. macOS의 TCC 보호 경로는 open이 돌아오지
// 않아서, 끝까지 기다리면 --timeout이 지나도 센 결과를 보여 주지 못한다.
func (walker *duWalker) walk(search func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		search()
	}()
	select {
	case <-done:
		return
	case <-walker.ctx.Done():
	}
	select {
	case <-done:
		return
	case <-time.After(duAbandonGrace):
	}
	// Lock은 트리를 고치는 중인 goroutine만 기다린다. 그 뒤에 깨어난 goroutine은 apply에서 취소된 ctx를 보고 트리를 고치지 않는다.
	walker.tree.Lock()
	select {
	case <-done:
	default:
		walker.stopped.Store(true)
		walker.abandoned = true
	}
	walker.tree.Unlock()
}

// apply는 트리를 고치는 change를 tree.RLock 안에서 부른다. 시간이 다 됐으면 부르지 않고 false를 돌려준다.
func (walker *duWalker) apply(change func()) bool {
	walker.tree.RLock()
	defer walker.tree.RUnlock()
	if walker.ctx.Err() != nil {
		walker.stopped.Store(true)
		return false
	}
	change()
	return true
}

// duEntry는 디렉터리에서 읽은 항목 하나다. 시스템 호출로 모은 뒤 apply 안에서 트리에 넣는다. name은 디렉터리와
// 큰 파일 후보에만 채운다. skip은 건너뛸 마운트 지점이다.
type duEntry struct {
	name     string
	dev, ino uint64
	bytes    uint64
	links    uint64
	dir      bool
	skip     *duMount
}

// addEntries는 apply 안에서 부른다. 새 하위 디렉터리를 subdirs에 붙여 돌려준다.
func (walker *duWalker) addEntries(dir duDir, entries []duEntry, subdirs []duDir) []duDir {
	for _, entry := range entries {
		if entry.dir {
			if entry.skip != nil {
				walker.mu.Lock()
				walker.skipped = append(walker.skipped, *entry.skip)
				walker.mu.Unlock()
				continue
			}
			child := &duNode{name: entry.name, parent: dir.node, dir: true, bytes: entry.bytes, own: entry.bytes, entries: 1}
			dir.node.children = append(dir.node.children, child)
			subdirs = append(subdirs, duDir{node: child, path: duJoin(dir.path, entry.name)})
			continue
		}
		if entry.links > 1 && walker.counted(entry.dev, entry.ino, dir.node) {
			continue
		}
		dir.node.bytes += entry.bytes
		dir.node.entries++
		if entry.name != "" {
			walker.files.offer(dir.node, entry.name, entry.bytes)
		}
	}
	return subdirs
}

// markUnreadable은 apply 안에서 부른다. 디렉터리 하나는 한 번만 읽지 못한 것으로 센다.
func (walker *duWalker) markUnreadable(dir duDir, err error) {
	if !dir.node.unreadable {
		dir.node.unreadable = walker.unreadableDir(dir.path, err)
	}
}

func duJoin(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// duLink는 hard link가 여럿인 파일 하나다. owner가 그 파일을 셌고, others는 세지 않고 지나갔다. owner의 링크가 지워지면
// others 중 하나가 다시 세야 합계가 맞는다.
type duLink struct {
	owner  *duNode
	others []*duNode
}

// duLinkOthers는 세지 않고 지나간 디렉터리를 남기는 상한이다. 한 파일을 수만 곳에 링크한 트리에서 탐색이 느려지지 않게 한다.
const duLinkOthers = 64

// counted는 hard link가 여럿인 파일을 처음 볼 때만 false를 돌려주고 센 디렉터리를 남긴다. 센 디렉터리가 빠져 주인이 없는
// 파일은 다음에 만난 디렉터리가 센다. link 수가 1인 파일은 표에 넣지 않는다.
func (walker *duWalker) counted(dev, ino uint64, node *duNode) bool {
	key := duInode{dev: dev, ino: ino}
	walker.mu.Lock()
	defer walker.mu.Unlock()
	link := walker.seen[key]
	if link == nil {
		walker.seen[key] = &duLink{owner: node}
		return false
	}
	if link.owner == nil {
		link.owner = node
		return false
	}
	if link.owner != node && len(link.others) < duLinkOthers && !slices.Contains(link.others, node) {
		link.others = append(link.others, node)
	}
	return true
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
