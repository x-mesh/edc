package edc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNetworkRatesRequireConsecutiveCounters(t *testing.T) {
	before := &networkHealth{Supported: true, Scope: "net:[1]", Counters: map[string]networkReading{"listen_drops": networkNumber(100)}}
	now := &networkHealth{Supported: true, Scope: "net:[1]", Counters: map[string]networkReading{"listen_drops": networkNumber(106)}}
	rate := calculateNetworkHealthRate(before, now, 2)
	if got := rate.Rates["listen_drops"].PerSecond; got == nil || *got != 3 {
		t.Fatalf("rate = %+v", rate)
	}
	for _, test := range []struct {
		name        string
		before, now *networkHealth
		seconds     float64
	}{
		{"first", nil, now, 1},
		{"missing baseline", &networkHealth{Scope: "net:[1]"}, now, 1},
		{"missing current", before, &networkHealth{Scope: "net:[1]", Counters: map[string]networkReading{"listen_drops": networkUnavailable("permission denied")}}, 1},
		{"reset", now, before, 1},
		{"invalid interval", before, now, 0},
		{"namespace changed", &networkHealth{Scope: "net:[2]", Counters: before.Counters}, now, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := calculateNetworkHealthRate(test.before, test.now, test.seconds).Rates["listen_drops"]
			if value.PerSecond != nil || value.Status != "unavailable" {
				t.Fatalf("false observation: %+v", value)
			}
		})
	}
	zero := calculateNetworkHealthRate(now, now, 1).Rates["listen_drops"]
	if zero.PerSecond == nil || *zero.PerSecond != 0 || zero.Status != "observed" {
		t.Fatalf("observed zero = %+v", zero)
	}
}

func TestNetworkParsersKeepUnavailableDistinctFromZero(t *testing.T) {
	named := parseNetworkNamedCounters("TcpExt: ListenDrops ListenOverflows Other\nTcpExt: 0 9 broken\nUdp: RcvbufErrors\nUdp: 12\n")
	if value, ok := named["TcpExt.ListenDrops"]; !ok || value != 0 {
		t.Fatalf("zero = %+v", named)
	}
	if named["TcpExt.ListenOverflows"] != 9 || named["Udp.RcvbufErrors"] != 12 {
		t.Fatalf("counters = %+v", named)
	}
	if _, ok := named["TcpExt.Other"]; ok {
		t.Fatalf("invalid counter accepted: %+v", named)
	}
	if len(parseNetworkNamedCounters("TcpExt: ListenDrops ListenOverflows\nTcpExt: 100\n")) != 0 {
		t.Fatal("truncated counters accepted")
	}
	cpus := parseNetworkCPUStats("00000010 00000001 00000002\n00000020 00000003 00000004\n", false)
	if cpus["1"] != 4 || cpus["2"] != 6 {
		t.Fatalf("softnet = %+v", cpus)
	}
	ct := parseNetworkCPUStats("entries drop early_drop\n000000ff 00000002 00000003\n000000ff 00000004 00000005\n", true)
	if ct["drop"] != 6 || ct["early_drop"] != 8 {
		t.Fatalf("conntrack = %+v", ct)
	}
	bad := parseNetworkCPUStats("0 invalid 0\n0 1 0\n", false)
	if _, ok := bad["1"]; ok {
		t.Fatalf("partial sum accepted: %+v", bad)
	}
	if len(parseNetworkCPUStats("0 1 2\n0\n", false)) != 0 {
		t.Fatal("truncated CPU row accepted")
	}
	overflow := parseNetworkCPUStats("0 ffffffffffffffff 0\n0 1 0\n", false)
	if _, ok := overflow["1"]; ok {
		t.Fatal("overflow accepted")
	}
}

func TestNetworkInfoAndJSONPreserveMissingValues(t *testing.T) {
	health := &networkHealth{Supported: true, Scope: "net:[1]", Settings: map[string]networkReading{"net.netfilter.nf_conntrack_max": networkNumber(1000)}, Gauges: map[string]networkReading{"conntrack_entries": networkNumber(980)}, Counters: map[string]networkReading{"listen_drops": networkUnavailable("permission denied")}}
	var output bytes.Buffer
	printInfoNetworkHealth(&output, health, false)
	if !strings.Contains(output.String(), "980 / 1000 (98.0%)") || !strings.Contains(output.String(), "Local ports: unavailable") {
		t.Fatalf("info = %s", output.String())
	}
	rate := calculateNetworkHealthRate(nil, health, 1)
	sample := newTopSample(hostDetails{}, time.Unix(1, 0), resourceRate{NetworkHealth: rate})
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"network_limits"`) || !strings.Contains(text, `"reason":"permission denied"`) || strings.Contains(text, `"per_s":0`) {
		t.Fatalf("JSON = %s", text)
	}
	if cell := networkConntrackCell(rate); cell.level != topLevelDanger {
		t.Fatalf("cell = %+v", cell)
	}
	if value, valid := networkConntrackUsage(&networkHealth{Settings: map[string]networkReading{"net.netfilter.nf_conntrack_max": networkNumber(0)}, Gauges: health.Gauges}); valid || value != 0 {
		t.Fatal("zero limit accepted")
	}
}

func TestNetworkHistoryUsesSelectedSampleAndFitsTerminal(t *testing.T) {
	makeHealth := func(count uint64) *networkHealthRate {
		return calculateNetworkHealthRate(nil, &networkHealth{Supported: true, Scope: "net:[1]", Gauges: map[string]networkReading{"conntrack_entries": networkNumber(count)}, Settings: map[string]networkReading{"net.netfilter.nf_conntrack_max": networkNumber(1000)}}, 1)
	}
	model := topModel{view: topViewNetwork, width: 80, height: 24, selected: 0, previous: resourceSnapshot{NetworkHealth: &networkHealth{}}, rows: []topDashboardRow{
		{at: time.Unix(1, 0), rate: resourceRate{NetworkHealth: makeHealth(100)}},
		{at: time.Unix(2, 0), rate: resourceRate{NetworkHealth: makeHealth(900)}},
	}}
	lines := model.panelLines()
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "100/1000 (10.0%)") || strings.Contains(text, "900/1000") {
		t.Fatalf("history = %s", text)
	}
	for _, line := range lines {
		if liveWidth(line) > 80 {
			t.Fatalf("line too wide: %q", line)
		}
	}
	if peaks := strings.Join(model.peakLines(), "\n"); !strings.Contains(peaks, "90.0%") {
		t.Fatalf("peaks = %s", peaks)
	}
}
