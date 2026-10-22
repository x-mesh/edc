package edc

import (
	"fmt"
	"io"
	"sort"
	"strconv"
)

const (
	// syscallFileRows는 파일 표에 보이는 행 수다. --all은 모든 행을 보이고, --json에는 늘 모든 행이 들어간다.
	syscallFileRows = 20
	// syscallFileEventLimit은 CPU마다 1초 창에서 낼 수 있는 파일 event의 수다. 기본 상한은 오류 출력용이라 낮다.
	syscallFileEventLimit = 10000
	// syscallFilePathMax는 표가 기록하는 경로의 수다. 오래 도는 process가 임시 파일을 계속 만들면 경로가 끝없이 늘어난다.
	// 넘은 뒤의 새 경로는 건수만 세고, 이미 있는 경로는 계속 센다.
	syscallFilePathMax    = 10000
	syscallUnreadablePath = "(path unreadable)"
)

// BPF의 enum open_kind와 enum syscall_flags_mode다.
const (
	syscallOpenWrite  uint8 = 1
	syscallOpenCreate uint8 = 2
	syscallOpenTrunc  uint8 = 4
	// syscallOpenUnknown은 flags를 읽지 않은 open이다. openat2의 flags는 사용자 memory의 구조체 안에 있다.
	syscallOpenUnknown uint8 = 8
	syscallOpenExcl    uint8 = 16

	syscallFlagsNone  uint8 = 0
	syscallFlagsArg   uint8 = 1
	syscallFlagsCreat uint8 = 2
	syscallFlagsHow   uint8 = 3

	// BPF의 enum event_reason이다.
	syscallReasonFile uint8 = 1
	syscallReasonLine uint8 = 2
)

// syscallFileCall은 파일 표에 넣는 호출이다. flagsArg는 open flags 인자의 위치(0부터)이고, flagsMode는 BPF가 그 값을
// 읽는 방식이다. flags가 없는 호출은 둘 다 0이다.
type syscallFileCall struct {
	flagsArg  uint8
	flagsMode uint8
	// failedOnly는 실패한 호출만 받는다. 성공한 stat과 access는 수가 많고, 실패는 찾다가 없었던 파일을 보여 준다.
	failedOnly bool
}

// syscallFileCalls는 파일을 열거나 바꾸는 호출이고, 파일을 확인만 하는 호출은 실패한 것만 넣는다.
// 경로는 모두 syscallPathArgs에 있다. amd64에만 있는 이름은 arm64에서 번호가 없어 건너뛴다.
var syscallFileCalls = map[string]syscallFileCall{
	"open": {flagsArg: 1, flagsMode: syscallFlagsArg}, "openat": {flagsArg: 2, flagsMode: syscallFlagsArg},
	"openat2": {flagsMode: syscallFlagsHow}, "creat": {flagsMode: syscallFlagsCreat},
	"truncate": {}, "unlink": {}, "unlinkat": {}, "rmdir": {},
	"mkdir": {}, "mkdirat": {}, "mknod": {}, "mknodat": {},
	"rename": {}, "renameat": {}, "renameat2": {},
	"link": {}, "linkat": {}, "symlink": {}, "symlinkat": {},
	"chmod": {}, "fchmodat": {}, "fchmodat2": {}, "chown": {}, "lchown": {}, "fchownat": {},
	"stat": {failedOnly: true}, "lstat": {failedOnly: true}, "newfstatat": {failedOnly: true}, "statx": {failedOnly: true}, "statfs": {failedOnly: true},
	"access": {failedOnly: true}, "faccessat": {failedOnly: true}, "faccessat2": {failedOnly: true},
	"readlink": {failedOnly: true}, "readlinkat": {failedOnly: true},
}

// syscallFileAction은 호출 하나가 파일에 한 일이다. 파일 표의 호출이 아니면 빈 문자열이다. open 계열은 flags를 따른다.
// 쓰기로 열면 O_CREAT가 있어도 write다. kernel이 돌려주는 값으로는 새로 만들었는지 알 수 없고, `>>`로 덧붙이는 로그도
// O_CREAT로 열기 때문이다. 새로 만들었다고 확실한 O_CREAT|O_EXCL만 create다. flags를 읽지 않는 openat2는 other다.
func syscallFileAction(name string, kind uint8) string {
	if _, ok := syscallFileCalls[name]; !ok {
		return ""
	}
	switch name {
	case "open", "openat", "openat2", "creat":
		switch {
		case kind&syscallOpenUnknown != 0:
			return "other"
		case kind&syscallOpenCreate != 0 && kind&syscallOpenExcl != 0:
			return "create"
		case kind&syscallOpenWrite != 0:
			return "write"
		case kind&syscallOpenCreate != 0:
			return "create"
		}
		return "read"
	case "truncate":
		return "write"
	case "unlink", "unlinkat", "rmdir":
		return "delete"
	case "rename", "renameat", "renameat2":
		return "rename"
	case "mkdir", "mkdirat", "mknod", "mknodat", "link", "linkat", "symlink", "symlinkat":
		return "create"
	}
	return "other"
}

// syscallFileRow는 경로 하나에 한 일이다. 호출이 실패해도 시도한 일로 센다. 없는 파일을 열려다 실패하면 read 1과
// ENOENT 1이다.
type syscallFileRow struct {
	Path   string            `json:"path"`
	Read   uint64            `json:"read"`
	Write  uint64            `json:"write"`
	Create uint64            `json:"create"`
	Delete uint64            `json:"delete"`
	Rename uint64            `json:"rename"`
	Other  uint64            `json:"other"`
	Errors uint64            `json:"errors"`
	Errnos map[string]uint64 `json:"errnos,omitempty"`
}

func (row syscallFileRow) total() uint64 {
	return row.Read + row.Write + row.Create + row.Delete + row.Rename + row.Other
}

// errnoText는 syscallReportRow와 같은 모양이다. 예: "2 ENOENT".
func (row syscallFileRow) errnoText() string {
	return syscallReportRow{Errors: row.Errors, Errnos: row.Errnos}.errnoText()
}

// syscallFileTable은 파일 event를 경로별로 모은다.
type syscallFileTable struct {
	rows    map[string]*syscallFileRow
	dropped uint64
}

func (table *syscallFileTable) add(event syscallEvent) {
	if event.Action == "" {
		return
	}
	path := event.Path
	switch {
	case event.PathUnreadable || path == "":
		path = syscallUnreadablePath
	case event.PathTruncated:
		path += "…"
	}
	if table.rows == nil {
		table.rows = map[string]*syscallFileRow{}
	}
	row := table.rows[path]
	if row == nil && len(table.rows) >= syscallFilePathMax {
		table.dropped++
		return
	}
	if row == nil {
		row = &syscallFileRow{Path: execPrintable([]byte(path))}
		table.rows[path] = row
	}
	switch event.Action {
	case "read":
		row.Read++
	case "write":
		row.Write++
	case "create":
		row.Create++
	case "delete":
		row.Delete++
	case "rename":
		row.Rename++
	default:
		row.Other++
	}
	if event.Errno != "" {
		row.Errors++
		if row.Errnos == nil {
			row.Errnos = map[string]uint64{}
		}
		row.Errnos[event.Errno]++
	}
}

// priority는 행의 정렬 순서다. 파일을 바꾼 경로(0), 실패했거나 확인만 한 경로(1), 읽기만 한 경로(2) 순이다. 오래 도는
// process는 읽기 행이 대부분이라 호출 수 순서만 쓰면 바꾼 파일이 아래로 밀린다.
func (row syscallFileRow) priority() int {
	switch {
	case row.Write+row.Create+row.Delete+row.Rename > 0:
		return 0
	case row.Errors+row.Other > 0:
		return 1
	}
	return 2
}

// report는 바꾼 경로, 실패한 경로, 읽은 경로 순이고 각 묶음 안에서는 많이 건드린 경로가 먼저다.
func (table *syscallFileTable) report() []syscallFileRow {
	rows := make([]syscallFileRow, 0, len(table.rows))
	for _, row := range table.rows {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if left, right := rows[i].priority(), rows[j].priority(); left != right {
			return left < right
		}
		if left, right := rows[i].total(), rows[j].total(); left != right {
			return left > right
		}
		return rows[i].Path < rows[j].Path
	})
	return rows
}

// printFiles는 파일 표를 쓴다. 경로는 길이가 제각각이라 맨 뒤에 둔다. event를 놓쳤으면 표가 일부라고 알린다.
func (report syscallReport) printFiles(out io.Writer, style traceSummaryStyle, limit int) {
	io.WriteString(out, "\n")
	style.line(out, style.subheading("FILES")+style.dim(": WRITE includes opens that create the file. CREATE is mkdir, link, and opens with O_EXCL. OTHER includes failed stat and access checks"))
	shown := report.Files[:min(len(report.Files), limit)]
	errorWidth := len("ERRORS")
	for _, row := range shown {
		errorWidth = max(errorWidth, len(row.errnoText()))
	}
	style.line(out, style.dim(fmt.Sprintf("%6s %6s %6s %6s %6s %6s  %-*s %s", "READ", "WRITE", "CREATE", "DELETE", "RENAME", "OTHER", errorWidth, "ERRORS", "PATH")))
	for _, row := range shown {
		style.printf(out, "%6d %6d %6d %6d %6d %6d  %-*s %s\n", row.Read, row.Write, row.Create, row.Delete, row.Rename, row.Other, errorWidth, style.count("ERRORS", row.errnoText()), row.Path)
	}
	if hidden := len(report.Files) - len(shown); hidden > 0 {
		style.line(out, style.hint(syscallHiddenRows(hidden, false)))
	}
	if lost := report.Summary.SampledOut + report.Summary.RingLost; lost > 0 {
		style.line(out, style.hint("the table is incomplete: edc did not record "+strconv.FormatUint(lost, 10)+" file events"))
	}
	if report.FilesDropped > 0 {
		style.line(out, style.hint(fmt.Sprintf("the table holds the first %d paths: %d file events of other paths are not in it", syscallFilePathMax, report.FilesDropped)))
	}
}
