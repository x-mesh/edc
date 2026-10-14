//go:build linux

package edc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestReclaimEventOffsetsMatchTheBPFStruct(t *testing.T) {
	var event reclaimEventsReclaimEvent
	for name, offsets := range map[string][2]uintptr{
		"timestamp_ns": {unsafe.Offsetof(event.TimestampNs), 0},
		"latency_ns":   {unsafe.Offsetof(event.LatencyNs), 8},
		"reclaimed":    {unsafe.Offsetof(event.Reclaimed), 16},
		"cgroup_id":    {unsafe.Offsetof(event.CgroupId), 24},
		"tgid":         {unsafe.Offsetof(event.Tgid), 32},
		"tid":          {unsafe.Offsetof(event.Tid), 36},
		"kind":         {unsafe.Offsetof(event.Kind), 40},
		"comm":         {unsafe.Offsetof(event.Comm), 48},
		"end":          {unsafe.Sizeof(event), reclaimEventSize},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parseReclaimEvent reads %d", name, offsets[0], offsets[1])
		}
	}
}

func TestParseReclaimEventReadsEachField(t *testing.T) {
	sample := make([]byte, reclaimEventSize)
	put := func(offset int, value uint64) {
		for index := range 8 {
			sample[offset+index] = byte(value >> (8 * index))
		}
	}
	put(0, 1_000)
	put(8, 2_500_000)
	put(16, 3)
	put(24, 77)
	put(32, uint64(42)|uint64(43)<<32)
	put(40, 1)
	copy(sample[48:], "postgres")
	event, ok := parseReclaimEvent(sample, 5, 4096)
	if !ok || event.TimestampNS != 1_005 || event.LatencyMS != 2.5 || event.CgroupID != 77 || event.PID != 42 || event.TID != 43 || event.Kind != "memcg" || event.Process != "postgres" || event.Reclaimed == nil || *event.Reclaimed != 3*4096 {
		t.Fatalf("event = %+v %v", event, ok)
	}
	put(40, 2)
	if throttle, _ := parseReclaimEvent(sample, 0, 4096); throttle.Kind != "throttle" || throttle.Reclaimed != nil {
		t.Fatalf("throttle = %+v", throttle)
	}
	if _, ok := parseReclaimEvent(sample[:reclaimEventSize-1], 0, 4096); ok {
		t.Fatal("read a short record")
	}
}

// TestTraceReclaimHelper는 아래 시험이 cgroup 안에서 띄우는 process다. read는 file을 읽어 page cache를 채우고, anon은
// memory.high를 넘게 할당한 채 사용자 공간과 kernel을 오간다.
func TestTraceReclaimHelper(t *testing.T) {
	switch os.Getenv("EDC_RECLAIM_HELPER") {
	case "read":
		for range 3 {
			file, err := os.Open(os.Getenv("EDC_RECLAIM_FILE"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, file)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	case "anon":
		// memory.high를 넘으면 kernel이 사용자 공간으로 돌아갈 때마다 재운다. 시험이 3초 뒤에 이 process를 끝낸다.
		var held [][]byte
		for range 40 {
			chunk := make([]byte, 1<<20)
			for index := 0; index < len(chunk); index += 4096 {
				chunk[index] = 1
			}
			held = append(held, chunk)
			syscall.Getppid()
		}
		_ = held
	default:
		t.Skip("helper process for TestTraceReclaimCountsMemcgReclaimAndThrottle")
	}
}

// reclaimTestCgroup은 memory 설정을 건 시험용 cgroup이다. 그 안의 process만 회수와 throttle을 겪는다. swap을 막아 anon
// page가 host의 swap으로 나가지 않게 한다.
func reclaimTestCgroup(t *testing.T, name string, settings map[string]string) (*os.File, uint64) {
	t.Helper()
	dir := fmt.Sprintf("/sys/fs/cgroup/edc-trace-reclaim-%s-%d", name, os.Getpid())
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() {
		for range 50 {
			if os.Remove(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("remove %s", dir)
	})
	_ = os.WriteFile(dir+"/memory.swap.max", []byte("0"), 0o644)
	for file, value := range settings {
		if err := os.WriteFile(dir+"/"+file, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	var stat unix.Stat_t
	if err := unix.Fstat(int(handle.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	// cgroup v2의 cgroup id는 그 디렉터리의 inode 번호다.
	return handle, stat.Ino
}

// reclaimRunHelper는 limit이 0이 아니면 그 시간 뒤에 helper를 SIGKILL로 끝낸다. throttle에 걸린 helper는 스스로 끝나지
// 않을 만큼 오래 잠든다.
func reclaimRunHelper(t *testing.T, cgroup *os.File, mode string, limit time.Duration, env ...string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestTraceReclaimHelper$", "-test.count=1")
	command.Env = append(append(os.Environ(), "EDC_RECLAIM_HELPER="+mode), env...)
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if limit > 0 {
		timer := time.AfterFunc(limit, func() { _ = command.Process.Kill() })
		defer timer.Stop()
	}
	err := command.Wait()
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); limit > 0 && ok && status.Signal() == syscall.SIGKILL {
		return
	}
	if err != nil {
		t.Fatalf("%s helper: %v", mode, err)
	}
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. page cache가 memory.max에 닿으면 memcg 회수가, anon이 memory.high를
// 넘으면 throttle이 생기는지 본다. 회수할 file은 tmpfs에 두면 회수되지 않으므로, TempDir이 tmpfs이면
// EDC_RECLAIM_DIR에 disk 디렉터리를 준다.
func TestTraceReclaimCountsMemcgReclaimAndThrottle(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace reclaim needs BPF capabilities")
	}
	if err := reclaimTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.subtree_control")
	if err != nil || !strings.Contains(string(controllers), "memory") {
		t.Skip("needs cgroup v2 with the memory controller")
	}
	dir := os.Getenv("EDC_RECLAIM_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == unix.TMPFS_MAGIC {
		t.Skipf("%s is tmpfs, where page cache is not reclaimed. Set EDC_RECLAIM_DIR to a disk directory", dir)
	}
	path := fmt.Sprintf("%s/edc-reclaim-%d.data", dir, os.Getpid())
	data, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	chunk := make([]byte, 1<<20)
	for range 64 {
		if _, err := data.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	// 쓴 page는 이 시험 process의 cgroup에 잡혀 있다. 비워 두어야 helper가 읽을 때 helper의 cgroup으로 다시 잡힌다.
	if err := data.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fadvise(int(data.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	data.Close()
	limited, limitedID := reclaimTestCgroup(t, "max", map[string]string{"memory.max": "16M"})
	high, highID := reclaimTestCgroup(t, "high", map[string]string{"memory.high": "16M"})

	stop := make(chan struct{})
	type result struct {
		collection reclaimCollection
		err        error
	}
	finished := make(chan result, 1)
	options := reclaimTraceOptions{container: &traceContainer{name: "test", cgroups: map[uint64]bool{limitedID: true, highID: true}}}
	go func() {
		collection, err := collectReclaimStats(options, stop, nil)
		finished <- result{collection, err}
	}()
	// program을 붙인 때를 알 수 없으므로 잠시 기다린다. 붙기 전의 회수는 세지 않을 뿐 시험을 틀리게 하지 않는다.
	time.Sleep(500 * time.Millisecond)
	reclaimRunHelper(t, limited, "read", 0, "EDC_RECLAIM_FILE="+path)
	reclaimRunHelper(t, high, "anon", 3*time.Second)
	close(stop)
	done := <-finished
	if done.err != nil {
		t.Fatal(done.err)
	}
	report := reclaimReportFor(done.collection, "", uint64(os.Getpagesize()))
	rows := map[string]reclaimReportRow{}
	for _, row := range report.Rows {
		rows[row.Kind] = row
	}
	byCgroup := reclaimReportFor(done.collection, traceGroupByCgroup, uint64(os.Getpagesize()))
	for _, row := range byCgroup.Rows {
		t.Logf("cgroup %d: %d stalls %.3fms", *row.CgroupID, row.Stalls, row.TotalMS)
	}
	memcg, throttle := rows["memcg"], rows["throttle"]
	// 64MiB를 세 번 읽는 동안 16MiB 한도 안에서 page cache를 비워야 하므로 회수가 여러 번 생긴다.
	if memcg.Stalls == 0 || memcg.ReclaimedBytes < 32<<20 || memcg.TotalMS <= 0 {
		t.Errorf("memcg = %+v", memcg)
	}
	if throttle.Stalls == 0 || throttle.TotalMS <= 0 {
		t.Errorf("throttle = %+v", throttle)
	}
	for _, row := range report.Rows {
		t.Logf("%s %s: %d stalls, total %.3fms, max %.3fms, reclaimed %d bytes", row.Process, row.Kind, row.Stalls, row.TotalMS, row.MaxMS, row.ReclaimedBytes)
	}
}
