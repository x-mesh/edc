//go:build linux

package edc

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestPrivEventOffsetsMatchTheBPFStruct(t *testing.T) {
	var event privEventsPrivEvent
	for name, offsets := range map[string][2]uintptr{
		"timestamp_ns": {unsafe.Offsetof(event.TimestampNs), 0},
		"cgroup_id":    {unsafe.Offsetof(event.CgroupId), 8},
		"kind":         {unsafe.Offsetof(event.Kind), 16},
		"tgid":         {unsafe.Offsetof(event.Tgid), 20},
		"tid":          {unsafe.Offsetof(event.Tid), 24},
		"cap":          {unsafe.Offsetof(event.Cap), 28},
		"ids":          {unsafe.Offsetof(event.Ids), 32},
		"comm":         {unsafe.Offsetof(event.Comm), 64},
		"name":         {unsafe.Offsetof(event.Name), 80},
		"end":          {unsafe.Sizeof(event), privEventSize},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parsePrivEvent reads %d", name, offsets[0], offsets[1])
		}
	}
}

func TestParsePrivEventReadsEachKind(t *testing.T) {
	sample := make([]byte, privEventSize)
	put32 := func(offset int, value uint32) {
		for index := range 4 {
			sample[offset+index] = byte(value >> (8 * index))
		}
	}
	put32(0, 1_000)
	put32(8, 77)
	put32(20, 42)
	put32(24, 43)
	put32(28, 23)
	copy(sample[64:], "nginx")
	event, ok := parsePrivEvent(sample, 5)
	if !ok || event.Event != privEventCap || event.TimestampNS != 1_005 || event.CgroupID != 77 || event.PID != 42 || event.TID != 43 || event.Capability != "CAP_SYS_NICE" || event.Process != "nginx" {
		t.Fatalf("cap = %+v %v", event, ok)
	}
	put32(16, 1)
	for index, value := range []uint32{1000, 1000, 1000, 0, 50, 50, 50, 50} {
		put32(32+4*index, value)
	}
	if cred, _ := parsePrivEvent(sample, 0); cred.Event != privEventCred || cred.change() != "euid 1000→0" || *cred.GID != [2]uint32{50, 50} {
		t.Fatalf("cred = %+v", cred)
	}
	put32(16, 2)
	copy(sample[80:], "dummy")
	if module, _ := parsePrivEvent(sample, 0); module.Event != privEventMod || module.Module != "dummy" {
		t.Fatalf("module = %+v", module)
	}
	put32(16, 9)
	if _, ok := parsePrivEvent(sample, 0); ok {
		t.Fatal("read an unknown kind")
	}
	if _, ok := parsePrivEvent(sample[:privEventSize-1], 0); ok {
		t.Fatal("read a short record")
	}
}

const (
	privTestNiceCalls = 100
	privTestGID       = 12345
)

// TestTracePrivHelper는 아래 시험이 띄우는 process다. 두 동작 모두 이 thread의 자격만 바꾼다.
func TestTracePrivHelper(t *testing.T) {
	runtime.LockOSThread()
	switch os.Getenv("EDC_PRIV_HELPER") {
	case "nice":
		// 이 thread의 effective 집합에서 CAP_SYS_NICE만 지운다. 그러면 nice를 낮추는 setpriority마다 capable(CAP_SYS_NICE)가
		// 한 번 거부된다. RLIMIT_NICE가 0이라 그 전의 rlimit 검사는 통과하지 못한다.
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var data [2]unix.CapUserData
		if err := unix.Capget(&header, &data[0]); err != nil {
			t.Fatal(err)
		}
		data[0].Effective &^= 1 << unix.CAP_SYS_NICE
		if err := unix.Capset(&header, &data[0]); err != nil {
			t.Fatal(err)
		}
		for range privTestNiceCalls {
			if _, _, errno := syscall.RawSyscall(syscall.SYS_SETPRIORITY, syscall.PRIO_PROCESS, 0, uintptr(^uint(0))); errno == 0 {
				t.Fatal("setpriority -1 worked without CAP_SYS_NICE")
			}
		}
	case "gid":
		// syscall.Setresgid는 모든 thread를 바꾸므로 raw syscall로 이 thread만 바꾼다.
		keep := ^uintptr(0)
		for _, egid := range []uintptr{privTestGID, 0} {
			if _, _, errno := syscall.RawSyscall(syscall.SYS_SETRESGID, keep, egid, keep); errno != 0 {
				t.Fatal(errno)
			}
		}
	default:
		t.Skip("helper process for TestTracePrivCountsDeniedChecksAndCredentialChanges")
	}
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. 거부된 capability 검사의 수와 이름, 자격 변경 두 번을 runner kernel에서 본다.
func TestTracePrivCountsDeniedChecksAndCredentialChanges(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace priv needs BPF capabilities")
	}
	if err := privTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	type result struct {
		collection privCollection
		err        error
	}
	finished := make(chan result, 1)
	go func() {
		collection, err := collectPrivEvents(privTraceOptions{comms: []string{strings.TrimSpace(string(comm))}}, stop, nil)
		finished <- result{collection, err}
	}()
	time.Sleep(time.Second)
	pids := map[string]uint32{}
	for _, mode := range []string{"nice", "gid"} {
		command := exec.Command(os.Args[0], "-test.run=^TestTracePrivHelper$", "-test.count=1")
		command.Env = append(os.Environ(), "EDC_PRIV_HELPER="+mode)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s helper: %v\n%s", mode, err, output)
		}
		pids[mode] = uint32(command.ProcessState.Pid())
	}
	close(stop)
	done := <-finished
	if done.err != nil {
		t.Fatal(done.err)
	}
	if reason, missing := done.collection.summary.Unavailable["capability"]; missing {
		t.Fatalf("capability hook missing: %s", reason)
	}
	var denied int
	for _, event := range done.collection.events {
		if event.Event == privEventCap && event.PID == pids["nice"] {
			if event.Capability != "CAP_SYS_NICE" {
				t.Errorf("denied %s", event.Capability)
			}
			denied++
		}
	}
	if denied != privTestNiceCalls {
		t.Errorf("denied CAP_SYS_NICE events = %d, want %d", denied, privTestNiceCalls)
	}
	report := privReportFor(done.collection, true)
	var counted uint64
	for _, row := range report.Capabilities {
		if row.Capability == "CAP_SYS_NICE" && row.Result == "denied" && !row.NoAudit {
			counted += row.Checks
		}
	}
	// 같은 이름의 시험 process도 걸리지만 CAP_SYS_NICE를 거부당하는 것은 helper뿐이다.
	if counted != privTestNiceCalls {
		t.Errorf("denied CAP_SYS_NICE checks = %d, want %d", counted, privTestNiceCalls)
	}
	if reason, missing := done.collection.summary.Unavailable["credentials"]; missing {
		t.Fatalf("credential hook missing: %s", reason)
	}
	var changes []string
	for _, event := range report.Credentials {
		if event.PID == pids["gid"] {
			changes = append(changes, event.change())
		}
	}
	if strings.Join(changes, ", ") != "egid 0→12345, egid 12345→0" {
		t.Errorf("credential changes = %q", changes)
	}
	t.Logf("denied %d, changes %q", denied, changes)
}
