package edc

import (
	"fmt"
	"sync"
)

const darwinTLSOutputMaxRecords = 4096
const darwinTLSOutputMaxBytes = 32 << 20

type darwinTLSRecordQueue struct {
	mu                 sync.Mutex
	items              []darwinTLSRecord
	head, count, bytes int
	ready              chan struct{}
}

func newDarwinTLSRecordQueue() *darwinTLSRecordQueue {
	return &darwinTLSRecordQueue{items: make([]darwinTLSRecord, 16), ready: make(chan struct{}, 1)}
}
func (q *darwinTLSRecordQueue) push(record darwinTLSRecord) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	size := cap(record.payload)
	if q.count >= darwinTLSOutputMaxRecords || q.bytes+size > darwinTLSOutputMaxBytes {
		return fmt.Errorf("macOS TLS output queue exceeds %d records or %d bytes", darwinTLSOutputMaxRecords, darwinTLSOutputMaxBytes)
	}
	if q.count == len(q.items) {
		size := min(2*len(q.items), darwinTLSOutputMaxRecords)
		items := make([]darwinTLSRecord, size)
		for i := 0; i < q.count; i++ {
			items[i] = q.items[(q.head+i)%len(q.items)]
		}
		q.items = items
		q.head = 0
	}
	q.items[(q.head+q.count)%len(q.items)] = record
	q.count++
	q.bytes += size
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return nil
}
func (q *darwinTLSRecordQueue) pop() (darwinTLSRecord, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.count == 0 {
		return darwinTLSRecord{}, false
	}
	record := q.items[q.head]
	q.items[q.head] = darwinTLSRecord{}
	q.head = (q.head + 1) % len(q.items)
	q.count--
	q.bytes -= cap(record.payload)
	if q.count > 0 {
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
	return record, true
}
func (q *darwinTLSRecordQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}
