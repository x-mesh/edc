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
	write("net/netstat", "TcpExt: ListenOverflows ListenDrops SyncookiesSent TCPMemoryPressures TCPAbortOnMemory RcvPruned\nTcpExt: 10 20 0 4 1 9\n")
	write("net/snmp", "Tcp: AttemptFails CurrEstab RetransSegs OutRsts\nTcp: 7 3 11 13\nUdp: RcvbufErrors SndbufErrors\nUdp: 4 5\n")
	write("net/sockstat", "TCP: inuse 12 orphan 2 tw 40 alloc 15 mem 3\n")
	write("sys/net/ipv4/tcp_mem", "92277\t123039\t184554\n")
	write("sysfs/class/net/eth0/statistics/rx_missed_errors", "5\n")
	write("sysfs/class/net/eth0/statistics/rx_fifo_errors", "1\n")
	write("sysfs/class/net/eth0/statistics/tx_fifo_errors", "0\n")
	write("sysfs/class/net/eth1/statistics/rx_missed_errors", "7\n")
	write("sysfs/class/net/eth1/statistics/rx_fifo_errors", "0\n")
	write("sysfs/class/net/eth1/statistics/tx_fifo_errors", "2\n")
	// loopback은 더하지 않는다.
	write("sysfs/class/net/lo/statistics/rx_missed_errors", "100\n")
	write("sysfs/class/net/lo/statistics/rx_fifo_errors", "100\n")
	write("sysfs/class/net/lo/statistics/tx_fifo_errors", "100\n")
	write("net/softnet_stat", "00000010 00000002 00000003\n00000011 00000004 00000005\n")
	write("net/stat/nf_conntrack", "entries drop early_drop insert_failed\n000000ff 00000001 00000002 00000003\n000000ff 00000002 00000003 00000004\n")
	health := collectLinuxNetworkHealth(root, filepath.Join(root, "sysfs"))
	if usage, ok := networkConntrackUsage(health); !ok || usage != 25 {
		t.Fatalf("usage = %v %v", usage, ok)
	}
	if usage, ok := networkTCPMemoryUsage(health); !ok || usage < 0.0016 || usage > 0.0017 {
		t.Fatalf("tcp memory usage = %v %v", usage, ok)
	}
	if health.Settings["net.ipv4.tcp_mem"].Text != "92277 123039 184554" {
		t.Fatalf("tcp_mem = %+v", health.Settings["net.ipv4.tcp_mem"])
	}
	if health.Settings["net.ipv4.ip_local_port_range"].Text != "32768 60999" || health.Settings["net.ipv4.ip_local_reserved_ports"].Text != "none" {
		t.Fatalf("settings = %+v", health.Settings)
	}
	for _, name := range []string{"net.core.somaxconn", "net.core.rmem_max"} {
		if health.Settings[name].Value != nil || health.Settings[name].Status != "unavailable" {
			t.Fatalf("invalid setting %s = %+v", name, health.Settings[name])
		}
	}
	for name, expected := range map[string]uint64{"conntrack_entries": 250, "tcp_established": 3, "tcp_inuse": 12, "tcp_time_wait": 40, "tcp_orphans": 2, "tcp_memory_pages": 3} {
		if got := health.Gauges[name].Value; got == nil || *got != expected {
			t.Fatalf("%s = %+v", name, health.Gauges[name])
		}
	}
	for name, expected := range map[string]uint64{"listen_overflows": 10, "listen_drops": 20, "syn_cookies_sent": 0, "udp_rcvbuf_errors": 4, "udp_sndbuf_errors": 5, "softnet_dropped": 6, "softnet_time_squeeze": 8, "conntrack_drop": 3, "conntrack_early_drop": 5, "conntrack_insert_failed": 7, "tcp_retrans_segs": 11, "tcp_out_rsts": 13, "tcp_attempt_fails": 7, "tcp_memory_pressures": 4, "tcp_abort_on_memory": 1, "tcp_rcv_pruned": 9, "nic_rx_missed": 12, "nic_rx_fifo": 1, "nic_tx_fifo": 2} {
		if got := health.Counters[name].Value; got == nil || *got != expected {
			t.Fatalf("%s = %+v", name, health.Counters[name])
		}
	}
	write("net/netstat", "TcpExt: ListenOverflows ListenDrops\nTcpExt: malformed\n")
	// 한 interface의 파일이 깨지면 합계를 내지 않는다. 빠진 interface의 drop을 0으로 치면 안 된다.
	write("sysfs/class/net/eth1/statistics/rx_missed_errors", "x\n")
	after := collectLinuxNetworkHealth(root, filepath.Join(root, "sysfs"))
	if after.Counters["nic_rx_missed"].Value != nil || after.Counters["nic_rx_fifo"].Value == nil {
		t.Fatalf("nic counters after a broken file = %+v", after.Counters)
	}
	rate := calculateNetworkHealthRate(health, after, 1)
	if rate.Rates["listen_drops"].PerSecond != nil {
		t.Fatal("failed read produced a rate")
	}
	// top --interface가 다시 더할 수 있게 interface마다의 값도 남긴다. 깨진 file은 그 interface의 값만 unavailable이다.
	if eth0, eth1 := after.NICInterfaces["eth0"]["nic_rx_missed"], after.NICInterfaces["eth1"]["nic_rx_missed"]; eth0.Value == nil || *eth0.Value != 5 || eth1.Value != nil || after.NICInterfaces["lo"] != nil {
		t.Fatalf("per interface = %+v", after.NICInterfaces)
	}
	if rate.NICInterfaces != nil {
		t.Fatal("the rate must not keep the values of each interface")
	}
}

func TestNetworkLinuxMissingProcDoesNotReportZero(t *testing.T) {
	health := collectLinuxNetworkHealth(t.TempDir(), t.TempDir())
	for _, readings := range []map[string]networkReading{health.Settings, health.Gauges, health.Counters} {
		for name, value := range readings {
			if value.Status != "unavailable" || value.Value != nil || !strings.Contains(value.Reason, "not exposed") {
				t.Fatalf("%s = %+v", name, value)
			}
		}
	}
}
