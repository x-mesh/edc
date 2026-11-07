package edc

import (
	"strings"
	"testing"
)

func TestDarwinTLSGoOrderKeepsResponseAfterWriteAndDefersClose(t *testing.T) {
	o := newDarwinTLSGoOrder()
	begin := darwinTLSRecord{pid: 42, object: 1, goPhase: 1, goroutine: 3, frame: 4, sent: true}
	read := begin
	read.sent = false
	read.goroutine = 5
	response := darwinTLSRecord{pid: 42, object: 1, payload: []byte("response")}
	close := darwinTLSRecord{pid: 42, object: 1, closed: true}
	for _, r := range []darwinTLSRecord{begin, read, response, close} {
		out, err := o.add(r)
		if err != nil || len(out) != 0 {
			t.Fatalf("out=%#v error=%v", out, err)
		}
	}
	request := darwinTLSRecord{pid: 42, object: 1, sent: true, payload: []byte("request")}
	out, err := o.add(request)
	if err != nil || len(out) != 1 || string(out[0].payload) != "request" {
		t.Fatal(out, err)
	}
	begin.goPhase = 2
	out, err = o.add(begin)
	if err != nil || len(out) != 1 || string(out[0].payload) != "response" {
		t.Fatal(out, err)
	}
	read.goPhase = 2
	out, err = o.add(read)
	if err != nil || len(out) != 1 || !out[0].closed {
		t.Fatal(out, err)
	}
	if len(o.calls) != 0 || len(o.connections) != 0 || o.bytes != 0 || o.records != 0 {
		t.Fatalf("state=%#v", o)
	}
}
func TestDarwinTLSGoOrderBoundsAndFinish(t *testing.T) {
	begin := darwinTLSRecord{pid: 42, object: 1, goPhase: 1, goroutine: 3, frame: 4, sent: true}
	o := newDarwinTLSGoOrder()
	o.add(begin)
	if _, err := o.add(begin); err == nil {
		t.Fatal("duplicate begin accepted")
	}
	if _, err := o.finish(); err == nil {
		t.Fatal("unfinished write accepted")
	}
	r := darwinTLSRecord{pid: 42, object: 1, payload: make([]byte, 0, darwinTLSGoOrderConnectionBytes+1)}
	if _, err := o.add(r); err == nil {
		t.Fatal("retained capacity exceeds connection limit")
	}
	end := begin
	end.object = 2
	end.goPhase = 2
	if _, err := o.add(end); err == nil {
		t.Fatal("unmatched end accepted")
	}
	o = newDarwinTLSGoOrder()
	begin.sent = false
	o.add(begin)
	if _, err := o.finish(); err != nil {
		t.Fatalf("idle read prevents normal stop: %v", err)
	}
}
func TestDarwinTLSGoControlDecoder(t *testing.T) {
	var d darwinTLSDecoder
	for _, phase := range []string{"BEGIN", "END"} {
		r, err := d.line("EDC_GO_" + phase + " 42 123 1 456 789")
		if err != nil || r == nil || r.object != 123 || r.goroutine != 456 || r.frame != 789 || !r.sent || r.goPhase == 0 {
			t.Fatal(r, err)
		}
	}
	for _, line := range []string{"EDC_GO_BEGIN 0 1 1 1 1", "EDC_GO_BEGIN 42 1 2 1 1", "EDC_GO_END 42 1 0 0 1", "EDC_GO_BEGIN 42 1 1 2", "EDC_GO_CONCURRENT"} {
		if _, err := d.line(line); err == nil {
			t.Fatalf("accepted %q", line)
		}
	}
	if _, err := d.line("EDC_TLS 42 100 1 1 0 0 1 1 -1 client"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.line("EDC_GO_BEGIN 42 1 1 1 1"); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatal("control interleaving accepted", err)
	}
}

func TestDarwinTLSGoOrderRejectsDistinctOverlappingCalls(t *testing.T) {
	for _, sent := range []bool{false, true} {
		o := newDarwinTLSGoOrder()
		first := darwinTLSRecord{object: 1, goroutine: 2, frame: 3, sent: sent, goPhase: 1}
		if _, err := o.add(first); err != nil {
			t.Fatal(err)
		}
		second := first
		second.goroutine++
		second.frame++
		if _, err := o.add(second); err == nil || !strings.Contains(err.Error(), "overlapping") {
			t.Fatalf("sent=%t accepted concurrent call: %v", sent, err)
		}
		first.goPhase = 2
		if _, err := o.add(first); err != nil {
			t.Fatal(err)
		}
		if len(o.calls) != 0 || len(o.connections) != 0 {
			t.Fatal("rejected call changed the accepted call lifecycle")
		}
	}
}

func TestDarwinTLSGoOrderConnectionChurn(t *testing.T) {
	order := newDarwinTLSGoOrder()
	for round := 0; round < 1000; round++ {
		for object := uint64(1); object <= 16; object++ {
			records := []darwinTLSRecord{
				{object: object, goPhase: 1, goroutine: object * 2, frame: 1, sent: true},
				{object: object, goPhase: 1, goroutine: object*2 + 1, frame: 2},
				{object: object, payload: []byte("response")},
				{object: object, closed: true},
			}
			for _, record := range records {
				if out, err := order.add(record); err != nil || len(out) != 0 {
					t.Fatalf("round %d: %v %v", round, out, err)
				}
			}
		}
		for object := uint64(16); object > 0; object-- {
			out, err := order.add(darwinTLSRecord{object: object, sent: true, payload: []byte("request")})
			if err != nil || len(out) != 1 || string(out[0].payload) != "request" {
				t.Fatalf("request: %v %v", out, err)
			}
			out, err = order.add(darwinTLSRecord{object: object, goPhase: 2, goroutine: object * 2, frame: 1, sent: true})
			if err != nil || len(out) != 1 || string(out[0].payload) != "response" {
				t.Fatalf("response: %v %v", out, err)
			}
			out, err = order.add(darwinTLSRecord{object: object, goPhase: 2, goroutine: object*2 + 1, frame: 2})
			if err != nil || len(out) != 1 || !out[0].closed {
				t.Fatalf("close: %v %v", out, err)
			}
		}
		if len(order.connections)+len(order.calls)+order.bytes+order.records != 0 {
			t.Fatalf("round %d retained TLS calls", round)
		}
	}
}

func TestDarwinTLSGoOrderDuplicateDiagnostics(t *testing.T) {
	order := newDarwinTLSGoOrder()
	begin := darwinTLSRecord{object: 10, goroutine: 20, frame: 64, goPhase: 1, sent: true}
	if _, err := order.add(begin); err != nil {
		t.Fatal(err)
	}
	if _, err := order.add(begin); err == nil || !strings.Contains(err.Error(), "first_record=1 duplicate_record=2") || !strings.Contains(err.Error(), "frame=64 sent=true") {
		t.Fatalf("duplicate diagnostic: %v", err)
	}
}

func TestDarwinTLSGoConcurrentDiagnostic(t *testing.T) {
	var decoder darwinTLSDecoder
	line := "EDC_GO_CONCURRENT pid=42 object=100 direction=0 goroutine=200 frame=64 owner=300 owner_frame=128 fd=7"
	if _, err := decoder.line(line); err == nil || !strings.Contains(err.Error(), line) {
		t.Fatalf("missing overlap context: %v", err)
	}
}

func TestDarwinTLSGoWriteSnapshotConfirmsOnlyReturnedBytes(t *testing.T) {
	order := newDarwinTLSGoOrder()
	begin := darwinTLSRecord{pid: 42, object: 1, goroutine: 2, frame: 64, sent: true, goPhase: 1}
	if _, err := order.add(begin); err != nil {
		t.Fatal(err)
	}
	for i, data := range []string{"hello", " world"} {
		r := darwinTLSRecord{pid: 42, object: 1, sent: true, goPhase: 3, offset: 100 + uint32(i*5), wallNS: 100, payload: []byte(data), size: uint64(len(data))}
		if out, err := order.add(r); err != nil || len(out) != 0 {
			t.Fatalf("unconfirmed write escaped: %v %v", out, err)
		}
	}
	read := darwinTLSRecord{pid: 42, object: 1, payload: []byte("response")}
	if out, err := order.add(read); err != nil || len(out) != 0 {
		t.Fatal(out, err)
	}
	result := begin
	result.goPhase = 4
	result.size = 7
	result.offset = 100
	result.fd = 9
	out, err := order.add(result)
	if err != nil || len(out) != 2 || string(out[0].payload) != "hello" || string(out[1].payload) != " w" || out[0].offset != 100 || out[1].offset != 105 || out[1].size != 2 || out[0].fd != 9 || out[0].goPhase != 0 {
		t.Fatalf("confirmed=%+v err=%v", out, err)
	}
	begin.goPhase = 2
	out, err = order.add(begin)
	if err != nil || len(out) != 1 || string(out[0].payload) != "response" {
		t.Fatalf("response order: %v %v", out, err)
	}
	if order.bytes != 0 || order.records != 0 || len(order.calls) != 0 || len(order.connections) != 0 {
		t.Fatal("snapshot retained after return")
	}
}

func TestDarwinTLSGoWriteSnapshotFailureAndLimits(t *testing.T) {
	for _, actual := range []uint64{0, 4} {
		order := newDarwinTLSGoOrder()
		begin := darwinTLSRecord{object: 1, goroutine: 2, frame: 64, sent: true, goPhase: 1}
		order.add(begin)
		if _, err := order.add(darwinTLSRecord{object: 1, sent: true, goPhase: 3, payload: []byte("abc")}); err != nil {
			t.Fatal(err)
		}
		result := begin
		result.goPhase = 4
		result.size = actual
		out, err := order.add(result)
		if actual == 0 {
			if err != nil || len(out) != 0 || order.bytes != 0 || order.records != 0 {
				t.Fatal(out, err)
			}
		} else if err == nil {
			t.Fatal("return exceeds captured snapshot")
		}
	}
	order := newDarwinTLSGoOrder()
	if _, err := order.add(darwinTLSRecord{object: 1, sent: true, goPhase: 3, payload: []byte("abc")}); err == nil {
		t.Fatal("orphan write snapshot accepted")
	}
	order.add(darwinTLSRecord{object: 1, goroutine: 2, frame: 64, sent: true, goPhase: 1})
	if _, err := order.add(darwinTLSRecord{object: 1, sent: true, goPhase: 3, payload: make([]byte, 0, darwinTLSGoOrderConnectionBytes+1)}); err == nil {
		t.Fatal("unbounded write snapshot")
	}
	if _, err := order.finish(); err == nil {
		t.Fatal("unfinished write published")
	}
}

func TestDarwinTLSGoWriteSnapshotDecoder(t *testing.T) {
	var decoder darwinTLSDecoder
	for _, line := range []string{"EDC_GO_WRITE 42 100 1 1 0 0 3 3 7 client", "0: 61 62 63"} {
		if _, err := decoder.line(line); err != nil {
			t.Fatal(err)
		}
	}
	r, err := decoder.line("EDC_END")
	if err != nil || r == nil || r.goPhase != 3 || string(r.payload) != "abc" {
		t.Fatalf("snapshot=%+v err=%v", r, err)
	}
	r, err = decoder.line("EDC_GO_WRITE_RESULT 42 1 2 64 2 100 7")
	if err != nil || r == nil || r.goPhase != 4 || r.size != 2 || r.offset != 100 || r.fd != 7 {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	for _, line := range []string{"EDC_GO_WRITE_RESULT 42 1 2 64 2 100 -2", "EDC_GO_WRITE_RESULT 0 1 2 64 2 100 7", "EDC_GO_WRITE_RESULT 42 1 2"} {
		if _, err := decoder.line(line); err == nil {
			t.Fatalf("invalid result accepted: %s", line)
		}
	}
}
