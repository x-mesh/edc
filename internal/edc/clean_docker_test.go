package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 관측한 OrbStack 응답에서 값만 줄인 것이다. ActiveCount가 0인 객체에는 그 키가 없다.
const (
	cleanDockerImageDF = `{"ImageUsage":{"ActiveCount":12,"Reclaimable":49040816462,"TotalCount":287,"TotalSize":92819046936},` +
		`"ContainerUsage":{"Reclaimable":1593687321,"TotalCount":20,"TotalSize":1593687321},` +
		`"BuildCacheUsage":{"Reclaimable":2591000565,"TotalCount":277,"TotalSize":3730872599}}`
	cleanDockerVolumeDF = `{"VolumeUsage":{"ActiveCount":13,"Reclaimable":10123147613,"TotalCount":98,"TotalSize":20817611480}}`
)

const (
	cleanDockerPingPath   = "/_ping"
	cleanDockerInfoPath   = "/v1.52/info"
	cleanDockerImagePath  = "/v1.52/system/df?type=image&type=container&type=build-cache"
	cleanDockerVolumePath = "/v1.52/system/df?type=volume"
)

// cleanDockerFake는 Docker 엔진 흉내를 내는 서버다. 받은 요청을 모두 기록한다.
type cleanDockerFake struct {
	mu       sync.Mutex
	requests []string
	// api가 비면 Api-Version 헤더를 내지 않는다.
	api string
	// delay는 경로(쿼리 포함)마다 응답 전에 기다리는 시간이다.
	delay map[string]time.Duration
	// status는 경로마다 200 대신 내는 상태 코드이고, body는 본문을 바꾼다.
	status map[string]int
	body   map[string]string
}

func newCleanDockerFake() *cleanDockerFake {
	return &cleanDockerFake{api: "1.56", delay: map[string]time.Duration{}, status: map[string]int{}, body: map[string]string{}}
}

func (fake *cleanDockerFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.RequestURI()
	fake.mu.Lock()
	fake.requests = append(fake.requests, r.Method+" "+key)
	delay, status, body, api := fake.delay[key], fake.status[key], fake.body[key], fake.api
	fake.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if api != "" {
		w.Header().Set("Api-Version", api)
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if body == "" {
		switch key {
		case cleanDockerPingPath:
			body = "OK"
		case cleanDockerInfoPath:
			body = `{"OperatingSystem":"OrbStack","ServerVersion":"29.8.2"}`
		case cleanDockerImagePath:
			body = cleanDockerImageDF
		case cleanDockerVolumePath:
			body = cleanDockerVolumeDF
		default:
			http.NotFound(w, r)
			return
		}
	}
	_, _ = w.Write([]byte(body))
}

func (fake *cleanDockerFake) seen() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.requests...)
}

// cleanDockerServe는 임시 폴더로 옮긴 뒤 상대 경로 socket에서 서버를 연다. unix socket 경로는 약 100바이트를 넘으면 안 된다.
func cleanDockerServe(t *testing.T, handler http.Handler) string {
	t.Helper()
	t.Chdir(t.TempDir())
	listener, err := net.Listen("unix", "docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "docker.sock"
}

// cleanDockerDeps는 docker가 있고 끝점이 socket인 deps다. dials는 연결 시도 수를 센다.
func cleanDockerDeps(t *testing.T, socket string, dials *atomic.Int32) cleanDeps {
	t.Helper()
	deps := cleanTestDeps(cleanTestHome(t), func(_ context.Context, spec cleanToolSpec) cleanToolOutput {
		if spec.Name != "docker" || !reflect.DeepEqual(spec.Args, []string{"context", "inspect", "--format", "{{json .Endpoints.docker}}"}) {
			t.Errorf("unexpected tool call %+v", spec)
		}
		return cleanToolOutput{Stdout: fmt.Sprintf(`{"Host":"unix://%s","SkipTLSVerify":false}`+"\n", socket)}
	})
	deps.lookPath = func(name string) (string, error) { return "/bin/" + name, nil }
	deps.dockerDial = func(ctx context.Context, path string) (net.Conn, error) {
		dials.Add(1)
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", path)
	}
	return deps
}

func cleanDockerShorten(t *testing.T, ping, info, dataFrame, volume time.Duration) {
	t.Helper()
	oldPing, oldInfo, oldDF, oldVolume := cleanDockerPingTimeout, cleanDockerInfoTimeout, cleanDockerDFTimeout, cleanDockerVolumeTimeout
	cleanDockerPingTimeout, cleanDockerInfoTimeout, cleanDockerDFTimeout, cleanDockerVolumeTimeout = ping, info, dataFrame, volume
	t.Cleanup(func() {
		cleanDockerPingTimeout, cleanDockerInfoTimeout, cleanDockerDFTimeout, cleanDockerVolumeTimeout = oldPing, oldInfo, oldDF, oldVolume
	})
}

func cleanDockerType(t *testing.T, section cleanDocker, kind string) cleanDockerTypeResult {
	t.Helper()
	for _, result := range section.Types {
		if result.Type == kind {
			return result
		}
	}
	t.Fatalf("section lacks type %q: %+v", kind, section)
	return cleanDockerTypeResult{}
}

func cleanJSONMap(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestCleanDockerReadsEngineTotalsWithGETOnly(t *testing.T) {
	fake := newCleanDockerFake()
	socket := cleanDockerServe(t, fake)
	var dials atomic.Int32
	section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
	if section.Status != cleanMeasured || len(section.Reasons) != 0 || section.APIVersion != "1.56" || section.ServerVersion != "29.8.2" ||
		section.OperatingSystem != "OrbStack" || section.Inside != cleanOrbStack || section.MeasuredAt == nil || section.ElapsedMS == nil {
		t.Fatalf("section = %+v", section)
	}
	wantRequests := []string{"GET " + cleanDockerPingPath, "GET " + cleanDockerInfoPath, "GET " + cleanDockerImagePath, "GET " + cleanDockerVolumePath}
	if got := fake.seen(); !reflect.DeepEqual(got, wantRequests) {
		t.Fatalf("requests = %v", got)
	}
	var kinds []string
	for _, result := range section.Types {
		kinds = append(kinds, result.Type)
	}
	if !reflect.DeepEqual(kinds, cleanDockerTypes) {
		t.Fatalf("types = %v", kinds)
	}
	want := map[string][4]int64{
		cleanDockerImages:     {92819046936, 49040816462, 287, 12},
		cleanDockerContainers: {1593687321, 1593687321, 20, 0},
		cleanDockerBuildCache: {3730872599, 2591000565, 277, 0},
	}
	for kind, values := range want {
		result := cleanDockerType(t, section, kind)
		if result.Status != cleanMeasured || *result.TotalBytes != values[0] || *result.ReclaimableBytes != values[1] ||
			*result.TotalCount != values[2] || *result.ActiveCount != values[3] || result.UserData || result.Cleanup == "" {
			t.Fatalf("%s = %+v", kind, result)
		}
	}
	volumes := cleanDockerType(t, section, cleanDockerVolumes)
	if volumes.Status != cleanMeasured || *volumes.TotalBytes != 20817611480 || *volumes.TotalCount != 98 || *volumes.ActiveCount != 13 ||
		volumes.ReclaimableBytes != nil || volumes.Cleanup != "" || !volumes.UserData {
		t.Fatalf("volumes = %+v", volumes)
	}
	for _, entry := range cleanJSONMap(t, section)["types"].([]interface{}) {
		fields := entry.(map[string]interface{})
		if fields["type"] == cleanDockerVolumes {
			if _, has := fields["reclaimable_bytes"]; has {
				t.Fatalf("volumes carry reclaimable_bytes: %v", fields)
			}
			if fields["user_data"] != true {
				t.Fatalf("volumes are not user data: %v", fields)
			}
		}
	}
}

func TestCleanDockerParsesUsageObjects(t *testing.T) {
	number := func(value int64) *int64 { return &value }
	tests := []struct {
		name   string
		usage  *cleanDockerUsage
		status string
		reason string
		total  int64
		active int64
	}{
		{"no object", nil, cleanNotReported, "", 0, 0},
		{"no size and no count", &cleanDockerUsage{ActiveCount: number(3)}, cleanNotReported, "", 0, 0},
		{"count without size or active", &cleanDockerUsage{TotalCount: number(2)}, cleanMeasured, "", 0, 0},
		{"full", &cleanDockerUsage{TotalSize: number(10), Reclaimable: number(4), TotalCount: number(2), ActiveCount: number(1)}, cleanMeasured, "", 10, 1},
		{"size without count", &cleanDockerUsage{TotalSize: number(10)}, cleanFailed, cleanReasonInvalidOutput, 0, 0},
		{"negative size", &cleanDockerUsage{TotalSize: number(-1), TotalCount: number(1)}, cleanFailed, cleanReasonInvalidOutput, 0, 0},
		{"negative reclaimable", &cleanDockerUsage{TotalSize: number(1), Reclaimable: number(-5), TotalCount: number(1)}, cleanFailed, cleanReasonInvalidOutput, 0, 0},
	}
	for _, test := range tests {
		result := cleanDockerTypeFrom(cleanDockerImages, test.usage)
		if result.Status != test.status {
			t.Fatalf("%s: status = %q", test.name, result.Status)
		}
		if test.reason != "" && !reflect.DeepEqual(result.Reasons, []string{test.reason}) {
			t.Fatalf("%s: reasons = %v", test.name, result.Reasons)
		}
		if test.status != cleanMeasured {
			if result.TotalBytes != nil || result.ReclaimableBytes != nil || result.TotalCount != nil {
				t.Fatalf("%s: a type that is not measured carries numbers: %+v", test.name, result)
			}
			continue
		}
		if *result.TotalBytes != test.total || *result.ActiveCount != test.active || result.ReclaimableBytes == nil {
			t.Fatalf("%s: result = %+v", test.name, result)
		}
	}
}

func TestCleanDockerEndpointStates(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.sock")
	inspect := func(stdout string) func(context.Context, cleanToolSpec) cleanToolOutput {
		return cleanStdout(stdout)
	}
	tests := []struct {
		name    string
		lookup  bool
		run     func(context.Context, cleanToolSpec) cleanToolOutput
		status  string
		reason  string
		detail  string
		wantURL string
	}{
		{"docker CLI missing", false, inspect(""), cleanToolAbsent, "", "", ""},
		{"inspect times out", true, func(context.Context, cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Err: errCleanToolTimeout}
		}, cleanUnknownLocation, cleanReasonToolTimeout, "", ""},
		{"inspect fails", true, func(context.Context, cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Err: errors.New("exit status 1"), ExitCode: 1, Stderr: "no such context\n"}
		}, cleanUnknownLocation, cleanReasonToolFailed, "exit 1: no such context", ""},
		{"null", true, inspect("null\n"), cleanUnknownLocation, cleanReasonInvalidOutput, "null", ""},
		{"array", true, inspect("[]\n"), cleanUnknownLocation, cleanReasonInvalidOutput, "[]", ""},
		{"empty host", true, inspect(`{"Host":""}`), cleanUnknownLocation, cleanReasonInvalidOutput, `{"Host":""}`, ""},
		{"truncated", true, func(context.Context, cleanToolSpec) cleanToolOutput {
			return cleanToolOutput{Stdout: `{"Host":"unix:///x.sock"}`, Truncated: true}
		}, cleanUnknownLocation, cleanReasonInvalidOutput, `{"Host":"unix:///x.sock"}`, ""},
		{"tcp", true, inspect(`{"Host":"tcp://10.0.0.5:2375"}`), cleanUnsupported, cleanReasonEndpointScheme, "tcp", ""},
		{"ssh", true, inspect(`{"Host":"ssh://me@example.com"}`), cleanUnsupported, cleanReasonEndpointScheme, "ssh", ""},
		{"npipe", true, inspect(`{"Host":"npipe:////./pipe/docker_engine"}`), cleanUnsupported, cleanReasonEndpointScheme, "npipe", ""},
		{"no scheme", true, inspect(`{"Host":"example.com"}`), cleanUnknownLocation, cleanReasonInvalidOutput, "", ""},
		{"unix without a path", true, inspect(`{"Host":"unix://"}`), cleanUnknownLocation, cleanReasonInvalidOutput, `{"Host":"unix://"}`, "unix://"},
		{"missing socket", true, inspect(`{"Host":"unix://` + missing + `"}`), cleanNotRunning, cleanReasonSocketMissing, "", "unix://" + missing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var dials atomic.Int32
			deps := cleanDockerDeps(t, "unused", &dials)
			deps.run = test.run
			if !test.lookup {
				deps.lookPath = func(string) (string, error) { return "", errors.New("not found") }
			}
			section := cleanDockerScan(context.Background(), deps)
			if section.Status != test.status || section.Detail != test.detail || section.Endpoint != test.wantURL {
				t.Fatalf("section = %+v", section)
			}
			if test.reason != "" && !reflect.DeepEqual(section.Reasons, []string{test.reason}) {
				t.Fatalf("reasons = %v", section.Reasons)
			}
			if dials.Load() != 0 || len(section.Types) != 0 || section.MeasuredAt != nil {
				t.Fatalf("dials = %d, section = %+v", dials.Load(), section)
			}
		})
	}
}

func TestCleanDockerConnectionFailures(t *testing.T) {
	t.Run("a closed socket refuses the connection", func(t *testing.T) {
		t.Chdir(t.TempDir())
		listener, err := net.Listen("unix", "docker.sock")
		if err != nil {
			t.Fatal(err)
		}
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = listener.Close()
		var dials atomic.Int32
		section := cleanDockerScan(context.Background(), cleanDockerDeps(t, "docker.sock", &dials))
		if section.Status != cleanUnreachable || !reflect.DeepEqual(section.Reasons, []string{cleanReasonConnectionRefused}) || dials.Load() != 1 {
			t.Fatalf("section = %+v, dials = %d", section, dials.Load())
		}
	})
	t.Run("a socket without permission is permission denied", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores socket permissions")
		}
		socket := cleanDockerServe(t, newCleanDockerFake())
		if err := os.Chmod(socket, 0); err != nil {
			t.Fatal(err)
		}
		var dials atomic.Int32
		section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
		if section.Status != cleanUnreachable || !reflect.DeepEqual(section.Reasons, []string{cleanReasonPermission}) {
			t.Fatalf("section = %+v", section)
		}
	})
	t.Run("a ping that never answers is an engine timeout", func(t *testing.T) {
		cleanDockerShorten(t, 80*time.Millisecond, time.Second, time.Second, time.Second)
		fake := newCleanDockerFake()
		fake.delay[cleanDockerPingPath] = 5 * time.Second
		socket := cleanDockerServe(t, fake)
		var dials atomic.Int32
		section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
		if section.Status != cleanUnreachable || !reflect.DeepEqual(section.Reasons, []string{cleanReasonEngineTimeout}) {
			t.Fatalf("section = %+v", section)
		}
		if got := fake.seen(); len(got) != 1 {
			t.Fatalf("requests after a timed out ping = %v", got)
		}
	})
}

func TestCleanDockerRejectsUnusableResponses(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*cleanDockerFake)
		status   string
		reason   string
		detail   string
		requests int
	}{
		{"API older than 1.52", func(f *cleanDockerFake) { f.api = "1.51" }, cleanUnsupported, cleanReasonAPIVersion, "1.51", 1},
		{"API 1.9 is older than 1.52", func(f *cleanDockerFake) { f.api = "1.9" }, cleanUnsupported, cleanReasonAPIVersion, "1.9", 1},
		{"API 0.99", func(f *cleanDockerFake) { f.api = "0.99" }, cleanUnsupported, cleanReasonAPIVersion, "0.99", 1},
		{"no Api-Version header", func(f *cleanDockerFake) { f.api = "" }, cleanFailed, cleanReasonInvalidOutput, "Api-Version: ", 1},
		{"Api-Version is not a number", func(f *cleanDockerFake) { f.api = "latest" }, cleanFailed, cleanReasonInvalidOutput, "Api-Version: latest", 1},
		{"ping HTTP 500", func(f *cleanDockerFake) { f.status[cleanDockerPingPath] = 500 }, cleanFailed, cleanReasonHTTPStatus, "HTTP 500", 1},
		{"info HTTP 500", func(f *cleanDockerFake) { f.status[cleanDockerInfoPath] = 500 }, cleanFailed, cleanReasonHTTPStatus, "HTTP 500", 2},
		{"info is not JSON", func(f *cleanDockerFake) { f.body[cleanDockerInfoPath] = "<html>" }, cleanFailed, cleanReasonInvalidOutput, errCleanDockerInvalid.Error(), 2},
		{"info over 16 MiB", func(f *cleanDockerFake) { f.body[cleanDockerInfoPath] = strings.Repeat("x", cleanDockerBodyMax+1) },
			cleanFailed, cleanReasonInvalidOutput, errCleanDockerTooLarge.Error(), 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newCleanDockerFake()
			test.setup(fake)
			socket := cleanDockerServe(t, fake)
			var dials atomic.Int32
			section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
			if section.Status != test.status || !reflect.DeepEqual(section.Reasons, []string{test.reason}) || section.Detail != test.detail {
				t.Fatalf("section = %+v", section)
			}
			if got := fake.seen(); len(got) != test.requests || len(section.Types) != 0 {
				t.Fatalf("requests = %v, types = %v (no df request is allowed)", got, section.Types)
			}
		})
	}
	t.Run("a df response over 16 MiB fails only its types", func(t *testing.T) {
		fake := newCleanDockerFake()
		fake.body[cleanDockerImagePath] = strings.Repeat("x", cleanDockerBodyMax+1)
		socket := cleanDockerServe(t, fake)
		var dials atomic.Int32
		section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
		if section.Status != cleanPartial || !reflect.DeepEqual(section.Reasons, []string{cleanReasonInvalidOutput}) {
			t.Fatalf("section = %+v", section)
		}
		if cleanDockerType(t, section, cleanDockerImages).Status != cleanFailed || cleanDockerType(t, section, cleanDockerVolumes).Status != cleanMeasured {
			t.Fatalf("types = %+v", section.Types)
		}
	})
	t.Run("a df HTTP error fails its types with the status", func(t *testing.T) {
		fake := newCleanDockerFake()
		fake.status[cleanDockerVolumePath] = 500
		socket := cleanDockerServe(t, fake)
		var dials atomic.Int32
		section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
		volumes := cleanDockerType(t, section, cleanDockerVolumes)
		if section.Status != cleanPartial || volumes.Status != cleanFailed || !reflect.DeepEqual(volumes.Reasons, []string{cleanReasonHTTPStatus}) {
			t.Fatalf("section = %+v", section)
		}
	})
	t.Run("a df response that is not JSON fails its types", func(t *testing.T) {
		fake := newCleanDockerFake()
		fake.body[cleanDockerImagePath] = "not json"
		fake.body[cleanDockerVolumePath] = "not json"
		socket := cleanDockerServe(t, fake)
		var dials atomic.Int32
		section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
		if section.Status != cleanFailed || !reflect.DeepEqual(section.Reasons, []string{cleanReasonInvalidOutput}) {
			t.Fatalf("section = %+v", section)
		}
	})
}

func TestCleanDockerVolumeTimeoutOnlyDropsVolumes(t *testing.T) {
	cleanDockerShorten(t, time.Second, time.Second, time.Second, 100*time.Millisecond)
	fake := newCleanDockerFake()
	fake.delay[cleanDockerVolumePath] = 5 * time.Second
	socket := cleanDockerServe(t, fake)
	var dials atomic.Int32
	section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
	if section.Status != cleanPartial || !reflect.DeepEqual(section.Reasons, []string{cleanReasonTimeout}) {
		t.Fatalf("section = %+v", section)
	}
	volumes := cleanDockerType(t, section, cleanDockerVolumes)
	if volumes.Status != cleanFailed || !reflect.DeepEqual(volumes.Reasons, []string{cleanReasonTimeout}) || volumes.TotalBytes != nil {
		t.Fatalf("volumes = %+v", volumes)
	}
	for _, kind := range []string{cleanDockerImages, cleanDockerContainers, cleanDockerBuildCache} {
		if cleanDockerType(t, section, kind).Status != cleanMeasured {
			t.Fatalf("%s = %+v", kind, cleanDockerType(t, section, kind))
		}
	}
}

func TestCleanDockerEveryDFTimeoutFailsTheSection(t *testing.T) {
	cleanDockerShorten(t, time.Second, time.Second, 80*time.Millisecond, 80*time.Millisecond)
	fake := newCleanDockerFake()
	fake.delay[cleanDockerImagePath] = 5 * time.Second
	fake.delay[cleanDockerVolumePath] = 5 * time.Second
	socket := cleanDockerServe(t, fake)
	var dials atomic.Int32
	section := cleanDockerScan(context.Background(), cleanDockerDeps(t, socket, &dials))
	if section.Status != cleanFailed || !reflect.DeepEqual(section.Reasons, []string{cleanReasonTimeout}) || len(section.Types) != 4 {
		t.Fatalf("section = %+v", section)
	}
	if section.Inside != cleanOrbStack {
		t.Fatalf("a failed section still knows where the engine runs: %+v", section)
	}
}

func TestCleanDockerStopsWhenTheContextIsDone(t *testing.T) {
	fake := newCleanDockerFake()
	socket := cleanDockerServe(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var dials atomic.Int32
	section := cleanDockerScan(ctx, cleanDockerDeps(t, socket, &dials))
	if section.Status != cleanNotScanned || !reflect.DeepEqual(section.Reasons, []string{cleanReasonStopped}) || dials.Load() != 0 || len(fake.seen()) != 0 {
		t.Fatalf("section = %+v", section)
	}
	t.Run("the global limit ends during the ping", func(t *testing.T) {
		slow := newCleanDockerFake()
		slow.delay[cleanDockerPingPath] = 5 * time.Second
		socket := cleanDockerServe(t, slow)
		limited, release := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer release()
		section := cleanDockerScan(limited, cleanDockerDeps(t, socket, &dials))
		if section.Status != cleanNotScanned || !reflect.DeepEqual(section.Reasons, []string{cleanReasonTimeout}) {
			t.Fatalf("section = %+v", section)
		}
	})
	expired, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	<-expired.Done()
	if section := cleanDockerScan(expired, cleanDockerDeps(t, socket, &dials)); section.Status != cleanNotScanned || section.Reasons[0] != cleanReasonTimeout {
		t.Fatalf("section = %+v", section)
	}
}

func TestCleanDockerVersionSupported(t *testing.T) {
	tests := []struct {
		version          string
		supported, valid bool
	}{
		{"1.56", true, true}, {"1.52", true, true}, {"1.51", false, true}, {"1.9", false, true}, {"2.0", true, true}, {"0.99", false, true},
		{"", false, false}, {"1", false, false}, {"1.x", false, false}, {"x.52", false, false}, {"1.-3", false, false},
	}
	for _, test := range tests {
		if supported, valid := cleanDockerVersionSupported(test.version); supported != test.supported || valid != test.valid {
			t.Fatalf("%q = %v, %v", test.version, supported, valid)
		}
	}
}

func TestCleanVMDisksByPlatform(t *testing.T) {
	if got := cleanVMDisks("linux", "/home/me"); len(got) != 0 {
		t.Fatalf("linux = %+v", got)
	}
	darwin := cleanVMDisks("darwin", "/Users/me")
	if len(darwin) != 1 || darwin[0].id != cleanOrbStack ||
		darwin[0].path != "/Users/me/Library/Group Containers/HUAQ24HBR6.dev.orbstack/data" ||
		!reflect.DeepEqual(darwin[0].holds, []string{"docker_engine", "linux_machines"}) {
		t.Fatalf("darwin = %+v", darwin)
	}
	if disks := cleanVMScan(context.Background(), cleanTestDeps(t.TempDir(), nil), "linux"); disks == nil || len(disks) != 0 {
		t.Fatalf("linux scan = %#v", disks)
	}
}

func TestCleanVMScanMeasuresAllocatedBlocksNotTheLogicalSize(t *testing.T) {
	home := cleanTestHome(t)
	dir := cleanVMDisks("darwin", home)[0].path
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const logical = 256 << 20
	if err := os.Truncate(filepath.Join(dir, "data.img.raw"), 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "data.img.raw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(logical); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	disks := cleanVMScan(context.Background(), cleanTestDeps(home, nil), "darwin")
	if len(disks) != 1 {
		t.Fatalf("disks = %+v", disks)
	}
	disk := disks[0]
	if disk.Status != cleanMeasured || disk.UsageBytes == nil || !disk.UserData || disk.Filesystem == nil || disk.MeasuredAt == nil {
		t.Fatalf("disk = %+v", disk)
	}
	if *disk.UsageBytes >= logical/2 {
		t.Skipf("this file system does not keep the file sparse (%d of %d bytes)", *disk.UsageBytes, logical)
	}
}

func TestCleanVMScanFailuresAreNotZeroBytes(t *testing.T) {
	home := cleanTestHome(t)
	t.Run("a missing disk is absent", func(t *testing.T) {
		disk := cleanVMScan(context.Background(), cleanTestDeps(home, nil), "darwin")[0]
		if disk.Status != cleanAbsent || disk.UsageBytes != nil {
			t.Fatalf("disk = %+v", disk)
		}
		if _, has := cleanJSONMap(t, disk)["usage_bytes"]; has {
			t.Fatal("an absent disk has usage_bytes")
		}
	})
	t.Run("a disk that cannot be read is unreadable", func(t *testing.T) {
		if err := os.MkdirAll(cleanVMDisks("darwin", home)[0].path, 0o755); err != nil {
			t.Fatal(err)
		}
		deps := cleanTestDeps(home, nil)
		deps.measure = func(context.Context, duOptions, duSources) (duScan, error) { return duScan{}, fs.ErrPermission }
		disk := cleanVMScan(context.Background(), deps, "darwin")[0]
		if disk.Status != cleanUnreadable || !reflect.DeepEqual(disk.Reasons, []string{cleanReasonPermission}) || disk.UsageBytes != nil {
			t.Fatalf("disk = %+v", disk)
		}
	})
	t.Run("a stopped scan is not scanned", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		disk := cleanVMScan(ctx, cleanTestDeps(home, nil), "darwin")[0]
		if disk.Status != cleanNotScanned || disk.UsageBytes != nil {
			t.Fatalf("disk = %+v", disk)
		}
	})
}

func TestCleanResultReflectsTheSectionStates(t *testing.T) {
	disk := func(status string) []cleanVMDisk { return []cleanVMDisk{{ID: cleanOrbStack, Status: status}} }
	// 다른 섹션은 확인할 일이 없는 상태로 둔다.
	report := func(docker cleanDocker, disks []cleanVMDisk) cleanReport {
		return cleanReport{docker: docker, vmDisks: disks, worktrees: cleanWorktrees{Status: cleanAbsent},
			projectBuildDirs: cleanProjectBuildDirs{Status: cleanNoProjectRoot}}
	}
	tests := []struct {
		name   string
		report cleanReport
		status Status
	}{
		{"no engine tool and no disk", report(cleanDocker{Status: cleanToolAbsent}, disk(cleanAbsent)), StatusPass},
		{"a stopped engine", report(cleanDocker{Status: cleanNotRunning}, nil), StatusPass},
		{"a measured engine and disk", report(cleanDocker{Status: cleanMeasured}, disk(cleanMeasured)), StatusPass},
		{"an unreachable engine", report(cleanDocker{Status: cleanUnreachable}, nil), StatusWarn},
		{"a partial engine", report(cleanDocker{Status: cleanPartial}, nil), StatusWarn},
		{"an unsupported engine", report(cleanDocker{Status: cleanUnsupported}, nil), StatusWarn},
		{"an unreadable disk", report(cleanDocker{Status: cleanToolAbsent}, disk(cleanUnreadable)), StatusWarn},
	}
	for _, test := range tests {
		if got := cleanResult(Result{}, test.report, time.Minute, nil); got.Status != test.status {
			t.Fatalf("%s: status = %q", test.name, got.Status)
		}
	}
}

func TestRunCleanTextShowsTheEngineAndTheDiskSeparately(t *testing.T) {
	socket := cleanDockerServe(t, newCleanDockerFake())
	var dials atomic.Int32
	deps := cleanDockerDeps(t, socket, &dials)
	inner := deps.run
	deps.run = func(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
		if spec.Name == "docker" {
			return inner(ctx, spec)
		}
		return cleanToolOutput{Stdout: filepath.Join(deps.home, "absent", spec.Name) + "\n"}
	}
	var output bytes.Buffer
	if code := runCleanWith([]string{"-v"}, "test", deps, true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d\n%s", code, output.String())
	}
	text := output.String()
	for _, want := range []string{"Docker engine (OrbStack, API 1.56)", "engine totals, not added to any total", "images", "engine reclaimable 45.7 GB",
		"user data", "docker image prune -a", "docker container prune", "docker builder prune"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "docker volume prune") {
		t.Fatalf("text suggests pruning volumes:\n%s", text)
	}
}

func TestCleanBoundaryDockerEngineAndDiskAreNeverSummed(t *testing.T) {
	socket := cleanDockerServe(t, newCleanDockerFake())
	var dials atomic.Int32
	deps := cleanDockerDeps(t, socket, &dials)
	home := deps.home
	registry := filepath.Join(home, ".cargo/registry")
	cleanTestTree(t, registry, 4<<10)
	disk := cleanVMDisks("darwin", home)[0].path
	cleanTestTree(t, disk, 4<<10)
	const diskBytes = 100 << 30
	deps.lookPath = func(name string) (string, error) {
		if name == "docker" {
			return "/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	deps.measure = func(_ context.Context, options duOptions, _ duSources) (duScan, error) {
		total := uint64(8192)
		if strings.HasSuffix(options.root, "orbstack/data") || strings.HasSuffix(options.root, "HUAQ24HBR6.dev.orbstack/data") {
			total = diskBytes
		}
		scan := cleanFakeScan(total)
		scan.filesystem = duFilesystem{Mount: "/", FSType: "apfs"}
		return scan, nil
	}
	report := cleanCollect(context.Background(), deps, "darwin")
	if report.docker.Status != cleanMeasured || report.docker.Inside != cleanOrbStack {
		t.Fatalf("docker = %+v", report.docker)
	}
	if len(report.vmDisks) != 1 || !report.vmDisks[0].counted() || *report.vmDisks[0].UsageBytes != diskBytes {
		t.Fatalf("vm disks = %+v", report.vmDisks)
	}
	var want uint64
	for _, item := range report.candidates {
		if item.counted() && item.Inside == "" {
			want += *item.UsageBytes
		}
	}
	if want == 0 || len(report.filesystems) != 1 || report.filesystems[0].UsageBytes != want {
		t.Fatalf("filesystems = %+v, candidate sum = %d", report.filesystems, want)
	}
	for _, id := range report.filesystems[0].Candidates {
		if id == cleanOrbStack {
			t.Fatalf("the VM disk is in the sum: %v", report.filesystems[0].Candidates)
		}
	}
	result := cleanResult(Result{}, report, time.Minute, nil)
	if _, ok := result.Metrics["docker"].(cleanDocker); !ok {
		t.Fatalf("metrics = %v", result.Metrics)
	}
	if got := cleanJSONMap(t, result.Metrics)["docker"].(map[string]interface{})["inside"]; got != cleanOrbStack {
		t.Fatalf("docker.inside = %v", got)
	}
}

func TestCleanBoundaryDockerStatesAreNotZeroBytes(t *testing.T) {
	scenarios := []struct {
		name  string
		setup func(*testing.T, *cleanDockerFake, *cleanDeps) string
	}{
		{"tool absent", func(t *testing.T, _ *cleanDockerFake, deps *cleanDeps) string {
			deps.lookPath = func(string) (string, error) { return "", errors.New("not found") }
			return "docker.sock"
		}},
		{"socket missing", func(*testing.T, *cleanDockerFake, *cleanDeps) string {
			return filepath.Join(os.TempDir(), "edc-no-such-docker.sock")
		}},
		{"old API", func(_ *testing.T, fake *cleanDockerFake, _ *cleanDeps) string {
			fake.api = "1.51"
			return "docker.sock"
		}},
		{"HTTP error", func(_ *testing.T, fake *cleanDockerFake, _ *cleanDeps) string {
			fake.status[cleanDockerInfoPath] = 500
			return "docker.sock"
		}},
		{"volume timeout", func(t *testing.T, fake *cleanDockerFake, _ *cleanDeps) string {
			cleanDockerShorten(t, time.Second, time.Second, time.Second, 80*time.Millisecond)
			fake.delay[cleanDockerVolumePath] = 5 * time.Second
			return "docker.sock"
		}},
		{"no volumes reported", func(_ *testing.T, fake *cleanDockerFake, _ *cleanDeps) string {
			fake.body[cleanDockerVolumePath] = `{}`
			return "docker.sock"
		}},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fake := newCleanDockerFake()
			socket := cleanDockerServe(t, fake)
			var dials atomic.Int32
			deps := cleanDockerDeps(t, socket, &dials)
			path := scenario.setup(t, fake, &deps)
			deps.run = cleanStdout(fmt.Sprintf(`{"Host":"unix://%s"}`, path))
			section := cleanDockerScan(context.Background(), deps)
			fields := cleanJSONMap(t, section)
			notMeasuredSection := section.Status != cleanMeasured && section.Status != cleanPartial
			if _, has := fields["types"]; has == notMeasuredSection {
				t.Fatalf("types key = %v for status %q", has, section.Status)
			}
			for _, entry := range section.Types {
				entryFields := cleanJSONMap(t, entry)
				for _, key := range []string{"total_bytes", "reclaimable_bytes", "total_count", "active_count"} {
					if _, has := entryFields[key]; has != (entry.Status == cleanMeasured && !(key == "reclaimable_bytes" && entry.Type == cleanDockerVolumes)) {
						t.Fatalf("%s has %s = %v with status %q", entry.Type, key, has, entry.Status)
					}
				}
			}
			for _, verbose := range []bool{false, true} {
				if text := formatCleanDocker(section, verbose, 0); strings.Contains(text, "0 B") || strings.TrimSpace(text) == "" {
					t.Fatalf("text = %q", text)
				}
			}
		})
	}
	t.Run("the plain text run", func(t *testing.T) {
		var output bytes.Buffer
		if code := runCleanWith(nil, "test", cleanRunDeps(t, cleanTestHome(t)), true, nil, &output); code != 0 || strings.Contains(output.String(), "0 B") {
			t.Fatalf("exit code = %d\n%s", code, output.String())
		}
		if !strings.Contains(output.String(), "Docker engine: tool not found") {
			t.Fatalf("text = %s", output.String())
		}
	})
}

// 개수 칸은 volumes 행을 포함해 모든 행에서 같은 열에 온다. 회수 가능량 문구는 길이가 달라 줄 끝에 둔다.
func TestFormatCleanDockerAlignsTheItemCounts(t *testing.T) {
	value := func(number int64) *int64 { return &number }
	measured := func(kind string, total, reclaimable, count, active int64) cleanDockerTypeResult {
		return cleanDockerTypeResult{Type: kind, Status: cleanMeasured, TotalBytes: value(total), ReclaimableBytes: value(reclaimable), TotalCount: value(count), ActiveCount: value(active)}
	}
	section := cleanDocker{Status: cleanMeasured, Types: []cleanDockerTypeResult{
		measured(cleanDockerImages, 12_100_000_000, 1_300_000_000, 9, 4),
		measured(cleanDockerContainers, 216_500_000, 216_200_000, 4, 3),
		measured(cleanDockerBuildCache, 8_700_000_000, 8_500_000_000, 30, 0),
		measured(cleanDockerVolumes, 407_000_000, 0, 5, 3),
	}}
	var columns []int
	for _, row := range strings.Split(formatCleanDocker(section, false, 0), "\n") {
		if index := strings.Index(row, " items, "); index >= 0 {
			columns = append(columns, liveWidth(row[:strings.LastIndex(row[:index], "  ")]))
		}
	}
	if len(columns) != 4 {
		t.Fatalf("rows with a count = %d:\n%s", len(columns), formatCleanDocker(section, false, 0))
	}
	for _, column := range columns[1:] {
		if column != columns[0] {
			t.Fatalf("count columns = %v:\n%s", columns, formatCleanDocker(section, false, 0))
		}
	}
}
