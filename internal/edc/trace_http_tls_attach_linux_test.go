//go:build linux

package edc

import (
	"io"
	"testing"
)

// startTLSFixtureOnAttach는 collector가 모든 hook을 붙이고 ring을 읽기 시작할 때 fixture의 stdin에 go 한 줄을 쓴다.
// fixture는 그 줄을 받은 뒤에 요청을 보내므로, BPF 적재가 느려도 요청이 attach보다 먼저 지나가지 않는다.
func startTLSFixtureOnAttach(t *testing.T, stdin io.Writer) {
	t.Helper()
	captureAttachedHook = func() {
		if _, err := io.WriteString(stdin, "go\n"); err != nil {
			t.Errorf("start the TLS fixture: %v", err)
		}
	}
	t.Cleanup(func() { captureAttachedHook = nil })
}
