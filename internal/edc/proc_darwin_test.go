//go:build darwin

package edc

import "testing"

func TestParseLsofFieldsCountsOnlyOpenFDs(t *testing.T) {
	output := "p990\nfcwd\ntDIR\nn/\nftxt\ntREG\nn/usr/libexec/app\nf3\ntREG\nn/var/log/app.log\nf4\ntIPv6\nn*:8080\nTST=LISTEN\nf5\ntIPv4\nn*:8080\nTST=LISTEN\nf6\ntIPv4\nn10.0.0.5:8080->10.0.0.9:51234\nTST=ESTABLISHED\nf7\ntIPv4\nn10.0.0.5:40000->10.0.0.7:5432\nTST=CLOSE_WAIT\n"
	var details procDetails
	parseLsofFields(output, &details)
	if details.FDTypes["REG"] != 1 || details.FDTypes["DIR"] != 0 || details.FDTypes["IPv4"] != 3 || details.FDTypes["IPv6"] != 1 {
		t.Fatalf("types = %v", details.FDTypes)
	}
	if len(details.Listen) != 2 || details.Conns["ESTABLISHED"] != 1 || details.Conns["CLOSE_WAIT"] != 1 {
		t.Fatalf("sockets = %+v", details)
	}
	if details.Remotes["10.0.0.9"] != 1 || details.Remotes["10.0.0.7:5432"] != 1 {
		t.Fatalf("remotes = %v", details.Remotes)
	}
}
