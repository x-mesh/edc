//go:build darwin

package edc

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// macOS tcp_connection_info의 tcpi_options bit다(netinet/tcp.h의 TCPCI_OPT_*).
const (
	darwinTCPOptTimestamps = 0x01
	darwinTCPOptSACK       = 0x02
	darwinTCPOptWindow     = 0x04
	darwinTCPOptECN        = 0x08
)

// readTCPHandshake는 연결된 socket의 TCP_CONNECTION_INFO를 읽는다. RTT, RTO는 millisecond 단위다.
func readTCPHandshake(connection net.Conn) (tcpHandshake, error) {
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		return tcpHandshake{}, errTCPInfoUnavailable
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return tcpHandshake{}, err
	}
	var info *unix.TCPConnectionInfo
	var infoErr error
	if err := raw.Control(func(fd uintptr) {
		info, infoErr = unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	}); err != nil {
		return tcpHandshake{}, err
	}
	if infoErr != nil {
		return tcpHandshake{}, infoErr
	}
	handshake := tcpHandshake{
		RTT:          time.Duration(info.Srtt) * time.Millisecond,
		RTTVar:       time.Duration(info.Rttvar) * time.Millisecond,
		RTO:          time.Duration(info.Rto) * time.Millisecond,
		MSS:          int(info.Maxseg),
		SACK:         info.Options&darwinTCPOptSACK != 0,
		Timestamps:   info.Options&darwinTCPOptTimestamps != 0,
		WindowScale:  info.Options&darwinTCPOptWindow != 0,
		ECN:          info.Options&darwinTCPOptECN != 0,
		SendScale:    -1,
		ReceiveScale: -1,
		Retransmits:  int(info.Txretransmitpackets),
	}
	if handshake.WindowScale {
		handshake.SendScale, handshake.ReceiveScale = int(info.Snd_wscale), int(info.Rcv_wscale)
	}
	return handshake, nil
}
