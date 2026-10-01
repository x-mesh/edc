package edc

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func ioFixtureEvents() []ioEvent {
	queue1, service1, total1 := 1.0, 2.0, 3.0
	queue2, service2, total2 := 4.0, 5.0, 9.0
	return []ioEvent{{Process: "postgres", Device: "8:0", CgroupID: 7, Operation: "write", Bytes: 4096, QueueMS: &queue1, ServiceMS: &service1, TotalMS: &total1}, {Process: "nginx", Device: "8:1", CgroupID: 8, Operation: "read", Bytes: 512, QueueMS: &queue2, ServiceMS: &service2, TotalMS: &total2}}
}

func TestIOGroupedReportHasRowsAndLatencyJSON(t *testing.T) {
	report := ioGroupedReportFor(ioFixtureEvents(), ioSummary{EventCount: 2}, traceGroupByDevice)
	if report.GroupBy != traceGroupByDevice || len(report.Rows) != 2 || report.Rows[0].Group != "8:0" || report.Rows[0].WriteOps != 1 || report.Rows[1].ReadOps != 1 {
		t.Fatalf("report = %+v", report)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	queue, ok := value["queue"].(map[string]any)
	if !ok || queue["average_ms"] != 2.5 || queue["p95_ms"] != 4.0 || queue["max_ms"] != 4.0 {
		t.Fatalf("queue JSON = %#v", value["queue"])
	}
	if rows, ok := value["rows"].([]any); !ok || len(rows) != 2 {
		t.Fatalf("rows JSON = %#v", value["rows"])
	}
}

func TestIOReportParityWithRawEvents(t *testing.T) {
	events := ioFixtureEvents()
	report := ioReportFor(events, ioSummary{EventCount: uint64(len(events))})
	if report.EventCount != uint64(len(events)) || report.Bytes != events[0].Bytes+events[1].Bytes || report.ReadOps != 1 || report.WriteOps != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestIOLatencyReportUsesP95AndOperationCounts(t *testing.T) {
	values := []ioEvent{}
	for i := 1; i <= 20; i++ {
		value := float64(i)
		operation := "read"
		if i%2 == 0 {
			operation = "write"
		}
		values = append(values, ioEvent{Operation: operation, Bytes: 512, QueueMS: &value, ServiceMS: &value, TotalMS: &value})
	}
	report := ioReportFor(values, ioSummary{EventCount: 20})
	if report.ReadOps != 10 || report.WriteOps != 10 || report.Bytes != 10240 {
		t.Fatalf("report = %+v", report)
	}
	if report.Total.P95MS == nil || *report.Total.P95MS != 19 || report.Total.MaxMS == nil || *report.Total.MaxMS != 20 {
		t.Fatalf("latency = %+v", report.Total)
	}
}

func TestTraceIORejectsIrrelevantOptions(t *testing.T) {
	previous := os.Stderr
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = file
	t.Cleanup(func() { os.Stderr = previous; file.Close() })
	if code := runIOTrace([]string{"--port", "80"}); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if code := runIOTrace([]string{"--group-by", "target"}); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("group-by")) {
		t.Fatalf("stderr = %q", output)
	}
}

func TestTraceIORejectsANonPositiveSlowThreshold(t *testing.T) {
	for _, value := range []string{"0", "-1ms"} {
		stderr := captureTraceStderr(t, func() {
			if code := runIOTrace([]string{"--slow", value}); code != 2 {
				t.Fatalf("--slow %s code = %d", value, code)
			}
		})
		if !bytes.Contains([]byte(stderr), []byte("--slow")) {
			t.Fatalf("--slow %s stderr = %q", value, stderr)
		}
	}
}

func TestIOOptionsMatchProcessDeviceAndContainer(t *testing.T) {
	options := ioTraceOptions{process: "postgres", device: "8:0", container: &traceContainer{cgroups: map[uint64]bool{42: true}}}
	if !options.matches(ioEvent{Process: "postgres", Device: "8:0", CgroupID: 42}) {
		t.Fatal("matching event was rejected")
	}
	if options.matches(ioEvent{Process: "postgres", Device: "8:1", CgroupID: 42}) {
		t.Fatal("wrong device was accepted")
	}
	if !validIOGroupBy(traceGroupByCgroup) || validIOGroupBy("target") {
		t.Fatal("group validation is wrong")
	}
	if ioLatencyThreshold != time.Millisecond {
		t.Fatalf("threshold = %s", ioLatencyThreshold)
	}
}
