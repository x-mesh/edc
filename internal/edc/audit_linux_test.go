//go:build linux

package edc

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestAuditConnectedTCPKeepsEstablishedRows(t *testing.T) {
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0016 0100007F:D4D8 01 00000000:00000000 00:00000000 00000000     0        0 1000 1 0000000000000000 100 0 0 10 0
   1: 0100007F:D4D8 0100007F:0016 01 00000000:00000000 00:00000000 00000000     0        0 2000 1 0000000000000000 100 0 0 10 0
   2: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3000 1 0000000000000000 100 0 0 10 0
   3: 0100007F:0017 0100007F:D4D9 06 00000000:00000000 00:00000000 00000000     0        0 4000 1 0000000000000000 100 0 0 10 0
   4: 0100007F:0018 0100007F:D4DA 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 100 0 0 10 0
`
	got := auditConnectedTCP(parseLinuxTCPTable(strings.NewReader(table)))
	want := []auditTCPSocket{
		{Local: netip.MustParseAddrPort("127.0.0.1:22"), Remote: netip.MustParseAddrPort("127.0.0.1:54488"), Inode: 1000},
		{Local: netip.MustParseAddrPort("127.0.0.1:54488"), Remote: netip.MustParseAddrPort("127.0.0.1:22"), Inode: 2000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sockets = %+v", got)
	}
}

func TestAuditConnectedTCPReadsIPv6Rows(t *testing.T) {
	table := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:0016 00000000000000000000000001000000:D4D8 01 00000000:00000000 00:00000000 00000000     0        0 5000 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:0016 0000000000000000FFFF00000100007F:D4D9 01 00000000:00000000 00:00000000 00000000     0        0 6000 1 0000000000000000 100 0 0 10 0
`
	got := auditConnectedTCP(parseLinuxTCPTable(strings.NewReader(table)))
	want := []auditTCPSocket{
		{Local: netip.MustParseAddrPort("[::1]:22"), Remote: netip.MustParseAddrPort("[::1]:54488"), Inode: 5000},
		{Local: netip.MustParseAddrPort("127.0.0.1:22"), Remote: netip.MustParseAddrPort("127.0.0.1:54489"), Inode: 6000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sockets = %+v", got)
	}
}
