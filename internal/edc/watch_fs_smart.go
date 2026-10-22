package edc

import (
	"path/filepath"
	"time"
)

// fsWatchSmartWindow는 이벤트를 모으는 시간이다. 규칙의 debounce 기본값과 같다. 창의 첫 이벤트를 받은 뒤 이만큼 지나면
// 모은 이벤트를 한꺼번에 낸다.
const fsWatchSmartWindow = 200 * time.Millisecond

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

func (smart *fsWatchSmart) entry(event fsWatchEvent, kind string) *fsWatchSmartEntry {
	entry := &fsWatchSmartEntry{kind: kind, path: event.Path, isDir: event.IsDir, count: 1, time: event.Time}
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
			return
		}
		switch {
		case entry != nil && entry.kind == "remove":
			// 지웠다가 같은 이름으로 다시 만들었으면 내용이 바뀐 것이다.
			entry.kind, entry.time = "modify", event.Time
		case entry != nil && (entry.kind == "create" || entry.kind == "modify"):
			entry.count, entry.time = entry.count+1, event.Time
		default:
			smart.entry(event, "create")
		}
	case "modify":
		if entry != nil && (entry.kind == "create" || entry.kind == "modify") {
			entry.count, entry.time = entry.count+1, event.Time
			return
		}
		smart.entry(event, "modify")
	case "remove":
		switch {
		case entry != nil && entry.kind == "create" && entry.origin == "":
			// 이 창 안에서 만들었다 지운 파일이다.
			entry.kind, entry.time = "temp", event.Time
		case entry != nil && (entry.kind == "create" || entry.kind == "modify"):
			entry.kind, entry.time = "remove", event.Time
		default:
			smart.entry(event, "remove")
		}
	case "rename":
		// 이 창 안에서 만든 파일의 이름을 바꾸면 그 줄을 이어 쓰고, 아니면 새 줄이다.
		if entry != nil && (entry.kind == "create" || entry.kind == "modify") {
			entry.kind, entry.time = "rename", event.Time
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

// flushAll은 창과 상관없이 모은 이벤트를 모두 낸다. 감시를 멈출 때 쓴다.
func (smart *fsWatchSmart) flushAll() []fsWatchRecord {
	var records []fsWatchRecord
	for _, entry := range smart.entries {
		if entry.hidden {
			continue
		}
		record := fsWatchRecord{Type: "event", Time: entry.time, Event: entry.kind, Path: entry.path, IsDir: entry.isDir}
		if entry.kind == "modify" && entry.count > 1 {
			record.Count = entry.count
		}
		if entry.origin != "" {
			record.Origin = fsWatchSmartOrigin(entry.origin, entry.path)
		}
		records = append(records, record)
	}
	smart.opened, smart.entries, smart.renamed = time.Time{}, nil, nil
	smart.byPath = map[string]*fsWatchSmartEntry{}
	return records
}

// fsWatchSmartOrigin은 같은 디렉터리에서 바뀐 이름이면 이름만, 다른 디렉터리에서 옮겨 왔으면 경로 전체를 보인다.
func fsWatchSmartOrigin(origin, path string) string {
	if filepath.Dir(origin) == filepath.Dir(path) {
		return filepath.Base(origin)
	}
	return origin
}
