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
	return streamTop(ctx, os.Stdout, *interval, *count, !*noHeader)
}

func streamTop(ctx context.Context, writer io.Writer, interval time.Duration, count int, header bool) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintf(os.Stderr, "host 정보를 읽지 못했습니다: %v\n", err)
		return 1
	}
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
			printTopRow(writer, current.TakenAt, calculateRate(previous, current))
			previous = current
			printed++
		}
	}
	return 0
}

func printTopHeader(writer io.Writer, details hostDetails) {
	title := fmt.Sprintf("🐰 %s <%s, %d cores, %s> 🐰", details.Hostname, details.Model, details.Cores, formatBytes(details.MemoryTotal))
	width := max(111, len([]rune(title))+4)
	fmt.Fprintf(writer, "╭%s╮\n│ %-*s │\n╰%s╯\n", strings.Repeat("─", width-2), width-4, title, strings.Repeat("─", width-2))
	fmt.Fprintln(writer, "│    time│   net_in│  net_out│     pk_in│    pk_out│ load│   usr│   sys│   i/o│   disk_rd│   disk_wr│ mem_%│")
}

func printTopRow(writer io.Writer, at time.Time, rate resourceRate) {
	fmt.Fprintf(writer, "│%8s│%9s│%9s│%10.0f│%10.0f│%5.1f│%6.1f%%│%6.1f%%│%6.2f│%10s│%10s│%6.1f%%│\n",
		at.Format("15:04:05"), formatRate(rate.NetIn), formatRate(rate.NetOut), rate.PacketsIn, rate.PacketsOut, rate.Load1, rate.CPUUser, rate.CPUSystem, rate.CPUIOWait, formatRate(rate.DiskRead), formatRate(rate.DiskWrite), rate.MemoryPercent)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
