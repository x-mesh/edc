package edc

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func checkNetworkTestFacts(network checkNetwork) checkFacts {
	if network.Route == "" {
		network.Route = "10.0.0.1 dev eth0"
	}
	network.DNS = infoDNS{Config: resolvConfig{Nameservers: []string{"10.0.0.2"}}}
	return checkFacts{Network: network}
}

func TestCheckNetworkJudgesTheRouteInterfaceLink(t *testing.T) {
	for _, test := range []struct {
		name string
		link checkNetworkLink
		want Status
	}{
		{"up", checkNetworkLink{State: "up", Carrier: "1"}, StatusPass},
		{"unread", checkNetworkLink{}, StatusPass},
		// tun, WireGuard, Tailscale은 operstate를 알리지 않는다.
		{"virtual", checkNetworkLink{State: "unknown", Carrier: "1"}, StatusPass},
		{"virtual without carrier", checkNetworkLink{State: "unknown", Carrier: "0"}, StatusPass},
		{"dormant", checkNetworkLink{State: "dormant", Carrier: "1"}, StatusPass},
		{"admin down", checkNetworkLink{State: "down"}, StatusFail},
		{"cable out", checkNetworkLink{State: "down", Carrier: "0"}, StatusFail},
		{"lower layer down", checkNetworkLink{State: "lowerlayerdown", Carrier: "0"}, StatusFail},
	} {
		result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{Link: test.link}), checkTestPolicy)
		if result.Status != test.want {
			t.Errorf("%s = %+v, want %s", test.name, result, test.want)
		}
		if test.want == StatusFail && (result.Next != "edc net interfaces" || !strings.Contains(result.Summary, "eth0")) {
			t.Errorf("%s = %+v", test.name, result)
		}
	}
	result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{Link: checkNetworkLink{State: "down", Carrier: "0"}}), checkTestPolicy)
	if !strings.Contains(result.Summary, "no carrier") {
		t.Fatalf("no carrier = %+v", result)
	}
}

func TestCheckNetworkWarnsOnAnUnresolvedGateway(t *testing.T) {
	for _, test := range []struct {
		state string
		want  Status
	}{{"", StatusPass}, {"REACHABLE", StatusPass}, {"STALE", StatusPass}, {"COMPLETE", StatusPass}, {"INCOMPLETE", StatusWarn}, {"FAILED", StatusWarn}} {
		result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{GatewayNeighbor: test.state}), checkTestPolicy)
		if result.Status != test.want {
			t.Errorf("neighbor %q = %+v, want %s", test.state, result, test.want)
		}
		if test.want == StatusWarn && (result.Next != "edc trace arp" || !strings.Contains(result.Summary, "10.0.0.1")) {
			t.Errorf("neighbor %q = %+v", test.state, result)
		}
	}
	result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{Route: "fe80::1 dev en0", GatewayNeighbor: "FAILED"}), checkTestPolicy)
	if result.Next != "edc trace ndp" {
		t.Fatalf("IPv6 gateway = %+v", result)
	}
}

func TestCheckGatewayAddressSkipsPointToPointRoutes(t *testing.T) {
	for _, test := range []struct {
		route, gateway, iface, address string
	}{
		{"10.0.0.1 dev eth0", "10.0.0.1", "eth0", "10.0.0.1"},
		{"0.0.0.0 dev tun0", "0.0.0.0", "tun0", ""},
		{"dev wg0", "", "wg0", ""},
		{"link#14 dev utun3", "link#14", "utun3", ""},
		{"fe80::1%en0 dev en0", "fe80::1%en0", "en0", "fe80::1"},
		{"", "", "", ""},
	} {
		gateway, iface := checkRouteParts(test.route)
		if gateway != test.gateway || iface != test.iface {
			t.Errorf("route %q = %q, %q", test.route, gateway, iface)
		}
		address, ok := checkGatewayAddress(gateway)
		if ok != (test.address != "") || (ok && address.String() != test.address) {
			t.Errorf("gateway %q = %s, %v", gateway, address, ok)
		}
	}
}

func TestCheckNetworkJudgesOrphanSockets(t *testing.T) {
	for _, test := range []struct {
		percent float64
		want    Status
	}{{79, StatusPass}, {80, StatusWarn}, {90, StatusFail}} {
		result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{Orphans: test.percent, OrphansOK: true}), checkTestPolicy)
		if result.Status != test.want {
			t.Errorf("orphans %.0f%% = %s, want %s", test.percent, result.Status, test.want)
		}
	}
}

func checkCounterTestHealth(scope string, counters map[string]uint64, nic map[string]uint64) *networkHealth {
	health := &networkHealth{Scope: scope, Counters: map[string]networkReading{}, NICInterfaces: map[string]map[string]networkReading{}}
	for name, value := range counters {
		health.Counters[name] = networkNumber(value)
	}
	for iface, value := range nic {
		health.NICInterfaces[iface] = map[string]networkReading{"nic_rx_missed": networkNumber(value)}
	}
	return health
}

func TestCheckCounterGrowth(t *testing.T) {
	previous := checkCounterTestHealth("net:[1]", map[string]uint64{"listen_drops": 10, "softnet_dropped": 50, "conntrack_drop": 7, "tcp_retrans_segs": 100}, map[string]uint64{"eth0": 3, "veth1": 9})
	current := checkCounterTestHealth("net:[1]", map[string]uint64{"listen_drops": 13, "softnet_dropped": 40, "conntrack_drop": 7, "tcp_retrans_segs": 900}, map[string]uint64{"eth0": 5, "veth2": 100})
	grown := checkCounterGrowth(previous, current)
	// softnet_dropped가 줄어든 것은 카운터 초기화다. veth2는 처음 읽을 때 없었고 veth1은 사라져 NIC 합에서 뺀다.
	// 재전송은 정상 host에서도 늘어 보지 않는다.
	if want := []string{"listen_drops +3", "nic_rx_missed +2"}; !slices.Equal(grown, want) {
		t.Fatalf("grown = %v, want %v", grown, want)
	}
	if grown := checkCounterGrowth(previous, checkCounterTestHealth("net:[2]", map[string]uint64{"listen_drops": 99}, nil)); len(grown) != 0 {
		t.Fatalf("another namespace = %v", grown)
	}
	unread := checkCounterTestHealth("net:[1]", nil, nil)
	unread.Counters["listen_drops"] = networkUnavailable("permission denied")
	if grown := checkCounterGrowth(previous, unread); len(grown) != 0 {
		t.Fatalf("unreadable = %v", grown)
	}

	result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{CountersGrown: []string{"listen_drops +3", "nic_rx_missed +2"}}), checkTestPolicy)
	if result.Status != StatusWarn || result.Next != "edc listen" || !strings.Contains(result.Summary, "listen_drops +3, nic_rx_missed +2") {
		t.Fatalf("grown = %+v", result)
	}
	if next := checkCounterNext("nic_rx_fifo +1"); next != "edc net interfaces" {
		t.Fatalf("nic next = %s", next)
	}
	if next := checkCounterNext("conntrack_drop +1"); next != "edc top" {
		t.Fatalf("conntrack next = %s", next)
	}
}

func TestCheckCounterSampleIsSkippedBeforeTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if waitCheckCounterSample(ctx, started, checkCounterInterval) {
		t.Fatal("waited past the deadline")
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("waited %s before skipping", elapsed)
	}
	if !waitCheckCounterSample(context.Background(), time.Now(), 10*time.Millisecond) {
		t.Fatal("no deadline must wait for the second sample")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if waitCheckCounterSample(cancelled, time.Now(), time.Hour) {
		t.Fatal("cancelled context waited")
	}

	result := checkNetworkResult(checkNetworkTestFacts(checkNetwork{CountersSkipped: true}), checkTestPolicy)
	if result.Status != StatusPass || !strings.Contains(result.Summary, "not compared") {
		t.Fatalf("skipped counters = %+v", result)
	}
}

// 짧은 --timeout에서도 network는 route와 DNS를 판정해야 한다. 카운터를 기다리다 deadline에 걸려 점검 전체가 skip이 되면 안 된다.
func TestCollectCheckFactsKeepsTheNetworkResultUnderAShortTimeout(t *testing.T) {
	sources := checkSources{
		network: func(ctx context.Context) checkNetwork {
			network := checkNetwork{Route: "10.0.0.1 dev eth0", DNS: infoDNS{Config: resolvConfig{Nameservers: []string{"10.0.0.2"}}}}
			network.CountersSkipped = !waitCheckCounterSample(ctx, time.Now(), checkCounterInterval)
			return network
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	facts := collectCheckFacts(ctx, sources, nil)
	if len(facts.TimedOut) != 0 || !facts.Network.CountersSkipped {
		t.Fatalf("timed out = %v, network = %+v", facts.TimedOut, facts.Network)
	}
	if result := checkNetworkResult(facts, checkTestPolicy); result.Status != StatusPass {
		t.Fatalf("network = %+v", result)
	}
}

func TestParseIfconfigLink(t *testing.T) {
	for _, test := range []struct {
		name, output string
		want         checkNetworkLink
	}{
		{"wifi", "en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500\n\tether aa:bb:cc:dd:ee:ff\n\tmedia: autoselect\n\tstatus: active\n", checkNetworkLink{State: "up", Carrier: "1"}},
		{"cable out", "en7: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500\n\tmedia: autoselect (none)\n\tstatus: inactive\n", checkNetworkLink{State: "down", Carrier: "0"}},
		{"utun", "utun4: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1280\n\tinet 100.64.0.2 --> 100.64.0.2 netmask 0xffffffff\n", checkNetworkLink{State: "unknown"}},
		{"admin down", "en0: flags=8822<BROADCAST,SMART,SIMPLEX,MULTICAST> mtu 1500\n\tstatus: inactive\n", checkNetworkLink{State: "down"}},
	} {
		if got := parseIfconfigLink(test.output); got != test.want {
			t.Errorf("%s = %+v, want %+v", test.name, got, test.want)
		}
	}
}
