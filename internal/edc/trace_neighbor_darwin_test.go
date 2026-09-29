package edc

import (
	"net/netip"
	"testing"
	"time"
)

// macOS CI에서 실제 ARP와 NDP table을 읽는다. 항목 수는 환경마다 달라서 형식만 확인한다.
func TestReadDarwinNeighborTablesOnThisMac(t *testing.T) {
	for _, family := range []byte{darwinAFInet, darwinAFInet6} {
		neighbors, err := readDarwinNeighborTable(family, neighborInterfaceNames{})
		if err != nil {
			t.Fatal(err)
		}
		for key, neighbor := range neighbors {
			address, err := netip.ParseAddr(neighbor.ip)
			if err != nil || address.Is4() != (family == darwinAFInet) || address.Zone() != "" || neighbor.iface == "" || key.ip != neighbor.ip || neighbor.state == "" {
				t.Fatalf("family %d neighbor = %#v", family, neighbor)
			}
		}
		t.Logf("family %d: %d entries", family, len(neighbors))
	}
	for _, protocol := range []string{"arp", "ndp"} {
		summary, err := collectNeighborEvents(protocol, 1500*time.Millisecond, nil, nil)
		if err != nil || summary.Event != "capture_summary" {
			t.Fatalf("collectNeighborEvents(%s) = %#v, %v", protocol, summary, err)
		}
	}
}
