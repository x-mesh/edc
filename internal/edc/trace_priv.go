package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tracePrivUsage = "edc trace priv [--denied] [--process <list>] [--raw | --json <path>]"
	privTraceRows  = 20
	privEventCap   = "cap_denied"
	privEventCred  = "cred"
	privEventMod   = "module"
)

// privCapabilities는 include/uapi/linux/capability.h의 번호 순서다.
var privCapabilities = []string{
	"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID", "SETPCAP", "LINUX_IMMUTABLE",
	"NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER", "SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE",
	"SYS_PACCT", "SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE",
	"AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG", "WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF",
	"CHECKPOINT_RESTORE",
}

func privCapabilityName(cap int32) string {
	if cap >= 0 && int(cap) < len(privCapabilities) {
		return "CAP_" + privCapabilities[cap]
	}
	return "CAP_" + strconv.Itoa(int(cap))
}

type privTraceOptions struct {
	duration     time.Duration
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	jsonPath     string
	raw          bool
	denied       bool
}

// privEvent는 거부된 capability 검사, 자격 변경, module 적재 하나다. 자격 변경이면 바뀌기 전과 뒤의 id가 있다.
type privEvent struct {
	TimestampNS uint64     `json:"timestamp_ns"`
	Event       string     `json:"event"`
	PID         uint32     `json:"pid"`
	TID         uint32     `json:"tid"`
	Process     string     `json:"process"`
	CgroupID    uint64     `json:"cgroup_id"`
	Capability  string     `json:"capability,omitempty"`
	UID         *[2]uint32 `json:"uid,omitempty"`
	EUID        *[2]uint32 `json:"euid,omitempty"`
	GID         *[2]uint32 `json:"gid,omitempty"`
	EGID        *[2]uint32 `json:"egid,omitempty"`
	Module      string     `json:"module,omitempty"`
}

// change는 자격 변경에서 바뀐 id만 "uid 1000→0" 꼴로 잇는다.
func (event privEvent) change() string {
	var parts []string
	for _, id := range []struct {
		name  string
		value *[2]uint32
	}{{"uid", event.UID}, {"euid", event.EUID}, {"gid", event.GID}, {"egid", event.EGID}} {
		if id.value != nil && id.value[0] != id.value[1] {
			parts = append(parts, fmt.Sprintf("%s %d→%d", id.name, id.value[0], id.value[1]))
		}
	}
	return strings.Join(parts, " ")
}

func (event privEvent) line() string {
	at := time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000000")
	switch event.Event {
	case privEventCred:
		return fmt.Sprintf("%s  %s %d/%d  cred  %s", at, event.Process, event.PID, event.TID, event.change())
	case privEventMod:
		return fmt.Sprintf("%s  %s %d/%d  module  %s", at, event.Process, event.PID, event.TID, event.Module)
	}
	return fmt.Sprintf("%s  %s %d/%d  denied  %s", at, event.Process, event.PID, event.TID, event.Capability)
}

// privSample은 BPF map의 capability 검사 항목 하나다. NoAudit은 결과를 기록하지 않는 검사다.
type privSample struct {
	Process  string
	CgroupID uint64
	Cap      int32
	Denied   bool
	NoAudit  bool
	Checks   uint64
}

type privSummary struct {
	// Event는 --raw의 마지막 줄에만 붙는다.
	Event        string            `json:"event,omitempty"`
	DurationMS   int64             `json:"duration_ms"`
	Checks       uint64            `json:"checks"`
	Denied       uint64            `json:"denied"`
	StatsEntries int               `json:"stats_entries"`
	StatsFull    uint64            `json:"stats_full"`
	Events       uint64            `json:"events"`
	SampledOut   uint64            `json:"sampled_out"`
	RingLost     uint64            `json:"ring_lost"`
	SelfExcluded bool              `json:"self_excluded"`
	Unavailable  map[string]string `json:"unavailable,omitempty"`
}

type privCollection struct {
	samples []privSample
	events  []privEvent
	summary privSummary
}

type privCapabilityRow struct {
	Process    string `json:"process"`
	Capability string `json:"capability"`
	Result     string `json:"result"`
	NoAudit    bool   `json:"noaudit,omitempty"`
	Checks     uint64 `json:"checks"`
}

type privReport struct {
	Capabilities []privCapabilityRow `json:"capabilities"`
	Denied       []privEvent         `json:"denied"`
	Credentials  []privEvent         `json:"credentials"`
	Modules      []privEvent         `json:"modules"`
	Summary      privSummary         `json:"summary"`
}

func runTracePriv(args []string) int {
	options := privTraceOptions{}
	var process string
	set := flag.NewFlagSet("trace priv", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.BoolVar(&options.raw, "raw", false, T("command.trace.option.raw"))
	set.BoolVar(&options.denied, "denied", false, T("command.trace.option.denied"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace priv"))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	if options.raw && options.jsonPath != "" {
		fmt.Fprintln(os.Stderr, T("cli.trace.raw_json_conflict"))
		return 2
	}
	pids, comms, err := parseSyscallProcessFilter(process)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_process", err))
		return 2
	}
	options.pids, options.comms = pids, comms
	if err := privTracePrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if options.containerRef != "" {
		container, code, err := resolveTraceContainer(options.containerRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return code
		}
		options.container = container
	}
	if traceIsTerminal(os.Stderr) {
		if options.duration > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.priv_running_for", options.duration))
		} else {
			fmt.Fprintln(os.Stderr, T("cli.trace.priv_running"))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	encoder := json.NewEncoder(os.Stdout)
	var onEvent func(privEvent) error
	if options.raw {
		onEvent = func(event privEvent) error { return encoder.Encode(event) }
	}
	collection, err := collectPrivEvents(options, ctx.Done(), onEvent)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.priv_failed", err))
		return 1
	}
	report := privReportFor(collection, options.denied)
	switch {
	case options.raw:
		summary := report.Summary
		summary.Event = "priv_summary"
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case options.jsonPath != "":
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	default:
		report.print(os.Stdout)
	}
	privNotices(os.Stderr, report.Summary)
	return 0
}

// privNotices는 stderr로 가므로 --json -와 --raw의 stdout을 바꾸지 않는다.
func privNotices(out io.Writer, summary privSummary) {
	names := make([]string, 0, len(summary.Unavailable))
	for name := range summary.Unavailable {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintln(out, T("cli.trace.priv_unavailable", name, summary.Unavailable[name]))
	}
	if summary.StatsFull > 0 {
		fmt.Fprintln(out, T("cli.trace.priv_map_full", summary.StatsFull))
	}
	if !summary.SelfExcluded {
		fmt.Fprintln(out, T("cli.trace.priv_self_included"))
	}
}

func privReportFor(collection privCollection, deniedOnly bool) privReport {
	report := privReport{Summary: collection.summary, Capabilities: []privCapabilityRow{}, Denied: []privEvent{}, Credentials: []privEvent{}, Modules: []privEvent{}}
	report.Summary.StatsEntries = len(collection.samples)
	rows := map[privCapabilityRow]uint64{}
	for _, sample := range collection.samples {
		report.Summary.Checks += sample.Checks
		if sample.Denied && !sample.NoAudit {
			report.Summary.Denied += sample.Checks
		}
		if deniedOnly && !sample.Denied {
			continue
		}
		result := "allowed"
		if sample.Denied {
			result = "denied"
		}
		rows[privCapabilityRow{Process: emptyAs(sample.Process, "-"), Capability: privCapabilityName(sample.Cap), Result: result, NoAudit: sample.NoAudit}] += sample.Checks
	}
	for row, checks := range rows {
		row.Checks = checks
		report.Capabilities = append(report.Capabilities, row)
	}
	// 거부가 먼저 오고, 그 안에서는 기록되는 검사, 횟수 순이다. noaudit 거부는 kernel이 동작을 고르려고 물어본 것이다.
	sort.Slice(report.Capabilities, func(i, j int) bool {
		left, right := report.Capabilities[i], report.Capabilities[j]
		if left.Result != right.Result {
			return left.Result == "denied"
		}
		if left.NoAudit != right.NoAudit {
			return !left.NoAudit
		}
		if left.Checks != right.Checks {
			return left.Checks > right.Checks
		}
		return left.Process+left.Capability < right.Process+right.Capability
	})
	for _, event := range collection.events {
		switch event.Event {
		case privEventCred:
			report.Credentials = append(report.Credentials, event)
		case privEventMod:
			report.Modules = append(report.Modules, event)
		default:
			report.Denied = append(report.Denied, event)
		}
	}
	return report
}

func (report privReport) print(out io.Writer) {
	summary := report.Summary
	fmt.Fprintf(out, "PRIV TRACE  duration %s  checks %d  denied %d\n", syscallTime(float64(summary.DurationMS)*float64(time.Millisecond)), summary.Checks, summary.Denied)
	if _, missing := summary.Unavailable["capability"]; !missing {
		fmt.Fprintf(out, "%-16s %-24s %-16s %10s\n", "PROCESS", "CAPABILITY", "RESULT", "CHECKS")
		for index, row := range report.Capabilities {
			if index == privTraceRows {
				fmt.Fprintf(out, "... %d more rows in --json\n", len(report.Capabilities)-privTraceRows)
				break
			}
			result := row.Result
			if row.NoAudit {
				result += " (noaudit)"
			}
			fmt.Fprintf(out, "%-16.16s %-24s %-16s %10d\n", row.Process, row.Capability, result, row.Checks)
		}
	}
	for _, section := range []struct {
		title  string
		events []privEvent
	}{{"DENIED", report.Denied}, {"CREDENTIALS", report.Credentials}, {"MODULES", report.Modules}} {
		if len(section.events) == 0 {
			continue
		}
		fmt.Fprintln(out, section.title)
		for index, event := range section.events {
			if index == privTraceRows {
				fmt.Fprintf(out, "... %d more in --json\n", len(section.events)-privTraceRows)
				break
			}
			fmt.Fprintln(out, "  "+event.line())
		}
	}
	fmt.Fprintf(out, "entries %d  stats_full %d  events %d  sampled_out %d  ring_lost %d\n", summary.StatsEntries, summary.StatsFull, summary.Events, summary.SampledOut, summary.RingLost)
}
