//go:build linux

package edc

import (
	"encoding/binary"
	"strings"
)

// mysqlRecordType은 BPF가 MySQL 읽기와 쓰기의 앞부분을 넘기는 레코드다. 배치는 HTTP 레코드와 같다. kind는 서버 쪽 여부이고
// offset은 그 읽기나 쓰기의 전체 byte 수이며, 앞의 len byte만 payload에 있다.
const (
	mysqlRecordType   = 11
	mysqlRecordServer = 1
)

func parseMySQLRecord(sample []byte) (mysqlRecord, bool) {
	if len(sample) < httpRecordPayloadOffset || binary.LittleEndian.Uint32(sample[8:12]) != mysqlRecordType {
		return mysqlRecord{}, false
	}
	family := binary.LittleEndian.Uint16(sample[36:38])
	var source, destination [16]byte
	copy(source[:], sample[44:60])
	copy(destination[:], sample[60:76])
	record := mysqlRecord{
		bootTimeNS:  binary.LittleEndian.Uint64(sample[0:8]),
		pid:         binary.LittleEndian.Uint32(sample[12:16]),
		cgroupID:    binary.LittleEndian.Uint64(sample[16:24]),
		socket:      binary.LittleEndian.Uint64(sample[24:32]),
		sent:        sample[38] == httpRecordSent,
		server:      sample[39] == mysqlRecordServer,
		source:      formatCaptureAddress(family, source, binary.LittleEndian.Uint16(sample[40:42])),
		destination: formatCaptureAddress(family, destination, binary.LittleEndian.Uint16(sample[42:44])),
		process:     strings.TrimRight(string(sample[76:92]), "\x00"),
		size:        int(binary.LittleEndian.Uint32(sample[92:96])),
		payload:     sample[httpRecordPayloadOffset:],
	}
	if length := int(binary.LittleEndian.Uint32(sample[32:36])); length < len(record.payload) {
		record.payload = record.payload[:length]
	}
	return record, true
}
