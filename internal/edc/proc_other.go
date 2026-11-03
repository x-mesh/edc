//go:build !linux && !darwin

package edc

func readProcDetails(int) procDetails {
	err := errProcUnsupported
	return procDetails{FDErr: err, SocketErr: err}
}
