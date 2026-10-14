//go:build linux

package edc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
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
		"total_vm_kb":  {unsafe.Offsetof(record.TotalVmKb), 344},
		"anon_rss_kb":  {unsafe.Offsetof(record.AnonRssKb), 352},
		"file_rss_kb":  {unsafe.Offsetof(record.FileRssKb), 360},
		"shmem_rss_kb": {unsafe.Offsetof(record.ShmemRssKb), 368},
		"record_end":   {unsafe.Sizeof(record), execRecordSize},
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

// TestTraceExecOOMHelper는 아래 시험이 cgroup 안에서 띄우는 process다. memory.max를 넘을 때까지 page를 채운다.
func TestTraceExecOOMHelper(t *testing.T) {
	if os.Getenv("EDC_TRACE_OOM_HELPER") != "1" {
		t.Skip("helper process for TestTraceExecReportsAnOOMKill")
	}
	var held [][]byte
	for range 512 {
		chunk := make([]byte, 1<<20)
		for index := 0; index < len(chunk); index += 4096 {
			chunk[index] = 1
		}
		held = append(held, chunk)
	}
	t.Fatalf("allocated %d MiB without an OOM kill", len(held))
}

// 시험용 cgroup의 memory.max를 32MiB로 두고 그 안에서 page를 채우게 해 memcg OOM을 일으킨다. OOM killer는 그 cgroup 안에서만
// victim을 고르므로 다른 process는 영향을 받지 않는다. oom event와, 같은 PID의 exit에 붙는 oom_killed를 함께 본다.
func TestTraceExecReportsAnOOMKill(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace exec needs BPF and perf capabilities")
	}
	if err := execTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.subtree_control")
	if err != nil || !strings.Contains(string(controllers), "memory") {
		t.Skip("needs cgroup v2 with the memory controller")
	}
	dir := fmt.Sprintf("/sys/fs/cgroup/edc-trace-oom-%d", os.Getpid())
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skip(err)
	}
	defer func() {
		// 마지막 victim이 사라진 뒤에야 rmdir이 된다.
		for range 50 {
			if os.Remove(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("remove %s", dir)
	}()
	if err := os.WriteFile(dir+"/memory.max", []byte("32M"), 0o644); err != nil {
		t.Fatal(err)
	}
	// swap이 있으면 page가 swap으로 빠져 OOM까지 오래 걸린다.
	_ = os.WriteFile(dir+"/memory.swap.max", []byte("0"), 0o644)
	cgroup, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cgroup.Close()

	stop := make(chan struct{})
	observed := make(chan execEvent, 256)
	type result struct {
		summary execSummary
		err     error
	}
	finished := make(chan result, 1)
	pids := make(chan uint32, 64)
	go func() {
		watched := map[uint32]bool{}
		summary, err := collectExecEvents(30*time.Second, 0, func(event execEvent) error {
			for drained := false; !drained; {
				select {
				case pid := <-pids:
					watched[pid] = true
				default:
					drained = true
				}
			}
			if event.Event == execEventOOM || watched[event.PID] {
				select {
				case observed <- event:
				default:
				}
			}
			return nil
		}, stop)
		finished <- result{summary, err}
	}()

	var oom, exit *execEvent
	var child uint32
	deadline := time.Now().Add(20 * time.Second)
	for (oom == nil || exit == nil) && time.Now().Before(deadline) {
		command := exec.Command(os.Args[0], "-test.run=^TestTraceExecOOMHelper$", "-test.count=1")
		command.Env = append(os.Environ(), "EDC_TRACE_OOM_HELPER=1")
		command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		child = uint32(command.Process.Pid)
		pids <- child
		_ = command.Wait()
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
			t.Fatalf("helper ended with %v, want SIGKILL from the OOM killer", command.ProcessState)
		}
		oom, exit = nil, nil
		wait := time.After(500 * time.Millisecond)
	drain:
		for {
			select {
			case event := <-observed:
				if event.PID != child {
					continue
				}
				copied := event
				switch event.Event {
				case execEventOOM:
					oom = &copied
				case execEventExit:
					exit = &copied
				}
				if oom != nil && exit != nil {
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
	if oom == nil || exit == nil {
		t.Fatalf("pid %d: oom %+v exit %+v, summary %+v", child, oom, exit, done.summary)
	}
	// memcg OOM은 메모리를 할당하던 victim 자신이 일으킨다.
	if oom.SenderPID == nil || *oom.SenderPID != child || !oom.FromKernel {
		t.Errorf("oom trigger = %+v", oom)
	}
	if oom.AnonRSSKB != nil && (*oom.AnonRSSKB < 8*1024 || *oom.AnonRSSKB > 64*1024 || oom.Process == "") {
		t.Errorf("oom detail = %+v anon %d kB", oom, *oom.AnonRSSKB)
	}
	if !exit.OOMKilled || exit.Signal != "SIGKILL" {
		t.Errorf("exit = %+v", exit)
	}
	t.Logf("oom %s · exit %s", execEventDetail(*oom), execEventDetail(*exit))
}
