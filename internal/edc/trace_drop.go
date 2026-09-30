package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type drop_record dropEvents drop_events_bpf.c -- -I./bpf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// dropRecordSize는 drop_events_bpf.c의 struct drop_record 크기다.
	dropRecordSize = 72
	// dropEventLimit는 CPU마다 1초에 보내는 event의 상한이다. 넘으면 이유별 합계만 센다.
	dropEventLimit = 1000
	// dropReasonMax는 BPF의 이유별 counter 크기(DROP_REASON_MAX)다.
	dropReasonMax    = 512
	dropReasonPrefix = "SKB_DROP_REASON_"
)

type dropRecord struct {
	bootTimeNS  uint64
	location    uint64
	socket      uint64
	reason      uint32
	length      uint32
	protocol    uint16
	l4          uint8
	family      uint8
	sport       uint16
	dport       uint16
	source      [16]byte
	destination [16]byte
}

func parseDropRecord(sample []byte) (dropRecord, bool) {
	if len(sample) < dropRecordSize {
		return dropRecord{}, false
	}
	record := dropRecord{
		bootTimeNS: binary.LittleEndian.Uint64(sample[0:8]),
		location:   binary.LittleEndian.Uint64(sample[8:16]),
		socket:     binary.LittleEndian.Uint64(sample[16:24]),
		reason:     binary.LittleEndian.Uint32(sample[24:28]),
		length:     binary.LittleEndian.Uint32(sample[28:32]),
		protocol:   binary.LittleEndian.Uint16(sample[32:34]),
		l4:         sample[34],
		family:     sample[35],
		sport:      binary.LittleEndian.Uint16(sample[36:38]),
		dport:      binary.LittleEndian.Uint16(sample[38:40]),
	}
	copy(record.source[:], sample[40:56])
	copy(record.destination[:], sample[56:72])
	return record, true
}

// dropReasons는 kernel의 enum skb_drop_reason이다. 번호는 kernel마다 달라서 kernel BTF에서 읽는다.
type dropReasons struct {
	names  map[uint32]string
	values map[string]uint32
}

// newDropReasons는 enum의 이름과 값에서 SKB_DROP_REASON_ 앞부분을 뺀 이름표를 만든다.
func newDropReasons(values map[string]uint64) dropReasons {
	reasons := dropReasons{names: map[uint32]string{}, values: map[string]uint32{}}
	for name, value := range values {
		short := strings.TrimPrefix(name, dropReasonPrefix)
		reasons.names[uint32(value)] = short
		reasons.values[short] = uint32(value)
	}
	return reasons
}

// name은 이유 번호의 이름이다. 5.17 전 kernel은 이유를 넘기지 않아 모두 unknown이다.
func (reasons dropReasons) name(value uint32) string {
	if name, ok := reasons.names[value]; ok {
		return name
	}
	if len(reasons.names) == 0 {
		return "unknown"
	}
	return "reason " + strconv.FormatUint(uint64(value), 10)
}

// selectNames는 --reason 값을 이유 번호로 바꾼다. 대소문자와 SKB_DROP_REASON_ 앞부분은 가리지 않는다.
func (reasons dropReasons) selectNames(names []string) ([]uint32, error) {
	var selected []uint32
	for _, name := range names {
		short := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), dropReasonPrefix)
		value, ok := reasons.values[short]
		if !ok || value >= dropReasonMax {
			return nil, errors.New(T("cli.trace.drop_reason_unknown", name, len(reasons.values)))
		}
		selected = append(selected, value)
	}
	return selected, nil
}

// splitDropReasons는 쉼표로 나눈 --reason 값이다.
func splitDropReasons(value string) []string {
	var names []string
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// parseNetstat는 /proc/net/netstat에서 prefix(예: TcpExt) 줄의 이름과 값을 읽는다. 이 파일은 이름 줄과 값 줄이 짝을 이룬다.
func parseNetstat(data []byte, prefix string) map[string]uint64 {
	counters := map[string]uint64{}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != prefix+":" {
			continue
		}
		if names == nil {
			names = fields[1:]
			continue
		}
		for index, value := range fields[1:] {
			if index >= len(names) {
				break
			}
			if parsed, err := strconv.ParseUint(value, 10, 64); err == nil {
				counters[names[index]] = parsed
			}
		}
		names = nil
	}
	return counters
}

// kernelSymbols는 /proc/kallsyms의 함수 주소와 이름이다. location은 kfree_skb를 부른 함수 안의 주소다.
type kernelSymbols struct {
	addresses []uint64
	names     []string
}

// parseKallsyms는 text 영역의 symbol만 읽는다. 권한이 없어 주소가 모두 0이면 이름을 찾지 못한다.
func parseKallsyms(reader io.Reader) kernelSymbols {
	type symbol struct {
		address uint64
		name    string
	}
	var symbols []symbol
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || (fields[1] != "t" && fields[1] != "T") {
			continue
		}
		address, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil || address == 0 {
			continue
		}
		symbols = append(symbols, symbol{address: address, name: fields[2]})
	}
	sort.Slice(symbols, func(i, j int) bool { return symbols[i].address < symbols[j].address })
	table := kernelSymbols{addresses: make([]uint64, len(symbols)), names: make([]string, len(symbols))}
	for index, symbol := range symbols {
		table.addresses[index], table.names[index] = symbol.address, symbol.name
	}
	return table
}

// name은 address를 포함하는 함수의 이름이다. 가장 가까운 앞 symbol을 쓴다.
func (symbols kernelSymbols) name(address uint64) string {
	index := sort.Search(len(symbols.addresses), func(i int) bool { return symbols.addresses[i] > address }) - 1
	if index < 0 || address == 0 {
		return ""
	}
	return symbols.names[index]
}

// dropOwner는 버려진 패킷의 local socket을 가진 process다.
type dropOwner struct {
	pid     uint32
	process string
	cgroup  uint64
}

// etherTypeNames는 IP가 아닌 패킷의 이름이다. 목적지 칸에 쓴다.
var etherTypeNames = map[uint16]string{0x0004: "llc", 0x0806: "arp", 0x86dd: "ipv6", 0x0800: "ipv4", 0x88cc: "lldp", 0x8100: "vlan", 0x88a8: "qinq", 0x8847: "mpls"}

// traceDropScrollLabels는 목적지 칸에 패킷의 목적지를, event 칸에 버린 이유를 쓴다.
func traceDropScrollLabels(event captureEvent) (string, string) {
	destination := event.Destination
	if destination == "" {
		destination = emptyAs(event.Target, "-")
	}
	return destination, emptyAs(event.Reason, event.Event)
}

type dropTraceRow struct {
	Reason    string   `json:"reason"`
	Location  string   `json:"location,omitempty"`
	Events    uint64   `json:"events"`
	Bytes     uint64   `json:"bytes"`
	Processes []string `json:"processes,omitempty"`
	Example   string   `json:"example,omitempty"`
}

type dropReasonTotal struct {
	Reason string `json:"reason"`
	Drops  uint64 `json:"drops"`
}

type dropTraceReport struct {
	DurationMS int64  `json:"duration_ms"`
	Drops      uint64 `json:"drops"`
	Events     uint64 `json:"events"`
	Sampled    uint64 `json:"sampled_out"`
	LostEvents uint64 `json:"lost_events"`
	// ListenOverflows와 ListenDrops는 captureSummary의 같은 값이다. 읽지 못했거나 --reason이 다른 이유만 고르면 없다.
	ListenOverflows *uint64           `json:"listen_overflows,omitempty"`
	ListenDrops     *uint64           `json:"listen_drops,omitempty"`
	Reasons         []dropReasonTotal `json:"reasons"`
	Rows            []dropTraceRow    `json:"rows"`
}

type dropRowKey struct{ reason, location string }

// dropTraceSummarizer는 --group-by 없이 끝난 drop trace를 이유와 위치마다 한 행으로 묶는다. 이유별 합계는 BPF counter의
// 정확한 값을 쓰고, 행은 받은 event로 만든다. event는 상한을 넘으면 표본이다.
type dropTraceSummarizer struct {
	rows      map[dropRowKey]*dropTraceRow
	processes map[dropRowKey]map[string]bool
	events    uint64
}

func newDropTraceSummarizer() *dropTraceSummarizer {
	return &dropTraceSummarizer{rows: map[dropRowKey]*dropTraceRow{}, processes: map[dropRowKey]map[string]bool{}}
}

func (summarizer *dropTraceSummarizer) observe(event captureEvent) {
	summarizer.events++
	key := dropRowKey{reason: event.Reason, location: event.Location}
	row := summarizer.rows[key]
	if row == nil {
		row = &dropTraceRow{Reason: event.Reason, Location: event.Location}
		summarizer.rows[key], summarizer.processes[key] = row, map[string]bool{}
		row.Example = event.Destination
		if event.Source != "" {
			row.Example = event.Source + " > " + event.Destination
		}
	}
	row.Events++
	row.Bytes += event.Bytes
	if event.Process != "" {
		summarizer.processes[key][event.Process] = true
	}
}

func (summarizer *dropTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := dropTraceReport{DurationMS: duration.Milliseconds(), Events: summarizer.events, Sampled: summary.DropSampled, LostEvents: summary.LostEvents,
		ListenOverflows: summary.ListenOverflows, ListenDrops: summary.ListenDrops}
	for reason, drops := range summary.DropCounts {
		report.Drops += drops
		report.Reasons = append(report.Reasons, dropReasonTotal{Reason: reason, Drops: drops})
	}
	sort.Slice(report.Reasons, func(i, j int) bool {
		if report.Reasons[i].Drops != report.Reasons[j].Drops {
			return report.Reasons[i].Drops > report.Reasons[j].Drops
		}
		return report.Reasons[i].Reason < report.Reasons[j].Reason
	})
	for key, row := range summarizer.rows {
		for process := range summarizer.processes[key] {
			row.Processes = append(row.Processes, process)
		}
		sort.Strings(row.Processes)
		report.Rows = append(report.Rows, *row)
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		left, right := report.Rows[i], report.Rows[j]
		if left.Events != right.Events {
			return left.Events > right.Events
		}
		if left.Reason != right.Reason {
			return left.Reason < right.Reason
		}
		return left.Location < right.Location
	})
	return report
}

// -d는 연결마다 한 행을 쓰는 option이다. drop 요약은 이미 이유와 위치마다 한 행이라 같은 표를 쓴다.
func (report dropTraceReport) print(bool) {
	fmt.Fprintf(os.Stdout, "DROP trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Drops: %d\nEvents: %d\nSampled out: %d\nLost events: %d\n", report.Drops, report.Events, report.Sampled, report.LostEvents)
	if report.ListenOverflows != nil && report.ListenDrops != nil {
		fmt.Fprintf(os.Stdout, "Listen overflows: %d\nListen drops: %d (TcpExt in /proc/net/netstat, all listen sockets in this network namespace)\n", *report.ListenOverflows, *report.ListenDrops)
	}
	if len(report.Reasons) > 0 {
		fmt.Fprintln(os.Stdout, "\nREASON\tDROPS")
		for _, reason := range report.Reasons {
			fmt.Fprintf(os.Stdout, "%s\t%d\n", reason.Reason, reason.Drops)
		}
	}
	if len(report.Rows) > 0 {
		fmt.Fprintln(os.Stdout, "\nREASON\tWHERE\tEVENTS\tBYTES\tPROCESSES\tEXAMPLE")
		for _, row := range report.Rows {
			fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%s\t%s\t%s\n", row.Reason, emptyAs(row.Location, "-"), row.Events, traceBytes(row.Bytes), emptyAs(strings.Join(row.Processes, ","), "-"), emptyAs(row.Example, "-"))
		}
	}
}
