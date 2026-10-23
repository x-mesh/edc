//go:build linux || darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCtrCollectorCreateConfigHasReadOnlyAndNoPersistenceBoundaries(t *testing.T) {
	config := ctrCollectorCreateConfigValue("sha256:image", "edc-collector-test", "owner", "/var/lib/docker/containers/id/log.json")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tty             bool `json:"Tty"`
		NetworkDisabled bool `json:"NetworkDisabled"`
		HostConfig      struct {
			NetworkMode    string   `json:"NetworkMode"`
			ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
			Privileged     bool     `json:"Privileged"`
			PidMode        string   `json:"PidMode"`
			CgroupnsMode   string   `json:"CgroupnsMode"`
			CapDrop        []string `json:"CapDrop"`
			CapAdd         []string `json:"CapAdd"`
			LogConfig      struct {
				Type string `json:"Type"`
			} `json:"LogConfig"`
			Mounts []ctrCollectorMount `json:"Mounts"`
		} `json:"HostConfig"`
	}
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Tty || !got.NetworkDisabled || got.HostConfig.Privileged || !got.HostConfig.ReadonlyRootfs || got.HostConfig.NetworkMode != "none" || got.HostConfig.PidMode != "host" || got.HostConfig.CgroupnsMode != "host" || got.HostConfig.LogConfig.Type != "none" {
		t.Fatalf("unsafe create payload: %s", data)
	}
	if len(got.HostConfig.CapDrop) != 1 || got.HostConfig.CapDrop[0] != "ALL" || len(got.HostConfig.CapAdd) != 2 || got.HostConfig.CapAdd[0] != "SYS_PTRACE" || got.HostConfig.CapAdd[1] != "DAC_READ_SEARCH" {
		t.Fatalf("unexpected capabilities: %s", data)
	}
	if config.HostConfig.Memory != 128<<20 || config.HostConfig.PidsLimit != 64 || config.HostConfig.NanoCpus != 1_000_000_000 || !config.OpenStdin || !config.StdinOnce {
		t.Fatalf("unexpected helper limits or stdin settings: %+v", config)
	}
	if len(got.HostConfig.Mounts) != 2 || !got.HostConfig.Mounts[0].ReadOnly || got.HostConfig.Mounts[0].Source != "/sys/fs/cgroup" || !got.HostConfig.Mounts[1].ReadOnly {
		t.Fatalf("unexpected mounts: %s", data)
	}
}

func TestReadCtrCollectorAttachSeparatesStdoutAndRejectsTruncation(t *testing.T) {
	frame := func(stream byte, payload string) []byte {
		result := make([]byte, 8+len(payload))
		result[0] = stream
		binary.BigEndian.PutUint32(result[4:8], uint32(len(payload)))
		copy(result[8:], payload)
		return result
	}
	data := append(frame(2, "ignored diagnostic"), frame(1, `{"ok":true}`)...)
	got, err := readCtrCollectorAttach(bufioReader(data))
	if err != nil || !bytes.Equal(got, []byte(`{"ok":true}`)) {
		t.Fatalf("stdout=%q err=%v", got, err)
	}
	if _, err = readCtrCollectorAttach(bufioReader(frame(1, "{}")[:9])); err == nil {
		t.Fatal("accepted truncated frame")
	}
	overLimit := make([]byte, 8)
	overLimit[0] = 1
	binary.BigEndian.PutUint32(overLimit[4:], ctrCollectorOutputLimit+1)
	if _, err = readCtrCollectorAttach(bufioReader(overLimit)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}

func bufioReader(data []byte) *bufio.Reader {
	return bufio.NewReader(bytes.NewReader(data))
}

type ctrCollectorFakeOptions struct {
	attachStatus int
	stallHeader  bool
	stallBody    bool
	loseCreate   bool
	waitBody     string
	deleteStatus int
	imageOS      string
	imageArch    string
	protocol     string
	postPID      int
}

type ctrCollectorFakeState struct {
	mu         sync.Mutex
	order      []string
	config     ctrCollectorContainerConfig
	request    ctrCollectorRequest
	deleted    int
	active     int
	maxActive  int
	attachGate <-chan struct{}
}

func ctrCollectorFakeEngine(t *testing.T, options ctrCollectorFakeOptions, state *ctrCollectorFakeState) *ctrEngine {
	t.Helper()
	const api = "1.50"
	const targetID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const helperID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		state.mu.Lock()
		state.order = append(state.order, request.Method+" "+request.URL.RequestURI())
		state.mu.Unlock()
		switch {
		case path == "/version":
			fmt.Fprint(writer, `{"ApiVersion":"1.50","MinAPIVersion":"1.41"}`)
		case path == "/v"+api+"/info":
			fmt.Fprint(writer, `{"OSType":"linux","Architecture":"aarch64","DockerRootDir":"/var/lib/docker","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin"]}`)
		case strings.HasPrefix(path, "/v"+api+"/images/"):
			fmt.Fprintf(writer, `{"Id":"sha256:collector","Os":%q,"Architecture":%q,"Config":{"Labels":{"%s":%q}}}`, optionString(options.imageOS, "linux"), optionString(options.imageArch, "arm64"), ctrCollectorProtocolLabel, optionString(options.protocol, "1"))
		case path == "/v"+api+"/containers/create":
			body, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(body, &state.config)
			state.mu.Lock()
			state.active++
			if state.active > state.maxActive {
				state.maxActive = state.active
			}
			state.mu.Unlock()
			if options.loseCreate {
				hijacker, ok := writer.(http.Hijacker)
				if !ok {
					t.Error("server does not support hijacking")
					return
				}
				conn, _, err := hijacker.Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			fmt.Fprintf(writer, `{"Id":%q}`, helperID)
		case path == "/v"+api+"/containers/"+helperID+"/attach":
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("server does not support hijacking")
				return
			}
			conn, rw, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if options.stallHeader {
				<-request.Context().Done()
				conn.Close()
				return
			}
			status := options.attachStatus
			if status == 0 {
				status = http.StatusSwitchingProtocols
			}
			if status == http.StatusSwitchingProtocols {
				fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			} else {
				fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
			}
			if err = rw.Flush(); err != nil {
				t.Error(err)
				conn.Close()
				return
			}
			input, err := io.ReadAll(conn)
			if err != nil {
				t.Error(err)
				conn.Close()
				return
			}
			if options.stallBody {
				state.mu.Lock()
				state.order = append(state.order, "ATTACH_BODY")
				state.mu.Unlock()
				_, _ = conn.Write([]byte{1, 0, 0, 0, 0, 0, 0, 20, '{'})
				<-request.Context().Done()
				conn.Close()
				return
			}
			state.mu.Lock()
			state.order = append(state.order, "ATTACH_INPUT")
			gate := state.attachGate
			state.mu.Unlock()
			if gate != nil {
				<-gate
			}
			var protocolRequest ctrCollectorRequest
			if err = json.Unmarshal(bytes.TrimSpace(input), &protocolRequest); err != nil {
				t.Error(err)
			} else {
				state.mu.Lock()
				state.request = protocolRequest
				state.mu.Unlock()
			}
			response := ctrCollectorResponse{RequestID: protocolRequest.RequestID, TargetID: protocolRequest.TargetID, TargetPID: protocolRequest.TargetPID, ProtocolVersion: 1,
				FDStatus: ctrCollectorMetricStatus{Status: "available"}, ListenStatus: ctrCollectorMetricStatus{Status: "available"},
				OOMStatus: ctrCollectorMetricStatus{Status: "available"}, OOM: uint64Pointer(2), OOMKill: uint64Pointer(1),
				LogStatus: ctrCollectorMetricStatus{Status: "available"}, LogBytes: uint64Pointer(123)}
			payload, _ := json.Marshal(response)
			frame := make([]byte, 8+len(payload))
			frame[0] = 1
			binary.BigEndian.PutUint32(frame[4:], uint32(len(payload)))
			copy(frame[8:], payload)
			if _, err = conn.Write(frame); err != nil {
				t.Error(err)
			}
			conn.Close()
		case path == "/v"+api+"/containers/"+helperID+"/start":
			state.mu.Lock()
			state.order = append(state.order, "START")
			state.mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case path == "/v"+api+"/containers/"+helperID+"/wait":
			body := options.waitBody
			if body == "" {
				body = `{"StatusCode":0,"Error":null}`
			}
			fmt.Fprint(writer, body)
		case path == "/v"+api+"/containers/"+helperID+"/json" || strings.HasPrefix(path, "/v"+api+"/containers/edc-collector-") && strings.HasSuffix(path, "/json"):
			fmt.Fprintf(writer, `{"Id":%q,"Config":{"Labels":{"org.x-mesh.edc.collector.owner":%q}}}`, helperID, state.config.Labels["org.x-mesh.edc.collector.owner"])
		case path == "/v"+api+"/containers/"+targetID+"/json":
			pid := 42
			if options.postPID > 0 {
				pid = options.postPID
			}
			fmt.Fprintf(writer, `{"Id":%q,"Name":"/work","Config":{"Image":"work:latest","Tty":false},"State":{"Status":"running","Pid":%d,"StartedAt":"2026-10-23T10:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"},"HostConfig":{"LogConfig":{"Type":"json-file","Config":{}},"RestartPolicy":{"Name":"no"}},"LogPath":"/var/lib/docker/containers/%s/work-json.log"}`, targetID, pid, targetID)
		case request.Method == http.MethodDelete && path == "/v"+api+"/containers/"+helperID:
			state.mu.Lock()
			state.deleted++
			state.active--
			state.order = append(state.order, "DELETE")
			state.mu.Unlock()
			if options.deleteStatus != 0 {
				writer.WriteHeader(options.deleteStatus)
				fmt.Fprint(writer, `{"message":"denied"}`)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	})
	return ctrSocketFixture(t, handler)
}

func optionString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func uint64Pointer(value uint64) *uint64 { return &value }

func TestCtrCollectorLifecycleAttachesBeforeStartAndCleansUp(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{attachStatus: http.StatusSwitchingProtocols}, state)
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC), Disk: ctrDisk{LogDriver: "json-file", LogPath: "/var/lib/docker/containers/" + strings.Repeat("a", 64) + "/work-json.log"}}
	response, err := engine.collect(context.Background(), inspect, "registry/team/collector:dev")
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.request.TargetID != inspect.ID || state.request.TargetPID != inspect.PID || state.request.ProtocolVersion != 1 || !state.request.LogAvailable {
		t.Fatalf("request = %+v", state.request)
	}
	if state.config.Image != "sha256:collector" || state.config.User != "0:0" || state.config.HostConfig.LogConfig.Type != "none" || !state.config.HostConfig.ReadonlyRootfs || state.config.HostConfig.Privileged {
		t.Fatalf("create config = %+v", state.config)
	}
	attachIndex, startIndex, inputIndex, waitIndex, deleteIndex := -1, -1, -1, -1, -1
	for i, event := range state.order {
		switch event {
		case "ATTACH_INPUT":
			inputIndex = i
		case "START":
			startIndex = i
		case "DELETE":
			deleteIndex = i
		default:
			if strings.Contains(event, "/attach?") {
				attachIndex = i
			}
			if strings.Contains(event, "/wait?") {
				waitIndex = i
			}
		}
	}
	if attachIndex < 0 || startIndex < 0 || inputIndex < startIndex || waitIndex < inputIndex || deleteIndex < waitIndex {
		t.Fatalf("lifecycle order = %v", state.order)
	}
	if response.OOMKill == nil || *response.OOMKill != 1 || response.LogBytes == nil || *response.LogBytes != 123 || state.deleted != 1 {
		t.Fatalf("response=%+v deleted=%d", response, state.deleted)
	}
	imagePath := false
	for _, event := range state.order {
		if event == "GET /v1.50/images/registry%2Fteam%2Fcollector:dev/json" {
			imagePath = true
		}
	}
	if !imagePath {
		t.Fatalf("tagged image reference was not escaped as one path segment: %v", state.order)
	}
}

func TestCtrCollectorLifecycleAcceptsHTTP200Attach(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{attachStatus: http.StatusOK}, state)
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	if _, err := engine.collect(context.Background(), inspect, "collector:dev"); err != nil {
		t.Fatal(err)
	}
}

func TestCtrCollectorRejectsImageMismatchBeforeCreate(t *testing.T) {
	for _, options := range []ctrCollectorFakeOptions{{imageArch: "amd64"}, {protocol: "2"}, {imageOS: "windows"}} {
		state := &ctrCollectorFakeState{}
		engine := ctrCollectorFakeEngine(t, options, state)
		inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
		if _, err := engine.collect(context.Background(), inspect, "collector:dev"); err == nil {
			t.Fatalf("accepted incompatible image: %+v", options)
		}
		state.mu.Lock()
		created := state.active
		state.mu.Unlock()
		if created != 0 {
			t.Fatalf("created helper for incompatible image: %+v", options)
		}
	}
}

func TestCtrCollectorWaitEmptyObjectFailsAndDeletesHelper(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{waitBody: `{}`}, state)
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	if _, err := engine.collect(context.Background(), inspect, "collector:dev"); err == nil {
		t.Fatal("accepted empty wait response")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleted != 1 {
		t.Fatalf("helper cleanup count = %d", state.deleted)
	}
}

func TestCtrCollectorDetectsTargetPIDChangeAndReportsCleanupFailure(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{postPID: 43, deleteStatus: http.StatusForbidden}, state)
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	_, err := engine.collect(context.Background(), inspect, "collector:dev")
	var cleanupErr *ctrCollectorCleanupError
	if !errors.As(err, &cleanupErr) || cleanupErr.ID != strings.Repeat("b", 64) || !strings.Contains(err.Error(), "edc-collector-") {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestCtrCollectorCreateResponseLossChecksOwnerBeforeDelete(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{loseCreate: true}, state)
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	_, err := engine.collect(context.Background(), inspect, "collector:dev")
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("create response loss error = %v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.deleted != 1 {
		t.Fatalf("helper cleanup count = %d, order=%v", state.deleted, state.order)
	}
}

func TestCtrCollectorAttachCancelClosesStalledHandshake(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{stallHeader: true}, state)
	ctx, cancel := context.WithCancel(context.Background())
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	done := make(chan error, 1)
	go func() { _, err := engine.collect(ctx, inspect, "collector:dev"); done <- err }()
	deadline := time.After(2 * time.Second)
	for {
		state.mu.Lock()
		attached := false
		for _, event := range state.order {
			if strings.Contains(event, "/attach?") {
				attached = true
				break
			}
		}
		state.mu.Unlock()
		if attached {
			break
		}
		select {
		case <-deadline:
			t.Fatal("attach request did not arrive")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled collector returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collector did not cancel")
	}
}

func TestCtrCollectorAttachCancelClosesStalledBody(t *testing.T) {
	state := &ctrCollectorFakeState{}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{stallBody: true}, state)
	ctx, cancel := context.WithCancel(context.Background())
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	done := make(chan error, 1)
	go func() { _, err := engine.collect(ctx, inspect, "collector:dev"); done <- err }()
	deadline := time.After(2 * time.Second)
	for {
		state.mu.Lock()
		streaming := false
		for _, event := range state.order {
			if event == "ATTACH_BODY" {
				streaming = true
				break
			}
		}
		state.mu.Unlock()
		if streaming {
			break
		}
		select {
		case <-deadline:
			t.Fatal("collector did not reach attach body")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled collector returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collector body read did not cancel")
	}
}

func TestCtrCollectorSerializesHelperLifecycles(t *testing.T) {
	gate := make(chan struct{})
	state := &ctrCollectorFakeState{attachGate: gate}
	engine := ctrCollectorFakeEngine(t, ctrCollectorFakeOptions{}, state)
	inspect := ctrInspect{ID: strings.Repeat("a", 64), PID: 42, Status: "running", StartedAt: time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)}
	done := make(chan error, 2)
	go func() { _, err := engine.collect(context.Background(), inspect, "collector:dev"); done <- err }()
	deadline := time.After(2 * time.Second)
	for {
		state.mu.Lock()
		atAttach := false
		for _, event := range state.order {
			if event == "ATTACH_INPUT" {
				atAttach = true
				break
			}
		}
		state.mu.Unlock()
		if atAttach {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first collector did not reach attach")
		case <-time.After(10 * time.Millisecond):
		}
	}
	go func() { _, err := engine.collect(context.Background(), inspect, "collector:dev"); done <- err }()
	time.Sleep(50 * time.Millisecond)
	state.mu.Lock()
	if state.active != 1 || state.maxActive != 1 {
		t.Fatalf("overlapping helpers: active=%d max=%d", state.active, state.maxActive)
	}
	state.mu.Unlock()
	close(gate)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("collector lifecycle did not finish")
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.active != 0 || state.deleted != 2 || state.maxActive != 1 {
		t.Fatalf("active=%d deleted=%d max=%d", state.active, state.deleted, state.maxActive)
	}
}

func TestCtrCollectorRejectsInvalidMountPath(t *testing.T) {
	inspect := ctrInspect{ID: strings.Repeat("a", 64), Disk: ctrDisk{LogDriver: "json-file", LogPath: "/var/lib/docker/containers/../secret"}}
	if got := eligibleCtrLogPath("/var/lib/docker", inspect); got != "" {
		t.Fatalf("accepted unsafe log path %q", got)
	}
	if strings.Contains(filepath.Clean(ctrSegment("registry/team/collector:dev")), "registry/team") {
		t.Fatal("image ref path was not escaped")
	}
}
