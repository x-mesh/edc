package edc

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
)

// macOS의 routing message 배치다. arp -an과 같은 sysctl(NET_RT_FLAGS, RTF_LLINFO)이 이 message를 이어서 준다.
// byte 해석은 OS와 상관이 없어 이 파일은 Linux에서도 빌드하고 테스트한다. 값은 macOS의 net/route.h와 sys/socket.h에서 왔다.
const (
	darwinRtMsghdrSize = 92
	darwinRTAXCount    = 8
	darwinRTADst       = 0x1
	darwinRTAGateway   = 0x2
	darwinAFInet       = 2
	darwinAFLink       = 18
	darwinAFInet6      = 30
	darwinRTFReject    = 0x8
	darwinRTFLLInfo    = 0x400
	darwinRTFStatic    = 0x800
)

// darwinNeighborEntry는 ARP table의 항목 하나다. index는 interface 번호다.
type darwinNeighborEntry struct {
	index int
	ip    string
	mac   string
	flags uint32
}

// parseDarwinNeighborTable은 rt_msghdr 뒤에 rtm_addrs의 bit 순서로 이어지는 sockaddr를 읽는다. sockaddr는 4바이트 단위로 붙는다.
// family는 darwinAFInet이나 darwinAFInet6이고, 다른 family의 항목은 뺀다. macOS는 x86_64와 arm64 모두 little endian이다.
func parseDarwinNeighborTable(rib []byte, family byte) ([]darwinNeighborEntry, error) {
	var entries []darwinNeighborEntry
	for len(rib) > 0 {
		if len(rib) < 2 {
			return nil, errors.New("routing message header is cut")
		}
		length := int(binary.LittleEndian.Uint16(rib[0:2]))
		if length < darwinRtMsghdrSize || length > len(rib) {
			return nil, errors.New("routing message is cut")
		}
		message := rib[:length]
		rib = rib[length:]
		flags := binary.LittleEndian.Uint32(message[8:12])
		if flags&darwinRTFLLInfo == 0 {
			continue
		}
		entry := darwinNeighborEntry{index: int(binary.LittleEndian.Uint16(message[4:6])), flags: flags}
		present := binary.LittleEndian.Uint32(message[12:16])
		addresses := message[darwinRtMsghdrSize:]
		for bit := uint32(0); bit < darwinRTAXCount && len(addresses) > 0; bit++ {
			if present&(1<<bit) == 0 {
				continue
			}
			size, step := int(addresses[0]), 4
			if size > 0 {
				step = (size + 3) &^ 3
			}
			if size > len(addresses) {
				break
			}
			sockaddr := addresses[:size]
			switch 1 << bit {
			case darwinRTADst:
				switch {
				case family == darwinAFInet && size >= 8 && sockaddr[1] == darwinAFInet:
					entry.ip = netip.AddrFrom4([4]byte(sockaddr[4:8])).String()
				case family == darwinAFInet6 && size >= 24 && sockaddr[1] == darwinAFInet6:
					address := [16]byte(sockaddr[8:24])
					// macOS kernel은 link-local 주소의 3·4번째 byte에 interface 번호를 넣어 둔다. ndp -a처럼 지운다.
					if address[0] == 0xfe && address[1]&0xc0 == 0x80 {
						address[2], address[3] = 0, 0
					}
					entry.ip = netip.AddrFrom16(address).String()
				}
			case darwinRTAGateway:
				if size >= 8 && sockaddr[1] == darwinAFLink {
					if index := int(binary.LittleEndian.Uint16(sockaddr[2:4])); index != 0 {
						entry.index = index
					}
					// sockaddr_dl의 data에는 interface 이름 뒤에 link 주소가 온다. 확인이 끝나지 않은 항목은 주소 길이가 0이다.
					start, addressLength := 8+int(sockaddr[5]), int(sockaddr[6])
					if addressLength == 6 && start+addressLength <= size {
						entry.mac = net.HardwareAddr(sockaddr[start : start+addressLength]).String()
					}
				}
			}
			addresses = addresses[min(step, len(addresses)):]
		}
		if entry.ip != "" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// darwinNeighborState는 macOS 항목의 상태다. macOS에는 Linux의 NUD 상태가 없어 flag와 MAC으로 정한다.
func darwinNeighborState(entry darwinNeighborEntry) string {
	switch {
	case entry.flags&darwinRTFReject != 0:
		return traceNeighborFailedState
	case entry.mac == "":
		return "INCOMPLETE"
	case entry.flags&darwinRTFStatic != 0:
		return "PERMANENT"
	}
	return "COMPLETE"
}

func darwinNeighbors(entries []darwinNeighborEntry, names neighborInterfaceNames) map[neighborKey]traceNeighbor {
	neighbors := make(map[neighborKey]traceNeighbor, len(entries))
	for _, entry := range entries {
		neighbor := traceNeighbor{iface: names.name(entry.index), ip: entry.ip, mac: entry.mac, state: darwinNeighborState(entry)}
		neighbors[neighborKey{iface: neighbor.iface, ip: neighbor.ip}] = neighbor
	}
	return neighbors
}
