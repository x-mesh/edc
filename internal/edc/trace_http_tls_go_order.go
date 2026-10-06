//go:build linux

package edc

import (
	"bytes"
	"encoding/binary"
	"slices"
)

const (
	goTLSOrderRecord             = 22
	goTLSWriteBegin              = 1
	goTLSWriteEnd                = 2
	goTLSOrderSize               = 40
	goTLSOrderMaxConnections     = 4096
	goTLSOrderMaxWriters         = 10240
	goTLSOrderMaxBytes           = 32 << 20
	goTLSOrderMaxConnectionBytes = 4 << 20
	goTLSOrderMaxRecords         = 4096
)

type goTLSOrderConnection struct {
	writes map[[2]uint64]struct{}
	reads  [][]byte
	closes [][]byte
	bytes  int
}

type goTLSOrder struct {
	connections map[uint64]*goTLSOrderConnection
	writers     int
	bytes       int
	records     int
	lost        uint64
	failed      bool
}

func newGoTLSOrder() *goTLSOrder {
	return &goTLSOrder{connections: map[uint64]*goTLSOrderConnection{}}
}

func (order *goTLSOrder) fail() {
	// Lost ordering state can pair a response with another request, so stop forwarding TLS records.
	order.lost += uint64(order.records + order.writers + 1)
	clear(order.connections)
	order.writers, order.records, order.bytes = 0, 0, 0
	order.failed = true
}

func (order *goTLSOrder) add(sample []byte) [][]byte {
	if len(sample) >= 12 && binary.LittleEndian.Uint32(sample[8:12]) == goTLSOrderRecord {
		if order.failed {
			return nil
		}
		if len(sample) != goTLSOrderSize {
			order.fail()
			return nil
		}
		phase := binary.LittleEndian.Uint32(sample[12:16])
		socket := binary.LittleEndian.Uint64(sample[16:24])
		writer := [2]uint64{binary.LittleEndian.Uint64(sample[24:32]), binary.LittleEndian.Uint64(sample[32:40])}
		if socket == 0 || writer[0] == 0 || writer[1] == 0 {
			order.fail()
			return nil
		}
		connection := order.connections[socket]
		switch phase {
		case goTLSWriteBegin:
			if order.writers >= goTLSOrderMaxWriters || (connection == nil && len(order.connections) >= goTLSOrderMaxConnections) {
				order.fail()
				return nil
			}
			if connection == nil {
				connection = &goTLSOrderConnection{writes: map[[2]uint64]struct{}{}}
				order.connections[socket] = connection
			}
			if _, exists := connection.writes[writer]; exists {
				order.fail()
				return nil
			}
			connection.writes[writer] = struct{}{}
			order.writers++
		case goTLSWriteEnd:
			if connection == nil {
				order.fail()
				return nil
			}
			if _, exists := connection.writes[writer]; !exists {
				order.fail()
				return nil
			}
			delete(connection.writes, writer)
			order.writers--
			if len(connection.writes) == 0 {
				delete(order.connections, socket)
				order.bytes -= connection.bytes
				order.records -= len(connection.reads) + len(connection.closes)
				return append(connection.reads, connection.closes...)
			}
		default:
			order.fail()
		}
		return nil
	}
	packet, http := parseHTTPRecord(sample)
	if http && packet.decrypted && order.failed {
		order.lost++
		return nil
	}
	socket, close := parseTCPDestroyRecord(sample)
	if http {
		socket = packet.socket
	}
	connection := order.connections[socket]
	if connection == nil || (!close && (!http || !packet.decrypted || packet.sent || (!packet.continued && packet.offset == 0 && bytes.HasPrefix(packet.payload, http2Preface)))) {
		return [][]byte{sample}
	}
	if order.bytes+len(sample) > goTLSOrderMaxBytes || connection.bytes+len(sample) > goTLSOrderMaxConnectionBytes || order.records >= goTLSOrderMaxRecords {
		order.fail()
		order.lost++
		return nil
	}
	held := slices.Clone(sample)
	if close {
		connection.closes = append(connection.closes, held)
	} else {
		connection.reads = append(connection.reads, held)
	}
	connection.bytes += len(sample)
	order.bytes += len(sample)
	order.records++
	return nil
}

func (order *goTLSOrder) finish() uint64 {
	order.lost += uint64(order.records + order.writers)
	clear(order.connections)
	order.records, order.writers, order.bytes = 0, 0, 0
	return order.lost
}
