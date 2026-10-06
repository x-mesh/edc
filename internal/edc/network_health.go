package edc

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

type networkReading struct {
	Status string  `json:"status"`
	Value  *uint64 `json:"value,omitempty"`
	Text   string  `json:"text,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

type networkHealth struct {
	Supported bool                      `json:"supported"`
	Scope     string                    `json:"scope"`
	Settings  map[string]networkReading `json:"settings,omitempty"`
	Gauges    map[string]networkReading `json:"gauges,omitempty"`
	Counters  map[string]networkReading `json:"counters,omitempty"`
}

type networkCounterRate struct {
	Status    string   `json:"status"`
	PerSecond *float64 `json:"per_s,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

type networkHealthRate struct {
	networkHealth
	Rates map[string]networkCounterRate `json:"rates,omitempty"`
}

func networkNumber(value uint64) networkReading {
	return networkReading{Status: "observed", Value: &value}
}

func networkUnavailable(reason string) networkReading {
	return networkReading{Status: "unavailable", Reason: reason}
}

func networkReadingText(value networkReading) string {
	if value.Value != nil {
		return fmt.Sprint(*value.Value)
	}
	if value.Text != "" {
		return value.Text
	}
	if value.Status == "" {
		return "unavailable"
	}
	return value.Status
}

func calculateNetworkHealthRate(previous, current *networkHealth, seconds float64) *networkHealthRate {
	if current == nil {
		return nil
	}
	result := &networkHealthRate{networkHealth: *current, Rates: make(map[string]networkCounterRate)}
	for name, now := range current.Counters {
		rate := networkCounterRate{Status: "unavailable", Reason: "two consecutive observations required"}
		if now.Value == nil {
			rate.Reason = now.Reason
		} else if previous != nil && previous.Scope == current.Scope {
			before := previous.Counters[name]
			if before.Value != nil && seconds > 0 {
				if *now.Value >= *before.Value {
					value := float64(*now.Value-*before.Value) / seconds
					rate.Status, rate.PerSecond, rate.Reason = "observed", &value, ""
				} else {
					rate.Reason = "counter reset"
				}
			}
		}
		result.Rates[name] = rate
	}
	return result
}

func networkConntrackUsage(health *networkHealth) (float64, bool) {
	if health == nil {
		return 0, false
	}
	count, limit := health.Gauges["conntrack_entries"].Value, health.Settings["net.netfilter.nf_conntrack_max"].Value
	if count == nil || limit == nil || *limit == 0 {
		return 0, false
	}
	return float64(*count) / float64(*limit) * 100, true
}

func printInfoNetworkHealth(writer io.Writer, health *networkHealth, verbose bool) {
	fmt.Fprintln(writer, "\nNetwork Limits")
	if health == nil || !health.Supported {
		fmt.Fprintln(writer, "└── Unsupported: Linux network limits require Linux")
		return
	}
	ct := networkReadingText(health.Gauges["conntrack_entries"]) + " / " + networkReadingText(health.Settings["net.netfilter.nf_conntrack_max"])
	if usage, ok := networkConntrackUsage(health); ok {
		ct += fmt.Sprintf(" (%.1f%%)", usage)
	}
	setting := func(name string) string { return networkReadingText(health.Settings[name]) }
	lines := []string{
		"Conntrack: " + ct,
		"Local ports: " + setting("net.ipv4.ip_local_port_range") + " · Reserved: " + setting("net.ipv4.ip_local_reserved_ports"),
		"Listen: accept=" + setting("net.core.somaxconn") + " · SYN=" + setting("net.ipv4.tcp_max_syn_backlog") + " · syncookies=" + setting("net.ipv4.tcp_syncookies"),
		"Socket buffer max: receive=" + setting("net.core.rmem_max") + " · send=" + setting("net.core.wmem_max") + " bytes",
		"Receive backlog: " + setting("net.core.netdev_max_backlog") + " · budget=" + setting("net.core.netdev_budget") + " · budget_us=" + setting("net.core.netdev_budget_usecs"),
		"Neighbor max: IPv4=" + setting("net.ipv4.neigh.default.gc_thresh3") + " · IPv6=" + setting("net.ipv6.neigh.default.gc_thresh3"),
		"Routing: forward(v4/v6)=" + setting("net.ipv4.ip_forward") + "/" + setting("net.ipv6.conf.all.forwarding") + " · rp_filter(all/default)=" + setting("net.ipv4.conf.all.rp_filter") + "/" + setting("net.ipv4.conf.default.rp_filter"),
	}
	for index, line := range lines {
		branch := "├──"
		if index == len(lines)-1 {
			branch = "└──"
		}
		fmt.Fprintf(writer, "%s %s\n", branch, line)
	}
	if verbose {
		fmt.Fprintf(writer, "    Scope: %s; softnet and TCP TIME_WAIT can be host-wide.\n", health.Scope)
		for _, name := range networkSettingNames {
			reading := health.Settings[name]
			fmt.Fprintf(writer, "    %s: %s", name, networkReadingText(reading))
			if reading.Reason != "" {
				fmt.Fprintf(writer, " (%s)", infoSingleLine(reading.Reason))
			}
			fmt.Fprintln(writer)
		}
	}
}

var networkSettingNames = []string{
	"net.netfilter.nf_conntrack_max",
	"net.ipv4.ip_local_port_range", "net.ipv4.ip_local_reserved_ports",
	"net.core.somaxconn", "net.ipv4.tcp_max_syn_backlog", "net.ipv4.tcp_syncookies",
	"net.core.rmem_max", "net.core.wmem_max", "net.ipv4.tcp_rmem", "net.ipv4.tcp_wmem",
	"net.core.netdev_max_backlog", "net.core.netdev_budget", "net.core.netdev_budget_usecs",
	"net.ipv4.neigh.default.gc_thresh3", "net.ipv6.neigh.default.gc_thresh3",
	"net.ipv4.conf.all.rp_filter", "net.ipv4.conf.default.rp_filter",
	"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding",
}

func networkRateText(health *networkHealthRate, name string) string {
	if health == nil || health.Rates[name].PerSecond == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *health.Rates[name].PerSecond)
}

func networkGaugeText(health *networkHealthRate, name string) string {
	if health == nil || health.Gauges[name].Value == nil {
		return "—"
	}
	return fmt.Sprint(*health.Gauges[name].Value)
}

func networkHealthLines(health *networkHealthRate) []string {
	if health == nil || !health.Supported {
		return []string{"network limits · unsupported (Linux only)"}
	}
	ct := networkGaugeText(health, "conntrack_entries") + "/" + networkReadingText(health.Settings["net.netfilter.nf_conntrack_max"])
	if usage, ok := networkConntrackUsage(&health.networkHealth); ok {
		ct += fmt.Sprintf(" (%.1f%%)", usage)
	}
	return []string{
		"netns · conntrack " + ct + " · TCP ESTAB/CLOSE_WAIT " + networkGaugeText(health, "tcp_established"),
		"TCPv4 in-use " + networkGaugeText(health, "tcp_inuse") + " · TIME_WAIT " + networkGaugeText(health, "tcp_time_wait") + " (can be host-wide)",
		"listen/s · overflow " + networkRateText(health, "listen_overflows") + " · drop " + networkRateText(health, "listen_drops") + " · SYN cookies " + networkRateText(health, "syn_cookies_sent"),
		"drop/s · conntrack drop " + networkRateText(health, "conntrack_drop") + " · early " + networkRateText(health, "conntrack_early_drop") + " · UDP buffer " + networkRateText(health, "udp_rcvbuf_errors"),
		"softnet/s (host) · drop " + networkRateText(health, "softnet_dropped") + " · budget exhausted " + networkRateText(health, "softnet_time_squeeze"),
		"local ports " + networkReadingText(health.Settings["net.ipv4.ip_local_port_range"]) + " · accept/SYN " + networkReadingText(health.Settings["net.core.somaxconn"]) + "/" + networkReadingText(health.Settings["net.ipv4.tcp_max_syn_backlog"]),
		"— unavailable · counts are not port utilization · rates use consecutive samples",
	}
}

func parseNetworkNamedCounters(input string) map[string]uint64 {
	result := make(map[string]uint64)
	lines := strings.Split(strings.TrimSpace(input), "\n")
	for index := 0; index+1 < len(lines); index++ {
		names, values := strings.Fields(lines[index]), strings.Fields(lines[index+1])
		if len(names) < 2 || len(names) != len(values) || names[0] != values[0] {
			continue
		}
		for column := 1; column < len(names); column++ {
			if value, ok := parseNetworkUint(values[column], 10); ok {
				result[strings.TrimSuffix(names[0], ":")+"."+names[column]] = value
			}
		}
		index++
	}
	return result
}

func parseNetworkUint(value string, base int) (uint64, bool) {
	number, err := strconv.ParseUint(value, base, 64)
	return number, err == nil
}

func parseNetworkCPUStats(input string, named bool) map[string]uint64 {
	result := make(map[string]uint64)
	invalid := make(map[string]bool)
	lines := strings.Split(strings.TrimSpace(input), "\n")
	var names []string
	if named && len(lines) > 0 {
		names, lines = strings.Fields(lines[0]), lines[1:]
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if (named && len(fields) != len(names)) || (!named && len(fields) < 3) {
			return map[string]uint64{}
		}
		for index, field := range fields {
			name := fmt.Sprint(index)
			if named {
				name = names[index]
			}
			value, ok := parseNetworkUint(field, 16)
			if !ok || ^uint64(0)-result[name] < value {
				invalid[name] = true
				continue
			}
			result[name] += value
		}
	}
	for name := range invalid {
		delete(result, name)
	}
	return result
}

func networkSettingLines(health *networkHealthRate) []string {
	if health == nil || !health.Supported {
		return nil
	}
	setting := func(name string) string { return networkReadingText(health.Settings[name]) }
	return []string{
		"reserved ports " + setting("net.ipv4.ip_local_reserved_ports") + " · syncookies " + setting("net.ipv4.tcp_syncookies"),
		"buffer max bytes · receive " + setting("net.core.rmem_max") + " · send " + setting("net.core.wmem_max"),
		"TCP buffer min/default/max · receive " + setting("net.ipv4.tcp_rmem"),
		"TCP buffer min/default/max · send " + setting("net.ipv4.tcp_wmem"),
		"receive backlog " + setting("net.core.netdev_max_backlog") + " · budget " + setting("net.core.netdev_budget") + " / " + setting("net.core.netdev_budget_usecs") + "us",
		"neighbor max · IPv4 " + setting("net.ipv4.neigh.default.gc_thresh3") + " · IPv6 " + setting("net.ipv6.neigh.default.gc_thresh3"),
		"forward IPv4/IPv6 " + setting("net.ipv4.ip_forward") + "/" + setting("net.ipv6.conf.all.forwarding") + " · rp_filter all/default " + setting("net.ipv4.conf.all.rp_filter") + "/" + setting("net.ipv4.conf.default.rp_filter"),
	}
}
