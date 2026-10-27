package edc

import (
	"fmt"
	"sync"
)

// fsWatchQueueLimit는 출력이 이벤트보다 느릴 때 메모리에 둘 이벤트 수다. 측정에서 이벤트 하나가 경로를 포함해 약 160 byte를
// 차지해 다 차면 큐만 약 40MB다. 커널 큐는 16384건(fs.fanotify.max_queued_events)이라 터미널이 몇 초만 밀려도 넘치고,
// 넘치면 그동안의 변경을 잃으므로 그 앞에서 밀린 이벤트를 받아 둔다.
const fsWatchQueueLimit = 1 << 18

// fsWatchQueueResume는 이벤트를 버리기 시작한 뒤 다시 받기 시작하는 큐 길이다. 가득 찬 채로 하나 비면 하나 받는 식으로
// 오가면 안내가 이벤트마다 나오므로, 절반까지 빠질 때까지 버린다.
const fsWatchQueueResume = fsWatchQueueLimit / 2

// fsWatchQueueHandoff는 감시 루프에 넘기는 채널의 크기다. 큐 goroutine과 감시 루프가 이벤트마다 서로 깨우지 않게 한다.
const fsWatchQueueHandoff = 256

// fsWatchBuffered는 source의 이벤트를 limit까지 메모리에 모아 두는 source를 돌려준다. 감시 루프가 출력에 막혀 있는 동안에도
// 드라이버에서 계속 받아 커널 큐가 넘치지 않게 한다. 다 차면 새 이벤트를 버리고, 처음 버릴 때와 resume까지 빠져 다시 받기
// 시작할 때 각각 한 번 알린다. 오류 채널과 driver는 그대로 넘긴다.
func fsWatchBuffered(source fsWatchSource, limit, resume int) fsWatchSource {
	out := make(chan fsWatchEvent, fsWatchQueueHandoff)
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		defer close(out)
		var queue []fsWatchEvent
		dropped, dropping := 0, false
		// 버리는 중에 감시가 끝나도 버린 수는 알린다.
		defer func() {
			if dropped > 0 {
				fmt.Fprintln(fsWatchNotice, T("watchfs.queue_dropped", dropped))
			}
		}()
		pop := func() {
			// 버린 뒤 이어서 잡은 메모리를 놓게 비운 칸은 지우고, 다 비면 배열도 놓는다.
			queue[0] = fsWatchEvent{}
			queue = queue[1:]
			if len(queue) == 0 {
				queue = nil
			}
			if dropping && len(queue) <= resume {
				dropping = false
				fmt.Fprintln(fsWatchNotice, T("watchfs.queue_dropped", dropped))
				dropped = 0
			}
		}
		in := source.events
		for {
			// 감시 루프가 받을 수 있으면 먼저 넘긴다. 큐에는 감시 루프가 막혀 있는 동안의 이벤트만 남는다.
			for len(queue) > 0 {
				select {
				case out <- queue[0]:
					pop()
					continue
				default:
				}
				break
			}
			// 소스가 끝나고 큐도 비었으면 끝낸다. 여기서 확인하지 않으면 아래 select에서 close만 기다린다.
			if in == nil && len(queue) == 0 {
				break
			}
			var send chan<- fsWatchEvent
			var first fsWatchEvent
			if len(queue) > 0 {
				send, first = out, queue[0]
			}
			select {
			case <-done:
				return
			case event, ok := <-in:
				if !ok {
					in = nil
					continue
				}
				if dropping || len(queue) >= limit {
					if !dropping {
						dropping = true
						fmt.Fprintln(fsWatchNotice, T("watchfs.queue_full", limit))
					}
					dropped++
					continue
				}
				queue = append(queue, event)
			case send <- first:
				pop()
			}
		}
	}()
	var once sync.Once
	return fsWatchSource{events: out, errors: source.errors, driver: source.driver, driverReason: source.driverReason, close: func() {
		once.Do(func() {
			source.close()
			close(done)
			<-finished
		})
	}}
}
