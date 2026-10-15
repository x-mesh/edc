package edc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func richHTTPTestResult(t *testing.T) (Result, *httptest.Server) {
	t.Helper()
	server := httpHopsTestServer(t)
	path := filepath.Join(t.TempDir(), "page.html")
	result := probeHTTPWithOptions(context.Background(), server.URL+"/start", httpCheckOptions{detail: httpDetailFull, bodyPath: path, headers: http.Header{"X-Debug": {"on"}}})
	if result.Status != StatusPass {
		t.Fatalf("status = %s, error = %#v", result.Status, result.Error)
	}
	return result, server
}

func richHTTPStripped(lines []string) string {
	return ansi.Strip(strings.Join(lines, "\n"))
}

func TestRichHTTPDrawsTheRequestAndBodyPanels(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	result, server := richHTTPTestResult(t)

	lines := richEvidenceLines(true, result)
	drawn := strings.Join(lines, "\n")
	plain := richHTTPStripped(lines)
	for _, want := range []string{
		"╭─ requests",
		"1  301   " + server.URL + "/start  HTTP/1.1",
		"2  200   " + server.URL + "/login  HTTP/1.1",
		"> x-debug",
		"set-cookie",
		"sid=***; HttpOnly",
		"╭─ body  text/html; charset=utf-8 · ",
		"Log & in",
		"saved to ",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q:\n%s", want, plain)
		}
	}
	for _, want := range []string{
		"\033[1;30;46m 301 \033[0m",
		"\033[1;30;42m 200 \033[0m",
		"\033[1;4m" + server.URL + "/start\033[0m",
		"\033[33m> x-debug\033[0m",
		"\033[1mLog & in\033[0m",
	} {
		if !strings.Contains(drawn, want) {
			t.Errorf("missing SGR %q:\n%q", want, drawn)
		}
	}
	if strings.Contains(drawn, "\x1b[31m</body>") {
		t.Error("the body preview must keep the escaped escape bytes")
	}
	if richEvidenceLines(false, result) != nil {
		t.Error("plain output keeps the evidence text")
	}
}

// --redact는 report를 JSON으로 한 번 돌리므로 hops가 []interface{}, 숫자가 float64가 된다.
func TestRichHTTPReadsTheRedactedReport(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	result, _ := richHTTPTestResult(t)
	native := richHTTPStripped(richHTTPCheck(result, 100))

	report := Report{Results: []Result{result}}
	redactReport(&report)
	redacted := report.Results[0]
	if _, ok := redacted.Metrics["hops"].([]interface{}); !ok {
		t.Fatalf("the round trip must change the hops type: %T", redacted.Metrics["hops"])
	}
	drawn := richHTTPStripped(richHTTPCheck(redacted, 100))
	if strings.Contains(drawn, "127.0.0.1") || !strings.Contains(drawn, "peer <ip:") {
		t.Errorf("a redacted report must show only tokens:\n%s", drawn)
	}
	if strings.Count(drawn, "\n") != strings.Count(native, "\n") || !strings.Contains(drawn, "1  301   ") || !strings.Contains(drawn, " ms  ") {
		t.Errorf("the redacted drawing lost rows:\n%s\n---\n%s", drawn, native)
	}
}

func TestRichHTTPNeedsHops(t *testing.T) {
	result := Result{Probe: "http.check", Status: StatusPass, Metrics: map[string]interface{}{}, Evidence: []Evidence{{Label: "requests", Value: "x"}}}
	if richHTTPCheck(result, 100) != nil {
		t.Error("no hops falls back to the evidence text")
	}
}

// 실패하면 output.go의 ERROR 상자가 줄마다 "│ "를 붙인다. 패널을 겹쳐 그리지 않는다.
func TestRichHTTPFailureRowsHaveNoPanel(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://127.0.0.1:1/", http.StatusFound)
	}))
	defer server.Close()
	result := probeHTTPWithOptions(context.Background(), server.URL, httpCheckOptions{detail: httpDetailHops})
	if result.Status != StatusFail {
		t.Fatalf("status = %s", result.Status)
	}
	lines := richEvidenceLines(true, result)
	plain := richHTTPStripped(lines)
	if len(lines) == 0 || strings.ContainsAny(plain, "╭╰│") {
		t.Fatalf("failure rows = %q", plain)
	}
	if !strings.HasPrefix(plain, "1  302   "+server.URL) {
		t.Errorf("failure rows = %q", plain)
	}
}

func TestRichHTTPWaterfallSharesOneScale(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	result := Result{Probe: "http.check", Status: StatusPass, Evidence: []Evidence{{Label: T("observe.http.requests"), Value: "x"}}, Metrics: map[string]interface{}{
		"hops": []map[string]interface{}{
			{"url": "http://a.example", "status_code": 307, "proto": "HTTP/1.1", "dns_ms": int64(2), "connect_ms": int64(3), "ttfb_ms": int64(24), "peer_ip": "192.0.2.1"},
			{"url": "https://a.example/login", "status_code": 200, "proto": "HTTP/2.0", "dns_ms": int64(1), "connect_ms": int64(2), "tls_ms": int64(16), "ttfb_ms": int64(128), "decompressed": true},
		},
	}}
	lines := richHTTPCheck(result, 100)
	plain := strings.Split(richHTTPStripped(lines), "\n")
	if !strings.Contains(plain[0], "■ DNS  ■ TCP  ■ TLS  ■ TTFB") {
		t.Errorf("legend = %q", plain[0])
	}
	var columns []int
	var cells []int
	for _, line := range plain {
		if index := strings.Index(line, " ms  "); index >= 0 {
			columns = append(columns, liveWidth(line[:index]))
			cells = append(cells, strings.Count(line, "█"))
		}
	}
	if len(columns) != 2 || columns[0] != columns[1] {
		t.Fatalf("the totals must share a column:\n%s", strings.Join(plain, "\n"))
	}
	if maxCells := max(httpRichMinCells, 98*3/10); cells[1] != maxCells || cells[0] != 6 {
		t.Errorf("bar cells = %v, want [6 %d]", cells, maxCells)
	}
	if !strings.Contains(plain[2], "DNS 2 · TCP 3 · TTFB 24 · peer 192.0.2.1") || !strings.Contains(plain[5], "gzip decoded") {
		t.Errorf("details:\n%s", strings.Join(plain, "\n"))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "\033[34m█") {
		t.Error("DNS is blue")
	}
}

func TestRichHTTPStaysInsideTheWidth(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	result, _ := richHTTPTestResult(t)
	result.Metrics["body_preview"] = strings.Repeat("long line ", 40)
	for _, width := range []int{40, 60, 100} {
		for _, line := range richHTTPCheck(result, width) {
			if liveWidth(line) > width {
				t.Errorf("width %d: %d cells: %q", width, liveWidth(line), ansi.Strip(line))
			}
		}
	}
}
