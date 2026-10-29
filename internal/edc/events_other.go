//go:build !linux

package edc

// events는 /dev/kmsg와 systemd journal을 읽는다. macOS에는 둘 다 없어 진단 보고서를 읽는 collectMacEventsFacts를 쓴다.
func hostEventsSources() (eventsSources, bool) { return eventsSources{}, false }
