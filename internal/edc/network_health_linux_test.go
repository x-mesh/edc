//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkLinuxProcFixture(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("sys/net/netfilter/nf_conntrack_count", "250\n")
	write("sys/net/netfilter/nf_conntrack_max", "1000\n")
	write("sys/net/ipv4/ip_local_port_range", "32768\t60999\n")
	write("sys/net/ipv4/ip_local_reserved_ports", "\n")
	write("sys/net/core/somaxconn", "broken\n")
	write("net/netstat", "TcpExt: ListenOverflows ListenDrops SyncookiesSent\nTcpExt: 10 20 0\n")
	write("net/snmp", "Tcp: CurrEstab\nTcp: 3\nUdp: RcvbufErrors SndbufErrors\nUdp: 4 5\n")
	write("net/sockstat", "TCP: inuse 12 orphan 0 tw 40 alloc 15 mem 3\n")
	write("net/softnet_stat", "00000010 00000002 00000003\n00000011 00000004 00000005\n")
	write("net/stat/nf_conntrack", "entries drop early_drop insert_failed\n000000ff 00000001 00000002 00000003\n000000ff 00000002 00000003 00000004\n")
	health := collectLinuxNetworkHealth(root)
	if usage, ok := networkConntrackUsage(health); !ok || usage != 25 {
		t.Fatalf("usage = %v %v", usage, ok)
	}
	if health.Settings["net.ipv4.ip_local_port_range"].Text != "32768 60999" || health.Settings["net.ipv4.ip_local_reserved_ports"].Text != "none" {
		t.Fatalf("settings = %+v", health.Settings)
	}
	for _, name := range []string{"net.core.somaxconn", "net.core.rmem_max"} {
		if health.Settings[name].Value != nil || health.Settings[name].Status != "unavailable" {
			t.Fatalf("invalid setting %s = %+v", name, health.Settings[name])
		}
	}
	for name, expected := range map[string]uint64{"conntrack_entries": 250, "tcp_established": 3, "tcp_inuse": 12, "tcp_time_wait": 40} {
		if got := health.Gauges[name].Value; got == nil || *got != expected {
			t.Fatalf("%s = %+v", name, health.Gauges[name])
		}
	}
	for name, expected := range map[string]uint64{"listen_overflows": 10, "listen_drops": 20, "syn_cookies_sent": 0, "udp_rcvbuf_errors": 4, "udp_sndbuf_errors": 5, "softnet_dropped": 6, "softnet_time_squeeze": 8, "conntrack_drop": 3, "conntrack_early_drop": 5, "conntrack_insert_failed": 7} {
		if got := health.Counters[name].Value; got == nil || *got != expected {
			t.Fatalf("%s = %+v", name, health.Counters[name])
		}
	}
	write("net/netstat", "TcpExt: ListenOverflows ListenDrops\nTcpExt: malformed\n")
	after := collectLinuxNetworkHealth(root)
	rate := calculateNetworkHealthRate(health, after, 1)
	if rate.Rates["listen_drops"].PerSecond != nil {
		t.Fatal("failed read produced a rate")
	}
}

func TestNetworkLinuxMissingProcDoesNotReportZero(t *testing.T) {
	health := collectLinuxNetworkHealth(t.TempDir())
	for _, readings := range []map[string]networkReading{health.Settings, health.Gauges, health.Counters} {
		for name, value := range readings {
			if value.Status != "unavailable" || value.Value != nil || !strings.Contains(value.Reason, "not exposed") {
				t.Fatalf("%s = %+v", name, value)
			}
		}
	}
}
