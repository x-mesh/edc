//go:build !linux

package edc

// check는 systemd, /proc, 커널 기록을 읽는다. macOS에는 같은 원천이 없다.
func hostCheckSources() (checkSources, bool) { return checkSources{}, false }
