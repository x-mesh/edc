//go:build linux

package edc

import (
	"os"
	"strconv"
	"time"

	"github.com/vishvananda/netlink"
)

// traceTLSExecChecks는 exec 알림을 받은 process를 다시 볼 시각이다. 알림은 동적 linker가 library를 적재하기 전에 오고,
// python처럼 시작한 뒤 TLS library를 dlopen하는 program도 있다. 첫 요청보다 먼저 붙도록 앞쪽을 촘촘히 둔다. 빌드처럼
// exec가 몰리면 확인 횟수가 그대로 CPU가 되므로 다섯 번만 본다. test가 바꾼다.
var traceTLSExecChecks = []time.Duration{5 * time.Millisecond, 25 * time.Millisecond, 200 * time.Millisecond, time.Second, 3 * time.Second}

// traceTLSExecPending은 다시 볼 process 수의 상한이다. 빌드처럼 exec가 몰리면 넘는 process는 전체 탐색에 맡긴다.
const traceTLSExecPending = 4096

// traceTLSRescanFloor와 traceTLSRescanCost는 모든 process를 다시 보는 주기다. 한 번 보는 데 걸린 시간의
// traceTLSRescanCost배와 traceTLSRescanFloor 중 긴 쪽을 기다려, process가 많은 host에서도 이 탐색이 CPU를 1% 넘게 쓰지
// 않게 한다. host 디렉터리는 trace를 시작할 때만 본다. test가 바꾼다.
var (
	traceTLSRescanFloor = 2 * time.Second
	traceTLSRescanCost  = 100
)

// traceTLSExecCheck는 exec한 process 하나와, traceTLSExecChecks에서 다음에 볼 순번이다.
type traceTLSExecCheck struct {
	pid   string
	start time.Time
	next  int
}

func (check traceTLSExecCheck) due() time.Time {
	return check.start.Add(traceTLSExecChecks[check.next])
}

// watchTraceTLS는 --tls 자동 탐색으로 trace를 시작한 뒤에 나타난 TLS 파일을 찾아 attach에 넘긴다. execs는 exec한
// process의 PID다. nil이면 exec 알림 없이 전체 탐색만 한다. stop이 닫히면 돌아온다.
func watchTraceTLS(finder *traceTLSFinder, execs <-chan uint32, attach func(traceTLSTarget), stop <-chan struct{}) {
	var checks []traceTLSExecCheck
	rescan := time.NewTimer(traceTLSRescanFloor)
	defer rescan.Stop()
	// scan은 탐색이 새로 고른 파일만 attach에 넘긴다. trace 중의 안내는 화면에 쓸 수 없으므로 쌓지 않는다.
	scan := func(find func()) {
		known := len(finder.targets)
		find()
		finder.notices = nil
		for _, target := range finder.targets[known:] {
			attach(target)
		}
	}
	for {
		var due <-chan time.Time
		if len(checks) > 0 {
			next := checks[0].due()
			for _, check := range checks[1:] {
				if check.due().Before(next) {
					next = check.due()
				}
			}
			due = time.After(time.Until(next))
		}
		select {
		case <-stop:
			return
		case pid, ok := <-execs:
			if !ok {
				execs = nil
				continue
			}
			if len(checks) < traceTLSExecPending {
				checks = append(checks, traceTLSExecCheck{pid: strconv.FormatUint(uint64(pid), 10), start: time.Now()})
			}
		case <-due:
		case <-rescan.C:
			started := time.Now()
			scan(finder.scanProcesses)
			interval := time.Since(started) * time.Duration(traceTLSRescanCost)
			if interval < traceTLSRescanFloor {
				interval = traceTLSRescanFloor
			}
			rescan.Reset(interval)
		}
		now := time.Now()
		kept := checks[:0]
		for _, check := range checks {
			if now.Before(check.due()) {
				kept = append(kept, check)
				continue
			}
			alive := true
			scan(func() { alive = finder.scanProcess(check.pid) })
			if check.next++; alive && check.next < len(traceTLSExecChecks) {
				kept = append(kept, check)
			}
		}
		checks = kept
	}
}

// traceTLSExecEvents는 kernel의 process 알림(proc connector)에서 exec한 process의 PID를 받는다. 구독하지 못하면 오류를
// 돌려주고, watchTraceTLS는 전체 탐색만 한다. 알림이 밀려 kernel이 버리면 구독이 끝나 channel이 닫힌다. stop이 닫히면
// 구독을 닫는다.
func traceTLSExecEvents(stop <-chan struct{}) (<-chan uint32, error) {
	events := make(chan netlink.ProcEvent, 256)
	failed := make(chan error, 1)
	if err := netlink.ProcEventMonitor(events, stop, failed); err != nil {
		return nil, err
	}
	execs := make(chan uint32, 256)
	go func() {
		defer close(execs)
		// 구독을 닫은 뒤에도 남은 알림을 끝까지 읽어야 netlink의 receive goroutine이 끝난다.
		for event := range events {
			exec, ok := event.Msg.(*netlink.ExecProcEvent)
			if !ok {
				continue
			}
			select {
			case execs <- exec.ProcessTgid:
			case <-stop:
			}
		}
	}()
	return execs, nil
}

// traceTLSExecProblem은 trace 중에 exec 알림을 받지 못할 이유이고, 없으면 ""이다. kernel은 처음 network namespace의
// 구독자에게만 알림을 보내고, 다른 namespace에서는 구독이 성공해도 알림이 오지 않는다. 그래서 PID 1과 namespace를
// 비교한다. 구독을 열어 시험하지 않는 것은, 6.17에서 같은 process가 구독 하나를 닫으면 먼저 연 구독에도 알림이 끊겼기
// 때문이다. 화면을 열기 전에 알리려고 trace 전에 부른다. test가 바꾼다.
var traceTLSExecProblem = func() string {
	self, selfErr := os.Readlink("/proc/self/ns/net")
	first, firstErr := os.Readlink("/proc/1/ns/net")
	if selfErr == nil && firstErr == nil && self != first {
		return "network namespace " + self
	}
	return ""
}
