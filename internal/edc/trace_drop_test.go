package edc

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestDropRecordOffsetsMatchTheBPFStruct(t *testing.T) {
	var record dropEventsDropRecord
	for name, offsets := range map[string][2]uintptr{
		"location": {unsafe.Offsetof(record.Location), 8},
		"sock_ino": {unsafe.Offsetof(record.SockIno), 16},
		"reason":   {unsafe.Offsetof(record.Reason), 24},
		"len":      {unsafe.Offsetof(record.Len), 28},
		"protocol": {unsafe.Offsetof(record.Protocol), 32},
		"l4":       {unsafe.Offsetof(record.L4), 34},
		"family":   {unsafe.Offsetof(record.Family), 35},
		"sport":    {unsafe.Offsetof(record.Sport), 36},
		"dport":    {unsafe.Offsetof(record.Dport), 38},
		"saddr":    {unsafe.Offsetof(record.Saddr), 40},
		"daddr":    {unsafe.Offsetof(record.Daddr), 56},
		"size":     {unsafe.Sizeof(record), dropRecordSize},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parseDropRecord reads %d", name, offsets[0], offsets[1])
		}
	}
}

func dropSample(reason uint32, family uint8, source, destination []byte, sport, dport uint16) []byte {
	sample := make([]byte, dropRecordSize)
	binary.LittleEndian.PutUint64(sample[0:8], 1000)
	binary.LittleEndian.PutUint64(sample[8:16], 0xffffffff81000123)
	binary.LittleEndian.PutUint64(sample[16:24], 4242)
	binary.LittleEndian.PutUint32(sample[24:28], reason)
	binary.LittleEndian.PutUint32(sample[28:32], 60)
	binary.LittleEndian.PutUint16(sample[32:34], 0x0800)
	sample[34] = 6
	sample[35] = family
	binary.LittleEndian.PutUint16(sample[36:38], sport)
	binary.LittleEndian.PutUint16(sample[38:40], dport)
	copy(sample[40:56], source)
	copy(sample[56:72], destination)
	return sample
}

func TestParseDropRecordReadsTheFields(t *testing.T) {
	record, ok := parseDropRecord(dropSample(3, 4, []byte{10, 0, 0, 9}, []byte{10, 0, 0, 5}, 40022, 9999))
	if !ok {
		t.Fatal("record is not parsed")
	}
	if record.location != 0xffffffff81000123 || record.socket != 4242 || record.reason != 3 || record.length != 60 || record.protocol != 0x0800 || record.l4 != 6 || record.family != 4 || record.sport != 40022 || record.dport != 9999 || record.source[0] != 10 || record.destination[3] != 5 {
		t.Fatalf("record = %+v", record)
	}
	if _, ok := parseDropRecord(make([]byte, dropRecordSize-1)); ok {
		t.Fatal("a short sample must not parse")
	}
}

func TestDropReasonsUseTheKernelNames(t *testing.T) {
	reasons := newDropReasons(map[string]uint64{"SKB_NOT_DROPPED_YET": 0, "SKB_DROP_REASON_NO_SOCKET": 3, "SKB_DROP_REASON_TCP_LISTEN_OVERFLOW": 40})
	if reasons.name(3) != "NO_SOCKET" || reasons.name(0) != "SKB_NOT_DROPPED_YET" || reasons.name(99) != "reason 99" {
		t.Fatalf("names = %q, %q, %q", reasons.name(3), reasons.name(0), reasons.name(99))
	}
	selected, err := reasons.selectNames([]string{"no_socket", " SKB_DROP_REASON_TCP_LISTEN_OVERFLOW "})
	if err != nil || len(selected) != 2 || selected[0] != 3 || selected[1] != 40 {
		t.Fatalf("selected = %v, %v", selected, err)
	}
	if _, err := reasons.selectNames([]string{"BOGUS"}); err == nil || !strings.Contains(err.Error(), "BOGUS") {
		t.Fatalf("unknown reason = %v", err)
	}
	// 5.17 전 kernel에는 enum이 없어 모든 event가 unknown이다.
	if old := newDropReasons(nil); old.name(0) != "unknown" {
		t.Fatalf("old kernel name = %q", old.name(0))
	}
}

func TestSplitDropReasonsDropsEmptyNames(t *testing.T) {
	if got := strings.Join(splitDropReasons(" NO_SOCKET,,tcp_reset , "), "|"); got != "NO_SOCKET|tcp_reset" {
		t.Fatalf("split = %q", got)
	}
	if len(splitDropReasons("")) != 0 {
		t.Fatal("an empty value selects nothing")
	}
}

func TestParseNetstatReadsThePairedLines(t *testing.T) {
	counters := parseNetstat([]byte(`TcpExt: SyncookiesSent ListenOverflows ListenDrops
TcpExt: 0 1891 1893
IpExt: InNoRoutes ListenOverflows
IpExt: 7 99
`), "TcpExt")
	if counters["ListenOverflows"] != 1891 || counters["ListenDrops"] != 1893 || counters["SyncookiesSent"] != 0 || len(counters) != 3 {
		t.Fatalf("counters = %v", counters)
	}
	if len(parseNetstat([]byte("TcpExt: A B\n"), "TcpExt")) != 0 {
		t.Fatal("a name line without values gives no counters")
	}
}

func TestParseKallsymsFindsTheContainingFunction(t *testing.T) {
	symbols := parseKallsyms(strings.NewReader(`ffffffff81000000 T _stext
ffffffff81100000 t tcp_v4_rcv
ffffffff81100800 T tcp_v4_do_rcv
ffffffff81200000 D some_data
0000000000000000 T hidden
`))
	for address, want := range map[uint64]string{0xffffffff81100123: "tcp_v4_rcv", 0xffffffff81100800: "tcp_v4_do_rcv", 0xffffffff81250000: "tcp_v4_do_rcv", 0x1000: "", 0: ""} {
		if got := symbols.name(address); got != want {
			t.Fatalf("name(%#x) = %q, want %q", address, got, want)
		}
	}
}

func TestTraceDropScrollLabelsShowTheReason(t *testing.T) {
	destination, label := traceDropScrollLabels(captureEvent{Event: "drop_no_socket", Reason: "NO_SOCKET", Destination: "10.0.0.5:9999"})
	if destination != "10.0.0.5:9999" || label != "NO_SOCKET" {
		t.Fatalf("labels = %q, %q", destination, label)
	}
	if destination, _ := traceDropScrollLabels(captureEvent{Reason: "NOT_SPECIFIED", Target: "llc"}); destination != "llc" {
		t.Fatalf("non-IP destination = %q", destination)
	}
}

func TestDropTraceSummarizerUsesTheExactCounts(t *testing.T) {
	summarizer := newDropTraceSummarizer()
	for _, event := range []captureEvent{
		{Reason: "NO_SOCKET", Location: "tcp_v4_rcv", Source: "10.0.0.9:40022", Destination: "10.0.0.5:9999", Bytes: 40},
		{Reason: "NO_SOCKET", Location: "tcp_v4_rcv", Destination: "10.0.0.5:9999", Bytes: 40},
		{Reason: "SOCKET_RCVBUFF", Location: "udp_queue_rcv_one_skb", Process: "slowreader", Bytes: 512},
	} {
		summarizer.observe(event)
	}
	overflows, drops := uint64(124), uint64(126)
	report := summarizer.summarize(captureSummary{DropCounts: map[string]uint64{"NO_SOCKET": 130008, "SOCKET_RCVBUFF": 1998}, DropSampled: 128003, ListenOverflows: &overflows, ListenDrops: &drops}, time.Second).(dropTraceReport)
	if report.Drops != 132006 || report.Events != 3 || report.Sampled != 128003 || len(report.Reasons) != 2 || report.Reasons[0].Reason != "NO_SOCKET" || *report.ListenOverflows != 124 || *report.ListenDrops != 126 {
		t.Fatalf("report = %+v", report)
	}
	if first := report.Rows[0]; first.Reason != "NO_SOCKET" || first.Events != 2 || first.Bytes != 80 || first.Example != "10.0.0.9:40022 > 10.0.0.5:9999" {
		t.Fatalf("first row = %+v", first)
	}
	if second := report.Rows[1]; strings.Join(second.Processes, ",") != "slowreader" {
		t.Fatalf("second row = %+v", second)
	}
}
