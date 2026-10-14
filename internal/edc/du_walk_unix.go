//go:build linux || darwin

package edc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

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

func duJoin(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
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
