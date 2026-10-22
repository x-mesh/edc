package edc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// traceCommandCgroup은 명령을 담으려고 만든 cgroup v2 디렉터리다. ID는 디렉터리의 inode이고, eBPF가 보는 cgroup ID와 같다.
type traceCommandCgroup struct {
	dir  string
	file *os.File
	id   uint64
}

// remove는 남은 process를 죽이고 디렉터리를 지운다. process가 막 끝난 직후에는 kernel이 아직 cgroup을 비우는 중이라
// EBUSY가 잠시 날 수 있어 몇 번 다시 시도한다.
func (cgroup *traceCommandCgroup) remove() {
	_ = cgroup.file.Close()
	_ = os.WriteFile(cgroup.dir+"/cgroup.kill", []byte("1"), 0o644)
	for range 20 {
		err := os.Remove(cgroup.dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// traceCommand는 `trace syscall -- <command>`로 실행할 명령이다. cgroup이 있으면 명령을 그 안에서 바로 시작하므로
// 자식 process까지 처음부터 센다. 없으면 PID만 거르고, 명령은 trace가 붙을 때까지 기다렸다가 exec한다.
type traceCommand struct {
	argv   []string
	path   string
	cgroup *traceCommandCgroup
	// fallback은 cgroup을 쓰지 못한 이유다. cgroup을 쓰면 비어 있다.
	fallback string
	done     chan struct{}

	mutex       sync.Mutex
	cmd         *exec.Cmd
	interrupted bool
	signals     int
	waitErr     error
}

var errTraceCommandInterrupted = errors.New("interrupted before the command started")

// prepareTraceCommand는 명령을 찾고 cgroup을 만든다. 명령이 없으면 trace를 붙이기 전에 알 수 있도록 먼저 찾는다.
func prepareTraceCommand(argv []string) (*traceCommand, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	command := &traceCommand{argv: argv, path: path, done: make(chan struct{})}
	if command.cgroup, err = createTraceCommandCgroup(); err == nil {
		// cgroup을 만들 수 있어도 kernel이 5.7 아래이거나 seccomp가 clone3를 막으면 그 안에서 시작할 수 없다. 명령을 시작하기
		// 전에 알아야 PID 필터로 대신할 수 있으므로, 빈 명령으로 한 번 시험한다.
		if err = probeTraceCommandCgroup(command.cgroup); err != nil {
			command.cgroup.remove()
		}
	}
	if err != nil {
		command.cgroup, command.fallback = nil, err.Error()
	}
	return command, nil
}

// container는 cgroup 필터다. cgroup을 못 쓰면 nil이다.
func (command *traceCommand) container() *traceContainer {
	if command.cgroup == nil {
		return nil
	}
	return &traceContainer{name: command.argv[0], cgroups: map[uint64]bool{command.cgroup.id: true}}
}

// start는 명령을 시작하고, 끝나면 done을 닫는 goroutine을 띄운다. allowPID는 cgroup이 없을 때 쓰는 PID 필터다.
func (command *traceCommand) start(allowPID func(uint32) error) error {
	command.mutex.Lock()
	defer command.mutex.Unlock()
	if command.interrupted {
		return errTraceCommandInterrupted
	}
	var gate *os.File
	var cmd *exec.Cmd
	if command.cgroup != nil {
		cmd = exec.Command(command.path, command.argv[1:]...)
		cmd.Args[0] = command.argv[0]
		setTraceCommandCgroup(cmd, command.cgroup)
	} else {
		reader, writer, err := os.Pipe()
		if err != nil {
			return err
		}
		defer reader.Close()
		// 3번 fd가 닫히거나 한 줄이 올 때까지 기다린 뒤 exec한다. PID는 exec 전후로 같다.
		cmd = exec.Command("/bin/sh", append([]string{"-c", `read -r _ <&3; exec "$@"`, "sh", command.path}, command.argv[1:]...)...)
		cmd.ExtraFiles = []*os.File{reader}
		gate = writer
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		if gate != nil {
			gate.Close()
		}
		return err
	}
	if gate != nil {
		err := allowPID(uint32(cmd.Process.Pid))
		gate.Close()
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
	}
	command.cmd = cmd
	go func() {
		err := cmd.Wait()
		command.mutex.Lock()
		command.waitErr = err
		command.mutex.Unlock()
		close(command.done)
	}()
	return nil
}

// interrupt는 edc가 받은 신호를 명령에 알린다. SIGINT는 터미널이 명령에도 보내므로 첫 번째는 넘기지 않고, SIGTERM은
// 전달한다. 어떤 신호든 두 번째부터는 명령을 죽인다. 명령을 시작하기 전이면 시작하지 않는다.
func (command *traceCommand) interrupt(signal os.Signal) {
	command.mutex.Lock()
	defer command.mutex.Unlock()
	if command.cmd == nil {
		command.interrupted = true
		return
	}
	command.signals++
	switch {
	case command.signals > 1:
		_ = command.cmd.Process.Kill()
	case signal == syscall.SIGTERM:
		_ = command.cmd.Process.Signal(signal)
	}
}

// exitCode는 명령의 종료 코드다. 신호로 끝났으면 shell처럼 128에 신호 번호를 더한다.
func (command *traceCommand) exitCode() int {
	command.mutex.Lock()
	defer command.mutex.Unlock()
	var exitErr *exec.ExitError
	switch {
	case command.waitErr == nil:
		return 0
	case errors.As(command.waitErr, &exitErr):
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	return 1
}

func (command *traceCommand) close() {
	if command.cgroup != nil {
		command.cgroup.remove()
	}
}

const traceCommandCgroupPrefix = "edc-trace-"

func traceCommandCgroupName() string {
	return traceCommandCgroupPrefix + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
