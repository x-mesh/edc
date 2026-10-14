//go:build linux

package edc

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf/btf"
	"golang.org/x/sys/unix"
)

func TestIOTracepointsComeFromKernelBTF(t *testing.T) {
	spec := func(names ...string) *btf.Spec {
		types := make([]btf.Type, 0, len(names))
		for _, name := range names {
			types = append(types, &btf.Typedef{Name: "btf_trace_" + name, Type: &btf.Int{Name: "int", Size: 4}})
		}
		builder, err := btf.NewBuilder(types, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := builder.Marshal(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := btf.LoadSpecFromReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return loaded
	}
	if err := traceTracepointsAvailable(spec(ioTracepoints...), ioTracepoints); err != nil {
		t.Fatal(err)
	}
	err := traceTracepointsAvailable(spec("block_rq_insert", "block_rq_issue", "block_rq_complete"), ioTracepoints)
	if err == nil || !strings.Contains(err.Error(), "block_rq_requeue") {
		t.Fatalf("missing requeue = %v", err)
	}
}

func TestParseIORecordKeepsInsertIdentity(t *testing.T) {
	sample := make([]byte, 88)
	binary.LittleEndian.PutUint64(sample[0:8], 100)
	binary.LittleEndian.PutUint64(sample[8:16], 42)
	binary.LittleEndian.PutUint64(sample[16:24], 4096)
	binary.LittleEndian.PutUint64(sample[24:32], 2_000_000)
	binary.LittleEndian.PutUint64(sample[32:40], 3_000_000)
	binary.LittleEndian.PutUint64(sample[40:48], 5_000_000)
	binary.LittleEndian.PutUint32(sample[48:52], 8<<20)
	binary.LittleEndian.PutUint32(sample[52:56], 123)
	copy(sample[56:72], "writer")
	copy(sample[72:80], "WS")
	sample[80] = 1
	event, ok := parseIORecord(sample, 1_000)
	if !ok || event.PID != 123 || event.Process != "writer" || event.CgroupID != 42 || event.Device != "8:0" || event.Operation != "write" || *event.TotalMS != 5 || event.Attribution != "insert" || event.QueueMS == nil || *event.QueueMS != 2 {
		t.Fatalf("event = %+v", event)
	}
	if event.TimestampNS != 1_100 || event.BootTimeNS != 100 {
		t.Fatalf("timestamp = %d, boot time = %d", event.TimestampNS, event.BootTimeNS)
	}
}

func TestParseIORecordLeavesQueueEmptyWithoutInsert(t *testing.T) {
	sample := make([]byte, 88)
	binary.LittleEndian.PutUint64(sample[32:40], 3_000_000)
	binary.LittleEndian.PutUint64(sample[40:48], 3_000_000)
	copy(sample[72:80], "R")
	sample[80] = 2
	event, ok := parseIORecord(sample, 0)
	if !ok || event.Attribution != "issue" || event.QueueMS != nil || *event.ServiceMS != 3 || *event.TotalMS != 3 {
		t.Fatalf("event = %+v", event)
	}
}

func TestParseIORecordRejectsShortSample(t *testing.T) {
	if _, ok := parseIORecord(make([]byte, 87), 0); ok {
		t.Fatal("short sample was accepted")
	}
}

// 기준을 1시간으로 두면 모든 요청이 기준보다 빨리 끝난다. 그 요청은 event로 나오지 않고 omitted_below_threshold로만 남는다.
// write-back cache가 있는 장치에서 기본 기준 1ms로 재면 쓰기가 모두 이렇게 빠져, 이 수가 없으면 쓰기가 없던 것처럼 보인다.
func TestTraceIOCountsTheRequestsBelowTheThreshold(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace io needs BPF capabilities")
	}
	if err := ioTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	// tmpfs에 쓰면 block 요청이 생기지 않는다. 시험 binary가 도는 작업 directory는 disk에 있다.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || fs.Type == unix.TMPFS_MAGIC {
		t.Skipf("%s is not on a block device", dir)
	}
	stop := make(chan struct{})
	type result struct {
		summary ioSummary
		err     error
	}
	finished := make(chan result, 1)
	var events uint64
	go func() {
		summary, err := collectIOEvents(ioTraceOptions{slow: time.Hour}, func(ioEvent) error { events++; return nil }, stop)
		finished <- result{summary, err}
	}()
	time.Sleep(time.Second)
	file, err := os.CreateTemp(dir, "edc-io-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(bytes.Repeat([]byte{1}, 1<<20)); err != nil {
		t.Fatal(err)
	}
	// fsync는 쓰기 요청이 끝난 뒤에 돌아오므로, 그 완료는 trace를 멈추기 전에 BPF를 지난다.
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	file.Close()
	close(stop)
	done := <-finished
	if done.err != nil {
		t.Fatal(done.err)
	}
	if done.summary.OmittedBelowThreshold == 0 || events != 0 || done.summary.EventCount != 0 {
		t.Fatalf("omitted %d, events %d, summary %+v", done.summary.OmittedBelowThreshold, events, done.summary)
	}
	t.Logf("omitted_below_threshold %d", done.summary.OmittedBelowThreshold)
}
