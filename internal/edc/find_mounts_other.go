//go:build !linux

package edc

// findSelfBind는 Linux의 bind mount 판별이다. macOS의 마운트 표에는 bind mount가 없다.
func findSelfBind([]diskMount, diskMount) bool { return false }
