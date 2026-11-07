//go:build darwin

package edc

import (
	"fmt"
	"github.com/ebitengine/purego"
	"testing"
	"time"
)

func TestDarwinTLSFDMetadataSurvivesCloseAndReuse(t *testing.T) {
	c := newDarwinTLSAddressCache(42)
	c.lookup = func(uint32, int32) (string, string) { return "", "" }
	for _, r := range []darwinTLSRecord{
		{pid: 42, fd: 7, fdMeta: 2, fdKind: 2, address: "127.0.0.1:443"},
		{pid: 42, fd: 7, fdMeta: 1, object: 10},
		{pid: 42, fd: 7, fdMeta: 2, fdKind: 1, address: "127.0.0.1:50000"},
		{pid: 42, fd: 7, fdMeta: 3},
		{pid: 42, fd: 7, fdMeta: 2, fdKind: 2, address: "127.0.0.1:8443"},
		{pid: 42, fd: 7, fdMeta: 2, fdKind: 1, address: "127.0.0.1:50001"},
		{pid: 42, fd: 7, fdMeta: 1, object: 20},
	} {
		if err := c.metadata(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		object        uint64
		local, remote string
	}{{10, "127.0.0.1:50000", "127.0.0.1:443"}, {20, "127.0.0.1:50001", "127.0.0.1:8443"}} {
		r := darwinTLSRecord{pid: 42, object: test.object, fd: 7}
		if err := c.decorate(&r); err != nil {
			t.Fatal(err)
		}
		if r.source != test.local || r.destination != test.remote {
			t.Fatalf("wrong socket generation: %#v", r)
		}
	}
	c.forget(10)
	c.forget(20)
	if len(c.objects) != 0 || len(c.references) != 0 || len(c.retained) != 0 {
		t.Fatalf("address history leaked: %#v", c)
	}
	if err := c.metadata(darwinTLSRecord{pid: 43, fd: 7, fdMeta: 1, object: 10}); err == nil {
		t.Fatal("cross-process metadata accepted")
	}
}
func TestDarwinTLSFDAddressDecoder(t *testing.T) {
	var decoder darwinTLSDecoder
	r, err := decoder.line("EDC_FD_ADDR 42 7 2 2 443 16777343 0 0 0")
	if err != nil || r.address != "127.0.0.1:443" || r.fdMeta != 2 {
		t.Fatal(r, err)
	}
	r, err = decoder.line("EDC_FD_ADDR 42 7 1 30 8443 0 0 0 16777216")
	if err != nil || r.address != "[::1]:8443" {
		t.Fatal(r, err)
	}
	for _, line := range []string{"EDC_FD_ADDR 0 7 0 2 443 1 0 0 0", "EDC_FD_ADDR 42 7 4 2 443 1 0 0 0", "EDC_FD_ADDR 42 7 0 2 65536 1 0 0 0", "EDC_FD_ADDR 42 7 0 30 443 4294967296 0 0 0", "EDC_FD_RESET 42 -1", "EDC_FD_BIND 42 0 7"} {
		if _, err := decoder.line(line); err == nil {
			t.Fatalf("accepted %q", line)
		}
	}
}
func TestDarwinTLSKernelFDExports(t *testing.T) {
	library, err := purego.Dlopen("/usr/lib/system/libsystem_kernel.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(library)
	for _, name := range []string{"read", "write", "readv", "writev", "recvfrom", "sendto", "recvmsg", "sendmsg", "connect", "bind", "getsockname", "getpeername", "accept", "socket", "close", "dup2"} {
		if address, err := purego.Dlsym(library, name); err != nil || address == 0 {
			t.Fatalf("socket symbol %s: address=%x error=%v", name, address, err)
		}
	}
}

func TestDarwinTLSProcessExitDrainsQueuedRecordsBeforeCleanup(t *testing.T) {
	done := make(chan struct{})
	close(done)
	worker := &darwinTLSWorker{done: done, announced: true}
	worker.ready.Store(true)
	capture := &darwinTLSCapture{records: newDarwinTLSRecordQueue(), workers: map[uint32]*darwinTLSWorker{42: worker}, notices: newTraceTLSNoticeBuffer()}
	var sent, received uint32
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("GET /%d HTTP/1.1\r\nHost: fixture\r\n\r\n", i))
		if err := capture.records.push(darwinTLSRecord{pid: 42, object: 1, sent: true, fd: -1, offset: sent, payload: data, size: uint64(len(data)), wallNS: uint64(1000 + i)}); err != nil {
			t.Fatal(err)
		}
		sent += uint32(len(data))
	}
	for i := 0; i < 200; i++ {
		data := []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		if err := capture.records.push(darwinTLSRecord{pid: 42, object: 1, fd: -1, offset: received, payload: data, size: uint64(len(data)), wallNS: uint64(3000 + i)}); err != nil {
			t.Fatal(err)
		}
		received += uint32(len(data))
	}
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	responses := 0
	emit := func(events []captureEvent) error {
		for _, event := range events {
			if event.Status == 200 {
				if event.Path != fmt.Sprintf("/%d", responses) {
					return fmt.Errorf("response %d matched %q", responses, event.Path)
				}
				responses++
			}
		}
		return nil
	}
	now := time.Now()
	if err := capture.poll(tracker, emit, now); err != nil {
		t.Fatal(err)
	}
	for capture.records.len() > 0 {
		if err := capture.drain(tracker, emit, now); err != nil {
			t.Fatal(err)
		}
	}
	if responses != 200 {
		t.Fatalf("responses=%d", responses)
	}
}

func TestDarwinTLSOutputQueueAcceptsBurstBeforeConsumerRuns(t *testing.T) {
	capture, err := newDarwinTLSCapture(traceScope{})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.stop()
	worker := &darwinTLSWorker{}
	for i := 0; i < 512; i++ {
		if !worker.enqueue(capture.records, darwinTLSRecord{pid: 42, object: 1, offset: uint32(i), payload: []byte("burst")}) {
			t.Fatalf("burst stopped after %d records: %v", i, worker.err)
		}
	}
	for i := 0; i < 512; i++ {
		record, ok := capture.records.pop()
		if !ok || record.offset != uint32(i) {
			t.Fatalf("record %d: %#v available=%t", i, record, ok)
		}
	}
	if capture.records.len() != 0 || capture.records.bytes != 0 {
		t.Fatal("burst records retained")
	}

}

func TestDarwinTLSAddressRolesPreserveServerPort(t *testing.T) {
	c := newDarwinTLSAddressCache(42)
	c.lookup = func(uint32, int32) (string, string) { return "", "" }
	var decoder darwinTLSDecoder
	for _, line := range []string{
		"EDC_FD_ADDR 42 8 0 2 49197 16777343 0 0 0",
		"EDC_FD_ADDR 42 8 1 2 49199 16777343 0 0 0",
		"EDC_FD_BIND 42 100 8",
	} {
		r, err := decoder.line(line)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.metadata(*r); err != nil {
			t.Fatal(err)
		}
	}
	request := []byte("GET /mapped HTTP/1.1\r\nHost: fixture\r\n\r\n")
	r := darwinTLSRecord{pid: 42, object: 100, fd: 8, sent: true, payload: request, size: uint64(len(request)), wallNS: 1000}
	if err := c.decorate(&r); err != nil {
		t.Fatal(err)
	}
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	events, unmapped := tracker.addDarwinTLS(r, 49197, time.Now())
	if unmapped || len(events) != 1 || events[0].Path != "/mapped" || r.source != "127.0.0.1:49199" || r.destination != "127.0.0.1:49197" {
		t.Fatalf("record=%#v events=%#v unmapped=%t", r, events, unmapped)
	}
}

func TestDarwinTLSFDCacheDoesNotAccumulateClosedDescriptors(t *testing.T) {
	c := newDarwinTLSAddressCache(42)
	c.lookup = func(uint32, int32) (string, string) { return "", "" }
	for i := 0; i < darwinTLSFDAddressLimit+1; i++ {
		fd := int32(i)
		for _, r := range []darwinTLSRecord{{pid: 42, fd: fd, fdMeta: 2, fdKind: 2, address: "127.0.0.1:443"}, {pid: 42, fd: fd, fdMeta: 1, object: 1}, {pid: 42, fd: fd, fdMeta: 3}} {
			if err := c.metadata(r); err != nil {
				t.Fatalf("descriptor %d: %v", i, err)
			}
		}
		r := darwinTLSRecord{pid: 42, fd: fd, object: 1}
		if err := c.decorate(&r); err != nil || r.destination != "127.0.0.1:443" {
			t.Fatalf("closed descriptor %d lost its active object: %#v %v", fd, r, err)
		}
		c.forget(1)
	}
	if len(c.fds) != 0 || len(c.retained) != 0 || len(c.objects) != 0 || len(c.references) != 0 {
		t.Fatalf("closed cache entries remain: descriptors=%d history=%d objects=%d references=%d", len(c.fds), len(c.retained), len(c.objects), len(c.references))
	}
}
