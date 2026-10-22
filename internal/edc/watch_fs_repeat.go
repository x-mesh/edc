package edc

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// fsWatchRepeatGap은 같은 줄이 이어서 나왔다고 보는 최대 간격이다. 이보다 늦게 오면 새 줄로 쓴다.
const fsWatchRepeatGap = 5 * time.Second

// fsWatchRepeater는 바로 앞에 쓴 줄과 같은 줄이 이어서 오면, 새 줄을 쓰지 않고 앞의 줄을 ×N으로 고쳐 쓰게 한다. 상태줄을 그리는
// 터미널에서만 쓴다. 고쳐 쓰려면 그 줄이 상태줄 바로 위에 있어야 하므로, 다른 것을 쓰면 reset으로 잊는다. 한 goroutine에서만 쓴다.
type fsWatchRepeater struct {
	width func() int
	style textStyle
	have  bool
	last  fsWatchRecord
	// rows는 last가 화면에서 차지하는 줄 수다. 0이면 고쳐 쓰지 않는다.
	rows int
}

func newFSWatchRepeater(width func() int, style textStyle) *fsWatchRepeater {
	return &fsWatchRepeater{width: width, style: style}
}

// reset은 화면 맨 아래 줄이 더 이상 앞의 이벤트 줄이 아니라는 뜻이다. 안내나 다른 기록을 쓴 뒤에 부른다.
func (repeater *fsWatchRepeater) reset() { repeater.have = false }

func fsWatchRepeatSignature(record fsWatchRecord) string {
	return record.Event + "\x00" + record.Path + "\x00" + record.Origin + "\x00" + record.Process + "\x00" + strconv.FormatBool(record.IsDir)
}

// fsWatchRecordCount는 기록이 합친 이벤트 수다. 합치지 않은 기록은 1이다.
func fsWatchRecordCount(record fsWatchRecord) int {
	if record.Count > 1 {
		return record.Count
	}
	return 1
}

// rowsOf는 기록 한 줄이 터미널에서 차지하는 줄 수다. 긴 경로는 줄이 넘어가므로 폭으로 나눈다. 폭을 모르면 0이다.
func (repeater *fsWatchRepeater) rowsOf(record fsWatchRecord) int {
	columns := repeater.width()
	if columns <= 0 {
		return 0
	}
	var line bytes.Buffer
	if err := writeFSWatchRecord(&line, record, false, repeater.style); err != nil {
		return 0
	}
	width := ansi.StringWidth(strings.TrimSuffix(line.String(), "\n"))
	return max(1, (width+columns-1)/columns)
}

// next는 기록을 화면에 올리는 방법을 정한다. 앞의 줄을 고쳐 써야 하면 합친 기록과 그 줄의 줄 수를, 새 줄로 쓰면 기록과 0을 돌려준다.
func (repeater *fsWatchRepeater) next(record fsWatchRecord) (fsWatchRecord, int) {
	if repeater.have && repeater.rows > 0 && fsWatchRepeatSignature(record) == fsWatchRepeatSignature(repeater.last) {
		if gap := record.Time.Sub(repeater.last.Time); gap >= 0 && gap <= fsWatchRepeatGap {
			replace := repeater.rows
			merged := record
			merged.Count = fsWatchRecordCount(repeater.last) + fsWatchRecordCount(record)
			repeater.last, repeater.rows = merged, repeater.rowsOf(merged)
			return merged, replace
		}
	}
	repeater.last, repeater.have, repeater.rows = record, true, repeater.rowsOf(record)
	return record, 0
}

// fsWatchHintLine은 안내 앞에 힌트 표시를 붙여 이벤트 줄과 구분한다.
func fsWatchHintLine(style textStyle, text string) string {
	return style.paint(T("watchfs.hint_prefix")+":", richYellow) + " " + text
}
