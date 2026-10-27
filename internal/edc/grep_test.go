package edc

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// grepCommand는 grepRun을 파일로 된 stdin, stdout, stderr로 실행한다. 터미널이 아니므로 색과 hint는 나오지 않는다.
func grepCommand(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	open := func(name, content string) *os.File {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	in, out, errOut := open("stdin", stdin), open("stdout", ""), open("stderr", "")
	defer in.Close()
	defer out.Close()
	defer errOut.Close()
	code := grepRun(args, in, out, errOut)
	stdout, _ := os.ReadFile(out.Name())
	stderr, _ := os.ReadFile(errOut.Name())
	return code, string(stdout), string(stderr)
}

// grepFixtureDir은 view의 fixture를 복사한 디렉터리다.
func grepFixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"access.log", "app.jsonl", "app.log"} {
		data, err := os.ReadFile(filepath.Join("testdata", "view", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGrepCommandPrintsLikeGrep(t *testing.T) {
	dir := grepFixtureDir(t)
	code, stdout, stderr := grepCommand(t, "", "-n", "users", filepath.Join(dir, "access.log"), filepath.Join(dir, "app.log"))
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	access := filepath.Join(dir, "access.log")
	want := access + ":1:203.0.113.9 - - [26/Oct/2026:10:00:00 +0900] \"GET /api/users HTTP/1.1\" 200 512 \"-\" \"curl/8.4.0\"\n" +
		access + ":4:198.51.100.4 - - [26/Oct/2026:10:00:03 +0900] \"GET /api/users HTTP/1.1\" 404 37 \"-\" \"curl/8.4.0\"\n" +
		access + ":6:192.0.2.77 - - [26/Oct/2026:10:00:05 +0900] \"GET /api/users HTTP/1.1\" 200 498\n"
	if stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	// 파일 하나면 경로를 붙이지 않는다. 디렉터리를 주면 안의 파일을 모두 찾고 경로를 붙인다.
	code, stdout, _ = grepCommand(t, "", "502", access)
	if code != 0 || strings.Contains(stdout, access) || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("single file: code %d stdout %q", code, stdout)
	}
	code, stdout, _ = grepCommand(t, "", "-l", "upstream", dir)
	if code != 0 || stdout != filepath.Join(dir, "app.jsonl")+"\n" {
		t.Fatalf("-l: code %d stdout %q", code, stdout)
	}
	if code, stdout, _ = grepCommand(t, "", "nothing-here", dir); code != 1 || stdout != "" {
		t.Fatalf("no match: code %d stdout %q", code, stdout)
	}
	code, _, stderr = grepCommand(t, "", "x", filepath.Join(dir, "missing.log"))
	if code != 2 || !strings.Contains(stderr, "missing.log") {
		t.Fatalf("missing file: code %d stderr %q", code, stderr)
	}
	if code, _, stderr = grepCommand(t, "", "--level", "loud", "x", access); code != 2 || !strings.Contains(stderr, "loud") {
		t.Fatalf("bad level: code %d stderr %q", code, stderr)
	}
}

func TestGrepCommandContextAndFilters(t *testing.T) {
	dir := grepFixtureDir(t)
	app := filepath.Join(dir, "app.log")
	code, stdout, _ := grepCommand(t, "", "-n", "-C", "1", "Caused", app)
	want := "6-\tat com.example.Api.handle(Api.java:41)\n7:Caused by: java.io.IOException: broken pipe\n8-\t... 12 more\n"
	if code != 0 || stdout != want {
		t.Fatalf("-C 1:\n%s\nwant:\n%s", stdout, want)
	}
	// 두 묶음이 떨어져 있으면 "--"로 가른다.
	code, stdout, _ = grepCommand(t, "", "-C", "0", "-E", "started|reported", app)
	if code != 0 || strings.Contains(stdout, "--") || strings.Count(stdout, "\n") != 2 {
		t.Fatalf("-C 0:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, "", "-n", "-C", "1", "-E", "started|reported", app)
	if code != 0 || !strings.Contains(stdout, "\n--\n") || !strings.HasPrefix(stdout, "1:") {
		t.Fatalf("-C 1 groups:\n%s", stdout)
	}
	// --level error는 stack trace까지 낸다.
	code, stdout, _ = grepCommand(t, "", "--level", "error", "", app)
	if code != 0 || strings.Count(stdout, "\n") != 6 || strings.Contains(stdout, "INFO") {
		t.Fatalf("--level error:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, "", "--status", "5xx", "", dir)
	if code != 0 || strings.Count(stdout, "\n") != 2 || !strings.Contains(stdout, "access.log:") || !strings.Contains(stdout, "app.jsonl:") {
		t.Fatalf("--status 5xx:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, "", "--since", "2026-10-26 10:00:03", "--until", "2026-10-26 10:00:04", "", app)
	if code != 0 || strings.Count(stdout, "\n") != 1 || !strings.Contains(stdout, "user reported") {
		t.Fatalf("--since/--until:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, "", "--last", "2", "users", filepath.Join(dir, "access.log"))
	if code != 0 || strings.Count(stdout, "\n") != 2 || !strings.HasPrefix(stdout, "198.51.100.4") {
		t.Fatalf("--last 2:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, "", "-i", "-n", "WARN", app)
	if code != 0 || stdout != "2:2026-10-26 10:00:01,004 WARN  [pool-1] c.e.Db - pool 80% used\n" {
		t.Fatalf("-i:\n%s", stdout)
	}
}

// stdin과 .gz는 차례로 읽는다. 결과는 파일을 직접 읽은 것과 같다.
func TestGrepCommandReadsStdinAndGzip(t *testing.T) {
	dir := grepFixtureDir(t)
	app, _ := os.ReadFile(filepath.Join(dir, "app.log"))
	code, stdout, _ := grepCommand(t, string(app), "-n", "-C", "1", "Caused")
	if code != 0 || stdout != "6-\tat com.example.Api.handle(Api.java:41)\n7:Caused by: java.io.IOException: broken pipe\n8-\t... 12 more\n" {
		t.Fatalf("stdin:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, string(app), "--level", "error", "--last", "2", "")
	if code != 0 || stdout != "Caused by: java.io.IOException: broken pipe\n\t... 12 more\n" {
		t.Fatalf("stdin --last:\n%s", stdout)
	}
	var zipped bytes.Buffer
	writer := gzip.NewWriter(&zipped)
	writer.Write(app)
	writer.Close()
	path := filepath.Join(dir, "app.log.gz")
	if err := os.WriteFile(path, zipped.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = grepCommand(t, "", "-n", "--level", "warn", "pool", path)
	if code != 0 || stdout != "2:2026-10-26 10:00:01,004 WARN  [pool-1] c.e.Db - pool 80% used\n" {
		t.Fatalf("gz:\n%s", stdout)
	}
}

// hint는 무엇을 건너뛰었는지 말한다. 건너뛴 범위, 끝에서 읽은 양, -n을 위해 센 줄 수, 열지 않은 파일, binary 파일이다.
func TestGrepHintsExplainSkips(t *testing.T) {
	hints := &grepHints{skippedOld: []string{"a.log", "b.log"}, binary: []string{"core"}, reports: []grepFileReport{
		{path: "x.log", skipped: 3 << 30, bytesRead: 2 << 20, counted: 32000},
		{path: "y.log", lastRead: 46 << 20},
		{path: "z.log", seekFailed: true},
		{path: "p.log", noTime: true},
	}}
	var out bytes.Buffer
	hints.render(&out, false)
	text := out.String()
	for _, want := range []string{"hint: ", "a.log, b.log", "core", "x.log: --since", "-n", "y.log: --last", "46.0 MB", "z.log: lines are not in time order", "p.log: no timestamps"} {
		if !strings.Contains(text, want) {
			t.Errorf("hints miss %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "%!") {
		t.Fatalf("format error:\n%s", text)
	}
}

// --since는 파일 안의 마지막 시각으로 건너뛴다. 수정 시각이 오래됐어도 내용이 범위 안이면 읽는다.
func TestGrepCommandSkipsFilesByTheirLastTimestamp(t *testing.T) {
	dir := grepFixtureDir(t)
	access := filepath.Join(dir, "access.log")
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	if err := os.Chtimes(access, when, when); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := grepCommand(t, "", "--since", "2026-10-26", "users", access)
	if code != 0 || strings.Count(stdout, "\n") != 3 {
		t.Fatalf("old mtime but recent lines: code %d stdout %q", code, stdout)
	}
	code, stdout, _ = grepCommand(t, "", "--since", "2027-01-01", "users", access)
	if code != 1 || stdout != "" {
		t.Fatalf("lines end before --since: code %d stdout %q", code, stdout)
	}
}

// 조건이 있으면 첫 인자가 실제 파일일 때 패턴을 생략할 수 있다. 도움말과 문서의 예제가 그대로 동작한다.
func TestGrepCommandPatternIsOptionalWithFilters(t *testing.T) {
	dir := grepFixtureDir(t)
	code, stdout, _ := grepCommand(t, "", "--status", "5xx", dir)
	if code != 0 || strings.Count(stdout, "\n") != 2 {
		t.Fatalf("--status without pattern:\n%s", stdout)
	}
	code, stdout, _ = grepCommand(t, "", "--level", "error", "--last", "2", filepath.Join(dir, "app.log"))
	if code != 0 || stdout != "Caused by: java.io.IOException: broken pipe\n\t... 12 more\n" {
		t.Fatalf("--level --last without pattern:\n%s", stdout)
	}
	// 첫 인자가 파일이 아니면 패턴이다.
	code, stdout, _ = grepCommand(t, "", "--status", "5xx", "orders", dir)
	if code != 0 || strings.Count(stdout, "\n") != 1 || !strings.Contains(stdout, "access.log:") {
		t.Fatalf("--status with pattern:\n%s", stdout)
	}
}

// pipe 출력은 원문 바이트를 지킨다. CRLF의 CR도 그대로 낸다.
func TestGrepCommandKeepsCarriageReturns(t *testing.T) {
	code, stdout, _ := grepCommand(t, "alpha\r\nbeta\r\n", "alpha")
	if code != 0 || stdout != "alpha\r\n" {
		t.Fatalf("stdin: %q", stdout)
	}
	path := filepath.Join(t.TempDir(), "crlf.log")
	if err := os.WriteFile(path, []byte("alpha\r\nbeta\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ = grepCommand(t, "", "alpha", path); code != 0 || stdout != "alpha\r\n" {
		t.Fatalf("file: %q", stdout)
	}
}

func TestGrepCommandSearchesSubdirectories(t *testing.T) {
	dir := grepFixtureDir(t)
	nested := filepath.Join(dir, "nested", "deeper")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "more.log"), []byte("one more upstream line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := grepCommand(t, "", "upstream", dir)
	if code != 0 || strings.Count(stdout, "\n") != 2 || !strings.Contains(stdout, filepath.Join(nested, "more.log")+":") {
		t.Fatalf("recursive:\n%s", stdout)
	}
}

// 손상된 .gz는 오류를 알리고 2로 끝난다. 다른 파일의 결과는 그대로 낸다.
func TestGrepCommandReportsACorruptGzip(t *testing.T) {
	dir := grepFixtureDir(t)
	broken := filepath.Join(dir, "broken.log.gz")
	if err := os.WriteFile(broken, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := grepCommand(t, "", "upstream", broken, filepath.Join(dir, "app.jsonl"))
	if code != 2 || !strings.Contains(stderr, "broken.log.gz") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// 시계가 틀린 줄 하나가 미래 시각을 달고 있어도 --until은 그 뒤의 범위 안 줄을 모두 낸다.
func TestGrepCommandUntilReadsPastAnOutlier(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	data := grepTimedAccessLog(3000, start)
	lines := bytes.SplitAfter(data, []byte{'\n'})
	outlier := []byte("10.0.0.1 - - [" + start.Add(72*time.Hour).Format("02/Jan/2006:15:04:05 -0700") + "] \"GET /clock-skew HTTP/1.1\" 200 1 \"-\" \"curl\"\n")
	lines = append(lines[:1000], append([][]byte{outlier}, lines[1000:]...)...)
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, bytes.Join(lines, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	until := start.Add(40 * time.Minute)
	code, stdout, _ := grepCommand(t, "", "--until", until.Format("2006-01-02 15:04:05"), "GET", path)
	want := 0
	filter := newGrepFilter(viewFormatAccess, nil, time.Now())
	for _, line := range lines {
		fields := filter.track(bytes.TrimSuffix(line, []byte{'\n'}))
		if at, ok := filter.timeOf(fields); ok && !at.After(until) {
			want++
		}
	}
	if code != 0 || strings.Count(stdout, "\n") != want || want < 2300 {
		t.Fatalf("until past an outlier: code %d got %d lines, want %d", code, strings.Count(stdout, "\n"), want)
	}
}
