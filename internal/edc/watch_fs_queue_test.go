package edc

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fsWatchTestNotice는 큐 goroutine이 쓰는 안내를 테스트 goroutine에서 읽을 수 있게 잠근다.
type fsWatchTestNotice struct {
	mutex  sync.Mutex
	buffer strings.Builder
}

func (notice *fsWatchTestNotice) Write(data []byte) (int, error) {
	notice.mutex.Lock()
	defer notice.mutex.Unlock()
	return notice.buffer.Write(data)
}

func (notice *fsWatchTestNotice) String() string {
	notice.mutex.Lock()
	defer notice.mutex.Unlock()
	return notice.buffer.String()
}

func (notice *fsWatchTestNotice) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(notice.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("notice %q does not contain %q", notice.String(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fsWatchTestReceive(t *testing.T, events <-chan fsWatchEvent) (fsWatchEvent, bool) {
	t.Helper()
	select {
	case event, ok := <-events:
		return event, ok
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
	}
	return fsWatchEvent{}, false
}

func TestFSWatchBufferedQueueDropsWhenFullAndResumesAfterDraining(t *testing.T) {
	var notice fsWatchTestNotice
	previous := fsWatchNotice.swap(&notice)
	defer fsWatchNotice.swap(previous)
	in := make(chan fsWatchEvent)
	var closeOnce sync.Once
	failures := make(chan error, 1)
	inner := fsWatchSource{events: in, errors: failures, driver: "test", driverReason: "reason", close: func() { closeOnce.Do(func() { close(in) }) }}
	var loss fsWatchLoss
	source := fsWatchBuffered(inner, 4, 2, &loss)
	defer source.close()
	if source.driver != "test" || source.driverReason != "reason" || source.errors != failures {
		t.Fatalf("source = %+v", source)
	}
	// 넘기는 채널이 차고 큐도 찬 뒤의 이벤트는 버린다. 버리기 시작할 때 한 번 알린다.
	kept := fsWatchQueueHandoff + 4
	for index := 0; index < kept+3; index++ {
		in <- fsWatchEvent{Event: "modify", Path: fmt.Sprint(index)}
	}
	notice.wait(t, T("watchfs.queue_full", 4))
	if strings.Count(notice.String(), T("watchfs.queue_full", 4)) != 1 {
		t.Fatalf("queue_full notice repeated: %q", notice.String())
	}
	// 둔 이벤트는 순서대로 나오고, 큐가 절반까지 빠지면 버린 수를 알린다.
	for index := 0; index < kept; index++ {
		event, ok := fsWatchTestReceive(t, source.events)
		if !ok || event.Path != fmt.Sprint(index) {
			t.Fatalf("event %d = %+v %v", index, event, ok)
		}
	}
	notice.wait(t, T("watchfs.queue_dropped", 3))
	if _, dropped, _ := loss.counts(); dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
	// 다시 받기 시작한 뒤의 이벤트는 그대로 지나간다.
	in <- fsWatchEvent{Event: "create", Path: "after"}
	if event, ok := fsWatchTestReceive(t, source.events); !ok || event.Path != "after" {
		t.Fatalf("event after resume = %+v %v", event, ok)
	}
	if strings.Count(notice.String(), T("watchfs.queue_full", 4)) != 1 {
		t.Fatalf("unexpected notice after resume: %q", notice.String())
	}
	// 닫으면 안쪽 source를 닫고 넘기는 채널도 닫힌다.
	source.close()
	if _, ok := fsWatchTestReceive(t, source.events); ok {
		t.Fatal("events still open after close")
	}
}

func TestFSWatchBufferedQueueReportsTheDropCountWhenClosedWhileDropping(t *testing.T) {
	var notice fsWatchTestNotice
	previous := fsWatchNotice.swap(&notice)
	defer fsWatchNotice.swap(previous)
	in := make(chan fsWatchEvent)
	inner := fsWatchSource{events: in, errors: make(chan error, 1), close: func() {}}
	var loss fsWatchLoss
	source := fsWatchBuffered(inner, 2, 1, &loss)
	for index := 0; index < fsWatchQueueHandoff+2+5; index++ {
		in <- fsWatchEvent{Event: "modify", Path: fmt.Sprint(index)}
	}
	notice.wait(t, T("watchfs.queue_full", 2))
	// 버리는 중에 닫혀도 버린 수를 알리고 loss에 더한다. close는 goroutine이 끝난 뒤 돌아오므로 바로 셀 수 있다.
	source.close()
	notice.wait(t, T("watchfs.queue_dropped", 5))
	if _, dropped, _ := loss.counts(); dropped != 5 {
		t.Fatalf("dropped = %d, want 5", dropped)
	}
}

func TestFSWatchBufferedQueueDrainsWhenTheSourceCloses(t *testing.T) {
	var notice fsWatchTestNotice
	previous := fsWatchNotice.swap(&notice)
	defer fsWatchNotice.swap(previous)
	in := make(chan fsWatchEvent)
	inner := fsWatchSource{events: in, errors: make(chan error, 1), close: func() {}}
	source := fsWatchBuffered(inner, 2, 1, nil)
	defer source.close()
	for index := 0; index < fsWatchQueueHandoff+2; index++ {
		in <- fsWatchEvent{Event: "modify", Path: fmt.Sprint(index)}
	}
	// 소스가 끝나도 둔 이벤트를 모두 넘긴 뒤에 채널을 닫는다.
	close(in)
	for index := 0; index < fsWatchQueueHandoff+2; index++ {
		if event, ok := fsWatchTestReceive(t, source.events); !ok || event.Path != fmt.Sprint(index) {
			t.Fatalf("event %d = %+v %v", index, event, ok)
		}
	}
	if _, ok := fsWatchTestReceive(t, source.events); ok {
		t.Fatal("events still open after the source closed")
	}
	if notice.String() != "" {
		t.Fatalf("unexpected notice %q", notice.String())
	}
}
