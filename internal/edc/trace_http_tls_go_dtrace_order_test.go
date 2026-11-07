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
