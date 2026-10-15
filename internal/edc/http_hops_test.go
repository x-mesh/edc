package edc

import (
	"context"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func httpHopsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/start":
			http.Redirect(writer, request, "/login", http.StatusMovedPermanently)
		case "/login":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			writer.Header().Add("Set-Cookie", "sid=secret123; HttpOnly")
			writer.Header().Set("X-Seen-Debug", request.Header.Get("X-Debug"))
			writer.Header().Set("X-Seen-Host", request.Host)
			_, _ = writer.Write([]byte("<html><head><title>Log &amp; in</title></head>\n<body>\x1b[31m</body></html>\n"))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func evidenceValue(result Result, label string) (string, bool) {
	for _, evidence := range result.Evidence {
		if evidence.Label == label {
			return evidence.Value, true
		}
	}
	return "", false
}

// 지금까지는 redirect 개수와 최종 URL만 남아, 몇 번째 응답이 무엇을 돌려줬는지 알 수 없었다.
func TestHTTPCheckListsEachRedirectInOrder(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	server := httpHopsTestServer(t)

	result := probeHTTPWithOptions(context.Background(), server.URL+"/start", httpCheckOptions{detail: httpDetailHops})
	if result.Status != StatusPass {
		t.Fatalf("status = %s, error = %#v", result.Status, result.Error)
	}
	requests, ok := evidenceValue(result, T("observe.http.requests"))
	if !ok {
		t.Fatalf("the redirect path is missing: %#v", result.Evidence)
	}
	first := strings.Index(requests, "1  301  "+server.URL+"/start")
	second := strings.Index(requests, "2  200  "+server.URL+"/login")
	if first < 0 || second < first {
		t.Errorf("the requests must come in order:\n%s", requests)
	}
	if strings.Contains(requests, "set-cookie") {
		t.Errorf("headers belong to -v only:\n%s", requests)
	}
	if strings.Contains(result.Summary, T("observe.probe.final_request")) || !strings.Contains(result.Summary, T("observe.probe.redirects", 1)) {
		t.Errorf("the summary must leave the timings to the request lines: %q", result.Summary)
	}
	if hops, _ := result.Metrics["hops"].([]map[string]interface{}); len(hops) != 2 || hops[0]["status_code"] != http.StatusMovedPermanently {
		t.Errorf("metrics hops = %#v", result.Metrics["hops"])
	}
	if _, ok := evidenceValue(result, T("observe.http.body")); ok {
		t.Error("the body preview belongs to -v only")
	}
}

// doctor와 watch는 같은 probe를 부른다. 기본값은 지금과 같은 한 줄 요약이다.
func TestHTTPCheckKeepsTheSummaryWithoutDetail(t *testing.T) {
	server := httpHopsTestServer(t)
	result := probeHTTPWithOptions(context.Background(), server.URL+"/start", httpCheckOptions{})
	if len(result.Evidence) != 0 || result.Metrics["hops"] != nil {
		t.Errorf("no detail must add nothing: %#v", result)
	}
	if !strings.Contains(result.Summary, T("observe.probe.final_request")) {
		t.Errorf("summary = %q", result.Summary)
	}
}

func TestHTTPCheckWithoutRedirectKeepsOneLine(t *testing.T) {
	server := httpHopsTestServer(t)
	result := probeHTTPWithOptions(context.Background(), server.URL+"/login", httpCheckOptions{detail: httpDetailHops})
	if len(result.Evidence) != 0 || !strings.Contains(result.Summary, "TTFB") {
		t.Errorf("one request needs no request list: %#v", result)
	}
}

func TestHTTPCheckVerboseShowsHeadersAndTheBody(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	server := httpHopsTestServer(t)
	headers := httpHeaderFlags{}
	for _, value := range []string{"X-Debug: 1", "Authorization: Bearer token-value", "Host: example.test"} {
		if err := headers.Set(value); err != nil {
			t.Fatal(err)
		}
	}

	result := probeHTTPWithOptions(context.Background(), server.URL+"/start", httpCheckOptions{detail: httpDetailFull, headers: headers.header()})
	requests, _ := evidenceValue(result, T("observe.http.requests"))
	for _, want := range []string{"> x-debug", "> authorization   ***", "set-cookie      sid=***; HttpOnly", "x-seen-debug    1", "location"} {
		if !strings.Contains(requests, want) {
			t.Errorf("the request list misses %q:\n%s", want, requests)
		}
	}
	for _, leaked := range []string{"secret123", "token-value"} {
		if strings.Contains(requests, leaked) {
			t.Errorf("a secret value leaked:\n%s", requests)
		}
	}
	if !strings.Contains(requests, "x-seen-host     example.test") {
		t.Errorf("-H Host must set the Host header like curl:\n%s", requests)
	}

	body, ok := evidenceValue(result, T("observe.http.body"))
	if !ok {
		t.Fatalf("the body preview is missing: %#v", result.Evidence)
	}
	for _, want := range []string{"text/html; charset=utf-8", "2 line(s)", "title  Log & in", `\x1b[31m`} {
		if !strings.Contains(body, want) {
			t.Errorf("the body preview misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\x1b") {
		t.Errorf("the body must not carry a raw escape:\n%q", body)
	}
	if result.Metrics["title"] != "Log & in" {
		t.Errorf("metrics title = %#v", result.Metrics["title"])
	}
}

// 중간 요청이 실패하면 몇 번째 redirect까지 갔는지가 원인을 좁힌다.
func TestHTTPCheckFailureKeepsTheRequestsSoFar(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	listener.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://"+closed+"/gone", http.StatusFound)
	}))
	defer server.Close()

	result := probeHTTPWithOptions(context.Background(), server.URL+"/", httpCheckOptions{detail: httpDetailHops})
	if result.Status != StatusFail {
		t.Fatalf("status = %s", result.Status)
	}
	requests, ok := evidenceValue(result, T("observe.http.requests"))
	if !ok || !strings.Contains(requests, "1  302  "+server.URL+"/") {
		t.Errorf("the failed result must keep the first request: %#v", result.Evidence)
	}
}

func TestHTTPHeaderFlagsRejectBrokenHeaders(t *testing.T) {
	for _, value := range []string{"NoColon", "Bad Name: x", ": x", "X-A: one\r\nX-B: two", "X-A: a\nb", "X-A: a\x01b", "X-A: a\x7fb"} {
		var headers httpHeaderFlags
		if err := headers.Set(value); err == nil {
			t.Errorf("Set(%q) must fail", value)
		}
	}
	var headers httpHeaderFlags
	for _, value := range []string{"X-A: 1", "x-a:2", "X-Empty:"} {
		if err := headers.Set(value); err != nil {
			t.Errorf("Set(%q): %v", value, err)
		}
	}
	header := headers.header()
	if got := header.Values("X-A"); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Errorf("X-A values = %#v", got)
	}
	if got, ok := header["X-Empty"]; !ok || got[0] != "" {
		t.Errorf("an empty value is still a header: %#v", header)
	}
}

func TestHTTPHeaderDisplayValueHidesSecrets(t *testing.T) {
	tests := []struct{ name, value, want string }{
		{"set-cookie", "sid=abc; Path=/; Secure", "sid=***; Path=/; Secure"},
		{"cookie", "a=1; b=2", "a=***; b=***"},
		{"authorization", "Basic dXNlcjpwYXNz", "***"},
		{"x-api-key", "k", "***"},
		{"content-type", "text/plain", "text/plain"},
		{"server", "evil\x1b[2J", `evil\x1b[2J`},
	}
	for _, test := range tests {
		if got := httpHeaderDisplayValue(test.name, test.value); got != test.want {
			t.Errorf("httpHeaderDisplayValue(%q, %q) = %q, want %q", test.name, test.value, got, test.want)
		}
	}
}

func TestHTTPBodyCounterCountsLines(t *testing.T) {
	for _, test := range []struct {
		parts []string
		lines int64
	}{
		{nil, 0},
		{[]string{"one"}, 1},
		{[]string{"one\n"}, 1},
		{[]string{"one\ntw", "o\nthree"}, 3},
	} {
		counter := &httpBodyCounter{keep: 4}
		for _, part := range test.parts {
			_, _ = counter.Write([]byte(part))
		}
		if counter.lines() != test.lines {
			t.Errorf("%q: lines = %d, want %d", test.parts, counter.lines(), test.lines)
		}
		if len(counter.preview) > 4 {
			t.Errorf("%q: preview kept %d bytes", test.parts, len(counter.preview))
		}
	}
}

// flag package는 첫 위치 인자에서 멈추므로 `edc http check URL -v`가 "needs one target"으로 실패했다.
func TestParseInterspersedReadsFlagsAfterTheTarget(t *testing.T) {
	tests := []struct {
		args       []string
		positional []string
		verbose    bool
	}{
		{[]string{"https://example.com", "-v"}, []string{"https://example.com"}, true},
		{[]string{"-v", "https://example.com"}, []string{"https://example.com"}, true},
		{[]string{"https://example.com"}, []string{"https://example.com"}, false},
		{[]string{"--", "-v"}, []string{"-v"}, false},
		{[]string{"a", "--", "-v"}, []string{"a", "-v"}, false},
		{[]string{"a", "b"}, []string{"a", "b"}, false},
	}
	for _, test := range tests {
		set := flag.NewFlagSet("test", flag.ContinueOnError)
		verbose := set.Bool("v", false, "")
		positional, err := parseInterspersed(set, test.args)
		if err != nil {
			t.Fatalf("%q: %v", test.args, err)
		}
		if strings.Join(positional, " ") != strings.Join(test.positional, " ") || *verbose != test.verbose {
			t.Errorf("%q: positional = %q, verbose = %t", test.args, positional, *verbose)
		}
	}
}

func TestHTTPCheckSavesTheWholeBody(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	server := httpHopsTestServer(t)
	path := filepath.Join(t.TempDir(), "page.html")

	result := probeHTTPWithOptions(context.Background(), server.URL+"/start", httpCheckOptions{detail: httpDetailHops, bodyPath: path})
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(saved), "<html><head><title>Log &amp; in</title>") || int64(len(saved)) != int64(result.Metrics["bytes_read"].(int64)) {
		t.Errorf("saved body = %q, bytes_read = %#v", saved, result.Metrics["bytes_read"])
	}
	body, ok := evidenceValue(result, T("observe.http.body"))
	if !ok || !strings.Contains(body, T("observe.http.body_saved", path, len(saved))) {
		t.Errorf("the result must say where the body went: %#v", result.Evidence)
	}
	if result.Metrics["body_output"] != path || result.Metrics["body_truncated"] != nil {
		t.Errorf("metrics = %#v", result.Metrics)
	}
}

func TestHTTPCheckMarksABodyCutAtTheLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(make([]byte, httpBodyLimit+5))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "big.bin")

	result := probeHTTPWithOptions(context.Background(), server.URL, httpCheckOptions{detail: httpDetailHops, bodyPath: path})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != httpBodyLimit || result.Metrics["body_truncated"] != true {
		t.Errorf("size = %d, metrics = %#v", info.Size(), result.Metrics)
	}
	if body, _ := evidenceValue(result, T("observe.http.body")); !strings.Contains(body, T("observe.http.body_truncated", "10 MiB")) {
		t.Errorf("the cut must be visible: %q", body)
	}
}

func TestHTTPCheckFailsWhenTheBodyCannotBeSaved(t *testing.T) {
	server := httpHopsTestServer(t)
	path := filepath.Join(t.TempDir(), "missing", "page.html")
	result := probeHTTPWithOptions(context.Background(), server.URL+"/login", httpCheckOptions{detail: httpDetailHops, bodyPath: path})
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "output" {
		t.Errorf("result = %#v", result)
	}
}

// 4 KiB 미리보기가 멀티바이트 문자 중간에서 잘리면 Content-Type이 없는 텍스트가 binary로 판정되었다.
func TestHTTPCheckKeepsATextBodyCutInsideARune(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	body := strings.Repeat("가", 2000)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header()["Content-Type"] = nil
		_, _ = writer.Write([]byte(body))
	}))
	defer server.Close()

	result := probeHTTPWithOptions(context.Background(), server.URL, httpCheckOptions{detail: httpDetailFull})
	preview, _ := evidenceValue(result, T("observe.http.body"))
	if strings.Contains(preview, T("observe.http.binary")) || strings.Contains(preview, `\x`) || !strings.Contains(preview, "가가가") {
		t.Errorf("a UTF-8 body cut at the preview limit must stay text:\n%s", preview)
	}
}

func TestHTTPTrimPartialRune(t *testing.T) {
	whole := []byte("ab가")
	for cut, want := range map[int]string{len(whole): "ab가", len(whole) - 1: "ab", len(whole) - 2: "ab", 2: "ab"} {
		if got := string(httpTrimPartialRune(whole[:cut])); got != want {
			t.Errorf("cut %d = %q, want %q", cut, got, want)
		}
	}
}

// --body -의 stdout이 터미널이면 상대가 보낸 escape sequence를 실행하지 않도록 바꿔 쓴다.
func TestHTTPBodyFileEscapesForATerminal(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	saved := &httpBodyFile{path: "-", file: file, escaped: true}
	_, _ = saved.Write([]byte("hi \x1b]52;c;ZXZpbA==\x07 there\n"))
	if err := saved.close(); err != nil || saved.err != nil {
		t.Fatal(err, saved.err)
	}
	_ = file.Close()
	written, _ := os.ReadFile(file.Name())
	if strings.ContainsRune(string(written), 0x1b) || !strings.Contains(string(written), `\x1b]52`) || !strings.HasSuffix(string(written), "there\n") {
		t.Errorf("written = %q", written)
	}
}

// flag 값이 "--"이면 위치 인자의 끝 표시가 아니다.
func TestParseInterspersedKeepsADashDashValue(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	body := set.String("body", "", "")
	verbose := set.Bool("v", false, "")
	positional, err := parseInterspersed(set, []string{"--body", "--", "https://example.com", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if *body != "--" || !*verbose || strings.Join(positional, " ") != "https://example.com" {
		t.Errorf("body = %q, verbose = %t, positional = %q", *body, *verbose, positional)
	}
	set = flag.NewFlagSet("test", flag.ContinueOnError)
	set.Bool("v", false, "")
	positional, _ = parseInterspersed(set, []string{"-v", "--", "-v"})
	if strings.Join(positional, " ") != "-v" {
		t.Errorf("after a boolean flag, -- still ends the flags: %q", positional)
	}
}
