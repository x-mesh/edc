package edc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ctr은 Unix socket을 통해 Docker Engine API를 읽는다. container를 시작하거나 멈추지 않는다.

const (
	ctrProbeID = "ctr"
	// ctrLogLines는 docker logs에서 읽는 마지막 줄 수이고, ctrLogShown은 그 가운데 보여 주는 오류 줄 수다.
	ctrLogLines = 50
	ctrLogShown = 5
	// ctrMemoryWarnPercent 이상이면 memory 한도에 가깝다. ctrPidsWarnPercent는 pids 한도, ctrThrottleWarnPercent는
	// CPU throttle이 걸린 period의 비율이다.
	ctrMemoryWarnPercent   = 90
	ctrPidsWarnPercent     = 90
	ctrThrottleWarnPercent = 25
	ctrShortIDLength       = 12
)

// errCtrNoImage는 이 호스트에 없는 이미지다. tag를 지웠거나 다른 호스트에서 만든 container다.
var errCtrNoImage = errors.New("no such image")

const ctrWritableWarnBytes = 10 << 30

var errCtrInvalidReference = errors.New("invalid container reference")

// ctrLogErrorLine은 로그에서 오류로 보는 줄이다. 로그 형식을 모르므로 흔한 낱말로만 고른다.
var ctrLogErrorLine = regexp.MustCompile(`(?i)\b(error|fatal|panic|exception|traceback|oom|out of memory)\b`)

type ctrInspect struct {
	PID          int
	Name         string
	Status       string
	Health       string
	Image        string
	ID           string
	StartedAt    time.Time
	FinishedAt   time.Time
	RestartCount int
	OOMKilled    bool
	ExitCode     int
	Ports        []ctrPublishedPort
	// RestartPolicy는 docker의 --restart 이름이고, RestartMax는 on-failure의 재시도 한도다. 0이면 한도가 없다.
	RestartPolicy string
	RestartMax    int
	// ImageID는 container가 실행 중인 이미지다. Image는 만들 때 쓴 참조라, 그 tag가 다른 이미지로 옮겨 가도 그대로 남는다.
	ImageID     string
	TTY         bool
	HealthData  *ctrHealth
	Disk        ctrDisk
	MemoryLimit *int64
	PidsLimit   *int64
	UsernsMode  string
}

// ctrImage는 이미지 참조가 지금 가리키는 이미지와 container가 실행 중인 이미지의 생성 시각이다. pull이나 build가 tag를
// 옮겨도 실행 중인 container는 다시 만들기 전까지 예전 이미지로 돈다.
type ctrImage struct {
	TagID      string
	TagCreated time.Time
	TagMissing bool
	Created    time.Time
	Now        time.Time
}

// ctrHealth는 docker가 남긴 healthcheck 기록이다. docker는 최근 검사 5개만 남긴다.
type ctrHealth struct {
	Status        string
	FailingStreak int
	Log           []ctrHealthCheck
}

type ctrHealthCheck struct {
	Start    time.Time
	ExitCode int
	Output   string
}

// ctrPublishedPort는 host에 publish한 container port다. docker는 IPv4와 IPv6에 따로 bind하므로 Hosts가 여럿일 수 있다.
type ctrPublishedPort struct {
	Proto string
	Port  int
	Hosts []string
}

// ctrLimits는 container cgroup의 한도와 사용량이다. CPU는 누적 counter라 container가 산 동안의 비율이다.
type ctrLimits struct {
	topLimitStatus
	Path            string
	Memory          topCgroupMemory
	Events          topCgroupEvents
	CPU             ctrCPULimits
	CPUUsage        topLimitStatus
	CPUUsagePercent *float64
	Pids            ctrPidsLimits
	PidsLimitKnown  bool
}

type ctrCPULimits struct {
	topLimitStatus
	Periods, Throttled, ThrottledUS uint64
}

type ctrPidsLimits struct {
	topLimitStatus
	Current   uint64
	Max       uint64
	Unlimited bool
}

type ctrLog struct {
	Lines   []string
	Scanned int
}

// ctrDisk는 container가 docker 저장소에 남기는 것이다. 크기가 nil이면 읽지 못했다.
type ctrDisk struct {
	Writable   *uint64
	LogSize    *uint64
	LogDriver  string
	LogPath    string
	LogMaxSize string
	LogMaxFile string
	LogSizeErr error
}

type ctrImageRef struct {
	ID      string
	Created time.Time
}

// ctrSources는 ctr이 읽는 원천이다. 테스트는 함수를 가짜로 넣는다.
type ctrSources struct {
	euid          int
	docker        func() bool
	inspect       func(ctx context.Context, reference string) (ctrInspect, error)
	top           func(ctx context.Context, id string) ([]topProcess, error)
	stats         func(ctx context.Context, id string, inspect ctrInspect, detailed bool) ctrLimits
	listStats     func(ctx context.Context, inspects []ctrInspect) map[string]ctrListStats
	logs          func(ctx context.Context, id string, lines int, tty bool) (string, error)
	image         func(ctx context.Context, reference string) (ctrImageRef, error)
	inspectDetail func(ctx context.Context, reference string) (ctrInspect, error)
	close         func()
	now           func() time.Time
	// listIDs는 모든 container의 ID이고, inspectMany는 여러 container를 한 번에 읽는다. container마다 docker를 부르면 수백 번이 된다.
	listIDs        func(ctx context.Context) ([]string, error)
	inspectMany    func(ctx context.Context, ids []string) ([]ctrInspect, error)
	collector      func(ctx context.Context, inspect ctrInspect, image string) (ctrCollectorResponse, error)
	collectorImage string
}

type ctrListStats struct {
	Limits                                                              ctrLimits
	NetworkRX, NetworkTX, BlockRead, BlockWrite                         *uint64
	NetworkRXReason, NetworkTXReason, BlockReadReason, BlockWriteReason string
	CPUReason                                                           string
}

type ctrFacts struct {
	svcFacts
	Reference           string
	Docker              bool
	Inspect             ctrInspect
	InspectOutput       string
	InspectErr          error
	Health              ctrHealth
	HealthErr           error
	Image               ctrImage
	ImageErr            error
	Limits              ctrLimits
	LimitsErr           error
	Log                 ctrLog
	LogErr              error
	Disk                ctrDisk
	DiskErr             error
	Collector           *ctrCollectorResponse
	CollectorErr        error
	CollectorCleanupErr *ctrCollectorCleanupError
}

func runCtr(args []string, version string) int {
	sources, ok := hostCtrSources()
	return runCtrWith(args, version, sources, ok)
}

func runCtrWith(args []string, version string, sources ctrSources, ok bool) int {
	if sources.close != nil {
		defer sources.close()
	}
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("ctr")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	all := set.Bool("all", false, T("command.ctr.option.all"))
	collectorImage := set.String("collector-image", "", T("command.ctr.option.collector_image"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	usage := T("cli.usage", "edc ctr [options] [--all] | edc ctr [options] <container>")
	sources.collectorImage = *collectorImage
	if set.NArg() == 0 {
		return runCtrList(options, version, sources, ok, *all)
	}
	// --all은 목록에서 정상 종료한 container까지 보이는 option이다. container 하나를 볼 때 받으면 무엇이 바뀌는지 오해한다.
	if set.NArg() != 1 || *all {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	reference, err := normalizeCtrReference(set.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, T("ctr.error.invalid_reference", set.Arg(0)))
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	started := time.Now()
	target := map[string]interface{}{"container": reference}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(ctrProbeID, T("ctr.skip.linux_only"))}, options.redact))
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	facts := collectCtrFacts(ctx, reference, sources)
	if facts.Docker && facts.InspectErr != nil && ctrNotFound(facts.InspectErr) {
		fmt.Fprintln(os.Stderr, T("ctr.error.not_found", reference))
		return 1
	}
	return emit(options, buildReport(version, started, target, evaluateCtr(facts), options.redact))
}

// normalizeCtrReference는 docker에 넘겨도 되는 이름이나 ID만 받는다. '-'로 시작하면 option이 된다.
func normalizeCtrReference(input string) (string, error) {
	if input == "" || strings.HasPrefix(input, "-") || len(input) > 128 {
		return "", errCtrInvalidReference
	}
	for _, char := range input {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.-/", char)) {
			return "", errCtrInvalidReference
		}
	}
	return input, nil
}

func ctrNotFound(err error) bool {
	var httpErr *ctrHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

// filterCtrLog는 docker logs 출력에서 오류 줄만 마지막 ctrLogShown줄 남긴다.
func filterCtrLog(output string) ctrLog {
	var log ctrLog
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		log.Scanned++
		if ctrLogErrorLine.MatchString(line) {
			log.Lines = append(log.Lines, svcTruncate(line, svcJournalLineRunes))
		}
	}
	if len(log.Lines) > ctrLogShown {
		log.Lines = log.Lines[len(log.Lines)-ctrLogShown:]
	}
	return log
}

func collectCtrFacts(ctx context.Context, reference string, sources ctrSources) ctrFacts {
	facts := ctrFacts{svcFacts: svcFacts{Unit: reference, Root: sources.euid == 0, Systemd: true, Probe: ctrProbeID}, Reference: reference, Docker: sources.docker()}
	if !facts.Docker {
		return facts
	}
	inspectSource := sources.inspect
	if sources.inspectDetail != nil {
		inspectSource = sources.inspectDetail
	}
	facts.Inspect, facts.InspectErr = inspectSource(ctx, reference)
	if facts.InspectErr != nil {
		facts.InspectOutput = facts.InspectErr.Error()
		return facts
	}
	inspect := facts.Inspect
	if inspect.Status == "running" && !inspect.StartedAt.IsZero() {
		if uptime := sources.now().Sub(inspect.StartedAt); uptime >= 0 {
			facts.Uptime, facts.UptimeKnown = uptime, true
		}
	}
	if inspect.Health != "" {
		facts.Health, facts.HealthErr = readCtrHealth(inspect)
	}
	if inspect.ImageID != "" {
		facts.Image, facts.ImageErr = readCtrImage(ctx, sources, inspect)
	}
	// 멈춘 container도 log와 writable layer를 남기므로 상태와 상관없이 읽는다.
	facts.Disk, facts.DiskErr = readCtrDisk(inspect)
	facts.Show = svcShow{ID: inspect.Name, LoadState: "loaded", ActiveState: inspect.Status, MainPID: inspect.PID}
	if inspect.PID > 0 {
		facts.Processes, facts.ProcessErr = sources.top(ctx, inspect.ID)
		facts.Limits = sources.stats(ctx, inspect.ID, inspect, true)
		facts.FD = map[int]topFDLimit{}
		for _, process := range svcRows(facts.Processes, inspect.PID) {
			facts.FD[process.PID] = topFDLimit{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: errCtrUnavailable.Error()}}
		}
		facts.ListenErr = errCtrUnavailable
	}
	output, err := sources.logs(ctx, inspect.ID, ctrLogLines, inspect.TTY)
	switch {
	case ctx.Err() != nil:
		facts.LogErr = fmt.Errorf("docker logs: %w", ctx.Err())
	case err != nil:
		facts.LogErr = fmt.Errorf("docker logs: %w: %s", err, infoSingleLine(firstLine(output)))
	default:
		facts.Log = filterCtrLog(output)
	}
	if sources.collectorImage != "" {
		switch {
		case sources.collector == nil:
			facts.CollectorErr = errCtrUnavailable
		case ctx.Err() != nil:
			facts.CollectorErr = ctx.Err()
		default:
			response, err := sources.collector(ctx, inspect, sources.collectorImage)
			facts.CollectorErr = err
			if err == nil {
				facts.Collector = &response
			} else {
				var cleanupErr *ctrCollectorCleanupError
				if errors.As(err, &cleanupErr) {
					facts.CollectorCleanupErr = cleanupErr
				}
			}
		}
		if facts.CollectorErr != nil {
			facts.Disk.LogSizeErr = fmt.Errorf("collector log size: %w", facts.CollectorErr)
		}
	}
	return facts
}

func readCtrHealth(inspect ctrInspect) (ctrHealth, error) {
	if inspect.HealthData != nil {
		return *inspect.HealthData, nil
	}
	return ctrHealth{}, errCtrUnavailable
}

// readCtrImage는 이미지 참조를 먼저 읽는다. 참조가 실행 중인 이미지를 가리키면 한 번으로 끝나고, 다른 이미지를 가리키거나
// 이 호스트에 없을 때만 실행 중인 이미지의 생성 시각을 한 번 더 읽는다.
func readCtrImage(ctx context.Context, sources ctrSources, inspect ctrInspect) (ctrImage, error) {
	image := ctrImage{Now: sources.now()}
	id, created, err := lookupCtrImage(ctx, sources, inspect.Image)
	switch {
	case errors.Is(err, errCtrNoImage):
		image.TagMissing = true
	case err != nil:
		return ctrImage{}, err
	default:
		image.TagID, image.TagCreated = id, created
	}
	if image.TagID == inspect.ImageID {
		image.Created = created
		return image, nil
	}
	// 실행 중인 이미지는 docker rmi -f로 tag만 지워져도 ID로 남는다. 그마저 없으면 생성 시각만 모른다.
	if _, image.Created, err = lookupCtrImage(ctx, sources, inspect.ImageID); err != nil && !errors.Is(err, errCtrNoImage) {
		return ctrImage{}, err
	}
	return image, nil
}

func lookupCtrImage(ctx context.Context, sources ctrSources, reference string) (string, time.Time, error) {
	image, err := sources.image(ctx, reference)
	switch {
	case err != nil && errors.Is(err, errCtrNoImage):
		return "", time.Time{}, errCtrNoImage
	case err != nil:
		return "", time.Time{}, fmt.Errorf("Docker Engine image API: %w", err)
	}
	if !strings.HasPrefix(image.ID, "sha256:") || image.Created.IsZero() {
		return "", time.Time{}, fmt.Errorf("invalid Docker image response")
	}
	return image.ID, image.Created, nil
}

func readCtrDisk(inspect ctrInspect) (ctrDisk, error) {
	disk := inspect.Disk
	if disk.LogSizeErr == nil {
		disk.LogSizeErr = errCtrUnavailable
	}
	return disk, nil
}

func evaluateCtr(facts ctrFacts) []Result {
	if !facts.Docker {
		return []Result{unsupported(ctrProbeID, T("ctr.skip.no_docker"))}
	}
	if facts.InspectErr != nil {
		detail := strings.TrimSpace(facts.InspectOutput)
		if detail == "" {
			detail = facts.InspectErr.Error()
		}
		// docker socket을 열지 못한 것은 container의 문제가 아니다. root나 docker 그룹이 필요하다고 알린다.
		if strings.Contains(detail, "permission denied") {
			return []Result{svcResultFor(ctrProbeID, "state", StatusSkip, T("ctr.skip.needs_docker_access"))}
		}
		result := svcResultFor(ctrProbeID, "state", StatusFail, T("ctr.state.inspect_failed", infoSingleLine(firstLine(detail))))
		result.Error = &DiagnosticError{Kind: "command", Message: detail}
		return []Result{result}
	}
	processes := svcProcessesResult(facts.svcFacts)
	if facts.ProcessErr != nil {
		processes = svcSkipFor(ctrProbeID, "processes", "Docker top API", facts.ProcessErr)
	}
	if facts.Collector != nil {
		if facts.Collector.OOMStatus.Status == "available" {
			facts.Limits.Events = topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: facts.Collector.OOM, OOMKill: facts.Collector.OOMKill}
		} else if facts.Limits.Events.Status != "available" && facts.Collector.OOMStatus.Reason != "" {
			facts.Limits.Events.topLimitStatus = topLimitStatus{Status: facts.Collector.OOMStatus.Status, Reason: facts.Collector.OOMStatus.Reason}
		}
		if facts.Collector.LogStatus.Status == "available" {
			facts.Disk.LogSize = facts.Collector.LogBytes
			facts.Disk.LogSizeErr = nil
		}
	}
	results := []Result{ctrStateResult(facts), ctrImageResult(facts), processes, ctrLimitsResult(facts), ctrFDResult(facts), ctrListenResult(facts), ctrLogResult(facts), ctrDiskResult(facts)}
	if facts.CollectorCleanupErr != nil {
		result := svcResultFor(ctrProbeID, "collector", StatusFail, facts.CollectorCleanupErr.Error())
		result.Error = &DiagnosticError{Kind: "command", Message: facts.CollectorCleanupErr.Error()}
		results = append(results, result)
	}
	return results
}

func ctrFDResult(facts ctrFacts) Result {
	if facts.Inspect.PID <= 0 {
		return svcResultFor(ctrProbeID, "fd", StatusSkip, T("ctr.skip.not_running"))
	}
	if facts.Collector != nil {
		metric := facts.Collector.FDStatus
		metrics := map[string]interface{}{"status": metric.Status, "processes": facts.Collector.FDs}
		if metric.Reason != "" {
			metrics["reason"] = metric.Reason
		}
		if metric.Status != "available" && metric.Status != "partial" {
			result := svcResultFor(ctrProbeID, "fd", StatusSkip, infoSingleLine(metric.Reason))
			result.Metrics = metrics
			return result
		}
		collectorFacts := svcFacts{Unit: facts.Reference, Probe: ctrProbeID, Show: svcShow{MainPID: facts.Inspect.PID}, FD: map[int]topFDLimit{}}
		for _, process := range facts.Collector.FDs {
			var used *int
			if process.Used != nil && *process.Used <= uint64(ctrCollectorFDLimit) {
				value := int(*process.Used)
				used = &value
			}
			collectorFacts.Processes = append(collectorFacts.Processes, topProcess{PID: process.PID, Command: ""})
			collectorFacts.FD[process.PID] = topFDLimit{topLimitStatus: topLimitStatus{Status: process.Status, Reason: process.Reason}, Used: used, Soft: process.Soft, Unlimited: process.Unlimited}
		}
		result := svcFDResult(collectorFacts)
		result.Metrics = metrics
		if metric.Status == "partial" {
			if result.Status == StatusPass {
				result.Status = StatusWarn
			}
			result.Warnings = append(result.Warnings, infoSingleLine(metric.Reason))
		}
		return result
	}
	if facts.CollectorErr != nil {
		return svcSkipFor(ctrProbeID, "fd", "collector", facts.CollectorErr)
	}
	return svcResultFor(ctrProbeID, "fd", StatusSkip, T("ctr.skip.fd_unavailable"))
}

// ctrImageResult는 이미지 참조가 지금 가리키는 이미지와 container가 실행 중인 이미지를 맞춰 본다. 다르면 새 이미지를
// 받았지만 container를 다시 만들지 않은 것이다.
func ctrImageResult(facts ctrFacts) Result {
	inspect, image := facts.Inspect, facts.Image
	switch {
	case inspect.ImageID == "":
		return svcResultFor(ctrProbeID, "image", StatusSkip, T("ctr.image.no_id"))
	case facts.ImageErr != nil:
		return svcSkipFor(ctrProbeID, "image", "docker image inspect", facts.ImageErr)
	}
	created := ctrImageCreated(image.Created, image.Now)
	metrics := map[string]interface{}{"reference": inspect.Image, "image_id": inspect.ImageID, "tag_missing": image.TagMissing}
	if !image.Created.IsZero() {
		metrics["image_created"] = image.Created.UTC().Format(time.RFC3339)
	}
	var result Result
	switch {
	case image.TagMissing:
		result = svcResultFor(ctrProbeID, "image", StatusPass, T("ctr.image.no_tag", inspect.Image, ctrShortImageID(inspect.ImageID), created))
	case image.TagID == inspect.ImageID:
		result = svcResultFor(ctrProbeID, "image", StatusPass, T("ctr.image.current", created))
	default:
		result = svcResultFor(ctrProbeID, "image", StatusWarn, T("ctr.image.outdated", inspect.Image, ctrShortImageID(image.TagID), ctrImageCreated(image.TagCreated, image.Now), ctrShortImageID(inspect.ImageID), created))
	}
	if !image.TagMissing {
		metrics["tag_image_id"] = image.TagID
		if !image.TagCreated.IsZero() {
			metrics["tag_image_created"] = image.TagCreated.UTC().Format(time.RFC3339)
		}
	}
	result.Metrics = metrics
	return result
}

func ctrShortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > ctrShortIDLength {
		return id[:ctrShortIDLength]
	}
	return id
}

// ctrImageCreated는 이미지 생성일과 지난 시간이다. 생성 시각을 모르면 -이다.
func ctrImageCreated(created, now time.Time) string {
	if created.IsZero() {
		return "-"
	}
	date := created.Local().Format(time.DateOnly)
	if age := now.Sub(created); age >= 0 {
		return T("ctr.image.created", date, svcFormatUptime(age))
	}
	return date
}

// ctrListenResult는 svc의 listen 결과에 publish한 port를 맞춰 본다. docker는 host port로 온 연결을 container IP로 넘기므로,
// 안에서 그 port를 listen하는 process가 없거나 loopback에서만 listen하면 publish한 port로는 닿지 않는다. 막 시작한 container는
// 아직 listen하기 전일 수 있어 fail이 아니라 warn이다.
func ctrListenResult(facts ctrFacts) Result {
	if facts.Inspect.PID <= 0 {
		return svcResultFor(ctrProbeID, "listen", StatusSkip, T("ctr.skip.not_running"))
	}
	result := svcResultFor(ctrProbeID, "listen", StatusSkip, T("ctr.skip.listen_unavailable"))
	if facts.Collector != nil {
		metric := facts.Collector.ListenStatus
		listeners := facts.Collector.Listeners
		published := make([]map[string]interface{}, 0, len(facts.Inspect.Ports))
		observed := make([]map[string]interface{}, 0, len(listeners))
		var publishedLines, listenerLines []string
		for _, listener := range listeners {
			observed = append(observed, map[string]interface{}{"proto": listener.Proto, "address": listener.Address, "port": listener.Port, "inode": listener.Inode})
			address := net.JoinHostPort(listener.Address, strconv.Itoa(listener.Port))
			listenerLines = append(listenerLines, fmt.Sprintf("%s %s (inode %d)", strings.ToUpper(listener.Proto), address, listener.Inode))
		}
		missing, loopbackOnly := 0, 0
		for _, port := range facts.Inspect.Ports {
			state := "unavailable"
			if metric.Status == "available" {
				state = "not_listening"
			}
			for _, listener := range listeners {
				if listener.Proto != port.Proto || listener.Port != port.Port {
					continue
				}
				switch listener.Address {
				case "::":
					if state != "listening" {
						state = "coverage_unknown"
					}
				case "127.0.0.1", "::1":
					if state != "listening" && state != "coverage_unknown" {
						state = "loopback_only"
					}
				default:
					state = "listening"
				}
			}
			if metric.Status == "available" && (state == "not_listening" || state == "loopback_only") {
				missing++
			}
			if state == "loopback_only" {
				loopbackOnly++
			}
			label := fmt.Sprintf("%d/%s", port.Port, port.Proto)
			publishedLines = append(publishedLines, fmt.Sprintf("%s → %s (state: %s)", label, strings.Join(port.Hosts, ", "), state))
			published = append(published, map[string]interface{}{"port": port.Port, "proto": port.Proto, "hosts": port.Hosts, "state": state})
		}
		status := StatusPass
		if metric.Status == "partial" {
			status = StatusWarn
		} else if metric.Status != "available" {
			status = StatusSkip
		} else if missing > 0 {
			status = StatusWarn
		}
		summary := T("ctr.collector.listen_summary", len(listeners), len(facts.Inspect.Ports))
		if metric.Status != "available" && metric.Reason != "" {
			summary = infoSingleLine(metric.Reason)
		} else if metric.Status == "partial" {
			summary = infoSingleLine(fmt.Sprintf("%s: %s", metric.Status, summary))
		}
		result = svcResultFor(ctrProbeID, "listen", status, summary)
		result.Metrics = map[string]interface{}{"status": metric.Status, "reason": metric.Reason, "published": published, "listeners": observed}
		if len(publishedLines) > 0 {
			result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.published"), Value: strings.Join(publishedLines, "\n")})
		}
		if len(listenerLines) > 0 {
			result.Evidence = append(result.Evidence, Evidence{Label: T("svc.evidence.sockets"), Value: strings.Join(listenerLines, "\n")})
		}
		if metric.Status != "available" && metric.Reason != "" {
			result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: metric.Reason})
		}
		if missing > 0 && metric.Status == "available" {
			result.Warnings = append(result.Warnings, T("ctr.collector.listen_missing", missing))
		}
		if loopbackOnly > 0 && metric.Status == "available" {
			result.Warnings = append(result.Warnings, T("ctr.collector.listen_loopback", loopbackOnly))
		}
		return result
	}
	if facts.CollectorErr != nil {
		return svcSkipFor(ctrProbeID, "listen", "collector", facts.CollectorErr)
	}
	var evidence []string
	var published []map[string]interface{}
	for _, port := range facts.Inspect.Ports {
		label := fmt.Sprintf("%d/%s", port.Port, port.Proto)
		evidence = append(evidence, fmt.Sprintf("%s → %s", label, strings.Join(port.Hosts, ", ")))
		published = append(published, map[string]interface{}{"port": port.Port, "proto": port.Proto, "hosts": port.Hosts, "state": "unavailable"})
	}
	result.Metrics = map[string]interface{}{"published": published}
	if len(evidence) > 0 {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.published"), Value: strings.Join(evidence, "\n")})
	}
	return result
}

// ctrStateText는 상세와 목록이 같이 쓰는 상태 문구다. 한 번도 돈 적 없는 container는 종료 코드가 없다.
func ctrStateText(inspect ctrInspect) string {
	state := inspect.Status
	if inspect.Health != "" {
		state += " (" + inspect.Health + ")"
	}
	if inspect.Status != "running" && !inspect.FinishedAt.IsZero() {
		state += fmt.Sprintf(", exit code %d", inspect.ExitCode)
	}
	if inspect.OOMKilled {
		state += ", OOM killed"
	}
	return state
}

func ctrStateResult(facts ctrFacts) Result {
	inspect := facts.Inspect
	state := ctrStateText(inspect)
	uptime := "-"
	if facts.UptimeKnown {
		uptime = svcFormatUptime(facts.Uptime)
	}
	status := StatusWarn
	switch inspect.Status {
	case "running":
		status = StatusPass
		if inspect.RestartCount > 0 || inspect.OOMKilled || inspect.Health == "unhealthy" {
			status = StatusWarn
		}
	case "exited", "dead":
		status = StatusFail
	}
	policy := ctrRestartPolicyText(inspect)
	result := svcResultFor(ctrProbeID, "state", status, T("ctr.state.summary", state, uptime, inspect.RestartCount, policy, inspect.Image))
	if status == StatusFail {
		result = svcFailed(result)
	}
	id := inspect.ID
	if len(id) > ctrShortIDLength {
		id = id[:ctrShortIDLength]
	}
	result.Metrics = map[string]interface{}{"status": inspect.Status, "pid": inspect.PID, "restart_count": inspect.RestartCount, "oom_killed": inspect.OOMKilled, "exit_code": inspect.ExitCode, "image": inspect.Image, "id": id, "restart_policy": inspect.RestartPolicy}
	if inspect.RestartPolicy == "on-failure" {
		result.Metrics["restart_max"] = inspect.RestartMax
	}
	// on-failure의 한도만큼 재시작한 뒤 멈춘 container는 docker가 다시 띄우지 않는다. 누가 시작하기 전까지 멈춰 있다.
	if inspect.Status == "exited" && inspect.RestartPolicy == "on-failure" && inspect.RestartMax > 0 && inspect.RestartCount >= inspect.RestartMax {
		result.Warnings = append(result.Warnings, T("ctr.state.restart_gave_up", inspect.RestartCount, policy))
	}
	if inspect.Health != "" {
		result.Metrics["health"] = inspect.Health
		addCtrHealth(&result, facts)
	}
	if facts.UptimeKnown {
		result.Metrics["uptime_s"] = int64(facts.Uptime.Seconds())
	}
	return result
}

// ctrRestartPolicyText는 docker --restart의 표기를 그대로 쓴다. HostConfig를 읽지 못해 정책을 모르면 -이다.
func ctrRestartPolicyText(inspect ctrInspect) string {
	switch {
	case inspect.RestartPolicy == "":
		return "-"
	case inspect.RestartPolicy == "no":
		return T("ctr.state.no_policy")
	case inspect.RestartPolicy == "on-failure" && inspect.RestartMax > 0:
		return fmt.Sprintf("on-failure:%d", inspect.RestartMax)
	}
	return inspect.RestartPolicy
}

// addCtrHealth는 healthcheck 기록을 상세에 붙인다. unhealthy이면 가장 최근에 실패한 검사의 출력을 warning으로 올린다.
// 기록을 읽지 못해도 상태는 docker inspect가 준 값이 맞으므로, 읽지 못한 까닭만 상세에 적는다.
func addCtrHealth(result *Result, facts ctrFacts) {
	if facts.HealthErr != nil {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.health"), Value: facts.HealthErr.Error()})
		return
	}
	health := facts.Health
	result.Metrics["health_failing_streak"] = health.FailingStreak
	if len(health.Log) == 0 {
		return
	}
	var lines []string
	var failed *ctrHealthCheck
	for index, check := range health.Log {
		lines = append(lines, fmt.Sprintf("%s  exit %d  %s", check.Start.Local().Format(time.DateTime), check.ExitCode, ctrHealthOutput(check.Output)))
		if check.ExitCode != 0 {
			failed = &health.Log[index]
		}
	}
	last := health.Log[len(health.Log)-1]
	result.Metrics["health_last_exit_code"], result.Metrics["health_last_output"] = last.ExitCode, ctrHealthOutput(last.Output)
	result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.health"), Value: strings.Join(lines, "\n")})
	if facts.Inspect.Health == "unhealthy" && failed != nil {
		result.Warnings = append(result.Warnings, T("ctr.state.health_failed", health.FailingStreak, failed.ExitCode, ctrHealthOutput(failed.Output)))
	}
}

// ctrHealthOutput은 검사 출력의 첫 줄이다. 출력이 없는 검사는 -이다.
func ctrHealthOutput(output string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "-"
	}
	return svcTruncate(line, svcJournalLineRunes)
}

func ctrLimitsResult(facts ctrFacts) Result {
	if facts.Collector != nil && facts.Collector.OOMStatus.Status == "available" && (facts.Inspect.PID <= 0 || facts.Limits.Status != "available") {
		metrics := map[string]interface{}{"oom_count": facts.Collector.OOM, "oom_kill_count": facts.Collector.OOMKill, "oom_status": facts.Collector.OOMStatus.Status}
		result := svcResultFor(ctrProbeID, "limits", StatusSkip, T("ctr.collector.oom_summary", *facts.Collector.OOM, *facts.Collector.OOMKill))
		result.Metrics = metrics
		return result
	}
	switch {
	case facts.Inspect.PID <= 0:
		result := svcResultFor(ctrProbeID, "limits", StatusSkip, T("ctr.skip.not_running"))
		if facts.CollectorErr != nil {
			result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: facts.CollectorErr.Error()})
		}
		return result
	case facts.LimitsErr != nil:
		result := svcSkipFor(ctrProbeID, "limits", "cgroup", facts.LimitsErr)
		if facts.CollectorErr != nil {
			result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: facts.CollectorErr.Error()})
		}
		return result
	case facts.Limits.Status != "available" && facts.Limits.CPUUsage.Status != "available":
		result := svcResultFor(ctrProbeID, "limits", StatusSkip, T("svc.skip.unreadable", "cgroup", infoSingleLine(facts.Limits.Reason)))
		if facts.CollectorErr != nil {
			result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: facts.CollectorErr.Error()})
		}
		return result
	}
	limits := facts.Limits
	var warnings []string
	metrics := map[string]interface{}{}
	cpuUsage := ""
	if limits.CPUUsage.Status == "available" && limits.CPUUsagePercent != nil {
		metrics["cpu_usage_percent"] = *limits.CPUUsagePercent
		cpuUsage = fmt.Sprintf("%.2f%%", *limits.CPUUsagePercent)
	}
	memory := "-"
	if limits.Memory.Status == "available" && limits.Memory.Used != nil {
		metrics["memory_used_bytes"] = *limits.Memory.Used
		switch {
		case limits.Memory.Unlimited || limits.Memory.Max == nil || *limits.Memory.Max == 0:
			memory = traceBytes(*limits.Memory.Used) + " / " + T("ctr.limits.no_limit")
		default:
			percent := float64(*limits.Memory.Used) / float64(*limits.Memory.Max) * 100
			memory = fmt.Sprintf("%s / %s (%.0f%%)", traceBytes(*limits.Memory.Used), traceBytes(*limits.Memory.Max), percent)
			metrics["memory_max_bytes"], metrics["memory_percent"] = *limits.Memory.Max, percent
			if percent >= ctrMemoryWarnPercent {
				warnings = append(warnings, T("ctr.limits.memory_high", memory))
			}
		}
	}
	oom := "-"
	if facts.Collector != nil && limits.Events.Status == "available" && limits.Events.OOM != nil && limits.Events.OOMKill != nil {
		oom = T("ctr.collector.oom_summary", *limits.Events.OOM, *limits.Events.OOMKill)
		metrics["oom_count"], metrics["oom_kill_count"] = *limits.Events.OOM, *limits.Events.OOMKill
		metrics["oom_scope"] = "container_cgroup_lifetime"
	}
	cpu := "-"
	if limits.CPU.Status == "available" {
		metrics["cpu_periods"], metrics["cpu_throttled_periods"], metrics["cpu_throttled_usec"] = limits.CPU.Periods, limits.CPU.Throttled, limits.CPU.ThrottledUS
		if limits.CPU.Periods > 0 {
			percent := float64(limits.CPU.Throttled) / float64(limits.CPU.Periods) * 100
			cpu = fmt.Sprintf("%d/%d periods (%.0f%%)", limits.CPU.Throttled, limits.CPU.Periods, percent)
			metrics["cpu_throttled_percent"] = percent
			if percent >= ctrThrottleWarnPercent {
				warnings = append(warnings, T("ctr.limits.throttled", cpu))
			}
		}
	}
	pids := "-"
	if limits.Pids.Status == "available" {
		metrics["pids_current"] = limits.Pids.Current
		limitKnown := limits.PidsLimitKnown || limits.Pids.Max > 0 || limits.Pids.Unlimited
		metrics["pids_limit_known"] = limitKnown
		if limits.Pids.Unlimited {
			pids = strconv.FormatUint(limits.Pids.Current, 10) + " / " + T("ctr.limits.no_limit")
		} else if !limitKnown {
			pids = T("ctr.limits.pids_unknown", limits.Pids.Current)
		} else {
			percent := float64(limits.Pids.Current) / float64(limits.Pids.Max) * 100
			pids = fmt.Sprintf("%d / %d (%.0f%%)", limits.Pids.Current, limits.Pids.Max, percent)
			metrics["pids_max"] = limits.Pids.Max
			if percent >= ctrPidsWarnPercent {
				warnings = append(warnings, T("ctr.limits.pids_high", pids))
			}
		}
	}
	status := StatusPass
	if len(warnings) > 0 {
		status = StatusWarn
	}
	supported := 0
	var unavailable []string
	for _, metric := range []topLimitStatus{limits.Memory.topLimitStatus, limits.Events.topLimitStatus, limits.CPU.topLimitStatus, limits.CPUUsage, limits.Pids.topLimitStatus} {
		if metric.Status == "available" {
			supported++
		} else if metric.Reason != "" {
			unavailable = append(unavailable, metric.Reason)
		}
	}
	if supported == 0 {
		status = StatusSkip
	}
	summary := T("ctr.limits.summary", memory, oom, cpu, pids)
	if cpuUsage != "" {
		summary = T("ctr.limits.summary_cpu", cpuUsage, memory, oom, cpu, pids)
	}
	result := svcResultFor(ctrProbeID, "limits", status, summary)
	result.Metrics = metrics
	result.Warnings = warnings
	if len(unavailable) > 0 {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: strings.Join(slices.Compact(unavailable), "; ")})
	}
	if facts.Collector != nil && facts.Collector.OOMStatus.Status != "available" && facts.Collector.OOMStatus.Reason != "" {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: facts.Collector.OOMStatus.Reason})
	} else if facts.CollectorErr != nil {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: facts.CollectorErr.Error()})
	}
	result.Evidence = append(result.Evidence, ctrLimitsResourceEvidence(limits))
	return result
}

func ctrLimitsResourceEvidence(limits ctrLimits) Evidence {
	unavailable := func(status topLimitStatus, fallback string) string {
		reason := strings.TrimSpace(status.Reason)
		if reason == "" {
			reason = fallback
		}
		return T("ctr.evidence.resource_unavailable", infoSingleLine(reason))
	}
	cpu := unavailable(limits.CPUUsage, T("ctr.skip.api_unavailable"))
	if limits.CPUUsage.Status == "available" && limits.CPUUsagePercent != nil {
		cpu = fmt.Sprintf("%.2f%%", *limits.CPUUsagePercent)
	}
	memory := unavailable(limits.Memory.topLimitStatus, T("ctr.skip.api_unavailable"))
	if limits.Memory.Status == "available" && limits.Memory.Used != nil {
		switch {
		case limits.Memory.Unlimited:
			memory = traceBytes(*limits.Memory.Used) + " / " + T("ctr.limits.no_limit")
		case limits.Memory.Max != nil && *limits.Memory.Max > 0:
			memory = traceBytes(*limits.Memory.Used) + " / " + traceBytes(*limits.Memory.Max)
		default:
			memory = traceBytes(*limits.Memory.Used) + " / " + T("ctr.limits.limit_unknown")
		}
	}
	pids := unavailable(limits.Pids.topLimitStatus, T("ctr.skip.api_unavailable"))
	if limits.Pids.Status == "available" {
		limitKnown := limits.PidsLimitKnown || limits.Pids.Max > 0 || limits.Pids.Unlimited
		switch {
		case limits.Pids.Unlimited:
			pids = strconv.FormatUint(limits.Pids.Current, 10) + " / " + T("ctr.limits.no_limit")
		case !limitKnown:
			pids = T("ctr.limits.pids_unknown", limits.Pids.Current)
		default:
			pids = fmt.Sprintf("%d / %d", limits.Pids.Current, limits.Pids.Max)
		}
	}
	oom := unavailable(limits.Events.topLimitStatus, T("ctr.skip.api_unavailable"))
	if limits.Events.Status == "available" && limits.Events.OOM != nil && limits.Events.OOMKill != nil {
		oom = T("ctr.collector.oom_summary", *limits.Events.OOM, *limits.Events.OOMKill)
	}
	throttle := unavailable(limits.CPU.topLimitStatus, T("ctr.skip.api_unavailable"))
	if limits.CPU.Status == "available" {
		throttle = fmt.Sprintf("%d/%d periods", limits.CPU.Throttled, limits.CPU.Periods)
		if limits.CPU.Periods > 0 {
			throttle += fmt.Sprintf(" (%.0f%%)", float64(limits.CPU.Throttled)/float64(limits.CPU.Periods)*100)
		}
	}
	lines := []string{
		T("ctr.evidence.resource_cpu", cpu),
		T("ctr.evidence.resource_memory", memory),
		T("ctr.evidence.resource_pids", pids),
		T("ctr.evidence.resource_oom", oom),
		T("ctr.evidence.resource_throttle", throttle),
	}
	return Evidence{Label: T("ctr.evidence.resources"), Value: strings.Join(lines, "\n")}
}

func ctrLogResult(facts ctrFacts) Result {
	if facts.LogErr != nil {
		return svcSkipFor(ctrProbeID, "log", "docker logs", facts.LogErr)
	}
	lines := facts.Log.Lines
	if len(lines) == 0 {
		return svcResultFor(ctrProbeID, "log", StatusPass, T("ctr.log.none", facts.Log.Scanned))
	}
	result := svcResultFor(ctrProbeID, "log", StatusWarn, T("ctr.log.errors", len(lines), facts.Log.Scanned, lines[len(lines)-1]))
	result.Evidence = []Evidence{{Label: T("ctr.evidence.log"), Value: strings.Join(lines, "\n")}}
	return result
}

func ctrDiskResult(facts ctrFacts) Result {
	if facts.DiskErr != nil {
		return svcSkipFor(ctrProbeID, "disk", "docker inspect --size", facts.DiskErr)
	}
	disk := facts.Disk
	driver := disk.LogDriver
	if driver == "" {
		driver = "-"
	}
	metrics := map[string]interface{}{"log_driver": disk.LogDriver}
	var warnings, files []string
	// journald처럼 파일을 남기지 않는 driver는 docker가 크기를 모른다. driver 이름만 적는다.
	logText := T("ctr.disk.log_driver", driver)
	if disk.LogPath != "" {
		size := "-"
		if disk.LogSize != nil {
			size = traceBytes(*disk.LogSize)
			metrics["log_bytes"] = *disk.LogSize
		}
		if rotation := ctrLogRotation(disk); rotation != "" {
			driver += ", " + rotation
		}
		logText = T("ctr.disk.log_file", size, driver)
		files = append(files, disk.LogPath)
		if disk.LogSizeErr != nil {
			files = append(files, disk.LogSizeErr.Error())
		}
	}
	if disk.LogMaxSize != "" {
		metrics["log_max_size"] = disk.LogMaxSize
	}
	if disk.LogMaxFile != "" {
		metrics["log_max_file"] = disk.LogMaxFile
	}
	writable := "-"
	if disk.Writable != nil {
		writable = traceBytes(*disk.Writable)
		metrics["writable_bytes"] = *disk.Writable
		if *disk.Writable >= ctrWritableWarnBytes {
			warnings = append(warnings, T("ctr.disk.writable_large", writable))
		}
	}
	status := StatusPass
	if len(warnings) > 0 {
		status = StatusWarn
	}
	if disk.Writable == nil {
		status = StatusSkip
	}
	result := svcResultFor(ctrProbeID, "disk", status, T("ctr.disk.summary", logText, writable))
	result.Metrics, result.Warnings = metrics, warnings
	if disk.LogSizeErr != nil {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: disk.LogSizeErr.Error()})
	}
	if len(files) > 0 {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.log_file"), Value: strings.Join(files, "\n")})
	}
	return result
}

// ctrLogRotation은 log가 얼마나 커질 수 있는지 적는다. json-file은 max-size가 없으면 rotation하지 않는다.
func ctrLogRotation(disk ctrDisk) string {
	switch {
	case disk.LogMaxSize != "" && disk.LogMaxFile != "":
		return "max-size " + disk.LogMaxSize + ", max-file " + disk.LogMaxFile
	case disk.LogMaxSize != "":
		return "max-size " + disk.LogMaxSize
	case disk.LogDriver == "json-file":
		return T("ctr.disk.no_rotation")
	}
	return ""
}
