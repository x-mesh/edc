//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"strings"
)

func collectNetworkHealth() *networkHealth { return collectLinuxNetworkHealth("/proc") }

func collectLinuxNetworkHealth(procRoot string) *networkHealth {
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
			} else if name == "net.ipv4.ip_local_reserved_ports" || name == "net.ipv4.ip_local_port_range" || name == "net.ipv4.tcp_rmem" || name == "net.ipv4.tcp_wmem" {
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
		"net/netstat": {"listen_overflows": "TcpExt.ListenOverflows", "listen_drops": "TcpExt.ListenDrops", "syn_cookies_sent": "TcpExt.SyncookiesSent"},
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
	for _, name := range []string{"tcp_inuse", "tcp_time_wait"} {
		health.Gauges[name] = networkReadError(err)
	}
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "TCP:" {
				continue
			}
			for i := 1; i+1 < len(fields); i += 2 {
				name := map[string]string{"inuse": "tcp_inuse", "tw": "tcp_time_wait"}[fields[i]]
				if number, ok := parseNetworkUint(fields[i+1], 10); name != "" && ok {
					health.Gauges[name] = networkNumber(number)
				}
			}
		}
	}
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
