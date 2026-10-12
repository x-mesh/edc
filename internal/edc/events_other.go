//go:build !linux

package edc

// events는 /dev/kmsg와 systemd journal을 읽는다. macOS에는 둘 다 없고 unified log는 형식이 달라 같은 판정을 할 수 없다.
func hostEventsSources() (eventsSources, bool) { return eventsSources{}, false }
