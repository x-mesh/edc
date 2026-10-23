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
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type ctrCollectorDaemonInfo struct {
	OSType        string   `json:"OSType"`
	Architecture  string   `json:"Architecture"`
	DockerRootDir string   `json:"DockerRootDir"`
	CgroupVersion string   `json:"CgroupVersion"`
	SecurityOpts  []string `json:"SecurityOptions"`
}

type ctrCollectorImageInfo struct {
	ID           string `json:"Id"`
	OS           string `json:"Os"`
	Architecture string `json:"Architecture"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type ctrCollectorCreated struct {
	ID string `json:"Id"`
}

type ctrCollectorMount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type ctrCollectorRestartPolicy struct {
	Name string `json:"Name"`
}

type ctrCollectorLogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

type ctrCollectorCreateHostConfig struct {
	NetworkMode    string                    `json:"NetworkMode"`
	ReadonlyRootfs bool                      `json:"ReadonlyRootfs"`
	Privileged     bool                      `json:"Privileged"`
	PidMode        string                    `json:"PidMode"`
	CgroupnsMode   string                    `json:"CgroupnsMode"`
	CapDrop        []string                  `json:"CapDrop"`
	CapAdd         []string                  `json:"CapAdd"`
	SecurityOpt    []string                  `json:"SecurityOpt"`
	RestartPolicy  ctrCollectorRestartPolicy `json:"RestartPolicy"`
	LogConfig      ctrCollectorLogConfig     `json:"LogConfig"`
	AutoRemove     bool                      `json:"AutoRemove"`
	Memory         int64                     `json:"Memory"`
	PidsLimit      int64                     `json:"PidsLimit"`
	NanoCpus       int64                     `json:"NanoCpus"`
	Mounts         []ctrCollectorMount       `json:"Mounts"`
}

type ctrCollectorContainerConfig struct {
	Image           string                       `json:"Image"`
	User            string                       `json:"User"`
	Entrypoint      []string                     `json:"Entrypoint"`
	AttachStdin     bool                         `json:"AttachStdin"`
	AttachStdout    bool                         `json:"AttachStdout"`
	AttachStderr    bool                         `json:"AttachStderr"`
	OpenStdin       bool                         `json:"OpenStdin"`
	StdinOnce       bool                         `json:"StdinOnce"`
	Tty             bool                         `json:"Tty"`
	NetworkDisabled bool                         `json:"NetworkDisabled"`
	Labels          map[string]string            `json:"Labels"`
	HostConfig      ctrCollectorCreateHostConfig `json:"HostConfig"`
}
type ctrCollectorContainerInfo struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type ctrCollectorCleanupError struct {
	Name  string
	ID    string
	Cause error
}

func (e *ctrCollectorCleanupError) Error() string {
	return fmt.Sprintf("helper %s (%s) cleanup failed; the remaining container must be checked: %v", e.Name, e.ID, e.Cause)
}

func (e *ctrCollectorCleanupError) Unwrap() error { return e.Cause }

func (e *ctrEngine) collectorRequest(ctx context.Context, method, path string, body []byte, limit int64) ([]byte, int, error) {
	if e.endpointErr != nil {
		return nil, 0, e.endpointErr
	}
	v, err := e.apiVersion(ctx)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, ctrRequestLimit)
	defer cancel()
	var source io.Reader
	if body != nil {
		source = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+"/v"+v+path, source)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(data)) > limit {
		return nil, resp.StatusCode, fmt.Errorf("Docker response exceeds %d bytes", limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, &ctrHTTPError{StatusCode: resp.StatusCode, Message: infoSingleLine(firstLine(strings.TrimSpace(string(data))))}
	}
	return data, resp.StatusCode, nil
}

func (e *ctrEngine) collect(ctx context.Context, inspect ctrInspect, image string) (ctrCollectorResponse, error) {
	if e.collectorSem == nil {
		return ctrCollectorResponse{}, fmt.Errorf("collector concurrency limit is unavailable")
	}
	select {
	case e.collectorSem <- struct{}{}:
	case <-ctx.Done():
		return ctrCollectorResponse{}, ctx.Err()
	}
	defer func() { <-e.collectorSem }()
	budget := ctxRemaining(ctx)
	if budget <= 0 {
		return ctrCollectorResponse{}, context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if inspect.ID == "" || !validCtrContainerID(inspect.ID) {
		return ctrCollectorResponse{}, fmt.Errorf("collector requires a full container ID")
	}
	if inspect.UsernsMode != "" {
		return ctrCollectorResponse{}, fmt.Errorf("container user namespace mode is unsupported")
	}
	if err := validateCtrCollectorImageReference(image); err != nil {
		return ctrCollectorResponse{}, fmt.Errorf("invalid collector image reference")
	}
	request, err := newCtrCollectorRequest(inspect, false, ctxRemaining(ctx))
	if err != nil {
		return ctrCollectorResponse{}, err
	}
	api, err := e.apiVersion(ctx)
	if err != nil {
		return ctrCollectorResponse{}, err
	}
	if !ctrCollectorAPIVersionSupported(api) {
		return ctrCollectorResponse{}, fmt.Errorf("collector requires Docker Engine API %s through %s", ctrMinAPIVersion, ctrMaxAPIVersion)
	}
	info, err := e.collectorInfo(ctx)
	if err != nil {
		return ctrCollectorResponse{}, err
	}
	if strings.ToLower(info.OSType) != "linux" {
		return ctrCollectorResponse{}, fmt.Errorf("collector requires a Linux Docker daemon")
	}
	if info.CgroupVersion != "2" {
		return ctrCollectorResponse{}, fmt.Errorf("collector requires cgroup v2")
	}
	for _, option := range info.SecurityOpts {
		if strings.Contains(strings.ToLower(option), "rootless") || strings.Contains(strings.ToLower(option), "userns") {
			return ctrCollectorResponse{}, fmt.Errorf("rootless or user namespace Docker daemon is unsupported")
		}
	}
	imageInfo, err := e.collectorImage(ctx, image)
	if err != nil {
		return ctrCollectorResponse{}, err
	}
	if imageInfo.OS != "linux" || normalizeCtrArchitecture(imageInfo.Architecture) != normalizeCtrArchitecture(info.Architecture) {
		return ctrCollectorResponse{}, fmt.Errorf("collector image platform does not match the Docker daemon")
	}
	architecture := normalizeCtrArchitecture(info.Architecture)
	if architecture != "amd64" && architecture != "arm64" {
		return ctrCollectorResponse{}, fmt.Errorf("collector does not support Docker daemon architecture %q", info.Architecture)
	}
	if imageInfo.Config.Labels[ctrCollectorProtocolLabel] != "1" {
		return ctrCollectorResponse{}, fmt.Errorf("collector image protocol label is not 1")
	}
	logPath := eligibleCtrLogPath(info.DockerRootDir, inspect)
	request.LogAvailable = logPath != ""
	requestBytes, err := encodeCtrCollectorRequest(request)
	if err != nil {
		return ctrCollectorResponse{}, err
	}
	name := "edc-collector-" + request.RequestID
	owner := request.RequestID
	config, err := json.Marshal(ctrCollectorCreateConfigValue(imageInfo.ID, name, owner, logPath))
	if err != nil {
		return ctrCollectorResponse{}, err
	}
	createPath := "/containers/create?name=" + url.QueryEscape(name)
	created, _, createErr := e.collectorRequest(ctx, http.MethodPost, createPath, config, ctrJSONLimit)
	var containerID string
	if createErr != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		containerID, err = e.collectorOwnedID(cleanupCtx, name, owner)
		if err != nil {
			cancelCleanup()
			return ctrCollectorResponse{}, &ctrCollectorCleanupError{Name: name, ID: "unknown", Cause: errors.Join(createErr, err)}
		}
		deleteErr := e.collectorDeleteWithContext(cleanupCtx, containerID)
		cancelCleanup()
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("create failed: %w", createErr), name, containerID, deleteErr)
	}
	var createdResponse ctrCollectorCreated
	if err = json.Unmarshal(created, &createdResponse); err != nil || !validCtrContainerID(createdResponse.ID) {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		containerID, err = e.collectorOwnedID(cleanupCtx, name, owner)
		if err != nil {
			cancelCleanup()
			return ctrCollectorResponse{}, &ctrCollectorCleanupError{Name: name, ID: "unknown", Cause: errors.Join(fmt.Errorf("invalid helper create response"), err)}
		}
		deleteErr := e.collectorDeleteWithContext(cleanupCtx, containerID)
		cancelCleanup()
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("invalid helper create response"), name, containerID, deleteErr)
	}
	containerID = createdResponse.ID
	attach, reader, stopAttach, err := e.collectorAttach(ctx, api, containerID)
	if err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(err, name, containerID, e.collectorDelete(containerID))
	}
	defer stopAttach()
	remainingMS := int64(ctxRemaining(ctx) / time.Millisecond)
	if remainingMS < 1 {
		return ctrCollectorResponse{}, e.collectorCleanupError(context.DeadlineExceeded, name, containerID, e.collectorDelete(containerID))
	}
	request.DeadlineMS = min(remainingMS, int64((5*time.Second)/time.Millisecond))
	requestBytes, err = encodeCtrCollectorRequest(request)
	if err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(err, name, containerID, e.collectorDelete(containerID))
	}
	startPath := "/containers/" + ctrSegment(containerID) + "/start"
	if _, _, err = e.collectorRequest(ctx, http.MethodPost, startPath, nil, 4096); err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("start collector helper: %w", err), name, containerID, e.collectorDelete(containerID))
	}
	if _, err = attach.Write(requestBytes); err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("write collector request: %w", err), name, containerID, e.collectorDelete(containerID))
	}
	if closer, ok := attach.(interface{ CloseWrite() error }); ok {
		if err = closer.CloseWrite(); err != nil {
			return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("close collector input: %w", err), name, containerID, e.collectorDelete(containerID))
		}
	}
	stdout, err := readCtrCollectorAttach(reader)
	if err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(err, name, containerID, e.collectorDelete(containerID))
	}
	wait, _, err := e.collectorRequest(ctx, http.MethodPost, "/containers/"+ctrSegment(containerID)+"/wait?condition=not-running", nil, 4096)
	if err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("wait for collector helper: %w", err), name, containerID, e.collectorDelete(containerID))
	}
	var waitResult struct {
		StatusCode *int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err = json.Unmarshal(wait, &waitResult); err != nil || waitResult.StatusCode == nil || *waitResult.StatusCode != 0 || waitResult.Error != nil {
		message := ""
		if waitResult.Error != nil {
			message = waitResult.Error.Message
		}
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("helper exit status %v: %s", waitResult.StatusCode, message), name, containerID, e.collectorDelete(containerID))
	}
	response, err := decodeCtrCollectorResponse(stdout, request)
	if err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(err, name, containerID, e.collectorDelete(containerID))
	}
	if err = response.validate(); err != nil {
		return ctrCollectorResponse{}, e.collectorCleanupError(err, name, containerID, e.collectorDelete(containerID))
	}
	latest, err := e.inspect(ctx, inspect.ID, false)
	if err != nil || latest.ID != inspect.ID || latest.PID != inspect.PID || !latest.StartedAt.Equal(inspect.StartedAt) {
		return ctrCollectorResponse{}, e.collectorCleanupError(fmt.Errorf("target container identity changed during collector run"), name, containerID, e.collectorDelete(containerID))
	}
	if err = e.collectorDelete(containerID); err != nil {
		return ctrCollectorResponse{}, fmt.Errorf("collector helper %s (%s) cleanup failed: %w", name, containerID, err)
	}
	return response, nil
}

func ctrCollectorAPIVersionSupported(version string) bool {
	return !versionLess(version, ctrMinAPIVersion) && !versionLess(ctrMaxAPIVersion, version)
}

func normalizeCtrArchitecture(value string) string {
	switch value {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return value
	}
}

func validateCtrCollectorImageReference(value string) error {
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "-") {
		return fmt.Errorf("invalid image reference")
	}
	for _, char := range value {
		if char <= 0x20 || char == 0x7f || char == '\\' || char == '?' || char == '#' {
			return fmt.Errorf("invalid image reference")
		}
	}
	return nil
}

func (e *ctrEngine) collectorInfo(ctx context.Context) (ctrCollectorDaemonInfo, error) {
	data, err := e.get(ctx, "/info", ctrJSONLimit)
	if err != nil {
		return ctrCollectorDaemonInfo{}, err
	}
	var info ctrCollectorDaemonInfo
	if err = json.Unmarshal(data, &info); err != nil || info.OSType == "" || info.Architecture == "" || !filepath.IsAbs(info.DockerRootDir) {
		return ctrCollectorDaemonInfo{}, fmt.Errorf("invalid Docker Engine info response")
	}
	return info, nil
}

func (e *ctrEngine) collectorImage(ctx context.Context, reference string) (ctrCollectorImageInfo, error) {
	data, err := e.get(ctx, "/images/"+ctrSegment(reference)+"/json", ctrJSONLimit)
	if err != nil {
		return ctrCollectorImageInfo{}, fmt.Errorf("local collector image lookup: %w", err)
	}
	var image ctrCollectorImageInfo
	if err = json.Unmarshal(data, &image); err != nil || !strings.HasPrefix(image.ID, "sha256:") {
		return ctrCollectorImageInfo{}, fmt.Errorf("invalid collector image response")
	}
	return image, nil
}

func eligibleCtrLogPath(root string, inspect ctrInspect) string {
	if inspect.Disk.LogDriver != "json-file" && inspect.Disk.LogDriver != "local" {
		return ""
	}
	path := inspect.Disk.LogPath
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ""
	}
	base := filepath.Join(root, "containers", inspect.ID)
	if !strings.HasPrefix(path, base+string(filepath.Separator)) {
		return ""
	}
	return path
}

func ctrCollectorCreateConfigValue(image, name, owner, logPath string) ctrCollectorContainerConfig {
	mounts := []ctrCollectorMount{{Type: "bind", Source: "/sys/fs/cgroup", Target: "/host/cgroup", ReadOnly: true}}
	if logPath != "" {
		mounts = append(mounts, ctrCollectorMount{Type: "bind", Source: logPath, Target: "/input/log", ReadOnly: true})
	}
	return ctrCollectorContainerConfig{
		Image: image, User: "0:0", Entrypoint: []string{"/edc-collector"}, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		OpenStdin: true, StdinOnce: true, NetworkDisabled: true, Labels: map[string]string{"org.x-mesh.edc.collector.owner": owner},
		HostConfig: ctrCollectorCreateHostConfig{
			NetworkMode: "none", ReadonlyRootfs: true, PidMode: "host", CgroupnsMode: "host", CapDrop: []string{"ALL"},
			CapAdd: []string{"SYS_PTRACE", "DAC_READ_SEARCH"}, SecurityOpt: []string{"no-new-privileges:true"},
			RestartPolicy: ctrCollectorRestartPolicy{Name: "no"}, LogConfig: ctrCollectorLogConfig{Type: "none", Config: map[string]string{}},
			Memory: 128 << 20, PidsLimit: 64, NanoCpus: 1_000_000_000, Mounts: mounts,
		},
	}
}

func (e *ctrEngine) collectorOwnedID(ctx context.Context, name, owner string) (string, error) {
	data, err := e.get(ctx, "/containers/"+ctrSegment(name)+"/json", ctrJSONLimit)
	if err != nil {
		return "", err
	}
	var info ctrCollectorContainerInfo
	if err = json.Unmarshal(data, &info); err != nil || !validCtrContainerID(info.ID) || info.Config.Labels["org.x-mesh.edc.collector.owner"] != owner {
		return "", fmt.Errorf("helper name does not identify this request")
	}
	return info.ID, nil
}

func (e *ctrEngine) collectorDelete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return e.collectorDeleteWithContext(ctx, id)
}

func (e *ctrEngine) collectorDeleteWithContext(ctx context.Context, id string) error {
	_, _, err := e.collectorRequest(ctx, http.MethodDelete, "/containers/"+ctrSegment(id)+"?force=1&v=0", nil, 4096)
	if ctrHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (e *ctrEngine) collectorCleanupError(cause error, name, id string, cleanupErr error) error {
	if cleanupErr == nil {
		return cause
	}
	return &ctrCollectorCleanupError{Name: name, ID: id, Cause: errors.Join(cause, cleanupErr)}
}

func (e *ctrEngine) collectorAttach(ctx context.Context, api, id string) (net.Conn, *bufio.Reader, func(), error) {
	dialCtx, cancel := context.WithTimeout(ctx, ctrDialTimeout)
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", e.socket)
	cancel()
	if err != nil {
		return nil, nil, nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	path := "/v" + api + "/containers/" + ctrSegment(id) + "/attach?logs=0&stream=1&stdin=1&stdout=1&stderr=1"
	if _, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", path); err != nil {
		conn.Close()
		stopCancel()
		return nil, nil, nil, err
	}
	reader := bufio.NewReaderSize(conn, 64<<10)
	req := &http.Request{Method: http.MethodPost}
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		stopCancel()
		return nil, nil, nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		conn.Close()
		stopCancel()
		return nil, nil, nil, &ctrHTTPError{StatusCode: response.StatusCode, Message: infoSingleLine(strings.TrimSpace(string(body)))}
	}
	return conn, reader, func() { stopCancel(); _ = conn.Close() }, nil
}

func readCtrCollectorAttach(reader *bufio.Reader) ([]byte, error) {
	var stdout bytes.Buffer
	var stderrSize int64
	for {
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if errors.Is(err, io.EOF) {
			return stdout.Bytes(), nil
		}
		if err != nil {
			return nil, fmt.Errorf("truncated collector attach frame: %w", err)
		}
		if header[0] != 1 && header[0] != 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return nil, fmt.Errorf("invalid collector attach frame")
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > ctrCollectorOutputLimit {
			return nil, fmt.Errorf("collector attach frame exceeds limit")
		}
		remaining := int64(size)
		if header[0] == 1 && int64(stdout.Len())+remaining > ctrCollectorOutputLimit {
			return nil, fmt.Errorf("collector stdout exceeds limit")
		}
		if header[0] == 2 && stderrSize+remaining > ctrCollectorStderrLimit {
			return nil, fmt.Errorf("collector stderr exceeds limit")
		}
		destination := io.Discard
		if header[0] == 1 {
			destination = &stdout
		} else {
			stderrSize += remaining
		}
		if _, err = io.CopyN(destination, reader, remaining); err != nil {
			return nil, fmt.Errorf("truncated collector attach payload: %w", err)
		}
	}
}
