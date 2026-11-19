//go:build !linux && !darwin

package edc

func readProcDetails(int) procDetails {
	err := errProcUnsupported
	return procDetails{FDErr: err, SocketErr: err}
}

// procHandle은 이 OS에서 아무것도 열지 않는다. 세부 정보를 읽지 않으므로 섞일 값이 없다.
type procHandle struct{}

func openProcHandle(int) (procHandle, bool) { return procHandle{}, true }

func (procHandle) sameProcess(topProcess) bool { return true }

func (procHandle) close() {}
