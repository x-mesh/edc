//go:build linux

package edc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestExecRecordOffsetsMatchTheBPFStruct(t *testing.T) {
	var record execEventsExecRecord
	for name, offsets := range map[string][2]uintptr{
		"timestamp_ns": {unsafe.Offsetof(record.TimestampNs), 0},
		"cgroup_id":    {unsafe.Offsetof(record.CgroupId), 8},
		"duration_ns":  {unsafe.Offsetof(record.DurationNs), 16},
		"pid":          {unsafe.Offsetof(record.Pid), 24},
		"ppid":         {unsafe.Offsetof(record.Ppid), 28},
		"sender_pid":   {unsafe.Offsetof(record.SenderPid), 32},
		"uid":          {unsafe.Offsetof(record.Uid), 36},
		"code":         {unsafe.Offsetof(record.Code), 40},
		"signal":       {unsafe.Offsetof(record.Signal), 44},
		"comm":         {unsafe.Offsetof(record.Comm), 48},
		"parent_comm":  {unsafe.Offsetof(record.ParentComm), 64},
		"kind":         {unsafe.Offsetof(record.Kind), 80},
		"flags":        {unsafe.Offsetof(record.Flags), 81},
		"args_len":     {unsafe.Offsetof(record.ArgsLen), 82},
		"args":         {unsafe.Offsetof(record.Args), 84},
		"args_end":     {unsafe.Offsetof(record.Args) + unsafe.Sizeof(record.Args), execRecordSize},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parseExecRecord reads %d", name, offsets[0], offsets[1])
		}
	}
	if len(record.Args) != execArgsMax {
		t.Fatalf("BPF args holds %d bytes, parseExecRecord accepts %d", len(record.Args), execArgsMax)
	}
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. runner kernel의 verifier가 execEvents를 받는지와 세 tracepoint의
// record가 parseExecRecord까지 오는지를 함께 본다.
func TestTraceExecCapturesExecSignalAndExit(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace exec needs BPF and perf capabilities")
	}
	if err := execTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	mask, err := parseExecSignals("USR2")
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("edc-trace-exec-test-%d", os.Getpid())
	stop := make(chan struct{})
	// 시험 process의 event만 보낸다. 다른 process의 event가 많아도 넘치지 않을 만큼 둔다.
	observed := make(chan execEvent, 256)
	type result struct {
		summary execSummary
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		pids := map[uint32]bool{}
		summary, err := collectExecEvents(20*time.Second, mask, func(event execEvent) error {
			if event.Event == execEventExec && strings.Contains(event.Args, marker) {
				pids[event.PID] = true
			}
			if pids[event.PID] {
				select {
				case observed <- event:
				default:
				}
			}
			return nil
		}, stop)
		finished <- result{summary, err}
	}()

	// program을 붙인 때를 알 수 없으므로, 세 event를 모두 볼 때까지 짧은 process를 되풀이해 띄운다.
	byPID := map[uint32]map[string]execEvent{}
	var complete map[string]execEvent
	deadline := time.Now().Add(15 * time.Second)
	for complete == nil && time.Now().Before(deadline) {
		command := exec.Command("/bin/sh", "-c", `trap 'exit 7' USR2; kill -USR2 $$; exit 9`, marker)
		if err := command.Run(); err == nil || command.ProcessState.ExitCode() != 7 {
			t.Fatalf("test process: %v exit %d", err, command.ProcessState.ExitCode())
		}
		wait := time.After(200 * time.Millisecond)
	drain:
		for {
			select {
			case event := <-observed:
				if byPID[event.PID] == nil {
					byPID[event.PID] = map[string]execEvent{}
				}
				byPID[event.PID][event.Event] = event
				if len(byPID[event.PID]) == 3 {
					complete = byPID[event.PID]
					break drain
				}
			case <-wait:
				break drain
			}
		}
	}
	close(stop)
	done := <-finished
	if done.err != nil {
		t.Fatal(done.err)
	}
	if complete == nil {
		t.Fatalf("no process showed exec, signal, and exit: %v, summary %+v", byPID, done.summary)
	}
	started, signaled, exited := complete[execEventExec], complete[execEventSignal], complete[execEventExit]
	if started.PPID != uint32(os.Getpid()) || started.Process != "sh" || !strings.Contains(started.Args, "kill -USR2") {
		t.Errorf("exec = %+v", started)
	}
	if signaled.Signal != "SIGUSR2" || signaled.SenderPID == nil || *signaled.SenderPID != signaled.PID || signaled.Result != "delivered" || signaled.FromKernel {
		t.Errorf("signal = %+v", signaled)
	}
	if exited.ExitCode == nil || *exited.ExitCode != 7 || exited.Signal != "" || exited.LifetimeMS == nil || *exited.LifetimeMS <= 0 {
		t.Errorf("exit = %+v", exited)
	}
	if done.summary.EventCount == 0 {
		t.Errorf("summary = %+v", done.summary)
	}
}
