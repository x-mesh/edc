package edc

import (
	"errors"
	"fmt"
)

// listenStopProcess는 S로 고른 process를 확인 상자가 떠 있는 동안 잡아 둔다. stop은 잡을 때의 process가 아직 그 PID를
// 쥐고 있을 때만 SIGTERM을 보낸다.
type listenStopProcess interface {
	stop() error
	close()
}

var (
	errListenStopGone        = errors.New("process exited")
	errListenStopChanged     = errors.New("process changed")
	errListenStopPermission  = errors.New("permission denied")
	errListenStopUnsupported = errors.New("unsupported")
)

// listenStopMessage는 S를 처리하다 난 오류를 화면 안내로 바꾼다.
func listenStopMessage(pid int, err error) string {
	switch {
	case errors.Is(err, errListenStopGone):
		return T("listenbrowse.stop_gone", pid)
	case errors.Is(err, errListenStopChanged):
		return T("listenbrowse.stop_changed", pid)
	case errors.Is(err, errListenStopPermission):
		return T("listenbrowse.stop_permission", pid)
	case errors.Is(err, errListenStopUnsupported):
		return T("listenbrowse.stop_unsupported")
	}
	return T("listenbrowse.stop_failed", pid, infoSingleLine(fmt.Sprint(err)))
}
