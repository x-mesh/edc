//go:build linux

package edc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// createTraceCommandCgroup은 cgroup v2 아래에 명령 전용 cgroup을 만든다. cgroup v1 host나 쓰기 권한이 없는 환경에서는
// 오류를 돌려주고, 호출한 쪽이 PID 필터로 대신한다.
func createTraceCommandCgroup() (*traceCommandCgroup, error) {
	for _, root := range traceCgroupRoots {
		if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
			continue
		}
		removeStaleTraceCommandCgroups(root)
		dir := filepath.Join(root, traceCommandCgroupName())
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
		file, err := os.Open(dir)
		if err != nil {
			_ = os.Remove(dir)
			return nil, err
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
			file.Close()
			_ = os.Remove(dir)
			return nil, err
		}
		return &traceCommandCgroup{dir: dir, file: file, id: stat.Ino}, nil
	}
	return nil, errors.New("cgroup v2 is not mounted")
}

// removeStaleTraceCommandCgroups는 죽은 edc가 남긴 빈 cgroup을 지운다. 출력 파이프가 닫혀 SIGPIPE로 죽거나 SIGKILL을 받은 edc는
// defer를 실행하지 못한다. process가 아직 남은 cgroup은 rmdir이 실패하므로 그대로 둔다.
func removeStaleTraceCommandCgroups(root string) {
	stale, _ := filepath.Glob(filepath.Join(root, traceCommandCgroupPrefix+"*"))
	for _, dir := range stale {
		name := strings.TrimPrefix(filepath.Base(dir), traceCommandCgroupPrefix)
		pidText, _, _ := strings.Cut(name, "-")
		if pid, err := strconv.Atoi(pidText); err == nil && pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			continue
		}
		_ = os.Remove(dir)
	}
}

// setTraceCommandCgroup은 명령이 cgroup 안에서 바로 시작되게 한다(clone3의 CLONE_INTO_CGROUP). 그러면 명령이 첫 번째
// 호출부터 필터에 걸린다.
func setTraceCommandCgroup(cmd *exec.Cmd, cgroup *traceCommandCgroup) {
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.file.Fd())}
}
