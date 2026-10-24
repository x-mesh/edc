//go:build linux || darwin

package edc

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ctrMinAPIVersion = "1.41"
	ctrMaxAPIVersion = "1.51"
	ctrJSONLimit     = 16 << 20
	ctrDialTimeout   = 2 * time.Second
	ctrRequestLimit  = 5 * time.Second
)

var errCtrUnavailable = errors.New("unavailable through the Docker Engine API")

type ctrHTTPError struct {
	StatusCode int
	Message    string
}

func (e *ctrHTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Docker Engine API returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("Docker Engine API returned HTTP %d", e.StatusCode)
}

func ctrHTTPStatus(err error, status int) bool {
	var httpErr *ctrHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

type ctrEngine struct {
	client       *http.Client
	base         string
	socket       string
	endpointErr  error
	mu           sync.Mutex
	version      string
	collectorSem chan struct{}
}

type ctrEngineVersion struct {
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
}

type ctrAPIContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

type ctrAPIInspect struct {
	ID           string   `json:"Id"`
	Name         string   `json:"Name"`
	Created      string   `json:"Created"`
	Path         string   `json:"Path"`
	Args         []string `json:"Args"`
	Image        string   `json:"Image"`
	RestartCount int      `json:"RestartCount"`
	State        struct {
		Status     string        `json:"Status"`
		Running    bool          `json:"Running"`
		Pid        int           `json:"Pid"`
		ExitCode   int           `json:"ExitCode"`
		StartedAt  string        `json:"StartedAt"`
		FinishedAt string        `json:"FinishedAt"`
		OOMKilled  bool          `json:"OOMKilled"`
		Health     *ctrAPIHealth `json:"Health"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
		Tty   bool   `json:"Tty"`
	} `json:"Config"`
	HostConfig struct {
		UsernsMode    string `json:"UsernsMode"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		Memory    *int64 `json:"Memory"`
		PidsLimit *int64 `json:"PidsLimit"`
		LogConfig struct {
			Type   string            `json:"Type"`
			Config map[string]string `json:"Config"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
	SizeRw  *uint64 `json:"SizeRw"`
	LogPath string  `json:"LogPath"`
}

type ctrAPIHealth struct {
	Status        string           `json:"Status"`
	FailingStreak int              `json:"FailingStreak"`
	Log           []ctrHealthCheck `json:"Log"`
}
type ctrAPIImage struct {
	ID      string `json:"Id"`
	Created string `json:"Created"`
}

type ctrAPITop struct {
	Titles    []string   `json:"Titles"`
	Processes [][]string `json:"Processes"`
}
type ctrAPIStatsCPUUsage struct {
	TotalUsage *uint64  `json:"total_usage"`
	PerCPU     []uint64 `json:"percpu_usage"`
}
type ctrAPIStatsCPU struct {
	CPUUsage       ctrAPIStatsCPUUsage `json:"cpu_usage"`
	SystemCPUUsage *uint64             `json:"system_cpu_usage"`
	OnlineCPUs     *uint64             `json:"online_cpus"`
	Throttling     struct {
		ThrottledNS *uint64 `json:"throttled_time"`
		Periods     *uint64 `json:"periods"`
		Throttled   *uint64 `json:"throttled_periods"`
	} `json:"throttling_data"`
}
type ctrAPIStats struct {
	Read    string `json:"read"`
	PreRead string `json:"preread"`
	Memory  struct {
		Usage *uint64 `json:"usage"`
		Limit *uint64 `json:"limit"`
	} `json:"memory_stats"`
	CPU    ctrAPIStatsCPU `json:"cpu_stats"`
	PreCPU ctrAPIStatsCPU `json:"precpu_stats"`
	Pids   struct {
		Current *uint64 `json:"current"`
		Limit   *uint64 `json:"limit"`
	} `json:"pids_stats"`
	Networks *map[string]ctrAPINetworkStats `json:"networks"`
	Blkio    *struct {
		ServiceBytes *[]ctrAPIBlockIO `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

type ctrAPINetworkStats struct {
	RXBytes *ctrAPIOptionalUint64 `json:"rx_bytes"`
	TXBytes *ctrAPIOptionalUint64 `json:"tx_bytes"`
}

type ctrAPIBlockIO struct {
	Op    string                `json:"op"`
	Value *ctrAPIOptionalUint64 `json:"value"`
}

type ctrAPIOptionalUint64 struct {
	Value *uint64
	Err   string
}

func (counter *ctrAPIOptionalUint64) UnmarshalJSON(data []byte) error {
	value, err := strconv.ParseUint(string(data), 10, 64)
	if err != nil {
		counter.Err = "counter is invalid"
		return nil
	}
	counter.Value = &value
	return nil
}

func hostCtrSources() (ctrSources, bool) {
	path, err := ctrSocketEndpoint()
	engine := newCtrEngine(path)
	engine.endpointErr = err
	return ctrSources{
		euid: os.Geteuid(), docker: func() bool { return true }, now: time.Now,
		inspect:       func(ctx context.Context, ref string) (ctrInspect, error) { return engine.inspect(ctx, ref, false) },
		inspectDetail: func(ctx context.Context, ref string) (ctrInspect, error) { return engine.inspect(ctx, ref, true) },
		listIDs:       func(ctx context.Context) ([]string, error) { return engine.list(ctx) },
		inspectMany:   func(ctx context.Context, ids []string) ([]ctrInspect, error) { return engine.inspectMany(ctx, ids) },
		image:         func(ctx context.Context, ref string) (ctrImageRef, error) { return engine.image(ctx, ref) },
		logs: func(ctx context.Context, id string, lines int, tty bool) (string, error) {
			return engine.logs(ctx, id, lines, tty)
		},
		close: engine.client.CloseIdleConnections,
		top:   func(ctx context.Context, id string) ([]topProcess, error) { return engine.top(ctx, id) },
		stats: func(ctx context.Context, id string, inspect ctrInspect, detailed bool) ctrLimits {
			return engine.stats(ctx, id, inspect, detailed)
		},
		listStats: func(ctx context.Context, inspects []ctrInspect) map[string]ctrListStats {
			return engine.listStats(ctx, inspects)
		},
		collector: func(ctx context.Context, inspect ctrInspect, image string) (ctrCollectorResponse, error) {
			return engine.collect(ctx, inspect, image)
		},
	}, true
}

func ctrSocketEndpoint() (string, error) {
	if contextName, ok := os.LookupEnv("DOCKER_CONTEXT"); ok && contextName != "" && contextName != "default" {
		return "", fmt.Errorf("DOCKER_CONTEXT=%q is unsupported; use DOCKER_HOST with a Unix socket", contextName)
	}
	if os.Getenv("DOCKER_CONTEXT") != "default" {
		if host := os.Getenv("DOCKER_HOST"); host != "" {
			u, err := url.Parse(host)
			if err != nil || u.Scheme != "unix" || !strings.HasPrefix(u.Path, "/") || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				return "", fmt.Errorf("DOCKER_HOST must be an absolute Unix socket address")
			}
			return filepath.Clean(u.Path), nil
		}
	}
	if os.Getenv("DOCKER_CONTEXT") == "default" {
		return ctrDefaultSocket(), nil
	}
	if runtime.GOOS == "darwin" && os.Getenv("HOME") != "" {
		path := filepath.Join(os.Getenv("HOME"), ".docker", "run", "docker.sock")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return ctrDefaultSocket(), nil
}

func ctrDefaultSocket() string {
	if runtime.GOOS == "darwin" && os.Getenv("HOME") != "" {
		path := filepath.Join(os.Getenv("HOME"), ".docker", "run", "docker.sock")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "/var/run/docker.sock"
}

func newCtrEngine(socket string) *ctrEngine {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: false, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, ctrDialTimeout)
		defer cancel()
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &ctrEngine{client: &http.Client{Transport: transport, Timeout: ctrRequestLimit, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: "http://docker", socket: socket, collectorSem: make(chan struct{}, 1)}
}

func (e *ctrEngine) request(ctx context.Context, method, path string, limit int64) ([]byte, int, error) {
	if e.endpointErr != nil {
		return nil, 0, e.endpointErr
	}
	ctx, cancel := context.WithTimeout(ctx, ctrRequestLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(data)) > limit {
		return nil, resp.StatusCode, fmt.Errorf("Docker response exceeds %d bytes", limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := infoSingleLine(firstLine(strings.TrimSpace(string(data))))
		return nil, resp.StatusCode, &ctrHTTPError{StatusCode: resp.StatusCode, Message: detail}
	}
	return data, resp.StatusCode, nil
}

func (e *ctrEngine) apiVersion(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.version != "" {
		return e.version, nil
	}
	data, _, err := e.request(ctx, "GET", "/version", ctrJSONLimit)
	if err != nil {
		return "", err
	}
	var v ctrEngineVersion
	if err = json.Unmarshal(data, &v); err != nil || v.APIVersion == "" || v.MinAPIVersion == "" {
		return "", fmt.Errorf("invalid Docker /version response")
	}
	if _, ok := parseCtrAPIVersion(v.APIVersion); !ok {
		return "", fmt.Errorf("invalid Docker API version %q", v.APIVersion)
	}
	if _, ok := parseCtrAPIVersion(v.MinAPIVersion); !ok {
		return "", fmt.Errorf("invalid Docker minimum API version %q", v.MinAPIVersion)
	}
	low := ctrMinAPIVersion
	if v.MinAPIVersion != "" && versionLess(low, v.MinAPIVersion) {
		low = v.MinAPIVersion
	}
	high := ctrMaxAPIVersion
	if versionLess(v.APIVersion, high) {
		high = v.APIVersion
	}
	if override := os.Getenv("DOCKER_API_VERSION"); override != "" {
		if _, ok := parseCtrAPIVersion(override); !ok || versionLess(override, low) || versionLess(high, override) {
			return "", fmt.Errorf("DOCKER_API_VERSION %q is outside daemon and client range %s-%s", override, low, high)
		}
		high = override
	}
	if versionLess(high, low) {
		return "", fmt.Errorf("Docker API range %s-%s does not intersect client range %s-%s", v.MinAPIVersion, v.APIVersion, ctrMinAPIVersion, ctrMaxAPIVersion)
	}
	e.version = high
	return high, nil
}

type ctrAPIVersion struct{ major, minor int }

func parseCtrAPIVersion(value string) (ctrAPIVersion, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return ctrAPIVersion{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 1 || minor < 0 || strconv.Itoa(major) != parts[0] || strconv.Itoa(minor) != parts[1] {
		return ctrAPIVersion{}, false
	}
	return ctrAPIVersion{major, minor}, true
}

func versionLess(a, b string) bool {
	av, aok := parseCtrAPIVersion(a)
	bv, bok := parseCtrAPIVersion(b)
	return aok && bok && (av.major < bv.major || av.major == bv.major && av.minor < bv.minor)
}
func (e *ctrEngine) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	v, err := e.apiVersion(ctx)
	if err != nil {
		return nil, err
	}
	returnBody, _, err := e.request(ctx, "GET", "/v"+v+path, limit)
	return returnBody, err
}
func ctrSegment(s string) string { return url.PathEscape(s) }

func (e *ctrEngine) list(ctx context.Context) ([]string, error) {
	data, err := e.get(ctx, "/containers/json?all=1", ctrJSONLimit)
	if err != nil {
		return nil, err
	}
	var rows []ctrAPIContainer
	if text := strings.TrimSpace(string(data)); text == "" || text[0] != '[' {
		return nil, fmt.Errorf("Docker container list is not an array")
	}
	if err = json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ID == "" {
			return nil, fmt.Errorf("Docker container list has an empty ID")
		}
		ids = append(ids, r.ID)
	}
	return ids, nil
}
func (e *ctrEngine) inspectMany(ctx context.Context, ids []string) ([]ctrInspect, error) {
	sem := make(chan struct{}, 4)
	type result struct {
		i    int
		item ctrInspect
		err  error
	}
	ch := make(chan result, len(ids))
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case sem <- struct{}{}:
			if ctx.Err() != nil {
				<-sem
				return nil, ctx.Err()
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		go func(i int, id string) {
			defer func() { <-sem }()
			item, err := e.inspect(ctx, id, false)
			ch <- result{i, item, err}
		}(i, id)
	}
	results := make([]ctrInspect, len(ids))
	var first error
	for range ids {
		r := <-ch
		if r.err != nil {
			if ctrHTTPStatus(r.err, http.StatusNotFound) {
				continue
			}
			if first == nil {
				first = r.err
			}
			continue
		}
		results[r.i] = r.item
	}
	items := make([]ctrInspect, 0, len(results))
	for _, item := range results {
		if item.ID != "" {
			items = append(items, item)
		}
	}
	return items, first
}
func (e *ctrEngine) inspect(ctx context.Context, id string, size bool) (ctrInspect, error) {
	path := "/containers/" + ctrSegment(id) + "/json"
	if size {
		path += "?size=true"
	}
	data, err := e.get(ctx, path, ctrJSONLimit)
	if err != nil {
		return ctrInspect{}, err
	}
	var v ctrAPIInspect
	if err = json.Unmarshal(data, &v); err != nil {
		return ctrInspect{}, err
	}
	return ctrInspectValue(v)
}
func ctrInspectValue(v ctrAPIInspect) (ctrInspect, error) {
	if v.ID == "" || strings.TrimSpace(v.Name) == "" || v.State.Status == "" {
		return ctrInspect{}, fmt.Errorf("Docker inspect response lacks ID, Name, or State.Status")
	}
	started, err := time.Parse(time.RFC3339Nano, v.State.StartedAt)
	if err != nil {
		return ctrInspect{}, fmt.Errorf("invalid StartedAt: %w", err)
	}
	finished, err := time.Parse(time.RFC3339Nano, v.State.FinishedAt)
	if err != nil {
		return ctrInspect{}, fmt.Errorf("invalid FinishedAt: %w", err)
	}
	health := ""
	if v.State.Health != nil {
		health = v.State.Health.Status
	}
	var ports []ctrPublishedPort
	for key, bindings := range v.NetworkSettings.Ports {
		if bindings == nil {
			continue
		}
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			return ctrInspect{}, fmt.Errorf("invalid published port %q", key)
		}
		port, err := strconv.Atoi(parts[0])
		if err != nil || port < 1 || port > 65535 || (parts[1] != "tcp" && parts[1] != "udp" && parts[1] != "sctp") {
			return ctrInspect{}, fmt.Errorf("invalid published port %q", key)
		}
		for _, binding := range bindings {
			hostPort, err := strconv.Atoi(binding.HostPort)
			if err != nil || hostPort < 1 || hostPort > 65535 {
				return ctrInspect{}, fmt.Errorf("invalid host port %q", binding.HostPort)
			}
			idx := -1
			for i := range ports {
				if ports[i].Proto == parts[1] && ports[i].Port == port {
					idx = i
					break
				}
			}
			if idx < 0 {
				ports = append(ports, ctrPublishedPort{Proto: parts[1], Port: port})
				idx = len(ports) - 1
			}
			host := binding.HostIP
			if host == "" {
				host = "*"
			}
			ports[idx].Hosts = append(ports[idx].Hosts, net.JoinHostPort(host, strconv.Itoa(hostPort)))
		}
	}
	slices.SortFunc(ports, func(a, b ctrPublishedPort) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Proto, b.Proto))
	})
	inspect := ctrInspect{PID: v.State.Pid, Name: strings.TrimPrefix(v.Name, "/"), Status: v.State.Status, Health: health, Image: v.Config.Image, ID: v.ID, StartedAt: started, FinishedAt: finished, RestartCount: v.RestartCount, OOMKilled: v.State.OOMKilled, ExitCode: v.State.ExitCode, Ports: ports, RestartPolicy: v.HostConfig.RestartPolicy.Name, RestartMax: v.HostConfig.RestartPolicy.MaximumRetryCount, ImageID: v.Image, TTY: v.Config.Tty, MemoryLimit: v.HostConfig.Memory, PidsLimit: v.HostConfig.PidsLimit, UsernsMode: v.HostConfig.UsernsMode}
	if v.State.Health != nil {
		inspect.HealthData = &ctrHealth{Status: v.State.Health.Status, FailingStreak: v.State.Health.FailingStreak, Log: v.State.Health.Log}
	}
	inspect.Disk = ctrDisk{Writable: v.SizeRw, LogDriver: v.HostConfig.LogConfig.Type, LogMaxSize: v.HostConfig.LogConfig.Config["max-size"], LogMaxFile: v.HostConfig.LogConfig.Config["max-file"], LogPath: v.LogPath}
	if inspect.Disk.LogPath == "" {
		inspect.Disk.LogSizeErr = errCtrUnavailable
	}
	return inspect, nil
}
func (e *ctrEngine) image(ctx context.Context, ref string) (ctrImageRef, error) {
	data, err := e.get(ctx, "/images/"+ctrSegment(ref)+"/json", ctrJSONLimit)
	if err != nil {
		if ctrHTTPStatus(err, http.StatusNotFound) {
			return ctrImageRef{}, errCtrNoImage
		}
		return ctrImageRef{}, err
	}
	var v ctrAPIImage
	if err = json.Unmarshal(data, &v); err != nil {
		return ctrImageRef{}, err
	}
	if v.ID == "" || v.Created == "" {
		return ctrImageRef{}, fmt.Errorf("Docker image response lacks ID or Created")
	}
	created, err := time.Parse(time.RFC3339Nano, v.Created)
	if err != nil {
		return ctrImageRef{}, fmt.Errorf("invalid image Created: %w", err)
	}
	return ctrImageRef{ID: v.ID, Created: created}, nil
}

func (e *ctrEngine) top(ctx context.Context, id string) ([]topProcess, error) {
	q := url.Values{"ps_args": {"-eo pid,pcpu,rss,args"}}
	data, err := e.get(ctx, "/containers/"+ctrSegment(id)+"/top?"+q.Encode(), ctrJSONLimit)
	if err != nil {
		return nil, err
	}
	var response ctrAPITop
	if err = json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	columns := make(map[string]int, len(response.Titles))
	for index, title := range response.Titles {
		columns[strings.ToUpper(title)] = index
	}
	for _, required := range []string{"PID", "%CPU", "RSS", "COMMAND"} {
		if _, ok := columns[required]; !ok {
			if required == "COMMAND" {
				if _, ok = columns["ARGS"]; ok {
					continue
				}
			}
			return nil, fmt.Errorf("Docker top response has no %s field", required)
		}
	}
	commandIndex, ok := columns["COMMAND"]
	if !ok {
		commandIndex = columns["ARGS"]
	}
	var processes []topProcess
	for _, row := range response.Processes {
		if len(row) != len(response.Titles) {
			return nil, fmt.Errorf("Docker top response has an invalid row")
		}
		pid, e1 := strconv.Atoi(row[columns["PID"]])
		cpu, e2 := strconv.ParseFloat(row[columns["%CPU"]], 64)
		rss, e3 := strconv.ParseUint(row[columns["RSS"]], 10, 64)
		if e1 != nil || pid <= 0 || e2 != nil || math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0 || e3 != nil || rss > math.MaxUint64/1024 {
			return nil, fmt.Errorf("Docker top response has an invalid process value")
		}
		processes = append(processes, topProcess{PID: pid, CPU: cpu, RSS: rss * 1024, Command: row[commandIndex]})
	}
	return processes, nil
}

func (e *ctrEngine) stats(ctx context.Context, id string, inspect ctrInspect, detailed bool) ctrLimits {
	unavailable := topLimitStatus{Status: "unavailable", Reason: errCtrUnavailable.Error()}
	limits := ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Memory: topCgroupMemory{topLimitStatus: unavailable}, Events: topCgroupEvents{topLimitStatus: unavailable}, CPU: ctrCPULimits{topLimitStatus: unavailable}, CPUUsage: topLimitStatus{Status: "unsupported", Reason: "detailed CPU samples were not requested"}, Pids: ctrPidsLimits{topLimitStatus: unavailable}}
	if detailed {
		limits.CPUUsage = topLimitStatus{Status: "unavailable", Reason: "CPU usage samples are missing"}
	}
	stats, err := e.statsSnapshot(ctx, id, !detailed)
	if err != nil {
		limits.topLimitStatus = topLimitStatus{Status: "error", Reason: err.Error()}
		limits.Memory.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: err.Error()}
		limits.CPU.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: err.Error()}
		limits.CPUUsage = topLimitStatus{Status: "error", Reason: err.Error()}
		limits.Pids.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: err.Error()}
		return limits
	}
	if stats.Memory.Usage != nil && (!detailed || stats.Memory.Limit != nil) {
		limits.Memory = topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: stats.Memory.Usage}
		if inspect.MemoryLimit != nil && *inspect.MemoryLimit > 0 {
			limit := uint64(*inspect.MemoryLimit)
			limits.Memory.Max = &limit
		} else if inspect.MemoryLimit != nil && *inspect.MemoryLimit == 0 {
			limits.Memory.Unlimited = true
		} else {
			reason := "HostConfig.Memory is missing"
			if inspect.MemoryLimit != nil {
				reason = "HostConfig.Memory is invalid"
			}
			limits.Memory.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: reason}
		}
	}
	if stats.CPU.Throttling.Periods != nil && stats.CPU.Throttling.Throttled != nil && stats.CPU.Throttling.ThrottledNS != nil {
		limits.CPU = ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: *stats.CPU.Throttling.Periods, Throttled: *stats.CPU.Throttling.Throttled}
		limits.CPU.ThrottledUS = *stats.CPU.Throttling.ThrottledNS / 1000
	} else {
		limits.CPU.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: "CPU throttling fields are missing"}
	}
	if detailed {
		percent, cpuErr := ctrCPUUsagePercent(stats.CPU, stats.PreCPU, stats.Read, stats.PreRead)
		if cpuErr != nil {
			limits.CPUUsage = topLimitStatus{Status: "unavailable", Reason: cpuErr.Error()}
		} else {
			limits.CPUUsage = topLimitStatus{Status: "available"}
			limits.CPUUsagePercent = &percent
		}
	}
	if stats.Pids.Current != nil {
		limits.Pids = ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: *stats.Pids.Current}
		if inspect.PidsLimit != nil && *inspect.PidsLimit > 0 {
			limits.Pids.Max = uint64(*inspect.PidsLimit)
			limits.PidsLimitKnown = true
		} else if inspect.PidsLimit != nil {
			limits.Pids.Unlimited = true
			limits.PidsLimitKnown = true
		}
	} else {
		limits.Pids.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: "pids current is missing"}
	}
	return limits
}

func (e *ctrEngine) statsSnapshot(ctx context.Context, id string, oneShot bool) (ctrAPIStats, error) {
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if !oneShot {
		q.Set("one-shot", "false")
	}
	data, err := e.get(ctx, "/containers/"+ctrSegment(id)+"/stats?"+q.Encode(), ctrJSONLimit)
	if err != nil {
		return ctrAPIStats{}, err
	}
	var stats ctrAPIStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return ctrAPIStats{}, err
	}
	return stats, nil
}

func (e *ctrEngine) listStats(ctx context.Context, inspects []ctrInspect) map[string]ctrListStats {
	return e.listStatsWithWait(ctx, inspects, time.Second)
}

func (e *ctrEngine) listStatsWithWait(ctx context.Context, inspects []ctrInspect, interval time.Duration) map[string]ctrListStats {
	out := make(map[string]ctrListStats, len(inspects))
	running := make([]ctrInspect, 0, len(inspects))
	for _, inspect := range inspects {
		if inspect.Status == "running" {
			running = append(running, inspect)
		}
	}
	if len(running) == 0 {
		return out
	}
	first, firstErr := e.statsWave(ctx, running)
	anyBaseline := false
	for _, sample := range first {
		if sample != nil {
			anyBaseline = true
			break
		}
	}
	if !anyBaseline {
		for _, inspect := range running {
			reason := firstErr[inspect.ID]
			if reason == "" && ctx.Err() != nil {
				reason = ctx.Err().Error()
			}
			if reason == "" {
				reason = "first CPU sample unavailable"
			}
			out[inspect.ID] = ctrUnavailableListStats(reason)
		}
		return out
	}
	if interval <= 0 {
		if ctx.Err() != nil {
			for _, inspect := range running {
				entry := ctrUnavailableListStats(ctx.Err().Error())
				if sample := first[inspect.ID]; sample != nil {
					entry = ctrListMetricsFromSnapshot(sample, inspect)
					entry.CPUReason = ctx.Err().Error()
				} else {
					entry = ctrUnavailableListStats(ctx.Err().Error())
				}
				out[inspect.ID] = entry
			}
			return out
		}
	} else {
		select {
		case <-ctx.Done():
			for _, inspect := range running {
				entry := ctrListStats{CPUReason: ctx.Err().Error()}
				if sample := first[inspect.ID]; sample != nil {
					entry = ctrListMetricsFromSnapshot(sample, inspect)
					entry.CPUReason = ctx.Err().Error()
				}
				out[inspect.ID] = entry
			}
			return out
		case <-time.After(interval):
		}
	}
	second, secondErr := e.statsWave(ctx, running)
	for _, inspect := range running {
		id := inspect.ID
		current, ok := second[id]
		previous, prevOK := first[id]
		currOK := ok && current != nil
		prevOK = prevOK && previous != nil
		entry := ctrListStats{}
		if currOK {
			entry = ctrListMetricsFromSnapshot(current, inspect)
		}
		if !currOK || !prevOK {
			entry.CPUReason = firstErr[id]
			if entry.CPUReason == "" {
				entry.CPUReason = secondErr[id]
			}
			if entry.CPUReason == "" && ctx.Err() != nil {
				entry.CPUReason = ctx.Err().Error()
			}
			if entry.CPUReason == "" {
				entry.CPUReason = "CPU sample unavailable"
			}
			if !currOK && !prevOK {
				entry = ctrUnavailableListStats(entry.CPUReason)
			}
		} else {
			percent, err := ctrCPUUsagePercent(current.CPU, previous.CPU, current.Read, previous.Read)
			if err == nil {
				entry.Limits.CPUUsagePercent = &percent
				entry.Limits.CPUUsage = topLimitStatus{Status: "available"}
			} else {
				entry.CPUReason = err.Error()
			}
		}
		if prevOK {
			prior := ctrListMetricsFromSnapshot(previous, inspect)
			if !currOK || entry.Limits.Memory.Used == nil {
				latest := entry.Limits.Memory.Reason
				if !currOK {
					latest = secondErr[id]
				}
				if prior.Limits.Memory.Used != nil {
					entry.Limits.Memory = prior.Limits.Memory
					entry.Limits.Memory.Reason = ctrPhaseTwoSampleReason("memory", latest, secondErr[id], ctx.Err()) + "; using phase-one value"
				} else {
					entry.Limits.Memory.Reason = ctrPhaseTwoSampleReason("memory", latest, secondErr[id], ctx.Err())
				}
			}
			if !currOK || entry.Limits.Pids.Status != "available" {
				latest := entry.Limits.Pids.Reason
				if !currOK {
					latest = secondErr[id]
				}
				if prior.Limits.Pids.Status == "available" {
					entry.Limits.Pids = prior.Limits.Pids
					entry.Limits.Pids.Reason = ctrPhaseTwoSampleReason("pids", latest, secondErr[id], ctx.Err()) + "; using phase-one value"
				} else {
					entry.Limits.Pids.Reason = ctrPhaseTwoSampleReason("pids", latest, secondErr[id], ctx.Err())
				}
				entry.Limits.PidsLimitKnown = prior.Limits.PidsLimitKnown
			}
			if entry.NetworkRX == nil {
				latest := ctrPhaseTwoSampleReason("network rx", entry.NetworkRXReason, secondErr[id], ctx.Err())
				entry.NetworkRX = prior.NetworkRX
				if entry.NetworkRX != nil {
					entry.NetworkRXReason = latest + "; using phase-one value"
				} else {
					entry.NetworkRXReason = latest
				}
			}
			if entry.NetworkTX == nil {
				latest := ctrPhaseTwoSampleReason("network tx", entry.NetworkTXReason, secondErr[id], ctx.Err())
				entry.NetworkTX = prior.NetworkTX
				if entry.NetworkTX != nil {
					entry.NetworkTXReason = latest + "; using phase-one value"
				} else {
					entry.NetworkTXReason = latest
				}
			}
			if entry.BlockRead == nil {
				latest := ctrPhaseTwoSampleReason("block read", entry.BlockReadReason, secondErr[id], ctx.Err())
				entry.BlockRead = prior.BlockRead
				if entry.BlockRead != nil {
					entry.BlockReadReason = latest + "; using phase-one value"
				} else {
					entry.BlockReadReason = latest
				}
			}
			if entry.BlockWrite == nil {
				latest := ctrPhaseTwoSampleReason("block write", entry.BlockWriteReason, secondErr[id], ctx.Err())
				entry.BlockWrite = prior.BlockWrite
				if entry.BlockWrite != nil {
					entry.BlockWriteReason = latest + "; using phase-one value"
				} else {
					entry.BlockWriteReason = latest
				}
			}
			if !currOK {
				entry.CPUReason = secondErr[id]
				if entry.CPUReason == "" && ctx.Err() != nil {
					entry.CPUReason = ctx.Err().Error()
				}
				if entry.CPUReason == "" {
					entry.CPUReason = "second CPU sample unavailable"
				}
				if entry.Limits.Memory.Used == nil {
					entry.Limits.Memory.Reason = secondErr[id]
				}
				if entry.Limits.Pids.Status != "available" {
					entry.Limits.Pids.Reason = secondErr[id]
				}
			}
		}
		out[id] = entry
	}
	return out
}

func ctrPhaseTwoSampleReason(metric, latest, requestErr string, ctxErr error) string {
	cause := latest
	if cause == "" {
		cause = requestErr
	}
	if cause == "" && ctxErr != nil {
		cause = ctxErr.Error()
	}
	if cause == "" {
		cause = "value is missing"
	}
	return "phase-two sample unavailable for " + metric + ": " + cause
}

func ctrUnavailableListStats(reason string) ctrListStats {
	return ctrListStats{
		Limits:    ctrLimits{Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: reason}}, Pids: ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: reason}}},
		CPUReason: reason, NetworkRXReason: reason, NetworkTXReason: reason, BlockReadReason: reason, BlockWriteReason: reason,
	}
}

func ctrListMetricsFromSnapshot(stats *ctrAPIStats, inspect ctrInspect) ctrListStats {
	entry := ctrListStats{Limits: ctrLimitsFromSnapshot(stats, inspect)}
	entry.NetworkRX, entry.NetworkTX, entry.NetworkRXReason, entry.NetworkTXReason = ctrNetworkTotals(stats.Networks)
	entry.BlockRead, entry.BlockWrite, entry.BlockReadReason, entry.BlockWriteReason = ctrBlockTotals(stats.Blkio)
	return entry
}

func (e *ctrEngine) statsWave(ctx context.Context, inspects []ctrInspect) (map[string]*ctrAPIStats, map[string]string) {
	type result struct {
		id    string
		stats *ctrAPIStats
		err   string
	}
	jobs := make(chan ctrInspect)
	results := make(chan result, len(inspects))
	workers := min(4, len(inspects))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for inspect := range jobs {
				if ctx.Err() != nil {
					continue
				}
				stats, err := e.statsSnapshot(ctx, inspect.ID, true)
				if err == nil {
					results <- result{id: inspect.ID, stats: &stats}
				} else {
					results <- result{id: inspect.ID, err: err.Error()}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, inspect := range inspects {
			if ctx.Err() != nil {
				return
			}
			select {
			case jobs <- inspect:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	out := make(map[string]*ctrAPIStats, len(inspects))
	errs := make(map[string]string)
	for result := range results {
		out[result.id] = result.stats
		if result.stats == nil {
			errs[result.id] = result.err
		}
	}
	return out, errs
}

func ctrLimitsFromSnapshot(stats *ctrAPIStats, inspect ctrInspect) ctrLimits {
	limits := ctrLimits{Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: "memory usage is missing"}}, Pids: ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: "pids current is missing"}}}
	if stats == nil {
		return limits
	}
	if stats.Memory.Usage != nil {
		limits.Memory = topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: stats.Memory.Usage}
		if inspect.MemoryLimit != nil && *inspect.MemoryLimit > 0 {
			v := uint64(*inspect.MemoryLimit)
			limits.Memory.Max = &v
		} else if inspect.MemoryLimit != nil && *inspect.MemoryLimit == 0 {
			limits.Memory.Unlimited = true
		} else {
			reason := "HostConfig.Memory is missing"
			if inspect.MemoryLimit != nil {
				reason = "HostConfig.Memory is invalid"
			}
			limits.Memory.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: reason}
		}
	}
	if stats.Pids.Current != nil {
		limits.Pids = ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: *stats.Pids.Current}
		if inspect.PidsLimit != nil && *inspect.PidsLimit > 0 {
			limits.Pids.Max = uint64(*inspect.PidsLimit)
			limits.PidsLimitKnown = true
		} else if inspect.PidsLimit != nil && *inspect.PidsLimit == 0 {
			limits.Pids.Unlimited = true
			limits.PidsLimitKnown = true
		} else if inspect.PidsLimit != nil {
			limits.Pids.Reason = "HostConfig.PidsLimit is invalid"
		}
	}
	return limits
}

func ctrNetworkTotals(networks *map[string]ctrAPINetworkStats) (*uint64, *uint64, string, string) {
	if networks == nil {
		return nil, nil, "network counters are missing", "network counters are missing"
	}
	var rx, tx uint64
	rxReason, txReason := "", ""
	for _, counters := range *networks {
		if counters.RXBytes == nil || counters.RXBytes.Value == nil {
			rxReason = "rx_bytes is missing"
			if counters.RXBytes != nil && counters.RXBytes.Err != "" {
				rxReason = "rx_bytes is invalid"
			}
		} else if ^uint64(0)-rx < *counters.RXBytes.Value {
			rxReason = "rx_bytes sum overflow"
		} else if rxReason == "" {
			rx += *counters.RXBytes.Value
		}
		if counters.TXBytes == nil || counters.TXBytes.Value == nil {
			txReason = "tx_bytes is missing"
			if counters.TXBytes != nil && counters.TXBytes.Err != "" {
				txReason = "tx_bytes is invalid"
			}
		} else if ^uint64(0)-tx < *counters.TXBytes.Value {
			txReason = "tx_bytes sum overflow"
		} else if txReason == "" {
			tx += *counters.TXBytes.Value
		}
	}
	var rxPtr, txPtr *uint64
	if rxReason == "" {
		rxPtr = &rx
	}
	if txReason == "" {
		txPtr = &tx
	}
	return rxPtr, txPtr, rxReason, txReason
}

func ctrBlockTotals(blkio *struct {
	ServiceBytes *[]ctrAPIBlockIO `json:"io_service_bytes_recursive"`
}) (*uint64, *uint64, string, string) {
	if blkio == nil || blkio.ServiceBytes == nil {
		return nil, nil, "block io counters are missing", "block io counters are missing"
	}
	var read, write uint64
	readReason, writeReason := "", ""
	for _, row := range *blkio.ServiceBytes {
		var target *uint64
		switch strings.ToLower(row.Op) {
		case "read":
			target = &read
		case "write":
			target = &write
		default:
			continue
		}
		if row.Value == nil || row.Value.Value == nil {
			if target == &read {
				readReason = "read value is missing"
				if row.Value != nil && row.Value.Err != "" {
					readReason = "read value is invalid"
				}
			} else {
				writeReason = "write value is missing"
				if row.Value != nil && row.Value.Err != "" {
					writeReason = "write value is invalid"
				}
			}
			continue
		}
		if ^uint64(0)-*target < *row.Value.Value {
			if target == &read {
				readReason = "read sum overflow"
			} else {
				writeReason = "write sum overflow"
			}
			continue
		}
		if target == &read && readReason == "" || target == &write && writeReason == "" {
			*target += *row.Value.Value
		}
	}
	var readPtr, writePtr *uint64
	if readReason == "" {
		readPtr = &read
	}
	if writeReason == "" {
		writePtr = &write
	}
	return readPtr, writePtr, readReason, writeReason
}

func ctrCPUUsagePercent(current, previous ctrAPIStatsCPU, read, preread string) (float64, error) {
	readTime, err := time.Parse(time.RFC3339Nano, read)
	if err != nil {
		return 0, fmt.Errorf("CPU stats read time is missing or invalid")
	}
	previousTime, err := time.Parse(time.RFC3339Nano, preread)
	if err != nil || !readTime.After(previousTime) {
		return 0, fmt.Errorf("CPU stats read time did not advance")
	}
	if current.CPUUsage.TotalUsage == nil || previous.CPUUsage.TotalUsage == nil || current.SystemCPUUsage == nil || previous.SystemCPUUsage == nil {
		return 0, fmt.Errorf("CPU usage counters are missing")
	}
	if *current.CPUUsage.TotalUsage < *previous.CPUUsage.TotalUsage || *current.SystemCPUUsage < *previous.SystemCPUUsage {
		return 0, fmt.Errorf("CPU usage counters reset")
	}
	cpuDelta := *current.CPUUsage.TotalUsage - *previous.CPUUsage.TotalUsage
	systemDelta := *current.SystemCPUUsage - *previous.SystemCPUUsage
	if systemDelta == 0 {
		return 0, fmt.Errorf("host CPU counter did not advance")
	}
	cpuCount := uint64(0)
	if current.OnlineCPUs != nil {
		cpuCount = *current.OnlineCPUs
	} else {
		cpuCount = uint64(len(current.CPUUsage.PerCPU))
	}
	if cpuCount == 0 {
		return 0, fmt.Errorf("CPU count is missing or zero")
	}
	percent := (float64(cpuDelta) / float64(systemDelta)) * float64(cpuCount) * 100
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, fmt.Errorf("CPU usage percentage is invalid")
	}
	return percent, nil
}

func (e *ctrEngine) logs(ctx context.Context, id string, lines int, tty bool) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(lines)}, "timestamps": {"1"}, "follow": {"0"}}
	data, err := e.get(ctx, "/containers/"+ctrSegment(id)+"/logs?"+q.Encode(), probeOutputLimit)
	if err != nil {
		return "", err
	}
	if tty {
		return string(data), nil
	}
	return ctrDecodeLogFrames(data)
}

func ctrDecodeLogFrames(data []byte) (string, error) {
	var output strings.Builder
	for len(data) > 0 {
		if len(data) < 8 {
			return "", fmt.Errorf("truncated Docker log frame header")
		}
		if (data[0] != 1 && data[0] != 2) || data[1] != 0 || data[2] != 0 || data[3] != 0 {
			return "", fmt.Errorf("invalid Docker log frame header")
		}
		size := binary.BigEndian.Uint32(data[4:8])
		data = data[8:]
		if uint64(size) > uint64(len(data)) {
			return "", fmt.Errorf("truncated Docker log frame payload")
		}
		output.Write(data[:size])
		data = data[size:]
	}
	return output.String(), nil
}
