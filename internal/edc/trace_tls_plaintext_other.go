//go:build !linux

package edc

import (
	"errors"
	"io"
)

type tlsPlaintextRecord struct {
	packet      *httpPacket
	closeSocket uint64
	err         error
}

func startTLSPlaintextCollector(traceScope, <-chan struct{}) (<-chan tlsPlaintextRecord, io.Closer, error) {
	return nil, nil, errors.New("TLS plaintext capture requires Linux")
}
