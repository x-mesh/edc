package edc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

// errEventsNoSystemd는 systemd가 PID 1이 아닌 호스트다. container와 Alpine에는 unit이 없다.
var errEventsNoSystemd = errors.New("no systemd")

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
	eventsLockup     = "lockup"
	eventsOops       = "oops"
	eventsHungTask   = "hung_task"
	eventsFailed     = "failed_units"
	eventsErrors     = "journal_errors"
	// eventsErrorUnits는 근거에 적는 unit 수다. 나머지는 개수만 센다.
	eventsErrorUnits = 10
)

// eventsTaintLetters는 Documentation/admin-guide/tainted-kernels.rst의 비트 순서다. 비트 0은 P(proprietary module)다.
const eventsTaintLetters = "PFSRMBUDAWCIOELKXTN"

// eventsTaintIncident는 사고와 이어지는 flag다. W를 뺀 넷은 창에 기록이 없어도 oops 결과를 warn으로 만든다.
// W는 펌웨어 문제로 부팅 때부터 켜진 호스트가 흔해 근거로만 남긴다.
var eventsTaintIncident = []byte{'D', 'W', 'L', 'M', 'B'}

// eventsSources는 events가 읽는 원천이다. 테스트는 함수를 가짜로 넣는다.
type eventsSources struct {
	now time.Time
	// boot는 부팅 시각이다. /dev/kmsg의 시각은 부팅 뒤 경과 시간이라 이것으로 벽시계 시각을 만든다.
	boot    time.Time
	kmsg    func(ctx context.Context) ([]string, error)
	journal func(ctx context.Context, start time.Time) (io.Reader, []string, error)
	// tainted는 kernel.tainted의 값이다. nil이면 읽지 않는다.
	tainted func() (uint64, error)
	// failedUnits는 systemctl list-units --state=failed의 출력이다. nil이면 읽지 않는다.
	failedUnits func(ctx context.Context) (string, error)
	// journalErrors는 창 안의 journalctl -p err 기록이다. nil이면 읽지 않는다.
	journalErrors func(ctx context.Context, start time.Time) (io.Reader, []string, error)
}

// eventsFailedUnit은 지금 failed 상태인 unit 하나다. 창과 상관없이 지금의 상태다.
type eventsFailedUnit struct {
	Unit, Load, Active, Sub, Description string
}

// eventsErrorSource는 오류 줄을 남긴 unit이나 program 하나다.
type eventsErrorSource struct {
	Name   string
	Count  int
	Latest journalEntry
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
	// SystemdUnit은 기록을 남긴 process의 unit이고, Identifier는 syslog 이름이다. 오류 줄을 묶을 때 쓴다.
	SystemdUnit string
	Identifier  string
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
	// Tainted는 kernel.tainted의 비트다. TaintedOK가 false면 읽지 못한 것이다.
	Tainted   uint64
	TaintedOK bool
	// FailedUnits는 지금 failed인 unit이다. FailedChecked가 false면 읽지 않았고, FailedErr가 이유다.
	FailedUnits   []eventsFailedUnit
	FailedChecked bool
	FailedErr     error
	// ErrorSources는 창 안의 오류 줄을 많은 순으로 묶은 것이다. ErrorLines는 그 합이다.
	ErrorSources []eventsErrorSource
	ErrorLines   int
	ErrorChecked bool
	ErrorErr     error
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
	// comm에 콜론이 들어가도(kworker/u8:3) 탐욕 매칭이 마지막 콜론을 pid 앞으로 둔다.
	eventsHungTaskLine    = regexp.MustCompile(`INFO: task (\S+):(\d+) blocked for more than (\d+) seconds`)
	eventsSoftLockup      = regexp.MustCompile(`BUG: soft lockup - CPU#(\d+) stuck for (\d+)s! \[(.+):(\d+)\]`)
	eventsHardLockup      = regexp.MustCompile(`Watchdog detected hard LOCKUP on cpu (\d+)`)
	eventsRCUStall        = regexp.MustCompile(`INFO: (rcu_\w+) (?:detected (?:expedited )?stalls on CPUs/tasks|self-detected stall on CPU)`)
	eventsWorkqueueLockup = regexp.MustCompile(`BUG: workqueue lockup - pool cpus=(\d+)`)
	// 첫 group이 요약이다. 주소와 register 값은 뺀다.
	eventsOopsLines = []*regexp.Regexp{
		regexp.MustCompile(`(BUG: unable to handle [^:]*?)(?: at | for address:)`),
		regexp.MustCompile(`(kernel BUG at \S+)`),
		regexp.MustCompile(`(Oops): [0-9a-fA-F]+ \[#\d+\]`),
		regexp.MustCompile(`(Kernel panic) - not syncing`),
		regexp.MustCompile(`(general protection fault)[^\[]*\[#\d+\]`),
	}
	eventsKernelWarning = regexp.MustCompile(`WARNING: CPU: (\d+) PID: (\d+) at (\S+)`)
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
	set := newFlagSet("events")
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
	// SystemdUnit과 Identifier는 trusted field와 syslog 이름이다.
	SystemdUnit json.RawMessage `json:"_SYSTEMD_UNIT"`
	Identifier  json.RawMessage `json:"SYSLOG_IDENTIFIER"`
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
		Realtime:    realtime,
		Monotonic:   monotonic,
		Boot:        journalField(raw.Boot),
		Transport:   journalField(raw.Transport),
		MessageID:   journalField(raw.MessageID),
		Unit:        journalField(raw.Unit),
		Message:     journalField(raw.Message),
		SystemdUnit: journalField(raw.SystemdUnit),
		Identifier:  journalField(raw.Identifier),
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
	if sources.tainted != nil {
		if value, err := sources.tainted(); err == nil {
			facts.Tainted, facts.TaintedOK = value, true
		}
	}
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
	collectEventsUnitState(ctx, sources, &facts)

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
	} else {
		// journal에 커널 줄이 없으면 조용한 호스트인지 journal이 커널을 모으지 않는지 알 수 없다. 통과로 보이지 않게 skip한다.
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
	if fault, ok := matchKernelFault(signal, message); ok {
		return fault, true
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
	case errors.Is(err, errEventsNoSystemd):
		return eventsResult(id, StatusSkip, T("events.skip.no_systemd"))
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
		eventsKernelCategory(facts, eventsLockup, byKind[eventsLockup], StatusFail),
		eventsOopsResult(facts, byKind[eventsOops]),
		eventsLinkResult(facts, byKind[eventsLink]),
		eventsUnitsResult(facts, byKind[eventsUnits]),
		eventsFailedResult(facts),
		eventsErrorsResult(facts),
		eventsKernelCategory(facts, eventsSegfault, byKind[eventsSegfault], StatusWarn),
		eventsKernelCategory(facts, eventsHungTask, byKind[eventsHungTask], StatusWarn),
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

// matchKernelFault는 커널 자신의 멈춤과 오류다. soft lockup도 BUG:로 시작하므로 oops보다 먼저 본다.
func matchKernelFault(signal eventsSignal, message string) (eventsSignal, bool) {
	if match := eventsHungTaskLine.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsHungTask, match[1]
		signal.Brief = fmt.Sprintf("%s pid=%s %ss", match[1], match[2], match[3])
		return signal, true
	}
	if match := eventsSoftLockup.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsLockup, "cpu"+match[1]
		signal.Brief = fmt.Sprintf("soft lockup cpu=%s %ss %s pid=%s", match[1], match[2], match[3], match[4])
		return signal, true
	}
	if match := eventsHardLockup.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsLockup, "cpu"+match[1]
		signal.Brief = "hard lockup cpu=" + match[1]
		return signal, true
	}
	if match := eventsRCUStall.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsLockup, match[1]
		signal.Brief = match[1] + " stall"
		return signal, true
	}
	if match := eventsWorkqueueLockup.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsLockup, "workqueue"
		signal.Brief = "workqueue lockup cpus=" + match[1]
		return signal, true
	}
	for _, pattern := range eventsOopsLines {
		if match := pattern.FindStringSubmatch(message); match != nil {
			signal.Kind, signal.Subject, signal.Failed = eventsOops, "kernel", true
			signal.Brief = match[1]
			return signal, true
		}
	}
	if match := eventsKernelWarning.FindStringSubmatch(message); match != nil {
		signal.Kind, signal.Subject = eventsOops, match[3]
		signal.Brief = fmt.Sprintf("WARNING at %s pid=%s", match[3], match[2])
		return signal, true
	}
	return eventsSignal{}, false
}

type eventsTaint struct {
	// Letters는 켜진 flag의 글자를 띄어 쓴 것이다. 비어 있으면 taint가 없거나 읽지 못한 것이다.
	Letters string
	// Incident는 D, L, M, B 가운데 하나라도 켜진 것이다.
	Incident bool
	Text     string
}

func eventsTaintOf(facts eventsFacts) eventsTaint {
	if !facts.TaintedOK || facts.Tainted == 0 {
		return eventsTaint{}
	}
	var letters []string
	for bit := 0; bit < len(eventsTaintLetters); bit++ {
		if facts.Tainted&(1<<uint(bit)) != 0 {
			letters = append(letters, string(eventsTaintLetters[bit]))
		}
	}
	taint := eventsTaint{Letters: strings.Join(letters, " ")}
	lines := []string{fmt.Sprintf("%s (%d)", taint.Letters, facts.Tainted)}
	for _, letter := range eventsTaintIncident {
		if !strings.Contains(taint.Letters, string(letter)) {
			continue
		}
		if letter != 'W' {
			taint.Incident = true
		}
		lines = append(lines, T("events.taint."+string(letter)))
	}
	taint.Text = strings.Join(lines, "\n")
	return taint
}

// eventsOopsResult는 커널 자신의 오류다. BUG, Oops, panic은 fail이고 WARNING만 있으면 warn이다.
// 창에 기록이 없어도 kernel.tainted의 D, L, M, B는 부팅 뒤 커널이 스스로 남긴 사고 표시라 warn으로 둔다.
func eventsOopsResult(facts eventsFacts, signals []eventsSignal) Result {
	const id = eventsOops
	if result, skip := eventsKernelSkip(facts, id); skip {
		return result
	}
	taint := eventsTaintOf(facts)
	start := facts.Start.Format(eventsTimeLayout)
	var failed []eventsSignal
	for _, signal := range signals {
		if signal.Failed {
			failed = append(failed, signal)
		}
	}
	var result Result
	switch {
	case len(failed) > 0:
		result = eventsResult(id, StatusFail, T("events.oops.found", len(failed), eventsLatest(failed[len(failed)-1])))
		result.Error = &DiagnosticError{Kind: "events", Message: result.Summary}
	case len(signals) > 0:
		result = eventsResult(id, StatusWarn, T("events.oops.warned", len(signals), eventsLatest(signals[len(signals)-1])))
	case taint.Incident:
		result = eventsResult(id, StatusWarn, T("events.oops.tainted", start, taint.Letters))
	default:
		result = eventsResult(id, StatusPass, T("events.oops.none", start))
	}
	if len(signals) > 0 {
		result.Evidence = eventsEvidence(signals)
		result.Next = eventsKernelNext(facts)
	}
	if taint.Letters != "" {
		result.Evidence = append(result.Evidence, Evidence{Label: T("events.evidence.tainted"), Value: taint.Text})
	}
	return result
}

// collectEventsUnitState는 지금 failed인 unit과 창 안의 오류 줄을 읽는다. PID 1과 커널 기록과 따로 읽어,
// unit 실패가 창 밖이어도 지금 상태로 보이고 일반 서비스의 오류 줄도 모인다.
func collectEventsUnitState(ctx context.Context, sources eventsSources, facts *eventsFacts) {
	if sources.failedUnits != nil {
		facts.FailedChecked = true
		output, err := sources.failedUnits(ctx)
		facts.FailedErr = err
		if err == nil {
			facts.FailedUnits = parseFailedUnits(output)
		}
	}
	if sources.journalErrors == nil {
		return
	}
	facts.ErrorChecked = true
	reader, warnings, err := sources.journalErrors(ctx, facts.Start)
	facts.Warnings = append(facts.Warnings, warnings...)
	if err != nil {
		facts.ErrorErr = err
		return
	}
	entries, truncated, err := readJournalEntries(reader, eventsJournalLimit)
	if truncated {
		facts.Warnings = append(facts.Warnings, T("events.window.truncated", "journalctl -p err"))
	}
	if err != nil {
		facts.ErrorErr = err
		return
	}
	facts.ErrorSources, facts.ErrorLines = groupErrorEntries(entries, facts.Start)
}

// parseFailedUnits는 list-units --plain --no-legend 출력을 읽는다. 열은 UNIT LOAD ACTIVE SUB DESCRIPTION이다.
func parseFailedUnits(output string) []eventsFailedUnit {
	var units []eventsFailedUnit
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// 오래된 systemctl은 --plain에서도 failed unit 앞에 ●를 붙인다.
		if fields[0] == "●" || fields[0] == "*" {
			fields = fields[1:]
			if len(fields) < 4 {
				continue
			}
		}
		units = append(units, eventsFailedUnit{Unit: fields[0], Load: fields[1], Active: fields[2], Sub: fields[3], Description: strings.Join(fields[4:], " ")})
	}
	return units
}

// groupErrorEntries는 오류 줄을 unit별로 센다. 커널 기록은 앞의 범주가 이미 보므로 뺀다. unit이 없는 기록은 syslog 이름으로 묶는다.
func groupErrorEntries(entries []journalEntry, start time.Time) ([]eventsErrorSource, int) {
	byName := map[string]*eventsErrorSource{}
	total := 0
	for _, entry := range entries {
		if entry.Transport == "kernel" || time.UnixMicro(entry.Realtime).Before(start) {
			continue
		}
		name := entry.SystemdUnit
		if name == "" {
			name = entry.Identifier
		}
		if name == "" {
			name = "-"
		}
		source := byName[name]
		if source == nil {
			source = &eventsErrorSource{Name: name}
			byName[name] = source
		}
		source.Count++
		if entry.Realtime >= source.Latest.Realtime {
			source.Latest = entry
		}
		total++
	}
	sources := make([]eventsErrorSource, 0, len(byName))
	for _, source := range byName {
		sources = append(sources, *source)
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Count != sources[j].Count {
			return sources[i].Count > sources[j].Count
		}
		return sources[i].Name < sources[j].Name
	})
	return sources, total
}

// eventsFailedResult는 지금 failed인 unit이다. 창과 상관없어서, 며칠 전에 죽어 아직 failed인 unit도 보인다.
func eventsFailedResult(facts eventsFacts) Result {
	const id = eventsFailed
	switch {
	case !facts.FailedChecked:
		return eventsSkip(id, "systemctl", errEventsNoSystemd)
	case facts.FailedErr != nil:
		return eventsSkip(id, "systemctl", facts.FailedErr)
	case len(facts.FailedUnits) == 0:
		return eventsResult(id, StatusPass, T("events.failed_units.none"))
	}
	names := make([]string, 0, len(facts.FailedUnits))
	lines := make([]string, 0, len(facts.FailedUnits))
	for _, unit := range facts.FailedUnits {
		names = append(names, unit.Unit)
		lines = append(lines, strings.TrimSpace(fmt.Sprintf("%s  %s/%s/%s  %s", unit.Unit, unit.Load, unit.Active, unit.Sub, unit.Description)))
	}
	shown := names
	if len(shown) > svcListenSummaryMax {
		shown = append(append([]string{}, shown[:svcListenSummaryMax]...), fmt.Sprintf("+%d", len(names)-svcListenSummaryMax))
	}
	result := eventsResult(id, StatusFail, T("events.failed_units.found", len(names), strings.Join(shown, ", ")))
	result.Error = &DiagnosticError{Kind: "events", Message: result.Summary}
	result.Evidence = []Evidence{{Label: T("events.evidence.failed_units"), Value: strings.Join(lines, "\n")}}
	result.Metrics = map[string]interface{}{"units": names}
	result.Next = "systemctl status " + names[0]
	return result
}

// eventsErrorsResult는 창 안의 오류 줄(priority err 이상)을 unit별로 센다. 오류 줄은 실패가 아니라 단서라 warn이다.
func eventsErrorsResult(facts eventsFacts) Result {
	const id = eventsErrors
	switch {
	case !facts.ErrorChecked:
		return eventsSkip(id, "journal", errEventsNoJournal)
	case facts.ErrorErr != nil:
		return eventsSkip(id, "journal", facts.ErrorErr)
	}
	start := facts.Start.Format(eventsTimeLayout)
	if facts.ErrorLines == 0 {
		return eventsResult(id, StatusPass, T("events.journal_errors.none", start))
	}
	top := facts.ErrorSources[0]
	result := eventsResult(id, StatusWarn, T("events.journal_errors.found", facts.ErrorLines, len(facts.ErrorSources), top.Name, top.Count))
	lines := make([]string, 0, eventsErrorUnits+1)
	counts := map[string]int{}
	for index, source := range facts.ErrorSources {
		counts[source.Name] = source.Count
		if index >= eventsErrorUnits {
			continue
		}
		at := time.UnixMicro(source.Latest.Realtime).In(facts.Now.Location()).Format(eventsTimeLayout)
		lines = append(lines, fmt.Sprintf("%5d  %s  %s  %s", source.Count, source.Name, at, svcTruncate(firstLine(source.Latest.Message), svcJournalLineRunes)))
	}
	if hidden := len(facts.ErrorSources) - eventsErrorUnits; hidden > 0 {
		lines = append(lines, T("events.evidence.more", hidden))
	}
	result.Evidence = []Evidence{{Label: T("events.evidence.error_sources"), Value: strings.Join(lines, "\n")}}
	result.Metrics = map[string]interface{}{"lines": facts.ErrorLines, "sources": counts}
	if strings.Contains(top.Name, ".") {
		result.Next = fmt.Sprintf("journalctl -u %s -p err --since @%d", top.Name, facts.Start.Unix())
	} else {
		result.Next = fmt.Sprintf("journalctl -t %s -p err --since @%d", top.Name, facts.Start.Unix())
	}
	return result
}
