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

// --since보다 오래전에 바뀐 파일은 열지 않는다.
func TestGrepCommandSkipsFilesOlderThanSince(t *testing.T) {
	dir := grepFixtureDir(t)
	old := filepath.Join(dir, "access.log")
	when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	if err := os.Chtimes(old, when, when); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := grepCommand(t, "", "--since", "2021-01-01", "users", old)
	if code != 1 || stdout != "" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
}
