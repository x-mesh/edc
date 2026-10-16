//go:build linux

package edc

import (
	"io"
	"testing"
	"time"
)

// tlsFixtureDrain은 fixture가 끝난 뒤 reader가 ring에 남은 레코드를 읽을 시간이다. stop을 닫으면 reader는 남은 레코드를 버리므로,
// 이 시간이 모자라면 시험은 요청 수가 모자라 실패한다.
const tlsFixtureDrain = 500 * time.Millisecond

type tlsFixtureOutput struct {
	text string
	err  error
}

// stopWhenTLSFixtureExits는 fixture의 stdout을 끝까지 읽고 tlsFixtureDrain 뒤에 stop을 닫는다. stdout은 fixture가 끝날 때 닫히고,
// fixture의 TLS 호출은 모두 그 전에 ring에 들어가므로 수집 창을 다 기다리지 않는다.
func stopWhenTLSFixtureExits(stdout io.Reader) (<-chan struct{}, <-chan tlsFixtureOutput) {
	stop := make(chan struct{})
	output := make(chan tlsFixtureOutput, 1)
	go func() {
		text, err := io.ReadAll(stdout)
		output <- tlsFixtureOutput{string(text), err}
		time.Sleep(tlsFixtureDrain)
		close(stop)
	}()
	return stop, output
}

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
