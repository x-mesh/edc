package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogResourceFieldsRoundTrip(t *testing.T) {
	resources := logResources{CPUUser: 1500 * time.Millisecond, CPUSys: 250 * time.Microsecond, MaxRSS: 4096, rusage: true, Scope: logResourceScopeGroup, Samples: 3, MemPeak: 9000, MemAvg: 7000, CPUPeak: 187.26, DiskRead: 10, DiskWrite: 20, disk: true}
	text := resources.fields()
	if strings.ContainsFunc(text, func(r rune) bool { return r > 127 }) || strings.Contains(text, "cpu_time_us") {
		t.Fatalf("fields: %q", text)
	}
	_, fields, err := parseHistoryMarker("=== edc log end status=exit exit=0" + text + " duration=2s ===")
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := parseHistoryResources(fields)
	if !ok || parsed.CPU != 1500250*time.Microsecond || parsed.MaxRSS != 4096 || parsed.Samples != 3 || parsed.MemPeak != 9000 || parsed.CPUPeak != 187.3 || !parsed.disk || parsed.DiskWrite != 20 || parsed.Scope != "pgrp" {
		t.Fatalf("parsed %+v ok=%v", parsed, ok)
	}
	container := logResources{Scope: logResourceScopeExec, Container: "0123456789ab", Samples: 1, CPUTime: 2 * time.Second, cpuTime: true}
	_, fields, _ = parseHistoryMarker("=== edc log end status=exit exit=0" + container.fields() + " duration=2s ===")
	if parsed, ok := parseHistoryResources(fields); !ok || parsed.CPU != 2*time.Second || parsed.Container != "0123456789ab" || parsed.memory() != 0 {
		t.Fatalf("container fields: %+v", parsed)
	}
	if parsed, ok := parseHistoryResources(map[string]string{"status": "exit"}); parsed != nil || !ok {
		t.Fatal("a marker without resource fields grew resources")
	}
	if _, ok := parseHistoryResources(map[string]string{"samples": "2", "mem_peak": "x", "cpu_peak": "1", "scope": "pgrp"}); ok {
		t.Fatal("malformed resource field accepted")
	}
}

func TestLogResourceAggregatePeaksAndTotals(t *testing.T) {
	started := time.Unix(100, 0)
	aggregate := newLogResourceAggregate(started, true)
	aggregate.add(logResourceReading{cpu: 100 * time.Millisecond, memory: 10, cpuValid: true, diskValid: true, diskRead: 5}, started.Add(200*time.Millisecond))
	aggregate.add(logResourceReading{cpu: 1100 * time.Millisecond, memory: 30, cpuValid: true, diskValid: true, diskRead: 9}, started.Add(1200*time.Millisecond))
	var resources logResources
	resources.addAggregate(aggregate, logResourceScopeGroup, "")
	if resources.Samples != 2 || resources.MemPeak != 30 || resources.MemAvg != 20 || resources.CPUPeak != 100 || resources.CPUTime != 1100*time.Millisecond || resources.DiskRead != 9 {
		t.Fatalf("zero base: %+v", resources)
	}
	whole := newLogResourceAggregate(started, false)
	whole.add(logResourceReading{cpu: time.Hour, cpuValid: true, memory: 50, peak: 900}, started)
	whole.add(logResourceReading{cpu: time.Hour + time.Second, cpuValid: true, memory: 60}, started.Add(2*time.Second))
	resources = logResources{}
	resources.addAggregate(whole, logResourceScopeContainer, "abc")
	if resources.CPUTime != time.Second || resources.CPUPeak != 50 || resources.MemPeak != 900 || resources.disk {
		t.Fatalf("first-reading base: %+v", resources)
	}
}

func TestLogMemberTotalsSurviveExitedMembers(t *testing.T) {
	var totals logMemberTotals
	first := logProcessMember{pid: 10, start: 1}
	second := logProcessMember{pid: 11, start: 2}
	reading := totals.reading(map[logProcessMember]logResourceReading{first: {cpu: time.Second, memory: 5, cpuValid: true, diskValid: true}, second: {cpu: time.Second, memory: 7, cpuValid: true, diskValid: true}})
	if reading.cpu != 2*time.Second || reading.memory != 12 {
		t.Fatalf("first reading: %+v", reading)
	}
	reused := logProcessMember{pid: 10, start: 9}
	reading = totals.reading(map[logProcessMember]logResourceReading{reused: {cpu: time.Second, memory: 3, cpuValid: true, diskValid: true}})
	if reading.cpu != 3*time.Second || reading.memory != 3 {
		t.Fatalf("exited members dropped from the total or a reused pid merged: %+v", reading)
	}
}

type fakeLogResourceProbe struct{ samples int }

func (probe *fakeLogResourceProbe) sample(time.Time) { probe.samples++ }

func (probe *fakeLogResourceProbe) finish(resources *logResources, _ *os.ProcessState) {
	resources.Samples = probe.samples
}

func TestLogResourceSamplerStopsBeforeReading(t *testing.T) {
	probe := &fakeLogResourceProbe{}
	sampler := startLogResourceSampler(probe, time.Millisecond, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if resources := sampler.finish(nil); resources.Samples == 0 {
		t.Fatal("sampler never sampled")
	}
}

func TestParseLogDockerCommand(t *testing.T) {
	cases := []struct {
		command []string
		want    logDockerCommand
		ok      bool
	}{
		{[]string{"docker", "run", "--rm", "alpine", "true"}, logDockerCommand{kind: "run"}, true},
		{[]string{"/usr/bin/docker", "--context", "prod", "container", "run", "alpine"}, logDockerCommand{kind: "run"}, true},
		{[]string{"docker", "exec", "-it", "-u", "root", "-e", "A=1", "--workdir=/srv", "web", "make", "test"}, logDockerCommand{kind: "exec", container: "web", argv: []string{"make", "test"}}, true},
		{[]string{"docker", "-H", "unix:///x.sock", "exec", "web", "--", "-v"}, logDockerCommand{kind: "exec", container: "web", argv: []string{"--", "-v"}}, true},
		{[]string{"docker", "exec", "web"}, logDockerCommand{}, false},
		{[]string{"docker", "compose", "up"}, logDockerCommand{}, false},
		{[]string{"podman", "run", "alpine"}, logDockerCommand{}, false},
	}
	for _, test := range cases {
		got, ok := parseLogDockerCommand(test.command)
		if ok != test.ok || got.kind != test.want.kind || got.container != test.want.container || strings.Join(got.argv, " ") != strings.Join(test.want.argv, " ") {
			t.Errorf("%v: got %+v %v", test.command, got, ok)
		}
	}
	if !logCommandLineMatches([]string{"/usr/bin/make", "test"}, []string{"make", "test"}) || logCommandLineMatches([]string{"make"}, []string{"make", "test"}) {
		t.Fatal("command line matching")
	}
	early, late := logProcessMember{pid: 5, start: 10}, logProcessMember{pid: 6, start: 20}
	if root, ok := pickLogExecRoot([]logExecCandidate{{late, []string{"make", "test"}}, {early, []string{"sh"}}}, []string{"make", "test"}); !ok || root != late {
		t.Fatal("exact argv did not win")
	}
	if root, ok := pickLogExecRoot([]logExecCandidate{{late, []string{"dd", "if", "/dev/zero"}}, {early, []string{"sh"}}}, []string{"dd", "if=/dev/zero"}); !ok || root != late {
		t.Fatal("rewritten argv lost its first word match")
	}
	if root, ok := pickLogExecRoot([]logExecCandidate{{late, []string{"node"}}}, []string{"npm", "test"}); !ok || root != late {
		t.Fatal("a lone candidate was rejected")
	}
	if _, ok := pickLogExecRoot([]logExecCandidate{{late, []string{"node"}}, {early, []string{"sh"}}}, []string{"npm", "test"}); ok {
		t.Fatal("ambiguous candidates produced a root")
	}
	read, write, ok := sumCgroupIOStat("8:0 rbytes=10 wbytes=20 rios=1 wios=2\n259:0 rbytes=5 wbytes=1 dbytes=0\n")
	if !ok || read != 15 || write != 21 {
		t.Fatalf("io.stat: %d %d %v", read, write, ok)
	}
}

func TestLogHistoryBaselineUsesEarlierSuccessesInTheSameDirectory(t *testing.T) {
	start := time.Unix(1000, 0)
	run := func(minutes int, cwd string, outcome logHistoryOutcome, memory uint64, cpu time.Duration) logHistoryAttempt {
		return logHistoryAttempt{CWD: cwd, Outcome: outcome, Started: start.Add(time.Duration(minutes) * time.Minute), Resources: &logHistoryResources{MaxRSS: memory, CPU: cpu, cpu: true}}
	}
	current := run(10, "/a", historyOutcomeFail, 400, 4*time.Second)
	rows := []logHistoryAttempt{current, run(9, "/a", historyOutcomeSuccess, 100, time.Second), run(8, "/b", historyOutcomeSuccess, 1, time.Millisecond), run(7, "/a", historyOutcomeFail, 1, time.Millisecond), run(6, "/a", historyOutcomeSuccess, 200, 2*time.Second)}
	if ratios := historyBaseline(rows, current); ratios.memory != 0 || ratios.cpu != 0 {
		t.Fatalf("two earlier successes produced a baseline: %+v", ratios)
	}
	rows = append(rows, run(5, "/a", historyOutcomeSuccess, 150, 2*time.Second), run(11, "/a", historyOutcomeSuccess, 1, time.Millisecond))
	ratios := historyBaseline(rows, current)
	if ratios.memory < 2.66 || ratios.memory > 2.67 || ratios.cpu != 2 {
		t.Fatalf("baseline: %+v", ratios)
	}
	container := run(12, "/a", historyOutcomeSuccess, 4000, time.Hour)
	container.Resources.Scope, container.Resources.Samples, container.Resources.MemPeak = logResourceScopeContainer, 1, 4000
	if mixed := historyBaseline(append(rows, container), container); mixed.memory != 0 {
		t.Fatalf("a container measurement was compared with process measurements: %+v", mixed)
	}
	lines := strings.Join(historyResourceLines(current, ratios), "\n")
	if !strings.Contains(lines, T("cli.log_history.resources.ratio", "2.7")) || !strings.Contains(lines, T("cli.log_history.resources.max_rss", formatSizeShort(400))) {
		t.Fatalf("resource lines: %q", lines)
	}
}

func TestLogRecordsResourcesOfTheRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.log")
	code, _, stderr, content := runLogTest(t, "stdout", path, strings.NewReader(""), "work")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q log=%q", code, stderr, content)
	}
	snapshot := collectLogHistory([]string{path}, nil)
	if len(snapshot.Rows) != 1 || len(snapshot.Issues) != 0 {
		t.Fatalf("rows=%+v issues=%v", snapshot.Rows, snapshot.Issues)
	}
	resources := snapshot.Rows[0].Resources
	if resources == nil || !resources.cpu || resources.CPU < 100*time.Millisecond || resources.MaxRSS < 64<<20 || resources.Samples < 1 || resources.MemPeak == 0 || resources.Scope != logResourceScopeGroup {
		t.Fatalf("resources: %+v\n%s", resources, content)
	}
}

func TestLogHistoryRunListShowsResourceColumns(t *testing.T) {
	exit := 0
	row := logHistoryAttempt{Started: time.Now(), Ended: time.Now(), Duration: 2 * time.Second, Outcome: historyOutcomeSuccess, Exit: &exit, Resources: &logHistoryResources{MaxRSS: 300 << 20, CPU: 1500 * time.Millisecond, cpu: true, DiskRead: 1 << 20, DiskWrite: 1 << 20, disk: true}}
	header, line := historyRunHeader(88), historyRunRow(row, 88, false, false, historyRatios{})
	for _, want := range []string{T("cli.log_history.column.memory"), T("cli.log_history.column.cpu"), T("cli.log_history.column.disk")} {
		if !strings.Contains(header, want) {
			t.Fatalf("header %q misses %q", header, want)
		}
	}
	if !strings.Contains(line, "300.0 MB") || !strings.Contains(line, "1.5s") || !strings.Contains(line, "2.0 MB") || len([]rune(line)) > 88 || len([]rune(header)) > 88 {
		t.Fatalf("row %q (%d) header %q (%d)", line, len([]rune(line)), header, len([]rune(header)))
	}
	if narrow := historyRunHeader(80); strings.Contains(narrow, T("cli.log_history.column.cpu")) || !strings.Contains(narrow, T("cli.log_history.column.memory")) {
		t.Fatalf("narrow header %q", narrow)
	}
	colored := historyRunRow(row, 88, false, true, historyRatios{cpu: 2.5})
	if !strings.Contains(colored, historySGR(richYellow, true)) {
		t.Fatalf("CPU twice the usual is not marked: %q", colored)
	}
	if empty := historyRunRow(logHistoryAttempt{Outcome: historyOutcomeRunning}, 88, false, false, historyRatios{}); strings.Count(empty, "—") < 4 {
		t.Fatalf("a run without resources should show dashes: %q", empty)
	}
	if historyShortDuration(83*time.Second) != "1m23s" || historyShortDuration(250*time.Millisecond) != "250ms" {
		t.Fatal("short duration")
	}
}

func TestLogHistoryDurationBaselineMarksSlowRuns(t *testing.T) {
	now := time.Now()
	exit := 0
	done := func(ago, duration time.Duration) logHistoryAttempt {
		started := now.Add(-ago)
		return logHistoryAttempt{CWD: "/a", Outcome: historyOutcomeSuccess, Started: started, Ended: started.Add(duration), Duration: duration, Exit: &exit}
	}
	rows := []logHistoryAttempt{done(time.Hour, time.Second), done(2*time.Hour, time.Second), done(3*time.Hour, 2*time.Second)}
	slow := done(time.Minute, 3*time.Second)
	if ratios := historyBaseline(rows, slow); ratios.duration != 3 || strings.Join(ratios.alerts(), ",") != "duration" || ratios.alertText() != T("cli.log_history.alert.duration")+" 3.0×" {
		t.Fatalf("finished run: %+v", ratios)
	}
	running := logHistoryAttempt{CWD: "/a", Outcome: historyOutcomeRunning, Started: now.Add(-5 * time.Second), Resources: &logHistoryResources{MaxRSS: 1 << 30}}
	if ratios := historyBaseline(rows, running); ratios.duration < 4.9 || ratios.memory != 0 {
		t.Fatalf("running run: %+v", ratios)
	}
	if row := historyRunRow(slow, 88, false, true, historyRatios{duration: 3}); !strings.Contains(row, historySGR(richYellow, true)) {
		t.Fatalf("slow duration not marked: %q", row)
	}
	group := logHistoryGroup{Key: strings.Repeat("a", 64), Representative: running, Rows: append([]logHistoryAttempt{running}, rows...), Running: 1}
	if !historyGroupSlow(group) || !strings.Contains(historyKeyRowColumns(group, historyKeyWidths(88, []logHistoryGroup{group}), false, false, now), T("cli.log_history.running_slow")) {
		t.Fatal("slow running command not marked in the key list")
	}
}
