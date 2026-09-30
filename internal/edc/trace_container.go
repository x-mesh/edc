package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// traceContainer는 --container로 고른 container다. cgroups는 그 container의 cgroup과 하위 cgroup의 ID다.
type traceContainer struct {
	name    string
	cgroups map[uint64]bool
}

// traceDockerInspect는 container의 첫 process PID와 이름을 docker에게 묻는다. test가 바꾼다.
var traceDockerInspect = func(reference string) ([]byte, error) {
	return exec.Command("docker", "inspect", "--type", "container", "--format", "{{.State.Pid}} {{.Name}}", reference).Output()
}

// traceProcRoot와 traceCgroupRoots는 test가 바꾼다. systemd의 hybrid 모드는 cgroup v2를 unified 아래에 붙인다.
var (
	traceProcRoot    = "/proc"
	traceCgroupRoots = []string{"/sys/fs/cgroup", "/sys/fs/cgroup/unified"}
)

// resolveTraceContainer는 docker container의 이름이나 ID를 cgroup ID로 바꾼다. eBPF event는 process의 cgroup v2 ID를
// 담고, 그 ID는 cgroup 디렉터리의 inode와 같다. 실패하면 exit code를 함께 돌려준다.
func resolveTraceContainer(reference string) (*traceContainer, int, error) {
	output, err := traceDockerInspect(reference)
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return nil, 2, errors.New(T("cli.trace.container_inspect", reference, strings.TrimSpace(string(exitErr.Stderr))))
	case err != nil:
		return nil, 3, errors.New(T("cli.trace.container_docker", err))
	}
	pidText, name, _ := strings.Cut(strings.TrimSpace(string(output)), " ")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return nil, 1, fmt.Errorf("docker inspect %s: unexpected output %q", reference, output)
	}
	name = strings.TrimPrefix(name, "/")
	if pid <= 0 {
		return nil, 2, errors.New(T("cli.trace.container_stopped", name))
	}
	cgroups, err := traceProcessCgroups(pid)
	if err != nil {
		return nil, 3, errors.New(T("cli.trace.container_cgroup", name, err))
	}
	return &traceContainer{name: name, cgroups: cgroups}, 0, nil
}

// traceProcessCgroups는 pid가 있는 cgroup v2와 그 아래 cgroup의 ID다. docker exec로 띄운 process도 같은 cgroup에
// 들어가고, container 안에서 만든 하위 cgroup은 따로 모은다. trace를 시작할 때 한 번만 읽는다.
func traceProcessCgroups(pid int) (map[uint64]bool, error) {
	data, err := os.ReadFile(filepath.Join(traceProcRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return nil, err
	}
	path, found := "", false
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			path, found = rest, true
			break
		}
	}
	if !found {
		return nil, errors.New("the process has no cgroup v2 entry. cgroup v1 is not supported")
	}
	// root cgroup을 모으면 host의 모든 process가 걸린다.
	if path == "/" {
		return nil, errors.New("the process is in the root cgroup")
	}
	for _, root := range traceCgroupRoots {
		if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
			continue
		}
		directory := filepath.Join(root, path)
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			continue
		}
		cgroups := map[uint64]bool{}
		err := filepath.WalkDir(directory, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.IsDir() {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				cgroups[uint64(stat.Ino)] = true
			}
			return nil
		})
		return cgroups, err
	}
	return nil, fmt.Errorf("cgroup %s is not under %s", path, strings.Join(traceCgroupRoots, " or "))
}

func traceContainerName(container *traceContainer) string {
	if container == nil {
		return ""
	}
	return container.name
}

// matches는 --process, --destination, --container로 event를 거른다. 주인을 모르는 socket의 TCP event는 cgroup ID가
// 0이라 --container에 걸리지 않는다. --process도 같다.
func (options tcpTraceOptions) matches(event captureEvent) bool {
	if !traceEventMatches(event, options.process, options.destination) {
		return false
	}
	return options.container == nil || options.container.cgroups[event.CgroupID]
}
