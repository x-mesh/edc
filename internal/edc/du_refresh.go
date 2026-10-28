package edc

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
)

// 탐색 화면은 탐색이 끝난 뒤에도 트리를 고쳐 쓴다. 디렉터리의 mtime과 남긴 큰 파일의 크기를 주기적으로 다시 보고, 바뀐
// 디렉터리는 바로 아래 항목만 다시 읽는다. 이벤트로 받지 않는 것은 inotify가 디렉터리마다 watch를 쓰고 write마다 이벤트를
// 내기 때문이다.

const (
	duRefreshAuto = "auto"
	// duRefreshMinimum은 자동 간격의 하한이다. 폴링 한 번이 짧은 트리에서 화면 숫자가 계속 흔들리지 않게 한다.
	duRefreshMinimum = 5 * time.Second
	// duRefreshCostFactor는 폴링 시간에 곱해 다음 간격을 정한다. 50배면 폴링에 쓰는 시간이 2% 이하다.
	duRefreshCostFactor = 50
)

// duRefreshSetting은 --refresh 값이다. every가 0이면 폴링 시간에 맞춰 간격을 정한다.
type duRefreshSetting struct {
	off      bool
	every    time.Duration
	explicit bool
}

func parseDuRefresh(text string) (duRefreshSetting, error) {
	switch text {
	case duRefreshAuto:
		return duRefreshSetting{}, nil
	case "0", "0s", "off":
		return duRefreshSetting{off: true}, nil
	}
	every, err := time.ParseDuration(text)
	if err != nil || every < time.Second {
		return duRefreshSetting{}, fmt.Errorf("%s", T("du.error.refresh_value", text))
	}
	return duRefreshSetting{every: every}, nil
}

func (setting duRefreshSetting) interval(last time.Duration) time.Duration {
	if setting.every > 0 {
		return setting.every
	}
	if next := duRefreshCostFactor * last; next > duRefreshMinimum {
		return next
	}
	return duRefreshMinimum
}

// duRefresher는 화면 하나의 갱신 상태다. 트리는 화면의 Update에서만 고친다. 폴링과 다시 읽기는 한 번에 하나만 돌고
// 트리를 읽기만 한 뒤 바꿀 내용을 메시지로 돌려준다. 그 사이에 Update는 트리를 고치지 않는다.
type duRefresher struct {
	ctx     context.Context
	walker  *duWalker
	root    string
	setting duRefreshSetting
	// floor는 탐색이 남긴 큰 파일 중 가장 작은 크기다. 다시 읽을 때 이보다 큰 파일만 새 줄로 올린다.
	floor uint64
	on    bool
	// reason은 자동 갱신이 꺼진 까닭이다. 비어 있으면 사용자가 껐다.
	reason   string
	since    time.Time
	interval time.Duration
	running  bool
	// generation은 a로 껐다 켤 때 앞서 걸어 둔 타이머를 무시하게 한다.
	generation int
	pending    *duNode
	// forced는 mtime과 관계없이 다음 폴링이 다시 읽을 디렉터리다. 하드링크를 센 디렉터리가 빠지면 남은 링크가 있는 곳이다.
	forced  map[*duNode]bool
	count   int
	last    time.Time
	started time.Time
}

type duRefreshTickMsg struct{ generation int }

type duRefreshDoneMsg struct{ result duRefreshResult }

// duRefreshResult는 폴링 한 번이 찾은 변화다. Update가 트리에 옮긴다.
type duRefreshResult struct {
	started  time.Time
	duration time.Duration
	files    []duFileChange
	dirs     []duDirRescan
	usage    duUsage
	usageOK  bool
	// manual은 r 키로 다시 읽은 결과다. 폴링의 기준 시각과 간격을 바꾸지 않는다.
	manual bool
	err    error
}

// duFileChange는 남긴 큰 파일 하나의 새 크기다. gone이면 지워졌거나 파일이 아니게 됐다.
type duFileChange struct {
	node  *duNode
	bytes uint64
	gone  bool
}

// duDirRescan은 다시 읽은 디렉터리 하나다. fresh는 같은 자리를 새로 읽은 노드이고, full이 아니면 이름이 같은 하위
// 디렉터리는 읽지 않고 기존 하위 트리를 그대로 쓴다. kept는 기존 큰 파일 줄의 새 크기다.
type duDirRescan struct {
	node        *duNode
	fresh       *duNode
	full        bool
	kept        map[*duNode]uint64
	newBig      []*duNode
	directories int64
	// gone은 다시 읽으려던 디렉터리가 지워졌다는 뜻이다. recount는 이 디렉터리가 세던 hard link를 대신 셀 디렉터리다.
	gone    bool
	recount []*duNode
}

// duMarkBase는 탐색이 끝난 트리의 크기를 기준으로 남긴다.
func duMarkBase(node *duNode) {
	node.base, node.known = node.total, true
	for _, child := range node.children {
		duMarkBase(child)
	}
}

// duApplyFile은 큰 파일 하나의 크기 변화를 조상까지 옮긴다.
func duApplyFile(change duFileChange) {
	node := change.node
	if !duAttached(node) {
		return
	}
	if change.gone {
		duDetach(node)
		return
	}
	delta := change.bytes - node.total
	node.bytes, node.total = change.bytes, change.bytes
	for parent := node.parent; parent != nil; parent = parent.parent {
		parent.total += delta
	}
}

// duAttached는 노드가 아직 트리에 붙어 있는지 본다. 같은 폴링에서 조상을 먼저 다시 읽으면 그 아래 노드가 빠질 수 있다.
func duAttached(node *duNode) bool {
	for ; node.parent != nil; node = node.parent {
		found := false
		for _, child := range node.parent.children {
			if child == node {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func duDetach(node *duNode) {
	parent := node.parent
	for index, child := range parent.children {
		if child == node {
			parent.children = append(parent.children[:index:index], parent.children[index+1:]...)
			break
		}
	}
	for ancestor := parent; ancestor != nil; ancestor = ancestor.parent {
		ancestor.total -= node.total
		ancestor.count -= node.count
	}
}

// duApplyRescan은 다시 읽은 디렉터리를 기존 노드에 옮기고 늘거나 준 양을 조상에 더한다. 기존 노드를 그대로 두어 화면이
// 보고 있는 디렉터리와 커서가 바뀌지 않는다. 빠진 노드를 돌려준다.
func duApplyRescan(rescan duDirRescan) []*duNode {
	node, fresh := rescan.node, rescan.fresh
	if !duAttached(node) {
		return nil
	}
	oldTotal, oldCount := node.total, node.count
	var children, dropped []*duNode
	bytes, entries := fresh.bytes, fresh.entries
	if rescan.full {
		duCarryBase(node, fresh)
		dropped = node.children
		for _, child := range fresh.children {
			child.parent = node
		}
		children = fresh.children
	} else {
		previous := map[string]*duNode{}
		for _, child := range node.children {
			if child.dir {
				previous[child.name] = child
			}
		}
		for _, child := range fresh.children {
			if old, found := previous[child.name]; found {
				old.bytes = old.bytes - old.own + child.own
				old.total = old.total - old.own + child.own
				old.own = child.own
				children = append(children, old)
				delete(previous, child.name)
				continue
			}
			child.parent = node
			children = append(children, child)
		}
		for _, old := range previous {
			dropped = append(dropped, old)
		}
		// 남아 있는 큰 파일 줄은 새 크기로 두고, 새로 floor를 넘은 파일은 줄을 만든다. 나머지는 디렉터리의 합에 남는다.
		for _, child := range node.children {
			if child.dir {
				continue
			}
			size, kept := rescan.kept[child]
			if !kept {
				dropped = append(dropped, child)
				continue
			}
			child.bytes, child.total = size, size
			children = append(children, child)
			bytes, entries = duSubtract(bytes, size), entries-1
		}
		for _, file := range rescan.newBig {
			file.parent = node
			children = append(children, file)
			bytes, entries = duSubtract(bytes, file.bytes), entries-1
		}
	}
	node.bytes, node.entries, node.own, node.unreadable, node.children = bytes, entries, fresh.own, fresh.unreadable, children
	node.total, node.count = node.bytes, node.entries
	for _, child := range children {
		node.total += child.total
		node.count += child.count
	}
	totalDelta, countDelta := node.total-oldTotal, node.count-oldCount
	for parent := node.parent; parent != nil; parent = parent.parent {
		parent.total += totalDelta
		parent.count += countDelta
	}
	return dropped
}

func duSubtract(value, amount uint64) uint64 {
	if amount > value {
		return 0
	}
	return value - amount
}

// duCarryBase는 하위 전체를 새로 읽을 때 이름이 같은 항목의 기준값을 옮긴다. 옮기지 않으면 다시 읽은 모든 항목이 새로
// 생긴 것처럼 보인다.
func duCarryBase(old, fresh *duNode) {
	byName := map[string]*duNode{}
	for _, child := range old.children {
		byName[child.name] = child
	}
	for _, child := range fresh.children {
		if previous, found := byName[child.name]; found && previous.dir == child.dir {
			child.base, child.known = previous.base, previous.known
			if child.dir {
				duCarryBase(previous, child)
			}
		}
	}
}

// duCountDirs는 하위의 디렉터리 수다. 다시 읽기로 빠진 하위 트리를 탐색한 디렉터리 수에서 뺀다.
func duCountDirs(node *duNode) int64 {
	if !node.dir {
		return 0
	}
	count := int64(1)
	for _, child := range node.children {
		count += duCountDirs(child)
	}
	return count
}

// duSubtree는 node 아래의 노드 집합이다. 빠진 하위 트리가 센 hard link 기록을 지울 때 쓴다.
func duSubtree(node *duNode, into map[*duNode]bool) {
	into[node] = true
	for _, child := range node.children {
		duSubtree(child, into)
	}
}

// newDuRefresher는 탐색이 끝난 화면의 갱신 상태를 만든다. 일부만 센 트리와 네트워크 파일 시스템에서는 꺼 둔다. 빠진
// 디렉터리가 있는 트리는 고쳐 써도 맞는 숫자가 되지 않고, 네트워크 파일 시스템에서는 stat 하나가 왕복 한 번이다.
func newDuRefresher(ctx context.Context, setting duRefreshSetting, scan duScan, started time.Time) *duRefresher {
	if scan.walker == nil || scan.root == nil || !scan.root.dir {
		return nil
	}
	duMarkBase(scan.root)
	refresher := &duRefresher{ctx: ctx, walker: scan.walker, root: scan.root.name, setting: setting, floor: scan.walker.floor(),
		since: started, started: started, interval: setting.interval(0)}
	switch {
	case setting.off:
	case scan.partial:
		refresher.reason = T("du.browse.refresh_partial")
	case !setting.explicit && cleanRemoteFSType(scan.filesystem.FSType):
		refresher.reason = scan.filesystem.FSType
	default:
		refresher.on = true
	}
	return refresher
}

func (refresher *duRefresher) tick() tea.Cmd {
	generation := refresher.generation
	return tea.Tick(refresher.interval, func(time.Time) tea.Msg { return duRefreshTickMsg{generation} })
}

// poll은 트리 전체를 다시 보는 Cmd다. 돌아올 때까지 Update는 트리를 고치지 않는다.
func (refresher *duRefresher) poll(root *duNode) tea.Cmd {
	refresher.running = true
	ctx, walker, since, floor, forced := refresher.ctx, refresher.walker, refresher.since, refresher.floor, refresher.takeForced()
	return func() tea.Msg { return duRefreshDoneMsg{duPoll(ctx, walker, root, since, floor, forced)} }
}

func (refresher *duRefresher) rescan(node *duNode) tea.Cmd {
	refresher.running = true
	ctx, walker, root, floor, path := refresher.ctx, refresher.walker, refresher.root, refresher.floor, duNodePath(node)
	return func() tea.Msg { return duRefreshDoneMsg{duRescanFull(ctx, walker, node, path, root, floor)} }
}

// apply는 Cmd가 찾은 변화를 트리에 옮긴다. 빠진 하위 트리의 hard link 기록을 지우고, 탐색한 디렉터리 수를 고친다.
func (refresher *duRefresher) apply(scan *duScan, result duRefreshResult) {
	refresher.running = false
	if result.err != nil {
		return
	}
	for _, file := range result.files {
		duApplyFile(file)
	}
	var recount []*duNode
	for _, rescan := range result.dirs {
		if !duAttached(rescan.node) {
			continue
		}
		var dropped []*duNode
		if rescan.gone {
			// 트리의 맨 위는 뺄 곳이 없다. 화면은 그 아래가 비었다는 사실을 다음 폴링이 보여 준다.
			if rescan.node.parent == nil {
				continue
			}
			duDetach(rescan.node)
			dropped = []*duNode{rescan.node}
		} else {
			dropped = duApplyRescan(rescan)
			refresher.walker.reown(rescan.fresh, rescan.node)
			// 다시 읽은 디렉터리 자신은 이미 센 디렉터리다.
			scan.directories += rescan.directories - 1
			recount = append(recount, rescan.recount...)
		}
		gone := map[*duNode]bool{}
		for _, node := range dropped {
			if node.dir {
				duSubtree(node, gone)
			}
			scan.directories -= duCountDirs(node)
		}
		if len(gone) > 0 {
			recount = append(recount, refresher.walker.orphans(refresher.walker.forget(gone))...)
		}
	}
	for _, node := range recount {
		if duAttached(node) {
			if refresher.forced == nil {
				refresher.forced = map[*duNode]bool{}
			}
			refresher.forced[node] = true
		}
	}
	if result.usageOK {
		scan.filesystem.SizeBytes, scan.filesystem.UsedBytes = result.usage.Size, result.usage.Used
		if scan.filesystem.Whole {
			scan.notFound = duSubtract(result.usage.Used, scan.root.total+scan.deleted.Bytes)
		}
	}
	refresher.count++
	refresher.last = time.Now()
	if !result.manual {
		refresher.since = result.started
		refresher.interval = refresher.setting.interval(result.duration)
	}
}

func (refresher *duRefresher) takeForced() map[*duNode]bool {
	forced := refresher.forced
	refresher.forced = nil
	return forced
}
