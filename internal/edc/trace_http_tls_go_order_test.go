//go:build linux

package edc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func goTLSOrderControl(phase uint32, socket, goroutine, frame uint64) []byte {
	sample := make([]byte, goTLSOrderSize)
	binary.LittleEndian.PutUint64(sample, 100)
	binary.LittleEndian.PutUint32(sample[8:], goTLSOrderRecord)
	binary.LittleEndian.PutUint32(sample[12:], phase)
	binary.LittleEndian.PutUint64(sample[16:], socket)
	binary.LittleEndian.PutUint64(sample[24:], goroutine)
	binary.LittleEndian.PutUint64(sample[32:], frame)
	return sample
}

func goTLSOrderSample(socket uint64, sent bool, payload string) []byte {
	sample := make([]byte, httpRecordPayloadOffset+len(payload))
	binary.LittleEndian.PutUint64(sample, 101)
	binary.LittleEndian.PutUint32(sample[8:], httpRecordType)
	binary.LittleEndian.PutUint64(sample[24:], socket)
	binary.LittleEndian.PutUint32(sample[32:], uint32(len(payload)))
	sample[38] = httpRecordDecrypted
	if sent {
		sample[38] |= httpRecordSent
	}
	copy(sample[httpRecordPayloadOffset:], payload)
	return sample
}

func goTLSOrderClose(socket uint64) []byte {
	sample := make([]byte, 32)
	binary.LittleEndian.PutUint32(sample[8:], tcpDestroyEventType)
	binary.LittleEndian.PutUint64(sample[24:], socket)
	return sample
}

func TestTraceTLSGoOrderReadAndCloseFollowWrite(t *testing.T) {
	order := newGoTLSOrder()
	begin, end := goTLSOrderControl(goTLSWriteBegin, 1, 2, 3), goTLSOrderControl(goTLSWriteEnd, 1, 2, 3)
	read := goTLSOrderSample(1, false, "HTTP/1.1 200 OK\r\n\r\n")
	close := goTLSOrderClose(1)
	write := goTLSOrderSample(1, true, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	for _, sample := range [][]byte{begin, read, close} {
		if len(order.add(sample)) != 0 {
			t.Fatal("Read/Close escaped before Write")
		}
	}
	read[httpRecordPayloadOffset] = 'X'
	if got := order.add(write); len(got) != 1 || !bytes.Equal(got[0], write) {
		t.Fatalf("Write was held: %v", got)
	}
	got := order.add(end)
	if len(got) != 2 || got[0][httpRecordPayloadOffset] != 'H' || !bytes.Equal(got[1], close) {
		t.Fatalf("Read/Close order or copied payload: %v", got)
	}
	if order.finish() != 0 || order.bytes != 0 || len(order.connections) != 0 {
		t.Fatalf("completed state leaked: %#v", order)
	}
	order.add(begin)
	if len(order.add(end)) != 0 || order.finish() != 0 {
		t.Fatal("connection or goroutine reuse inherited state")
	}
}

func TestTraceTLSGoOrderWaitsForEveryWriter(t *testing.T) {
	order := newGoTLSOrder()
	order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, 3))
	order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, 4))
	order.add(goTLSOrderSample(1, false, "HTTP/1.1 200 OK\r\n\r\n"))
	if len(order.add(goTLSOrderControl(goTLSWriteEnd, 1, 2, 4))) != 0 {
		t.Fatal("released Read before outer writer finished")
	}
	if len(order.add(goTLSOrderControl(goTLSWriteEnd, 1, 2, 3))) != 1 || order.finish() != 0 {
		t.Fatal("failed to release Read after all writers")
	}
}

func TestTraceTLSGoOrderLeavesOtherRecordsAlone(t *testing.T) {
	order := newGoTLSOrder()
	order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, 3))
	plain := goTLSOrderSample(1, false, "HTTP/1.1 200 OK\r\n\r\n")
	plain[38] = 0
	for _, sample := range [][]byte{plain, goTLSOrderSample(2, false, "other connection"), goTLSOrderClose(2), {1, 2, 3}} {
		if got := order.add(sample); len(got) != 1 || !bytes.Equal(got[0], sample) {
			t.Fatalf("changed unrelated record: %v", got)
		}
	}
}

func TestTraceTLSGoOrderMissingControlsStopTLS(t *testing.T) {
	for _, controls := range [][][]byte{
		{goTLSOrderControl(goTLSWriteEnd, 1, 2, 3)},
		{goTLSOrderControl(goTLSWriteBegin, 1, 2, 3), goTLSOrderControl(goTLSWriteEnd, 1, 2, 4)},
		{goTLSOrderControl(goTLSWriteBegin, 1, 2, 3), goTLSOrderControl(goTLSWriteBegin, 1, 2, 3)},
		{goTLSOrderControl(99, 1, 2, 3)},
		{goTLSOrderControl(goTLSWriteBegin, 0, 2, 3)},
		{goTLSOrderControl(goTLSWriteBegin, 1, 2, 3)[:12]},
	} {
		order := newGoTLSOrder()
		for _, sample := range controls {
			order.add(sample)
		}
		if !order.failed || len(order.add(goTLSOrderSample(1, true, "request"))) != 0 || len(order.add(goTLSOrderSample(1, false, "response"))) != 0 || order.finish() == 0 {
			t.Fatalf("missing controls hid corruption: %#v", order)
		}
	}
}

func TestTraceTLSGoOrderLimitsReportLoss(t *testing.T) {
	for _, limit := range []string{"connections", "writers", "bytes", "connection-bytes", "records"} {
		t.Run(limit, func(t *testing.T) {
			order := newGoTLSOrder()
			if limit == "connections" {
				for socket := uint64(1); socket <= goTLSOrderMaxConnections; socket++ {
					order.add(goTLSOrderControl(goTLSWriteBegin, socket, 2, 3))
				}
				order.add(goTLSOrderControl(goTLSWriteBegin, goTLSOrderMaxConnections+1, 2, 3))
			} else if limit == "writers" {
				for frame := uint64(1); frame <= goTLSOrderMaxWriters; frame++ {
					order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, frame))
				}
				order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, goTLSOrderMaxWriters+1))
			} else {
				order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, 3))
				sample := goTLSOrderSample(1, false, "response")
				switch limit {
				case "bytes":
					order.bytes = goTLSOrderMaxBytes
				case "connection-bytes":
					order.connections[1].bytes = goTLSOrderMaxConnectionBytes
				case "records":
					order.records = goTLSOrderMaxRecords
				}
				order.add(sample)
			}
			if !order.failed || order.finish() == 0 || order.bytes != 0 || len(order.connections) != 0 {
				t.Fatalf("ordering limit hid loss: %#v", order)
			}
		})
	}
}

func TestTraceTLSGoOrderUnfinishedWritesReportLoss(t *testing.T) {
	order := newGoTLSOrder()
	order.add(goTLSOrderControl(goTLSWriteBegin, 1, 2, 3))
	order.add(goTLSOrderSample(1, false, "response"))
	order.add(goTLSOrderClose(1))
	if order.finish() != 3 || order.bytes != 0 || len(order.connections) != 0 {
		t.Fatalf("unfinished write hid queued loss: %#v", order)
	}
}
