//go:build linux

package edc

import (
	"strings"
	"testing"
)

// traceOptionSamples는 option마다 parser가 받는 값이다. 값이 없는 option은 빈 문자열이다.
var traceOptionSamples = map[string]string{
	"duration": "1s", "slow": "1ms", "json": "out.json", "raw": "", "group-by": "process", "process": "1",
	"destination": "example.com:443", "detail": "", "container": "web", "side": "client", "payload": "all",
	"show-secrets": "", "tls": "", "tls-keylog": "keys.log", "port": "8080", "http-port": "8123",
	"reason": "NO_SOCKET", "device": "8:0", "signal": "all", "syscall": "openat", "errors": "all", "all": "",
	"files": "", "no-files": "", "fd-path": "", "denied": "", "frequency": "99", "folded": "stacks.txt", "off-cpu": "", "kind": "futex",
}

// 하위 명령 도움말의 option을 그 parser에 모두 주고 끝에 없는 option을 붙인다. parser가 도움말의 option을 모두 받으면 오류는
// 끝의 option을 가리키고, 그 전에 멈추므로 trace는 시작하지 않는다.
func TestTraceSubHelpOptionsParse(t *testing.T) {
	const sentinel = "--edc-help-sentinel"
	for _, sub := range traceSubDocs {
		args := []string{sub.name}
		for _, option := range sub.options {
			value, ok := traceOptionSamples[option]
			if !ok {
				t.Fatalf("%s: no sample value for --%s", sub.name, option)
			}
			if value == "" {
				args = append(args, "--"+option)
			} else {
				args = append(args, "--"+option+"="+value)
			}
		}
		args = append(args, sentinel)
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(args) })
		if code != 2 || !strings.Contains(stderr, "flag provided but not defined: -"+strings.TrimLeft(sentinel, "-")) {
			t.Errorf("%s: code %d, a listed option was rejected:\n%s", sub.name, code, stderr)
		}
	}
}
