//go:build darwin

package edc

import "fmt"

const darwinTLSFDAddressLimit = 16384

type darwinTLSAddress struct {
	source, destination string
	generation          uint64
	fd                  int32
}
type darwinTLSAddressCache struct {
	pid        uint32
	generation uint64
	fds        map[int32]darwinTLSAddress
	objects    map[uint64]darwinTLSAddress
	references map[uint64]int
	retained   map[uint64]darwinTLSAddress
	lookup     func(uint32, int32) (string, string)
}

func newDarwinTLSAddressCache(pid uint32) *darwinTLSAddressCache {
	return &darwinTLSAddressCache{pid: pid, fds: map[int32]darwinTLSAddress{}, objects: map[uint64]darwinTLSAddress{}, references: map[uint64]int{}, retained: map[uint64]darwinTLSAddress{}, lookup: darwinTLSAddresses}
}
func (c *darwinTLSAddressCache) metadata(r darwinTLSRecord) error {
	if r.pid != c.pid || r.fd < 0 {
		return fmt.Errorf("TLS socket metadata belongs to another process or descriptor")
	}
	value, exists := c.fds[r.fd]
	if r.fdMeta == 3 {
		if exists {
			if c.references[value.generation] > 0 {
				c.retained[value.generation] = value
			}
			c.generation++
			c.fds[r.fd] = darwinTLSAddress{fd: r.fd, generation: c.generation}
		}
		return nil
	}
	if !exists && len(c.fds) >= darwinTLSFDAddressLimit {
		return fmt.Errorf("TLS socket address cache exceeds %d descriptors", darwinTLSFDAddressLimit)
	}
	if value.generation == 0 || r.fdMeta == 2 && r.fdKind >= 2 {
		if c.references[value.generation] > 0 {
			c.retained[value.generation] = value
		}
		c.generation++
		value = darwinTLSAddress{fd: r.fd, generation: c.generation}
	}
	switch r.fdMeta {
	case 1:
		if _, exists := c.objects[r.object]; !exists && len(c.objects) >= pktapHTTPFlowLimit {
			return fmt.Errorf("TLS socket address cache exceeds %d objects", pktapHTTPFlowLimit)
		}
		source, destination := c.lookup(c.pid, r.fd)
		if destination != "" && (value.destination == "" || value.destination == destination) {
			value.source = source
			value.destination = destination
		}
		c.forget(r.object)
		c.references[value.generation]++
		c.objects[r.object] = value
	case 2:
		if r.fdKind == 1 || r.fdKind == 3 {
			value.source = r.address
		} else {
			value.destination = r.address
		}
	default:
		return fmt.Errorf("invalid TLS socket metadata kind")
	}
	c.fds[r.fd] = value
	return nil
}
func (c *darwinTLSAddressCache) decorate(r *darwinTLSRecord) error {
	if r.goPhase != 0 || r.closed {
		return nil
	}
	value, exists := c.objects[r.object]
	if !exists && r.fd >= 0 {
		if err := c.metadata(darwinTLSRecord{pid: r.pid, object: r.object, fd: r.fd, fdMeta: 1}); err != nil {
			return err
		}
		value, exists = c.objects[r.object]
	}
	if exists {
		if current := c.fds[value.fd]; current.generation == value.generation {
			value = current
		} else if retained, ok := c.retained[value.generation]; ok {
			value = retained
		}
		r.source, r.destination = value.source, value.destination
	}
	return nil
}

func (c *darwinTLSAddressCache) forget(object uint64) {
	if value, exists := c.objects[object]; exists {
		c.references[value.generation]--
		if c.references[value.generation] == 0 {
			delete(c.references, value.generation)
			delete(c.retained, value.generation)
		}
		delete(c.objects, object)
	}
}
