//go:build linux

package edc

import (
	"os"
	"testing"
)

func TestTLSPlaintextPIDFilter(t *testing.T) {
	scope := (tcpTraceOptions{tlsPlaintext: true, pid: 42}).scope("http")
	if scope.pid != 42 || !scope.tlsPlaintext {
		t.Fatalf("scope = %+v", scope)
	}
}

func TestTLSPlaintextDefaultsToAllProcesses(t *testing.T) {
	scope := (tcpTraceOptions{tlsPlaintext: true}).scope("http")
	if scope.pid != 0 || scope.container != nil {
		t.Fatalf("scope = %+v", scope)
	}
}

func TestTLSPlaintextExLengthUsesResultPointer(t *testing.T) {
	paths, err := findTLSLibraries(uint32(os.Getpid()), nil)
	if err != nil {
		t.Skipf("OpenSSL shared library is unavailable: %v", err)
	}
	found := false
	for _, path := range paths {
		symbols, err := elfSymbolSet(path)
		if err != nil {
			t.Fatal(err)
		}
		if symbols["SSL_write_ex"] && symbols["SSL_read_ex"] {
			found = true
		}
	}
	if !found {
		t.Skip("OpenSSL library has no SSL_write_ex and SSL_read_ex symbols")
	}
}
