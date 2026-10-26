//go:build !linux && !darwin

package edc

// check의 원천은 Linux와 macOS에만 있다.
func hostCheckSources() (checkSources, bool) { return checkSources{}, false }
