package edc

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc top [--interval 1s] [--count N]")
		return 2
	}
	if *interval < 200*time.Millisecond {
		fmt.Fprintln(os.Stderr, "--interval은 200ms 이상이어야 합니다")
		return 2
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, "--count는 0 이상이어야 합니다")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return streamTop(ctx, os.Stdout, *interval, *count, !*noHeader, isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "")
}

func streamTop(ctx context.Context, writer io.Writer, interval time.Duration, count int, header, color bool) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintf(os.Stderr, "host 정보를 읽지 못했습니다: %v\n", err)
		return 1
	}
	limits := newTopLimits(details.Cores, color)
	if header {
		printTopHeader(writer, details)
	}
	previous, err := collectResourceSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resource를 읽지 못했습니다: %v\n", err)
		return 1
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	printed := 0
	for count == 0 || printed < count {
		select {
		case <-ctx.Done():
			fmt.Fprintln(writer, "\n중지됨")
			return 0
		case <-ticker.C:
			current, err := collectResourceSnapshot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "resource를 읽지 못했습니다: %v\n", err)
				return 1
			}
			printTopRow(writer, current.TakenAt, calculateRate(previous, current), limits)
			previous = current
			printed++
		}
	}
	return 0
}

// topTableWidth는 80칼럼 터미널에서 줄이 접히지 않도록 표 전체 폭을 고정한다.
const topTableWidth = 80

func printTopHeader(writer io.Writer, details hostDetails) {
	title := fmt.Sprintf("🐰 %s <%s, %d cores, %s> 🐰", details.Hostname, details.Model, details.Cores, formatBytes(details.MemoryTotal))
	titleWidth := topDisplayWidth(title)
	width := max(topTableWidth, titleWidth+4)
	border := strings.Repeat("─", width-2)
	fmt.Fprintf(writer, "╭%s╮\n│ %s%s │\n╰%s╯\n", border, title, strings.Repeat(" ", width-4-titleWidth), border)
	fmt.Fprintln(writer, "│    time│net_in│net_out│ pk_in│pk_out│load│ usr%│ sys%│  i/o│dsk_r│dsk_w│mem_%│")
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
	fmt.Fprintf(writer, "│%8s│%6s│%7s│%6.0f│%6.0f│%s│%s│%s│%s│%5s│%5s│%s│\n",
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
