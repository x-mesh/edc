//go:build linux

package edc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/cilium/ebpf/btf"
)

// httpRecordType은 BPF가 HTTP message의 앞부분을 넘기는 레코드다. offset은 capture_events_bpf.c의 struct http_record와 같다.
const (
	httpRecordType          = 10
	httpRecordSent          = 1
	httpRecordContinuation  = 1
	httpRecordTLSHandshake  = 2 // capture_events_bpf.c의 TLS_HANDSHAKE. record 머리 없이 handshake message로 시작한다.
	httpRecordPayloadOffset = 96
	// httpRecordPayloadMax는 capture_events_bpf.c의 HTTP_PAYLOAD_SIZE로, 레코드 하나에 담기는 byte 수다.
	httpRecordPayloadMax = 16384
)

func parseHTTPRecord(sample []byte) (httpPacket, bool) {
	if len(sample) < httpRecordPayloadOffset || binary.LittleEndian.Uint32(sample[8:12]) != httpRecordType {
		return httpPacket{}, false
	}
	family := binary.LittleEndian.Uint16(sample[36:38])
	var source, destination [16]byte
	copy(source[:], sample[44:60])
	copy(destination[:], sample[60:76])
	packet := httpPacket{
		bootTimeNS:   binary.LittleEndian.Uint64(sample[0:8]),
		pid:          binary.LittleEndian.Uint32(sample[12:16]),
		cgroupID:     binary.LittleEndian.Uint64(sample[16:24]),
		socket:       binary.LittleEndian.Uint64(sample[24:32]),
		sent:         sample[38] == httpRecordSent,
		source:       formatCaptureAddress(family, source, binary.LittleEndian.Uint16(sample[40:42])),
		destination:  formatCaptureAddress(family, destination, binary.LittleEndian.Uint16(sample[42:44])),
		process:      strings.TrimRight(string(sample[76:92]), "\x00"),
		payload:      sample[httpRecordPayloadOffset:],
		continued:    sample[39] == httpRecordContinuation,
		tlsHandshake: sample[39] == httpRecordTLSHandshake,
		offset:       binary.LittleEndian.Uint32(sample[92:96]),
	}
	if size := int(binary.LittleEndian.Uint32(sample[32:36])); size < len(packet.payload) {
		packet.payload = packet.payload[:size]
	}
	return packet, true
}

// httpTracePrerequisites는 eBPF 조건과 함께 사용자 버퍼를 읽을 iov_iter 필드를 확인한다. BPF는 없는 필드를 건너뛰므로,
// 확인하지 않으면 오래된 kernel에서 trace http가 아무 event 없이 끝난다.
func httpTracePrerequisites() error {
	if err := captureEventsPrerequisites(); err != nil {
		return err
	}
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("read kernel BTF: %w", err)
	}
	return httpIOVIterFields(spec)
}

// httpIOVIterFields는 BPF가 읽는 iov_iter 필드가 kernel BTF에 있는지 본다. BTF에 iov_iter struct가 둘 이상인 kernel이
// 있어서(OrbStack 7.0) 하나라도 필드를 모두 가지면 통과한다. BPF의 CO-RE도 후보 중 맞는 struct를 고른다.
func httpIOVIterFields(spec *btf.Spec) error {
	types, err := spec.AnyTypesByName("iov_iter")
	if err != nil {
		return fmt.Errorf("find iov_iter in kernel BTF: %w", err)
	}
	missing := "iter_type"
	for _, typ := range types {
		iter, ok := typ.(*btf.Struct)
		if !ok {
			continue
		}
		missing = ""
		for _, field := range []string{"iter_type", "iov_offset", "nr_segs"} {
			if !btfHasMember(iter, field) {
				missing = field
				break
			}
		}
		if missing == "" && !btfHasMember(iter, "iov") && !btfHasMember(iter, "__iov") {
			missing = "iov"
		}
		if missing == "" {
			return nil
		}
	}
	return errors.New(T("cli.trace.http_kernel", missing))
}

// btfHasMember는 이름 없는 struct와 union 안까지 찾는다. iov_iter의 ubuf와 __iov는 이름 없는 union 안에 있다.
func btfHasMember(typ btf.Type, name string) bool {
	var members []btf.Member
	switch composite := btf.UnderlyingType(typ).(type) {
	case *btf.Struct:
		members = composite.Members
	case *btf.Union:
		members = composite.Members
	}
	for _, member := range members {
		if member.Name == name || (member.Name == "" && btfHasMember(member.Type, name)) {
			return true
		}
	}
	return false
}
