package edc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func checkEndpointTestConfig(endpoints ...string) checkConfig {
	return checkConfig{Routines: map[string]checkRoutineConfig{"web": {RequireEndpoints: endpoints}}}
}

func TestCheckEndpointJudgesEachAddress(t *testing.T) {
	refused := checkEndpointOutcome{Address: "127.0.0.1:1", Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), Kind: "connection_refused", Timeout: checkEndpointTimeout}
	timedOut := checkEndpointOutcome{Address: "10.0.0.9:443", Err: errors.New("dial tcp 10.0.0.9:443: i/o timeout"), Kind: "timeout", Timeout: 2 * time.Second}
	slow := checkEndpointOutcome{Address: "db:5432", Connect: 1500 * time.Millisecond}
	fast := checkEndpointOutcome{Address: "cache:6379", Connect: 3 * time.Millisecond}
	policy := newCheckPolicy(checkEndpointTestConfig("x:1"), "web")
	for _, test := range []struct {
		name      string
		endpoints []checkEndpointOutcome
		status    Status
		summary   string
		next      string
	}{
		{"all pass", []checkEndpointOutcome{fast, {Address: "db:5432", Connect: time.Second}}, StatusPass, "2 endpoints reachable", ""},
		{"one refused", []checkEndpointOutcome{fast, refused}, StatusFail, "127.0.0.1:1 refused the connection", "edc tcp check --diagnose 127.0.0.1:1"},
		{"one slow", []checkEndpointOutcome{slow, fast}, StatusWarn, "db:5432 connected in 1.5s", "edc tcp check --diagnose db:5432"},
		{"mixed", []checkEndpointOutcome{slow, timedOut, refused}, StatusFail, "db:5432 connected in 1.5s; 10.0.0.9:443 did not answer in 2s; 127.0.0.1:1 refused the connection", "edc tcp check --diagnose 10.0.0.9:443"},
	} {
		result := checkEndpointResult(checkFacts{Endpoints: test.endpoints}, policy)
		if result.Probe != "check.endpoint" || result.Status != test.status || result.Summary != test.summary || result.Next != test.next {
			t.Errorf("%s: result = %+v", test.name, result)
		}
		if (result.Error != nil) != (test.status == StatusFail) {
			t.Errorf("%s: error = %+v", test.name, result.Error)
		}
	}
}

func TestCheckEndpointAppearsOnlyWhenConfigured(t *testing.T) {
	calls := 0
	sources := checkTestSources()
	sources.endpoints = func(_ context.Context, addresses []string) []checkEndpointOutcome {
		calls++
		outcomes := make([]checkEndpointOutcome, len(addresses))
		for index, address := range addresses {
			outcomes[index] = checkEndpointOutcome{Address: address, Connect: time.Millisecond}
		}
		return outcomes
	}
	probes := func(policy checkPolicy) string {
		sources.requireEndpoints = policy.requireEndpoints
		var ids []string
		for _, result := range evaluateCheck(collectCheckFacts(context.Background(), sources, nil), policy) {
			ids = append(ids, result.Probe)
		}
		return strings.Join(ids, " ")
	}
	if ids := probes(newCheckPolicy(checkConfig{}, "")); strings.Contains(ids, "endpoint") || calls != 0 {
		t.Fatalf("no config: %s, calls %d", ids, calls)
	}
	// 전역 [check]의 endpoint는 루틴 없이도 돈다. 루틴의 endpoint는 전역에 더해진다.
	config := checkEndpointTestConfig("api:8080")
	config.RequireEndpoints = []string{"db:5432"}
	if policy := newCheckPolicy(config, "web"); strings.Join(policy.requireEndpoints, ",") != "db:5432,api:8080" {
		t.Fatalf("policy endpoints = %v", policy.requireEndpoints)
	}
	if ids := probes(newCheckPolicy(config, "")); !strings.HasSuffix(ids, " check.endpoint") || calls != 1 {
		t.Fatalf("global config: %s, calls %d", ids, calls)
	}
	// checks가 endpoint를 빼면 결과에 없고 접속도 하지 않는다.
	config.Routines["web"] = checkRoutineConfig{Checks: []string{"service"}, RequireEndpoints: []string{"api:8080"}}
	if ids := probes(newCheckPolicy(config, "web")); ids != "check.service" || calls != 1 {
		t.Fatalf("filtered routine: %s, calls %d", ids, calls)
	}
	config.Routines["web"] = checkRoutineConfig{Checks: []string{"service", "endpoint"}, RequireEndpoints: []string{"api:8080"}}
	if ids := probes(newCheckPolicy(config, "web")); ids != "check.service check.endpoint" || calls != 2 {
		t.Fatalf("selected routine: %s, calls %d", ids, calls)
	}
}

func TestCheckEndpointSkipsWhenTheSourceTimesOut(t *testing.T) {
	sources := checkTestSources()
	sources.requireEndpoints = []string{"api:8080"}
	// deadline과 함께 돌아온 결과는 버리지 않으므로, 끝나지 않는 원천으로 막아야 skip이 된다.
	block := make(chan struct{})
	defer close(block)
	sources.endpoints = func(context.Context, []string) []checkEndpointOutcome {
		<-block
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	policy := newCheckPolicy(checkConfig{RequireEndpoints: sources.requireEndpoints}, "")
	results := evaluateCheck(collectCheckFacts(ctx, sources, nil), policy)
	last := results[len(results)-1]
	if last.Probe != "check.endpoint" || last.Status != StatusSkip {
		t.Fatalf("timed out endpoint = %+v", last)
	}
}

func TestProbeCheckEndpointsConnectsAndClassifiesFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddress := closed.Addr().String()
	closed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcomes := probeCheckEndpoints(ctx, []string{listener.Addr().String(), refusedAddress})
	if len(outcomes) != 2 || outcomes[0].Err != nil || outcomes[0].Address != listener.Addr().String() {
		t.Fatalf("open port = %+v", outcomes)
	}
	if outcomes[1].Kind != "connection_refused" || outcomes[1].Err == nil {
		t.Fatalf("closed port = %+v", outcomes[1])
	}
	// --timeout이 endpoint 시간보다 짧으면 그쪽을 따른다.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if outcome := probeCheckEndpoint(short, listener.Addr().String()); outcome.Timeout > 50*time.Millisecond {
		t.Fatalf("timeout = %s", outcome.Timeout)
	}
}

func TestValidateCheckEndpointsNeedsHostAndPort(t *testing.T) {
	if err := validateCheckEndpoints("check", []string{"example.com:443", "[::1]:22", "10.0.0.1:5432"}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"localhost", "host:", ":443", "::1:443", ""} {
		if err := validateCheckEndpoints("check", []string{endpoint}); err == nil || !strings.Contains(err.Error(), "check.require_endpoints") {
			t.Errorf("%q: error = %v", endpoint, err)
		}
	}
}
