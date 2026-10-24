package edc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"
)

const (
	ctrCollectorProtocolVersion = 1
	ctrCollectorProtocolLabel   = "org.x-mesh.edc.collector.protocol"
	ctrCollectorInputLimit      = 64 << 10
	ctrCollectorOutputLimit     = 1 << 20
	ctrCollectorStderrLimit     = 64 << 10
	ctrCollectorProcessLimit    = 4096
	ctrCollectorFDLimit         = 65536
	ctrCollectorSocketLimit     = 65536
)

var errCtrCollectorProtocol = errors.New("invalid collector protocol response")

type ctrCollectorRequest struct {
	RequestID       string `json:"request_id"`
	TargetID        string `json:"target_id"`
	TargetPID       int    `json:"target_pid"`
	ProtocolVersion int    `json:"protocol_version"`
	LogAvailable    bool   `json:"log_available"`
	DeadlineMS      int64  `json:"deadline_ms"`
}

type ctrCollectorMetricStatus struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type ctrCollectorFD struct {
	PID       int     `json:"pid"`
	Used      *uint64 `json:"used,omitempty"`
	Soft      *uint64 `json:"soft,omitempty"`
	Unlimited bool    `json:"unlimited,omitempty"`
	Status    string  `json:"status"`
	Reason    string  `json:"reason,omitempty"`
}

type ctrCollectorListen struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Inode   uint64 `json:"inode"`
}

type ctrCollectorResponse struct {
	RequestID       string                   `json:"request_id"`
	TargetID        string                   `json:"target_id"`
	TargetPID       int                      `json:"target_pid"`
	ProtocolVersion int                      `json:"protocol_version"`
	FDStatus        ctrCollectorMetricStatus `json:"fd_status"`
	FDs             []ctrCollectorFD         `json:"fds"`
	ListenStatus    ctrCollectorMetricStatus `json:"listen_status"`
	Listeners       []ctrCollectorListen     `json:"listeners"`
	OOMStatus       ctrCollectorMetricStatus `json:"oom_status"`
	OOM             *uint64                  `json:"oom,omitempty"`
	OOMKill         *uint64                  `json:"oom_kill,omitempty"`
	LogStatus       ctrCollectorMetricStatus `json:"log_status"`
	LogBytes        *uint64                  `json:"log_bytes,omitempty"`
}

func newCtrCollectorRequest(inspect ctrInspect, logAvailable bool, timeout time.Duration) (ctrCollectorRequest, error) {
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return ctrCollectorRequest{}, err
	}
	return ctrCollectorRequest{
		RequestID: hex.EncodeToString(idBytes[:]), TargetID: inspect.ID, TargetPID: inspect.PID,
		ProtocolVersion: ctrCollectorProtocolVersion, LogAvailable: logAvailable,
		DeadlineMS: min(int64(timeout/time.Millisecond), int64((5*time.Second)/time.Millisecond)),
	}, nil
}

func encodeCtrCollectorRequest(request ctrCollectorRequest) ([]byte, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(data) > ctrCollectorInputLimit {
		return nil, fmt.Errorf("collector request exceeds %d bytes", ctrCollectorInputLimit)
	}
	return append(data, '\n'), nil
}

func decodeCtrCollectorResponse(data []byte, request ctrCollectorRequest) (ctrCollectorResponse, error) {
	if len(data) == 0 || len(data) > ctrCollectorOutputLimit {
		return ctrCollectorResponse{}, fmt.Errorf("collector stdout size is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var response ctrCollectorResponse
	if err := decoder.Decode(&response); err != nil {
		return ctrCollectorResponse{}, fmt.Errorf("decode collector response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ctrCollectorResponse{}, fmt.Errorf("%w: trailing JSON or data", errCtrCollectorProtocol)
	}
	if response.ProtocolVersion != request.ProtocolVersion || response.RequestID != request.RequestID || response.TargetID != request.TargetID || response.TargetPID != request.TargetPID {
		return ctrCollectorResponse{}, errCtrCollectorProtocol
	}
	return response, nil
}

func collectorUnavailable(reason string) ctrCollectorMetricStatus {
	if strings.TrimSpace(reason) == "" {
		reason = errCtrUnavailable.Error()
	}
	return ctrCollectorMetricStatus{Status: "unavailable", Reason: reason}
}

func (response ctrCollectorResponse) validate() error {
	for _, status := range []ctrCollectorMetricStatus{response.FDStatus, response.ListenStatus, response.OOMStatus, response.LogStatus} {
		if status.Status != "available" && status.Status != "partial" && status.Status != "unsupported" && status.Status != "unavailable" && status.Status != "error" {
			return fmt.Errorf("%w: unknown metric status %q", errCtrCollectorProtocol, status.Status)
		}
		if status.Status != "available" && status.Reason == "" {
			return fmt.Errorf("%w: missing metric status reason", errCtrCollectorProtocol)
		}
	}
	if len(response.FDs) > ctrCollectorProcessLimit || len(response.Listeners) > ctrCollectorSocketLimit {
		return fmt.Errorf("%w: collector item limit exceeded", errCtrCollectorProtocol)
	}
	seenPIDs := map[int]struct{}{}
	for _, process := range response.FDs {
		if process.PID <= 0 || process.PID > 1<<30 {
			return fmt.Errorf("%w: invalid process ID", errCtrCollectorProtocol)
		}
		if _, exists := seenPIDs[process.PID]; exists {
			return fmt.Errorf("%w: duplicate process ID", errCtrCollectorProtocol)
		}
		seenPIDs[process.PID] = struct{}{}
		if process.Used != nil && *process.Used > ctrCollectorFDLimit {
			return fmt.Errorf("%w: process FD count exceeds limit", errCtrCollectorProtocol)
		}
		if process.Status != "available" && process.Status != "partial" && process.Status != "permission_denied" && process.Status != "unsupported" && process.Status != "error" && process.Status != "unavailable" {
			return fmt.Errorf("%w: invalid process FD status", errCtrCollectorProtocol)
		}
		if process.Status == "available" && (process.Used == nil || !process.Unlimited && process.Soft == nil) || process.Status != "available" && process.Reason == "" {
			return fmt.Errorf("%w: incomplete process FD row", errCtrCollectorProtocol)
		}
	}
	seenSockets := map[uint64]struct{}{}
	for _, listener := range response.Listeners {
		if listener.Proto != "tcp" && listener.Proto != "udp" || listener.Port < 1 || listener.Port > 65535 || listener.Inode == 0 {
			return fmt.Errorf("%w: invalid listener", errCtrCollectorProtocol)
		}
		if _, err := netip.ParseAddr(listener.Address); err != nil {
			return fmt.Errorf("%w: invalid listener address", errCtrCollectorProtocol)
		}
		if _, exists := seenSockets[listener.Inode]; exists {
			return fmt.Errorf("%w: duplicate listener inode", errCtrCollectorProtocol)
		}
		seenSockets[listener.Inode] = struct{}{}
	}
	if response.OOMStatus.Status == "available" && (response.OOM == nil || response.OOMKill == nil) {
		return fmt.Errorf("%w: available OOM status lacks counters", errCtrCollectorProtocol)
	}
	if response.LogStatus.Status == "available" && response.LogBytes == nil {
		return fmt.Errorf("%w: available log status lacks a size", errCtrCollectorProtocol)
	}
	return nil
}

func marshalCtrCollectorResponse(response ctrCollectorResponse) ([]byte, error) {
	limit := ctrCollectorOutputLimit - 9
	data, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	for len(data) > limit {
		if len(response.Listeners) > 0 {
			response.Listeners = response.Listeners[:len(response.Listeners)/2]
			response.ListenStatus = ctrCollectorMetricStatus{Status: "partial", Reason: "collector output size limit reached"}
		} else if len(response.FDs) > 0 {
			response.FDs = response.FDs[:len(response.FDs)/2]
			response.FDStatus = ctrCollectorMetricStatus{Status: "partial", Reason: "collector output size limit reached"}
		} else {
			return nil, fmt.Errorf("collector response metadata exceeds output limit")
		}
		data, err = json.Marshal(response)
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}

func ctrCollectorContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, min(ctrRequestLimit, 5*time.Second))
}

func ctxRemaining(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 5 * time.Second
	}
	return min(5*time.Second, time.Until(deadline))
}

func validCtrContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
