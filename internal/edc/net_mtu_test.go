package edc

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// fakeMTUPath는 pathMTU까지 응답하고 그보다 크면 above를 돌려주는 경로다.
func fakeMTUPath(pathMTU int, above mtuAttempt) mtuSender {
	return func(_ context.Context, size int) mtuAttempt {
		if size <= pathMTU {
			return mtuAttempt{Size: size, Outcome: mtuReply}
		}
		attempt := above
		attempt.Size = size
		return attempt
	}
}

func fakeMTUDeps(interfaceMTU int, send mtuSender) mtuDeps {
	return mtuDeps{
		goos:           "linux",
		resolve:        func(context.Context, string) (string, error) { return "192.0.2.10", nil },
		routeInterface: func(context.Context, string) (string, Result) { return "eth0", Result{} },
		interfaceMTU:   func(string) (int, error) { return interfaceMTU, nil },
		lookPath:       func(path string) (string, error) { return path, nil },
		sender:         func(string, net.IP) mtuSender { return send },
	}
}

func mtuSizes(attempts []mtuAttempt) []int {
	sizes := make([]int, 0, len(attempts))
	for _, attempt := range attempts {
		sizes = append(sizes, attempt.Size)
	}
	return sizes
}

func TestNetMTUPassesInTwoProbesWhenThePathMatchesTheInterface(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(1500, mtuAttempt{Outcome: mtuTooBig})), "example.com", 0)
	if result.Status != StatusPass || result.Metrics["path_mtu"] != 1500 || result.Metrics["gap"] != 0 || result.Metrics["mss"] != 1460 {
		t.Fatalf("result = %+v", result)
	}
	attempts := result.Metrics["attempts"].([]mtuAttempt)
	if got := mtuSizes(attempts); !reflect.DeepEqual(got, []int{576, 1500}) {
		t.Errorf("sizes = %v", got)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

// 큰 packet이 ICMP 없이 사라지면 블랙홀이다. 무응답은 한 번 더 보내 손실과 구분한다.
func TestNetMTUFindsABlackHole(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(1438, mtuAttempt{Outcome: mtuTimeout})), "example.com", 0)
	if result.Status != StatusWarn || result.Metrics["path_mtu"] != 1438 || result.Metrics["gap"] != 62 || result.Metrics["mss"] != 1398 {
		t.Fatalf("result = %+v", result)
	}
	if result.Metrics["black_hole"] != true || result.Metrics["limit"] != "timeout" {
		t.Errorf("metrics = %v", result.Metrics)
	}
	attempts := result.Metrics["attempts"].([]mtuAttempt)
	if attempts[1].Size != 1500 || attempts[2].Size != 1500 {
		t.Errorf("a timeout must be retried once: %v", mtuSizes(attempts))
	}
	joined := strings.Join(result.Warnings, "\n")
	for _, want := range []string{"black hole", "IPsec ESP", "1398"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings miss %q: %v", want, result.Warnings)
		}
	}
}

// 한 번 잃은 응답은 블랙홀로 보지 않는다.
func TestNetMTURetriesALostReply(t *testing.T) {
	lost := false
	send := func(_ context.Context, size int) mtuAttempt {
		if size == 1500 && !lost {
			lost = true
			return mtuAttempt{Size: size, Outcome: mtuTimeout}
		}
		return mtuAttempt{Size: size, Outcome: mtuReply}
	}
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, send), "example.com", 0)
	if result.Status != StatusPass || result.Metrics["black_hole"] != false {
		t.Fatalf("result = %+v", result)
	}
}

// Linux ping은 kernel이 아는 경로 MTU를 함께 알려 준다. 그 크기로 바로 건너뛴다.
func TestNetMTUJumpsToTheReportedMTU(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(1420, mtuAttempt{Outcome: mtuTooBig, MTU: 1420})), "example.com", 0)
	attempts := result.Metrics["attempts"].([]mtuAttempt)
	if got := mtuSizes(attempts); !reflect.DeepEqual(got, []int{576, 1500, 1420}) {
		t.Errorf("sizes = %v", got)
	}
	if result.Status != StatusWarn || result.Metrics["black_hole"] != false || result.Metrics["limit"] != "too_big" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "kernel") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

func TestNetMTUReportsTheRouterThatSentFragmentationNeeded(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(1400, mtuAttempt{Outcome: mtuFragNeeded, MTU: 1400})), "example.com", 0)
	if result.Metrics["path_mtu"] != 1400 || result.Metrics["limit"] != "frag_needed" || result.Metrics["black_hole"] != false {
		t.Fatalf("metrics = %v", result.Metrics)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "MTU 1400") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

// 응답 없는 크기의 이분 탐색이 kernel 알림 없이도 정확한 경계에 닿는지 본다.
func TestNetMTUBisectsToTheExactBoundary(t *testing.T) {
	for _, pathMTU := range []int{576, 577, 1000, 1499} {
		result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(pathMTU, mtuAttempt{Outcome: mtuTooBig})), "example.com", 0)
		if result.Metrics["path_mtu"] != pathMTU {
			t.Errorf("path %d: metrics = %v", pathMTU, result.Metrics)
		}
	}
}

func TestNetMTUSkipsWhenICMPIsBlocked(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(0, mtuAttempt{Outcome: mtuTimeout})), "example.com", 0)
	if result.Status != StatusSkip || !strings.Contains(result.Summary, "576") {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "edc tcp check example.com:<port>") {
		t.Errorf("warnings = %v", result.Warnings)
	}
	if _, ok := result.Metrics["path_mtu"]; ok {
		t.Errorf("an unmeasured path must not carry a path MTU: %v", result.Metrics)
	}
}

func TestNetMTUSkipsWhenPingCannotSetDontFragment(t *testing.T) {
	deps := fakeMTUDeps(1500, fakeMTUPath(0, mtuAttempt{Outcome: mtuDenied, detail: "ping6: IPV6_DONTFRAG: Operation not permitted"}))
	deps.goos = "darwin"
	deps.resolve = func(context.Context, string) (string, error) { return "2001:db8::1", nil }
	result := probeNetMTUWith(context.Background(), deps, "example.com", 0)
	if result.Status != StatusSkip || !strings.Contains(result.Summary, "IPV6_DONTFRAG") || len(result.Warnings) != 1 {
		t.Fatalf("result = %+v", result)
	}
	attempts := result.Metrics["attempts"].([]mtuAttempt)
	if attempts[0].Size != 1280 {
		t.Errorf("IPv6 starts at 1280: %v", mtuSizes(attempts))
	}
}

// --max가 interface MTU보다 작고 그 크기까지 응답하면 경로 MTU는 하한만 안다.
func TestNetMTUStopsAtMax(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1500, fakeMTUPath(1500, mtuAttempt{Outcome: mtuTooBig})), "example.com", 1400)
	if result.Status != StatusPass || result.Metrics["at_least"] != true || result.Metrics["path_mtu"] != 1400 {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := result.Metrics["gap"]; ok {
		t.Errorf("a capped search must not claim a gap: %v", result.Metrics)
	}
}

// DF를 켠 packet은 interface MTU를 넘지 못하므로 더 큰 --max는 interface MTU로 줄인다.
func TestNetMTUNeverTriesAboveTheInterfaceMTU(t *testing.T) {
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(1280, fakeMTUPath(1280, mtuAttempt{Outcome: mtuTooBig})), "example.com", 9000)
	attempts := result.Metrics["attempts"].([]mtuAttempt)
	if got := mtuSizes(attempts); !reflect.DeepEqual(got, []int{576, 1280}) || result.Status != StatusPass {
		t.Fatalf("sizes = %v, result = %+v", got, result)
	}
}

// Linux loopback은 MTU 65536을 보고하지만 IP packet은 65535를 넘지 못한다. 그 위를 보내면 ping이 인자를
// 거절하고, 그 거절을 무응답으로 읽으면 멀쩡한 경로가 블랙홀로 판정된다.
func TestNetMTUNeverTriesAboveTheIPPacketLimit(t *testing.T) {
	rejected := mtuAttempt{Outcome: mtuError, detail: "ping: packet size 65508 is too large. Maximum is 65507"}
	result := probeNetMTUWith(context.Background(), fakeMTUDeps(65536, fakeMTUPath(65535, rejected)), "example.com", 0)
	attempts := result.Metrics["attempts"].([]mtuAttempt)
	if got := mtuSizes(attempts); !reflect.DeepEqual(got, []int{576, 65535}) {
		t.Fatalf("sizes = %v", got)
	}
	if result.Status != StatusPass || result.Metrics["max"] != 65535 || result.Metrics["black_hole"] != false || len(result.Warnings) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

// 사용자가 취소하면 그때까지 찾은 범위만 남기고 warn으로 끝낸다.
func TestNetMTUStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	send := func(_ context.Context, size int) mtuAttempt {
		if size == 1500 {
			cancel()
			return mtuAttempt{Size: size, Outcome: mtuTimeout}
		}
		return mtuAttempt{Size: size, Outcome: mtuReply}
	}
	result := probeNetMTUWith(ctx, fakeMTUDeps(1500, send), "example.com", 0)
	if result.Status != StatusWarn || result.Metrics["complete"] != false || result.Metrics["path_mtu"] != 576 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "between 576 and 1500") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

// 블랙홀에서 시간이 모자라 멈춘 탐색의 pathMTU는 하한일 뿐이다. 그 값으로 차이 원인이나 MSS를 짚지 않는다.
func TestNetMTUDoesNotDiagnoseFromAnIncompleteSearch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timeouts := 0
	send := func(_ context.Context, size int) mtuAttempt {
		if size <= 1000 {
			return mtuAttempt{Size: size, Outcome: mtuReply}
		}
		timeouts++
		if timeouts == 3 {
			cancel()
		}
		return mtuAttempt{Size: size, Outcome: mtuTimeout}
	}
	result := probeNetMTUWith(ctx, fakeMTUDeps(1500, send), "example.com", 0)
	if result.Status != StatusWarn || result.Metrics["complete"] != false || result.Metrics["black_hole"] != true || result.Metrics["path_mtu"] != 576 {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := result.Metrics["gap"]; ok {
		t.Errorf("an unfinished search must not claim a gap: %v", result.Metrics)
	}
	if result.Summary != T("mtu.summary.incomplete", 576, 1499, "eth0", 1500) {
		t.Errorf("summary = %q", result.Summary)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "between 576 and 1499") {
		t.Errorf("only the incomplete range belongs in the warnings: %v", result.Warnings)
	}
}

func TestNetMTUReportsProgress(t *testing.T) {
	var lines []string
	ctx := withProbeObserver(context.Background(), func(line string) { lines = append(lines, line) })
	probeNetMTUWith(ctx, fakeMTUDeps(1500, fakeMTUPath(1400, mtuAttempt{Outcome: mtuTooBig, MTU: 1400})), "example.com", 0)
	want := []string{"probing 576 bytes", "probing 1500 bytes · 576 replied", "probing 1400 bytes · 576 replied · 1500 failed"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("lines = %q", lines)
	}
}

func TestNetMTUPassesRouteFailuresThrough(t *testing.T) {
	deps := fakeMTUDeps(1500, nil)
	deps.routeInterface = func(context.Context, string) (string, Result) {
		return "", Result{Probe: "net.route", Status: StatusFail, Summary: "no route"}
	}
	result := probeNetMTUWith(context.Background(), deps, "example.com", 0)
	if result.Probe != netMTUProbeID || result.Status != StatusFail || result.Summary != "no route" {
		t.Fatalf("result = %+v", result)
	}
	deps.resolve = func(context.Context, string) (string, error) { return "", errors.New("no such host") }
	if result := probeNetMTUWith(context.Background(), deps, "example.com", 0); result.Status != StatusFail || result.Error.Kind != "dns" {
		t.Fatalf("result = %+v", result)
	}
}

// 두 OS의 ping 출력은 실제 실행에서 옮겼다. 둘 다 너무 큰 packet과 무응답에서 같은 exit code를 낸다.
func TestClassifyMTUPing(t *testing.T) {
	exit := exec.Command("false").Run()
	cases := []struct {
		name   string
		output string
		err    error
		want   mtuAttempt
	}{
		{"linux reply", "PING 8.8.8.8 (8.8.8.8) 1472(1500) bytes of data.\n1480 bytes from 8.8.8.8: icmp_seq=1 ttl=114 time=32.8 ms\n\n--- 8.8.8.8 ping statistics ---\n1 packets transmitted, 1 received, 0% packet loss, time 0ms", nil, mtuAttempt{Outcome: mtuReply}},
		{"linux local", "PING 8.8.8.8 (8.8.8.8) 1473(1501) bytes of data.\nping: sendmsg: Message too long\n\n--- 8.8.8.8 ping statistics ---\n1 packets transmitted, 0 received, +1 errors, 100% packet loss, time 0ms", exit, mtuAttempt{Outcome: mtuTooBig}},
		{"linux cached", "ping: local error: message too long, mtu=1400", exit, mtuAttempt{Outcome: mtuTooBig, MTU: 1400}},
		{"linux router", "From 10.9.0.1 icmp_seq=1 Frag needed and DF set (mtu = 1400)", exit, mtuAttempt{Outcome: mtuFragNeeded, MTU: 1400}},
		{"linux ipv6 router", "From fd00::1 icmp_seq=1 Packet too big: mtu=1400", exit, mtuAttempt{Outcome: mtuFragNeeded, MTU: 1400}},
		{"linux timeout", "PING 10.0.0.2 (10.0.0.2) 1472(1500) bytes of data.\n\n--- 10.0.0.2 ping statistics ---\n1 packets transmitted, 0 received, 100% packet loss, time 0ms", exit, mtuAttempt{Outcome: mtuTimeout}},
		{"macos reply", "PING 100.83.200.248 (100.83.200.248): 1252 data bytes\n1260 bytes from 100.83.200.248: icmp_seq=0 ttl=64 time=5.058 ms", nil, mtuAttempt{Outcome: mtuReply}},
		{"macos local", "ping: sendto: Message too long\nPING 1.1.1.1 (1.1.1.1): 1473 data bytes", exit, mtuAttempt{Outcome: mtuTooBig}},
		{"macos router", "PING 10.0.0.2 (10.0.0.2): 1472 data bytes\n36 bytes from 10.9.0.1: frag needed and DF set (MTU 1400)", exit, mtuAttempt{Outcome: mtuFragNeeded, MTU: 1400}},
		{"macos ipv6 root", "ping6: IPV6_DONTFRAG: Operation not permitted", exit, mtuAttempt{Outcome: mtuDenied, detail: "ping6: IPV6_DONTFRAG: Operation not permitted"}},
		{"unknown host", "ping: nowhere.invalid: Name or service not known\nping: unknown host", exit, mtuAttempt{Outcome: mtuError, detail: "ping: nowhere.invalid: Name or service not known"}},
		{"linux too large", "ping: packet size 65508 is too large. Maximum is 65507", exit, mtuAttempt{Outcome: mtuError, detail: "ping: packet size 65508 is too large. Maximum is 65507"}},
		{"macos too large", "ping: packet size too large: 65508 > 65507", exit, mtuAttempt{Outcome: mtuError, detail: "ping: packet size too large: 65508 > 65507"}},
		{"killed", "PING 10.0.0.2 (10.0.0.2): 1452 data bytes", context.DeadlineExceeded, mtuAttempt{Outcome: mtuTimeout}},
		{"not started", "", errors.New("fork/exec /sbin/ping: permission denied"), mtuAttempt{Outcome: mtuError, detail: "fork/exec /sbin/ping: permission denied"}},
	}
	for _, item := range cases {
		if got := classifyMTUPing(item.output, item.err); got != item.want {
			t.Errorf("%s: got %+v, want %+v", item.name, got, item.want)
		}
	}
}

func TestMTUPingCommand(t *testing.T) {
	cases := []struct {
		goos string
		ipv6 bool
		path string
		args []string
	}{
		{"linux", false, "ping", []string{"-n", "-M", "do", "-c", "1", "-W", "1", "-s", "1472", "192.0.2.1"}},
		{"linux", true, "ping", []string{"-6", "-n", "-M", "do", "-c", "1", "-W", "1", "-s", "1472", "192.0.2.1"}},
		{"darwin", false, "/sbin/ping", []string{"-n", "-D", "-c", "1", "-W", "1000", "-s", "1472", "192.0.2.1"}},
		{"darwin", true, "/sbin/ping6", []string{"-n", "-D", "-m", "-c", "1", "-s", "1472", "192.0.2.1"}},
	}
	for _, item := range cases {
		path, args := mtuPingCommand(item.goos, item.ipv6, 1472, "192.0.2.1")
		if path != item.path || !reflect.DeepEqual(args, item.args) {
			t.Errorf("%s ipv6=%v: %s %v", item.goos, item.ipv6, path, args)
		}
	}
}

func TestMTUGapHint(t *testing.T) {
	for gap, want := range map[int]string{8: "PPPoE", 60: "WireGuard", 80: "IPsec ESP", 220: "nested tunnels"} {
		if got := mtuGapHint(gap); !strings.Contains(got, want) {
			t.Errorf("gap %d: %q", gap, got)
		}
	}
}
