package edc

import (
	"cmp"
	"context"
	"net"
	"time"
)

// check.endpoint는 설정의 require_endpoints에 TCP로 접속해 본다. check의 다른 점검은 호스트만 읽지만, 이 점검은 패킷을
// 보내므로 대상을 적었을 때만 돈다. 판정 기준은 edc tcp check와 같다.

// checkEndpointTimeout은 endpoint 하나에 쓰는 접속 시간이다. --timeout이 더 짧으면 그쪽을 따른다.
const checkEndpointTimeout = 5 * time.Second

type checkEndpointOutcome struct {
	Address string
	// Connect는 접속에 걸린 시간이다. Err가 있으면 Kind는 classifyNetworkError의 분류이고 Timeout은 기다린 시간이다.
	Connect time.Duration
	Err     error
	Kind    string
	Timeout time.Duration
}

// probeCheckEndpoints는 대상에 함께 접속한다. 결과는 설정과 같은 순서다.
func probeCheckEndpoints(ctx context.Context, addresses []string) []checkEndpointOutcome {
	endpoints := make([]checkEndpointOutcome, len(addresses))
	done := make(chan struct{})
	for index, address := range addresses {
		go func() {
			endpoints[index] = probeCheckEndpoint(ctx, address)
			done <- struct{}{}
		}()
	}
	for range addresses {
		<-done
	}
	return endpoints
}

func probeCheckEndpoint(parent context.Context, address string) checkEndpointOutcome {
	timeout := checkEndpointTimeout
	if deadline, ok := parent.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	endpoint := checkEndpointOutcome{Address: address, Connect: time.Since(started), Timeout: timeout}
	if err != nil {
		endpoint.Err, endpoint.Kind = err, classifyNetworkError(err)
		return endpoint
	}
	connection.Close()
	return endpoint
}

func checkEndpointResult(facts checkFacts, policy checkPolicy) Result {
	const id = "endpoint"
	var issues checkIssues
	// next는 처음 실패한 endpoint를 가리킨다. 느린 endpoint는 실패한 것이 없을 때만 가리킨다.
	failed, slow := "", ""
	for _, endpoint := range facts.Endpoints {
		switch {
		case endpoint.Err != nil:
			issues.add(StatusFail, checkEndpointFailure(endpoint))
			if failed == "" {
				failed = endpoint.Address
			}
		case latencyStatus(endpoint.Connect) != StatusPass:
			issues.add(StatusWarn, T("check.endpoint.slow", endpoint.Address, endpoint.Connect.Round(time.Millisecond)))
			if slow == "" {
				slow = endpoint.Address
			}
		}
	}
	next := ""
	if address := cmp.Or(failed, slow); address != "" {
		next = "edc tcp check --diagnose " + quoteLocalShellWord(address)
	}
	return issues.result(id, T("check.endpoint.ok", len(facts.Endpoints)), next)
}

func checkEndpointFailure(endpoint checkEndpointOutcome) string {
	switch endpoint.Kind {
	case "connection_refused":
		return T("check.endpoint.refused", endpoint.Address)
	case "timeout":
		return T("check.endpoint.timeout", endpoint.Address, endpoint.Timeout.Round(time.Millisecond))
	case "dns":
		return T("check.endpoint.dns", endpoint.Address)
	}
	return T("check.endpoint.failed", endpoint.Address, infoSingleLine(endpoint.Err.Error()))
}
