//go:build linux

package edc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/cilium/ebpf/btf"
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
	if err := ioTracepointsAvailable(spec(ioTracepoints...)); err != nil {
		t.Fatal(err)
	}
	err := ioTracepointsAvailable(spec("block_rq_insert", "block_rq_issue", "block_rq_complete"))
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
