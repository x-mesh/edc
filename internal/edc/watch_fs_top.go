package edc

import (
	"container/heap"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// fsWatchTopShown은 종료 요약에 보이는 디렉터리의 수다. fsWatchTopMinEvents보다 이벤트가 적게 끝난 감시에는 보이지 않는다.
	fsWatchTopShown     = 5
	fsWatchTopMinEvents = 10
	// fsWatchTopMaxDirectories는 동시에 세는 디렉터리의 수다. 이름이 매번 다른 디렉터리가 쌓여도 메모리가 늘지 않게 한다. 넘으면 가장 적게
	// 센 디렉터리를 내보내고 새 디렉터리를 센다.
	fsWatchTopMaxDirectories = 10000
)

// fsWatchTopItem은 디렉터리 하나의 수다. 한도에서 다른 디렉터리를 밀어내고 들어온 항목은 밀려난 항목의 수를 이어받으므로 count가
// 실제 수보다 클 수 있고, 클 수 있는 폭은 inherited까지다. 실제 수는 count-inherited 이상 count 이하다.
type fsWatchTopItem struct {
	directory string
	count     int
	inherited int
	index     int
}

// fsWatchTopHeap은 가장 적게 센 항목이 맨 위에 오는 힙이다.
type fsWatchTopHeap []*fsWatchTopItem

func (heap fsWatchTopHeap) Len() int { return len(heap) }
func (heap fsWatchTopHeap) Less(i, j int) bool {
	if heap[i].count != heap[j].count {
		return heap[i].count < heap[j].count
	}
	return heap[i].directory > heap[j].directory
}
func (heap fsWatchTopHeap) Swap(i, j int) {
	heap[i], heap[j] = heap[j], heap[i]
	heap[i].index, heap[j].index = i, j
}
func (heap *fsWatchTopHeap) Push(item any) {
	entry := item.(*fsWatchTopItem)
	entry.index = len(*heap)
	*heap = append(*heap, entry)
}
func (heap *fsWatchTopHeap) Pop() any {
	old := *heap
	last := old[len(old)-1]
	*heap = old[:len(old)-1]
	return last
}

// fsWatchTop은 감시하는 동안 디렉터리마다 이벤트를 센다. 끝날 때 무엇이 가장 시끄러웠는지 보이려는 것이다. 한 goroutine에서만 쓴다.
// 디렉터리가 한도를 넘기 전에는 정확히 세고, 넘은 뒤에는 Space-Saving 방식으로 많이 나온 디렉터리를 놓치지 않게 센다.
type fsWatchTop struct {
	items map[string]*fsWatchTopItem
	total int
	// heap은 한도에 처음 닿았을 때 만든다. 그 전에는 힙을 유지하는 비용을 내지 않는다.
	heap fsWatchTopHeap
}

func newFSWatchTop() *fsWatchTop { return &fsWatchTop{items: map[string]*fsWatchTopItem{}} }

// add는 감시 루트 기준 경로의 이벤트 하나를 센다.
func (top *fsWatchTop) add(eventPath string) {
	top.total++
	directory := path.Dir(filepath.ToSlash(eventPath))
	if item, seen := top.items[directory]; seen {
		item.count++
		if top.heap != nil {
			heap.Fix(&top.heap, item.index)
		}
		return
	}
	if len(top.items) < fsWatchTopMaxDirectories {
		top.items[directory] = &fsWatchTopItem{directory: directory, count: 1}
		return
	}
	if top.heap == nil {
		top.startEviction()
	}
	// 가장 적게 센 디렉터리를 내보내고 그 수를 이어받는다. 새 디렉터리가 실제로 그보다 적었더라도 최악의 경우를 넘지 않는다.
	oldest := top.heap[0]
	delete(top.items, oldest.directory)
	oldest.directory, oldest.inherited, oldest.count = directory, oldest.count, oldest.count+1
	top.items[directory] = oldest
	heap.Fix(&top.heap, 0)
}

// startEviction은 한도에 닿았을 때 힙을 만든다. 같은 수의 항목이 있어도 결과가 달라지지 않게 이름 순서로 쌓는다.
func (top *fsWatchTop) startEviction() {
	items := make(fsWatchTopHeap, 0, len(top.items))
	for _, item := range top.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].directory < items[j].directory })
	for index, item := range items {
		item.index = index
	}
	top.heap = items
	heap.Init(&top.heap)
}

// fsWatchTopRow는 요약의 한 줄이다. Percent는 전체 이벤트에서 차지하는 비율이다. Approximate는 한도에서 다른 디렉터리를 밀어내고 들어와
// Events가 실제보다 클 수 있다는 뜻이다.
type fsWatchTopRow struct {
	Directory   string
	Events      int
	Percent     int
	Approximate bool
}

// rows는 이벤트가 많은 디렉터리부터 fsWatchTopShown개다. 이벤트가 적게 끝났으면 nil이다.
func (top *fsWatchTop) rows() []fsWatchTopRow {
	if top.total < fsWatchTopMinEvents {
		return nil
	}
	rows := make([]fsWatchTopRow, 0, len(top.items))
	for directory, item := range top.items {
		rows = append(rows, fsWatchTopRow{Directory: directory, Events: item.count, Percent: (item.count*100 + top.total/2) / top.total, Approximate: item.inherited > 0})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Events != rows[j].Events {
			return rows[i].Events > rows[j].Events
		}
		return rows[i].Directory < rows[j].Directory
	})
	return rows[:min(len(rows), fsWatchTopShown)]
}

// fsWatchTopText는 종료 요약 아래에 붙일 글이다. 보일 디렉터리가 없으면 빈 문자열이다.
func fsWatchTopText(rows []fsWatchTopRow) string {
	if len(rows) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString(T("watchfs.top_heading") + "\n")
	for _, row := range rows {
		events := fmt.Sprint(row.Events)
		if row.Approximate {
			events = "~" + events
		}
		fmt.Fprintf(&text, "  %6s  %3d%%  %s\n", events, row.Percent, terminalJSON(row.Directory))
	}
	return text.String()
}
