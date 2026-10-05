package edc

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyFixture(argv []string, cwd string, started time.Time, status string, duration string, exit int, attempt int) string {
	key, _ := commandKey(argv)
	text := fmt.Sprintf("=== edc log start time=%s pid=123 cwd=%s stream=both command_display=full command=%s command_key=%s command_key_version=1 ===\n=== edc log process attempt=%d pid=124 executable=%s ===\nbody output\n=== edc log end time=%s status=%s exit=%d duration=%s ===\n", started.Format(time.RFC3339Nano), asciiJSON(cwd), asciiJSON(argv), key, attempt, asciiJSON(argv[0]), started.Add(time.Second).Format(time.RFC3339Nano), status, exit, duration)
	if status == "signal" {
		text = strings.Replace(text, "status=signal", "status=signal signal=SIGTERM", 1)
	}
	return text
}

func parseHistoryFixture(t *testing.T, text string) *historyParser {
	t.Helper()
	parser := &historyParser{path: "fixture.log"}
	if err := readHistoryLines(strings.NewReader(text), parser); err != nil {
		t.Fatal(err)
	}
	parser.finish()
	return parser
}

func TestLogHistoryParserOutcomesAndRestart(t *testing.T) {
	started := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	text := ""
	for index, status := range []string{"exit", "exit", "timeout", "signal", "start_error", "wait_error", "log_error", "signal_error"} {
		exit := 0
		if index > 0 {
			exit = 2
		}
		if status == "timeout" {
			exit = 124
		}
		if status == "signal" {
			exit = 143
		}
		row := historyFixture([]string{"echo", "a b", "秘密"}, "/tmp/with spaces", started.Add(time.Duration(index)*time.Hour), status, "12ms", exit, index+1)
		if status == "start_error" {
			lines := strings.Split(row, "\n")
			lines[1] = `=== edc log start_error cause="not found" ===`
			row = strings.Join(lines, "\n")
		}
		text += row
	}
	parser := parseHistoryFixture(t, text)
	if parser.invalid || len(parser.rows) != 8 {
		t.Fatalf("invalid=%v rows=%+v", parser.invalid, parser.rows)
	}
	expected := []string{"SUCCESS", "FAIL", "TIMEOUT", "SIGNAL", "ERROR", "ERROR", "ERROR", "ERROR"}
	for i, row := range parser.rows {
		wantAttempt := i + 1
		if i == 4 {
			wantAttempt = 1
		}
		if row.Outcome != expected[i] || row.Duration != 12*time.Millisecond || row.CWD != "/tmp/with spaces" || row.Attempt != wantAttempt {
			t.Fatalf("row %d: %+v", i, row)
		}
		if row.Key != parser.rows[0].Key {
			t.Fatal("restart identity changed")
		}
	}
}

func TestLogHistoryMalformedAndIncompleteMetadata(t *testing.T) {
	started := time.Now().UTC()
	valid := historyFixture([]string{"ls", "-l"}, "/tmp", started, "exit", "2ms", 0, 1)
	for name, text := range map[string]string{
		"duration":       strings.Replace(valid, "duration=2ms", "duration=-2ms", 1),
		"key":            strings.Replace(valid, "command_key_version=1", "command_key_version=2", 1),
		"duplicate":      strings.Replace(valid, "pid=123", "pid=123 pid=456", 1),
		"exit":           strings.Replace(valid, "exit=0", "exit=999", 1),
		"time":           strings.Replace(valid, started.Format(time.RFC3339Nano), "bad-time", 1),
		"JSON":           strings.Replace(valid, `command=["ls","-l"]`, `command=[oops]`, 1),
		"unknown status": strings.Replace(valid, "status=exit", "status=mystery", 1),
	} {
		t.Run(name, func(t *testing.T) {
			parser := parseHistoryFixture(t, text)
			if !parser.invalid || len(parser.rows) > 0 && parser.rows[0].Outcome != "UNKNOWN" {
				t.Fatalf("accepted malformed: %+v", parser)
			}
		})
	}
	parser := parseHistoryFixture(t, strings.Split(valid, "=== edc log end")[0])
	if len(parser.rows) != 1 || parser.rows[0].Outcome != "UNKNOWN" || parser.rows[0].Exit != nil || historyDuration(parser.rows[0]) != "—" {
		t.Fatalf("incomplete=%+v", parser.rows)
	}
	parser = parseHistoryFixture(t, strings.Split(valid, "=== edc log end")[0]+valid)
	if len(parser.rows) != 2 || parser.rows[0].Outcome != "UNKNOWN" || parser.rows[1].Outcome != "SUCCESS" {
		t.Fatalf("overlapping starts=%+v", parser.rows)
	}
	orphan := parseHistoryFixture(t, "=== edc log end time="+started.Format(time.RFC3339Nano)+" status=exit exit=0 duration=1ms ===\n")
	if !orphan.invalid || len(orphan.rows) != 0 {
		t.Fatal("orphan end accepted")
	}
}

func TestLogHistoryDisplayModesAndLegacy(t *testing.T) {
	argv := []string{"/bin/ls", "-l"}
	key, _ := commandKey(argv)
	for _, mode := range []string{"full", "name", "none"} {
		var writer strings.Builder
		if err := writeLogStart(&writer, time.Now(), logOptions{command: argv, commandDisplay: mode, stream: "both"}); err != nil {
			t.Fatal(err)
		}
		parser := parseHistoryFixture(t, writer.String())
		if parser.invalid || len(parser.rows) != 1 {
			t.Fatalf("%s: %+v", mode, parser)
		}
		if mode == "none" {
			if parser.rows[0].Key != "" || strings.Contains(writer.String(), "command_key=") {
				t.Fatal("none exposes key")
			}
		} else if parser.rows[0].Key != key {
			t.Fatalf("%s key mismatch", mode)
		}
		if mode == "name" && strings.Contains(writer.String(), "-l") {
			t.Fatal("name exposes argument")
		}
	}
	for _, command := range []string{`["ls","-l"]`, `["ls"]`, `["/bin/ls"]`} {
		parser := parseHistoryFixture(t, `=== edc log start time=2026-10-05T01:00:00Z pid=1 cwd="/tmp" stream=both command=`+command+" ===\n")
		if parser.invalid || len(parser.rows) != 1 {
			t.Fatal("legacy parser failed")
		}
		if (parser.rows[0].Key == "") != (command == `["ls"]`) {
			t.Fatalf("legacy identity: %+v", parser.rows[0])
		}
	}
}

func TestLogHistoryRotationAndLongOutput(t *testing.T) {
	started := time.Now().UTC()
	data := historyFixture([]string{"echo", "a b"}, "/tmp", started, "exit", "2ms", 0, 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "job[1].log")
	split := strings.Index(data, "command_key=") + 7
	if err := os.WriteFile(path+".edc.1", []byte(data[:split]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data[split:]), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := collectLogHistory([]string{path, path})
	if len(snapshot.Issues) != 0 || len(snapshot.Rows) != 1 || snapshot.Rows[0].Outcome != "SUCCESS" {
		t.Fatalf("rotation: %+v", snapshot)
	}
	extended := strings.Replace(data, "body output", strings.Repeat("x", 3*historyMarkerLimit), 1)
	parser := parseHistoryFixture(t, extended)
	if parser.invalid || len(parser.rows) != 1 || parser.rows[0].Outcome != "SUCCESS" {
		t.Fatal("long body broke parsing")
	}
	oversized := strings.Replace(data, "body output", "=== edc log "+strings.Repeat("x", 2*historyMarkerLimit), 1)
	parser = parseHistoryFixture(t, oversized)
	if !parser.invalid || parser.rows[0].Outcome != "UNKNOWN" {
		t.Fatal("oversized marker accepted")
	}
	parts, err := historyFamily(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = readHistoryFamily(path, parts)
	if err == nil {
		t.Fatal("file changed during snapshot accepted")
	}
}

func TestLogHistoryDiscoveryAndScanLimits(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"one.log", "ls/two.log", "ls/nested/ignored.log"} {
		path = filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "one.log"), filepath.Join(dir, "link.log")); err != nil {
		t.Fatal(err)
	}
	paths, err := historyPaths(dir)
	if err != nil || len(paths) != 2 {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	snapshot := collectLogHistory([]string{filepath.Join(dir, "link.log")})
	if len(snapshot.Issues) == 0 {
		t.Fatal("explicit symlink accepted")
	}
	huge := filepath.Join(dir, "huge.log")
	if err := os.WriteFile(huge, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(huge, historyByteLimit+1); err != nil {
		t.Fatal(err)
	}
	snapshot = collectLogHistory([]string{huge})
	if len(snapshot.Issues) != 1 || len(snapshot.Rows) != 0 {
		t.Fatal("byte limit ignored")
	}
}

func TestLogHistoryFollowsNamedSymlinkDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(target, "ls"), 0700); err != nil {
		t.Fatal(err)
	}
	data := historyFixture([]string{"ls", "-l"}, "/tmp", time.Now().UTC(), "exit", "2ms", 0, 1)
	for _, path := range []string{"job.log", "ls/two.log"} {
		if err := os.WriteFile(filepath.Join(target, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	snapshot := collectLogHistory([]string{filepath.Join(link, "job.log")})
	if len(snapshot.Issues) != 0 || len(snapshot.Rows) != 1 || snapshot.Rows[0].Outcome != "SUCCESS" {
		t.Fatalf("file under symlinked directory: %+v", snapshot)
	}
	paths, err := historyPaths(link)
	if err != nil || len(paths) != 2 {
		t.Fatalf("symlinked root paths=%v err=%v", paths, err)
	}
	if err := os.Symlink(filepath.Join(target, "ls"), filepath.Join(target, "nested")); err != nil {
		t.Fatal(err)
	}
	paths, err = historyPaths(link)
	if err != nil || len(paths) != 2 {
		t.Fatalf("symlinked subdirectory followed: paths=%v err=%v", paths, err)
	}
}

func TestLogHistoryRunRowColorsEveryFailureRed(t *testing.T) {
	for _, outcome := range []string{"FAIL", "TIMEOUT", "SIGNAL", "ERROR"} {
		row := historyRunRow(logHistoryAttempt{Outcome: outcome}, 80, false, true)
		if !strings.Contains(row, "\x1b[31;1m") {
			t.Fatalf("%s not red: %q", outcome, row)
		}
	}
	if row := historyRunRow(logHistoryAttempt{Outcome: "UNKNOWN"}, 80, false, true); !strings.Contains(row, "\x1b[33m") {
		t.Fatalf("UNKNOWN not yellow: %q", row)
	}
}

func TestLogHistoryCLIExactArgumentsAndSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.log")
	started := time.Now().UTC()
	text := historyFixture([]string{"ls", "-l"}, "/a", started, "exit", "2ms", 0, 1) + historyFixture([]string{"ls", "-l"}, "/a", started.Add(time.Hour), "exit", "4ms", 0, 1) + historyFixture([]string{"ls", "-l"}, "/b", started.Add(2*time.Hour), "exit", "99ms", 2, 1) + historyFixture([]string{"ls", "/tmp"}, "/a", started.Add(3*time.Hour), "exit", "8ms", 0, 1)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args []string) (int, string, string) {
		t.Helper()
		var out, errors strings.Builder
		code := runLogWithStreams(append([]string{"history", "--file", path}, args...), logStreams{stdin: strings.NewReader(""), stdout: &out, stderr: &errors})
		return code, out.String(), errors.String()
	}
	code, output, stderr := run(nil)
	if code != 0 || stderr != "" || strings.Count(output, "  key ") != 2 || !strings.Contains(output, "ls -l") || !strings.Contains(output, "ls /tmp") {
		t.Fatalf("list: %d %q %q", code, output, stderr)
	}
	code, output, stderr = run([]string{"--", "ls", "-l"})
	if code != 0 || stderr != "" || strings.Contains(output, "ls /tmp") || !strings.Contains(output, "3ms") || !strings.Contains(output, "99ms") {
		t.Fatalf("exact: %d %q %q", code, output, stderr)
	}
	code, output, _ = run([]string{"--failed", "--", "ls", "-l"})
	if code != 0 || strings.Contains(output, "2ms") || !strings.Contains(output, "99ms") {
		t.Fatalf("failed: %d %q", code, output)
	}
	key, _ := commandKey([]string{"ls", "-l"})
	code, output, _ = run([]string{"--key", key[:12]})
	if code != 0 || !strings.Contains(output, key) {
		t.Fatal("prefix lookup failed")
	}
	for _, args := range [][]string{{"--limit", "0"}, {"--key", "short"}, {"--dir", dir}, {"--command", "ls", "extra"}, {"--key", key, "--", "ls"}, {"--command", "ls", "--key", key}} {
		if code, _, _ := run(args); code != 2 {
			t.Fatalf("invalid args %v code %d", args, code)
		}
	}
	options, err := parseLogHistoryOptions([]string{"--", "echo", "--failed", "a b"}, io.Discard)
	if err != nil || options.Failed || len(options.Exact) != 3 || options.Exact[1] != "--failed" {
		t.Fatal("child flags parsed as history flags")
	}
}

func TestLogHistoryAmbiguousKeysAndIncompleteCounts(t *testing.T) {
	first := strings.Repeat("a", 64)
	second := first[:63] + "b"
	groups := []logHistoryGroup{{Key: first}, {Key: second}}
	if _, err := selectHistoryGroup(groups, first[:12]); err == nil {
		t.Fatal("ambiguous prefix accepted")
	}
	if group, err := selectHistoryGroup(groups, first); err != nil || group.Key != first {
		t.Fatal("full key failed")
	}
	snapshot := logHistorySnapshot{Rows: []logHistoryAttempt{{Key: first, Command: []string{"ls"}, Outcome: "UNKNOWN"}, {Command: []string{"ls"}, Outcome: "SUCCESS"}}}
	indexed, unknown := logHistoryGroups(snapshot, logHistoryOptions{})
	if len(indexed) != 1 || indexed[0].Failed != 0 || indexed[0].Unknown != 1 || unknown != 1 {
		t.Fatal("unknown counted as failure or keyed")
	}
	indexed, _ = logHistoryGroups(snapshot, logHistoryOptions{Failed: true})
	if len(indexed) != 0 {
		t.Fatal("unknown counted as failure")
	}
}

func TestLogHistoryActualRecordingAndFailedQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actual.log")
	code, _, stderr, _ := runLogTest(t, "both", path, strings.NewReader(""), "exit7")
	if code != 7 || stderr != "" {
		t.Fatalf("record code=%d stderr=%s", code, stderr)
	}
	var output, errors strings.Builder
	args := []string{"history", "--file", path, "--failed", "--"}
	args = append(args, logHelperCommand("exit7")...)
	code = runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: &output, stderr: &errors})
	if code != 0 || errors.Len() != 0 || !strings.Contains(output.String(), "exit 7") {
		t.Fatalf("query code=%d output=%s errors=%s", code, output.String(), errors.String())
	}
}

func TestLogHistoryDuplicateEndIsUnknown(t *testing.T) {
	text := historyFixture([]string{"ls", "-l"}, "/tmp", time.Now(), "exit", "1ms", 0, 1)
	end := text[strings.Index(text, "=== edc log end"):]
	parser := parseHistoryFixture(t, text+end)
	if !parser.invalid || len(parser.rows) != 1 || parser.rows[0].Outcome != "UNKNOWN" {
		t.Fatal("duplicate end reported success")
	}
}

func TestLogHistoryAttemptAndDiscoveryBounds(t *testing.T) {
	dir := t.TempDir()
	for index := 0; index < historyFamilyLimit+1; index++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.log", index)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := historyPaths(dir)
	if err != errHistoryScanLimit || len(paths) != historyFamilyLimit {
		t.Fatalf("discovery bound: %d %v", len(paths), err)
	}
	snapshot, err := loadLogHistory(logHistoryOptions{Directory: dir})
	if err != nil || len(snapshot.Issues) == 0 {
		t.Fatal("partial scan not surfaced")
	}
	parser := &historyParser{}
	if err := readHistoryLines(strings.NewReader(strings.Repeat("=== edc log start time=bad cwd=bad ===\n", historyAttemptLimit+2)), parser); err == nil {
		t.Fatal("attempt limit not enforced")
	}
}

func TestLogHistoryDurationSummaryDoesNotOverflow(t *testing.T) {
	duration := time.Duration(1<<63 - 1)
	row := logHistoryAttempt{Command: []string{"test"}, Outcome: "SUCCESS", Duration: duration, Started: time.Now(), Ended: time.Now()}
	group := logHistoryGroup{Key: strings.Repeat("a", 64), Representative: row, Rows: []logHistoryAttempt{row, row}}
	var output strings.Builder
	printHistoryGroup(&output, group, logHistoryOptions{Limit: 20})
	if !strings.Contains(output.String(), T("cli.log_history.timing", duration.String(), duration.String(), duration.String())) {
		t.Fatal("duration mean overflowed")
	}
}

func TestLogHistoryOptionsHaveOneArgumentBoundary(t *testing.T) {
	key, _ := commandKey([]string{"ls", "-al"})
	cases := []struct {
		args, exact []string
		name        string
		failed      bool
	}{
		{args: nil},
		{args: []string{"--"}},
		{args: []string{"ls"}, exact: []string{"ls"}},
		{args: []string{"--", "ls"}, exact: []string{"ls"}},
		{args: []string{"ls", "-al"}, exact: []string{"ls", "-al"}},
		{args: []string{"--", "ls", "-al"}, exact: []string{"ls", "-al"}},
		{args: []string{"--failed", "ls", "-al"}, exact: []string{"ls", "-al"}, failed: true},
		{args: []string{"ls", "--failed"}, exact: []string{"ls", "--failed"}},
		{args: []string{"ls", "--", "-al"}, exact: []string{"ls", "--", "-al"}},
		{args: []string{"echo", "--command", "ls", "--key", key}, exact: []string{"echo", "--command", "ls", "--key", key}},
		{args: []string{"echo", "a b", ""}, exact: []string{"echo", "a b", ""}},
		{args: []string{"--command", "ls"}, name: "ls"},
		{args: []string{"--command=ls", "--failed"}, name: "ls", failed: true},
	}
	for _, row := range cases {
		options, err := parseLogHistoryOptions(row.args, io.Discard)
		if err != nil || options.Name != row.name || options.Failed != row.failed || len(options.Exact) != len(row.exact) {
			t.Fatalf("args=%q options=%+v err=%v", row.args, options, err)
		}
		for i, arg := range row.exact {
			if options.Exact[i] != arg {
				t.Fatalf("argv changed: %q -> %q", row.args, options.Exact)
			}
		}
		if len(row.exact) > 0 {
			expected, _ := commandKey(row.exact)
			if options.Key != expected {
				t.Fatal("exact query key inconsistent")
			}
		}
	}
	for _, args := range [][]string{
		{"--command", "ls", "ls"},
		{"--command", "ls", "--key", key},
		{"--key", key, "ls"},
		{"--command", ""},
		{"--key", ""},
		{"--command", "ls", "--command", "echo"},
		{"--key", key, "--key", key},
	} {
		if _, err := parseLogHistoryOptions(args, io.Discard); err == nil {
			t.Fatalf("conflicting selector accepted: %q", args)
		}
	}
}

func TestLogHistoryDirectArgvMatchesExplicitSeparator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	started := time.Now()
	data := historyFixture([]string{"ls"}, "/tmp", started, "exit", "1ms", 0, 1) + historyFixture([]string{"ls", "-al"}, "/tmp", started, "exit", "2ms", 0, 1)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		t.Helper()
		var out, errors strings.Builder
		code := runLogWithStreams(append([]string{"history", "--file", path}, args...), logStreams{stdin: strings.NewReader(""), stdout: &out, stderr: &errors})
		if errors.Len() > 0 {
			t.Fatalf("query failed: %s", errors.String())
		}
		return code, out.String()
	}
	code, direct := run("ls", "-al")
	explicitCode, explicit := run("--", "ls", "-al")
	if code != 0 || explicitCode != 0 || direct != explicit || !strings.Contains(direct, "2ms") || strings.Contains(direct, "1ms") {
		t.Fatal("optional separator changes query")
	}
	_, plain := run("ls")
	if !strings.Contains(plain, "1ms") || strings.Contains(plain, "2ms") {
		t.Fatal("single argv query became name list")
	}
	_, names := run("--command", "ls")
	if strings.Count(names, "  key ") != 2 {
		t.Fatalf("name list missing argument variants: %s", names)
	}
}
