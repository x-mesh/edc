package edc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeCtrCollectorResponseRequiresCorrelationAndOneJSON(t *testing.T) {
	request := ctrCollectorRequest{RequestID: "request", TargetID: strings.Repeat("a", 64), TargetPID: 42, ProtocolVersion: 1}
	valid := `{"request_id":"request","target_id":"` + request.TargetID + `","target_pid":42,"protocol_version":1,"fd_status":{"status":"available"},"listen_status":{"status":"available"},"oom_status":{"status":"available"},"oom":0,"oom_kill":0,"log_status":{"status":"available"},"log_bytes":0}`
	response, err := decodeCtrCollectorResponse([]byte(valid), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = response.validate(); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{valid + ` {}`, valid + " trailing", strings.Replace(valid, `"request_id":"request"`, `"request_id":"other"`, 1)} {
		if _, err = decodeCtrCollectorResponse([]byte(data), request); err == nil {
			t.Fatalf("accepted invalid response: %q", data[len(data)-min(len(data), 24):])
		}
	}
}

func TestDecodeCtrCollectorResponseRejectsOversizedAndInvalidMetricShape(t *testing.T) {
	request := ctrCollectorRequest{RequestID: "request", TargetID: strings.Repeat("b", 64), TargetPID: 7, ProtocolVersion: 1}
	if _, err := decodeCtrCollectorResponse(make([]byte, ctrCollectorOutputLimit+1), request); err == nil {
		t.Fatal("accepted oversized response")
	}
	response := ctrCollectorResponse{
		RequestID: request.RequestID, TargetID: request.TargetID, TargetPID: request.TargetPID, ProtocolVersion: 1,
		FDStatus: ctrCollectorMetricStatus{Status: "available"}, ListenStatus: ctrCollectorMetricStatus{Status: "available"},
		OOMStatus: ctrCollectorMetricStatus{Status: "available"}, LogStatus: ctrCollectorMetricStatus{Status: "available"},
	}
	if err := response.validate(); err == nil {
		t.Fatal("accepted missing available metric values")
	}
}

func TestCtrCollectorImageReferenceAllowsTagAndDigest(t *testing.T) {
	for _, image := range []string{"edc-collector:dev", "registry.example/team/collector:v1", "collector@sha256:" + strings.Repeat("a", 64)} {
		if err := validateCtrCollectorImageReference(image); err != nil {
			t.Errorf("rejected %q: %v", image, err)
		}
	}
	for _, image := range []string{"", "-collector", "collector image", "collector?x=1", strings.Repeat("a", 256)} {
		if err := validateCtrCollectorImageReference(image); err == nil {
			t.Errorf("accepted %q", image)
		}
	}
}

func TestMarshalCtrCollectorResponseMarksTruncatedMetric(t *testing.T) {
	response := ctrCollectorResponse{
		FDStatus:     ctrCollectorMetricStatus{Status: "available"},
		ListenStatus: ctrCollectorMetricStatus{Status: "available"},
		OOMStatus:    collectorUnavailable("counter unavailable"),
		LogStatus:    collectorUnavailable("log unavailable"),
	}
	for inode := uint64(1); inode <= 20000; inode++ {
		response.Listeners = append(response.Listeners, ctrCollectorListen{Proto: "tcp", Address: "2001:db8::1", Port: 8080, Inode: inode})
	}
	data, err := marshalCtrCollectorResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(data)+9 > ctrCollectorOutputLimit {
		t.Fatalf("response frame exceeds limit: %d", len(data)+9)
	}
	var got ctrCollectorResponse
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ListenStatus.Status != "partial" || got.ListenStatus.Reason == "" || len(got.Listeners) >= len(response.Listeners) {
		t.Fatalf("truncation was not reported: %+v", got.ListenStatus)
	}
}

func TestCtrCollectorVerboseListenShowsObservedSocketsAndPublishedPorts(t *testing.T) {
	inspect := ctrInspect{PID: 42, Ports: []ctrPublishedPort{
		{Proto: "tcp", Port: 8080, Hosts: []string{"127.0.0.1:80"}},
		{Proto: "udp", Port: 5353, Hosts: []string{"0.0.0.0:5353"}},
		{Proto: "tcp", Port: 8443, Hosts: []string{"127.0.0.1:443"}},
	}}
	response := ctrCollectorResponse{
		ListenStatus: ctrCollectorMetricStatus{Status: "available"},
		Listeners: []ctrCollectorListen{
			{Proto: "tcp", Address: "0.0.0.0", Port: 8080, Inode: 111},
			{Proto: "udp", Address: "127.0.0.1", Port: 5353, Inode: 222},
			{Proto: "tcp", Address: "2001:db8::1", Port: 8443, Inode: 333},
		},
	}
	result := ctrListenResult(ctrFacts{Reference: "web", Inspect: inspect, Collector: &response})
	var output strings.Builder
	printTerminalWithColor(&output, []Result{result}, true, false)
	for _, want := range []string{
		"TCP 0.0.0.0:8080",
		"UDP 127.0.0.1:5353",
		"TCP [2001:db8::1]:8443",
		"8080/tcp → 127.0.0.1:80",
		"5353/udp → 0.0.0.0:5353",
		"8443/tcp → 127.0.0.1:443",
		"inode 111",
		"loopback_only",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("verbose listen output misses %q:\n%s", want, output.String())
		}
	}
}

func TestCtrCollectorPartialListenKeepsRowsAndFailureReason(t *testing.T) {
	inspect := ctrInspect{PID: 42, Ports: []ctrPublishedPort{
		{Proto: "tcp", Port: 8080, Hosts: []string{"127.0.0.1:80"}},
		{Proto: "tcp", Port: 9090, Hosts: []string{"127.0.0.1:90"}},
	}}
	response := ctrCollectorResponse{
		ListenStatus: ctrCollectorMetricStatus{Status: "partial", Reason: "one network namespace could not be read"},
		Listeners:    []ctrCollectorListen{{Proto: "tcp", Address: "0.0.0.0", Port: 8080, Inode: 444}},
	}
	result := ctrListenResult(ctrFacts{Reference: "web", Inspect: inspect, Collector: &response})
	var output strings.Builder
	printTerminalWithColor(&output, []Result{result}, true, false)
	for _, want := range []string{"TCP 0.0.0.0:8080", "inode 444", "one network namespace could not be read", "9090/tcp", "unavailable"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("partial listen output misses %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "9090/tcp → 127.0.0.1:90 (not_listening)") {
		t.Fatalf("partial collection confirmed an absent listener:\n%s", output.String())
	}
}

func TestCtrCollectorEmptyAndFailedListenStayExplicit(t *testing.T) {
	inspect := ctrInspect{PID: 42, Ports: []ctrPublishedPort{{Proto: "tcp", Port: 9090, Hosts: []string{"127.0.0.1:90"}}}}
	empty := ctrCollectorResponse{ListenStatus: ctrCollectorMetricStatus{Status: "available"}}
	result := ctrListenResult(ctrFacts{Reference: "web", Inspect: inspect, Collector: &empty})
	if result.Metrics["status"] != "available" || result.Metrics["published"].([]map[string]interface{})[0]["state"] != "not_listening" {
		t.Fatalf("complete empty result = %+v", result)
	}
	failed := ctrListenResult(ctrFacts{Reference: "web", Inspect: inspect, CollectorErr: errors.New("attach stream timed out")})
	if failed.Status != StatusSkip || !strings.Contains(failed.Summary, "attach stream timed out") {
		t.Fatalf("failed result = %+v", failed)
	}
}
