//go:build !linux && !darwin

package edc

// audit는 Linux와 macOS의 계정·sshd·방화벽 원천만 읽는다. 다른 OS는 같은 파일과 명령이 없다.
func hostAuditSources() (auditSources, bool) { return auditSources{}, false }
