package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var ctrTestNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// ctrTestPorts는 8080을 host 80에 publish하고 80은 EXPOSE만 한 container다.
const ctrTestPorts = `{"80/tcp":null,"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"80"}]}`

// ctrTestImageID는 container가 실행 중인 이미지이고, ctrTestImageCreated는 그 이미지를 만든 시각이다.
var (
	ctrTestImageID      = "sha256:" + strings.Repeat("a", 64)
	ctrTestImageCreated = ctrTestNow.Add(-77 * 24 * time.Hour)
)

func ctrInspectLine(status string, restarts int, oom bool, exitCode int, health string) string {
	started := ctrTestNow.Add(-90 * time.Minute).Format(time.RFC3339Nano)
	finished := "0001-01-01T00:00:00Z"
	pid := 100
	if status != "running" {
		finished = ctrTestNow.Add(-time.Minute).Format(time.RFC3339Nano)
		pid = 0
	}
	return strings.Join([]string{itoa(pid), "/web", status, started, finished, itoa(restarts), boolText(oom), itoa(exitCode), "nginx:1.27", "0123456789abcdef0123456789abcdef", health, ctrTestPorts, "unless-stopped:0", ctrTestImageID}, "|")
}

// ctrImageLine은 docker image inspect --format ctrImageFormat의 출력이다.
func ctrImageLine(id string, created time.Time) string {
	return id + "|" + created.Format(time.RFC3339Nano) + "\n"
}

func ctrInspectLineWithPorts(ports string) string {
	return ctrInspectField(ctrInspectLine("running", 0, false, 0, ""), 11, ports)
}

func ctrInspectField(line string, index int, value string) string {
	fields := strings.Split(line, "|")
	fields[index] = value
	return strings.Join(fields, "|")
}

// ctrHealthJSON은 docker inspect --format '{{json .State.Health}}'의 출력이다.
func ctrHealthJSON(status string, streak int, checks ...ctrHealthCheck) string {
	data, _ := json.Marshal(ctrHealth{Status: status, FailingStreak: streak, Log: checks})
	return string(data)
}

func itoa(value int) string { return strconv.Itoa(value) }

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func ctrTestSources(inspect string, inspectErr error) ctrSources {
	return ctrSources{
		euid:    0,
		docker:  func() bool { return true },
		inspect: func(context.Context, string) (string, error) { return inspect, inspectErr },
		cgroupPath: func(pid int) (string, error) {
			return "/system.slice/docker-0123.scope", nil
		},
		limits: func(path string) ctrLimits {
			used, maximum, oom, kill := uint64(512<<20), uint64(1<<30), uint64(0), uint64(0)
			return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path,
				Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
				Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: &oom, OOMKill: &kill},
				CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 1000, Throttled: 10, ThrottledUS: 5000},
				Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 12, Max: 4096}}
		},
		processes: func(context.Context, string, int) ([]topProcess, error) {
			return []topProcess{{PID: 101, CPU: 3, RSS: 2 << 20, Command: "nginx"}, {PID: 100, CPU: 1, RSS: 4 << 20, Command: "nginx"}}, nil
		},
		fdLimit: func(int) topFDLimit { return svcAvailableFD(10, 1024) },
		listen: func(int) ([]listenSocket, error) {
			return []listenSocket{{Proto: "tcp", Address: "0.0.0.0", Port: 8080, PID: "101", Process: "nginx"}, {Proto: "tcp", Address: "127.0.0.1", Port: 5432, PID: "999", Process: "postgres"}}, nil
		},
		logs: func(context.Context, string, int) (string, error) {
			return "2026-10-12T11:59:00Z info ready\n2026-10-12T11:59:30Z [error] upstream timed out\n2026-10-12T11:59:40Z info ok\n", nil
		},
		health: func(context.Context, string) (string, error) {
			return ctrHealthJSON("healthy", 0, ctrHealthCheck{Start: ctrTestNow.Add(-time.Minute), Output: "ok\n"}, ctrHealthCheck{Start: ctrTestNow.Add(-30 * time.Second), Output: "ok\n"}), nil
		},
		image: func(context.Context, string) (string, error) {
			return ctrImageLine(ctrTestImageID, ctrTestImageCreated), nil
		},
		disk: func(context.Context, string) (string, error) {
			return ctrTestDisk, nil
		},
		fileSize: func(string) (int64, error) { return 540440, nil },
		now:      func() time.Time { return ctrTestNow },
	}
}

// ctrTestDisk는 docker inspect --size --format ctrDiskFormat의 출력이다. rotation 없는 json-file log를 쓴다.
const ctrTestDisk = "4096|json-file|/var/lib/docker/containers/0123/0123-json.log|{}"

func ctrByID(t *testing.T, results []Result, id string) Result {
	t.Helper()
	for _, result := range results {
		if result.Probe == "ctr."+id {
			return result
		}
	}
	t.Fatalf("ctr.%s가 없다: %+v", id, results)
	return Result{}
}

func TestParseCtrInspect(t *testing.T) {
	inspect, err := parseCtrInspect(`4321|/web|running|2026-10-12T10:30:00.123456789Z|0001-01-01T00:00:00Z|2|true|137|nginx:1.27|abcdef0123456789|healthy|{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5432"}],"80/tcp":null,"53/udp":[{"HostIp":"0.0.0.0","HostPort":"53"},{"HostIp":"::","HostPort":"53"}]}|on-failure:5|sha256:fedcba` + "\n")
	if err != nil || inspect.PID != 4321 || inspect.Name != "web" || inspect.Status != "running" || inspect.RestartCount != 2 || !inspect.OOMKilled || inspect.ExitCode != 137 || inspect.Image != "nginx:1.27" || inspect.ID != "abcdef0123456789" || inspect.Health != "healthy" || inspect.RestartPolicy != "on-failure" || inspect.RestartMax != 5 || inspect.ImageID != "sha256:fedcba" {
		t.Fatalf("inspect = %+v, %v", inspect, err)
	}
	if inspect.StartedAt.Year() != 2026 || !inspect.FinishedAt.IsZero() {
		t.Fatalf("times = %v %v", inspect.StartedAt, inspect.FinishedAt)
	}
	// EXPOSE만 한 80은 빠지고, port 순서로 놓이며, IPv6 host는 대괄호로 감싼다.
	if got := fmt.Sprintf("%v", inspect.Ports); got != "[{udp 53 [0.0.0.0:53 [::]:53]} {tcp 5432 [127.0.0.1:5432]}]" {
		t.Fatalf("ports = %s", got)
	}
	// network를 읽지 못한 container는 ports 칸이 비고, 멈춘 container는 null이다. HostConfig를 읽지 못하면 정책 칸이 빈다.
	for _, empty := range []string{"", "null", "{}"} {
		if inspect, err := parseCtrInspect("1|/web|exited|t|t|0|false|0|img|id||" + empty + "||"); err != nil || len(inspect.Ports) != 0 || inspect.RestartPolicy != "" || inspect.ImageID != "" {
			t.Errorf("ports %q = %+v, %v", empty, inspect, err)
		}
	}
	for _, bad := range []string{"", "x|y", "p|/web|running|t|t|0|false|0|img|id||{}|no:0|sha256:x", "4321|/web|running|t|t|r|false|0|img|id||{}|no:0|sha256:x", "1|/web|running|t|t|0|false|0|img|id||{|no:0|sha256:x", `1|/web|running|t|t|0|false|0|img|id||{"x/tcp":[{"HostIp":"","HostPort":"1"}]}|no:0|sha256:x`, "1|/web|running|t|t|0|false|0|img|id||{}|on-failure|sha256:x", "1|/web|running|t|t|0|false|0|img|id||{}|on-failure:x|sha256:x", "1|/web|running|t|t|0|false|0|img|id||{}|no:0"} {
		if _, err := parseCtrInspect(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for input, valid := range map[string]bool{"web": true, "my_app-1.2/x": true, "0123abcd": true, "": false, "-x": false, "a b": false, "a;b": false, "$(id)": false} {
		if _, err := normalizeCtrReference(input); (err == nil) != valid {
			t.Errorf("reference %q valid = %v, want %v", input, err == nil, valid)
		}
	}
}

func TestFilterCtrLogKeepsTheLastErrorLines(t *testing.T) {
	var lines []string
	for index := range 8 {
		lines = append(lines, "ok line", "ERROR number "+itoa(index))
	}
	log := filterCtrLog(strings.Join(lines, "\n") + "\n\n")
	if log.Scanned != 16 || len(log.Lines) != ctrLogShown || log.Lines[ctrLogShown-1] != "ERROR number 7" || log.Lines[0] != "ERROR number 3" {
		t.Fatalf("log = %+v", log)
	}
	if clean := filterCtrLog("info started\nwarning nothing\n"); clean.Scanned != 2 || len(clean.Lines) != 0 {
		t.Fatalf("clean = %+v", clean)
	}
	// "errors" 같은 낱말 일부는 맞추지 않지만 Traceback과 OOM은 맞춘다.
	if mixed := filterCtrLog("no errors here\nTraceback (most recent call last):\nOut of memory: killed\n"); len(mixed.Lines) != 2 {
		t.Fatalf("mixed = %+v", mixed)
	}
}

func TestEvaluateCtrRunningContainer(t *testing.T) {
	facts := collectCtrFacts(context.Background(), "web", ctrTestSources(ctrInspectLine("running", 0, false, 0, "healthy"), nil))
	results := evaluateCtr(facts)
	if len(results) != 8 || results[7].Probe != "ctr.disk" {
		t.Fatalf("results = %d: %+v", len(results), results)
	}
	for _, result := range results {
		if !strings.HasPrefix(result.Probe, "ctr.") {
			t.Errorf("probe %s is not ctr.*", result.Probe)
		}
	}
	state := ctrByID(t, results, "state")
	if state.Status != StatusPass || !strings.Contains(state.Summary, "running (healthy), up 1h 30m, 0 restarts (unless-stopped), image nginx:1.27") || state.Metrics["id"] != "0123456789ab" || state.Metrics["restart_policy"] != "unless-stopped" || len(state.Warnings) != 0 {
		t.Fatalf("state = %+v", state)
	}
	// 정상인 healthcheck도 -v에서 최근 검사를 보여 준다.
	wantChecks := ctrTestNow.Add(-time.Minute).Local().Format(time.DateTime) + "  exit 0  ok\n" + ctrTestNow.Add(-30*time.Second).Local().Format(time.DateTime) + "  exit 0  ok"
	if len(state.Evidence) != 1 || state.Evidence[0].Value != wantChecks || state.Metrics["health_failing_streak"] != 0 || state.Metrics["health_last_output"] != "ok" {
		t.Fatalf("state health = %+v", state)
	}
	created := ctrTestImageCreated.Local().Format(time.DateOnly)
	if image := results[1]; image.Probe != "ctr.image" || image.Status != StatusPass || image.Summary != "the image reference points to the running image, created "+created+" (77d 0h ago)" || image.Metrics["tag_image_id"] != ctrTestImageID {
		t.Fatalf("image = %+v", image)
	}
	processes := ctrByID(t, results, "processes")
	if processes.Status != StatusPass || !strings.Contains(processes.Evidence[0].Value, "(main)") {
		t.Fatalf("processes = %+v", processes)
	}
	limits := ctrByID(t, results, "limits")
	if limits.Status != StatusPass || !strings.Contains(limits.Summary, "memory 512.0MiB / 1.0GiB (50%) · OOM kills 0 · CPU throttled 10/1000 periods (1%) · pids 12 / 4096 (0%)") {
		t.Fatalf("limits = %+v", limits)
	}
	if listen := ctrByID(t, results, "listen"); listen.Status != StatusPass || !strings.Contains(listen.Summary, "tcp 0.0.0.0:8080") || strings.Contains(listen.Summary, "5432") || listen.Evidence[1].Value != "8080/tcp ← 0.0.0.0:80" {
		t.Fatalf("listen = %+v", listen)
	}
	log := ctrByID(t, results, "log")
	if log.Status != StatusWarn || !strings.Contains(log.Summary, "1 error lines in the last 3 log lines") || !strings.Contains(log.Evidence[0].Value, "upstream timed out") {
		t.Fatalf("log = %+v", log)
	}
	if disk := ctrByID(t, results, "disk"); disk.Status != StatusPass || disk.Summary != "log 527.8KiB (json-file, no rotation) · writable layer 4.0KiB" || disk.Metrics["log_bytes"] != uint64(540440) || disk.Metrics["writable_bytes"] != uint64(4096) || disk.Evidence[0].Value != "/var/lib/docker/containers/0123/0123-json.log" {
		t.Fatalf("disk = %+v", disk)
	}
}

// json-file log는 max-size가 없으면 줄지 않고, writable layer는 container를 다시 만들면 사라진다. 둘 다 크기로만 알린다.
func TestCtrDiskShowsTheLogAndTheWritableLayer(t *testing.T) {
	const gib = 1 << 30
	cases := []struct {
		name, output string
		logBytes     int64
		status       Status
		summary      string
		warning      string
	}{
		{"rotated", `4096|json-file|/log|{"max-file":"3","max-size":"10m"}`, 2 * gib, StatusPass, "log 2.0GiB (json-file, max-size 10m, max-file 3) · writable layer 4.0KiB", ""},
		{"max-size only", `0|local|/log|{"max-size":"20m"}`, 1024, StatusPass, "log 1.0KiB (local, max-size 20m) · writable layer 0B", ""},
		{"small and unbounded", "4096|json-file|/log|{}", gib - 1, StatusPass, "log 1024.0MiB (json-file, no rotation)", ""},
		{"large and unbounded", "4096|json-file|/log|null", gib, StatusWarn, "log 1.0GiB (json-file, no rotation)", "the json-file log has no max-size and is 1.0GiB"},
		{"large layer", "10737418240|json-file|/log|{}", 1, StatusWarn, "writable layer 10.0GiB", "the container wrote 10.0GiB into its writable layer"},
		// log option 값에 |가 있어도 맨 끝 칸이라 읽힌다.
		{"tag with a bar", `4096|json-file|/log|{"tag":"a|b","max-size":"5m"}`, 1, StatusPass, "(json-file, max-size 5m)", ""},
		{"no layer size", "null|json-file|/log|{}", 1, StatusPass, "writable layer -", ""},
	}
	for _, test := range cases {
		sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
		sources.disk = func(_ context.Context, id string) (string, error) {
			if id != "0123456789abcdef0123456789abcdef" {
				t.Errorf("%s: disk read %q, want the full container ID", test.name, id)
			}
			return test.output, nil
		}
		sources.fileSize = func(path string) (int64, error) {
			if path != "/log" {
				t.Errorf("%s: stat %q", test.name, path)
			}
			return test.logBytes, nil
		}
		disk := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "disk")
		if disk.Status != test.status || !strings.Contains(disk.Summary, test.summary) {
			t.Errorf("%s: %s %q, want %s containing %q", test.name, disk.Status, disk.Summary, test.status, test.summary)
		}
		if (test.warning == "") != (len(disk.Warnings) == 0) || test.warning != "" && !strings.Contains(disk.Warnings[0], test.warning) {
			t.Errorf("%s: warnings = %q", test.name, disk.Warnings)
		}
	}

	// journald처럼 파일이 없는 driver는 크기를 재지 않는다. 멈춘 container도 읽는다.
	sources := ctrTestSources(ctrInspectLine("exited", 0, false, 1, ""), nil)
	sources.disk = func(context.Context, string) (string, error) { return "8192|journald||{}", nil }
	sources.fileSize = func(path string) (int64, error) {
		t.Errorf("stat %q for a driver without a log file", path)
		return 0, nil
	}
	if disk := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "disk"); disk.Status != StatusPass || disk.Summary != "log driver journald · writable layer 8.0KiB" || len(disk.Evidence) != 0 || disk.Metrics["log_bytes"] != nil {
		t.Fatalf("journald = %+v", disk)
	}

	// root가 아니면 docker 저장소의 log 파일을 읽지 못한다. 크기는 -이고, 까닭을 상세에 적는다.
	sources = ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.fileSize = func(path string) (int64, error) {
		return 0, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
	}
	if disk := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "disk"); disk.Status != StatusPass || !strings.HasPrefix(disk.Summary, "log - (json-file, no rotation)") || !strings.Contains(disk.Evidence[0].Value, "permission denied") {
		t.Fatalf("unreadable log = %+v", disk)
	}

	// docker를 부르지 못했거나 출력이 틀리면 skip이다.
	for _, bad := range []struct {
		output string
		err    error
		text   string
	}{
		{"Error response from daemon: busy", errors.New("exit status 1"), "Error response from daemon: busy"},
		{"x|json-file|/log|{}", nil, `unexpected docker inspect size "x"`},
		{"4096|json-file", nil, "unexpected docker inspect output"},
		{"4096|json-file|/log|{", nil, "unexpected docker log options"},
	} {
		sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
		sources.disk = func(context.Context, string) (string, error) { return bad.output, bad.err }
		if disk := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "disk"); disk.Status != StatusSkip || !strings.Contains(disk.Summary, bad.text) {
			t.Errorf("%q: %+v", bad.output, disk)
		}
	}
}

// pull이나 build가 tag를 옮겨도 실행 중인 container는 예전 이미지로 돈다. 참조가 가리키는 이미지와 실행 중인 이미지를 맞춰 본다.
func TestCtrImageComparesTheReference(t *testing.T) {
	newer, newerCreated := "sha256:"+strings.Repeat("b", 64), ctrTestNow.Add(-24*time.Hour)
	created, newerDate := ctrTestImageCreated.Local().Format(time.DateOnly), newerCreated.Local().Format(time.DateOnly)
	cases := []struct {
		name      string
		reference string
		images    map[string]string
		status    Status
		summary   string
		asked     []string
	}{
		{"current", "nginx:1.27", map[string]string{"nginx:1.27": ctrImageLine(ctrTestImageID, ctrTestImageCreated)}, StatusPass,
			"the image reference points to the running image, created " + created + " (77d 0h ago)", []string{"nginx:1.27"}},
		// digest로 고정한 참조도 그대로 docker에 넘긴다.
		{"digest", "postgres:17.6-alpine@sha256:" + strings.Repeat("a", 64), map[string]string{"postgres:17.6-alpine@sha256:" + strings.Repeat("a", 64): ctrImageLine(ctrTestImageID, ctrTestImageCreated)}, StatusPass,
			"points to the running image", []string{"postgres:17.6-alpine@sha256:" + strings.Repeat("a", 64)}},
		{"outdated", "nginx:1.27", map[string]string{"nginx:1.27": ctrImageLine(newer, newerCreated), ctrTestImageID: ctrImageLine(ctrTestImageID, ctrTestImageCreated)}, StatusWarn,
			"nginx:1.27 now points to bbbbbbbbbbbb, created " + newerDate + " (1d 0h ago), but the container runs aaaaaaaaaaaa, created " + created + " (77d 0h ago). Recreate the container to use it", []string{"nginx:1.27", ctrTestImageID}},
		{"tag missing", "nginx:1.27", map[string]string{ctrTestImageID: ctrImageLine(ctrTestImageID, ctrTestImageCreated)}, StatusPass,
			"nginx:1.27 is not on this host, so edc cannot compare it. The container runs aaaaaaaaaaaa, created " + created + " (77d 0h ago)", []string{"nginx:1.27", ctrTestImageID}},
		// tag만 지워지고 이미지도 없으면 생성 시각만 모른다.
		{"both missing", "nginx:1.27", map[string]string{}, StatusPass,
			"nginx:1.27 is not on this host, so edc cannot compare it. The container runs aaaaaaaaaaaa, created -", []string{"nginx:1.27", ctrTestImageID}},
	}
	for _, test := range cases {
		var asked []string
		sources := ctrTestSources(ctrInspectField(ctrInspectLine("running", 0, false, 0, ""), 8, test.reference), nil)
		sources.image = func(_ context.Context, reference string) (string, error) {
			asked = append(asked, reference)
			if line, ok := test.images[reference]; ok {
				return line, nil
			}
			return "\nError response from daemon: No such image: " + reference + "\n", errors.New("exit status 1")
		}
		image := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "image")
		if image.Status != test.status || !strings.Contains(image.Summary, test.summary) || strings.Join(asked, " ") != strings.Join(test.asked, " ") {
			t.Errorf("%s: %s %q, asked %q", test.name, image.Status, image.Summary, asked)
		}
	}
	// docker가 다른 까닭으로 실패하면 비교하지 않고 skip이다.
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.image = func(context.Context, string) (string, error) {
		return "permission denied while trying to connect to the docker API", errors.New("exit status 1")
	}
	if image := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "image"); image.Status != StatusSkip || !strings.Contains(image.Summary, "permission denied") {
		t.Errorf("docker failure = %+v", image)
	}
	// 이미지 ID 칸이 비면 docker를 부르지 않는다.
	sources = ctrTestSources(ctrInspectField(ctrInspectLine("running", 0, false, 0, ""), 13, ""), nil)
	sources.image = func(context.Context, string) (string, error) {
		t.Error("image was read without an image ID")
		return "", nil
	}
	if image := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "image"); image.Status != StatusSkip || image.Summary != "docker inspect gave no image ID for the container" {
		t.Errorf("no image ID = %+v", image)
	}
}

// publish한 port로 들어온 연결은 container IP로 넘어간다. 안에서 그 port를 wildcard나 container 주소에서 listen해야 닿는다.
func TestCtrListenChecksThePublishedPorts(t *testing.T) {
	nginx := func(address string, port int) listenSocket {
		return listenSocket{Proto: "tcp", Address: address, Port: port, PID: "101", Process: "nginx"}
	}
	cases := []struct {
		name    string
		ports   string
		sockets []listenSocket
		status  Status
		warning string
		state   string
	}{
		{"wildcard", `{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"80"},{"HostIp":"::","HostPort":"80"}]}`, []listenSocket{nginx("*", 8080)}, StatusPass, "", "listening"},
		{"container address", `{"8080/tcp":[{"HostIp":"","HostPort":"80"}]}`, []listenSocket{nginx("172.17.0.2", 8080)}, StatusPass, "", "listening"},
		{"loopback only", `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}`, []listenSocket{nginx("127.0.0.1", 8080), nginx("::1", 8080)}, StatusWarn, "8080/tcp is published on 127.0.0.1:8080, but the container listens on it only on loopback", "loopback_only"},
		{"no listener", `{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5432"}]}`, []listenSocket{nginx("0.0.0.0", 8080)}, StatusWarn, "5432/tcp is published on 127.0.0.1:5432, but no container process listens on it", "not_listening"},
		// container 밖 process의 소켓이나 같은 번호의 udp 소켓은 이 port를 받지 않는다.
		{"other owner or proto", `{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"80"}]}`, []listenSocket{{Proto: "tcp", Address: "0.0.0.0", Port: 8080, PID: "999"}, {Proto: "udp", Address: "0.0.0.0", Port: 8080, PID: "101"}}, StatusWarn, "no container process listens", "not_listening"},
	}
	for _, test := range cases {
		sources := ctrTestSources(ctrInspectLineWithPorts(test.ports), nil)
		sources.listen = func(pid int) ([]listenSocket, error) {
			if pid != 100 {
				t.Errorf("%s: listen read pid %d, want the main pid 100", test.name, pid)
			}
			return test.sockets, nil
		}
		listen := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "listen")
		published, _ := listen.Metrics["published"].([]map[string]interface{})
		if listen.Status != test.status || len(published) != 1 || published[0]["state"] != test.state {
			t.Errorf("%s: %s %+v", test.name, listen.Status, listen.Metrics)
			continue
		}
		if (test.warning == "") != (len(listen.Warnings) == 0) || test.warning != "" && !strings.Contains(listen.Warnings[0], test.warning) {
			t.Errorf("%s: warnings = %q", test.name, listen.Warnings)
		}
	}
	// publish하지 않았으면 대조할 것이 없다.
	if listen := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources(ctrInspectLineWithPorts(`{"80/tcp":null}`), nil))), "listen"); listen.Status != StatusPass || listen.Metrics["published"] != nil {
		t.Errorf("exposed only = %+v", listen)
	}
	// 소켓을 읽지 못하면 대조하지 않고 skip이다. 빈 목록으로 대조하면 거짓 warn이 된다.
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.listen = func(int) ([]listenSocket, error) {
		return nil, errors.New("setns /proc/100/ns/net: operation not permitted")
	}
	if listen := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "listen"); listen.Status != StatusSkip || len(listen.Warnings) != 0 || !strings.Contains(listen.Summary, "operation not permitted") {
		t.Errorf("unreadable = %+v", listen)
	}
}

// unhealthy의 까닭은 healthcheck 출력에 있다. 상태 줄만으로는 무엇이 실패했는지 모른다.
func TestCtrStateExplainsAnUnhealthyContainer(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, "unhealthy"), nil)
	sources.health = func(_ context.Context, id string) (string, error) {
		if id != "0123456789abcdef0123456789abcdef" {
			t.Errorf("health read %q, want the full container ID", id)
		}
		return ctrHealthJSON("unhealthy", 3,
			ctrHealthCheck{Start: ctrTestNow.Add(-90 * time.Second), Output: "ok"},
			ctrHealthCheck{Start: ctrTestNow.Add(-60 * time.Second), ExitCode: 1, Output: "curl: (7) Failed to connect\nmore detail"},
			ctrHealthCheck{Start: ctrTestNow.Add(-30 * time.Second), ExitCode: 1, Output: "  curl: (7) connection refused \n"}), nil
	}
	state := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "state")
	if state.Status != StatusWarn || len(state.Warnings) != 1 || state.Warnings[0] != "the health check failed 3 times in a row, latest exit 1: curl: (7) connection refused" {
		t.Fatalf("unhealthy = %+v", state)
	}
	if lines := strings.Split(state.Evidence[0].Value, "\n"); len(lines) != 3 || !strings.HasSuffix(lines[1], "  exit 1  curl: (7) Failed to connect") || state.Metrics["health_last_exit_code"] != 1 {
		t.Fatalf("checks = %q, metrics = %+v", state.Evidence[0].Value, state.Metrics)
	}
	// 기록을 읽지 못해도 상태는 그대로이고, 읽지 못한 까닭을 상세에 적는다.
	sources.health = func(context.Context, string) (string, error) {
		return "Error response from daemon: busy", errors.New("exit status 1")
	}
	state = ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "state")
	if state.Status != StatusWarn || len(state.Warnings) != 0 || !strings.Contains(state.Evidence[0].Value, "Error response from daemon: busy") {
		t.Fatalf("unreadable health = %+v", state)
	}
	// healthcheck가 없는 container는 기록을 읽지 않는다.
	sources = ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.health = func(context.Context, string) (string, error) {
		t.Error("health was read for a container without a healthcheck")
		return "", nil
	}
	if state := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "state"); len(state.Evidence) != 0 || state.Metrics["health"] != nil {
		t.Fatalf("no healthcheck = %+v", state)
	}
}

// on-failure:N은 N번까지만 다시 띄운다. 한도에 닿아 멈춘 container는 누가 시작하기 전까지 그대로다.
func TestCtrStateShowsTheRestartPolicy(t *testing.T) {
	cases := []struct {
		name, line, summary, warning string
	}{
		{"gave up", ctrInspectField(ctrInspectLine("exited", 3, false, 1, ""), 12, "on-failure:3"), "3 restarts (on-failure:3)", "docker does not restart it again: it restarted 3 times, the limit of on-failure:3"},
		{"retries left", ctrInspectField(ctrInspectLine("exited", 2, false, 1, ""), 12, "on-failure:3"), "2 restarts (on-failure:3)", ""},
		{"no limit", ctrInspectField(ctrInspectLine("running", 4, false, 0, ""), 12, "on-failure:0"), "4 restarts (on-failure)", ""},
		{"no policy", ctrInspectField(ctrInspectLine("exited", 0, false, 1, ""), 12, "no:0"), "0 restarts (no restart policy)", ""},
		{"unknown", ctrInspectField(ctrInspectLine("running", 0, false, 0, ""), 12, ""), "0 restarts (-)", ""},
	}
	for _, test := range cases {
		state := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources(test.line, nil))), "state")
		if !strings.Contains(state.Summary, test.summary) {
			t.Errorf("%s: summary %q, want %q", test.name, state.Summary, test.summary)
		}
		if (test.warning == "") != (len(state.Warnings) == 0) || test.warning != "" && state.Warnings[0] != test.warning {
			t.Errorf("%s: warnings = %q", test.name, state.Warnings)
		}
	}
}

func TestEvaluateCtrStateAndLimitStatuses(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		status Status
		text   string
	}{
		{"restarted", ctrInspectLine("running", 2, false, 0, ""), StatusWarn, "2 restarts"},
		{"oom killed", ctrInspectLine("running", 0, true, 0, ""), StatusWarn, "OOM killed"},
		{"unhealthy", ctrInspectLine("running", 0, false, 0, "unhealthy"), StatusWarn, "running (unhealthy)"},
		{"exited", ctrInspectLine("exited", 0, false, 1, ""), StatusFail, "exited, exit code 1"},
		{"paused", ctrInspectLine("paused", 0, false, 0, ""), StatusWarn, "paused"},
	}
	for _, test := range cases {
		facts := collectCtrFacts(context.Background(), "web", ctrTestSources(test.line, nil))
		state := ctrByID(t, evaluateCtr(facts), "state")
		if state.Status != test.status || !strings.Contains(state.Summary, test.text) {
			t.Errorf("%s: %s %q, want %s containing %q", test.name, state.Status, state.Summary, test.status, test.text)
		}
	}
	// 멈춘 container는 process 관련 결과가 skip이고, 로그는 여전히 읽는다.
	stopped := evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources(ctrInspectLine("exited", 0, false, 1, ""), nil)))
	for _, id := range []string{"processes", "limits", "fd", "listen"} {
		if result := ctrByID(t, stopped, id); result.Status != StatusSkip || !strings.Contains(result.Summary, "container has no running process") {
			t.Errorf("stopped %s = %+v", id, result)
		}
	}
	if log := ctrByID(t, stopped, "log"); log.Status != StatusWarn {
		t.Errorf("stopped log = %+v", log)
	}
	// 한도에 가까우면 warn이다.
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.limits = func(path string) ctrLimits {
		used, maximum, oom, kill := uint64(950<<20), uint64(1000<<20), uint64(1), uint64(1)
		return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path,
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: &oom, OOMKill: &kill},
			CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 100, Throttled: 40},
			Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 95, Max: 100}}
	}
	limits := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "limits")
	if limits.Status != StatusWarn || len(limits.Warnings) != 4 || limits.Metrics["memory_percent"] != 95.0 || limits.Metrics["oom_kills"] != uint64(1) {
		t.Fatalf("near limits = %+v", limits)
	}
	// 한도가 없으면 그렇게 적고 경고하지 않는다.
	sources.limits = func(path string) ctrLimits {
		used := uint64(1 << 20)
		return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path,
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Unlimited: true},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "unsupported", Reason: "no memory.events.local"}},
			CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}},
			Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 3, Unlimited: true}}
	}
	if limits := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "limits"); limits.Status != StatusPass || !strings.Contains(limits.Summary, "memory 1.0MiB / no limit · OOM kills - · CPU throttled no limit · pids 3 / no limit") {
		t.Fatalf("unlimited = %+v", limits)
	}
}

func TestEvaluateCtrUnreadableSourcesSkip(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.cgroupPath = func(int) (string, error) { return "", errors.New("open /proc/100/cgroup: no such file") }
	sources.logs = func(context.Context, string, int) (string, error) {
		return "permission denied", errors.New("exit status 1")
	}
	results := evaluateCtr(collectCtrFacts(context.Background(), "web", sources))
	for _, id := range []string{"processes", "limits", "log"} {
		if result := ctrByID(t, results, id); result.Status != StatusSkip {
			t.Errorf("%s = %+v, want skip", id, result)
		}
	}
	noDocker := ctrTestSources("", nil)
	noDocker.docker = func() bool { return false }
	if results := evaluateCtr(collectCtrFacts(context.Background(), "web", noDocker)); len(results) != 1 || results[0].Status != StatusSkip || results[0].Probe != "ctr" {
		t.Fatalf("no docker = %+v", results)
	}
	failed := evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources("Error response from daemon: something", errors.New("exit status 1"))))
	if len(failed) != 1 || failed[0].Status != StatusFail || failed[0].Error == nil || !strings.Contains(failed[0].Summary, "something") {
		t.Fatalf("inspect failure = %+v", failed)
	}
	// docker socket 권한이 없으면 container를 판정할 수 없다. fail이 아니라 skip이다.
	denied := evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources("permission denied while trying to connect to the docker API at unix:///var/run/docker.sock", errors.New("exit status 1"))))
	if len(denied) != 1 || denied[0].Status != StatusSkip || !strings.Contains(denied[0].Summary, "docker group") {
		t.Fatalf("denied = %+v", denied)
	}
}

func TestRunCtrExitCodes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	stdout := os.Stdout
	os.Stdout = devNull
	defer func() { os.Stdout = stdout }()
	if code := runCtrWith([]string{"web"}, "test", ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil), true); code != 0 {
		t.Fatalf("running = %d", code)
	}
	if code := runCtrWith([]string{"web"}, "test", ctrTestSources(ctrInspectLine("exited", 0, false, 1, ""), nil), true); code != 1 {
		t.Fatalf("exited = %d", code)
	}
	if code := runCtrWith([]string{"web"}, "test", ctrTestSources("Error response from daemon: No such container: web", errors.New("exit status 1")), true); code != 1 {
		t.Fatalf("not found = %d", code)
	}
	if code := runCtrWith([]string{"web"}, "test", ctrSources{}, false); code != 0 {
		t.Fatalf("unsupported = %d", code)
	}
	// container 없이 부르면 목록이다. TestRunCtrWithoutAContainerListsThem이 본다.
	for _, args := range [][]string{{"-x"}, {"a", "b"}, {"--timeout", "x", "web"}} {
		if code := runCtrWith(args, "test", ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil), true); code != 2 {
			t.Errorf("args %q = %d, want 2", args, code)
		}
	}
	if code := runCtrWith([]string{"web", "-v"}, "test", ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil), true); code != 0 {
		t.Fatalf("option after the name = %d", code)
	}
}
