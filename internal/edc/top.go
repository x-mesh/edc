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
	"strings"
	"syscall"
	"time"
)

func runTop(args []string) int {
	set := flag.NewFlagSet("top", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	interval := set.Duration("interval", time.Second, "sampling interval")
	count := set.Int("count", 0, "출력 row 수, 0은 계속 실행")
	noHeader := set.Bool("no-header", false, "host와 column header 생략")
	jsonPath := set.String("json", "", "sample당 한 줄 JSON 출력 경로, stdout은 -")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc top [--interval 1s] [--count N] [--json <path|->]")
		return 2
	}
	if *interval < topMinInterval {
		fmt.Fprintf(os.Stderr, "--interval은 %s 이상이어야 합니다\n", topMinInterval)
		return 2
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, "--count는 0 이상이어야 합니다")
		return 2
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
	jsonOutput := *jsonPath != ""
	// 대시보드는 무한 실행에만 쓴다. --count와 --json은 표와 JSON을 그대로 흘려 보낸다.
	if !jsonOutput && *count == 0 && liveTerminal() {
		return runTopDashboard(*interval)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return streamTop(ctx, writer, topOptions{
		interval: *interval, count: *count, json: jsonOutput,
		header: !*noHeader && !jsonOutput,
		color:  !jsonOutput && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "",
	})
}

type topOptions struct {
	interval time.Duration
	count    int
	header   bool
	color    bool
	json     bool // sample당 한 줄 JSON을 쓰고 표와 중지 메시지는 생략한다
}

// topSample은 --json이 sample마다 한 줄로 내는 값이다. rate는 bytes/s와 percent다.
type topSample struct {
	Time       time.Time `json:"time"`
	Hostname   string    `json:"hostname"`
	Cores      int       `json:"cores"`
	NetIn      float64   `json:"net_in_bytes_per_s"`
	NetOut     float64   `json:"net_out_bytes_per_s"`
	PacketsIn  float64   `json:"packets_in_per_s"`
	PacketsOut float64   `json:"packets_out_per_s"`
	Load1      float64   `json:"load1"`
	CPUUser    float64   `json:"cpu_user_pct"`
	CPUSystem  float64   `json:"cpu_system_pct"`
	CPUIOWait  float64   `json:"cpu_iowait_pct"`
	DiskRead   float64   `json:"disk_read_bytes_per_s"`
	DiskWrite  float64   `json:"disk_write_bytes_per_s"`
	MemoryPct  float64   `json:"memory_pct"`
}

func newTopSample(details hostDetails, at time.Time, rate resourceRate) topSample {
	return topSample{
		Time: at.UTC(), Hostname: details.Hostname, Cores: details.Cores,
		NetIn: roundTopValue(rate.NetIn), NetOut: roundTopValue(rate.NetOut),
		PacketsIn: roundTopValue(rate.PacketsIn), PacketsOut: roundTopValue(rate.PacketsOut),
		Load1: roundTopValue(rate.Load1), CPUUser: roundTopValue(rate.CPUUser), CPUSystem: roundTopValue(rate.CPUSystem), CPUIOWait: roundTopValue(rate.CPUIOWait),
		DiskRead: roundTopValue(rate.DiskRead), DiskWrite: roundTopValue(rate.DiskWrite), MemoryPct: roundTopValue(rate.MemoryPercent),
	}
}

// roundTopValue는 소수점 둘째 자리까지만 남겨 JSON 한 줄을 짧게 유지한다.
func roundTopValue(value float64) float64 {
	return math.Round(value*100) / 100
}

func streamTop(ctx context.Context, writer io.Writer, options topOptions) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintf(os.Stderr, "host 정보를 읽지 못했습니다: %v\n", err)
		return 1
	}
	limits := newTopLimits(details.Cores, options.color)
	if options.header {
		printTopHeader(writer, details)
	}
	previous, err := collectResourceSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resource를 읽지 못했습니다: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(writer)
	ticker := time.NewTicker(options.interval)
	defer ticker.Stop()
	printed := 0
	for options.count == 0 || printed < options.count {
		select {
		case <-ctx.Done():
			if !options.json {
				fmt.Fprintln(writer, "\n중지됨")
			}
			return 0
		case <-ticker.C:
			current, err := collectResourceSnapshot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "resource를 읽지 못했습니다: %v\n", err)
				return 1
			}
			rate := calculateRate(previous, current)
			if options.json {
				if err := encoder.Encode(newTopSample(details, current.TakenAt, rate)); err != nil {
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
	// topDashboardFixedLines는 header 3줄, column header 1줄, 상태줄 1줄이다.
	topDashboardFixedLines = 5
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

func formatTopHeader(details hostDetails) string {
	title := fmt.Sprintf("🐰 %s <%s, %d cores, %s> 🐰", details.Hostname, details.Model, details.Cores, formatBytes(details.MemoryTotal))
	titleWidth := topDisplayWidth(title)
	width := max(topTableWidth, titleWidth+4)
	border := strings.Repeat("─", width-2)
	return fmt.Sprintf("╭%s╮\n│ %s%s │\n╰%s╯\n%s\n", border, title, strings.Repeat(" ", width-4-titleWidth), border, topColumnHeader)
}

const (
	topColorNormal = "\033[97m"
	topColorWarn   = "\033[38;5;208m"
	topColorDanger = "\033[91m"
	topColorReset  = "\033[0m"
)

type topThreshold struct{ warn, danger float64 }

// topLimits는 값의 위험도를 나누는 임계치다. load만 core 수에 비례하고 나머지는 백분율이다.
type topLimits struct {
	load, cpu, io, memory topThreshold
	color                 bool
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
		color:  color,
	}
}

func (limits topLimits) paint(text string, threshold topThreshold, value float64) string {
	if !limits.color {
		return text
	}
	code := topColorNormal
	switch {
	case value >= threshold.danger:
		code = topColorDanger
	case value >= threshold.warn:
		code = topColorWarn
	}
	return code + text + topColorReset
}

func printTopRow(writer io.Writer, at time.Time, rate resourceRate, limits topLimits) {
	fmt.Fprintln(writer, formatTopRow(at, rate, limits))
}

func formatTopRow(at time.Time, rate resourceRate, limits topLimits) string {
	return fmt.Sprintf("│%8s│%6s│%7s│%6.0f│%6.0f│%s│%s│%s│%s│%5s│%5s│%s│",
		at.Format("15:04:05"), formatRate(rate.NetIn), formatRate(rate.NetOut), rate.PacketsIn, rate.PacketsOut,
		limits.paint(fmt.Sprintf("%4.1f", rate.Load1), limits.load, rate.Load1),
		limits.paint(fmt.Sprintf("%5.1f", rate.CPUUser), limits.cpu, rate.CPUUser),
		limits.paint(fmt.Sprintf("%5.1f", rate.CPUSystem), limits.cpu, rate.CPUSystem),
		limits.paint(fmt.Sprintf("%5.2f", rate.CPUIOWait), limits.io, rate.CPUIOWait),
		formatRate(rate.DiskRead), formatRate(rate.DiskWrite),
		limits.paint(fmt.Sprintf("%5.1f", rate.MemoryPercent), limits.memory, rate.MemoryPercent))
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
