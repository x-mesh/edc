//go:build (!linux && !darwin) || (!amd64 && !arm64)

package edc

import "errors"

type grepONNX struct{}

func newGrepONNX(string, string) (*grepONNX, error) {
	return nil, errors.New("ONNX Runtime is unavailable for this platform")
}
func (*grepONNX) close() {}
func (*grepONNX) encode([][]int64) ([][]float32, error) {
	return nil, errors.New("ONNX Runtime is unavailable for this platform")
}
