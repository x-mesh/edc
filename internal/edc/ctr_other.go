//go:build !linux

package edc

// ctr은 docker와 cgroup v2를 읽는다. macOS의 docker는 VM 안에서 돌아 host에서 cgroup을 읽을 수 없다.
func hostCtrSources() (ctrSources, bool) { return ctrSources{}, false }
