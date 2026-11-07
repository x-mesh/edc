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
}
type darwinTLSGoOrder struct {
	calls          map[darwinTLSGoCall]struct{}
	connections    map[uint64]*darwinTLSGoConnection
	bytes, records int
}

func newDarwinTLSGoOrder() *darwinTLSGoOrder {
	return &darwinTLSGoOrder{calls: map[darwinTLSGoCall]struct{}{}, connections: map[uint64]*darwinTLSGoConnection{}}
}
func (o *darwinTLSGoOrder) add(r darwinTLSRecord) ([]darwinTLSRecord, error) {
	c := o.connections[r.object]
	key := darwinTLSGoCall{r.object, r.goroutine, r.frame, r.sent}
	if r.goPhase == 1 {
		if _, exists := o.calls[key]; exists {
			return nil, fmt.Errorf("duplicate Go TLS call begin")
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
		o.calls[key] = struct{}{}
		return nil, nil
	}
	if r.goPhase == 2 {
		if _, exists := o.calls[key]; !exists || c == nil {
			return nil, fmt.Errorf("Go TLS call end has no begin")
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
