package edc

import (
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
	// fsWatchTopMaxDirectories는 세는 디렉터리의 수다. 이름이 매번 다른 디렉터리가 쌓여도 메모리가 늘지 않게, 넘은 뒤의 새
	// 디렉터리는 따로 모은다.
	fsWatchTopMaxDirectories = 10000
)

// fsWatchTop은 감시하는 동안 디렉터리마다 이벤트를 센다. 끝날 때 무엇이 가장 시끄러웠는지 보이려는 것이다. 한 goroutine에서만 쓴다.
type fsWatchTop struct {
	counts map[string]int
	total  int
	// other는 디렉터리 수가 한도를 넘은 뒤에 온 이벤트의 수다.
	other int
}

func newFSWatchTop() *fsWatchTop { return &fsWatchTop{counts: map[string]int{}} }

// add는 감시 루트 기준 경로의 이벤트 하나를 센다.
func (top *fsWatchTop) add(eventPath string) {
	top.total++
	directory := path.Dir(filepath.ToSlash(eventPath))
	if _, seen := top.counts[directory]; !seen && len(top.counts) >= fsWatchTopMaxDirectories {
		top.other++
		return
	}
	top.counts[directory]++
}

// fsWatchTopRow는 요약의 한 줄이다. Percent는 전체 이벤트에서 차지하는 비율이다.
type fsWatchTopRow struct {
	Directory string
	Events    int
	Percent   int
}

// rows는 이벤트가 많은 디렉터리부터 fsWatchTopShown개다. 이벤트가 적게 끝났으면 nil이다.
func (top *fsWatchTop) rows() []fsWatchTopRow {
	if top.total < fsWatchTopMinEvents {
		return nil
	}
	rows := make([]fsWatchTopRow, 0, len(top.counts))
	for directory, count := range top.counts {
		rows = append(rows, fsWatchTopRow{Directory: directory, Events: count, Percent: (count*100 + top.total/2) / top.total})
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
		fmt.Fprintf(&text, "  %6d  %3d%%  %s\n", row.Events, row.Percent, terminalJSON(row.Directory))
	}
	return text.String()
}
