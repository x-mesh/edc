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
	events := t.redis.events(record, t.clockOffset)
	for i := range events {
		events[i].TLS = events[i].TLS || packet.decrypted
	}
	return events
}

func (t *pktapHTTPTracker) redisReplyNeedsMore(d *pktapHTTPDirection, final bool) bool {
	if !d.packet.decrypted {
		return false
	}
	conn := t.redis.conns[d.packet.socket]
	local, err := netip.ParseAddrPort(d.packet.source)
	if err != nil || (local.Port() == t.redisPort) != d.packet.sent || conn == nil || conn.responseSynced || conn.gap || conn.replication || len(conn.pending) == 0 {
		d.redisBoundary = nil
		d.redisBoundaryBytes = 0
		return false
	}
	if d.redisBoundary == nil {
		d.redisBoundary = &redisReader{}
	}
	valid := d.redisBoundary.feed(d.buffer[d.redisBoundaryBytes:], len(d.buffer)-d.redisBoundaryBytes, d.packet.bootTimeNS, func(redisFrame) {})
	d.redisBoundaryBytes = len(d.buffer)
	wait := valid && !d.redisBoundary.idle() && !d.redisBoundary.skipping() && !final
	if !wait {
		d.redisBoundary = nil
		d.redisBoundaryBytes = 0
	}
	return wait
}
