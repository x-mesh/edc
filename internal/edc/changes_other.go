//go:build !linux

package edc

// changes는 dpkg·dnf 로그, /etc, systemd unit 상태를 읽는다. macOS에는 이 원천이 없어 같은 방식으로 읽을 수 없다.
func hostChangesSources() (changesSources, bool) { return changesSources{}, false }
