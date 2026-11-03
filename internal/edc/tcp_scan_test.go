package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParsePortSetSortsAndDeduplicates(t *testing.T) {
	for _, test := range []struct {
		spec string
		want []int
	}{
		{"443,22,22,80", []int{22, 80, 443}},
		{"8000-8003, 22", []int{22, 8000, 8001, 8002, 8003}},
		{"1-3,2-4", []int{1, 2, 3, 4}},
		{"65535", []int{65535}},
	} {
		got, err := parsePortSet(test.spec)
		if err != nil {
			t.Fatalf("parsePortSet(%q) error = %v", test.spec, err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("parsePortSet(%q) = %v, want %v", test.spec, got, test.want)
		}
	}
}

func TestParsePortSetRejectsBadEntries(t *testing.T) {
	for _, spec := range []string{"", " , ", "abc", "0", "65536", "80-22", "22-", "-22", "1-70000"} {
		if ports, err := parsePortSet(spec); err == nil {
			t.Errorf("parsePortSet(%q) = %v, want an error", spec, ports)
		}
	}
}

func TestPortRangesFoldsRuns(t *testing.T) {
	for _, test := range []struct {
		ports []int
		want  string
	}{
		{nil, ""},
		{[]int{22}, "22"},
		{[]int{1, 2, 3, 5, 7, 8}, "1-3, 5, 7-8"},
		{[]int{80, 443}, "80, 443"},
	} {
		if got := portRanges(test.ports); got != test.want {
			t.Errorf("portRanges(%v) = %q, want %q", test.ports, got, test.want)
		}
	}
}

func TestParseScanHostTakesAHostWithoutAPort(t *testing.T) {
	for input, want := range map[string]string{
		"example.com": "example.com",
		" 10.0.0.5 ":  "10.0.0.5",
		"::1":         "::1",
		"[::1]":       "::1",
	} {
		got, err := parseScanHost(input)
		if err != nil || got != want {
			t.Errorf("parseScanHost(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "example.com:443", "[::1]:443", "[not-an-ip]"} {
		if got, err := parseScanHost(input); err == nil {
			t.Errorf("parseScanHost(%q) = %q, want an error", input, got)
		}
	}
	_, err := parseScanHost("example.com:443")
	if err == nil || !strings.Contains(err.Error(), "--ports") {
		t.Errorf("a host:port target must point at --ports, got %v", err)
	}
}

func TestTCPScanDefaultPortsCoverAuditAndNamedPorts(t *testing.T) {
	ports := tcpScanDefaultPorts()
	if !sortedInts(ports) {
		t.Fatalf("default ports are not sorted: %v", ports)
	}
	for port := range auditSensitivePorts {
		if !containsInt(ports, port) {
			t.Errorf("default ports miss the audit port %d", port)
		}
	}
	for _, port := range []int{22, 80, 443} {
		if !containsInt(ports, port) {
			t.Errorf("default ports miss %d", port)
		}
	}
}

// 실제 loopback으로 열림과 거부를, 응답하지 않는 dial로 응답 없음을 확인한다.
func TestScannerClassifiesOpenRefusedAndNoReply(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	openPort := listener.Addr().(*net.TCPAddr).Port
	closedPort := freeClosedPort(t)

	scanner := tcpScanner{dial: dialTCPOnce, parallel: 4, timeout: 2 * time.Second}
	results := scanner.scan(context.Background(), "127.0.0.1", []int{openPort, closedPort})
	states := map[int]portState{}
	for _, result := range results {
		states[result.Port] = result.State
	}
	if states[openPort] != portOpen {
		t.Errorf("open port %d = %s, want open", openPort, states[openPort])
	}
	if states[closedPort] != portRefused {
		t.Errorf("closed port %d = %s, want refused", closedPort, states[closedPort])
	}

	silent := tcpScanner{dial: blockingDial, parallel: 2, timeout: 30 * time.Millisecond}
	results = silent.scan(context.Background(), "192.0.2.1", []int{1, 2, 3})
	for _, result := range results {
		if result.State != portNoReply {
			t.Errorf("silent port %d = %s, want no reply", result.Port, result.State)
		}
	}
}

func TestScannerKeepsOtherErrorsWithTheirMessage(t *testing.T) {
	scanner := tcpScanner{dial: func(context.Context, string) error { return errors.New("connect: no route to host") }, parallel: 1, timeout: time.Second}
	results := scanner.scan(context.Background(), "192.0.2.1", []int{80})
	if results[0].State != portError || results[0].Error != "connect: no route to host" {
		t.Fatalf("result = %+v, want error with its message", results[0])
	}
}

// 부모 ctx가 먼저 끝나면 진행 중이던 시도와 꺼내지 않은 port는 모두 not scanned다. no reply로 세면 관측하지 않은 것을 사실처럼 적게 된다.
func TestScannerMarksPortsNotScannedWhenTheBudgetEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	scanner := tcpScanner{dial: blockingDial, parallel: 2, timeout: 5 * time.Second}
	results := scanner.scan(ctx, "192.0.2.1", []int{1, 2, 3, 4, 5, 6})
	counts := countPorts(results)
	if counts.notScanned != 6 || counts.noReply != 0 {
		t.Fatalf("counts = %+v, want 6 not scanned and 0 no reply", counts)
	}
}

func TestScannerReportsProgress(t *testing.T) {
	var last [3]int
	scanner := tcpScanner{
		dial:     func(context.Context, string) error { return nil },
		parallel: 3, timeout: time.Second,
		observe: func(done, total, open int) { last = [3]int{done, total, open} },
	}
	scanner.scan(context.Background(), "192.0.2.1", []int{1, 2, 3, 4, 5})
	if last != [3]int{5, 5, 5} {
		t.Fatalf("last progress = %v, want 5/5 with 5 open", last)
	}
}

func TestScanProgressReporterWritesTheProbeLine(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	if scanProgressReporter(context.Background()) != nil {
		t.Fatal("without an observer there is no reporter")
	}
	var lines []string
	ctx := withProbeObserver(context.Background(), func(line string) { lines = append(lines, line) })
	report := scanProgressReporter(ctx)
	report(1, 5, 0)
	report(2, 5, 1)
	report(5, 5, 2)
	if len(lines) < 2 || lines[0] != "1/5 ports · open 0" || lines[len(lines)-1] != "5/5 ports · open 2" {
		t.Fatalf("progress lines = %q, want the first and the last update", lines)
	}
}

func TestScanResultStatusRules(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	options := tcpScanOptions{common: commonOptions{timeout: time.Minute}, parallel: 8}
	port := func(number int, state portState) portResult { return portResult{Port: number, State: state} }

	mixed := scanResult(time.Now(), "example.test", "192.0.2.10", []portResult{port(22, portOpen), port(80, portRefused), port(81, portNoReply)}, options, 2*time.Second)
	if mixed.Status != StatusPass || mixed.Next != "" {
		t.Errorf("mixed = %s next %q, want pass without next", mixed.Status, mixed.Next)
	}
	for _, want := range []string{"1 open (22)", "1 refused", "1 no reply", "3 ports in", "peer 192.0.2.10"} {
		if !strings.Contains(mixed.Summary, want) {
			t.Errorf("mixed summary %q misses %q", mixed.Summary, want)
		}
	}
	if len(mixed.Warnings) != 1 {
		t.Errorf("mixed warnings = %v, want the no reply note only", mixed.Warnings)
	}
	if got := mixed.Metrics["open_ports"]; !reflect.DeepEqual(got, []int{22}) {
		t.Errorf("open_ports = %v", got)
	}

	silent := scanResult(time.Now(), "example.test", "192.0.2.10", []portResult{port(80, portNoReply), port(443, portNoReply)}, options, 2*time.Second)
	if silent.Status != StatusWarn || silent.Next != "edc tcp check --diagnose example.test:80" {
		t.Errorf("silent = %s next %q, want warn with a diagnose next", silent.Status, silent.Next)
	}
	if !strings.HasPrefix(silent.Summary, "no port answered within 2s") {
		t.Errorf("silent summary must say why: %q", silent.Summary)
	}

	allRefused := scanResult(time.Now(), "10.0.0.5", "10.0.0.5", []portResult{port(80, portRefused)}, options, 2*time.Second)
	if allRefused.Status != StatusPass || strings.Contains(allRefused.Summary, "peer") {
		t.Errorf("all refused = %s %q, want pass without a peer for an IP target", allRefused.Status, allRefused.Summary)
	}

	partial := scanResult(time.Now(), "example.test", "192.0.2.10", []portResult{port(22, portOpen), port(23, portNotScanned)}, options, 2*time.Second)
	if partial.Status != StatusWarn || len(partial.Warnings) != 1 || !strings.Contains(partial.Warnings[0], "1 of 2 ports") {
		t.Errorf("partial = %s %v, want warn with the partial note", partial.Status, partial.Warnings)
	}

	// 예산이 먼저 끝나 아무 port도 보지 못한 scan은 실패가 아니라 미확인이다.
	nothing := scanResult(time.Now(), "example.test", "192.0.2.10", []portResult{port(22, portNotScanned)}, options, 2*time.Second)
	if nothing.Status != StatusWarn || nothing.Error != nil || len(nothing.Warnings) != 1 || !strings.Contains(nothing.Warnings[0], "no port was checked") {
		t.Errorf("nothing scanned = %s %+v %v, want warn with the none-checked note", nothing.Status, nothing.Error, nothing.Warnings)
	}

	// 일부 port만 오류를 냈고 나머지는 예산으로 보지 못했으면 모두 실패했다고 단정하지 않는다.
	cut := scanResult(time.Now(), "example.test", "192.0.2.10", []portResult{{Port: 22, State: portError, Error: "no route to host"}, port(23, portNotScanned)}, options, 2*time.Second)
	if cut.Status != StatusWarn || cut.Error != nil || len(cut.Warnings) != 2 || !strings.Contains(cut.Warnings[0], "no route to host") || !strings.Contains(cut.Warnings[1], "1 of 2 ports") {
		t.Errorf("cut = %s %+v %v, want warn with the error and partial notes", cut.Status, cut.Error, cut.Warnings)
	}

	broken := scanResult(time.Now(), "example.test", "192.0.2.10", []portResult{{Port: 22, State: portError, Error: "no route to host"}}, options, 2*time.Second)
	if broken.Status != StatusFail || broken.Error == nil || broken.Error.Kind != "network" || !strings.Contains(broken.Summary, "no route to host") {
		t.Errorf("all errors = %s %+v %q, want fail in the network phase with the cause", broken.Status, broken.Error, broken.Summary)
	}
}

func TestScanEvidenceNamesOpenPortsAndFoldsTheRest(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	results := []portResult{
		{Port: 22, State: portOpen, Elapsed: 12 * time.Millisecond},
		{Port: 8081, State: portOpen, Elapsed: 300 * time.Microsecond},
		{Port: 1, State: portNoReply}, {Port: 2, State: portNoReply}, {Port: 3, State: portNoReply},
		{Port: 25, State: portRefused},
		{Port: 26, State: portError, Error: "boom"},
		{Port: 27, State: portNotScanned},
	}
	evidence := scanEvidence(countPorts(results), results)
	got := map[string]string{}
	for _, item := range evidence {
		got[item.Label] = item.Value
	}
	if got["open"] != "   22  ssh  12ms\n 8081  300µs" {
		t.Errorf("open evidence = %q", got["open"])
	}
	if got["no reply"] != "1-3" || got["refused"] != "25" || got["not scanned"] != "27" || got["errors"] != "26  boom" {
		t.Errorf("evidence = %v", got)
	}
}

func TestPortListFoldsTheTail(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	if got := portList([]int{1, 2, 3, 4, 5, 6, 7}, 5); got != "1, 2, 3, 4, 5, +2 more" {
		t.Errorf("portList = %q", got)
	}
	if got := portList([]int{80, 443}, 5); got != "80, 443" {
		t.Errorf("portList = %q", got)
	}
}

// 명령 전체를 --json으로 돌려 보고 결과 하나에 port 목록이 실리는지 본다.
func TestRunTCPScanWritesOneResultWithThePorts(t *testing.T) {
	previousConfig := activeConfig
	activeConfig = edcConfig{}
	defer func() { activeConfig = previousConfig }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	openPort := listener.Addr().(*net.TCPAddr).Port
	closedPort := freeClosedPort(t)

	path := filepath.Join(t.TempDir(), "scan.json")
	args := []string{"--json", path, "--ports", fmt.Sprintf("%d,%d", openPort, closedPort), "--parallel", "2", "127.0.0.1"}
	if code := runTCPScan(args, "test"); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Probe != tcpScanProbeID || report.Results[0].Status != StatusPass {
		t.Fatalf("results = %+v", report.Results)
	}
	metrics := report.Results[0].Metrics
	if metrics["open"] != float64(1) || metrics["refused"] != float64(1) || metrics["parallel"] != float64(2) {
		t.Errorf("metrics = %v", metrics)
	}
	ports, _ := metrics["ports"].([]interface{})
	if len(ports) != 2 {
		t.Errorf("ports metric = %v", metrics["ports"])
	}
}

func TestRunTCPScanRejectsBadOptions(t *testing.T) {
	previousConfig := activeConfig
	activeConfig = edcConfig{}
	defer func() { activeConfig = previousConfig }()
	for _, args := range [][]string{
		{"--ports", "abc", "127.0.0.1"},
		{"--parallel", "0", "127.0.0.1"},
		{"--parallel", strconv.Itoa(tcpScanMaxParallel + 1), "127.0.0.1"},
	} {
		if code := runTCPScan(args, "test"); code != 2 {
			t.Errorf("runTCPScan(%v) = %d, want 2", args, code)
		}
	}
	path := filepath.Join(t.TempDir(), "scan.json")
	if code := runTCPScan([]string{"--json", path, "--ports", "80", "127.0.0.1:80"}, "test"); code != 1 {
		t.Errorf("a host:port target must fail as an input error, got exit %d", code)
	}
}

func blockingDial(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// freeClosedPort는 방금 닫은 loopback port를 돌려준다. 그 port로의 connect는 RST를 받는다.
func freeClosedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func sortedInts(values []int) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
