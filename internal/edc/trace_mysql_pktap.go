package edc

import "net/netip"

func (t *pktapHTTPTracker) mysqlEvents(packet httpPacket, wire *mysqlStream) []captureEvent {
	local, err := netip.ParseAddrPort(packet.source)
	if err != nil {
		return nil
	}
	peer, err := netip.ParseAddrPort(packet.destination)
	if err != nil {
		return nil
	}
	if local.Port() != t.mysqlPort && peer.Port() != t.mysqlPort {
		return nil
	}
	server := local.Port() == t.mysqlPort
	if server != packet.sent && mysqlCleanCommand(packet.payload, len(packet.payload)) {
		wire.reset()
	}
	var events []captureEvent
	wire.feed(packet.payload, len(packet.payload), packet.bootTimeNS, func(message mysqlPacket) bool {
		data := make([]byte, 4+len(message.payload))
		data[0], data[1], data[2], data[3] = byte(message.length), byte(message.length>>8), byte(message.length>>16), message.seq
		copy(data[4:], message.payload)
		record := mysqlRecord{bootTimeNS: message.at, pid: packet.pid, cgroupID: packet.cgroupID, process: packet.process, socket: packet.socket, source: packet.source, destination: packet.destination, localPort: local.Port(), peerPort: peer.Port(), sent: packet.sent, server: server, size: 4 + message.length, payload: data}
		events = append(events, t.mysql.events(record, t.clockOffset)...)
		return true
	})
	for i := range events {
		events[i].TLS = events[i].TLS || packet.decrypted
	}
	return events
}
