//go:build !linux

package edc

// audit는 /etc의 계정·sshd·sudo 파일과 /proc/sys를 읽는다. macOS는 계정을 Directory Services에 두어 같은 방식으로 읽을 수 없다.
func hostAuditSources() (auditSources, bool) { return auditSources{}, false }
