//go:build linux

package edc

import (
	"context"
	"net"
	"sync"
	"time"
)

const (
	// diagnoseTraceStartLimit는 collector가 hook을 모두 붙이고 ring을 읽기 시작할 때까지 기다리는 상한이다. fentry와
	// fexit hook은 하나에 0.1초 넘게 걸려, 느린 host에서는 몇 초가 걸린다.
	diagnoseTraceStartLimit = 15 * time.Second
	diagnoseCanaryInterval  = 100 * time.Millisecond
	diagnoseCanaryTimeout   = 200 * time.Millisecond
)

// diagnoseCollector는 시작을 기다리는 collector 하나다. err는 done이 닫힌 뒤에만 읽는다.
type diagnoseCollector struct {
	skip    *string
	ready   chan struct{}
	done    chan struct{}
	err     error
	settled bool
}

// startDiagnoseTrace는 trace tcp와 trace drop의 collector를 이 process 안에서 돌린다. 둘 다 event를 읽기 시작한 뒤에
// 돌아와야 첫 시도의 SYN과 drop을 놓치지 않는다. 시작하지 못한 collector는 skip 이유를 남기고, 판정은 그 사실을 모르는
// 값으로 다룬다.
func startDiagnoseTrace(ctx context.Context, destination string) *diagnoseTraceRun {
	run := newDiagnoseTraceRun(destination)
	if err := captureEventsPrerequisites(); err != nil {
		run.tcpSkip = diagnoseSkipReason(err)
	}
	if err := dropTracePrerequisites(); err != nil {
		run.dropSkip = diagnoseSkipReason(err)
	}
	stop := make(chan struct{})
	var group sync.WaitGroup
	var collectors []*diagnoseCollector
	start := func(skip *string, collect func(func(captureEvent) error, <-chan struct{}) (captureSummary, error)) {
		if *skip != "" {
			return
		}
		collector := &diagnoseCollector{skip: skip, ready: make(chan struct{}), done: make(chan struct{})}
		collectors = append(collectors, collector)
		var once sync.Once
		group.Add(1)
		go func() {
			defer group.Done()
			defer close(collector.done)
			summary, err := collect(func(event captureEvent) error {
				// collector는 hook을 모두 붙인 뒤에 ring을 읽으므로, 어떤 event든 처음 받으면 시작한 것이다.
				once.Do(func() { close(collector.ready) })
				run.observe(event)
				return nil
			}, stop)
			collector.err = err
			// 시작하기 전에 끝난 collector는 waitDiagnoseCollectors가 skip 이유로 남긴다.
			select {
			case <-collector.ready:
				if err != nil {
					run.warn(T("diagnose.trace.failed", err))
				}
			default:
			}
			run.addLost(summary.LostEvents)
		}()
	}
	start(&run.tcpSkip, func(onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
		return collectCaptureEventsFor(traceScope{protocol: "tcp"}, 0, onEvent, stop)
	})
	start(&run.dropSkip, func(onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
		return collectDropEvents(nil, 0, onEvent, stop)
	})
	if len(collectors) == 0 {
		return run
	}
	var once sync.Once
	run.stop = func() { once.Do(func() { close(stop); group.Wait() }) }
	waitDiagnoseCollectors(ctx, collectors)
	return run
}

// waitDiagnoseCollectors는 닫힌 loopback port로 연결과 UDP를 보내 두 collector가 event를 받을 때까지 기다린다. 거부된
// 연결은 TCP 상태 event를, 받는 socket이 없는 SYN과 UDP는 drop event를 만든다. 정해진 시간을 기다리면 BPF 적재가 느린
// host에서 첫 시도를 놓친다.
func waitDiagnoseCollectors(ctx context.Context, collectors []*diagnoseCollector) {
	canary := diagnoseCanaryAddress()
	limit := time.NewTimer(diagnoseTraceStartLimit)
	defer limit.Stop()
	tick := time.NewTicker(diagnoseCanaryInterval)
	defer tick.Stop()
	for {
		pending := false
		for _, collector := range collectors {
			if collector.settled {
				continue
			}
			select {
			case <-collector.ready:
				collector.settled = true
			case <-collector.done:
				collector.settled = true
				*collector.skip = diagnoseSkipReason(collector.err)
			default:
				pending = true
			}
		}
		if !pending {
			return
		}
		sendDiagnoseCanary(canary)
		select {
		case <-ctx.Done():
			return
		case <-limit.C:
			for _, collector := range collectors {
				if !collector.settled {
					*collector.skip = T("diagnose.trace.not_ready", diagnoseTraceStartLimit)
				}
			}
			return
		case <-tick.C:
		}
	}
}

// diagnoseCanaryAddress는 잠깐 열었다 닫은 loopback port다. 아무도 듣지 않으므로 connect는 바로 거부된다.
func diagnoseCanaryAddress() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func sendDiagnoseCanary(address string) {
	if address == "" {
		return
	}
	if connection, err := net.DialTimeout("tcp", address, diagnoseCanaryTimeout); err == nil {
		connection.Close()
	}
	if connection, err := net.Dial("udp", address); err == nil {
		_, _ = connection.Write([]byte{0})
		connection.Close()
	}
}
