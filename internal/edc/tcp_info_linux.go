//go:build linux

package edc

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// Linux tcp_info의 tcpi_options bit다(include/uapi/linux/tcp.h).
const (
	linuxTCPOptTimestamps = 1
	linuxTCPOptSACK       = 2
	linuxTCPOptWindow     = 4
	linuxTCPOptECN        = 8
)

// readTCPHandshake는 연결된 socket의 TCP_INFO를 읽는다. RTT, RTO는 microsecond 단위다. window scale 값은 x/sys/unix의
// TCPInfo가 공개하지 않는 bit field에 있어 켜졌는지만 안다.
func readTCPHandshake(connection net.Conn) (tcpHandshake, error) {
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		return tcpHandshake{}, errTCPInfoUnavailable
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return tcpHandshake{}, err
	}
	var info *unix.TCPInfo
	var infoErr error
	if err := raw.Control(func(fd uintptr) {
		info, infoErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	}); err != nil {
		return tcpHandshake{}, err
	}
	if infoErr != nil {
		return tcpHandshake{}, infoErr
	}
	return tcpHandshake{
		RTT:          time.Duration(info.Rtt) * time.Microsecond,
		RTTVar:       time.Duration(info.Rttvar) * time.Microsecond,
		RTO:          time.Duration(info.Rto) * time.Microsecond,
		MSS:          int(info.Snd_mss),
		SACK:         info.Options&linuxTCPOptSACK != 0,
		Timestamps:   info.Options&linuxTCPOptTimestamps != 0,
		WindowScale:  info.Options&linuxTCPOptWindow != 0,
		ECN:          info.Options&linuxTCPOptECN != 0,
		SendScale:    -1,
		ReceiveScale: -1,
		Retransmits:  int(info.Total_retrans),
	}, nil
}
