//go:build !linux && !darwin

package edc

func readProcDetails(int) procDetails {
	err := errProcUnsupported
	return procDetails{FDErr: err, SocketErr: err}
}

// procReplaced는 이 OS에서 확인하지 않는다. 세부 정보를 읽지 않으므로 섞일 값이 없다.
func procReplaced(topProcess) bool { return false }
