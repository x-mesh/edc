package edc

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseTopInterfaceFilter(t *testing.T) {
	for _, value := range []string{"", "all", "  all  "} {
		if filter, err := parseTopInterfaceFilter(value); err != nil || filter.active() {
			t.Errorf("%q = %+v, %v; want every interface", value, filter, err)
		}
	}
	filter, err := parseTopInterfaceFilter("eth0, en*,physical")
	if err != nil || filter.String() != "eth0, en*,physical" || !reflect.DeepEqual(filter.names, []string{"eth0"}) || !reflect.DeepEqual(filter.patterns, []string{"en*"}) || !filter.physical {
		t.Fatalf("filter = %+v, %v", filter, err)
	}
	for _, value := range []string{"all,eth0", "eth0,,eth1", "eth0,eth0", "[bad"} {
		if _, err := parseTopInterfaceFilter(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestTopInterfaceFilterMatchesNamesGlobsAndPhysical(t *testing.T) {
	filter, _ := parseTopInterfaceFilter("eth0,veth*,physical")
	var asked []string
	physical := func(name string) bool { asked = append(asked, name); return name == "eno1" }
	for name, want := range map[string]bool{"eth0": true, "veth48baa04": true, "eno1": true, "docker0": false} {
		if got := filter.matches(name, physical); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// 이름과 glob으로 정해지는 interface는 sysfs를 읽지 않는다.
	if strings.Contains(strings.Join(asked, ","), "eth0") || strings.Contains(strings.Join(asked, ","), "veth") {
		t.Fatalf("physical asked for %v", asked)
	}
	if filter.withoutPhysical().active() {
		t.Fatal("a filter with physical must fall back to every interface")
	}
	if names, _ := parseTopInterfaceFilter("eth0,wg*"); names.withoutPhysical().String() != "eth0,wg*" {
		t.Fatal("a filter without physical must stay")
	}
	if missing := filter.missingNames([]string{"eno1", "docker0"}); !reflect.DeepEqual(missing, []string{"eth0"}) {
		t.Fatalf("missing = %v", missing)
	}
	none, _ := parseTopInterfaceFilter("wg*")
	if none.matchesAny([]string{"eno1", "docker0"}, physical) || !none.matchesAny([]string{"wg0"}, physical) {
		t.Fatal("matchesAny")
	}
}

func TestTopInterfaceFilterApplyKeepsTheSelectedCountersAndNICDrops(t *testing.T) {
	number := func(value uint64) networkReading { return networkNumber(value) }
	health := &networkHealth{Supported: true, Counters: map[string]networkReading{"nic_rx_missed": number(32), "softnet_dropped": number(5)}, NICInterfaces: map[string]map[string]networkReading{
		"eth0":    {"nic_rx_missed": number(10)},
		"docker0": {"nic_rx_missed": number(20)},
		"veth1":   {"nic_rx_missed": networkUnavailable("permission denied")},
	}}
	snapshot := resourceSnapshot{TakenAt: time.Unix(1, 0), NetHealthValid: true, NetworkHealth: health, NetInterfaces: []netInterfaceCounters{{Name: "eth0", BytesIn: 1}, {Name: "docker0", BytesIn: 2}, {Name: "veth1", BytesIn: 3}}}
	noPhysical := func(string) bool { return false }

	filter, _ := parseTopInterfaceFilter("eth0")
	got := filter.apply(snapshot, noPhysical)
	if len(got.NetInterfaces) != 1 || got.NetInterfaces[0].Name != "eth0" || !got.NetHealthValid {
		t.Fatalf("interfaces = %+v, valid %v", got.NetInterfaces, got.NetHealthValid)
	}
	if value := got.NetworkHealth.Counters["nic_rx_missed"].Value; value == nil || *value != 10 || got.NetworkHealth.Counters["softnet_dropped"].Value == nil {
		t.Fatalf("counters = %+v", got.NetworkHealth.Counters)
	}
	// 원래 snapshot은 그대로다. 대시보드는 같은 snapshot을 다른 선택으로 다시 쓸 수 있다.
	if value := snapshot.NetworkHealth.Counters["nic_rx_missed"].Value; *value != 32 || len(snapshot.NetInterfaces) != 3 {
		t.Fatal("apply changed the original snapshot")
	}

	filter, _ = parseTopInterfaceFilter("docker0,veth*")
	if reading := filter.apply(snapshot, noPhysical).NetworkHealth.Counters["nic_rx_missed"]; reading.Value != nil || reading.Reason != "permission denied" {
		t.Fatalf("a selected interface that edc cannot read = %+v", reading)
	}

	filter, _ = parseTopInterfaceFilter("wg*")
	got = filter.apply(snapshot, noPhysical)
	if got.NetHealthValid || len(got.NetInterfaces) != 0 || got.NetworkHealth.Counters["nic_rx_missed"].Reason != "no interface" {
		t.Fatalf("no match = %+v, %+v", got.NetInterfaces, got.NetworkHealth.Counters)
	}

	if all := (topInterfaceFilter{}).apply(snapshot, noPhysical); !reflect.DeepEqual(all, snapshot) {
		t.Fatal("every interface must leave the snapshot unchanged")
	}
}

func TestTopSampleAddsInterfacesOnlyWithAFilter(t *testing.T) {
	rate := resourceRate{NetHealthValid: true, NetIn: 1000, NetInterfaces: []netInterfaceRate{{Name: "eno1", In: 1000.004, Out: 2, PacketsIn: 3}}}
	sample := newTopSample(hostDetails{Hostname: "host"}, time.Unix(1, 0), rate)
	encoded, _ := json.Marshal(sample)
	if strings.Contains(string(encoded), "interface") {
		t.Fatalf("sample without a filter = %s", encoded)
	}
	sample.InterfaceFilter, sample.Interfaces = "physical", newTopInterfaceSamples(rate.NetInterfaces)
	encoded, _ = json.Marshal(sample)
	if !strings.Contains(string(encoded), `"interface_filter":"physical","interfaces":[{"name":"eno1","in_bytes_per_s":1000,"out_bytes_per_s":2,"packets_in_per_s":3,"packets_out_per_s":0,"errors_per_s":0,"drops_per_s":0}]`) {
		t.Fatalf("sample with a filter = %s", encoded)
	}
	sample.Interfaces = newTopInterfaceSamples(nil)
	if encoded, _ = json.Marshal(sample); !strings.Contains(string(encoded), `"interfaces":[]`) {
		t.Fatalf("a filter without traffic must give an empty list: %s", encoded)
	}
}

func TestConfigTopInterfaceLoadsAndRejectsBadValues(t *testing.T) {
	config, err := loadConfigAt(writeConfigFixture(t, "defaults: {top: {interface: \"physical,wg0\"}}\n"))
	if err != nil || config.Defaults.Top.Interface == nil || *config.Defaults.Top.Interface != "physical,wg0" {
		t.Fatalf("interface = %#v, error = %v", config.Defaults.Top.Interface, err)
	}
	if _, err := loadConfigAt(writeConfigFixture(t, "defaults: {top: {interface: \"all,eth0\"}}\n")); err == nil || !strings.Contains(err.Error(), "defaults.top.interface") {
		t.Fatalf("accepted all,eth0: %v", err)
	}
}

func TestTopRejectsAnInterfaceNameThatDoesNotExist(t *testing.T) {
	for _, value := range []string{"edc-missing0", "all,eth0"} {
		if code := runTop([]string{"--count", "1", "--interface", value}, "test"); code != 2 {
			t.Errorf("--interface %s exit = %d, want 2", value, code)
		}
	}
}

func TestTopJSONCarriesTheInterfaceSelection(t *testing.T) {
	// macOS는 켜진 interface만 읽으므로 켜진 것을 고른다.
	var up string
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 {
				up = iface.Name
				break
			}
		}
	}
	if up == "" {
		t.Skip("no interface other than loopback is up")
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{{up, true}, {"edc-none*", false}} {
		path := filepath.Join(t.TempDir(), "top.json")
		if code := runTop([]string{"--interval", "200ms", "--count", "1", "--interface", tc.value, "--json", path}, "test"); code != 0 {
			t.Fatalf("--interface %s exit = %d", tc.value, code)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var sample struct {
			Filter     string                `json:"interface_filter"`
			Interfaces *[]topInterfaceSample `json:"interfaces"`
			Valid      bool                  `json:"network_health_supported"`
		}
		if err := json.Unmarshal(data, &sample); err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if sample.Filter != tc.value || sample.Interfaces == nil || sample.Valid != tc.valid {
			t.Fatalf("--interface %s sample = %s", tc.value, data)
		}
		for _, item := range *sample.Interfaces {
			if item.Name != tc.value {
				t.Fatalf("--interface %s added %s", tc.value, item.Name)
			}
		}
	}
}
