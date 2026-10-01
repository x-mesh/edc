//go:build linux

package edc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

var ioTracepoints = []string{"block_rq_insert", "block_rq_issue", "block_rq_complete", "block_rq_requeue"}

func ioTracePrerequisites() error {
	return traceBPFPrerequisites("trace io", func(kernel *btf.Spec) error {
		return traceTracepointsAvailable(kernel, ioTracepoints)
	})
}

func collectIOEvents(options ioTraceOptions, onEvent func(ioEvent) error, stop <-chan struct{}) (ioSummary, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return ioSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadIoEvents()
	if err != nil {
		return ioSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	var variables ioEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return ioSummary{}, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := variables.MinimumLatencyNs.Set(uint64(options.slow)); err != nil {
		return ioSummary{}, fmt.Errorf("set latency threshold: %w", err)
	}
	clockOffset, err := captureClockOffset()
	if err != nil {
		return ioSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	objects := ioEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return ioSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()
	links := make([]link.Link, 0, 4)
	for _, attachment := range []struct {
		name    string
		program *ebpf.Program
	}{
		{"block_rq_insert", objects.Insert}, {"block_rq_issue", objects.Issue}, {"block_rq_complete", objects.Complete}, {"block_rq_requeue", objects.Requeue},
	} {
		attached, err := link.AttachTracing(link.TracingOptions{Program: attachment.program, AttachType: ebpf.AttachTraceRawTp})
		if err != nil {
			closeCaptureLinks(links)
			return ioSummary{}, fmt.Errorf("attach tp_btf/%s: %w", attachment.name, err)
		}
		links = append(links, attached)
	}
	defer closeCaptureLinks(links)
	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return ioSummary{}, fmt.Errorf("open event ring: %w", err)
	}
	defer reader.Close()
	deadline := time.Now().Add(options.duration)
	readerDone := make(chan struct{})
	if stop != nil {
		go func() {
			select {
			case <-stop:
				_ = reader.Close()
			case <-readerDone:
			}
		}()
		defer close(readerDone)
	}
	var events uint64
	finish := func() (ioSummary, error) {
		summary := ioSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "io_summary", EventCount: events}
		var counters ioEventsIoCounters
		if err := objects.Counters.Lookup(uint32(0), &counters); err != nil {
			return ioSummary{}, fmt.Errorf("read I/O counters: %w", err)
		}
		summary.MapFull, summary.UnmatchedCompletion, summary.Incomplete, summary.Requeue, summary.LostEvents = counters.Values[0], counters.Values[1], counters.Values[2], counters.Values[3], counters.Values[4]
		return summary, nil
	}
	for {
		if options.duration > 0 && !time.Now().Before(deadline) {
			return finish()
		}
		wake := time.Now().Add(time.Second)
		if options.duration > 0 && deadline.Before(wake) {
			wake = deadline
		}
		reader.SetDeadline(wake)
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if errors.Is(err, os.ErrClosed) && traceStopRequested(stop) {
			return finish()
		}
		if err != nil {
			return ioSummary{}, err
		}
		event, ok := parseIORecord(record.RawSample, clockOffset)
		if !ok || !options.matches(event) {
			continue
		}
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return ioSummary{}, err
			}
		}
		events++
	}
}

func parseIORecord(sample []byte, clockOffset int64) (ioEvent, bool) {
	const size = 88
	if len(sample) < size {
		return ioEvent{}, false
	}
	var attribution string
	switch sample[80] {
	case 1:
		attribution = "insert"
	case 2:
		attribution = "issue"
	default:
		return ioEvent{}, false
	}
	operation := strings.TrimRight(string(sample[72:80]), "\x00")
	if strings.HasPrefix(operation, "R") {
		operation = "read"
	} else if strings.HasPrefix(operation, "W") {
		operation = "write"
	} else {
		operation = "unknown"
	}
	queue := float64(binary.LittleEndian.Uint64(sample[24:32])) / float64(time.Millisecond)
	service := float64(binary.LittleEndian.Uint64(sample[32:40])) / float64(time.Millisecond)
	total := float64(binary.LittleEndian.Uint64(sample[40:48])) / float64(time.Millisecond)
	dev := binary.LittleEndian.Uint32(sample[48:52])
	major, minor := dev>>20, dev&((1<<20)-1)
	bootTime := binary.LittleEndian.Uint64(sample[0:8])
	event := ioEvent{TimestampNS: uint64(int64(bootTime) + clockOffset), BootTimeNS: bootTime, CgroupID: binary.LittleEndian.Uint64(sample[8:16]), Bytes: binary.LittleEndian.Uint64(sample[16:24]), PID: binary.LittleEndian.Uint32(sample[52:56]), Process: strings.TrimRight(string(sample[56:72]), "\x00"), Operation: operation, Device: ioDevice(major, minor), Major: major, Minor: minor, Event: "io_complete", Attribution: attribution, QueueMS: &queue, ServiceMS: &service, TotalMS: &total}
	if attribution == "issue" {
		event.QueueMS = nil
	}
	return event, true
}
