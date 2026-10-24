//go:build linux || darwin

package edc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func ctrSocketFixture(t testing.TB, handler http.Handler) *ctrEngine {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "edc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
	return newCtrEngine(path)
}

func TestCtrSocketNegotiatesAndDecodesTypedInspect(t *testing.T) {
	var requests []string
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.50","MinAPIVersion":"1.41"}`)
		case "/v1.50/containers/web/json":
			fmt.Fprint(w, `{"Id":"0123456789abcdef","Name":"/web","Image":"sha256:abc","Config":{"Image":"nginx:latest"},"State":{"Status":"running","Pid":42,"StartedAt":"2026-10-23T10:00:00Z","FinishedAt":"0001-01-01T00:00:00Z","Health":{"Status":"healthy"}},"RestartCount":2,"HostConfig":{"Memory":4096,"PidsLimit":100,"RestartPolicy":{"Name":"on-failure","MaximumRetryCount":4}},"NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"80"}],"80/tcp":null}}}`)
		case "/v1.50/containers/0123456789abcdef/stats":
			fmt.Fprint(w, `{"memory_stats":{"usage":2048,"limit":8192},"pids_stats":{"current":7,"limit":100}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	got, err := engine.inspect(context.Background(), "web", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "0123456789abcdef" || got.Name != "web" || got.PID != 42 || got.RestartPolicy != "on-failure" || got.RestartMax != 4 || got.Health != "healthy" || got.MemoryLimit == nil || *got.MemoryLimit != 4096 || got.PidsLimit == nil || *got.PidsLimit != 100 || len(got.Ports) != 1 || got.Ports[0].Hosts[0] != "127.0.0.1:80" {
		t.Fatalf("inspect = %+v", got)
	}
	limits := engine.stats(context.Background(), got.ID, got, false)
	if limits.Memory.Used == nil || *limits.Memory.Used != 2048 || limits.Memory.Max == nil || *limits.Memory.Max != 4096 || limits.Pids.Status != "available" || limits.Pids.Current != 7 || limits.Pids.Max != 100 {
		t.Fatalf("decoded inspect limits did not reach stats: %+v", limits)
	}
	if engine.version != "1.50" || len(requests) != 3 || requests[0] != "GET /version" || requests[1] != "GET /v1.50/containers/web/json" || requests[2] != "GET /v1.50/containers/0123456789abcdef/stats?one-shot=true&stream=false" {
		t.Fatalf("version %q, requests %v", engine.version, requests)
	}
}

func TestCtrSocketEndpointRules(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	t.Setenv("DOCKER_CONTEXT", "default")
	if _, err := ctrSocketEndpoint(); err != nil {
		t.Fatalf("default context must ignore DOCKER_HOST: %v", err)
	}
	t.Setenv("DOCKER_CONTEXT", "work")
	if _, err := ctrSocketEndpoint(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("nondefault context error = %v", err)
	}
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix:///tmp/daemon.sock")
	if got, err := ctrSocketEndpoint(); err != nil || got != "/tmp/daemon.sock" {
		t.Fatalf("endpoint = %q, %v", got, err)
	}
	for _, host := range []string{"tcp://127.0.0.1:2375", "ssh://user@host", "unix:relative.sock"} {
		t.Setenv("DOCKER_HOST", host)
		if _, err := ctrSocketEndpoint(); err == nil {
			t.Errorf("accepted %q", host)
		}
	}
}

func TestCtrSocketTopValidatesColumnsAndPreservesCommand(t *testing.T) {
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		if r.URL.Path != "/v1.51/containers/id/top" || r.URL.Query().Get("ps_args") != "-eo pid,pcpu,rss,args" {
			t.Errorf("request %s", r.URL.String())
		}
		fmt.Fprint(w, `{"Titles":["RSS","COMMAND","%CPU","PID"],"Processes":[["12","worker --name a b","1.5","7"]]}`)
	}))
	rows, err := engine.top(context.Background(), "id")
	if err != nil || len(rows) != 1 || rows[0].PID != 7 || rows[0].RSS != 12*1024 || rows[0].CPU != 1.5 || rows[0].Command != "worker --name a b" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestCtrSocketLogFramesAndErrors(t *testing.T) {
	payload := []byte("2026-10-23T10:00:00Z error failed\n")
	frame := make([]byte, 8+len(payload))
	frame[0] = 2
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	got, err := ctrDecodeLogFrames(frame)
	if err != nil || got != string(payload) {
		t.Fatalf("frame = %q, %v", got, err)
	}
	for _, bad := range [][]byte{{1, 0}, {3, 0, 0, 0, 0, 0, 0, 0}, {1, 1, 0, 0, 0, 0, 0, 0}, {1, 0, 0, 0, 0, 0, 0, 2, 'x'}} {
		if _, err := ctrDecodeLogFrames(bad); err == nil {
			t.Errorf("accepted frame %v", bad)
		}
	}
}

func BenchmarkCtrSocketList(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ids := make([]string, count)
			for i := range ids {
				ids[i] = fmt.Sprintf("%064x", i+1)
			}
			server := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/version":
					fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
				case "/v1.51/containers/json":
					rows := make([]ctrAPIContainer, count)
					for i, id := range ids {
						rows[i].ID = id
					}
					json.NewEncoder(w).Encode(rows)
				default:
					id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/containers/"), "/json")
					fmt.Fprintf(w, `{"Id":%q,"Name":"/web","Image":"sha256:abc","Config":{"Image":"nginx"},"State":{"Status":"exited","StartedAt":"0001-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}}`, id)
				}
			})
			dir, err := os.MkdirTemp("/tmp", "edc-")
			if err != nil {
				b.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "docker.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				b.Fatal(err)
			}
			httpServer := &http.Server{Handler: server}
			go httpServer.Serve(listener)
			defer httpServer.Close()
			engine := newCtrEngine(path)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := engine.list(context.Background()); err != nil {
					b.Fatal(err)
				}
				if _, err := engine.inspectMany(context.Background(), ids); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestCtrSocketVersionRejectsInvalidAndDisjointRanges(t *testing.T) {
	for _, tc := range []struct{ min, max string }{{"1.9", "1.9"}, {"1.x", "1.51"}, {"1.52", "1.60"}} {
		t.Run(tc.min+"-"+tc.max, func(t *testing.T) {
			engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"ApiVersion":%q,"MinAPIVersion":%q}`, tc.max, tc.min)
			}))
			if _, err := engine.apiVersion(context.Background()); err == nil {
				t.Fatal("expected version negotiation error")
			}
		})
	}
}

func TestCtrSocketAPIErrorIncludesBoundedDaemonMessage(t *testing.T) {
	for _, code := range []int{403, 404, 500} {
		engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/version" {
				fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
				return
			}
			w.WriteHeader(code)
			fmt.Fprint(w, "daemon denied request")
		}))
		_, err := engine.get(context.Background(), "/failure", 128)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", code)) || !strings.Contains(err.Error(), "daemon denied request") {
			t.Errorf("HTTP %d error = %v", code, err)
		}
	}
}

func TestCtrSocketStatsPreservesZeroAndMarksMissingFields(t *testing.T) {
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/id/stats":
			fmt.Fprint(w, `{"memory_stats":{"usage":0,"limit":9999},"cpu_stats":{"throttling_data":{"periods":0,"throttled_periods":0,"throttled_time":1500}},"pids_stats":{"current":0,"limit":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	memory, pids := int64(4096), int64(0)
	limits := engine.stats(context.Background(), "id", ctrInspect{MemoryLimit: &memory, PidsLimit: &pids}, false)
	if limits.Memory.Used == nil || *limits.Memory.Used != 0 || limits.Memory.Max == nil || *limits.Memory.Max != 4096 || limits.CPU.Status != "available" || limits.CPU.Periods != 0 || limits.CPU.ThrottledUS != 1 || limits.Pids.Status != "available" || limits.Pids.Current != 0 || !limits.Pids.Unlimited || limits.Events.Status != "unavailable" {
		t.Fatalf("limits=%+v", limits)
	}
	engine = ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	limits = engine.stats(context.Background(), "id", ctrInspect{}, false)
	if limits.Memory.Used != nil || limits.Memory.Status != "unavailable" || limits.CPU.Status != "unavailable" || limits.Pids.Status != "unavailable" {
		t.Fatalf("missing stats became values: %+v", limits)
	}
}

func TestCtrSocketStatsRendersCPUUsageAndPidsCurrentWithoutKnownLimit(t *testing.T) {
	detailRequest := false
	var detailRequestMu sync.Mutex
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/id/stats":
			detailRequestMu.Lock()
			detailRequest = r.URL.Query().Get("stream") == "false" && r.URL.Query().Get("one-shot") == "false"
			detailRequestMu.Unlock()
			fmt.Fprint(w, `{"read":"2026-10-23T11:00:02Z","preread":"2026-10-23T11:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":1500000000,"percpu_usage":[1000000000,500000000]},"system_cpu_usage":5000000000,"online_cpus":2,"throttling_data":{"periods":100,"throttled_periods":1,"throttled_time":1000}},"precpu_stats":{"cpu_usage":{"total_usage":500000000,"percpu_usage":[300000000,200000000]},"system_cpu_usage":1000000000,"online_cpus":2},"memory_stats":{"usage":4096,"limit":8192},"pids_stats":{"current":17,"limit":18446744073709551615}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	memoryLimit := int64(8192)
	limits := engine.stats(context.Background(), "id", ctrInspect{PID: 42, MemoryLimit: &memoryLimit}, true)
	result := ctrLimitsResult(ctrFacts{Inspect: ctrInspect{PID: 42}, Limits: limits})
	if result.Metrics["cpu_usage_percent"] != 50.0 || !strings.Contains(result.Summary, "CPU 50.00%") {
		t.Fatalf("CPU usage missing from detailed limits: %+v", result)
	}
	detailRequestMu.Lock()
	gotDetailRequest := detailRequest
	detailRequestMu.Unlock()
	if !gotDetailRequest || result.Metrics["pids_current"] != uint64(17) || !strings.Contains(result.Summary, "17 / unknown limit") {
		t.Fatalf("known PIDs current hidden by unknown limit: %+v", result)
	}
}

func TestCtrSocketStatsKeepsListOneShotAndUsesDetailedPair(t *testing.T) {
	var mu sync.Mutex
	var modes []string
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/id/stats":
			mu.Lock()
			modes = append(modes, r.URL.Query().Get("one-shot"))
			mu.Unlock()
			fmt.Fprint(w, `{"read":"2026-10-23T11:00:02Z","preread":"2026-10-23T11:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":150,"percpu_usage":[100,50]},"system_cpu_usage":500,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":50,"percpu_usage":[30,20]},"system_cpu_usage":100,"online_cpus":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	listLimits := engine.stats(context.Background(), "id", ctrInspect{}, false)
	detailLimits := engine.stats(context.Background(), "id", ctrInspect{}, true)
	mu.Lock()
	gotModes := append([]string(nil), modes...)
	mu.Unlock()
	if strings.Join(gotModes, ",") != "true,false" {
		t.Fatalf("stats request modes = %v", gotModes)
	}
	if listLimits.CPUUsage.Status != "unsupported" || detailLimits.CPUUsage.Status != "available" || detailLimits.CPUUsagePercent == nil || *detailLimits.CPUUsagePercent != 50 {
		t.Fatalf("list=%+v detail=%+v", listLimits.CPUUsage, detailLimits.CPUUsage)
	}
}

func TestCtrListStatsKeepsPhaseOneMetricsWhenPhaseTwoFails(t *testing.T) {
	var calls int
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/c/stats":
			calls++
			if calls == 1 {
				fmt.Fprint(w, `{"read":"2026-10-23T11:00:01Z","memory_stats":{"usage":80},"pids_stats":{"current":4},"networks":{"eth0":{"rx_bytes":10,"tx_bytes":20}},"blkio_stats":{"io_service_bytes_recursive":[{"op":"read","value":30},{"op":"write","value":40}]}}`)
			} else {
				http.Error(w, "phase two failed", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	memory, pids := int64(100), int64(8)
	result := engine.listStatsWithWait(context.Background(), []ctrInspect{{ID: "c", Status: "running", MemoryLimit: &memory, PidsLimit: &pids}}, 0)["c"]
	if result.Limits.Memory.Used == nil || *result.Limits.Memory.Used != 80 || result.Limits.Pids.Status != "available" || result.Limits.Pids.Current != 4 || result.NetworkRX == nil || *result.NetworkRX != 10 || result.BlockWrite == nil || *result.BlockWrite != 40 {
		t.Fatalf("phase one metrics were lost: %+v", result)
	}
	if result.Limits.CPUUsagePercent != nil || !strings.Contains(result.CPUReason, "phase two failed") {
		t.Fatalf("CPU error status=%+v reason=%q", result.Limits.CPUUsage, result.CPUReason)
	}
}

func TestCtrListStatsReportsPhaseTwoMetricFallbacks(t *testing.T) {
	calls := 0
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/c/stats":
			calls++
			if calls == 1 {
				fmt.Fprint(w, `{"read":"2026-10-23T11:00:01Z","memory_stats":{"usage":80},"cpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":100,"online_cpus":2},"pids_stats":{"current":4},"networks":{"eth0":{"rx_bytes":10,"tx_bytes":20}},"blkio_stats":{"io_service_bytes_recursive":[{"op":"read","value":30},{"op":"write","value":40}]}}`)
			} else {
				fmt.Fprint(w, `{"read":"2026-10-23T11:00:02Z","memory_stats":{},"cpu_stats":{"cpu_usage":{"total_usage":125},"system_cpu_usage":200,"online_cpus":2},"pids_stats":{},"networks":{"eth0":{"rx_bytes":-1,"tx_bytes":50}},"blkio_stats":{"io_service_bytes_recursive":[]}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	limit, pidsLimit := int64(100), int64(10)
	entry := engine.listStatsWithWait(context.Background(), []ctrInspect{{ID: "c", Status: "running", MemoryLimit: &limit, PidsLimit: &pidsLimit}}, 0)["c"]
	for _, check := range []struct {
		name, reason string
		value        *uint64
		want         uint64
	}{{"memory", entry.Limits.Memory.Reason, entry.Limits.Memory.Used, 80}, {"network rx", entry.NetworkRXReason, entry.NetworkRX, 10}} {
		if check.value == nil || *check.value != check.want || !strings.Contains(check.reason, "using phase-one value") || !strings.Contains(check.reason, "phase-two sample unavailable") {
			t.Fatalf("%s fallback = %v, reason %q", check.name, check.value, check.reason)
		}
	}
	if entry.Limits.CPUUsagePercent == nil || *entry.Limits.CPUUsagePercent != 50 {
		t.Fatalf("CPU = %v, want 50%%", entry.Limits.CPUUsagePercent)
	}
	if entry.Limits.Pids.Status != "available" || entry.Limits.Pids.Current != 4 || !strings.Contains(entry.Limits.Pids.Reason, "using phase-one value") {
		t.Fatalf("PIDs fallback = %+v", entry.Limits.Pids)
	}
	if entry.NetworkTX == nil || *entry.NetworkTX != 50 || entry.NetworkTXReason != "" {
		t.Fatalf("valid phase-two TX was lost: %v, %q", entry.NetworkTX, entry.NetworkTXReason)
	}
	item := ctrListItem{Name: "web", ID: "c", State: "running", Status: "running"}
	applyCtrListStats(&item, entry)
	if item.MemoryBytes == nil || *item.MemoryBytes != 80 || !strings.Contains(item.MemoryUnavailable, "using phase-one value") || item.PIDs == nil || *item.PIDs != 4 || !strings.Contains(item.PIDsUnavailable, "using phase-one value") {
		t.Fatalf("fallback reasons did not reach list item: %+v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil || !strings.Contains(string(encoded), "phase-two sample unavailable") {
		t.Fatalf("JSON did not preserve fallback reason: %s, %v", encoded, err)
	}
	result := evaluateCtrList([]ctrListItem{item}, nil)
	if len(result.Evidence) == 0 || !strings.Contains(result.Evidence[0].Value, "using phase-one value") {
		t.Fatalf("report evidence omitted fallback reason: %+v", result.Evidence)
	}
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	model.items, model.cursor, model.width, model.height = []ctrListItem{item}, 0, 80, 12
	if hint := strings.Join(model.selectedUnavailableLines(), "\n"); !strings.Contains(hint, "using phase-one value") {
		t.Fatalf("selected-row hint omitted fallback reason: %s", hint)
	}
}

func TestCtrListStatsBatchUsesTwoBoundedWavesAndPairsCPU(t *testing.T) {
	ids := make([]string, 9)
	inspects := make([]ctrInspect, len(ids))
	for index := range ids {
		ids[index] = fmt.Sprintf("ctr-%d", index)
		inspects[index] = ctrInspect{ID: ids[index], Status: "running"}
	}
	var mu sync.Mutex
	calls := make(map[string]int, len(ids))
	firstStarted, firstCompleted := 0, 0
	active, maxActive := 0, 0
	secondWaveBeforeBarrier := false
	releaseFirstFour := make(chan struct{})
	var releaseOnce sync.Once
	lastFirstRequest := make(chan struct{})
	releaseLastFirst := make(chan struct{})
	var lastFirstOnce sync.Once
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/containers/"), "/stats")
		mu.Lock()
		calls[id]++
		wave := calls[id]
		holdLastFirst := false
		active++
		maxActive = max(maxActive, active)
		if wave == 1 {
			firstStarted++
			if firstStarted == 4 {
				releaseOnce.Do(func() { close(releaseFirstFour) })
			}
			if firstStarted == len(ids) {
				holdLastFirst = true
				lastFirstOnce.Do(func() { close(lastFirstRequest) })
			}
		} else if firstStarted != len(ids) || firstCompleted != len(ids) {
			secondWaveBeforeBarrier = true
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		if wave == 1 {
			<-releaseFirstFour
			if holdLastFirst {
				<-releaseLastFirst
			}
		}
		phase := "01"
		cpuTotal, system := uint64(100), uint64(100)
		if wave == 2 {
			phase, cpuTotal, system = "02", 125, 200
		}
		fmt.Fprintf(w, ctrListStatsTestJSON, "2026-10-23T11:00:"+phase+"Z", cpuTotal, system)
		if wave == 1 {
			mu.Lock()
			firstCompleted++
			mu.Unlock()
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan map[string]ctrListStats, 1)
	go func() { done <- engine.listStatsWithWait(ctx, inspects, 0) }()
	select {
	case <-lastFirstRequest:
	case <-time.After(time.Second):
		t.Fatal("phase one did not reach its last request")
	}
	close(releaseLastFirst)
	var result map[string]ctrListStats
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("two-wave batch did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 4 {
		t.Fatalf("maximum in-flight stats requests = %d, want 4", maxActive)
	}
	if secondWaveBeforeBarrier {
		t.Fatal("phase two started before every phase-one response completed")
	}
	if len(calls) != len(ids) {
		t.Fatalf("container calls = %d, want %d: %v", len(calls), len(ids), calls)
	}
	for _, id := range ids {
		if calls[id] != 2 {
			t.Fatalf("%s stats calls = %d, want two", id, calls[id])
		}
		if value := result[id].Limits.CPUUsagePercent; value == nil || *value != 50 {
			t.Fatalf("%s CPU = %v, want 50%%", id, value)
		}
	}
}

func TestCtrListStatsBatchCancellationStopsQueuedRequestsAndKeepsCompletedSample(t *testing.T) {
	ids := make([]string, 9)
	inspects := make([]ctrInspect, len(ids))
	for index := range ids {
		ids[index] = fmt.Sprintf("cancel-%d", index)
		inspects[index] = ctrInspect{ID: ids[index], Status: "running", MemoryLimit: int64Ptr(100), PidsLimit: int64Ptr(10)}
	}
	var mu sync.Mutex
	statsRequests, blocked := 0, 0
	successID := ""
	blockedFour := make(chan struct{})
	var blockedOnce sync.Once
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/containers/"), "/stats")
		mu.Lock()
		statsRequests++
		if statsRequests == 1 {
			successID = id
			mu.Unlock()
			fmt.Fprintf(w, ctrListStatsTestJSON, "2026-10-23T11:00:01Z", 100, 100)
			return
		}
		blocked++
		if blocked == 4 {
			blockedOnce.Do(func() { close(blockedFour) })
		}
		mu.Unlock()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan map[string]ctrListStats, 1)
	go func() { done <- engine.listStatsWithWait(ctx, inspects, time.Hour) }()
	select {
	case <-blockedFour:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("four stats requests did not enter concurrently")
	}
	cancel()
	var result map[string]ctrListStats
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled batch did not return promptly")
	}
	mu.Lock()
	gotRequests, gotSuccessID := statsRequests, successID
	mu.Unlock()
	if gotRequests != 5 {
		t.Fatalf("stats GETs after cancellation = %d, want one completed and four in flight", gotRequests)
	}
	sample := result[gotSuccessID]
	if sample.Limits.Memory.Used == nil || *sample.Limits.Memory.Used != 77 || sample.Limits.Pids.Status != "available" || sample.Limits.Pids.Current != 0 {
		t.Fatalf("completed phase-one values were lost: %+v", sample)
	}
}

const ctrListStatsTestJSON = `{"read":%q,"memory_stats":{"usage":77},"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":2},"pids_stats":{"current":0}}`

func int64Ptr(value int64) *int64 { return &value }

func TestCtrNetworkAndBlockTotalsKeepDirectionStatus(t *testing.T) {
	zero, rx, tx := uint64(0), uint64(9), uint64(4)
	counter := func(value uint64) *ctrAPIOptionalUint64 { return &ctrAPIOptionalUint64{Value: &value} }
	networks := map[string]ctrAPINetworkStats{"a": {RXBytes: counter(rx), TXBytes: counter(tx)}, "b": {RXBytes: counter(rx)}}
	rxTotal, txTotal, rxErr, txErr := ctrNetworkTotals(&networks)
	if rxTotal == nil || *rxTotal != 18 || txTotal != nil || rxErr != "" || txErr == "" {
		t.Fatalf("network totals=%v,%v reasons=%q,%q", rxTotal, txTotal, rxErr, txErr)
	}
	empty := map[string]ctrAPINetworkStats{}
	rxTotal, txTotal, _, _ = ctrNetworkTotals(&empty)
	if rxTotal == nil || txTotal == nil || *rxTotal != 0 || *txTotal != 0 {
		t.Fatalf("empty network map is not zero: %v %v", rxTotal, txTotal)
	}
	rows := []ctrAPIBlockIO{{Op: "READ", Value: counter(rx)}, {Op: "write", Value: counter(tx)}, {Op: "Total", Value: counter(rx)}}
	block := &struct {
		ServiceBytes *[]ctrAPIBlockIO `json:"io_service_bytes_recursive"`
	}{ServiceBytes: &rows}
	read, write, _, _ := ctrBlockTotals(block)
	if read == nil || write == nil || *read != 9 || *write != 4 {
		t.Fatalf("block totals=%v,%v", read, write)
	}
	rows = []ctrAPIBlockIO{{Op: "read", Value: nil}, {Op: "write", Value: counter(zero)}}
	read, write, readErr, writeErr := ctrBlockTotals(block)
	if read != nil || readErr == "" || write == nil || *write != 0 || writeErr != "" {
		t.Fatalf("block partial=%v,%v reasons=%q,%q", read, write, readErr, writeErr)
	}
	var stats ctrAPIStats
	if err := json.Unmarshal([]byte(`{"memory_stats":{"usage":7},"networks":{"eth0":{"rx_bytes":-1,"tx_bytes":2}},"blkio_stats":{"io_service_bytes_recursive":[{"op":"read","value":-1},{"op":"read","value":"bad"},{"op":"write","value":5}]}}`), &stats); err != nil {
		t.Fatal(err)
	}
	rxTotal, txTotal, rxErr, txErr = ctrNetworkTotals(stats.Networks)
	read, write, readErr, writeErr = ctrBlockTotals(stats.Blkio)
	if stats.Memory.Usage == nil || *stats.Memory.Usage != 7 || rxTotal != nil || rxErr == "" || txTotal == nil || *txTotal != 2 || txErr != "" || read != nil || readErr == "" || write == nil || *write != 5 || writeErr != "" {
		t.Fatalf("invalid direction corrupted other metrics: %+v net=%v,%v (%q,%q) block=%v,%v (%q,%q)", stats, rxTotal, txTotal, rxErr, txErr, read, write, readErr, writeErr)
	}
	maxValue, one := ^uint64(0), uint64(1)
	networks = map[string]ctrAPINetworkStats{"a": {RXBytes: counter(maxValue), TXBytes: counter(zero)}, "b": {RXBytes: counter(one), TXBytes: counter(one)}}
	rxTotal, txTotal, rxErr, txErr = ctrNetworkTotals(&networks)
	if rxTotal != nil || rxErr == "" || txTotal == nil || *txTotal != 1 || txErr != "" {
		t.Fatalf("network overflow status=%v,%v (%q,%q)", rxTotal, txTotal, rxErr, txErr)
	}
	rows = []ctrAPIBlockIO{{Op: "read", Value: counter(maxValue)}, {Op: "read", Value: counter(one)}, {Op: "write", Value: counter(zero)}}
	read, write, readErr, writeErr = ctrBlockTotals(block)
	if read != nil || readErr == "" || write == nil || *write != 0 || writeErr != "" {
		t.Fatalf("block overflow status=%v,%v (%q,%q)", read, write, readErr, writeErr)
	}
}

func TestCtrCPUUsagePercentValidatesSamplesAndKeepsHighValues(t *testing.T) {
	ptr := func(value uint64) *uint64 { return &value }
	previous := ctrAPIStatsCPU{SystemCPUUsage: ptr(100), OnlineCPUs: ptr(2)}
	previous.CPUUsage.TotalUsage = ptr(10)
	current := ctrAPIStatsCPU{SystemCPUUsage: ptr(500), OnlineCPUs: ptr(2)}
	current.CPUUsage.TotalUsage = ptr(110)
	read, preread := "2026-10-23T11:00:02Z", "2026-10-23T11:00:01Z"
	percent, err := ctrCPUUsagePercent(current, previous, read, preread)
	if err != nil || percent != 50 {
		t.Fatalf("50%% CPU = %v, %v", percent, err)
	}
	current.CPUUsage.TotalUsage = ptr(10)
	percent, err = ctrCPUUsagePercent(current, previous, read, preread)
	if err != nil || percent != 0 {
		t.Fatalf("idle CPU = %v, %v", percent, err)
	}
	current.CPUUsage.TotalUsage = ptr(1110)
	percent, err = ctrCPUUsagePercent(current, previous, read, preread)
	if err != nil || percent != 550 {
		t.Fatalf("multi-core CPU = %v, %v", percent, err)
	}
	current.OnlineCPUs = nil
	current.CPUUsage.PerCPU = []uint64{0, 0, 0, 0}
	percent, err = ctrCPUUsagePercent(current, previous, read, preread)
	if err != nil || percent != 1100 {
		t.Fatalf("per-CPU fallback = %v, %v", percent, err)
	}
	current.OnlineCPUs = ptr(0)
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted zero CPU count")
	}
	current.OnlineCPUs = ptr(2)
	current.SystemCPUUsage = ptr(100)
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted zero system counter delta")
	}
	current.SystemCPUUsage = ptr(50)
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted a reset system counter")
	}
	current.SystemCPUUsage = ptr(500)
	if _, err = ctrCPUUsagePercent(current, previous, read, read); err == nil {
		t.Fatal("accepted a non-advancing read time")
	}
	current.CPUUsage.TotalUsage = nil
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted a missing current CPU counter")
	}
	current.CPUUsage.TotalUsage = ptr(110)
	current.OnlineCPUs = nil
	current.CPUUsage.PerCPU = nil
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted an unknown CPU count")
	}
	current.OnlineCPUs = ptr(2)
	current.CPUUsage.PerCPU = nil
	previous.CPUUsage.TotalUsage = nil
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted a missing baseline CPU counter")
	}
	previous.CPUUsage.TotalUsage = ptr(10)
	current.CPUUsage.TotalUsage = ptr(9)
	if _, err = ctrCPUUsagePercent(current, previous, read, preread); err == nil {
		t.Fatal("accepted a reset container CPU counter")
	}
	current.CPUUsage.TotalUsage = ptr(110)
	if _, err = ctrCPUUsagePercent(current, previous, "invalid", preread); err == nil {
		t.Fatal("accepted an invalid read time")
	}
}

func TestCtrSocketPartialInspectCapsConcurrencyAndKeepsRequestBudget(t *testing.T) {
	var current, maximum int
	var mu sync.Mutex
	var inspections int
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/json":
			fmt.Fprint(w, `[]`)
		default:
			mu.Lock()
			current++
			inspections++
			if current > maximum {
				maximum = current
			}
			mu.Unlock()
			defer func() { mu.Lock(); current--; mu.Unlock() }()
			time.Sleep(15 * time.Millisecond)
			if strings.Contains(r.URL.Path, "gone") {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"No such container"}`)
				return
			}
			if strings.Contains(r.URL.Path, "broken") {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"message":"inspect failed"}`)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.51/containers/"), "/json")
			fmt.Fprintf(w, `{"Id":%q,"Name":"/x","State":{"Status":"exited","StartedAt":"0001-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}}`, id)
		}
	}))
	ids := []string{"ok1", "ok2", "ok3", "ok4", "ok5", "ok6", "gone", "broken"}
	items, err := engine.inspectMany(context.Background(), ids)
	if err == nil || !strings.Contains(err.Error(), "inspect failed") || len(items) != 6 || inspections != 8 || maximum > 4 {
		t.Fatalf("items=%d err=%v requests=%d max=%d", len(items), err, inspections, maximum)
	}
}

func TestCtrSocketCancellationAndResponseLimit(t *testing.T) {
	started := make(chan struct{})
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := engine.get(ctx, "/wait", 32); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled request did not stop")
	}
	engine = ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		w.Write(make([]byte, 33))
	}))
	if _, err := engine.get(context.Background(), "/large", 32); err == nil || !strings.Contains(err.Error(), "exceeds 32 bytes") {
		t.Fatalf("oversize error=%v", err)
	}
}

func TestCtrSocketHostSourceWorksWithEmptyPath(t *testing.T) {
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		if r.URL.Path == "/v1.51/containers/json" {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Setenv("PATH", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix://"+engine.socket)
	sources, ok := hostCtrSources()
	if !ok {
		t.Fatal("Docker Engine API source unavailable")
	}
	items, err := collectCtrList(context.Background(), sources, false)
	if err != nil || len(items) != 0 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	sources.close()
}

func TestCtrSocketListRequestBudget(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
		case "/v1.51/containers/json":
			fmt.Fprint(w, `[{"Id":"running"},{"Id":"stopped"}]`)
		case "/v1.51/containers/running/json":
			fmt.Fprint(w, `{"Id":"running","Name":"/running","Image":"sha256:a","Config":{"Image":"a"},"State":{"Status":"running","Pid":10,"StartedAt":"2026-10-23T10:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"},"HostConfig":{"Memory":100,"PidsLimit":10}}`)
		case "/v1.51/containers/stopped/json":
			fmt.Fprint(w, `{"Id":"stopped","Name":"/stopped","Image":"sha256:b","Config":{"Image":"b"},"State":{"Status":"exited","Pid":0,"StartedAt":"0001-01-01T00:00:00Z","FinishedAt":"2026-10-23T10:00:00Z"},"HostConfig":{"Memory":0,"PidsLimit":0}}`)
		case "/v1.51/containers/running/stats":
			fmt.Fprint(w, `{"memory_stats":{"usage":20,"limit":100},"cpu_stats":{"throttling_data":{"periods":1,"throttled_periods":0,"throttled_time":0}},"pids_stats":{"current":2,"limit":10}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("PATH", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix://"+engine.socket)
	t.Setenv("DOCKER_API_VERSION", "")
	sources, _ := hostCtrSources()
	items, err := collectCtrList(context.Background(), sources, true)
	sources.close()
	if err != nil || len(items) != 2 || items[0].MemoryBytes == nil || *items[0].MemoryBytes != 20 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if len(paths) != 6 {
		t.Fatalf("requests=%v", paths)
	}
	for _, path := range paths {
		if strings.Contains(path, "logs") || strings.Contains(path, "/top") || strings.Contains(path, "size=true") {
			t.Fatalf("unexpected list request %q", path)
		}
	}
	if paths[0] != "GET /version" || paths[1] != "GET /v1.51/containers/json?all=1" {
		t.Fatalf("request order=%v", paths)
	}
}

func TestCtrSocketLogHTTPErrorNeverBecomesLogText(t *testing.T) {
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.WriteHeader(500)
			fmt.Fprint(w, "daemon log error")
			return
		}
		http.NotFound(w, r)
	}))
	text, err := engine.logs(context.Background(), "id", 50, false)
	if err == nil || text != "" || !strings.Contains(err.Error(), "daemon log error") {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestCtrSocketHTTPStatusUsesTypedCode(t *testing.T) {
	engine := ctrSocketFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.41"}`)
			return
		}
		w.WriteHeader(500)
		fmt.Fprint(w, "upstream text says HTTP 404 but this is an internal error")
	}))
	_, err := engine.image(context.Background(), "missing:tag")
	if err == nil || errors.Is(err, errCtrNoImage) || ctrHTTPStatus(err, http.StatusNotFound) {
		t.Fatalf("HTTP 500 was treated as missing image: %v", err)
	}
}

func TestCtrInspectRejectsMissingRequiredFieldsAndFormatsIPv6Ports(t *testing.T) {
	bad := ctrAPIInspect{}
	if _, err := ctrInspectValue(bad); err == nil {
		t.Fatal("accepted an inspect response without required fields")
	}
	var value ctrAPIInspect
	value.ID = "id"
	value.Name = "/web"
	value.State.Status = "running"
	value.State.StartedAt = "0001-01-01T00:00:00Z"
	value.State.FinishedAt = "0001-01-01T00:00:00Z"
	value.NetworkSettings.Ports = map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}{"8080/tcp": {{HostIP: "::", HostPort: "80"}}, "53/udp": {{HostIP: "", HostPort: "53"}}}
	got, err := ctrInspectValue(value)
	if err != nil || len(got.Ports) != 2 || got.Ports[0].Port != 53 || got.Ports[0].Hosts[0] != "*:53" || got.Ports[1].Hosts[0] != "[::]:80" {
		t.Fatalf("ports=%+v err=%v", got.Ports, err)
	}
}
