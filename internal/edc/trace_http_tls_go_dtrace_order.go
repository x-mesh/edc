package edc

import "fmt"

const darwinTLSGoOrderBytes = 32 << 20
const darwinTLSGoOrderConnectionBytes = 4 << 20
const darwinTLSGoOrderRecords = 4096
const darwinTLSGoOrderCalls = 10240

type darwinTLSGoCall struct {
	object, goroutine, frame uint64
	sent                     bool
}
type darwinTLSGoConnection struct {
	active, writers, bytes int
	reads, closes          []darwinTLSRecord
	writes                 []darwinTLSRecord
}
type darwinTLSGoObservation struct {
	serial uint64
	key    darwinTLSGoCall
	phase  uint8
	closed bool
	size   uint64
	offset uint32
}

type darwinTLSGoOrder struct {
	serial         uint64
	recent         [16]darwinTLSGoObservation
	calls          map[darwinTLSGoCall]uint64
	connections    map[uint64]*darwinTLSGoConnection
	bytes, records int
}

func newDarwinTLSGoOrder() *darwinTLSGoOrder {
	return &darwinTLSGoOrder{calls: map[darwinTLSGoCall]uint64{}, connections: map[uint64]*darwinTLSGoConnection{}}
}
func (o *darwinTLSGoOrder) add(r darwinTLSRecord) ([]darwinTLSRecord, error) {
	c := o.connections[r.object]
	key := darwinTLSGoCall{r.object, r.goroutine, r.frame, r.sent}
	o.serial++
	o.recent[(o.serial-1)%uint64(len(o.recent))] = darwinTLSGoObservation{o.serial, key, r.goPhase, r.closed, r.size, r.offset}
	if r.goPhase == 1 {
		if began, exists := o.calls[key]; exists {
			return nil, fmt.Errorf("duplicate Go TLS call begin: object=%x goroutine=%x frame=%d sent=%t first_record=%d duplicate_record=%d active=%d writers=%d recent=%+v", r.object, r.goroutine, r.frame, r.sent, began, o.serial, c.active, c.writers, o.recent)
		}
		if len(o.calls) >= darwinTLSGoOrderCalls || c == nil && len(o.connections) >= pktapHTTPFlowLimit {
			return nil, fmt.Errorf("Go TLS active call limit exceeded")
		}
		if c != nil && (r.sent && c.writers != 0 || !r.sent && c.active-c.writers != 0) {
			return nil, fmt.Errorf("overlapping Go TLS calls on one connection and direction are unsupported")
		}
		if c == nil {
			c = &darwinTLSGoConnection{}
			o.connections[r.object] = c
		}
		c.active++
		if r.sent {
			c.writers++
		}
		o.calls[key] = o.serial
		return nil, nil
	}
	if r.goPhase == 3 {
		if c == nil || c.writers != 1 || !r.sent {
			return nil, fmt.Errorf("Go TLS write snapshot has no active writer")
		}
		size := cap(r.payload)
		if o.records >= darwinTLSGoOrderRecords || o.bytes+size > darwinTLSGoOrderBytes || c.bytes+size > darwinTLSGoOrderConnectionBytes {
			return nil, fmt.Errorf("Go TLS write snapshot limit exceeded")
		}
		if len(c.writes) > 0 {
			previous := c.writes[len(c.writes)-1]
			if r.offset != previous.offset+uint32(len(previous.payload)) {
				return nil, fmt.Errorf("Go TLS write snapshot has a gap")
			}
		}
		c.writes = append(c.writes, r)
		c.bytes += size
		o.bytes += size
		o.records++
		return nil, nil
	}
	if r.goPhase == 4 {
		if _, exists := o.calls[key]; !exists || c == nil || c.writers != 1 {
			return nil, fmt.Errorf("Go TLS write result has no active call")
		}
		if len(c.writes) > 0 && c.writes[0].offset != r.offset {
			return nil, fmt.Errorf("Go TLS write snapshot offset changed before return")
		}
		captured := 0
		for _, chunk := range c.writes {
			captured += len(chunk.payload)
		}
		if uint64(captured) < min(r.size, uint64(httpMessageMax)) {
			return nil, fmt.Errorf("Go TLS write result exceeds its snapshot")
		}
		var output []darwinTLSRecord
		left := r.size
		for _, chunk := range c.writes {
			if left == 0 {
				break
			}
			n := min(uint64(len(chunk.payload)), left)
			chunk.payload = chunk.payload[:n]
			chunk.size = n
			chunk.fd = r.fd
			chunk.goPhase = 0
			chunk.offset = r.offset + uint32(r.size-left)
			if r.size > httpMessageMax {
				chunk.size = r.size
			}
			output = append(output, chunk)
			left -= n
		}
		o.release(c.writes)
		c.writes = nil
		return output, nil
	}
	if r.goPhase == 2 {
		if _, exists := o.calls[key]; !exists || c == nil {
			return nil, fmt.Errorf("Go TLS call end has no begin")
		}
		if r.sent && len(c.writes) > 0 {
			return nil, fmt.Errorf("Go TLS write ended before its snapshot was confirmed")
		}
		delete(o.calls, key)
		c.active--
		if r.sent {
			c.writers--
		}
		var output []darwinTLSRecord
		if c.writers == 0 {
			output = append(output, c.reads...)
			o.release(c.reads)
			c.reads = nil
		}
		if c.active == 0 {
			output = append(output, c.closes...)
			o.release(c.closes)
			delete(o.connections, r.object)
		}
		return output, nil
	}
	if r.goPhase != 0 {
		return nil, fmt.Errorf("invalid Go TLS call phase")
	}
	if c == nil {
		return []darwinTLSRecord{r}, nil
	}
	if r.closed || !r.sent && c.writers > 0 {
		size := cap(r.payload)
		if o.records >= darwinTLSGoOrderRecords || o.bytes+size > darwinTLSGoOrderBytes || c.bytes+size > darwinTLSGoOrderConnectionBytes {
			return nil, fmt.Errorf("Go TLS pending plaintext limit exceeded")
		}
		o.bytes += size
		c.bytes += size
		o.records++
		if r.closed {
			c.closes = append(c.closes, r)
		} else {
			c.reads = append(c.reads, r)
		}
		return nil, nil
	}
	return []darwinTLSRecord{r}, nil
}
func (o *darwinTLSGoOrder) release(records []darwinTLSRecord) {
	for _, r := range records {
		o.bytes -= cap(r.payload)
		o.connections[r.object].bytes -= cap(r.payload)
		o.records--
	}
}
func (o *darwinTLSGoOrder) finish() ([]darwinTLSRecord, error) {
	for _, c := range o.connections {
		if c.writers != 0 {
			return nil, fmt.Errorf("capture ended during a Go TLS write")
		}
	}
	var output []darwinTLSRecord
	for _, c := range o.connections {
		output = append(output, c.reads...)
		output = append(output, c.closes...)
	}
	clear(o.connections)
	clear(o.calls)
	o.bytes = 0
	o.records = 0
	return output, nil
}
