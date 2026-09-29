package edc

import (
	"encoding/binary"
	"testing"
)

// darwinRouteMessage는 NET_RT_FLAGS가 주는 rt_msghdr 하나를 만든다. 뒤에 sockaddr_in과 sockaddr_dl이 4바이트 단위로 붙는다.
func darwinRouteMessage(index uint16, flags uint32, destination []byte, family byte, mac []byte, name string) []byte {
	message := make([]byte, darwinRtMsghdrSize)
	message[2], message[3] = 5, 4
	binary.LittleEndian.PutUint16(message[4:6], index)
	binary.LittleEndian.PutUint32(message[8:12], flags)
	binary.LittleEndian.PutUint32(message[12:16], darwinRTADst|darwinRTAGateway)
	sockaddrIn := make([]byte, 16)
	sockaddrIn[0], sockaddrIn[1] = 16, family
	copy(sockaddrIn[4:], destination)
	sockaddrDL := make([]byte, 20)
	sockaddrDL[0], sockaddrDL[1] = 20, darwinAFLink
	binary.LittleEndian.PutUint16(sockaddrDL[2:4], index)
	sockaddrDL[4], sockaddrDL[5], sockaddrDL[6] = 6, byte(len(name)), byte(len(mac))
	copy(sockaddrDL[8:], name)
	copy(sockaddrDL[8+len(name):], mac)
	message = append(append(message, sockaddrIn...), sockaddrDL...)
	binary.LittleEndian.PutUint16(message[0:2], uint16(len(message)))
	return message
}

func TestParseDarwinARPTableReadsEntries(t *testing.T) {
	mac := []byte{2, 0, 0, 0, 0, 1}
	var rib []byte
	for _, message := range [][]byte{
		darwinRouteMessage(4, darwinRTFLLInfo, []byte{192, 0, 2, 1}, darwinAFInet, mac, ""),
		// 확인이 끝나지 않은 항목은 link 주소가 없다. 이름이 sockaddr_dl에 들어 있어도 주소는 이름 뒤에서 읽는다.
		darwinRouteMessage(4, darwinRTFLLInfo, []byte{192, 0, 2, 2}, darwinAFInet, nil, "en0"),
		darwinRouteMessage(5, darwinRTFLLInfo|darwinRTFReject, []byte{192, 0, 2, 3}, darwinAFInet, nil, ""),
		darwinRouteMessage(5, darwinRTFLLInfo|darwinRTFStatic, []byte{224, 0, 0, 251}, darwinAFInet, []byte{1, 0, 0x5e, 0, 0, 0xfb}, "en1"),
		// IPv6 이웃과 LLINFO가 없는 경로는 ARP 항목이 아니다.
		darwinRouteMessage(4, darwinRTFLLInfo, []byte{0xfe, 0x80}, 30, mac, ""),
		darwinRouteMessage(4, 0, []byte{192, 0, 2, 9}, darwinAFInet, mac, ""),
	} {
		rib = append(rib, message...)
	}
	entries, err := parseDarwinNeighborTable(rib)
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries = %#v, %v", entries, err)
	}
	want := []struct{ ip, mac, state string }{
		{"192.0.2.1", "02:00:00:00:00:01", "COMPLETE"},
		{"192.0.2.2", "", "INCOMPLETE"},
		{"192.0.2.3", "", traceNeighborFailedState},
		{"224.0.0.251", "01:00:5e:00:00:fb", "PERMANENT"},
	}
	for index, entry := range entries {
		if entry.ip != want[index].ip || entry.mac != want[index].mac || darwinNeighborState(entry) != want[index].state {
			t.Fatalf("entry %d = %#v state %q, want %+v", index, entry, darwinNeighborState(entry), want[index])
		}
	}
	neighbors := darwinNeighbors(entries[:1], neighborInterfaceNames{4: "en0"})
	if neighbor := neighbors[neighborKey{iface: "en0", ip: "192.0.2.1"}]; neighbor.mac != "02:00:00:00:00:01" || neighbor.state != "COMPLETE" {
		t.Fatalf("neighbors = %#v", neighbors)
	}
	for name, cut := range map[string][]byte{"short header": rib[:1], "cut message": rib[:len(rib)-4]} {
		if _, err := parseDarwinNeighborTable(cut); err == nil {
			t.Fatalf("%s parsed without an error", name)
		}
	}
}
