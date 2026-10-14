package edc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// trace exec은 process의 시작(exec), 끝(exit), 신호(signal), OOM killer의 선택(oom)을 본다. 네 event는 모두 같은 record 모양으로 오고,
// 이 파일은 record 읽기와 요약처럼 kernel이 필요 없는 부분이다. BPF 로딩은 trace_exec.go에 있다.

const (
	execEventExec   = "exec"
	execEventExit   = "exit"
	execEventSignal = "signal"
	execEventOOM    = "oom"
	// execRecordSize는 trace_exec_bpf.c의 exec_record 전체 크기다. args 뒤 4 byte padding 다음에 OOM의 메모리 칸 넷이 온다.
	execRecordSize = 376
	execArgsMax    = 256
	// execShortLifetime보다 짧게 살다 끝난 process는 요약에서 따로 센다. 짧은 process가 쏟아지는 것이 exec 폭풍이다.
	execShortLifetime = time.Second
	execGroupByParent = "parent"
	execGroupByCgroup = "cgroup"
)

var execSignalNames = []string{"", "HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE", "KILL", "USR1", "SEGV", "USR2", "PIPE", "ALRM", "TERM", "STKFLT", "CHLD", "CONT", "STOP", "TSTP", "TTIN", "TTOU", "URG", "XCPU", "XFSZ", "VTALRM", "PROF", "WINCH", "IO", "PWR", "SYS"}

// execDefaultSignals는 기본으로 기록하는 신호다. process를 끝내거나 core를 남기는 신호이고, 타이머(ALRM)와 자식 알림(CHLD)처럼
// 늘 오가는 신호는 뺀다. --signal all이 모두 켠다.
var execDefaultSignals = []string{"HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE", "KILL", "USR1", "SEGV", "USR2", "PIPE", "TERM", "STKFLT", "XCPU", "XFSZ", "SYS"}

// execSignalResults는 kernel의 TRACE_SIGNAL_* 순서다.
var execSignalResults = []string{"delivered", "ignored", "already_pending", "overflow_fail", "lose_info"}

type execEvent struct {
	TimestampNS   uint64   `json:"timestamp_ns"`
	BootTimeNS    uint64   `json:"boot_time_ns"`
	Event         string   `json:"event"`
	PID           uint32   `json:"pid"`
	Process       string   `json:"process"`
	PPID          uint32   `json:"ppid,omitempty"`
	Parent        string   `json:"parent,omitempty"`
	UID           uint32   `json:"uid"`
	CgroupID      uint64   `json:"cgroup_id"`
	Args          string   `json:"args,omitempty"`
	ArgsTruncated bool     `json:"args_truncated,omitempty"`
	ExitCode      *int32   `json:"exit_code,omitempty"`
	Signal        string   `json:"signal,omitempty"`
	CoreDumped    bool     `json:"core_dumped,omitempty"`
	LifetimeMS    *float64 `json:"lifetime_ms,omitempty"`
	SenderPID     *uint32  `json:"sender_pid,omitempty"`
	Sender        string   `json:"sender,omitempty"`
	FromKernel    bool     `json:"from_kernel,omitempty"`
	Result        string   `json:"result,omitempty"`
	OOMKilled     bool     `json:"oom_killed,omitempty"`
	// 아래 칸은 oom event에만 있다. 5.15 kernel의 tracepoint는 victim의 pid만 주므로 이 칸들과 process 이름이 빈다.
	TotalVMKB   *uint64 `json:"total_vm_kb,omitempty"`
	AnonRSSKB   *uint64 `json:"anon_rss_kb,omitempty"`
	FileRSSKB   *uint64 `json:"file_rss_kb,omitempty"`
	ShmemRSSKB  *uint64 `json:"shmem_rss_kb,omitempty"`
	OOMScoreAdj *int32  `json:"oom_score_adj,omitempty"`
}

type execSummary struct {
	TimestampNS     uint64 `json:"timestamp_ns"`
	Event           string `json:"event"`
	EventCount      uint64 `json:"event_count"`
	LostEvents      uint64 `json:"lost_events"`
	FilteredSignals uint64 `json:"filtered_signals"`
	ArgsTruncated   uint64 `json:"args_truncated"`
}

func execSignalName(number int32) string {
	if number > 0 && int(number) < len(execSignalNames) {
		return "SIG" + execSignalNames[number]
	}
	return fmt.Sprintf("SIG%d", number)
}

func execSignalNumber(name string) (int32, bool) {
	upper := strings.ToUpper(strings.TrimSpace(name))
	upper = strings.TrimPrefix(upper, "SIG")
	for number, candidate := range execSignalNames {
		if number > 0 && candidate == upper {
			return int32(number), true
		}
	}
	if number, err := strconv.Atoi(upper); err == nil && number > 0 && number < 64 {
		return int32(number), true
	}
	return 0, false
}

// parseExecSignals는 --signal 값을 bit 집합으로 바꾼다. 비면 기본 신호, all은 1부터 63까지, none은 0이다.
func parseExecSignals(value string) (uint64, error) {
	names := execDefaultSignals
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
	case "all":
		return (1 << 64) - 2, nil
	case "none":
		return 0, nil
	default:
		names = strings.Split(value, ",")
	}
	var mask uint64
	for _, name := range names {
		number, ok := execSignalNumber(name)
		if !ok {
			return 0, fmt.Errorf("unknown signal %q", strings.TrimSpace(name))
		}
		mask |= 1 << uint(number)
	}
	return mask, nil
}

func parseExecRecord(sample []byte, clockOffset int64) (execEvent, bool) {
	if len(sample) < execRecordSize {
		return execEvent{}, false
	}
	kind, flags := sample[80], sample[81]
	argsLen := int(binary.LittleEndian.Uint16(sample[82:84]))
	if argsLen > execArgsMax {
		return execEvent{}, false
	}
	boot := binary.LittleEndian.Uint64(sample[0:8])
	event := execEvent{
		TimestampNS: uint64(int64(boot) + clockOffset),
		BootTimeNS:  boot,
		CgroupID:    binary.LittleEndian.Uint64(sample[8:16]),
		PID:         binary.LittleEndian.Uint32(sample[24:28]),
		UID:         binary.LittleEndian.Uint32(sample[36:40]),
		Process:     string(execTrimNUL(sample[48:64])),
	}
	code := int32(binary.LittleEndian.Uint32(sample[40:44]))
	signal := int32(binary.LittleEndian.Uint32(sample[44:48]))
	other := string(execTrimNUL(sample[64:80]))
	switch kind {
	case 0:
		event.Event = execEventExec
		event.PPID, event.Parent = binary.LittleEndian.Uint32(sample[28:32]), other
		event.Args = execArgsText(sample[84 : 84+argsLen])
		event.ArgsTruncated = flags&1 != 0
	case 1:
		event.Event = execEventExit
		event.PPID, event.Parent = binary.LittleEndian.Uint32(sample[28:32]), other
		exitCode := code
		event.ExitCode = &exitCode
		if signal != 0 {
			event.Signal = execSignalName(signal)
		}
		event.CoreDumped = flags&2 != 0
		event.OOMKilled = flags&8 != 0
		lifetime := float64(binary.LittleEndian.Uint64(sample[16:24])) / float64(time.Millisecond)
		event.LifetimeMS = &lifetime
	case 2:
		event.Event = execEventSignal
		event.Signal = execSignalName(code)
		sender := binary.LittleEndian.Uint32(sample[32:36])
		event.SenderPID = &sender
		event.Sender = other
		event.FromKernel = flags&4 != 0
		if signal >= 0 && int(signal) < len(execSignalResults) {
			event.Result = execSignalResults[signal]
		} else {
			event.Result = fmt.Sprintf("result_%d", signal)
		}
	case 3:
		// OOM을 일으킨 task는 sender 칸으로 온다. 신호 event처럼 "누가 이 process를 끝냈는가"의 답이다.
		event.Event = execEventOOM
		sender := binary.LittleEndian.Uint32(sample[32:36])
		event.SenderPID = &sender
		event.Sender = other
		event.FromKernel = true
		if flags&16 != 0 {
			values := make([]uint64, 4)
			for index := range values {
				values[index] = binary.LittleEndian.Uint64(sample[344+8*index:])
			}
			adj := code
			event.TotalVMKB, event.AnonRSSKB, event.FileRSSKB, event.ShmemRSSKB, event.OOMScoreAdj = &values[0], &values[1], &values[2], &values[3], &adj
		}
	default:
		return execEvent{}, false
	}
	return event, true
}

func execTrimNUL(value []byte) []byte {
	if index := bytes.IndexByte(value, 0); index >= 0 {
		return value[:index]
	}
	return value
}

// execArgsText는 NUL로 나뉜 argv를 공백으로 잇는다. 상대가 보낸 값이 아니라 이 호스트의 명령줄이지만, 제어 문자는 화면을
// 흔들 수 있어 \xNN으로 바꾼다.
func execArgsText(raw []byte) string {
	trimmed := bytes.TrimRight(raw, "\x00")
	if len(trimmed) == 0 {
		return ""
	}
	fields := bytes.Split(trimmed, []byte{0})
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, execPrintable(field))
	}
	return strings.Join(parts, " ")
}

func execPrintable(value []byte) string {
	var out strings.Builder
	for _, b := range value {
		if b < 0x20 || b == 0x7f {
			fmt.Fprintf(&out, `\x%02x`, b)
			continue
		}
		out.WriteByte(b)
	}
	return out.String()
}

// execEventDetail은 스크롤 행과 요약 뒤에 붙는 한 줄 설명이다. event마다 다른 칸을 한 칸으로 모은다.
func execEventDetail(event execEvent) string {
	switch event.Event {
	case execEventExec:
		text := fmt.Sprintf("ppid %d %s · %s", event.PPID, emptyAs(event.Parent, "-"), emptyAs(event.Args, "-"))
		if event.ArgsTruncated {
			text += " …"
		}
		return text
	case execEventExit:
		parts := []string{}
		if event.ExitCode != nil {
			parts = append(parts, fmt.Sprintf("code %d", *event.ExitCode))
		}
		if event.Signal != "" {
			parts = append(parts, event.Signal)
		}
		if event.CoreDumped {
			parts = append(parts, "core")
		}
		if event.OOMKilled {
			parts = append(parts, "oom-killed")
		}
		if event.LifetimeMS != nil {
			parts = append(parts, "after "+execLifetimeText(*event.LifetimeMS))
		}
		return strings.Join(parts, " · ")
	case execEventSignal:
		sender := "kernel"
		if event.SenderPID != nil && *event.SenderPID != 0 {
			sender = fmt.Sprintf("%s(%d)", emptyAs(event.Sender, "-"), *event.SenderPID)
			if event.FromKernel {
				sender = "kernel via " + sender
			}
		}
		return fmt.Sprintf("%s ← %s · %s", event.Signal, sender, event.Result)
	case execEventOOM:
		text := "oom victim ← " + execOOMTrigger(event)
		if event.AnonRSSKB != nil {
			text += fmt.Sprintf(" · anon %s file %s shmem %s · adj %d", traceBytes(*event.AnonRSSKB*1024), traceBytes(*event.FileRSSKB*1024), traceBytes(*event.ShmemRSSKB*1024), *event.OOMScoreAdj)
		}
		return text
	}
	return ""
}

func execOOMTrigger(event execEvent) string {
	if event.SenderPID == nil {
		return "-"
	}
	return fmt.Sprintf("%s(%d)", emptyAs(event.Sender, "-"), *event.SenderPID)
}

func execLifetimeText(ms float64) string {
	switch {
	case ms >= 1000:
		return fmt.Sprintf("%.1fs", ms/1000)
	case ms >= 1:
		return fmt.Sprintf("%.0fms", ms)
	default:
		return fmt.Sprintf("%.2fms", ms)
	}
}

func execGroupKey(groupBy string, event execEvent) string {
	switch groupBy {
	case traceGroupByEvent:
		return event.Event
	case execGroupByParent:
		if event.Event == execEventSignal || event.Event == execEventOOM {
			return emptyAs(event.Sender, "-")
		}
		return emptyAs(event.Parent, "-")
	case execGroupByCgroup:
		return strconv.FormatUint(event.CgroupID, 10)
	}
	return emptyAs(event.Process, "-")
}

// execProcessMatches는 --process의 쉼표 목록과 process 이름이나 PID를 맞춘다. 신호는 받는 쪽이 대상이다.
func execProcessMatches(filter string, event execEvent) bool {
	if filter == "" {
		return true
	}
	for _, item := range strings.Split(filter, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == event.Process || item == strconv.FormatUint(uint64(event.PID), 10) {
			return true
		}
	}
	return false
}

type execReportRow struct {
	Group        string
	Execs        uint64
	Exits        uint64
	NonZeroExits uint64
	Signaled     uint64
	ShortLived   uint64
	OOMKilled    uint64
	lifetimes    []float64
}

type execReportOutputRow struct {
	Group         string   `json:"group"`
	Execs         uint64   `json:"execs"`
	Exits         uint64   `json:"exits"`
	NonZeroExits  uint64   `json:"nonzero_exits"`
	Signaled      uint64   `json:"signaled"`
	ShortLived    uint64   `json:"short_lived"`
	OOMKilled     uint64   `json:"oom_killed"`
	AvgLifetimeMS *float64 `json:"avg_lifetime_ms"`
	MaxLifetimeMS *float64 `json:"max_lifetime_ms"`
}

type execSignalRow struct {
	Signal     string `json:"signal"`
	Sender     string `json:"sender"`
	Target     string `json:"target"`
	Result     string `json:"result"`
	FromKernel bool   `json:"from_kernel"`
	Count      uint64 `json:"count"`
}

type execReport struct {
	DurationMS      int64                 `json:"duration_ms"`
	GroupBy         string                `json:"group_by,omitempty"`
	ShortLifetimeMS int64                 `json:"short_lifetime_ms"`
	Rows            []execReportOutputRow `json:"rows"`
	Signals         []execSignalRow       `json:"signals"`
	OOMs            []execEvent           `json:"ooms"`
	Summary         execSummary           `json:"summary"`
}

// execAggregator는 event를 group 행과 신호 행으로 쌓는다. 신호 행은 보낸 process, 받은 process, 신호, 결과가 같은 것을 모은다.
type execAggregator struct {
	groupBy string
	rows    map[string]*execReportRow
	signals map[string]*execSignalRow
	ooms    []execEvent
}

func newExecAggregator(groupBy string) *execAggregator {
	return &execAggregator{groupBy: groupBy, rows: map[string]*execReportRow{}, signals: map[string]*execSignalRow{}}
}

func (aggregator *execAggregator) add(event execEvent) {
	key := execGroupKey(aggregator.groupBy, event)
	row := aggregator.rows[key]
	if row == nil {
		row = &execReportRow{Group: key}
		aggregator.rows[key] = row
	}
	switch event.Event {
	case execEventExec:
		row.Execs++
	case execEventExit:
		row.Exits++
		if event.ExitCode != nil && *event.ExitCode != 0 {
			row.NonZeroExits++
		}
		if event.Signal != "" {
			row.Signaled++
		}
		if event.OOMKilled {
			row.OOMKilled++
		}
		if event.LifetimeMS != nil {
			row.lifetimes = append(row.lifetimes, *event.LifetimeMS)
			if *event.LifetimeMS < float64(execShortLifetime/time.Millisecond) {
				row.ShortLived++
			}
		}
	case execEventSignal:
		sender := "kernel"
		if event.SenderPID != nil && *event.SenderPID != 0 {
			sender = emptyAs(event.Sender, "-")
		}
		signalKey := strings.Join([]string{event.Signal, sender, event.Process, event.Result, strconv.FormatBool(event.FromKernel)}, "\x00")
		signal := aggregator.signals[signalKey]
		if signal == nil {
			signal = &execSignalRow{Signal: event.Signal, Sender: sender, Target: emptyAs(event.Process, "-"), Result: event.Result, FromKernel: event.FromKernel}
			aggregator.signals[signalKey] = signal
		}
		signal.Count++
	case execEventOOM:
		aggregator.ooms = append(aggregator.ooms, event)
	}
}

func (aggregator *execAggregator) report(duration time.Duration, summary execSummary) execReport {
	report := execReport{DurationMS: duration.Milliseconds(), GroupBy: aggregator.groupBy, ShortLifetimeMS: execShortLifetime.Milliseconds(), Summary: summary, Rows: []execReportOutputRow{}, Signals: []execSignalRow{}, OOMs: append([]execEvent{}, aggregator.ooms...)}
	if report.DurationMS < 1 {
		report.DurationMS = 1
	}
	for _, row := range aggregator.rows {
		output := execReportOutputRow{Group: row.Group, Execs: row.Execs, Exits: row.Exits, NonZeroExits: row.NonZeroExits, Signaled: row.Signaled, ShortLived: row.ShortLived, OOMKilled: row.OOMKilled}
		if len(row.lifetimes) > 0 {
			var total, maximum float64
			for _, value := range row.lifetimes {
				total += value
				if value > maximum {
					maximum = value
				}
			}
			average := total / float64(len(row.lifetimes))
			output.AvgLifetimeMS, output.MaxLifetimeMS = &average, &maximum
		}
		report.Rows = append(report.Rows, output)
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		left, right := report.Rows[i], report.Rows[j]
		if left.Execs+left.Exits != right.Execs+right.Exits {
			return left.Execs+left.Exits > right.Execs+right.Exits
		}
		return left.Group < right.Group
	})
	for _, signal := range aggregator.signals {
		report.Signals = append(report.Signals, *signal)
	}
	sort.Slice(report.Signals, func(i, j int) bool {
		left, right := report.Signals[i], report.Signals[j]
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		return left.Signal+left.Sender+left.Target < right.Signal+right.Sender+right.Target
	})
	return report
}

func (report execReport) print() {
	fmt.Printf("EXEC TRACE  duration %dms  short-lived under %dms\n", report.DurationMS, report.ShortLifetimeMS)
	fmt.Printf("%-24s %6s %6s %8s %8s %6s %4s %10s %10s\n", "GROUP", "EXEC", "EXIT", "NONZERO", "SIGNALED", "SHORT", "OOM", "AVG LIFE", "MAX LIFE")
	for _, row := range report.Rows {
		fmt.Printf("%-24s %6d %6d %8d %8d %6d %4d %10s %10s\n", row.Group, row.Execs, row.Exits, row.NonZeroExits, row.Signaled, row.ShortLived, row.OOMKilled, execOptionalLifetime(row.AvgLifetimeMS), execOptionalLifetime(row.MaxLifetimeMS))
	}
	if len(report.Signals) > 0 {
		fmt.Printf("%-10s %-20s %-20s %-18s %6s\n", "SIGNAL", "SENDER", "TARGET", "RESULT", "COUNT")
		for _, row := range report.Signals {
			result := row.Result
			if row.FromKernel {
				result += " (kernel)"
			}
			fmt.Printf("%-10s %-20s %-20s %-18s %6d\n", row.Signal, row.Sender, row.Target, result, row.Count)
		}
	}
	if len(report.OOMs) > 0 {
		fmt.Printf("%-12s %-8s %-16s %-24s %s\n", "OOM", "PID", "VICTIM", "TRIGGER", "MEMORY")
		for _, event := range report.OOMs {
			memory := "-"
			if event.AnonRSSKB != nil {
				memory = fmt.Sprintf("anon %s file %s shmem %s adj %d", traceBytes(*event.AnonRSSKB*1024), traceBytes(*event.FileRSSKB*1024), traceBytes(*event.ShmemRSSKB*1024), *event.OOMScoreAdj)
			}
			fmt.Printf("%-12s %-8d %-16s %-24s %s\n", time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000"), event.PID, emptyAs(event.Process, "-"), execOOMTrigger(event), memory)
		}
	}
	fmt.Printf("events %d  ring_lost %d  filtered_signals %d  args_truncated %d\n", report.Summary.EventCount, report.Summary.LostEvents, report.Summary.FilteredSignals, report.Summary.ArgsTruncated)
}

func execOptionalLifetime(value *float64) string {
	if value == nil {
		return "-"
	}
	return execLifetimeText(*value)
}
