package edc

import (
	"errors"
	"strings"
	"testing"
)

func TestInfoResourcesRanksRSSWithoutChangingTheProcessList(t *testing.T) {
	processes := []topProcess{{PID: 1, CPU: 100, RSS: 1 << 20, Threads: 2, Command: "cpu-heavy"}, {PID: 2, RSS: 4 << 30, Threads: 3, Command: "/apps/memory-heavy"}, {PID: 3, RSS: 2 << 30, Command: "second"}, {PID: 4, RSS: 1 << 30, Command: "third"}}
	var output strings.Builder
	printInfoResources(&output, infoMemory{Total: 8 << 30, Available: 3 << 30, Basis: "test estimate", Details: []infoMemoryDetail{{"Cache", 1 << 30}}}, nil, infoSignals{}, processes, nil, []infoCapability{{"CPU wait / I/O latency", "permission required", "missing CAP_BPF\nCAP_PERFMON"}}, false, true)
	text := output.String()
	for _, want := range []string{"Memory Status", "Available: 3.00 GB", "Basis: test estimate", "Cache: 1.00 GB", "Processes observed: 4", "Threads observed: 5 (counted for 2 of 4 processes)", "memory-heavy", "permission required", "missing CAP_BPF CAP_PERFMON"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "cpu-heavy") || strings.Contains(text, "\033") || processes[0].PID != 1 {
		t.Fatalf("only RSS leaders must appear, without color or input mutation: %s", text)
	}
	if strings.Index(text, "memory-heavy") > strings.Index(text, "second") || strings.Index(text, "second") > strings.Index(text, "third") {
		t.Fatalf("RSS order is incorrect: %s", text)
	}
}

func TestInfoResourcesDistinguishesErrorsAndUnsupportedFeatures(t *testing.T) {
	var output strings.Builder
	printInfoResources(&output, infoMemory{}, errors.New("Mach read failed"), infoSignals{}, nil, errors.New("process read failed"), []infoCapability{{"PSI", "unsupported", "Linux-only"}}, false, false)
	text := output.String()
	for _, want := range []string{"Unavailable: Mach read failed", "Unavailable: process read failed", "PSI: unsupported"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "Used:") || strings.Contains(text, "Processes observed: 0") || strings.Contains(text, "NaN") {
		t.Fatalf("unreadable data must not look like zero usage: %s", text)
	}
}

func TestInfoSnapshotCollectorsReadLocalData(t *testing.T) {
	memory, err := collectInfoMemory()
	if err != nil || memory.Total == 0 || memory.Available > memory.Total || memory.Basis == "" {
		t.Fatalf("memory = %+v, %v", memory, err)
	}
	processes, err := collectInfoProcesses()
	if err != nil || len(processes) == 0 {
		t.Fatalf("processes = %d, %v", len(processes), err)
	}
	if capabilities := collectInfoCapabilities(); len(capabilities) != 3 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestInfoCompactMemoryKeepsValuesAndMovesExplanationToVerbose(t *testing.T) {
	memory := infoMemory{Total: 8 << 30, Available: 3 << 30, Basis: "availability estimate, not pressure", Details: []infoMemoryDetail{{"File-backed", 1 << 30}, {"Wired", 2 << 30}, {"Compressed physical", 0}, {"Compressed original", 0}}}
	var compact, verbose strings.Builder
	printInfoResources(&compact, memory, nil, infoSignals{}, nil, nil, nil, false, false)
	printInfoResources(&verbose, memory, nil, infoSignals{}, nil, nil, nil, false, true)
	section := strings.Split(strings.Split(compact.String(), "Memory Status\n")[1], "\n\n")[0]
	if len(strings.Split(section, "\n")) != 3 {
		t.Fatalf("compact memory must use three value rows: %q", section)
	}
	for _, want := range []string{"Used: 5.00 GB / 8.00 GB", "Available: 3.00 GB", "File-backed: 1.00 GB", "Wired: 2.00 GB", "Compressed physical: 0.00 GB", "Compressed original: 0.00 GB"} {
		if !strings.Contains(section, want) {
			t.Fatalf("compact output lost %q: %q", want, section)
		}
	}
	if strings.Contains(compact.String(), "Basis:") || !strings.Contains(verbose.String(), "Basis: availability estimate, not pressure") || !strings.Contains(verbose.String(), "counters can overlap") {
		t.Fatal("verbose must retain the calculation and overlap notes")
	}
}

func TestInfoProcessSnapshotUsesRSSNotFootprint(t *testing.T) {
	base := []topProcess{{PID: 1, Command: "alpha", RSS: 2 << 20, Threads: 1}, {PID: 2, Command: "beta", RSS: 1 << 20, Threads: 2}}
	withFootprint := append([]topProcess(nil), base...)
	withFootprint[0].Memory, withFootprint[0].MemoryValid = 1<<20, true
	withFootprint[1].Memory, withFootprint[1].MemoryValid = 9<<30, true
	var plain, footprint strings.Builder
	printInfoResources(&plain, infoMemory{Total: 1, Available: 1}, nil, infoSignals{}, base, nil, nil, false, false)
	printInfoResources(&footprint, infoMemory{Total: 1, Available: 1}, nil, infoSignals{}, withFootprint, nil, nil, false, false)
	if plain.String() != footprint.String() {
		t.Fatalf("process output changed with footprint:\n%s\n%s", plain.String(), footprint.String())
	}
	alpha, beta := strings.Index(plain.String(), "PID 1       alpha"), strings.Index(plain.String(), "PID 2       beta")
	if alpha < 0 || beta < 0 || alpha > beta || !strings.Contains(plain.String(), "2.0 MB") || !strings.Contains(plain.String(), "1.0 MB") {
		t.Fatalf("processes are not in RSS order: %q", plain.String())
	}
}

func TestInfoCompactSystemKeepsHostIdentityAndVerboseKernel(t *testing.T) {
	details := hostDetails{Hostname: "host", System: "darwin", OS: "macOS", Version: "27", Machine: "arm64", Processor: "long kernel build string", Release: "27.0", Model: "M4", Cores: 16, MemoryTotal: 64 << 30}
	public := &publicNetworkInfo{IP: "192.0.2.1", City: "city", Region: "region", Country: "country", Timezone: "zone", Org: "provider"}
	var compact, verbose strings.Builder
	printInfo(&compact, "test", details, nil, nil, public, infoSignals{}, false, false)
	printInfo(&verbose, "test", details, nil, nil, public, infoSignals{}, false, true)
	for _, want := range []string{"host", "macOS 27", "arm64", "M4", "16 cores", "64.00 GB", "Files (edc soft/hard)", "Swap:", "192.0.2.1", "city", "region", "country", "zone", "provider"} {
		if !strings.Contains(compact.String(), want) {
			t.Fatalf("compact system lost %q", want)
		}
	}
	if strings.Contains(compact.String(), "Description :") || strings.Contains(compact.String(), details.Processor) || !strings.Contains(verbose.String(), details.Processor) || !strings.Contains(verbose.String(), details.Release) {
		t.Fatal("verbose must retain the full kernel data")
	}
}

func TestInfoListenShowsExposedPortsAndCountsLoopback(t *testing.T) {
	sockets := []listenSocket{{Proto: "tcp", Address: "127.0.0.1", Port: 80, Process: "local", PID: "1"}}
	for port := 1; port <= infoListenRows+2; port++ {
		sockets = append(sockets, listenSocket{Proto: "tcp", Address: "*", Port: 1000 + port, Process: "app", PID: "2"})
	}
	sockets = append(sockets, listenSocket{Proto: "udp", Address: "192.0.2.10", Port: 53})
	result := Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": sockets}}

	var compact strings.Builder
	printInfoListen(&compact, result, true, false, false)
	text := compact.String()
	for _, want := range []string{"TCP 8 · UDP 1 · Exposed 8 · Loopback 1", "1001   tcp  *  app (2)", "… 3 more exposed · edc listen", "└── Sockets of other accounts are hidden without root"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q가 없다:\n%s", want, text)
		}
	}
	if strings.Contains(text, "local (1)") {
		t.Fatalf("-v 없이 loopback 소켓을 나열했다:\n%s", text)
	}

	var verbose strings.Builder
	printInfoListen(&verbose, result, false, false, true)
	text = verbose.String()
	// 프로세스를 모르는 소켓도 포트는 보여 준다. 이름 자리는 없음으로 적는다.
	for _, want := range []string{"local (1)", "53     udp  192.0.2.10  -"} {
		if !strings.Contains(text, want) {
			t.Fatalf("-v에 %q가 없다:\n%s", want, text)
		}
	}
	if strings.Contains(text, "more exposed") || strings.Contains(text, "without root") {
		t.Fatalf("-v가 목록을 자르거나 쓰지 않을 안내를 붙였다:\n%s", text)
	}
}

func TestInfoInterfaceTagsNameWhatTheAddressDoesNotSay(t *testing.T) {
	cases := []struct {
		iface interfaceDetails
		want  string
	}{
		{interfaceDetails{Name: "en0", Address: "192.168.1.92", Mask: "255.255.255.0"}, ""},
		{interfaceDetails{Name: "en9", Address: "169.254.5.180", Mask: "255.255.0.0"}, "link-local"},
		{interfaceDetails{Name: "bridge101", Address: "192.168.156.0", Mask: "255.255.255.0"}, "network address"},
		{interfaceDetails{Name: "utun9", Address: "100.65.168.177", Mask: "255.255.255.255"}, "tunnel"},
		{interfaceDetails{Name: "wg0", Address: "10.8.0.0", Mask: "255.255.255.254"}, "tunnel"},
		{interfaceDetails{Name: "eth1", Address: "10.0.0.0", Mask: "255.255.255.255"}, ""},
	}
	for _, test := range cases {
		if got := strings.Join(infoInterfaceTags(test.iface), ", "); got != test.want {
			t.Errorf("%s %s/%s: tags = %q, want %q", test.iface.Name, test.iface.Address, test.iface.Mask, got, test.want)
		}
	}
}

// 기본 경로를 가진 인터페이스가 맨 위에 오고, 나머지는 이름 순서를 지킨다.
func TestInfoLocalIPListsTheDefaultRouteFirst(t *testing.T) {
	interfaces := []interfaceDetails{
		{Name: "bridge0", Address: "10.0.0.3", Mask: "255.255.255.0"},
		{Name: "en0", Address: "192.168.1.92", Mask: "255.255.255.0", Gateway: "192.168.1.1"},
		{Name: "en9", Address: "169.254.5.180", Mask: "255.255.0.0"},
	}
	var output strings.Builder
	printInfo(&output, "test", hostDetails{Hostname: "host"}, interfaces, nil, nil, infoSignals{}, false, false)
	text := output.String()
	en0, bridge, en9 := strings.Index(text, "en0 "), strings.Index(text, "bridge0 "), strings.Index(text, "en9 ")
	if en0 < 0 || en0 > bridge || bridge > en9 || !strings.Contains(text, "G/W: 192.168.1.1") || !strings.Contains(text, "255.255.0.0 · link-local") {
		t.Fatalf("local IP order or tags are wrong:\n%s", text)
	}
}

func TestInfoProcessSizeUsesTheUnitsOfTheOtherSections(t *testing.T) {
	for bytes, want := range map[uint64]string{5 << 30: "5.00 GB", 995 << 20: "995.0 MB", 30 << 20: "30.0 MB", 512 << 10: "512 KB"} {
		if got := infoProcessSize(bytes); got != want {
			t.Errorf("infoProcessSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestInfoListenReportsUnavailableProbe(t *testing.T) {
	var output strings.Builder
	printInfoListen(&output, Result{Status: StatusSkip, Summary: "ss not found\nin PATH"}, false, false, false)
	if output.String() != "\nListening Ports\n└── Unavailable: ss not found in PATH\n" {
		t.Fatalf("output = %q", output.String())
	}
}

// 기본 실행은 DNS로 IP만 받는다. 기관과 지역이 없으면 빈 칸을 늘어놓지 않고 그 줄을 뺀다.
func TestInfoPublicIPWithoutIPinfoOmitsRegion(t *testing.T) {
	var output strings.Builder
	printInfo(&output, "test", hostDetails{}, nil, nil, &publicNetworkInfo{IP: "192.0.2.1"}, infoSignals{}, false, false)
	if !strings.Contains(output.String(), "├── Public IP: 192.0.2.1\n") {
		t.Fatalf("IP 줄이 없다:\n%s", output.String())
	}
	if strings.Contains(output.String(), "Region:") || strings.Contains(output.String(), "192.0.2.1 ·") {
		t.Fatalf("없는 기관과 지역을 출력했다:\n%s", output.String())
	}
}

// python 확인은 shim에 따라 수백 ms가 걸려 -v에서만 한다. 확인하지 않았으면 없는 값을 적지 않는다.
func TestInfoRuntimeShowsPythonOnlyWhenChecked(t *testing.T) {
	var compact, verbose strings.Builder
	printInfo(&compact, "test", hostDetails{}, nil, nil, nil, infoSignals{}, false, false)
	printInfo(&verbose, "test", hostDetails{PythonVersion: "3.13.1"}, nil, nil, nil, infoSignals{}, false, true)
	if !strings.Contains(compact.String(), "├── Runtime: Go ") || strings.Contains(compact.String(), "Python") {
		t.Fatalf("기본 출력의 Runtime 줄이 틀렸다:\n%s", compact.String())
	}
	if !strings.Contains(verbose.String(), "├── Runtime: Python 3.13.1 · Go ") {
		t.Fatalf("-v에 python 버전이 없다:\n%s", verbose.String())
	}
}
