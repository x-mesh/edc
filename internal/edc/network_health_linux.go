//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"strings"
)

func collectNetworkHealth() *networkHealth { return collectLinuxNetworkHealth("/proc", "/sys") }

// networkNICCounters는 /sys/class/net/<iface>/statistics에서 더하는 장치 쪽 drop이다. rx_missed_errors는 ring이 가득 차
// 장치가 버린 패킷이고, fifo는 FIFO overrun이다. /proc/net/dev의 drop 열은 커널 쪽 rx_dropped라 이 둘을 담지 않는다.
var networkNICCounters = map[string]string{"nic_rx_missed": "rx_missed_errors", "nic_rx_fifo": "rx_fifo_errors", "nic_tx_fifo": "tx_fifo_errors"}

func collectLinuxNetworkHealth(procRoot, sysRoot string) *networkHealth {
	health := &networkHealth{Supported: true, Scope: "current network namespace", Settings: make(map[string]networkReading), Gauges: make(map[string]networkReading), Counters: make(map[string]networkReading)}
	if namespace, err := os.Readlink(filepath.Join(procRoot, "self/ns/net")); err == nil {
		health.Scope = namespace
	}
	read := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(procRoot, path)) }
	for _, name := range networkSettingNames {
		data, err := read("sys/" + strings.ReplaceAll(name, ".", "/"))
		value := networkUnavailable("not exposed")
		if err != nil {
			value = networkReadError(err)
		} else {
			text := strings.Join(strings.Fields(string(data)), " ")
			if number, ok := parseNetworkUint(text, 10); ok {
				value = networkNumber(number)
			} else if name == "net.ipv4.ip_local_reserved_ports" || name == "net.ipv4.ip_local_port_range" || name == "net.ipv4.tcp_rmem" || name == "net.ipv4.tcp_wmem" || name == "net.ipv4.tcp_mem" {
				if text == "" {
					text = "none"
				}
				value = networkReading{Status: "observed", Text: text}
			} else {
				value = networkUnavailable("invalid value")
			}
		}
		health.Settings[name] = value
	}
	data, err := read("sys/net/netfilter/nf_conntrack_count")
	health.Gauges["conntrack_entries"] = networkReadError(err)
	if err == nil {
		if number, ok := parseNetworkUint(strings.TrimSpace(string(data)), 10); ok {
			health.Gauges["conntrack_entries"] = networkNumber(number)
		}
	}
	for path, fields := range map[string]map[string]string{
		"net/netstat": {"listen_overflows": "TcpExt.ListenOverflows", "listen_drops": "TcpExt.ListenDrops", "syn_cookies_sent": "TcpExt.SyncookiesSent", "tcp_memory_pressures": "TcpExt.TCPMemoryPressures", "tcp_abort_on_memory": "TcpExt.TCPAbortOnMemory", "tcp_rcv_pruned": "TcpExt.RcvPruned"},
		"net/snmp":    {"udp_rcvbuf_errors": "Udp.RcvbufErrors", "udp_sndbuf_errors": "Udp.SndbufErrors", "tcp_established": "Tcp.CurrEstab", "tcp_retrans_segs": "Tcp.RetransSegs", "tcp_out_rsts": "Tcp.OutRsts", "tcp_attempt_fails": "Tcp.AttemptFails"},
	} {
		data, err := read(path)
		values := parseNetworkNamedCounters(string(data))
		for name, field := range fields {
			value := networkReadError(err)
			if number, ok := values[field]; ok && err == nil {
				value = networkNumber(number)
			}
			if name == "tcp_established" {
				health.Gauges[name] = value
			} else {
				health.Counters[name] = value
			}
		}
	}
	data, err = read("net/sockstat")
	for _, name := range []string{"tcp_inuse", "tcp_time_wait", "tcp_orphans", "tcp_memory_pages"} {
		health.Gauges[name] = networkReadError(err)
	}
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "TCP:" {
				continue
			}
			for i := 1; i+1 < len(fields); i += 2 {
				// mem은 page 단위다. tcp_mem의 세 값과 같은 단위라 그대로 비교한다.
				name := map[string]string{"inuse": "tcp_inuse", "tw": "tcp_time_wait", "orphan": "tcp_orphans", "mem": "tcp_memory_pages"}[fields[i]]
				if number, ok := parseNetworkUint(fields[i+1], 10); name != "" && ok {
					health.Gauges[name] = networkNumber(number)
				}
			}
		}
	}
	totals, perInterface := collectLinuxNICCounters(filepath.Join(sysRoot, "class/net"))
	for name, value := range totals {
		health.Counters[name] = value
	}
	health.NICInterfaces = perInterface
	for path, columns := range map[string]map[string]string{
		"net/softnet_stat":      {"softnet_dropped": "1", "softnet_time_squeeze": "2"},
		"net/stat/nf_conntrack": {"conntrack_drop": "drop", "conntrack_early_drop": "early_drop", "conntrack_insert_failed": "insert_failed"},
	} {
		data, err := read(path)
		values := parseNetworkCPUStats(string(data), path == "net/stat/nf_conntrack")
		for name, column := range columns {
			value := networkReadError(err)
			if number, ok := values[column]; ok && err == nil {
				value = networkNumber(number)
			}
			health.Counters[name] = value
		}
	}
	return health
}

// collectLinuxNICCounters는 loopback을 뺀 interface마다 장치 쪽 drop을 읽고, 모든 interface의 합도 낸다. 현재 namespace의
// interface만 보인다. interface가 하나도 없거나 디렉터리를 읽지 못하면 unavailable이다. 한 interface의 파일을 읽지 못하면 그 값은
// unavailable이다.
func collectLinuxNICCounters(classNet string) (map[string]networkReading, map[string]map[string]networkReading) {
	entries, err := os.ReadDir(classNet)
	if err != nil {
		result := map[string]networkReading{}
		for name := range networkNICCounters {
			result[name] = networkReadError(err)
		}
		return result, nil
	}
	perInterface := map[string]map[string]networkReading{}
	for _, entry := range entries {
		if entry.Name() == "lo" {
			continue
		}
		counters := map[string]networkReading{}
		for name, file := range networkNICCounters {
			data, err := os.ReadFile(filepath.Join(classNet, entry.Name(), "statistics", file))
			if err != nil {
				counters[name] = networkReadError(err)
				continue
			}
			number, ok := parseNetworkUint(strings.TrimSpace(string(data)), 10)
			if !ok {
				counters[name] = networkReadError(os.ErrInvalid)
				continue
			}
			counters[name] = networkNumber(number)
		}
		perInterface[entry.Name()] = counters
	}
	totals := sumNetworkNICCounters(perInterface, func(string) bool { return true })
	for name := range networkNICCounters {
		if _, ok := totals[name]; !ok {
			totals[name] = networkUnavailable("no interface")
		}
	}
	return totals, perInterface
}

func networkReadError(err error) networkReading {
	switch {
	case err == nil:
		return networkUnavailable("invalid or missing field")
	case os.IsNotExist(err):
		return networkUnavailable("not exposed; feature may be disabled")
	case os.IsPermission(err):
		return networkUnavailable("permission denied")
	default:
		return networkUnavailable(err.Error())
	}
}
