package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Docker 엔진 수치는 엔진이 API로 답한 값이다. 호스트 경로를 걸어 얻은 값이 아니므로 파일 시스템 합계에 더하지 않는다.
// 엔진에는 GET만 보낸다.

const (
	cleanNotRunning  = "not_running"
	cleanUnreachable = "unreachable"
	cleanUnsupported = "unsupported"
	cleanFailed      = "failed"
	cleanNotReported = "not_reported"
)

const (
	cleanReasonEndpointScheme    = "endpoint_scheme"
	cleanReasonSocketMissing     = "socket_missing"
	cleanReasonConnectionRefused = "connection_refused"
	cleanReasonEngineTimeout     = "engine_timeout"
	cleanReasonAPIVersion        = "api_version"
	cleanReasonHTTPStatus        = "http_status"
	cleanReasonRequestFailed     = "request_failed"
)

const (
	cleanDockerImages     = "images"
	cleanDockerContainers = "containers"
	cleanDockerBuildCache = "build_cache"
	cleanDockerVolumes    = "volumes"
	cleanOrbStack         = "orbstack"

	// Usage 요약 객체는 이 API 버전부터 응답에 있다. 더 낮은 버전은 배열에서 계산하지 않고 지원하지 않는다고 보인다.
	cleanDockerAPIMajor = 1
	cleanDockerAPIMinor = 52
	cleanDockerAPIPath  = "/v1.52"
	cleanDockerBodyMax  = 16 << 20
)

var cleanDockerTypes = []string{cleanDockerImages, cleanDockerContainers, cleanDockerBuildCache, cleanDockerVolumes}

// 요청 하나의 제한이다. 시험이 줄여 쓴다. 볼륨 집계는 크기에 따라 이 Mac에서도 1~7초 걸려 따로 길게 둔다.
var (
	cleanDockerPingTimeout   = 2 * time.Second
	cleanDockerInfoTimeout   = 5 * time.Second
	cleanDockerDFTimeout     = 5 * time.Second
	cleanDockerVolumeTimeout = 20 * time.Second
)

var errCleanDockerTooLarge = errors.New("response body is too large")

type cleanDockerTypeResult struct {
	Type             string   `json:"type"`
	Status           string   `json:"status"`
	Reasons          []string `json:"reasons,omitempty"`
	TotalBytes       *int64   `json:"total_bytes,omitempty"`
	ReclaimableBytes *int64   `json:"reclaimable_bytes,omitempty"`
	TotalCount       *int64   `json:"total_count,omitempty"`
	ActiveCount      *int64   `json:"active_count,omitempty"`
	UserData         bool     `json:"user_data,omitempty"`
	Cleanup          string   `json:"cleanup,omitempty"`
}

// cleanDocker는 엔진 집계 섹션이다. total_bytes는 measured인 유형에만 있고, 측정하지 못한 것은 상태와 이유로 남긴다.
type cleanDocker struct {
	Status          string                  `json:"status"`
	Reasons         []string                `json:"reasons,omitempty"`
	Detail          string                  `json:"detail,omitempty"`
	Endpoint        string                  `json:"endpoint,omitempty"`
	APIVersion      string                  `json:"api_version,omitempty"`
	ServerVersion   string                  `json:"server_version,omitempty"`
	OperatingSystem string                  `json:"operating_system,omitempty"`
	RootDir         string                  `json:"root_dir,omitempty"`
	Inside          string                  `json:"inside,omitempty"`
	MeasuredAt      *time.Time              `json:"measured_at,omitempty"`
	ElapsedMS       *int64                  `json:"elapsed_ms,omitempty"`
	Types           []cleanDockerTypeResult `json:"types,omitempty"`
	// live는 지금 도는 엔진의 볼륨과 컨테이너 이름이다. 옛 데이터 디렉터리의 항목이 엔진에 같은 이름으로 있는지 비교할 때만 쓴다.
	live cleanDockerLive
}

// cleanDockerLive의 known이 false이면 엔진에 물어보지 못한 것이다. 이름이 없다는 뜻이 아니다.
type cleanDockerLive struct {
	known      bool
	volumes    map[string]bool
	containers map[string]string
	// imagesKnown이 false이면 이미지 목록만 읽지 못한 것이다. 볼륨과 컨테이너 이름은 known이 따로 말한다.
	imagesKnown bool
	images      map[string]bool
}

type cleanDockerUsage struct {
	TotalSize   *int64
	Reclaimable *int64
	TotalCount  *int64
	ActiveCount *int64
}

type cleanDockerDF struct {
	ImageUsage      *cleanDockerUsage
	ContainerUsage  *cleanDockerUsage
	VolumeUsage     *cleanDockerUsage
	BuildCacheUsage *cleanDockerUsage
}

func (section *cleanDocker) fail(status, reason, detail string) {
	section.Status, section.Detail = status, detail
	if reason != "" {
		section.Reasons = []string{reason}
	}
}

// cleanDockerScan은 docker CLI에는 끝점만 묻고, 수치는 그 끝점의 unix socket에서 GET으로 읽는다.
func cleanDockerScan(ctx context.Context, deps cleanDeps) cleanDocker {
	var section cleanDocker
	if err := ctx.Err(); err != nil {
		section.fail(cleanNotScanned, cleanContextReason(err), "")
		return section
	}
	if _, err := deps.lookPath("docker"); err != nil {
		section.fail(cleanToolAbsent, "", "")
		return section
	}
	socket, ok := cleanDockerLocate(ctx, deps, &section)
	if !ok {
		return section
	}
	started := time.Now()
	cleanDockerQuery(ctx, deps, socket, &section)
	at, elapsed := started.UTC(), time.Since(started).Milliseconds()
	section.MeasuredAt, section.ElapsedMS = &at, &elapsed
	return section
}

func cleanDockerQuery(ctx context.Context, deps cleanDeps, socket string, section *cleanDocker) {
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return deps.dockerDial(ctx, socket) },
		DisableKeepAlives: true,
	}}
	defer client.CloseIdleConnections()
	if !cleanDockerHandshake(ctx, client, section) {
		return
	}
	first := cleanDockerRequestTypes(ctx, client, cleanDockerDFTimeout, "/system/df?type=image&type=container&type=build-cache",
		[]string{cleanDockerImages, cleanDockerContainers, cleanDockerBuildCache})
	volumes := cleanDockerRequestTypes(ctx, client, cleanDockerVolumeTimeout, "/system/df?type=volume", []string{cleanDockerVolumes})
	section.Types = append(first, volumes...)
	cleanDockerFinish(section)
	section.live = cleanDockerLiveNames(ctx, client, cleanDockerAPIPath)
}

// cleanDockerEngineRoot는 엔진이 쓰는 데이터 디렉터리를 /info에서 읽는다. 읽지 못하면 빈 문자열이다.
func cleanDockerEngineRoot(ctx context.Context, client *http.Client, prefix string) string {
	info, err := cleanDockerGet(ctx, client, cleanDockerInfoTimeout, prefix+"/info")
	if err != nil {
		return ""
	}
	var engine struct{ DockerRootDir string }
	if json.Unmarshal(info.body, &engine) != nil {
		return ""
	}
	return engine.DockerRootDir
}

// cleanDockerLiveNames는 엔진의 볼륨과 컨테이너 이름을 읽는다. 둘 중 하나라도 읽지 못하면 known이 false다.
func cleanDockerLiveNames(ctx context.Context, client *http.Client, prefix string) cleanDockerLive {
	volumes, err := cleanDockerGet(ctx, client, cleanDockerDFTimeout, prefix+"/volumes")
	if err != nil {
		return cleanDockerLive{}
	}
	var volumeList struct{ Volumes []struct{ Name string } }
	if json.Unmarshal(volumes.body, &volumeList) != nil {
		return cleanDockerLive{}
	}
	containers, err := cleanDockerGet(ctx, client, cleanDockerDFTimeout, prefix+"/containers/json?all=1")
	if err != nil {
		return cleanDockerLive{}
	}
	var containerList []struct {
		Names  []string
		Labels map[string]string
	}
	if json.Unmarshal(containers.body, &containerList) != nil {
		return cleanDockerLive{}
	}
	live := cleanDockerLive{known: true, volumes: map[string]bool{}, containers: map[string]string{}}
	if images, err := cleanDockerGet(ctx, client, cleanDockerDFTimeout, prefix+"/images/json"); err == nil {
		var imageList []struct{ RepoTags []string }
		if json.Unmarshal(images.body, &imageList) == nil {
			live.imagesKnown, live.images = true, map[string]bool{}
			for _, image := range imageList {
				for _, tag := range image.RepoTags {
					live.images[tag] = true
				}
			}
		}
	}
	for _, volume := range volumeList.Volumes {
		live.volumes[volume.Name] = true
	}
	for _, container := range containerList {
		for _, name := range container.Names {
			live.containers[strings.TrimPrefix(name, "/")] = container.Labels["com.docker.compose.project"]
		}
	}
	return live
}

// cleanDockerLocate는 docker context에서 socket 경로를 읽는다. 기본 경로를 추측하지 않고, unix socket이 아니면 연결하지 않는다.
func cleanDockerLocate(ctx context.Context, deps cleanDeps, section *cleanDocker) (string, bool) {
	output := deps.run(ctx, cleanToolSpec{Dir: deps.home, Name: "docker", Args: []string{"context", "inspect", "--format", "{{json .Endpoints.docker}}"}})
	if output.Err != nil {
		switch {
		case ctx.Err() != nil:
			section.fail(cleanNotScanned, cleanContextReason(ctx.Err()), "")
		case errors.Is(output.Err, errCleanToolTimeout):
			section.fail(cleanUnknownLocation, cleanReasonToolTimeout, "")
		default:
			detail := fmt.Sprintf("exit %d", output.ExitCode)
			if line := cleanFirstLine(output.Stderr); line != "" {
				detail += ": " + line
			}
			if output.ExitCode == 0 {
				detail = output.Err.Error()
			}
			section.fail(cleanUnknownLocation, cleanReasonToolFailed, detail)
		}
		return "", false
	}
	var endpoint struct{ Host string }
	if output.Truncated || json.Unmarshal([]byte(output.Stdout), &endpoint) != nil || endpoint.Host == "" {
		section.fail(cleanUnknownLocation, cleanReasonInvalidOutput, cleanFirstLine(output.Stdout))
		return "", false
	}
	section.Endpoint = endpoint.Host
	path, isUnix := strings.CutPrefix(endpoint.Host, "unix://")
	if !isUnix {
		// 끝점 전체에는 주소나 인증 정보가 있을 수 있어 scheme만 남긴다.
		section.Endpoint = ""
		scheme, _, found := strings.Cut(endpoint.Host, "://")
		if !found {
			section.fail(cleanUnknownLocation, cleanReasonInvalidOutput, "")
			return "", false
		}
		section.fail(cleanUnsupported, cleanReasonEndpointScheme, scheme)
		return "", false
	}
	if path == "" {
		section.fail(cleanUnknownLocation, cleanReasonInvalidOutput, cleanFirstLine(output.Stdout))
		return "", false
	}
	if _, err := os.Stat(path); err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			section.fail(cleanNotRunning, cleanReasonSocketMissing, "")
		case errors.Is(err, fs.ErrPermission):
			section.fail(cleanUnreachable, cleanReasonPermission, "")
		default:
			section.fail(cleanUnreachable, cleanReasonStatError, err.Error())
		}
		return "", false
	}
	return path, true
}

// cleanDockerHandshake는 /_ping으로 API 버전을 확인하고 /info로 엔진을 알아본다. 계속할 수 없으면 false다.
func cleanDockerHandshake(ctx context.Context, client *http.Client, section *cleanDocker) bool {
	ping, err := cleanDockerGet(ctx, client, cleanDockerPingTimeout, "/_ping")
	if err != nil {
		// 전체 제한이 끝났거나 사용자가 멈춘 것은 엔진이 답하지 않은 것과 다르다.
		if ctx.Err() != nil {
			section.fail(cleanNotScanned, cleanContextReason(ctx.Err()), "")
			return false
		}
		reason, detail := cleanDockerRequestFailure(ctx, err)
		switch reason {
		case cleanReasonTimeout:
			section.fail(cleanUnreachable, cleanReasonEngineTimeout, "")
		case cleanReasonConnectionRefused, cleanReasonPermission:
			section.fail(cleanUnreachable, reason, "")
		default:
			section.fail(cleanFailed, reason, detail)
		}
		return false
	}
	version := ping.header.Get("Api-Version")
	supported, valid := cleanDockerVersionSupported(version)
	switch {
	case !valid:
		section.fail(cleanFailed, cleanReasonInvalidOutput, "Api-Version: "+cleanFirstLine(version))
		return false
	case !supported:
		section.APIVersion = version
		section.fail(cleanUnsupported, cleanReasonAPIVersion, version)
		// 집계는 1.52 이상만 지원하지만 데이터 디렉터리와 이름 목록은 엔진의 자기 버전으로 읽을 수 있다.
		// 옛 데이터 디렉터리 섹션이 이 값을 쓴다. 실패해도 상태는 그대로 unsupported다.
		prefix := "/v" + version
		section.RootDir = cleanDockerEngineRoot(ctx, client, prefix)
		section.live = cleanDockerLiveNames(ctx, client, prefix)
		return false
	}
	section.APIVersion = version
	info, err := cleanDockerGet(ctx, client, cleanDockerInfoTimeout, cleanDockerAPIPath+"/info")
	var engine struct{ OperatingSystem, ServerVersion, DockerRootDir string }
	if err == nil && json.Unmarshal(info.body, &engine) != nil {
		err = errCleanDockerInvalid
	}
	if err != nil {
		if ctx.Err() != nil {
			section.fail(cleanNotScanned, cleanContextReason(ctx.Err()), "")
			return false
		}
		reason, detail := cleanDockerRequestFailure(ctx, err)
		section.fail(cleanFailed, reason, detail)
		return false
	}
	section.OperatingSystem, section.ServerVersion, section.RootDir = engine.OperatingSystem, engine.ServerVersion, engine.DockerRootDir
	if strings.EqualFold(strings.TrimSpace(engine.OperatingSystem), "OrbStack") {
		section.Inside = cleanOrbStack
	}
	return true
}

var errCleanDockerInvalid = errors.New("response body is not the expected JSON")

// cleanDockerVersionSupported는 "1.56" 꼴의 Api-Version을 읽는다. 형식이 맞지 않으면 valid가 false다.
func cleanDockerVersionSupported(version string) (supported, valid bool) {
	majorText, minorText, found := strings.Cut(version, ".")
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(minorText)
	if !found || majorErr != nil || minorErr != nil || major < 0 || minor < 0 {
		return false, false
	}
	return major > cleanDockerAPIMajor || (major == cleanDockerAPIMajor && minor >= cleanDockerAPIMinor), true
}

type cleanDockerResponse struct {
	header http.Header
	body   []byte
}

type cleanDockerStatusError struct{ status int }

func (err cleanDockerStatusError) Error() string { return "HTTP " + strconv.Itoa(err.status) }

// cleanDockerGet은 GET 하나를 보낸다. 200이 아니면 오류이고, 본문은 상한까지만 읽고 넘으면 오류다.
func cleanDockerGet(ctx context.Context, client *http.Client, timeout time.Duration, path string) (cleanDockerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return cleanDockerResponse{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return cleanDockerResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return cleanDockerResponse{}, cleanDockerStatusError{response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, cleanDockerBodyMax+1))
	if err != nil {
		return cleanDockerResponse{}, err
	}
	if len(body) > cleanDockerBodyMax {
		return cleanDockerResponse{}, errCleanDockerTooLarge
	}
	return cleanDockerResponse{header: response.Header, body: body}, nil
}

// cleanDockerRequestFailure는 요청 오류를 이유 하나로 바꾼다. 사용자가 멈췄거나 전체 제한이 끝났으면 그 이유가 우선한다.
func cleanDockerRequestFailure(ctx context.Context, err error) (reason, detail string) {
	var statusErr cleanDockerStatusError
	switch {
	case ctx.Err() != nil:
		return cleanContextReason(ctx.Err()), ""
	case errors.As(err, &statusErr):
		return cleanReasonHTTPStatus, statusErr.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return cleanReasonTimeout, ""
	case errors.Is(err, errCleanDockerTooLarge), errors.Is(err, errCleanDockerInvalid):
		return cleanReasonInvalidOutput, err.Error()
	case errors.Is(err, syscall.ECONNREFUSED):
		return cleanReasonConnectionRefused, ""
	case errors.Is(err, fs.ErrPermission):
		return cleanReasonPermission, ""
	}
	return cleanReasonRequestFailed, cleanFirstLine(err.Error())
}

// cleanDockerRequestTypes는 df 요청 하나로 유형 여러 개를 읽는다. 요청이 실패하면 그 유형들만 실패로 남는다.
func cleanDockerRequestTypes(ctx context.Context, client *http.Client, timeout time.Duration, query string, kinds []string) []cleanDockerTypeResult {
	response, err := cleanDockerGet(ctx, client, timeout, cleanDockerAPIPath+query)
	var df cleanDockerDF
	if err == nil && json.Unmarshal(response.body, &df) != nil {
		err = errCleanDockerInvalid
	}
	usages := map[string]*cleanDockerUsage{cleanDockerImages: df.ImageUsage, cleanDockerContainers: df.ContainerUsage,
		cleanDockerBuildCache: df.BuildCacheUsage, cleanDockerVolumes: df.VolumeUsage}
	results := make([]cleanDockerTypeResult, 0, len(kinds))
	for _, kind := range kinds {
		if err == nil {
			results = append(results, cleanDockerTypeFrom(kind, usages[kind]))
			continue
		}
		reason, _ := cleanDockerRequestFailure(ctx, err)
		results = append(results, cleanDockerTypeResult{Type: kind, Status: cleanFailed, Reasons: []string{reason}, UserData: kind == cleanDockerVolumes})
	}
	return results
}

// cleanDockerTypeFrom은 Usage 객체 하나를 읽는다. TotalCount가 있을 때만 빠진 키를 0으로 본다. 엔진이 0인 값은 키를 내지 않는다.
func cleanDockerTypeFrom(kind string, usage *cleanDockerUsage) cleanDockerTypeResult {
	result := cleanDockerTypeResult{Type: kind, UserData: kind == cleanDockerVolumes}
	if usage == nil || (usage.TotalSize == nil && usage.TotalCount == nil) {
		result.Status = cleanNotReported
		return result
	}
	invalid := usage.TotalCount == nil
	for _, value := range []*int64{usage.TotalSize, usage.Reclaimable, usage.TotalCount, usage.ActiveCount} {
		invalid = invalid || (value != nil && *value < 0)
	}
	if invalid {
		result.Status, result.Reasons = cleanFailed, []string{cleanReasonInvalidOutput}
		return result
	}
	orZero := func(value *int64) *int64 {
		if value == nil {
			return new(int64)
		}
		return value
	}
	result.Status = cleanMeasured
	result.TotalBytes, result.TotalCount, result.ActiveCount = orZero(usage.TotalSize), usage.TotalCount, orZero(usage.ActiveCount)
	// 볼륨의 회수 가능량은 사용자 데이터를 지우라는 뜻으로 읽힐 수 있어 내지 않는다.
	if kind != cleanDockerVolumes {
		result.ReclaimableBytes = orZero(usage.Reclaimable)
		result.Cleanup = map[string]string{cleanDockerImages: "docker image prune -a", cleanDockerContainers: "docker container prune",
			cleanDockerBuildCache: "docker builder prune"}[kind]
	}
	return result
}

// cleanDockerFinish는 유형별 결과로 섹션 상태를 정한다. 측정하지 못한 유형이 있으면 partial이고, 하나도 없으면 failed다.
func cleanDockerFinish(section *cleanDocker) {
	measured, failed := 0, 0
	for _, result := range section.Types {
		switch result.Status {
		case cleanMeasured:
			measured++
		case cleanFailed:
			failed++
			for _, reason := range result.Reasons {
				if !slices.Contains(section.Reasons, reason) {
					section.Reasons = append(section.Reasons, reason)
				}
			}
		}
	}
	switch {
	case failed == 0:
		section.Status = cleanMeasured
	case measured > 0:
		section.Status = cleanPartial
	default:
		section.Status = cleanFailed
	}
}

// cleanVMDiskSpec은 확인한 VM 디스크 위치다. 확인하지 않은 경로는 두지 않는다.
type cleanVMDiskSpec struct {
	id    string
	path  string
	holds []string
}

// cleanVMDisks는 호스트에서 걸어 잴 수 있는 VM 디스크다. Linux의 엔진 데이터는 호스트 경로로 걷지 않는다.
func cleanVMDisks(goos, home string) []cleanVMDiskSpec {
	if goos != "darwin" {
		return nil
	}
	return []cleanVMDiskSpec{{id: cleanOrbStack, path: filepath.Join(home, "Library/Group Containers/HUAQ24HBR6.dev.orbstack/data"),
		holds: []string{"docker_engine", "linux_machines"}}}
}

// cleanVMDisk는 VM 디스크 하나다. 이미지와 볼륨, 가상 머신의 파일이 들어 있어 사용자 데이터로 보고 캐시로 분류하지 않는다.
type cleanVMDisk struct {
	ID           string              `json:"id"`
	Status       string              `json:"status"`
	Reasons      []string            `json:"reasons,omitempty"`
	Detail       string              `json:"detail,omitempty"`
	Path         string              `json:"path"`
	ResolvedPath string              `json:"resolved_path,omitempty"`
	UsageBytes   *uint64             `json:"usage_bytes,omitempty"`
	LowerBound   bool                `json:"lower_bound,omitempty"`
	Filesystem   *cleanFilesystemRef `json:"filesystem,omitempty"`
	MeasuredAt   *time.Time          `json:"measured_at,omitempty"`
	ElapsedMS    *int64              `json:"elapsed_ms,omitempty"`
	UserData     bool                `json:"user_data"`
	Holds        []string            `json:"holds"`
}

func (disk cleanVMDisk) counted() bool {
	return disk.UsageBytes != nil && (disk.Status == cleanMeasured || disk.Status == cleanPartial)
}

// cleanVMScan은 VM 디스크를 할당 block으로 잰다. sparse 파일의 논리 크기는 쓰지 않는다.
func cleanVMScan(ctx context.Context, deps cleanDeps, goos string) []cleanVMDisk {
	specs := cleanVMDisks(goos, deps.home)
	disks := make([]cleanVMDisk, 0, len(specs))
	for _, spec := range specs {
		disk := cleanVMDisk{ID: spec.id, Path: spec.path, UserData: true, Holds: spec.holds}
		disks = append(disks, disk)
		last := &disks[len(disks)-1]
		if err := ctx.Err(); err != nil {
			last.Status, last.Reasons = cleanNotScanned, []string{cleanContextReason(err)}
			continue
		}
		resolved, err := duResolveRoot(spec.path)
		if err != nil {
			last.Status, last.Reasons, last.Detail = cleanFailure(err)
			continue
		}
		last.ResolvedPath = resolved
		measurement, err := cleanMeasurePath(ctx, deps, resolved)
		if err != nil {
			last.Status, last.Reasons, last.Detail = cleanFailure(err)
			continue
		}
		at, elapsed := measurement.measuredAt, measurement.elapsedMS
		last.MeasuredAt, last.ElapsedMS, last.Filesystem = &at, &elapsed, measurement.filesystem
		last.Status, last.Reasons, last.LowerBound, last.UsageBytes = measurement.status, measurement.reasons, measurement.lowerBound, measurement.usage
	}
	return disks
}

// cleanSectionsQuiet는 조용한 상태만 있는지 본다. 엔진이 없거나 멈췄고 VM 디스크가 없는 것은 확인할 일이 아니다.
func cleanSectionsQuiet(report cleanReport) bool {
	if !slices.Contains([]string{cleanMeasured, cleanToolAbsent, cleanNotRunning}, report.docker.Status) {
		return false
	}
	for _, disk := range report.vmDisks {
		if disk.Status != cleanMeasured && disk.Status != cleanAbsent {
			return false
		}
	}
	return true
}

func cleanStateText(status string, reasons []string) string {
	word := T("clean.status." + status)
	if len(reasons) > 0 {
		return T("clean.with_reason", word, T("clean.reason."+reasons[0]))
	}
	return word
}

func formatCleanDocker(section cleanDocker, verbose bool, width int, style cleanStyle) string {
	var builder strings.Builder
	if section.Status != cleanMeasured && section.Status != cleanPartial {
		builder.WriteString(duFit("\n"+style.state(T("clean.docker.state", cleanStateText(section.Status, section.Reasons)), section.Status), width) + "\n")
		if section.Detail != "" && (verbose || section.Status != cleanToolAbsent && section.Status != cleanNotRunning) {
			builder.WriteString(duFit("  "+style.dim(section.Detail), width) + "\n")
		}
		return builder.String()
	}
	line := func(indent, text string) { builder.WriteString(style.row(indent, text, width)) }
	var info []string
	if section.OperatingSystem != "" {
		info = append(info, section.OperatingSystem)
	}
	if section.APIVersion != "" {
		info = append(info, T("clean.docker.api", section.APIVersion))
	}
	suffix := ""
	if len(info) > 0 {
		suffix = " (" + strings.Join(info, ", ") + ")"
	}
	builder.WriteString(style.open(T("clean.docker.title", suffix), T("clean.docker.panel"),
		strings.Join(append(info, T("clean.docker.note")), " · "), width))
	sizes := make([]string, len(section.Types))
	items := make([]string, len(section.Types))
	var sizeWidth, nameWidth, itemsWidth int
	for index, result := range section.Types {
		sizes[index] = cleanNoSize
		if result.Status == cleanMeasured {
			sizes[index] = formatSizeShort(uint64(*result.TotalBytes))
			items[index] = T("clean.docker.items", *result.TotalCount, *result.ActiveCount)
		}
		sizeWidth = max(sizeWidth, liveWidth(sizes[index]))
		nameWidth = max(nameWidth, liveWidth(T("clean.docker.type."+result.Type)))
		itemsWidth = max(itemsWidth, liveWidth(items[index]))
	}
	for index, result := range section.Types {
		// 길이가 같은 개수 칸을 앞에 두고, 종류마다 길이가 다른 회수 가능량 문구는 끝에 둔다. 문구를 칸으로 맞추면 가장 긴
		// volumes 문구가 모든 행의 개수를 오른쪽으로 민다.
		var rest string
		var bytes uint64
		switch {
		case result.Status != cleanMeasured:
			rest = style.state(cleanStateText(result.Status, result.Reasons), result.Status)
		case result.Type == cleanDockerVolumes:
			bytes = uint64(*result.TotalBytes)
			rest = style.dim(cleanPad(items[index], itemsWidth) + "  " + T("clean.docker.no_reclaim"))
		default:
			bytes = uint64(*result.TotalBytes)
			reclaimable := uint64(*result.ReclaimableBytes)
			rest = style.dim(cleanPad(items[index], itemsWidth)+"  ") +
				style.phrase("clean.docker.reclaimable", formatSizeShort(reclaimable), richSizeColor(int64(reclaimable)))
		}
		line("  ", style.size(cleanPadLeft(sizes[index], sizeWidth), bytes, true)+"  "+cleanPad(T("clean.docker.type."+result.Type), nameWidth)+"  "+rest)
		if verbose && result.Cleanup != "" {
			line(strings.Repeat(" ", 2+sizeWidth+4), style.phrase("clean.cleanup", result.Cleanup, richCyan))
		}
	}
	if verbose && section.Endpoint != "" {
		line("  ", style.dim(T("clean.docker.endpoint", section.Endpoint)))
	}
	builder.WriteString(style.close())
	return builder.String()
}

func formatCleanVMDisks(disks []cleanVMDisk, inside string, width int, home string, style cleanStyle) string {
	var shown []cleanVMDisk
	var absent []string
	for _, disk := range disks {
		if disk.Status == cleanAbsent {
			absent = append(absent, T("clean.vm.disk."+disk.ID))
			continue
		}
		shown = append(shown, disk)
	}
	size := func(disk cleanVMDisk) string {
		if disk.counted() {
			return cleanSize(*disk.UsageBytes, disk.LowerBound)
		}
		return cleanNoSize
	}
	var sizeWidth, nameWidth int
	for _, disk := range shown {
		sizeWidth = max(sizeWidth, liveWidth(size(disk)))
		nameWidth = max(nameWidth, liveWidth(T("clean.vm.disk."+disk.ID)))
	}
	var builder strings.Builder
	if len(shown) > 0 {
		builder.WriteString(style.open(T("clean.vm.title"), T("clean.vm.panel"), T("clean.vm.note"), width))
	}
	for _, disk := range shown {
		place := style.dim(cleanHomePath(disk.Path, home))
		var bytes uint64
		if disk.counted() {
			bytes = *disk.UsageBytes
		} else {
			place = strings.TrimSpace(style.state(cleanStateText(disk.Status, disk.Reasons), disk.Status) + "  " + place)
		}
		builder.WriteString(style.row("  ", style.size(cleanPadLeft(size(disk), sizeWidth), bytes, true)+"  "+cleanPad(T("clean.vm.disk."+disk.ID), nameWidth)+"  "+place, width))
		if disk.ID == cleanOrbStack && inside == cleanOrbStack {
			builder.WriteString(style.row(strings.Repeat(" ", 2+sizeWidth+2), style.dim(T("clean.vm.inside")), width))
		}
	}
	if len(shown) > 0 {
		builder.WriteString(style.close())
	}
	if len(absent) > 0 {
		if len(shown) == 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(style.wrap(T("clean.vm.fold_absent", strings.Join(absent, ", ")), width))
	}
	return builder.String()
}
