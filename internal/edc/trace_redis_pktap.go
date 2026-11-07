package edc

import "net/netip"

func (t *pktapHTTPTracker) redisEvents(packet httpPacket) []captureEvent {
	local, err := netip.ParseAddrPort(packet.source)
	if err != nil {
		return nil
	}
	peer, err := netip.ParseAddrPort(packet.destination)
	if err != nil {
		return nil
	}
	if local.Port() != t.redisPort && peer.Port() != t.redisPort {
		return nil
	}
	record := mysqlRecord{bootTimeNS: packet.bootTimeNS, pid: packet.pid, cgroupID: packet.cgroupID, process: packet.process, socket: packet.socket, source: packet.source, destination: packet.destination, localPort: local.Port(), peerPort: peer.Port(), sent: packet.sent, server: local.Port() == t.redisPort, size: len(packet.payload), payload: packet.payload}
	return t.redis.events(record, t.clockOffset)
}
