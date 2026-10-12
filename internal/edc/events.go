package edc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// events는 커널 로그와 systemd(PID 1) journal을 읽기만 해서 사고 신호를 시간 순서로 보인다.
// 로그 전체를 보여 주지 않고, 판정할 수 있는 신호만 범주별 Result로 둔다.

// errEventsNeedsRoot는 권한 때문에 읽지 못한 원천이다. 비어 있는 결과를 통과로 보이지 않게 skip으로 둔다.
var errEventsNeedsRoot = errors.New("needs root")

// errEventsNoJournal은 journalctl이 없는 경우다. unit과 시계 판정은 journal에만 있어 skip이 된다.
var errEventsNoJournal = errors.New("no journalctl")

const (
	eventsDefaultSince = 6 * time.Hour
	// journalctl -n과 출력·한 줄 크기의 상한이다. 큰 호스트에서도 메모리와 시간이 일정하게 끝나게 한다.
	eventsJournalMaxEntries = 20000
	eventsJournalLimit      = 32 << 20
	eventsJournalLineLimit  = 1 << 20
	eventsKmsgRecordSize    = 8 << 10
	eventsKmsgMaxRecords    = 100000
	eventsEvidenceLines     = 20
	eventsTimelineLines     = 50
	eventsTimeLayout        = "2006-01-02 15:04:05"

	// systemd의 SD_MESSAGE_UNIT_FAILURE_RESULT와 SD_MESSAGE_UNIT_RESTART_SCHEDULED다.
	// SD_MESSAGE_UNIT_FAILED는 job 실패라 unit이 스스로 죽은 경우를 놓치므로 쓰지 않는다.
	eventsUnitFailureID = "d9b373ed55a64feb8242e02dbe79a49c"
	eventsUnitRestartID = "5eb03494b6584870a536b337290809b3"
)

const (
	eventsOOM        = "oom"
	eventsSegfault   = "segfault"
	eventsDiskIO     = "disk_io"
	eventsFSReadonly = "fs_readonly"
	eventsLink       = "link"
	eventsUnits      = "units"
	eventsClock      = "clock"
)

// eventsSources는 events가 읽는 원천이다. 테스트는 함수를 가짜로 넣는다.
type eventsSources struct {
	now time.Time
	// boot는 부팅 시각이다. /dev/kmsg의 시각은 부팅 뒤 경과 시간이라 이것으로 벽시계 시각을 만든다.
	boot    time.Time
	kmsg    func(ctx context.Context) ([]string, error)
	journal func(ctx context.Context, start time.Time) (io.Reader, []string, error)
}

type kmsgRecord struct {
	Priority int
	Sequence uint64
	Micros   int64
	Message  string
}

type journalEntry struct {
	Realtime  int64
	Monotonic int64
	Boot      string
	Transport string
	MessageID string
	Unit      string
	Message   string
}

type kernelLine struct {
	Time    time.Time
	Message string
}

type eventsSignal struct {
	Kind string
	Time time.Time
	// Subject는 묶는 기준이다. link는 interface, units는 unit 이름이다.
	Subject string
	// Brief는 요약과 timeline에 쓰는 짧은 설명이고, Message는 근거에 쓰는 원문이다.
	Brief   string
	Message string
	Up      bool
	Failed  bool
}

type eventsFacts struct {
	Now, Start time.Time
	Since      time.Duration
	// KernelSource는 커널 신호를 읽은 원천이다. 비어 있으면 KernelErr가 이유다.
	KernelSource string
	KernelErr    error
	JournalErr   error
	Signals      []eventsSignal
	Warnings     []string
}

var (
	eventsOOMKilled    = regexp.MustCompile(`(Memory cgroup out of memory|Out of memory)(?: \(oom_kill_allocating_task\))?: Kill(?:ed)? process (\d+) \(([^)]*)\)`)
	eventsOOMKillLine  = regexp.MustCompile(`oom-kill:.*?task_memcg=([^,]*),task=([^,]*),pid=(\d+)`)
	eventsSegfaultLine = regexp.MustCompile(`(\S+)\[(\d+)\]: segfault at `)
	eventsGPFault      = regexp.MustCompile(`traps: (\S+)\[(\d+)\] general protection`)
	eventsDiskErrors   = []*regexp.Regexp{
		regexp.MustCompile(`Buffer I/O error on dev(?:ice)? ([^,\s]+)`),
		regexp.MustCompile(`critical medium error, dev ([^,\s]+)`),
		regexp.MustCompile(`I/O error, dev ([^,\s]+)`),
		regexp.MustCompile(`XFS \(([^)]+)\): metadata I/O error`),
	}
	eventsReadonly = []*regexp.Regexp{
		regexp.MustCompile(`\(([^)]+)\): Remounting filesystem read-only`),
		regexp.MustCompile(`\(device ([^)]+)\): forced readonly`),
		regexp.MustCompile(`\(([^)]+)\): .*?Filesystem has been shut down`),
		regexp.MustCompile(`()Remounting filesystem read-only`),
		regexp.MustCompile(`()forced readonly`),
		regexp.MustCompile(`()Filesystem has been shut down`),
	}
	eventsLinkNIC   = regexp.MustCompile(`(\S+):? NIC Link is (Up|Down)`)
	eventsLinkPlain = regexp.MustCompile(`(\S+): Link is (Up|Down)`)
	eventsLinkReady = regexp.MustCompile(`(\S+): link becomes ready`)
)

func runEvents(args []string, version string) int {
	options, since, ok := parseEventsFlags(args, configuredCommon(15*time.Second), os.Stderr)
	if !ok {
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	sources, supported := hostEventsSources()
	if !supported {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("events", T("events.skip.linux_only"))}, options.redact))
	}
	return emit(options, buildReport(version, started, nil, evaluateEvents(collectEventsFacts(ctx, sources, since)), options.redact))
}

// parseEventsFlags는 원천을 읽기 전에 --since를 검사한다. 0이나 음수가 창을 조용히 비우거나 넓히지 않게 한다.
func parseEventsFlags(args []string, options commonOptions, stderr io.Writer) (commonOptions, time.Duration, bool) {
	set := flag.NewFlagSet("events", flag.ContinueOnError)
	set.SetOutput(stderr)
	bindCommon(set, &options)
	since := set.Duration("since", eventsDefaultSince, T("events.option.since"))
	if err := set.Parse(args); err != nil {
		return options, 0, false
	}
	if set.NArg() != 0 {
		fmt.Fprintln(stderr, T("cli.usage", "edc events [--since 6h] [options]"))
		return options, 0, false
	}
	if *since <= 0 {
		fmt.Fprintln(stderr, T("events.since_positive"))
		return options, 0, false
	}
	return options, *since, true
}

// readKmsgRecords는 /dev/kmsg에서 한 번에 기록 하나씩 읽는다. 비차단으로 열어 EAGAIN이 끝이다.
func readKmsgRecords(ctx context.Context, read func([]byte) (int, error)) ([]string, error) {
	buffer := make([]byte, eventsKmsgRecordSize)
	var records []string
	for attempt := 0; attempt < eventsKmsgMaxRecords && len(records) < eventsKmsgMaxRecords; attempt++ {
		if err := ctx.Err(); err != nil {
			return records, err
		}
		n, err := read(buffer)
		switch {
		// 읽는 사이 ring buffer가 돌아 기록이 덮이면 EPIPE가 오고, 다음 read는 남은 가장 오래된 기록부터 준다.
		case errors.Is(err, syscall.EPIPE):
			continue
		case errors.Is(err, syscall.EAGAIN), errors.Is(err, io.EOF):
			return records, nil
		case err != nil:
			return records, err
		case n == 0:
			return records, nil
		}
		records = append(records, string(buffer[:n]))
	}
	return records, nil
}

// parseKmsgRecord는 `prio,seq,usec,flags[,...];message` 한 기록을 읽는다. 공백으로 시작하는 줄은 key=value 이어짐이다.
func parseKmsgRecord(record string) (kmsgRecord, bool) {
	line, _, _ := strings.Cut(record, "\n")
	if strings.HasPrefix(line, " ") {
		return kmsgRecord{}, false
	}
	header, message, found := strings.Cut(line, ";")
	if !found {
		return kmsgRecord{}, false
	}
	fields := strings.Split(header, ",")
	if len(fields) < 4 {
		return kmsgRecord{}, false
	}
	priority, err := strconv.Atoi(fields[0])
	if err != nil || priority < 0 {
		return kmsgRecord{}, false
	}
	sequence, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return kmsgRecord{}, false
	}
	micros, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || micros < 0 {
		return kmsgRecord{}, false
	}
	return kmsgRecord{Priority: priority, Sequence: sequence, Micros: micros, Message: message}, true
}

// readJournalEntries는 journalctl -o json 출력을 한 줄씩 읽는다. 상한에 닿으면 읽기를 멈추고 truncated를 알린다.
func readJournalEntries(reader io.Reader, limit int64) ([]journalEntry, bool, error) {
	limited := &io.LimitedReader{R: reader, N: limit}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), eventsJournalLineLimit)
	var entries []journalEntry
	for scanner.Scan() {
		if entry, ok := parseJournalLine(scanner.Bytes()); ok {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return entries, true, nil
		}
		return entries, false, err
	}
	return entries, limited.N <= 0, nil
}

type journalRaw struct {
	Realtime  json.RawMessage `json:"__REALTIME_TIMESTAMP"`
	Monotonic json.RawMessage `json:"__MONOTONIC_TIMESTAMP"`
	Boot      json.RawMessage `json:"_BOOT_ID"`
	Transport json.RawMessage `json:"_TRANSPORT"`
	MessageID json.RawMessage `json:"MESSAGE_ID"`
	Unit      json.RawMessage `json:"UNIT"`
	Message   json.RawMessage `json:"MESSAGE"`
}

func parseJournalLine(line []byte) (journalEntry, bool) {
	var raw journalRaw
	if err := json.Unmarshal(line, &raw); err != nil {
		return journalEntry{}, false
	}
	realtime, err := strconv.ParseInt(journalField(raw.Realtime), 10, 64)
	if err != nil {
		return journalEntry{}, false
	}
	monotonic, err := strconv.ParseInt(journalField(raw.Monotonic), 10, 64)
	if err != nil {
		return journalEntry{}, false
	}
	return journalEntry{
		Realtime:  realtime,
		Monotonic: monotonic,
		Boot:      journalField(raw.Boot),
		Transport: journalField(raw.Transport),
		MessageID: journalField(raw.MessageID),
		Unit:      journalField(raw.Unit),
		Message:   journalField(raw.Message),
	}, true
}

// journalField는 journal JSON 값 하나를 문자열로 바꾼다.
// 출력할 수 없는 바이트가 든 값은 숫자 배열로, 같은 field가 여러 번 나오면 값의 배열로 온다.
func journalField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var bytes []byte
	var numbers []int
	if json.Unmarshal(raw, &numbers) == nil {
		for _, number := range numbers {
			bytes = append(bytes, byte(number))
		}
		return string(bytes)
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) == nil && len(values) > 0 {
		return journalField(values[0])
	}
	return ""
}

func collectEventsFacts(ctx context.Context, sources eventsSources, since time.Duration) eventsFacts {
	facts := eventsFacts{Now: sources.now, Start: sources.now.Add(-since), Since: since}
	var kernel []kernelLine
	var journal []journalEntry
	reader, warnings, err := sources.journal(ctx, facts.Start)
	facts.Warnings = append(facts.Warnings, warnings...)
	if err == nil {
		entries, truncated, readErr := readJournalEntries(reader, eventsJournalLimit)
		if truncated {
			facts.Warnings = append(facts.Warnings, T("events.window.truncated", "journalctl"))
		}
		err = readErr
		for _, entry := range entries {
			if time.UnixMicro(entry.Realtime).Before(facts.Start) {
				continue
			}
			journal = append(journal, entry)
			if entry.Transport == "kernel" {
				kernel = append(kernel, kernelLine{Time: time.UnixMicro(entry.Realtime).In(sources.now.Location()), Message: entry.Message})
			}
		}
	}
	facts.JournalErr = err

	// /dev/kmsg는 현재 부팅의 ring buffer만 담는다. 재부팅 앞의 OOM이 사라지지 않게 journal을 먼저 쓴다.
	if err == nil && len(kernel) > 0 {
		facts.KernelSource = "journal"
	} else if records, kmsgErr := sources.kmsg(ctx); kmsgErr == nil {
		facts.KernelSource = "/dev/kmsg"
		if len(records) >= eventsKmsgMaxRecords {
			facts.Warnings = append(facts.Warnings, T("events.window.truncated", "/dev/kmsg"))
		}
		for _, text := range records {
			record, ok := parseKmsgRecord(text)
			if !ok {
				continue
			}
			at := sources.boot.Add(time.Duration(record.Micros) * time.Microsecond)
			if at.Before(facts.Start) {
				continue
			}
			kernel = append(kernel, kernelLine{Time: at, Message: record.Message})
		}
	} else if err == nil {
		facts.KernelSource = "journal"
	} else {
		facts.KernelErr = kmsgErr
	}

	facts.Signals = append(facts.Signals, matchKernelLines(kernel)...)
	if facts.JournalErr == nil {
		facts.Signals = append(facts.Signals, unitSignals(journal, sources.now.Location())...)
		facts.Signals = append(facts.Signals, clockSignals(journal, sources.now.Location())...)
	}
	sort.SliceStable(facts.Signals, func(i, j int) bool { return facts.Signals[i].Time.Before(facts.Signals[j].Time) })
	return facts
}

// matchKernelLines는 커널 메시지에서 신호를 찾는다. 맞지 않는 줄은 신호가 아닐 뿐이다.
func matchKernelLines(lines []kernelLine) []eventsSignal {
	// oom-kill 줄은 Killed process 줄 앞에 나온다. 순서에 기대지 않고 pid로 cgroup을 짝짓는다.
	cgroups := map[string]string{}
	for _, line := range lines {
		if match := eventsOOMKillLine.FindStringSubmatch(line.Message); match != nil {
			cgroups[match[3]] = match[1]
		}
	}
	var signals []eventsSignal
	for _, line := range lines {
		if signal, ok := matchKernelLine(line, cgroups); ok {
			signals = append(signals, signal)
		}
	}
	return signals
}

func matchKernelLine(line kernelLine, cgroups map[string]string) (eventsSignal, bool) {
	message := line.Message
	signal := eventsSignal{Time: line.Time, Message: message}
	if match := eventsOOMKilled.FindStringSubmatch(message); match != nil {
		cgroup, ok := cgroups[match[2]]
		if !ok || cgroup == "" {
			cgroup = "?"
		}
		signal.Kind, signal.Subject = eventsOOM, match[3]
		signal.Brief = fmt.Sprintf("%s pid=%s cgroup=%s", match[3], match[2], cgroup)
		return signal, true
	}
	if match := eventsSegfaultLine.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsSegfault, match[1]
		signal.Brief = fmt.Sprintf("%s pid=%s segfault", match[1], match[2])
		return signal, true
	}
	if match := eventsGPFault.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsSegfault, match[1]
		signal.Brief = fmt.Sprintf("%s pid=%s general protection", match[1], match[2])
		return signal, true
	}
	for _, pattern := range eventsDiskErrors {
		if match := pattern.FindStringSubmatch(message); match != nil {
			signal.Kind, signal.Subject = eventsDiskIO, match[1]
			signal.Brief = "dev=" + match[1]
			return signal, true
		}
	}
	for _, pattern := range eventsReadonly {
		if match := pattern.FindStringSubmatch(message); match != nil {
			device := match[1]
			if device == "" {
				device = "?"
			}
			signal.Kind, signal.Subject = eventsFSReadonly, device
			signal.Brief = "dev=" + device
			return signal, true
		}
	}
	for _, pattern := range []*regexp.Regexp{eventsLinkNIC, eventsLinkPlain} {
		if match := pattern.FindStringSubmatch(message); match != nil {
			return linkSignal(signal, match[1], match[2] == "Up"), true
		}
	}
	if match := eventsLinkReady.FindStringSubmatch(message); match != nil {
		return linkSignal(signal, match[1], true), true
	}
	return eventsSignal{}, false
}

func linkSignal(signal eventsSignal, iface string, up bool) eventsSignal {
	signal.Kind, signal.Subject, signal.Up = eventsLink, strings.TrimSuffix(iface, ":"), up
	state := "down"
	if up {
		state = "up"
	}
	signal.Brief = signal.Subject + " " + state
	return signal
}

// unitSignals는 PID 1이 남긴 unit 실패 결과와 예약된 재시작을 모은다.
func unitSignals(entries []journalEntry, location *time.Location) []eventsSignal {
	var signals []eventsSignal
	for _, entry := range entries {
		if entry.Unit == "" || (entry.MessageID != eventsUnitFailureID && entry.MessageID != eventsUnitRestartID) {
			continue
		}
		failed := entry.MessageID == eventsUnitFailureID
		brief := entry.Unit + " restart"
		if failed {
			brief = entry.Unit + " failed"
		}
		message := entry.Message
		if message == "" {
			message = brief
		}
		signals = append(signals, eventsSignal{Kind: eventsUnits, Time: time.UnixMicro(entry.Realtime).In(location), Subject: entry.Unit, Brief: brief, Message: message, Failed: failed})
	}
	return signals
}

// clockSignals는 같은 부팅 안에서 이웃한 기록의 벽시계 간격과 monotonic 간격을 비교한다.
// NTP slew는 500ppm을 넘지 않으므로 1초 이상이면서 monotonic 간격의 0.1%를 넘는 차이만 step으로 본다.
func clockSignals(journal []journalEntry, location *time.Location) []eventsSignal {
	byBoot := map[string][]journalEntry{}
	for _, entry := range journal {
		if entry.Boot != "" {
			byBoot[entry.Boot] = append(byBoot[entry.Boot], entry)
		}
	}
	var signals []eventsSignal
	for boot, entries := range byBoot {
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Monotonic < entries[j].Monotonic })
		for index := 1; index < len(entries); index++ {
			monotonic := entries[index].Monotonic - entries[index-1].Monotonic
			step := entries[index].Realtime - entries[index-1].Realtime - monotonic
			magnitude := step
			if magnitude < 0 {
				magnitude = -magnitude
			}
			if magnitude < int64(time.Second/time.Microsecond) || magnitude*1000 <= monotonic {
				continue
			}
			short := boot
			if len(short) > 8 {
				short = short[:8]
			}
			brief := fmt.Sprintf("%+.1fs", float64(step)/1e6)
			signals = append(signals, eventsSignal{
				Kind: eventsClock, Time: time.UnixMicro(entries[index].Realtime).In(location), Subject: boot, Brief: brief,
				Message: fmt.Sprintf("%s (boot %s)", brief, short),
			})
		}
	}
	return signals
}

func eventsResult(id string, status Status, summary string) Result {
	return Result{Probe: "events." + id, Status: status, StartedAt: time.Now().UTC(), Summary: summary}
}

// eventsSkip은 원천을 읽지 못한 범주다. 권한 때문이면 root나 journal 그룹이 필요하다고 알린다.
func eventsSkip(id, source string, err error) Result {
	switch {
	case errors.Is(err, errEventsNoJournal):
		return eventsResult(id, StatusSkip, T("events.skip.no_journal"))
	case errors.Is(err, errEventsNeedsRoot), errors.Is(err, fs.ErrPermission):
		return eventsResult(id, StatusSkip, T("events.skip.needs_root", source))
	}
	return eventsResult(id, StatusSkip, T("events.skip.unreadable", source, infoSingleLine(err.Error())))
}

func evaluateEvents(facts eventsFacts) []Result {
	byKind := map[string][]eventsSignal{}
	for _, signal := range facts.Signals {
		byKind[signal.Kind] = append(byKind[signal.Kind], signal)
	}
	return []Result{
		eventsWindow(facts),
		eventsKernelCategory(facts, eventsOOM, byKind[eventsOOM], StatusFail),
		eventsKernelCategory(facts, eventsDiskIO, byKind[eventsDiskIO], StatusFail),
		eventsKernelCategory(facts, eventsFSReadonly, byKind[eventsFSReadonly], StatusFail),
		eventsLinkResult(facts, byKind[eventsLink]),
		eventsUnitsResult(facts, byKind[eventsUnits]),
		eventsKernelCategory(facts, eventsSegfault, byKind[eventsSegfault], StatusWarn),
		eventsClockResult(facts, byKind[eventsClock]),
	}
}

func eventsWindow(facts eventsFacts) Result {
	const id = "window"
	since := eventsDurationText(facts.Since)
	if facts.KernelErr != nil && facts.JournalErr != nil {
		result := eventsResult(id, StatusSkip, T("events.window.none", since))
		result.Evidence = []Evidence{{Label: T("events.evidence.source"), Value: eventsSourceText(facts)}}
		result.Warnings = facts.Warnings
		return result
	}
	result := eventsResult(id, StatusPass, T("events.window.summary", len(facts.Signals), since, facts.Start.Format(eventsTimeLayout)))
	if len(facts.Signals) > 0 {
		lines := make([]string, 0, len(facts.Signals))
		for _, signal := range facts.Signals {
			lines = append(lines, fmt.Sprintf("%s  %-11s  %s", signal.Time.Format(eventsTimeLayout), signal.Kind, signal.Brief))
		}
		result.Evidence = append(result.Evidence, Evidence{Label: T("events.evidence.timeline"), Value: eventsRecent(lines, eventsTimelineLines)})
	}
	result.Evidence = append(result.Evidence, Evidence{Label: T("events.evidence.source"), Value: eventsSourceText(facts)})
	result.Warnings = facts.Warnings
	return result
}

func eventsSourceText(facts eventsFacts) string {
	kernel := "journalctl (_TRANSPORT=kernel)"
	switch {
	case facts.KernelErr != nil:
		kernel = eventsSkip("kernel", "/dev/kmsg", facts.KernelErr).Summary
	case facts.KernelSource == "/dev/kmsg":
		kernel = "/dev/kmsg"
	}
	units := "journalctl (_PID=1)"
	if facts.JournalErr != nil {
		units = eventsSkip("units", "journal", facts.JournalErr).Summary
	}
	return fmt.Sprintf("kernel: %s\nunits, clock: %s\nwindow: %s .. %s", kernel, units, facts.Start.Format(eventsTimeLayout), facts.Now.Format(eventsTimeLayout))
}

// eventsRecent는 최근 줄만 남긴다. 앞에서 버린 줄 수를 첫 줄에 적는다.
func eventsRecent(lines []string, limit int) string {
	if len(lines) <= limit {
		return strings.Join(lines, "\n")
	}
	dropped := len(lines) - limit
	return T("events.evidence.more", dropped) + "\n" + strings.Join(lines[dropped:], "\n")
}

func eventsEvidence(signals []eventsSignal) []Evidence {
	lines := make([]string, 0, len(signals))
	for _, signal := range signals {
		lines = append(lines, signal.Time.Format(eventsTimeLayout)+"  "+firstLine(signal.Message))
	}
	return []Evidence{{Label: T("events.evidence.events"), Value: eventsRecent(lines, eventsEvidenceLines)}}
}

func eventsLatest(signal eventsSignal) string {
	return signal.Time.Format(eventsTimeLayout) + " " + signal.Brief
}

func eventsKernelSkip(facts eventsFacts, id string) (Result, bool) {
	if facts.KernelErr == nil {
		return Result{}, false
	}
	return eventsSkip(id, "/dev/kmsg", facts.KernelErr), true
}

func eventsKernelNext(facts eventsFacts) string {
	if facts.KernelSource == "/dev/kmsg" {
		return "dmesg -T"
	}
	return fmt.Sprintf("journalctl -k --since @%d", facts.Start.Unix())
}

func eventsKernelCategory(facts eventsFacts, id string, signals []eventsSignal, status Status) Result {
	if result, skip := eventsKernelSkip(facts, id); skip {
		return result
	}
	start := facts.Start.Format(eventsTimeLayout)
	if len(signals) == 0 {
		return eventsResult(id, StatusPass, T("events."+id+".none", start))
	}
	result := eventsResult(id, status, T("events."+id+".found", len(signals), eventsLatest(signals[len(signals)-1])))
	if status == StatusFail {
		result.Error = &DiagnosticError{Kind: "events", Message: result.Summary}
	}
	result.Evidence = eventsEvidence(signals)
	result.Next = eventsKernelNext(facts)
	return result
}

// eventsLinkResult는 interface마다 마지막 상태를 본다. 마지막이 down이면 아직 끊긴 것이고, down 뒤에 up이 오면 흔들린 것이다.
func eventsLinkResult(facts eventsFacts, signals []eventsSignal) Result {
	const id = eventsLink
	if result, skip := eventsKernelSkip(facts, id); skip {
		return result
	}
	last := map[string]bool{}
	wentDown := map[string]bool{}
	var order []string
	for _, signal := range signals {
		if _, seen := last[signal.Subject]; !seen {
			order = append(order, signal.Subject)
		}
		last[signal.Subject] = signal.Up
		if !signal.Up {
			wentDown[signal.Subject] = true
		}
	}
	var down, flapped []string
	for _, iface := range order {
		switch {
		case !last[iface]:
			down = append(down, iface)
		case wentDown[iface]:
			flapped = append(flapped, iface)
		}
	}
	var result Result
	switch {
	case len(down) > 0:
		result = eventsResult(id, StatusFail, T("events.link.down", strings.Join(down, ", "), len(signals)))
		result.Error = &DiagnosticError{Kind: "events", Message: result.Summary}
		result.Next = "ip -s link show " + down[0]
	case len(flapped) > 0:
		result = eventsResult(id, StatusWarn, T("events.link.flap", strings.Join(flapped, ", "), len(signals)))
		result.Next = "ip -s link show " + flapped[0]
	default:
		return eventsResult(id, StatusPass, T("events.link.none", facts.Start.Format(eventsTimeLayout), len(signals)))
	}
	result.Evidence = eventsEvidence(signals)
	return result
}

// eventsUnitsResult는 unit마다 창 안의 순서를 본다. 마지막이 실패면 아직 실패한 상태다.
// 실패 뒤 재시작이 왔거나 재시작이 3번 이상이면 되살아났어도 반복해서 죽는 unit이다.
func eventsUnitsResult(facts eventsFacts, signals []eventsSignal) Result {
	const id = eventsUnits
	if facts.JournalErr != nil {
		return eventsSkip(id, "journal", facts.JournalErr)
	}
	type unitState struct {
		last       eventsSignal
		failedOnce bool
		restarts   int
		recovered  bool
	}
	units := map[string]*unitState{}
	for _, signal := range signals {
		state := units[signal.Subject]
		if state == nil {
			state = &unitState{}
			units[signal.Subject] = state
		}
		if signal.Failed {
			state.failedOnce = true
		} else {
			state.restarts++
			state.recovered = state.recovered || state.failedOnce
		}
		state.last = signal
	}
	var failed, restarting []eventsSignal
	for _, state := range units {
		switch {
		case state.last.Failed:
			failed = append(failed, state.last)
		case state.recovered || state.restarts >= 3:
			restarting = append(restarting, state.last)
		}
	}
	byTime := func(list []eventsSignal) {
		sort.Slice(list, func(i, j int) bool { return list[i].Time.Before(list[j].Time) })
	}
	byTime(failed)
	byTime(restarting)
	var result Result
	switch {
	case len(failed) > 0:
		latest := failed[len(failed)-1]
		result = eventsResult(id, StatusFail, T("events.units.failed", len(failed), eventsLatest(latest)))
		result.Error = &DiagnosticError{Kind: "events", Message: result.Summary}
		result.Next = fmt.Sprintf("journalctl -u %s --since @%d", latest.Subject, facts.Start.Unix())
	case len(restarting) > 0:
		latest := restarting[len(restarting)-1]
		result = eventsResult(id, StatusWarn, T("events.units.restarting", len(restarting), eventsLatest(latest)))
		result.Next = fmt.Sprintf("journalctl -u %s --since @%d", latest.Subject, facts.Start.Unix())
	case len(signals) > 0:
		return eventsResult(id, StatusPass, T("events.units.ok", len(signals)))
	default:
		return eventsResult(id, StatusPass, T("events.units.none", facts.Start.Format(eventsTimeLayout)))
	}
	result.Evidence = eventsEvidence(signals)
	return result
}

func eventsClockResult(facts eventsFacts, signals []eventsSignal) Result {
	const id = eventsClock
	if facts.JournalErr != nil {
		return eventsSkip(id, "journal", facts.JournalErr)
	}
	if len(signals) == 0 {
		return eventsResult(id, StatusPass, T("events.clock.none", facts.Start.Format(eventsTimeLayout)))
	}
	result := eventsResult(id, StatusWarn, T("events.clock.found", len(signals), eventsLatest(signals[len(signals)-1])))
	result.Evidence = eventsEvidence(signals)
	return result
}

// eventsDurationText는 6h0m0s를 6h로 줄인다.
func eventsDurationText(value time.Duration) string {
	text := value.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}
