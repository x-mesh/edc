//go:build linux

package edc

import (
	"os"
	"strings"
	"testing"
)

func TestClickHouseTraceOptionChecks(t *testing.T) {
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"clickhouse", "--port", "0"}, "--port must be from 1 to 65535"},
		{[]string{"clickhouse", "--port", "65536"}, "--port must be from 1 to 65535"},
		{[]string{"clickhouse", "--http-port", "0"}, "--http-port must be from 1 to 65535"},
		{[]string{"clickhouse", "--http-port", "65536"}, "--http-port must be from 1 to 65535"},
		{[]string{"clickhouse", "--http-port", "9000"}, "both use 9000"},
		{[]string{"clickhouse", "--port", "8123"}, "both use 8123"},
		{[]string{"clickhouse", "--port", "9001", "--http-port", "9001"}, "both use 9001"},
		{[]string{"clickhouse", "--tls"}, "--tls"},
		{[]string{"clickhouse", "--payload"}, "--payload is not available for trace clickhouse"},
		{[]string{"clickhouse", "--side", "both"}, "--side"},
		// --show-secrets는 --payload 없이 받는다. 그래서 다음 검사인 port 범위에서 멈춘다.
		{[]string{"clickhouse", "--show-secrets", "--port", "0"}, "--port must be from 1 to 65535"},
	} {
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(test.args) })
		if code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("trace %q exit = %d, stderr %q, want 2 and %q", test.args, code, stderr, test.stderr)
		}
	}
}

func TestClickHouseTraceLabelAndPortFilter(t *testing.T) {
	for _, test := range []struct{ side, want string }{{"", "clickhouse"}, {traceClientSide, "clickhouse --side client"}, {traceServerSide, "clickhouse --side server"}} {
		if got := traceLabel("clickhouse", test.side); got != test.want {
			t.Fatalf("traceLabel(clickhouse, %q) = %q, want %q", test.side, got, test.want)
		}
	}
	if scope := (tcpTraceOptions{port: 9440, showSecrets: true}).scope("clickhouse"); scope.port != 9440 || !scope.showSecrets || scope.protocol != "clickhouse" {
		t.Fatalf("scope = %+v", scope)
	}
	if scope := (tcpTraceOptions{port: 9440, httpPort: 18123}).scope("clickhouse"); scope.port != 9440 || scope.httpPort != 18123 {
		t.Fatalf("scope = %+v", scope)
	}
	for _, test := range []struct {
		port, httpPort   uint16
		wantNative, want uint16
	}{{0, 0, 9000, 8123}, {9440, 18123, 9440, 18123}} {
		filter := captureEventFilterFor(traceScope{protocol: "clickhouse", port: test.port, httpPort: test.httpPort})
		if filter.mysqlPort != test.wantNative || filter.clickHouseHTTPPort != test.want {
			t.Fatalf("ports %d/%d filter = %d/%d, want %d/%d", test.port, test.httpPort, filter.mysqlPort, filter.clickHouseHTTPPort, test.wantNative, test.want)
		}
	}
	for _, test := range []struct {
		port uint16
		want uint16
	}{{0, 9000}, {9440, 9440}} {
		if got := captureEventFilterFor(traceScope{protocol: "clickhouse", port: test.port}).mysqlPort; got != test.want {
			t.Fatalf("port %d filter = %d, want %d", test.port, got, test.want)
		}
	}
}

func TestClickHouseTraceSlowOptionChecks(t *testing.T) {
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"clickhouse", "--slow", "0s"}, "--slow must be greater than 0"},
		{[]string{"clickhouse", "--slow", "-1ms"}, "--slow must be greater than 0"},
		{[]string{"clickhouse", "--slow", "1.5ms", "--port", "0"}, "--port must be from 1 to 65535"},
	} {
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(test.args) })
		if code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("trace %q exit = %d, stderr %q, want 2 and %q", test.args, code, stderr, test.stderr)
		}
	}
}

func TestClickHouseTraceSlowRejectsNonInteractiveModes(t *testing.T) {
	for _, language := range supportedLanguages {
		restore := currentLanguage()
		setLanguage(language)
		for _, args := range [][]string{
			{"clickhouse", "--slow", "1ms", "--raw"},
			{"clickhouse", "--slow", "1ms", "--json", "report.json"},
			{"clickhouse", "--slow", "1ms", "--group-by", "process"},
		} {
			var code int
			stderr := captureTraceStderr(t, func() { code = runTrace(args) })
			if code != 2 || !strings.Contains(stderr, T("cli.trace.slow_interactive")) {
				t.Fatalf("%s trace %q exit = %d, stderr %q", language, args, code, stderr)
			}
		}
		setLanguage(restore)
	}
}

func TestClickHouseTraceSlowRejectsRedirectedTerminal(t *testing.T) {
	previous := traceIsTerminal
	defer func() { traceIsTerminal = previous }()
	traceIsTerminal = func(*os.File) bool { return false }
	var code int
	stderr := captureTraceStderr(t, func() { code = runTrace([]string{"clickhouse", "--slow", "1ms"}) })
	if code != 2 || !strings.Contains(stderr, T("cli.trace.slow_interactive")) {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
}
