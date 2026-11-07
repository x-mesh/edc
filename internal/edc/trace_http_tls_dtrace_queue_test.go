package edc

import (
	"sync"
	"testing"
)

func TestDarwinTLSOutputQueueWrapAndGrowth(t *testing.T) {
	q := newDarwinTLSRecordQueue()
	for i := 0; i < 16; i++ {
		if err := q.push(darwinTLSRecord{offset: uint32(i), payload: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		r, ok := q.pop()
		if !ok || r.offset != uint32(i) {
			t.Fatal(r, ok)
		}
	}
	for i := 16; i < 512; i++ {
		if err := q.push(darwinTLSRecord{offset: uint32(i), payload: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 8; i < 512; i++ {
		r, ok := q.pop()
		if !ok || r.offset != uint32(i) {
			t.Fatalf("FIFO %d: %#v available=%t", i, r, ok)
		}
	}
	if q.len() != 0 || q.bytes != 0 {
		t.Fatalf("records=%d bytes=%d", q.len(), q.bytes)
	}
	for _, r := range q.items {
		if r.payload != nil {
			t.Fatal("queue retains consumed payload")
		}
	}
}
func TestDarwinTLSOutputQueueBounds(t *testing.T) {
	q := newDarwinTLSRecordQueue()
	if err := q.push(darwinTLSRecord{payload: make([]byte, 0, darwinTLSOutputMaxBytes+1)}); err == nil {
		t.Fatal("retained payload capacity bypasses byte limit")
	}
	if q.len() != 0 || q.bytes != 0 {
		t.Fatal("rejected payload changed accounting")
	}
	for i := 0; i < darwinTLSOutputMaxRecords; i++ {
		if err := q.push(darwinTLSRecord{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.push(darwinTLSRecord{}); err == nil {
		t.Fatal("zero-length records bypass record limit")
	}
}
func TestDarwinTLSOutputQueueWakesAndPreservesConcurrentFIFO(t *testing.T) {
	q := newDarwinTLSRecordQueue()
	const producers = 4
	const perProducer = 1000
	var writers sync.WaitGroup
	for pid := uint32(1); pid <= producers; pid++ {
		writers.Add(1)
		go func(pid uint32) {
			defer writers.Done()
			for i := 0; i < perProducer; i++ {
				if err := q.push(darwinTLSRecord{pid: pid, offset: uint32(i), payload: []byte{1}}); err != nil {
					t.Error(err)
					return
				}
			}
		}(pid)
	}
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	defer writers.Wait()
	expected := map[uint32]uint32{}
	consumed := 0
	for consumed < producers*perProducer {
		select {
		case <-q.ready:
		case <-done:
			if q.len() == 0 {
				t.Fatalf("only %d records delivered", consumed)
			}
		}
		for {
			r, ok := q.pop()
			if !ok {
				break
			}
			if r.offset != expected[r.pid] {
				t.Fatalf("PID %d: sequence=%d expected=%d", r.pid, r.offset, expected[r.pid])
			}
			expected[r.pid]++
			consumed++
		}
	}
	writers.Wait()
	if q.len() != 0 || q.bytes != 0 {
		t.Fatal("concurrent delivery retained records")
	}
}
