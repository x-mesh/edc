package edc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
)

var eventsTestNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func eventsResultByID(t *testing.T, results []Result, id string) Result {
	t.Helper()
	for _, result := range results {
		if result.Probe == "events."+id {
			return result
		}
	}
	t.Fatalf("events.%s가 없다", id)
	return Result{}
}

func eventsEvidenceValue(result Result, label string) string {
	for _, evidence := range result.Evidence {
		if evidence.Label == T(label) {
			return evidence.Value
		}
	}
	return ""
}

// journalLine은 journalctl -o json 한 줄을 만든다. at은 eventsTestNow에서 거꾸로 잰 시각이다.
func journalLine(boot string, ago, monotonic time.Duration, transport, messageID, unit, message string) string {
	realtime := eventsTestNow.Add(-ago).UnixMicro()
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d","__MONOTONIC_TIMESTAMP":"%d","_BOOT_ID":%q,"_TRANSPORT":%q,"MESSAGE_ID":%q,"UNIT":%q,"MESSAGE":%q}`,
		realtime, monotonic.Microseconds(), boot, transport, messageID, unit, message)
}

func kernelJournalLine(ago time.Duration, message string) string {
	return journalLine("boot1", ago, 24*time.Hour-ago, "kernel", "", "", message)
}

func eventsTestSources(journal string, journalErr error, kmsg []string, kmsgErr error) eventsSources {
	return eventsSources{
		now:  eventsTestNow,
		boot: eventsTestNow.Add(-24 * time.Hour),
		kmsg: func(context.Context) ([]string, error) { return kmsg, kmsgErr },
		journal: func(context.Context, time.Time) (io.Reader, []string, error) {
			if journalErr != nil {
				return nil, nil, journalErr
			}
			return strings.NewReader(journal), nil, nil
		},
	}
}

func evaluateEventsFixture(sources eventsSources) []Result {
	return evaluateEvents(collectEventsFacts(context.Background(), sources, 6*time.Hour))
}

func TestEventsKernelMatchersReadRealLines(t *testing.T) {
	cases := []struct {
		message, kind, brief string
	}{
		{"Out of memory: Killed process 4242 (java) total-vm:8123456kB, anon-rss:4012345kB, file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:9000kB oom_score_adj:0", eventsOOM, "java pid=4242 cgroup=?"},
		{"Memory cgroup out of memory: Killed process 777 (node) total-vm:1234kB, anon-rss:512kB", eventsOOM, "node pid=777 cgroup=?"},
		// 4.x 커널은 Kill process로 쓴다.
		{"Out of memory: Kill process 99 (mysqld) score 901 or sacrifice child", eventsOOM, "mysqld pid=99 cgroup=?"},
		{"nginx[1234]: segfault at 0 ip 000055d5c0a1b2c3 sp 00007ffd2b3c4d50 error 4 in nginx[55d5c0a00000+100000] likely on CPU 3 (core 3, socket 0)", eventsSegfault, "nginx pid=1234 segfault"},
		{"traps: python3[5678] general protection fault ip:7f1e2d3c4b5a sp:7ffc1a2b3c4d error:0 in libc.so.6[7f1e2d200000+195000]", eventsSegfault, "python3 pid=5678 general protection"},
		{"blk_update_request: I/O error, dev sda, sector 2048 op 0x0:(READ) flags 0x0 phys_seg 1 prio class 0", eventsDiskIO, "dev=sda"},
		{"I/O error, dev nvme0n1, sector 123456 op 0x1:(WRITE) flags 0x800 phys_seg 1 prio class 2", eventsDiskIO, "dev=nvme0n1"},
		{"Buffer I/O error on dev sdb1, logical block 0, async page read", eventsDiskIO, "dev=sdb1"},
		{"critical medium error, dev sdc, sector 99 op 0x0:(READ) flags 0x0 phys_seg 1 prio class 0", eventsDiskIO, "dev=sdc"},
		{"XFS (dm-2): metadata I/O error in \"xfs_trans_read_buf_map\" at daddr 0x2 len 1 error 5", eventsDiskIO, "dev=dm-2"},
		{"EXT4-fs (sda1): Remounting filesystem read-only", eventsFSReadonly, "dev=sda1"},
		{"BTRFS info (device sdd1): forced readonly", eventsFSReadonly, "dev=sdd1"},
		{"XFS (sdb1): Log I/O Error Detected. Shutting down filesystem", "", ""},
		{"XFS (sdb2): Filesystem has been shut down due to log error (0x2).", eventsFSReadonly, "dev=sdb2"},
		{"e1000e 0000:00:1f.6 eno1: NIC Link is Down", eventsLink, "eno1 down"},
		{"igb 0000:01:00.0 eth1: igb: eth1 NIC Link is Up 1000 Mbps Full Duplex, Flow Control: RX/TX", eventsLink, "eth1 up"},
		{"r8169 0000:02:00.0 enp2s0: Link is Down", eventsLink, "enp2s0 down"},
		{"IPv6: ADDRCONF(NETDEV_CHANGE): ens3: link becomes ready", eventsLink, "ens3 up"},
		{"systemd-journald[300]: Received SIGTERM", "", ""},
		{"audit: type=1400 audit(1700000000.123:45): apparmor=\"DENIED\"", "", ""},
	}
	for _, test := range cases {
		signals := matchKernelLines([]kernelLine{{Time: eventsTestNow, Message: test.message}})
		if test.kind == "" {
			if len(signals) != 0 {
				t.Errorf("%q를 신호로 읽었다: %+v", test.message, signals)
			}
			continue
		}
		if len(signals) != 1 || signals[0].Kind != test.kind || signals[0].Brief != test.brief {
			t.Errorf("%q = %+v, want %s %q", test.message, signals, test.kind, test.brief)
		}
	}
}

func TestEventsOOMPairsTheCgroupByPID(t *testing.T) {
	lines := []kernelLine{
		{Time: eventsTestNow, Message: "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/system.slice/app.service,task_memcg=/system.slice/app.service,task=java,pid=4242,uid=1000"},
		{Time: eventsTestNow, Message: "Memory cgroup out of memory: Killed process 4242 (java) total-vm:8123456kB"},
		// oom-kill 줄이 빠진 kill은 cgroup을 모른다고 적는다.
		{Time: eventsTestNow, Message: "Out of memory: Killed process 31 (cron) total-vm:1kB"},
	}
	signals := matchKernelLines(lines)
	if len(signals) != 2 {
		t.Fatalf("signals = %+v", signals)
	}
	if signals[0].Brief != "java pid=4242 cgroup=/system.slice/app.service" || signals[1].Brief != "cron pid=31 cgroup=?" {
		t.Fatalf("briefs = %q, %q", signals[0].Brief, signals[1].Brief)
	}
}

func TestParseKmsgRecord(t *testing.T) {
	record, ok := parseKmsgRecord("3,1234,5678901,-,caller=T42;EXT4-fs (sda1): Remounting filesystem read-only\n SUBSYSTEM=block\n DEVICE=b8:1\n")
	if !ok || record.Priority != 3 || record.Sequence != 1234 || record.Micros != 5678901 || record.Message != "EXT4-fs (sda1): Remounting filesystem read-only" {
		t.Fatalf("record = %+v, %v", record, ok)
	}
	for _, bad := range []string{
		"",
		" SUBSYSTEM=block",
		"6,1,2,-",
		"6,1,abc,-;message",
		"x,1,2,-;message",
		"6,1;message",
		"6,1,-5,-;message",
	} {
		if record, ok := parseKmsgRecord(bad); ok {
			t.Errorf("%q를 읽었다: %+v", bad, record)
		}
	}
}

func TestParseJournalLineDecodesByteArrayMessages(t *testing.T) {
	entry, ok := parseJournalLine([]byte(`{"__REALTIME_TIMESTAMP":"1760270400000000","__MONOTONIC_TIMESTAMP":"5000000","_BOOT_ID":"abc","_TRANSPORT":"kernel","MESSAGE":[73,47,79,32,101,114,114,111,114,255]}`))
	if !ok || entry.Realtime != 1760270400000000 || entry.Monotonic != 5000000 || entry.Boot != "abc" || entry.Transport != "kernel" || !strings.HasPrefix(entry.Message, "I/O error") {
		t.Fatalf("entry = %+v, %v", entry, ok)
	}
	entry, ok = parseJournalLine([]byte(`{"__REALTIME_TIMESTAMP":"1","__MONOTONIC_TIMESTAMP":"2","UNIT":["a.service","b.service"],"MESSAGE":null}`))
	if !ok || entry.Unit != "a.service" || entry.Message != "" {
		t.Fatalf("entry = %+v, %v", entry, ok)
	}
	for _, bad := range []string{`not json`, `{"MESSAGE":"x"}`, `{"__REALTIME_TIMESTAMP":"x","__MONOTONIC_TIMESTAMP":"1"}`, `{"__REALTIME_TIMESTAMP":"1"}`} {
		if entry, ok := parseJournalLine([]byte(bad)); ok {
			t.Errorf("%q를 읽었다: %+v", bad, entry)
		}
	}
}

func TestReadJournalEntriesStopsAtTheLimits(t *testing.T) {
	line := kernelJournalLine(time.Hour, "hello") + "\n"
	entries, truncated, err := readJournalEntries(strings.NewReader(line+line), eventsJournalLimit)
	if err != nil || truncated || len(entries) != 2 {
		t.Fatalf("entries = %d, truncated = %v, err = %v", len(entries), truncated, err)
	}
	entries, truncated, err = readJournalEntries(strings.NewReader(strings.Repeat(line, 10)), int64(len(line)*3+5))
	if err != nil || !truncated || len(entries) != 3 {
		t.Fatalf("limit: entries = %d, truncated = %v, err = %v", len(entries), truncated, err)
	}
	long := `{"MESSAGE":"` + strings.Repeat("x", eventsJournalLineLimit) + `"}` + "\n"
	entries, truncated, err = readJournalEntries(strings.NewReader(line+long+line), eventsJournalLimit)
	if err != nil || !truncated || len(entries) != 1 {
		t.Fatalf("long line: entries = %d, truncated = %v, err = %v", len(entries), truncated, err)
	}
}

func TestEventsTruncatedJournalWarnsOnTheWindow(t *testing.T) {
	long := `{"MESSAGE":"` + strings.Repeat("x", eventsJournalLineLimit) + `"}`
	results := evaluateEventsFixture(eventsTestSources(kernelJournalLine(time.Hour, "r8169 eth0: Link is Down")+"\n"+long+"\n", nil, nil, errors.New("unused")))
	window := eventsResultByID(t, results, "window")
	if window.Status != StatusPass || len(window.Warnings) != 1 || !strings.Contains(window.Warnings[0], "journalctl") {
		t.Fatalf("window = %+v", window)
	}
	if link := eventsResultByID(t, results, "link"); link.Status != StatusFail {
		t.Fatalf("link = %+v", link)
	}
}

func TestReadKmsgRecordsEndsOnEAGAINAndSkipsEPIPE(t *testing.T) {
	steps := []struct {
		data string
		err  error
	}{
		{"6,1,10,-;first", nil},
		{"", syscall.EPIPE},
		{"6,3,30,-;third", nil},
		{"", syscall.EAGAIN},
		{"6,4,40,-;never", nil},
	}
	index := 0
	records, err := readKmsgRecords(context.Background(), func(buffer []byte) (int, error) {
		step := steps[index]
		index++
		return copy(buffer, step.data), step.err
	})
	if err != nil || len(records) != 2 || records[1] != "6,3,30,-;third" {
		t.Fatalf("records = %q, err = %v", records, err)
	}

	// 끝없이 EPIPE를 주는 원천도 상한에서 멈춘다.
	calls := 0
	records, err = readKmsgRecords(context.Background(), func([]byte) (int, error) {
		calls++
		return 0, syscall.EPIPE
	})
	if err != nil || len(records) != 0 || calls != eventsKmsgMaxRecords {
		t.Fatalf("records = %d, calls = %d, err = %v", len(records), calls, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readKmsgRecords(ctx, func([]byte) (int, error) { return 0, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestEventsFallsBackToKmsgAndFiltersTheWindow(t *testing.T) {
	kmsg := []string{
		// 부팅 뒤 1시간이라 창(최근 6시간)보다 앞이다.
		"6,1,3600000000,-;Out of memory: Killed process 1 (old) total-vm:1kB",
		"3,2,86000000000,-;Out of memory: Killed process 2 (new) total-vm:1kB",
		"bad record",
	}
	results := evaluateEventsFixture(eventsTestSources("", errEventsNoJournal, kmsg, nil))
	oom := eventsResultByID(t, results, "oom")
	if oom.Status != StatusFail || !strings.Contains(oom.Summary, "new pid=2") || strings.Contains(eventsEvidenceValue(oom, "events.evidence.events"), "old") {
		t.Fatalf("oom = %+v", oom)
	}
	if oom.Next != "dmesg -T" {
		t.Fatalf("next = %q", oom.Next)
	}
	for _, id := range []string{"units", "clock"} {
		if result := eventsResultByID(t, results, id); result.Status != StatusSkip || result.Summary != T("events.skip.no_journal") {
			t.Fatalf("%s = %+v", id, result)
		}
	}
	window := eventsResultByID(t, results, "window")
	if window.Status != StatusPass || !strings.Contains(eventsEvidenceValue(window, "events.evidence.source"), "kernel: /dev/kmsg") {
		t.Fatalf("window = %+v", window)
	}
}

func TestEventsPrefersJournalKernelEntries(t *testing.T) {
	journal := strings.Join([]string{
		kernelJournalLine(2*time.Hour, "EXT4-fs (sda1): Remounting filesystem read-only"),
		kernelJournalLine(time.Hour, "blk_update_request: I/O error, dev sda, sector 1 op 0x0:(READ)"),
	}, "\n")
	kmsgRead := false
	sources := eventsTestSources(journal, nil, nil, nil)
	sources.kmsg = func(context.Context) ([]string, error) {
		kmsgRead = true
		return nil, nil
	}
	results := evaluateEventsFixture(sources)
	if kmsgRead {
		t.Fatal("journal에 커널 기록이 있는데 /dev/kmsg도 읽었다")
	}
	window := eventsResultByID(t, results, "window")
	timeline := eventsEvidenceValue(window, "events.evidence.timeline")
	if window.Summary != T("events.window.summary", 2, "6h", "2026-10-12 06:00:00") || strings.Index(timeline, "fs_readonly") > strings.Index(timeline, "disk_io") {
		t.Fatalf("window = %+v", window)
	}
	diskIO := eventsResultByID(t, results, "disk_io")
	if diskIO.Status != StatusFail || diskIO.Next != fmt.Sprintf("journalctl -k --since @%d", eventsTestNow.Add(-6*time.Hour).Unix()) {
		t.Fatalf("disk_io = %+v", diskIO)
	}
	if result := eventsResultByID(t, results, "segfault"); result.Status != StatusPass {
		t.Fatalf("segfault = %+v", result)
	}
}

func TestEventsNeedsRootSkipsInsteadOfPassing(t *testing.T) {
	results := evaluateEventsFixture(eventsTestSources("", errEventsNeedsRoot, nil, errEventsNeedsRoot))
	if len(results) != 8 || results[0].Probe != "events.window" {
		t.Fatalf("results = %+v", results)
	}
	for _, result := range results {
		if result.Status != StatusSkip {
			t.Errorf("%s = %s: %s", result.Probe, result.Status, result.Summary)
		}
	}
	if units := eventsResultByID(t, results, "units"); units.Summary != T("events.skip.needs_root", "journal") {
		t.Fatalf("units = %q", units.Summary)
	}
	if oom := eventsResultByID(t, results, "oom"); oom.Summary != T("events.skip.needs_root", "/dev/kmsg") {
		t.Fatalf("oom = %q", oom.Summary)
	}
}

func TestEventsJournalTimeoutSkipsWithTheError(t *testing.T) {
	results := evaluateEventsFixture(eventsTestSources("", context.DeadlineExceeded, nil, nil))
	units := eventsResultByID(t, results, "units")
	if units.Status != StatusSkip || !strings.Contains(units.Summary, "deadline exceeded") {
		t.Fatalf("units = %+v", units)
	}
	// /dev/kmsg는 읽혔으므로 커널 범주는 판정한다.
	if oom := eventsResultByID(t, results, "oom"); oom.Status != StatusPass {
		t.Fatalf("oom = %+v", oom)
	}
}

func TestEventsLinkFlapWarnsAndDownFails(t *testing.T) {
	flap := strings.Join([]string{
		kernelJournalLine(3*time.Hour, "e1000e 0000:00:1f.6 eno1: NIC Link is Down"),
		kernelJournalLine(2*time.Hour, "e1000e 0000:00:1f.6 eno1: NIC Link is Up 1000 Mbps Full Duplex"),
		kernelJournalLine(time.Hour, "IPv6: ADDRCONF(NETDEV_CHANGE): veth1: link becomes ready"),
	}, "\n")
	link := eventsResultByID(t, evaluateEventsFixture(eventsTestSources(flap, nil, nil, nil)), "link")
	if link.Status != StatusWarn || link.Summary != T("events.link.flap", "eno1", 3) || link.Next != "ip -s link show eno1" {
		t.Fatalf("flap = %+v", link)
	}
	down := flap + "\n" + kernelJournalLine(30*time.Minute, "e1000e 0000:00:1f.6 eno1: NIC Link is Down")
	link = eventsResultByID(t, evaluateEventsFixture(eventsTestSources(down, nil, nil, nil)), "link")
	if link.Status != StatusFail || link.Summary != T("events.link.down", "eno1", 4) {
		t.Fatalf("down = %+v", link)
	}
	upOnly := kernelJournalLine(time.Hour, "IPv6: ADDRCONF(NETDEV_CHANGE): veth1: link becomes ready")
	if link := eventsResultByID(t, evaluateEventsFixture(eventsTestSources(upOnly, nil, nil, nil)), "link"); link.Status != StatusPass {
		t.Fatalf("up only = %+v", link)
	}
}

func unitJournalLine(ago time.Duration, failed bool, unit string) string {
	if failed {
		return journalLine("boot1", ago, 24*time.Hour-ago, "journal", eventsUnitFailureID, unit, unit+": Failed with result 'exit-code'.")
	}
	return journalLine("boot1", ago, 24*time.Hour-ago, "journal", eventsUnitRestartID, unit, unit+": Scheduled restart job, restart counter is at 1.")
}

func TestEventsUnitsJudgeTheLastStateAndRestartLoops(t *testing.T) {
	cases := []struct {
		name   string
		lines  []string
		status Status
		unit   string
	}{
		{"failed", []string{unitJournalLine(2*time.Hour, false, "a.service"), unitJournalLine(time.Hour, true, "app.service")}, StatusFail, "app.service"},
		{"failed then restarted", []string{unitJournalLine(2*time.Hour, true, "app.service"), unitJournalLine(time.Hour, false, "app.service")}, StatusWarn, "app.service"},
		{"restart loop", []string{unitJournalLine(3*time.Hour, false, "w.service"), unitJournalLine(2*time.Hour, false, "w.service"), unitJournalLine(time.Hour, false, "w.service")}, StatusWarn, "w.service"},
		{"two restarts", []string{unitJournalLine(2*time.Hour, false, "w.service"), unitJournalLine(time.Hour, false, "w.service")}, StatusPass, ""},
		{"job failure is not a unit failure", []string{journalLine("boot1", time.Hour, time.Hour, "journal", "be02cf6855d2428ba40df7e9d022f03d", "x.service", "Failed to start x.service.")}, StatusPass, ""},
	}
	for _, test := range cases {
		units := eventsResultByID(t, evaluateEventsFixture(eventsTestSources(strings.Join(test.lines, "\n"), nil, nil, nil)), "units")
		if units.Status != test.status {
			t.Errorf("%s: %+v", test.name, units)
			continue
		}
		if test.unit != "" && units.Next != fmt.Sprintf("journalctl -u %s --since @%d", test.unit, eventsTestNow.Add(-6*time.Hour).Unix()) {
			t.Errorf("%s: next = %q", test.name, units.Next)
		}
	}
}

func TestEventsClockStepWarnsButSlewAndBootChangePass(t *testing.T) {
	entry := func(boot string, realtimeAgo, monotonic time.Duration) string {
		return journalLine(boot, realtimeAgo, monotonic, "journal", "", "", "tick")
	}
	cases := []struct {
		name   string
		lines  []string
		status Status
	}{
		// monotonic은 1분 흘렀는데 벽시계는 1분 3초 흘렀다.
		{"step", []string{entry("b", 2*time.Hour, time.Hour), entry("b", 2*time.Hour-63*time.Second, time.Hour+time.Minute)}, StatusWarn},
		// 6시간에 5초는 0.023%라 slew다.
		{"slew", []string{entry("b", 6*time.Hour-time.Minute, time.Hour), entry("b", time.Minute-5*time.Second, 7*time.Hour-2*time.Minute)}, StatusPass},
		{"two boots", []string{entry("b1", 3*time.Hour, 5*time.Hour), entry("b2", 2*time.Hour, time.Minute)}, StatusPass},
	}
	for _, test := range cases {
		clock := eventsResultByID(t, evaluateEventsFixture(eventsTestSources(strings.Join(test.lines, "\n"), nil, nil, nil)), "clock")
		if clock.Status != test.status {
			t.Errorf("%s: %+v", test.name, clock)
		}
		if test.status == StatusWarn && !strings.Contains(clock.Summary, "+3.0s") {
			t.Errorf("%s: summary = %q", test.name, clock.Summary)
		}
	}
}

func TestEventsEvidenceKeepsTheMostRecentLines(t *testing.T) {
	var lines []string
	for index := 0; index < 60; index++ {
		lines = append(lines, kernelJournalLine(time.Duration(60-index)*time.Minute, fmt.Sprintf("app[%d]: segfault at 0 ip 0 sp 0 error 4", index)))
	}
	results := evaluateEventsFixture(eventsTestSources(strings.Join(lines, "\n"), nil, nil, nil))
	segfault := eventsResultByID(t, results, "segfault")
	evidence := strings.Split(eventsEvidenceValue(segfault, "events.evidence.events"), "\n")
	if segfault.Status != StatusWarn || len(evidence) != eventsEvidenceLines+1 || evidence[0] != T("events.evidence.more", 40) || !strings.Contains(evidence[len(evidence)-1], "app[59]") {
		t.Fatalf("segfault = %s, evidence = %q", segfault.Status, evidence)
	}
	timeline := strings.Split(eventsEvidenceValue(eventsResultByID(t, results, "window"), "events.evidence.timeline"), "\n")
	if len(timeline) != eventsTimelineLines+1 || timeline[0] != T("events.evidence.more", 10) {
		t.Fatalf("timeline = %d lines, first %q", len(timeline), timeline[0])
	}
}

func TestParseEventsFlagsRejectsBadSince(t *testing.T) {
	for _, args := range [][]string{{"--since", "0"}, {"--since", "-1h"}, {"--since", "abc"}, {"--since", "6"}, {"extra"}} {
		var stderr bytes.Buffer
		if _, _, ok := parseEventsFlags(args, commonOptions{}, &stderr); ok || stderr.Len() == 0 {
			t.Errorf("%q를 받아들였다", args)
		}
	}
	options, since, ok := parseEventsFlags([]string{"--since", "90m", "--json", "-"}, commonOptions{}, io.Discard)
	if !ok || since != 90*time.Minute || options.jsonPath != "-" {
		t.Fatalf("since = %v, options = %+v, ok = %v", since, options, ok)
	}
	if _, since, ok := parseEventsFlags(nil, commonOptions{}, io.Discard); !ok || since != eventsDefaultSince {
		t.Fatalf("default since = %v", since)
	}
	if text := eventsDurationText(90 * time.Minute); text != "1h30m" {
		t.Fatalf("duration = %q", text)
	}
}

// journal이 커널 메시지를 모으지 않는 호스트(컨테이너, ForwardToKmsg만 켠 설정)와 조용한 호스트는 모두 커널 줄이 0개다.
// 둘을 구분할 수 없으므로 /dev/kmsg도 못 읽으면 커널 항목은 통과가 아니라 skip이다.
func TestEventsEmptyKernelJournalWithoutKmsgSkips(t *testing.T) {
	results := evaluateEventsFixture(eventsTestSources("", nil, nil, errEventsNeedsRoot))
	for _, id := range []string{"oom", "disk_io", "fs_readonly", "segfault", "link"} {
		if result := eventsResultByID(t, results, id); result.Status != StatusSkip {
			t.Errorf("%s = %s: %s", id, result.Status, result.Summary)
		}
	}
	if units := eventsResultByID(t, results, "units"); units.Status == StatusSkip {
		t.Errorf("journal은 읽었는데 units가 skip이다: %s", units.Summary)
	}
}
