package edc

import "net/netip"

func (t *pktapHTTPTracker) postgresEvents(packet httpPacket) []captureEvent {
	local, err := netip.ParseAddrPort(packet.source)
	if err != nil {
		return nil
	}
	peer, err := netip.ParseAddrPort(packet.destination)
	if err != nil {
		return nil
	}
	if local.Port() != t.postgresPort && peer.Port() != t.postgresPort {
		return nil
	}
	record := mysqlRecord{bootTimeNS: packet.bootTimeNS, pid: packet.pid, cgroupID: packet.cgroupID, process: packet.process, socket: packet.socket, source: packet.source, destination: packet.destination, localPort: local.Port(), peerPort: peer.Port(), sent: packet.sent, server: local.Port() == t.postgresPort, size: len(packet.payload), payload: packet.payload}
	events := t.postgres.events(record, t.clockOffset)
	for i := range events {
		events[i].TLS = events[i].TLS || packet.decrypted
	}
	return events
}
