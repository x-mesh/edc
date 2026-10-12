//go:build !linux

package edc

// svc는 systemd와 /proc을 읽는다. macOS의 launchd는 unit·cgroup 모델이 달라 같은 화면을 만들 수 없다.
func hostSvcSources() (svcSources, bool) { return svcSources{}, false }
