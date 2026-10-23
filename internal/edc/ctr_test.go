package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
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
	parsedInspect, parseErr := parseCtrInspect(inspect)
	if inspectErr == nil {
		inspectErr = parseErr
	} else if inspect != "" {
		inspectErr = fmt.Errorf("%s: %w", inspect, inspectErr)
	}
	if parsedInspect.Health != "" {
		parsedInspect.HealthData = &ctrHealth{Status: parsedInspect.Health, Log: []ctrHealthCheck{{Start: ctrTestNow.Add(-time.Minute), Output: "ok\n"}, {Start: ctrTestNow.Add(-30 * time.Second), Output: "ok\n"}}}
	}
	parsedInspect.Disk = ctrTestDiskValue()
	return ctrSources{
		euid:    0,
		docker:  func() bool { return true },
		inspect: func(context.Context, string) (ctrInspect, error) { return parsedInspect, inspectErr },
		stats: func(context.Context, string, ctrInspect, bool) ctrLimits {
			used, maximum := uint64(512<<20), uint64(1<<30)
			return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"},
				Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
				Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: "OOM counters are unavailable through the Docker Engine API"}},
				CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 1000, Throttled: 10, ThrottledUS: 5000},
				Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 12, Max: 4096}}
		},
		top: func(context.Context, string) ([]topProcess, error) {
			return []topProcess{{PID: 101, CPU: 3, RSS: 2 << 20, Command: "nginx"}, {PID: 100, CPU: 1, RSS: 4 << 20, Command: "nginx"}}, nil
		},
		logs: func(context.Context, string, int, bool) (string, error) {
			return "2026-10-12T11:59:00Z info ready\n2026-10-12T11:59:30Z [error] upstream timed out\n2026-10-12T11:59:40Z info ok\n", nil
		},
		image: func(context.Context, string) (ctrImageRef, error) {
			return ctrImageRef{ID: ctrTestImageID, Created: ctrTestImageCreated}, nil
		},
		now: func() time.Time { return ctrTestNow },
	}
}

func parseCtrInspect(output string) (ctrInspect, error) {
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 14 {
		return ctrInspect{}, fmt.Errorf("unexpected inspect fixture")
	}
	pid, e1 := strconv.Atoi(fields[0])
	restarts, e2 := strconv.Atoi(fields[5])
	exit, e3 := strconv.Atoi(fields[7])
	ports, e4 := parseCtrPorts(fields[11])
	policy, maxRetry, e5 := parseCtrRestartPolicy(fields[12])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
		return ctrInspect{}, fmt.Errorf("invalid inspect fixture")
	}
	started, _ := time.Parse(time.RFC3339Nano, fields[3])
	finished, _ := time.Parse(time.RFC3339Nano, fields[4])
	return ctrInspect{PID: pid, Name: strings.TrimPrefix(fields[1], "/"), Status: fields[2], StartedAt: started, FinishedAt: finished, RestartCount: restarts, OOMKilled: fields[6] == "true", ExitCode: exit, Image: fields[8], ID: fields[9], Health: fields[10], Ports: ports, RestartPolicy: policy, RestartMax: maxRetry, ImageID: fields[13]}, nil
}

func parseCtrRestartPolicy(text string) (string, int, error) {
	if text == "" {
		return "", 0, nil
	}
	name, count, ok := strings.Cut(text, ":")
	if !ok {
		return "", 0, fmt.Errorf("invalid restart policy")
	}
	n, err := strconv.Atoi(count)
	return name, n, err
}
func parseCtrPorts(text string) ([]ctrPublishedPort, error) {
	if text == "" {
		return nil, nil
	}
	var bindings map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(text), &bindings); err != nil {
		return nil, err
	}
	var ports []ctrPublishedPort
	for key, hosts := range bindings {
		if len(hosts) == 0 {
			continue
		}
		number, proto, _ := strings.Cut(key, "/")
		port, err := strconv.Atoi(number)
		if err != nil {
			return nil, err
		}
		item := ctrPublishedPort{Proto: proto, Port: port}
		for _, host := range hosts {
			address := host.HostIP
			if address == "" {
				address = "*"
			}
			item.Hosts = append(item.Hosts, net.JoinHostPort(address, host.HostPort))
		}
		ports = append(ports, item)
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return ports[i].Proto < ports[j].Proto
	})
	return ports, nil
}
func parseCtrDisk(output string) (ctrDisk, error) {
	fields := strings.SplitN(strings.TrimSpace(output), "|", 4)
	if len(fields) != 4 {
		return ctrDisk{}, fmt.Errorf("unexpected docker inspect output")
	}
	disk := ctrDisk{LogDriver: fields[1], LogPath: fields[2]}
	if fields[0] != "null" {
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return ctrDisk{}, fmt.Errorf("unexpected docker inspect size %q", fields[0])
		}
		disk.Writable = &v
	}
	if fields[3] != "" {
		var opts map[string]string
		if err := json.Unmarshal([]byte(fields[3]), &opts); err != nil {
			return ctrDisk{}, fmt.Errorf("unexpected docker log options")
		}
		disk.LogMaxSize, disk.LogMaxFile = opts["max-size"], opts["max-file"]
	}
	return disk, nil
}

func ctrTestDiskValue() ctrDisk {
	value := uint64(4096)
	return ctrDisk{Writable: &value, LogDriver: "json-file", LogPath: "/var/lib/docker/containers/0123/0123-json.log"}
}

func ctrTestSetInspect(sources *ctrSources, update func(*ctrInspect)) {
	original := sources.inspect
	sources.inspect = func(ctx context.Context, reference string) (ctrInspect, error) {
		inspect, err := original(ctx, reference)
		if err == nil {
			update(&inspect)
		}
		return inspect, err
	}
}

func ctrTestParseDisk(line string, err error) (ctrDisk, error) {
	if err != nil {
		return ctrDisk{}, err
	}
	return parseCtrDisk(line)
}
func ctrTestParseImage(line string, err error) (ctrImageRef, error) {
	if err != nil {
		if strings.Contains(line, "No such image") {
			return ctrImageRef{}, errCtrNoImage
		}
		return ctrImageRef{}, errors.New(line)
	}
	parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
	if len(parts) != 2 {
		return ctrImageRef{}, errors.New("invalid image fixture")
	}
	created, parseErr := time.Parse(time.RFC3339Nano, parts[1])
	return ctrImageRef{ID: parts[0], Created: created}, parseErr
}
func ctrTestParseHealth(line string, err error) (ctrHealth, error) {
	if err != nil {
		return ctrHealth{}, errors.New(line)
	}
	var health ctrHealth
	parseErr := json.Unmarshal([]byte(line), &health)
	return health, parseErr
}

// ctrTestDisk는 json-file log driver와 writable layer 크기를 나타낸다.
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
	if limits.Status != StatusPass || !strings.Contains(limits.Summary, "memory 512.0MiB / 1.0GiB (50%) · OOM kills - · CPU throttled 10/1000 periods (1%) · pids 12 / 4096 (0%)") || len(limits.Evidence) == 0 {
		t.Fatalf("limits = %+v", limits)
	}
	if listen := ctrByID(t, results, "listen"); listen.Status != StatusSkip || !strings.Contains(listen.Summary, "does not report actual container listeners") || listen.Evidence[0].Value != "8080/tcp → 0.0.0.0:80" {
		t.Fatalf("listen = %+v", listen)
	}
	log := ctrByID(t, results, "log")
	if log.Status != StatusWarn || !strings.Contains(log.Summary, "1 error lines in the last 3 log lines") || !strings.Contains(log.Evidence[0].Value, "upstream timed out") {
		t.Fatalf("log = %+v", log)
	}
	if disk := ctrByID(t, results, "disk"); disk.Status != StatusPass || disk.Summary != "log - (json-file, no rotation) · writable layer 4.0KiB" || disk.Metrics["log_bytes"] != nil || disk.Metrics["writable_bytes"] != uint64(4096) || !strings.Contains(disk.Evidence[0].Value, "unavailable") {
		t.Fatalf("disk = %+v", disk)
	}
}

// json-file log는 max-size가 없으면 줄지 않고, writable layer는 container를 다시 만들면 사라진다. 둘 다 크기로만 알린다.
func TestCtrDiskReportsUnavailableLogSize(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	ctrTestSetInspect(&sources, func(inspect *ctrInspect) {
		disk := ctrDiskValue(10 << 30)
		disk.LogDriver = "json-file"
		disk.LogMaxSize = "10m"
		inspect.Disk = disk
	})
	disk := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "disk")
	if disk.Status != StatusWarn || disk.Metrics["log_bytes"] != nil || disk.Metrics["writable_bytes"] != uint64(10<<30) || !strings.Contains(disk.Summary, "log driver json-file") || len(disk.Evidence) == 0 || !strings.Contains(disk.Evidence[0].Value, "unavailable") {
		t.Fatalf("disk=%+v", disk)
	}
}

func ctrDiskValue(size uint64) ctrDisk {
	return ctrDisk{Writable: &size, LogDriver: "json-file", LogSizeErr: errCtrUnavailable}
}

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
		sources.image = func(_ context.Context, reference string) (ctrImageRef, error) {
			asked = append(asked, reference)
			if line, ok := test.images[reference]; ok {
				return ctrTestParseImage(line, nil)
			}
			return ctrTestParseImage("No such image: "+reference, errors.New("exit status 1"))
		}
		image := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "image")
		if image.Status != test.status || !strings.Contains(image.Summary, test.summary) || strings.Join(asked, " ") != strings.Join(test.asked, " ") {
			t.Errorf("%s: %s %q, asked %q", test.name, image.Status, image.Summary, asked)
		}
	}
	// docker가 다른 까닭으로 실패하면 비교하지 않고 skip이다.
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.image = func(context.Context, string) (ctrImageRef, error) {
		return ctrImageRef{}, errors.New("permission denied while trying to connect to the docker API")
	}
	if image := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "image"); image.Status != StatusSkip || !strings.Contains(image.Summary, "permission denied") {
		t.Errorf("docker failure = %+v", image)
	}
	// 이미지 ID 칸이 비면 docker를 부르지 않는다.
	sources = ctrTestSources(ctrInspectField(ctrInspectLine("running", 0, false, 0, ""), 13, ""), nil)
	sources.image = func(context.Context, string) (ctrImageRef, error) {
		t.Error("image was read without an image ID")
		return ctrImageRef{}, nil
	}
	if image := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "image"); image.Status != StatusSkip || image.Summary != "the inspect response has no image ID" {
		t.Errorf("no image ID = %+v", image)
	}
}

// publish한 port로 들어온 연결은 container IP로 넘어간다. 안에서 그 port를 wildcard나 container 주소에서 listen해야 닿는다.
func TestCtrListenIsUnavailableThroughEngineAPI(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	result := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "listen")
	if result.Status != StatusSkip || !strings.Contains(result.Summary, "does not report actual container listeners") {
		t.Fatalf("listen=%+v", result)
	}
}

func TestCtrStateUsesInspectHealthHistory(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, "unhealthy"), nil)
	ctrTestSetInspect(&sources, func(inspect *ctrInspect) {
		inspect.HealthData = &ctrHealth{FailingStreak: 3, Log: []ctrHealthCheck{{Start: ctrTestNow.Add(-time.Minute), ExitCode: 1, Output: "curl failed"}}}
	})
	state := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "state")
	if state.Status != StatusWarn || len(state.Warnings) != 1 || !strings.Contains(state.Warnings[0], "curl failed") {
		t.Fatalf("health=%+v", state)
	}
	sources = ctrTestSources(ctrInspectLine("running", 0, false, 0, "unhealthy"), nil)
	ctrTestSetInspect(&sources, func(inspect *ctrInspect) { inspect.HealthData = nil })
	state = ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "state")
	if state.Status != StatusWarn || len(state.Warnings) != 0 || len(state.Evidence) == 0 || !strings.Contains(state.Evidence[0].Value, "unavailable") {
		t.Fatalf("unavailable health=%+v", state)
	}
}

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
	sources.stats = func(context.Context, string, ctrInspect, bool) ctrLimits {
		used, maximum := uint64(950<<20), uint64(1000<<20)
		return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"},
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: "OOM counters are unavailable through the Docker Engine API"}},
			CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 100, Throttled: 40},
			Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 95, Max: 100}}
	}
	limits := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "limits")
	if limits.Status != StatusWarn || len(limits.Warnings) != 3 || limits.Metrics["memory_percent"] != 95.0 || limits.Metrics["oom_kills"] != nil {
		t.Fatalf("near limits = %+v", limits)
	}
	// 한도가 없으면 그렇게 적고 경고하지 않는다.
	sources.stats = func(context.Context, string, ctrInspect, bool) ctrLimits {
		used := uint64(1 << 20)
		return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"},
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Unlimited: true},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "unsupported", Reason: "no memory.events.local"}},
			CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}},
			Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 3, Unlimited: true}}
	}
	if limits := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "limits"); limits.Status != StatusPass || !strings.Contains(limits.Summary, "memory 1.0MiB / no limit · OOM kills - · CPU throttled - · pids 3 / no limit") {
		t.Fatalf("unlimited = %+v", limits)
	}
}

func TestCtrLimitsShowsCPUWhenOtherMetricsAreUnavailable(t *testing.T) {
	percent := 75.0
	result := ctrLimitsResult(ctrFacts{
		Inspect: ctrInspect{PID: 42},
		Limits: ctrLimits{
			topLimitStatus:  topLimitStatus{Status: "unavailable", Reason: "other resource fields are missing"},
			CPUUsage:        topLimitStatus{Status: "available"},
			CPUUsagePercent: &percent,
		},
	})
	if result.Status != StatusPass || result.Metrics["cpu_usage_percent"] != 75.0 || !strings.HasPrefix(result.Summary, "CPU 75.00%") {
		t.Fatalf("CPU-only detail = %+v", result)
	}
	if _, ok := result.Metrics["memory_used_bytes"]; ok {
		t.Fatalf("missing memory became a value: %+v", result.Metrics)
	}
}

func TestCtrLimitsPreservesZeroPidsCurrentWithUnknownLimit(t *testing.T) {
	result := ctrLimitsResult(ctrFacts{
		Inspect: ctrInspect{PID: 42},
		Limits: ctrLimits{
			topLimitStatus: topLimitStatus{Status: "available"},
			Pids:           ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 0},
		},
	})
	if result.Metrics["pids_current"] != uint64(0) || result.Metrics["pids_limit_known"] != false || !strings.Contains(result.Summary, "0 / unknown limit") {
		t.Fatalf("zero current with unknown limit = %+v", result)
	}
	missing := ctrLimitsResult(ctrFacts{
		Inspect: ctrInspect{PID: 42},
		Limits: ctrLimits{
			topLimitStatus: topLimitStatus{Status: "available"},
			Pids:           ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: "pids current missing"}},
		},
	})
	if _, exists := missing.Metrics["pids_current"]; exists {
		t.Fatalf("missing current became zero: %+v", missing)
	}
}

func TestCtrLimitsKeepsAPIStatsWhenTopFails(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.top = func(context.Context, string) ([]topProcess, error) {
		return nil, errors.New("top columns unavailable")
	}
	percent := 50.0
	used := uint64(4096)
	sources.stats = func(_ context.Context, _ string, _ ctrInspect, detailed bool) ctrLimits {
		if !detailed {
			t.Error("detail stats requested one-shot list mode")
		}
		return ctrLimits{
			topLimitStatus:  topLimitStatus{Status: "available"},
			Memory:          topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used},
			CPUUsage:        topLimitStatus{Status: "available"},
			CPUUsagePercent: &percent,
			Pids:            ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 17},
		}
	}
	results := evaluateCtr(collectCtrFacts(context.Background(), "web", sources))
	if processes := ctrByID(t, results, "processes"); processes.Status != StatusSkip {
		t.Fatalf("top result=%+v", processes)
	}
	limits := ctrByID(t, results, "limits")
	if limits.Status != StatusPass || limits.Metrics["cpu_usage_percent"] != 50.0 || limits.Metrics["memory_used_bytes"] != used {
		t.Fatalf("stats were hidden by top failure: %+v", limits)
	}
}

func TestEvaluateCtrUnreadableSourcesSkip(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.top = func(context.Context, string) ([]topProcess, error) {
		return nil, errors.New("Docker top is unavailable")
	}
	sources.logs = func(context.Context, string, int, bool) (string, error) {
		return "permission denied", errors.New("exit status 1")
	}
	results := evaluateCtr(collectCtrFacts(context.Background(), "web", sources))
	processes := ctrByID(t, results, "processes")
	if processes.Status != StatusSkip || !strings.Contains(processes.Summary, "Docker top API") || strings.Contains(processes.Summary, "/proc") {
		t.Fatalf("top API error source = %+v", processes)
	}
	for _, id := range []string{"processes", "log"} {
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
	if len(denied) != 1 || denied[0].Status != StatusSkip || !strings.Contains(denied[0].Summary, "Docker Engine socket") {
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
