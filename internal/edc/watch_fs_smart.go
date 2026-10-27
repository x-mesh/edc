package edc

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// fsWatchSmartWindow는 이벤트를 모으는 시간이다. 규칙의 debounce 기본값과 같다. 창의 첫 이벤트를 받은 뒤 이만큼 지나면
// 모은 이벤트를 한꺼번에 낸다.
const fsWatchSmartWindow = 200 * time.Millisecond

// fsWatchSmartGroupMin은 한 창에서 같은 디렉터리의 같은 종류 줄을 한 줄로 묶기 시작하는 수다. rm -rf나 복사는 디렉터리마다
// 수천 줄을 내는데, 터미널이 그 줄을 찍는 속도가 이벤트보다 느려 뒤의 이벤트를 잃는다. 이 수를 넘으면 `REMOVE "dir/..." ×N`처럼
// 디렉터리 한 줄로 보인다. 경로는 터미널 출력이 ASCII 밖의 글자를 \u로 바꾸므로 …이 아니라 ...을 붙인다.
const fsWatchSmartGroupMin = 50

// fsWatchSmart는 짧은 창 안의 이벤트를 모아 결과만 보이는 기록으로 바꾼다. 임시 파일에 쓰고 이름을 바꿔 교체하는 저장은
// create, modify, rename, create 네 줄이 되는데, 이것을 최종 이름 한 줄로 줄인다. 사람이 읽는 출력에만 쓴다. 규칙과 JSON은
// 원본 이벤트를 쓴다. 한 goroutine에서만 쓴다.
type fsWatchSmart struct {
	window  time.Duration
	opened  time.Time
	entries []*fsWatchSmartEntry
	byPath  map[string]*fsWatchSmartEntry
	// renamed는 바로 앞 이벤트가 rename일 때 그 항목이다. 바로 뒤의 create가 그 이름의 도착점이다.
	renamed *fsWatchSmartEntry
}

type fsWatchSmartEntry struct {
	kind   string
	path   string
	isDir  bool
	origin string
	count  int
	time   time.Time
	hidden bool
	// pid는 이름을 아는 마지막 이벤트를 일으킨 process의 번호이고, processes는 이 줄에 든 이벤트를 일으킨 서로 다른 process의 이름을
	// 처음 나온 순서로 모은 것이다. 이름을 모르는 이벤트는 아무것도 바꾸지 않는다.
	pid       int
	processes []string
	// known은 이름이 fsWatchSmartScanNames개를 넘은 뒤에만 쓰는 조회용 집합이다. 적을 때는 슬라이스를 훑는 쪽이 싸다.
	known map[string]struct{}
}

func newFSWatchSmart(window time.Duration) *fsWatchSmart {
	return &fsWatchSmart{window: window, byPath: map[string]*fsWatchSmartEntry{}}
}

// fsWatchSmartEnabled는 묶어서 보일지 정한다. 기본은 터미널의 텍스트 출력에서 켜고, JSON은 모든 이벤트를 그대로 낸다.
// --smart는 pipe로 나가는 텍스트에서도 켜고, --no-smart는 늘 끈다.
func fsWatchSmartEnabled(on, off bool, jsonPath string, terminal bool) bool {
	return !off && jsonPath == "" && (on || terminal)
}

func (smart *fsWatchSmart) pending() bool { return !smart.opened.IsZero() }

// deadline은 모은 이벤트를 내야 하는 시각이다. 모은 것이 없으면 false다.
func (smart *fsWatchSmart) deadline() (time.Time, bool) {
	if !smart.pending() {
		return time.Time{}, false
	}
	return smart.opened.Add(smart.window), true
}

func (entry *fsWatchSmartEntry) setProcess(event fsWatchEvent) {
	if event.Process == "" {
		return
	}
	entry.pid = event.PID
	entry.addProcess(event.Process)
}

// addProcess는 이 줄에 든 서로 다른 process의 이름을 처음 나온 순서로 모은다. 이름 수에는 상한이 없다. 한 줄이 가질 수 있는 이름은
// 창 안의 이벤트 수를 넘지 못하므로 메모리가 끝없이 늘지 않고, 많아지면 집합으로 조회해 이벤트마다 훑는 비용도 늘지 않는다.
func (entry *fsWatchSmartEntry) addProcess(name string) {
	if entry.known == nil {
		if slices.Contains(entry.processes, name) {
			return
		}
		entry.processes = append(entry.processes, name)
		if len(entry.processes) > fsWatchSmartScanNames {
			entry.known = make(map[string]struct{}, 2*len(entry.processes))
			for _, known := range entry.processes {
				entry.known[known] = struct{}{}
			}
		}
		return
	}
	if _, ok := entry.known[name]; ok {
		return
	}
	entry.known[name] = struct{}{}
	entry.processes = append(entry.processes, name)
}

// fsWatchSmartProcessShown은 한 줄에 이름으로 보이는 process의 수다. 넘으면 나머지는 수만 붙인다. fsWatchSmartScanNames는 슬라이스를
// 훑어 이름을 찾는 한도다.
const (
	fsWatchSmartProcessShown = 3
	fsWatchSmartScanNames    = 8
)

// fsWatchProcessNames는 줄에 보일 process 이름이다. 예: "editor, formatter" 또는 "a, b, c +2".
func fsWatchProcessNames(names []string) string {
	if len(names) <= fsWatchSmartProcessShown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(names[:fsWatchSmartProcessShown], ", "), len(names)-fsWatchSmartProcessShown)
}

func (smart *fsWatchSmart) entry(event fsWatchEvent, kind string) *fsWatchSmartEntry {
	entry := &fsWatchSmartEntry{kind: kind, path: event.Path, isDir: event.IsDir, count: 1, time: event.Time}
	entry.setProcess(event)
	smart.entries = append(smart.entries, entry)
	smart.byPath[event.Path] = entry
	return entry
}

// add는 이벤트 하나를 모은다. now는 창을 여는 시각이다.
func (smart *fsWatchSmart) add(event fsWatchEvent, now time.Time) {
	if !smart.pending() {
		smart.opened = now
	}
	renamed := smart.renamed
	smart.renamed = nil
	entry := smart.byPath[event.Path]
	switch event.Event {
	case "create":
		// 바로 앞 rename의 도착점이면 그 이름을 출처로 적고, 옛 이름의 줄은 낸다고 하지 않는다.
		if renamed != nil && renamed.isDir == event.IsDir && renamed.path != event.Path {
			renamed.hidden = true
			delete(smart.byPath, renamed.path)
			created := smart.entry(event, "create")
			created.origin = renamed.path
			// 이름이 바뀐 줄은 옛 이름 줄의 동작이 이어진 것이므로, 거기에 든 process도 함께 보인다. 도착점 이벤트의 process가 뒤에 온다.
			inherited := created.processes
			created.processes, created.known = nil, nil
			for _, name := range renamed.processes {
				created.addProcess(name)
			}
			for _, name := range inherited {
				created.addProcess(name)
			}
			if created.pid == 0 {
				created.pid = renamed.pid
			}
			return
		}
		switch {
		case entry != nil && entry.kind == "remove":
			// 지웠다가 같은 이름으로 다시 만들었으면 내용이 바뀐 것이다.
			entry.kind, entry.time = "modify", event.Time
			entry.setProcess(event)
		case entry != nil && (entry.kind == "create" || entry.kind == "modify"):
			entry.count, entry.time = entry.count+1, event.Time
			entry.setProcess(event)
		default:
			smart.entry(event, "create")
		}
	case "modify":
		if entry != nil && (entry.kind == "create" || entry.kind == "modify") {
			entry.count, entry.time = entry.count+1, event.Time
			entry.setProcess(event)
			return
		}
		smart.entry(event, "modify")
	case "remove":
		switch {
		case entry != nil && entry.kind == "create" && entry.origin == "":
			// 이 창 안에서 만들었다 지운 파일이다.
			entry.kind, entry.time = "temp", event.Time
			entry.setProcess(event)
		case entry != nil && (entry.kind == "create" || entry.kind == "modify"):
			entry.kind, entry.time = "remove", event.Time
			entry.setProcess(event)
		default:
			smart.entry(event, "remove")
		}
	case "rename":
		// 이 창 안에서 만든 파일의 이름을 바꾸면 그 줄을 이어 쓰고, 아니면 새 줄이다.
		if entry != nil && (entry.kind == "create" || entry.kind == "modify") {
			entry.kind, entry.time = "rename", event.Time
			entry.setProcess(event)
			smart.renamed = entry
			return
		}
		smart.renamed = smart.entry(event, "rename")
	default:
		smart.entry(event, event.Event)
	}
}

// flush는 창이 지났으면 모은 이벤트를 기록으로 내고 비운다. 아직이면 nil이다.
func (smart *fsWatchSmart) flush(now time.Time) []fsWatchRecord {
	if !smart.pending() || now.Sub(smart.opened) < smart.window {
		return nil
	}
	return smart.flushAll()
}

// flushAll은 창과 상관없이 모은 이벤트를 모두 낸다. 감시를 멈출 때 쓴다. 한 디렉터리에 같은 종류의 줄이
// fsWatchSmartGroupMin개를 넘으면 그 줄들은 첫 줄의 자리에 디렉터리 한 줄로 낸다.
func (smart *fsWatchSmart) flushAll() []fsWatchRecord {
	groups := smart.groups()
	var records []fsWatchRecord
	for _, entry := range smart.entries {
		if entry.hidden {
			continue
		}
		if group, ok := groups[fsWatchSmartGroupKey(entry)]; ok {
			if group.first == entry {
				records = append(records, group.record())
			}
			continue
		}
		records = append(records, entry.record())
	}
	smart.opened, smart.entries, smart.renamed = time.Time{}, nil, nil
	smart.byPath = map[string]*fsWatchSmartEntry{}
	return records
}

// record는 줄 하나의 기록이다.
func (entry *fsWatchSmartEntry) record() fsWatchRecord {
	record := fsWatchRecord{Type: "event", Time: entry.time, Event: entry.kind, Path: entry.path, IsDir: entry.isDir, PID: entry.pid, Process: fsWatchProcessNames(entry.processes)}
	if entry.kind == "modify" && entry.count > 1 {
		record.Count = entry.count
	}
	if entry.origin != "" {
		record.Origin = fsWatchSmartOrigin(entry.origin, entry.path)
	}
	return record
}

// fsWatchSmartGroup은 한 디렉터리에서 같은 종류로 묶인 줄들이다. first는 묶음이 나갈 자리를 정하는 첫 줄이다.
type fsWatchSmartGroup struct {
	directory string
	kind      string
	first     *fsWatchSmartEntry
	count     int
	time      time.Time
	pid       int
	processes []string
	known     map[string]struct{}
}

// fsWatchSmartGroupKey는 줄이 속하는 묶음이다. 디렉터리와 종류가 같아야 한 묶음이다.
func fsWatchSmartGroupKey(entry *fsWatchSmartEntry) string {
	return path.Dir(entry.path) + "\x00" + entry.kind
}

// groups는 fsWatchSmartGroupMin개를 넘는 묶음을 찾는다. 줄이 그 수를 넘지 않으면 셀 필요가 없다.
func (smart *fsWatchSmart) groups() map[string]*fsWatchSmartGroup {
	if len(smart.entries) <= fsWatchSmartGroupMin {
		return nil
	}
	counts := map[string]int{}
	for _, entry := range smart.entries {
		if !entry.hidden {
			counts[fsWatchSmartGroupKey(entry)]++
		}
	}
	groups := map[string]*fsWatchSmartGroup{}
	for _, entry := range smart.entries {
		key := fsWatchSmartGroupKey(entry)
		if entry.hidden || counts[key] <= fsWatchSmartGroupMin {
			continue
		}
		group, ok := groups[key]
		if !ok {
			group = &fsWatchSmartGroup{directory: path.Dir(entry.path), kind: entry.kind, first: entry, known: map[string]struct{}{}}
			groups[key] = group
		}
		group.count++
		if entry.time.After(group.time) {
			group.time = entry.time
		}
		if entry.pid != 0 {
			group.pid = entry.pid
		}
		for _, name := range entry.processes {
			if _, seen := group.known[name]; !seen {
				group.known[name] = struct{}{}
				group.processes = append(group.processes, name)
			}
		}
	}
	return groups
}

// record는 묶음의 한 줄이다. 경로는 디렉터리 뒤에 ...을 붙이고, Count는 묶인 항목의 수다.
func (group *fsWatchSmartGroup) record() fsWatchRecord {
	return fsWatchRecord{Type: "event", Time: group.time, Event: group.kind, Path: fsWatchSmartGroupPath(group.directory), Count: group.count, PID: group.pid, Process: fsWatchProcessNames(group.processes)}
}

// fsWatchSmartGroupPath는 묶은 줄에 보일 경로다. 감시 루트 바로 아래는 "./..."이다.
func fsWatchSmartGroupPath(directory string) string {
	if directory == "." {
		return "./..."
	}
	return directory + "/..."
}

// fsWatchSmartOrigin은 같은 디렉터리에서 바뀐 이름이면 이름만, 다른 디렉터리에서 옮겨 왔으면 경로 전체를 보인다.
func fsWatchSmartOrigin(origin, path string) string {
	if filepath.Dir(origin) == filepath.Dir(path) {
		return filepath.Base(origin)
	}
	return origin
}
