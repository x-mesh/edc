package edc

import (
	"net/netip"
	"testing"
	"time"
)

// macOS CI에서 실제 ARP table을 읽는다. 항목 수는 환경마다 달라서 형식만 확인한다.
func TestReadDarwinARPTableOnThisMac(t *testing.T) {
	neighbors, err := readDarwinARPTable(arpInterfaceNames{})
	if err != nil {
		t.Fatal(err)
	}
	for key, neighbor := range neighbors {
		address, err := netip.ParseAddr(neighbor.ip)
		if err != nil || !address.Is4() || neighbor.iface == "" || key.ip != neighbor.ip || neighbor.state == "" {
			t.Fatalf("neighbor = %#v", neighbor)
		}
	}
	t.Logf("%d IPv4 ARP entries", len(neighbors))
	summary, err := collectARPEvents(1500*time.Millisecond, nil, nil)
	if err != nil || summary.Event != "capture_summary" {
		t.Fatalf("collectARPEvents = %#v, %v", summary, err)
	}
}
