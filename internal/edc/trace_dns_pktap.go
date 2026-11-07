package edc

func (t *pktapHTTPTracker) dnsEvents(packet httpPacket, tcp bool) []captureEvent {
	event, ok := t.dns.event(dnsPacket{bootTimeNS: packet.bootTimeNS, pid: packet.pid, cgroupID: packet.cgroupID, process: packet.process, sent: packet.sent, tcp: tcp, socket: packet.socket, source: packet.source, destination: packet.destination, payload: packet.payload}, t.clockOffset)
	if !ok {
		return nil
	}
	if tcp {
		event.Transport = "tcp"
	}
	return []captureEvent{event}
}
