package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func runTop(args []string, version string) (code int) {
	if len(args) > 0 && args[0] == "setup" {
		return runTopSetup(args[1:], version)
	}
	set := newFlagSet("top")
	set.SetOutput(os.Stderr)
	config := activeConfig.Defaults.Top
	interval := set.Duration("interval", configuredDuration(config.Interval, time.Second), T("command.top.option.interval"))
	count := set.Int("count", configuredInt(config.Count, 0), T("command.top.option.count"))
	noHeader := set.Bool("no-header", configuredBool(config.NoHeader, false), T("command.top.option.no_header"))
	processValue := set.String("process", "", T("command.top.option.process"))
	topValue := set.Int("top", topJSONProcesses, T("command.top.option.top"))
	sortValue := set.String("sort", "cpu", T("command.top.option.sort"))
	fullCommand := set.Bool("full", false, T("command.top.option.full"))
	splitValue := set.String("split", "", T("command.top.option.split"))
	viewValue := set.String("view", configuredString(config.View, ""), T("command.top.option.view"))
	panelsValue := set.String("panels", configuredString(config.Panels, ""), T("command.top.option.panels"))
	columnsValue := set.String("columns", configuredString(config.Columns, ""), T("command.top.option.columns"))
	interfaceValue := set.String("interface", configuredString(config.Interface, ""), T("command.top.option.interface"))
	ebpf := set.Bool("ebpf", false, T("command.top.option.ebpf"))
	// -d와 --detail은 사용자에게 보이는 이름이다. v0.28.0의 --ebpf도 같은 값으로 계속 받는다.
	set.BoolVar(ebpf, "detail", false, T("command.top.option.detail"))
	set.BoolVar(ebpf, "d", false, T("command.top.option.detail"))
	jsonPath := set.String("json", configuredStringFallback(config.JSON, activeConfig.Defaults.Common.JSON, ""), T("command.top.option.json"))
	writePath := set.String("write", "", T("command.top.option.write"))
	set.StringVar(writePath, "w", "", T("command.top.option.write"))
	autoRecord := set.Bool("auto-record", configuredBool(config.AutoRecord, false), T("command.top.option.auto_record"))
	retainValue := set.String("retain", configuredString(config.Retain, ""), T("command.top.option.retain"))
	args, defaultWrite := normalizeTopWriteArgs(args, set)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("observe.top.usage"))
		return 2
	}
	if *interval < topMinInterval {
		fmt.Fprintln(os.Stderr, T("observe.top.interval_minimum", topMinInterval))
		return 2
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, T("observe.top.count_minimum"))
		return 2
	}
	if defaultWrite {
		path, err := defaultHistoryPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		*writePath = path
	}
	filter, err := parseTopProcessFilter(*processValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.process_invalid"))
		return 2
	}
	if *writePath == "-" || (*writePath != "" && sameHistoryPath(*writePath, *jsonPath)) {
		fmt.Fprintln(os.Stderr, T("history.error.path"))
		return 2
	}
	explicitSplit, explicitView, explicitPanels, explicitColumns, explicitAutoRecord, explicitRetain, explicitTop := false, false, false, false, false, false, false
	set.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "top", "sort":
			explicitTop = true
		case "split":
			explicitSplit = true
		case "view":
			explicitView = true
		case "panels":
			explicitPanels = true
		case "columns":
			explicitColumns = true
		case "retain":
			explicitRetain = true
		case "auto-record":
			explicitAutoRecord = true
		}
		if (option.Name == "write" || option.Name == "w") && option.Value.String() == "" {
			code = 2
		}
	})
	if code != 0 {
		fmt.Fprintln(os.Stderr, T("history.error.path"))
		return code
	}
	jsonOutput := *jsonPath != ""
	// 대시보드는 무한 실행에만 쓴다. --count와 --json은 표와 JSON을 그대로 흘려 보낸다.
	dashboard := !jsonOutput && *count == 0 && liveTerminal()
	// --split 검사는 --json 파일을 열거나 --write DB를 만들기 전에 끝낸다. 실패한 실행이 파일을 비우거나 DB를 남기면 안 된다.
	splitText := configuredString(config.Split, "")
	if explicitSplit {
		splitText = *splitValue
	}
	var split []topView
	if explicitSplit || splitText != "" {
		if split, err = parseTopSplit(splitText); err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.split_invalid"))
			return 2
		}
	}
	if explicitSplit && filter.active() {
		fmt.Fprintln(os.Stderr, T("observe.top.split_with_process"))
		return 2
	}
	if explicitSplit && !dashboard {
		fmt.Fprintln(os.Stderr, T("observe.top.split_needs_view"))
		return 2
	}
	if explicitAutoRecord && *autoRecord && !dashboard {
		fmt.Fprintln(os.Stderr, T("observe.top.auto_record_needs_view"))
		return 2
	}
	// config의 view, panels, process_sort는 대시보드 배치라서 표와 JSON에서는 쓰지 않는다. 플래그로 준 값만 오류로 알린다.
	var layout topLayout
	if *viewValue != "" {
		if layout.view, err = parseTopView(*viewValue); err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.view_invalid"))
			return 2
		}
	}
	if *panelsValue != "" {
		if layout.panels, err = parseTopPanels(*panelsValue); err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.panels_invalid"))
			return 2
		}
		layout.panelsSet = true
	}
	if *columnsValue != "" {
		if layout.columns, err = parseTopColumns(*columnsValue); err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.columns_invalid"))
			return 2
		}
	}
	if config.ProcessSort != nil && *config.ProcessSort != "" {
		if layout.processSort, err = parseTopProcessSort(*config.ProcessSort); err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.process_sort_invalid"))
			return 2
		}
	}
	if explicitView && filter.active() {
		fmt.Fprintln(os.Stderr, T("observe.top.view_with_process"))
		return 2
	}
	if (explicitView || explicitPanels || explicitColumns) && !dashboard {
		fmt.Fprintln(os.Stderr, T("observe.top.layout_needs_view"))
		return 2
	}
	interfaces, err := parseTopInterfaceFilter(*interfaceValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.interface_invalid", err))
		return 2
	}
	if interfaces.physical && !topPhysicalSupported {
		fmt.Fprintln(os.Stderr, T("observe.top.interface_physical_unsupported"))
		interfaces = interfaces.withoutPhysical()
	}
	if interfaces.active() {
		present, err := topPresentInterfaces()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if missing := interfaces.missingNames(present); len(missing) > 0 {
			fmt.Fprintln(os.Stderr, T("observe.top.interface_missing", strings.Join(missing, ", ")))
			return 2
		}
		if !interfaces.matchesAny(present, topInterfacePhysical) {
			fmt.Fprintln(os.Stderr, T("observe.top.interface_none", interfaces))
		}
	}
	var writer io.Writer = os.Stdout
	if *jsonPath != "" && *jsonPath != "-" {
		file, err := os.OpenFile(*jsonPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer file.Close()
		writer = file
	}
	if filter.active() && !dashboard && !jsonOutput && *writePath == "" {
		// 표에는 process 열이 없다. 필터를 조용히 무시하면 전체 host 값을 필터한 값으로 읽게 된다.
		fmt.Fprintln(os.Stderr, T("observe.top.process_needs_view"))
		return 2
	}
	// 대시보드는 실행 중에 f나 /로 process를 고를 수 있으므로 -d만 줘도 된다. 표와 JSON은 고를 수 없다.
	if *ebpf && !filter.active() && !dashboard {
		fmt.Fprintln(os.Stderr, T("observe.top.ebpf_needs_process"))
		return 2
	}
	// 표에는 process 열이 없어서 전체 명령줄을 보여 줄 자리가 없다.
	if *fullCommand && !dashboard && !jsonOutput && *writePath == "" {
		fmt.Fprintln(os.Stderr, T("observe.top.full_needs_view"))
		return 2
	}
	// 상위 N개는 JSON에만 붙는다. 표에는 process 열이 없고, --process는 고른 process를 모두 내므로 겹친다.
	if explicitTop && (!jsonOutput || filter.active()) {
		fmt.Fprintln(os.Stderr, T("observe.top.top_needs_json"))
		return 2
	}
	if *topValue < 0 {
		fmt.Fprintln(os.Stderr, T("observe.top.top_minimum"))
		return 2
	}
	if *sortValue != "cpu" && *sortValue != "mem" {
		fmt.Fprintln(os.Stderr, T("observe.top.sort_invalid"))
		return 2
	}
	topProcesses, topRank := 0, topViewCPU
	if *sortValue == "mem" {
		topRank = topViewMemory
	}
	if jsonOutput && !filter.active() {
		topProcesses = *topValue
		processSampler.setCandidateLimit(topProcesses)
		defer processSampler.setCandidateLimit(0)
	}
	// 표본을 읽기 전에 정해야 수집이 comm 대신 전체 명령줄을 담는다. 앞선 실행의 값이 남지 않게 매번 쓴다.
	topFullCommand = *fullCommand
	if *ebpf {
		observe, stop, err := startTopProcessProbe()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		defer stop()
		processSampler.setObserver(observe)
		defer processSampler.setObserver(nil)
	}
	processSampler.setFilter(filter)
	if *retainValue != "" {
		retain, err := parseLogAge(*retainValue)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.retain_invalid"))
			return 2
		}
		// 대시보드는 -w가 없어도 w로 기본 DB에 기록한다. 표와 JSON은 -w를 줄 때만 기록한다.
		retainPath := *writePath
		if retainPath == "" && dashboard {
			if retainPath, err = defaultHistoryPath(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		switch {
		case retainPath != "":
			stop, err := startHistoryRetention(retainPath, retain, *interval)
			if err != nil {
				fmt.Fprintln(os.Stderr, T("history.error.retain", err))
				return 1
			}
			defer func() {
				if err := stop(); err != nil {
					fmt.Fprintln(os.Stderr, T("history.error.retain", err))
				}
			}()
		// 설정의 retain은 기록할 때만 쓰는 기본값이라 기록하지 않는 표 출력을 막지 않는다.
		case explicitRetain:
			fmt.Fprintln(os.Stderr, T("observe.top.retain_needs_write"))
			return 2
		}
	}
	var recorder *topRecorder
	if *writePath != "" {
		details, err := collectHostDetails()
		if err != nil {
			fmt.Fprintln(os.Stderr, T("observe.top.error.host", err))
			return 1
		}
		if defaultWrite {
			if err := os.MkdirAll(filepath.Dir(*writePath), 0o700); err != nil {
				fmt.Fprintln(os.Stderr, T("history.error.write", err))
				return 1
			}
		}
		recorder, err = newTopRecorder(*writePath, version, details, *interval, filter.String(), interfaces.String(), *ebpf)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("history.error.write", err))
			return 1
		}
		defer func() {
			status := "finished"
			if code != 0 {
				status = "error"
			}
			if err := recorder.Close(status); err != nil {
				fmt.Fprintln(os.Stderr, T("history.error.write", err))
				code = 1
			}
		}()
	}
	if dashboard {
		return runTopDashboard(*interval, version, filter, interfaces, *writePath, recorder, split, layout, *autoRecord)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return streamTop(ctx, writer, topOptions{
		interval: *interval, count: *count, json: jsonOutput,
		process: filter.active(),
		top:     topProcesses, rank: topRank,
		recorder: recorder, filter: filter.String(), interfaces: interfaces,
		header: !*noHeader && !jsonOutput,
		color:  !jsonOutput && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "",
	})
}

func normalizeTopWriteArgs(args []string, set *flag.FlagSet) ([]string, bool) {
	normalized := make([]string, 0, len(args))
	defaultWrite := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			normalized = append(normalized, args[index:]...)
			break
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if name == "w" || name == "write" {
			defaultWrite = false
			if hasValue {
				normalized = append(normalized, arg)
			} else if index+1 < len(args) && (!strings.HasPrefix(args[index+1], "-") || args[index+1] == "-") {
				index++
				normalized = append(normalized, arg+"="+args[index])
			} else {
				normalized = append(normalized, arg+"=")
				defaultWrite = true
			}
			continue
		}
		if name == "split" {
			switch {
			case hasValue:
				normalized = append(normalized, arg)
			case index+1 < len(args) && !strings.HasPrefix(args[index+1], "-"):
				index++
				normalized = append(normalized, arg+"="+args[index])
			default:
				normalized = append(normalized, arg+"=all")
			}
			continue
		}
		normalized = append(normalized, arg)
		if option := set.Lookup(name); option != nil && !hasValue && index+1 < len(args) {
			boolean, ok := option.Value.(interface{ IsBoolFlag() bool })
			if !ok || !boolean.IsBoolFlag() {
				index++
				normalized = append(normalized, args[index])
			}
		}
	}
	return normalized, defaultWrite
}

type topOptions struct {
	interval time.Duration
	count    int
	header   bool
	color    bool
	json     bool // sample당 한 줄 JSON을 쓰고 표와 중지 메시지는 생략한다
	process  bool // sample에 필터에 맞는 process 목록을 더한다
	// top은 필터가 없을 때 JSON sample에 더할 상위 process 수다. 0이면 더하지 않는다.
	top int
	// rank는 top의 순위 기준이다. 대시보드의 CPU와 memory 순위와 같다.
	rank     topView
	recorder *topRecorder
	filter   string
	// interfaces는 net 값에 더할 interface다. JSON에는 고른 interface마다의 값도 더한다.
	interfaces topInterfaceFilter
}

// topSample은 --json이 sample마다 한 줄로 내는 값이다. rate는 bytes/s와 percent다.
type topSample struct {
	NetworkLimits *networkHealthRate `json:"network_limits,omitempty"`
	Time          time.Time          `json:"time"`
	Hostname      string             `json:"hostname"`
	Cores         int                `json:"cores"`
	NetIn         float64            `json:"net_in_bytes_per_s"`
	NetOut        float64            `json:"net_out_bytes_per_s"`
	PacketsIn     float64            `json:"packets_in_per_s"`
	PacketsOut    float64            `json:"packets_out_per_s"`
	NetErrors     *float64           `json:"network_errors_per_s"`
	NetDrops      *float64           `json:"network_drops_per_s"`
	NetHealth     bool               `json:"network_health_supported"`
	// InterfaceFilter와 Interfaces는 --interface를 쓸 때만 나온다. Interfaces는 고른 interface 중 이 구간에 값이 있는 것이다.
	InterfaceFilter string                `json:"interface_filter,omitempty"`
	Interfaces      *[]topInterfaceSample `json:"interfaces,omitempty"`
	Load1           float64               `json:"load1"`
	CPUUser         float64               `json:"cpu_user_pct"`
	CPUSystem       float64               `json:"cpu_system_pct"`
	CPUIOWait       *float64              `json:"cpu_iowait_pct"`
	// CPUIOWaitOK는 iowait을 쟀는지다. 재지 못한 지표는 0 대신 null로 써서 '쟀더니 없었다'와 구분한다.
	CPUIOWaitOK   bool     `json:"cpu_iowait_supported"`
	DiskRead      float64  `json:"disk_read_bytes_per_s"`
	DiskWrite     float64  `json:"disk_write_bytes_per_s"`
	DiskIOPS      *float64 `json:"disk_iops"`
	DiskAwait     *float64 `json:"disk_await_ms"`
	DiskBusy      *float64 `json:"disk_busy_pct"`
	DiskHealth    bool     `json:"disk_health_supported"`
	DiskBusyOK    bool     `json:"disk_busy_supported"`
	PSICPU        *float64 `json:"psi_cpu_some_avg10_pct"`
	PSIMemory     *float64 `json:"psi_memory_some_avg10_pct"`
	PSIIO         *float64 `json:"psi_io_some_avg10_pct"`
	PSIMemoryFull *float64 `json:"psi_memory_full_avg10_pct"`
	PSIIOFull     *float64 `json:"psi_io_full_avg10_pct"`
	PSIValid      bool     `json:"psi_supported"`
	MemoryPct     float64  `json:"memory_pct"`
	// MemoryPressure는 macOS kernel의 memory 압박 단계(normal, warn, critical)다. 다른 host에서는 빠진다.
	MemoryPressure          string  `json:"memory_pressure,omitempty"`
	MemoryPressureSupported bool    `json:"memory_pressure_supported"`
	SwapOut                 float64 `json:"swap_out_bytes_per_s"`
	// SwapUsed와 SwapTotal은 지금 쓰는 swap과 swap 전체 크기다. 읽지 못하면 빠진다.
	SwapUsed  *uint64 `json:"swap_used_bytes,omitempty"`
	SwapTotal *uint64 `json:"swap_total_bytes,omitempty"`
	// Compressor는 macOS compressor가 쓰는 물리 memory다. Linux에서는 빠진다.
	Compressor *uint64 `json:"compressor_bytes,omitempty"`
	// ProcsCreated는 초당 만들어진 process 수다. 1초 sample 사이에 끝나는 build process는 목록에 없어 이 값으로만 보인다.
	// 첫 sample처럼 모르면 null이다. ProcsCreatedSource는 kernel(Linux)이나 pid_delta(macOS 근사)다.
	ProcsCreated       *float64 `json:"procs_created_per_s"`
	ProcsCreatedSource string   `json:"procs_created_source,omitempty"`
	// Processes는 --process를 쓸 때만 나온다. 맞는 process가 없으면 빈 배열이고, 첫 sample처럼 목록이 아직 없으면 빠진다.
	Processes *[]topProcessSample `json:"processes,omitempty"`
	// ProcessTotal은 필터에 맞은 process 전체의 합이다. Processes는 CPU 상위만 남기지만 합은 모두 센다.
	ProcessTotal *topProcessTotalSample `json:"process_total,omitempty"`
}

type topInterfaceSample struct {
	Name       string  `json:"name"`
	In         float64 `json:"in_bytes_per_s"`
	Out        float64 `json:"out_bytes_per_s"`
	PacketsIn  float64 `json:"packets_in_per_s"`
	PacketsOut float64 `json:"packets_out_per_s"`
	Errors     float64 `json:"errors_per_s"`
	Drops      float64 `json:"drops_per_s"`
}

func newTopInterfaceSamples(rates []netInterfaceRate) *[]topInterfaceSample {
	samples := make([]topInterfaceSample, 0, len(rates))
	for _, rate := range rates {
		samples = append(samples, topInterfaceSample{
			Name: rate.Name, In: roundTopValue(rate.In), Out: roundTopValue(rate.Out), PacketsIn: roundTopValue(rate.PacketsIn), PacketsOut: roundTopValue(rate.PacketsOut),
			Errors: roundTopValue(rate.Errors), Drops: roundTopValue(rate.Drops),
		})
	}
	return &samples
}

type topProcessTotalSample struct {
	Count    int     `json:"count"`
	CPU      float64 `json:"cpu_pct"`
	RSSBytes uint64  `json:"rss_bytes"`
	// MemoryBytes는 맞은 process의 memory_bytes 합이다. 하나라도 읽지 못했으면 빠진다.
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
	Threads     int     `json:"threads,omitempty"`
	// EBPF는 --ebpf가 직전 window 동안 센 값이다. 첫 관측 전에는 빠진다.
	EBPF *topProbeSample `json:"ebpf,omitempty"`
}

// topProcessSample은 --process가 sample마다 붙이는 process 한 개의 값이다. CPU는 core 하나가 100%다.
// Started, Threads, FDs, 디스크 rate는 읽을 수 없으면 빠진다. (pid, started)가 process 하나를 가리킨다.
type topProcessSample struct {
	PID     int        `json:"pid"`
	Started *time.Time `json:"started,omitempty"`
	Command string     `json:"command"`
	CPU     float64    `json:"cpu_pct"`
	// RSSBytes는 OS가 RSS를 주지 않는 process에서 빠진다. macOS kernel_task가 그렇다.
	RSSBytes *uint64 `json:"rss_bytes,omitempty"`
	// MemoryBytes는 macOS phys_footprint 또는 Linux RSS+VmSwap이다. 읽지 못한 process는 빠진다.
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
	Threads     int     `json:"threads,omitempty"`
	FDs         int     `json:"fds,omitempty"`
	// Limits는 실시간 출력에서 늘 있다. history는 수집하지 않은 limits의 자리 표시 문구를 행마다 저장하지 않으려고 뺀다.
	Limits    *topProcessLimits `json:"limits,omitempty"`
	DiskRead  *float64          `json:"disk_read_bytes_per_s,omitempty"`
	DiskWrite *float64          `json:"disk_write_bytes_per_s,omitempty"`
	// EBPF는 --ebpf가 직전 window 동안 센 값이다. 첫 관측 전에는 빠진다.
	EBPF *topProbeSample `json:"ebpf,omitempty"`
}

// topJSONProcesses는 --process 없이 --json이 sample마다 더하는 상위 process 수의 기본값이다.
// 스크립트와 agent가 대시보드 없이도 원인 process를 알 수 있게 한다.
const topJSONProcesses = 10

func newTopProcessSamples(processes []topProcess) *[]topProcessSample {
	samples := make([]topProcessSample, 0, len(processes))
	for _, process := range processes {
		sample := topProcessSample{PID: process.PID, Command: process.Command, CPU: roundTopValue(process.CPU), Threads: process.Threads, FDs: process.FDs, EBPF: newTopProbeSample(process.Probe)}
		sample.Limits = processLimitsOf(process)
		if !process.RSSMissing {
			rss := process.RSS
			sample.RSSBytes = &rss
		}
		if process.MemoryValid {
			memory := process.Memory
			sample.MemoryBytes = &memory
		}
		if !process.Started.IsZero() {
			started := process.Started.UTC().Truncate(time.Second)
			sample.Started = &started
		}
		if process.DiskValid {
			read, write := roundTopValue(process.DiskRead), roundTopValue(process.DiskWrite)
			sample.DiskRead, sample.DiskWrite = &read, &write
		}
		samples = append(samples, sample)
	}
	return &samples
}

func newTopProcessTotalSample(total topProcessTotal) *topProcessTotalSample {
	sample := &topProcessTotalSample{Count: total.Count, CPU: roundTopValue(total.CPU), RSSBytes: total.RSS, Threads: total.Threads, EBPF: newTopProbeSample(total.Probe)}
	if !total.MemoryEstimated {
		memory := total.Memory
		sample.MemoryBytes = &memory
	}
	return sample
}

// topProbeSample은 window 동안 센 run-queue 대기와 block I/O 지연이다. 지연은 ms이고, p95는 그 값이 든 구간의 위쪽 경계라 실제 값은 그 아래다.
// 개수가 0이면 평균과 p95는 빠진다. I/O는 요청을 낸 process로 잡으므로 writeback은 kworker로 잡힌다.
// io_supported와 p95_supported는 이 방법이 그 값을 셀 수 있는지다. source가 libproc이면 macOS의 누적 counter라 둘 다
// false이고, runq_count와 같은 값을 context_switches로도 내보낸다. unreadable은 권한이 없어 읽지 못한 process 수이고,
// 하나도 읽지 못했으면 runq 값도 빠진다.
type topProbeSample struct {
	Source          topProbeSource `json:"source"`
	WindowS         float64        `json:"window_s"`
	IOSupported     bool           `json:"io_supported"`
	P95Supported    bool           `json:"p95_supported"`
	Unreadable      int            `json:"unreadable,omitempty"`
	ContextSwitches *uint64        `json:"context_switches,omitempty"`
	RunqCount       *uint64        `json:"runq_count,omitempty"`
	RunqAvgMS       *float64       `json:"runq_avg_ms,omitempty"`
	RunqP95MS       *float64       `json:"runq_p95_ms,omitempty"`
	IOOps           *uint64        `json:"io_ops,omitempty"`
	IOBytes         *uint64        `json:"io_bytes,omitempty"`
	IOAvgMS         *float64       `json:"io_avg_ms,omitempty"`
	IOP95MS         *float64       `json:"io_p95_ms,omitempty"`
	// Waits는 대기 시간이 긴 syscall부터 topProbeWaitLimit개다. 셀 수 없으면 빠진다.
	Waits []topProbeWaitSample `json:"waits,omitempty"`
}

// topProbeWaitLimit은 JSON에 남기는 대기 syscall 수다.
const topProbeWaitLimit = 5

type topProbeWaitSample struct {
	Syscall string  `json:"syscall"`
	Count   uint64  `json:"count"`
	TotalMS float64 `json:"total_ms"`
}

func newTopProbeSample(stats *topProbeStats) *topProbeSample {
	if stats == nil {
		return nil
	}
	sample := &topProbeSample{Source: stats.Source, WindowS: roundTopValue(stats.Window.Seconds()), IOSupported: stats.IO != nil, P95Supported: stats.RunqHist != nil, Unreadable: stats.Unreadable}
	if stats.unmeasured() {
		return sample
	}
	rounded := func(value float64, ok bool) *float64 {
		if !ok {
			return nil
		}
		value = math.Round(value*1000) / 1000
		return &value
	}
	runq := stats.RunqCount
	sample.RunqCount = &runq
	if stats.Source == topProbeSourceLibproc {
		sample.ContextSwitches = &runq
	}
	sample.RunqAvgMS = rounded(topProbeAverageMS(stats.RunqSumNS, stats.RunqCount))
	if stats.RunqHist != nil {
		sample.RunqP95MS = rounded(topProbePercentileMS(*stats.RunqHist, 0.95))
	}
	if io := stats.IO; io != nil {
		ops, bytes := io.Count, io.Bytes
		sample.IOOps, sample.IOBytes = &ops, &bytes
		sample.IOAvgMS = rounded(topProbeAverageMS(io.SumNS, io.Count))
		sample.IOP95MS = rounded(topProbePercentileMS(io.Hist, 0.95))
	}
	for _, row := range topProbeWaitRows(stats.Waits, topProbeWaitLimit) {
		sample.Waits = append(sample.Waits, topProbeWaitSample{Syscall: row.Syscall, Count: row.Count, TotalMS: math.Round(float64(row.SumNS)/float64(time.Millisecond)*1000) / 1000})
	}
	return sample
}

func newTopSample(details hostDetails, at time.Time, rate resourceRate) topSample {
	sample := topSample{
		Time: at.UTC(), Hostname: details.Hostname, Cores: details.Cores, NetworkLimits: rate.NetworkHealth,
		NetIn: roundTopValue(rate.NetIn), NetOut: roundTopValue(rate.NetOut),
		PacketsIn: roundTopValue(rate.PacketsIn), PacketsOut: roundTopValue(rate.PacketsOut), NetHealth: rate.NetHealthValid,
		Load1: roundTopValue(rate.Load1), CPUUser: roundTopValue(rate.CPUUser), CPUSystem: roundTopValue(rate.CPUSystem), CPUIOWaitOK: rate.CPUIOWaitValid,
		DiskRead: roundTopValue(rate.DiskRead), DiskWrite: roundTopValue(rate.DiskWrite), DiskHealth: rate.DiskHealthValid, DiskBusyOK: rate.DiskBusyValid, PSIValid: rate.PSIValid, MemoryPct: roundTopValue(rate.MemoryPercent), SwapOut: roundTopValue(rate.SwapOut),
	}
	measured := func(ok bool, value float64) *float64 {
		if !ok {
			return nil
		}
		rounded := roundTopValue(value)
		return &rounded
	}
	sample.NetErrors, sample.NetDrops = measured(rate.NetHealthValid, rate.NetErrors), measured(rate.NetHealthValid, rate.NetDrops)
	sample.CPUIOWait = measured(rate.CPUIOWaitValid, rate.CPUIOWait)
	sample.DiskIOPS, sample.DiskAwait = measured(rate.DiskHealthValid, rate.DiskIOPS), measured(rate.DiskHealthValid, rate.DiskAwait)
	sample.DiskBusy = measured(rate.DiskBusyValid, rate.DiskBusy)
	sample.PSICPU, sample.PSIMemory, sample.PSIIO = measured(rate.PSIValid, rate.PSICPU), measured(rate.PSIValid, rate.PSIMemory), measured(rate.PSIValid, rate.PSIIO)
	sample.PSIMemoryFull, sample.PSIIOFull = measured(rate.PSIValid, rate.PSIMemoryFull), measured(rate.PSIValid, rate.PSIIOFull)
	if rate.MemoryPressure.known() {
		sample.MemoryPressure, sample.MemoryPressureSupported = rate.MemoryPressure.String(), true
	}
	if rate.SwapUsageValid {
		used, total := rate.SwapUsed, rate.SwapTotal
		sample.SwapUsed, sample.SwapTotal = &used, &total
	}
	if rate.CompressorValid {
		compressor := rate.Compressor
		sample.Compressor = &compressor
	}
	if rate.ProcsCreatedValid {
		created := roundTopValue(rate.ProcsCreated)
		sample.ProcsCreated, sample.ProcsCreatedSource = &created, rate.ProcsCreatedSource
	}
	return sample
}

// roundTopValue는 소수점 둘째 자리까지만 남겨 JSON 한 줄을 짧게 유지한다.
func roundTopValue(value float64) float64 {
	return math.Round(value*100) / 100
}

func streamTop(ctx context.Context, writer io.Writer, options topOptions) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.error.host", err))
		return 1
	}
	limits := newTopLimits(details.Cores, options.color)
	if options.header && options.color {
		fmt.Fprint(writer, richTopHeader(details))
	} else if options.header {
		printTopHeader(writer, details)
	}
	previous, err := collectResourceSnapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.error.resource", err))
		return 1
	}
	previous = options.interfaces.apply(previous, topInterfacePhysical)
	encoder := json.NewEncoder(writer)
	ticker := time.NewTicker(options.interval)
	defer ticker.Stop()
	printed := 0
	var failed <-chan struct{}
	if options.recorder != nil {
		failed = options.recorder.failed
	}
	for options.count == 0 || printed < options.count {
		select {
		case <-failed:
			return 1
		case <-ctx.Done():
			if !options.json {
				fmt.Fprintln(writer, "\n"+T("observe.top.stopped"))
			}
			return 0
		case <-ticker.C:
			current, err := collectResourceSnapshot()
			if err != nil {
				fmt.Fprintln(os.Stderr, T("observe.top.error.resource", err))
				return 1
			}
			current = options.interfaces.apply(current, topInterfacePhysical)
			rate := calculateRate(previous, current)
			if options.process || options.top > 0 {
				current.Processes, current.ProcessTotal, current.ProcessesValid, current.ProcessesAt = processSampler.refreshNowAt()
			} else if options.recorder != nil {
				current.Processes, current.ProcessTotal, current.ProcessesValid, current.ProcessesAt = processSampler.latestWithTotalAt()
			}
			if options.recorder != nil {
				if err := options.recorder.Record(newHistoryTopSample(details, previous, current, rate, options.filter)); err != nil {
					return 1
				}
			}
			if options.json {
				sample := newTopSample(details, current.TakenAt, rate)
				if options.interfaces.active() {
					sample.InterfaceFilter, sample.Interfaces = options.interfaces.String(), newTopInterfaceSamples(rate.NetInterfaces)
				}
				if options.process {
					if current.ProcessesValid {
						sample.Processes = newTopProcessSamples(current.Processes)
						sample.ProcessTotal = newTopProcessTotalSample(current.ProcessTotal)
					}
				} else if options.top > 0 && current.ProcessesValid {
					ranked := topRankedProcesses(current.Processes, options.rank)
					sample.Processes = newTopProcessSamples(ranked[:min(options.top, len(ranked))])
				}
				if err := encoder.Encode(sample); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
			} else {
				printTopRow(writer, current.TakenAt, rate, limits)
			}
			previous = current
			printed++
		}
	}
	return 0
}

const (
	// topTableWidth는 80칼럼 터미널에서 줄이 접히지 않도록 표 전체 폭을 고정한다.
	topTableWidth = 80
	// topMinInterval은 sampling이 host에 부담을 주지 않는 하한이다.
	topMinInterval = 200 * time.Millisecond
	topMaxInterval = time.Minute
	// topDashboardHistory는 대시보드가 되돌아볼 수 있는 row 수다.
	topDashboardHistory = 500
)

// topIntervalLadder는 +와 -로 옮겨 다니는 interval 단계다.
var topIntervalLadder = []time.Duration{
	topMinInterval, 500 * time.Millisecond, time.Second, 2 * time.Second,
	5 * time.Second, 10 * time.Second, 30 * time.Second, topMaxInterval,
}

const topColumnHeader = "│    time│net_in│net_out│ pk_in│pk_out│load│ usr%│ sys%│  i/o│dsk_r│dsk_w│mem_%│"

func printTopHeader(writer io.Writer, details hostDetails) {
	fmt.Fprint(writer, formatTopHeader(details))
}

// richTopHeader는 색을 쓰는 터미널의 표 머리다. 글자와 폭은 formatTopHeader와 같고, 테두리와 열 이름은 회색, 제목은 자홍 굵게다.
func richTopHeader(details hostDetails) string {
	lines := strings.Split(strings.TrimSuffix(formatTopHeader(details), "\n"), "\n")
	inside := strings.TrimSuffix(strings.TrimPrefix(lines[1], "│ "), " │")
	title := strings.TrimRight(inside, " ")
	lines[0], lines[2], lines[3] = richDim(lines[0]), richDim(lines[2]), richDim(lines[3])
	lines[1] = richDim("│ ") + richPaint(title, richMagenta, true) + inside[len(title):] + richDim(" │")
	return strings.Join(lines, "\n") + "\n"
}

func formatTopHeader(details hostDetails) string {
	title := fmt.Sprintf("🐰 %s <%s, %d cores, %s> 🐰", details.Hostname, details.Model, details.Cores, formatBytes(details.MemoryTotal))
	titleWidth := topDisplayWidth(title)
	width := max(topTableWidth, titleWidth+4)
	border := strings.Repeat("─", width-2)
	return fmt.Sprintf("╭%s╮\n│ %s%s │\n╰%s╯\n%s\n", border, title, strings.Repeat(" ", width-4-titleWidth), border, topColumnHeader)
}

type topThreshold struct{ warn, danger float64 }

// topLevel은 값이 임계치를 넘은 정도다. 색 문자열과 나눠 두어야 칸의 폭을 먼저 맞춘 뒤 색을 입힐 수 있다.
type topLevel int

const (
	topLevelNormal topLevel = iota
	topLevelWarn
	topLevelDanger
)

func (threshold topThreshold) level(value float64) topLevel {
	switch {
	case value >= threshold.danger:
		return topLevelDanger
	case value >= threshold.warn:
		return topLevelWarn
	}
	return topLevelNormal
}

// topLimits는 값의 위험도를 나누는 임계치다. load는 core 수에 비례하고, await는 ms,
// network는 초당 error·drop 수이며 나머지는 백분율이다.
type topLimits struct {
	load, cpu, io, memory, await, psi, network topThreshold
	// swap은 swap 사용률이다. kernel의 압박 단계가 normal이어도 swap이 거의 찰 수 있어 따로 경고한다.
	swap  topThreshold
	color bool
	// memoryTotal은 host memory 크기(byte)다. memory 증가 경고가 남은 양을 다 쓰는 시간을 구할 때 쓴다. 모르면 0이다.
	memoryTotal uint64
}

func newTopLimits(cores int, color bool) topLimits {
	if cores < 1 {
		cores = 1
	}
	return topLimits{
		load:   topThreshold{warn: 0.7 * float64(cores), danger: float64(cores)},
		cpu:    topThreshold{warn: 70, danger: 90},
		io:     topThreshold{warn: 10, danger: 25},
		memory: topThreshold{warn: 90, danger: 95},
		swap:   topThreshold{warn: 80, danger: 95},
		await:  topThreshold{warn: 20, danger: 50},
		psi:    topThreshold{warn: 10, danger: 25},
		// 가상 interface는 drop이 조금씩 꾸준히 난다. 1/s 미만은 잡음으로 보고, 50/s에서 memory 위험과 같은 무게가 된다.
		network: topThreshold{warn: 1, danger: 50},
		color:   color,
	}
}

func (limits topLimits) paint(text string, threshold topThreshold, value float64) string {
	return topPaint(text, threshold.level(value), limits.color)
}

// tint는 top 표의 paint다. paint는 info의 디스크 막대도 쓴다.
func (limits topLimits) tint(text string, threshold topThreshold, value float64) string {
	return topTint(text, threshold.level(value), limits.color)
}

// topPaint는 폭을 이미 맞춘 칸에 색을 입힌다.
// 정상값은 터미널 기본색으로 두어야 경고와 위험만 눈에 들어온다. 색은 topTint와 같은 rich 팔레트다.
func topPaint(text string, level topLevel, color bool) string {
	return topTint(text, level, color)
}

// topTint는 top의 topPaint다. 폭을 이미 맞춘 칸에 경고는 노랑, 위험은 빨강을 rich 팔레트로 입힌다.
func topTint(text string, level topLevel, color bool) string {
	if !color || level == topLevelNormal {
		return text
	}
	return richPaint(text, topLevelColor(level), false)
}

func topLevelColor(level topLevel) string {
	if level == topLevelDanger {
		return richRed
	}
	return richYellow
}

func printTopRow(writer io.Writer, at time.Time, rate resourceRate, limits topLimits) {
	fmt.Fprintln(writer, formatTopRow(at, rate, limits))
}

func formatTopRow(at time.Time, rate resourceRate, limits topLimits) string {
	return fmt.Sprintf("│%8s│%6s│%7s│%6.0f│%6.0f│%s│%s│%s│%s│%5s│%5s│%s│",
		at.Format("15:04:05"), formatRate(rate.NetIn), formatRate(rate.NetOut), rate.PacketsIn, rate.PacketsOut,
		limits.tint(fmt.Sprintf("%4.1f", rate.Load1), limits.load, rate.Load1),
		limits.tint(fmt.Sprintf("%5.1f", rate.CPUUser), limits.cpu, rate.CPUUser),
		limits.tint(fmt.Sprintf("%5.1f", rate.CPUSystem), limits.cpu, rate.CPUSystem),
		limits.tint(fmt.Sprintf("%5.2f", rate.CPUIOWait), limits.io, rate.CPUIOWait),
		formatRate(rate.DiskRead), formatRate(rate.DiskWrite),
		limits.tint(fmt.Sprintf("%5.1f", rate.MemoryPercent), limits.memory, rate.MemoryPercent))
}

// topDisplayWidth는 두 칸을 차지하는 🐰를 반영한 터미널 표시 폭이다.
func topDisplayWidth(value string) int {
	return len([]rune(value)) + strings.Count(value, "🐰")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
