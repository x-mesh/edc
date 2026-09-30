//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

const (
	// socketTargetRefresh는 socket 파일을 다시 stat하는 간격이다. 서비스가 재시작하면 socket 파일을 새로 만들어 inode가 바뀐다.
	socketTargetRefresh = time.Second
	// socketTargetMax는 socket_events_bpf.c의 socket_targets map 크기다.
	socketTargetMax = 64
	socketPathSize  = 108
)

// socketTraceTarget은 trace socket이 볼 socket 파일이다. path는 사용자가 준 경로의 절대 경로다.
type socketTraceTarget struct {
	path string
	key  socketEventsSocketTarget
}

// resolveSocketTarget은 경로가 stream unix socket 파일인지 확인한다. 다른 종류의 socket은 이 trace의 hook을 지나지 않아
// 아무 event 없이 끝나므로 먼저 알린다.
func resolveSocketTarget(path string) (socketTraceTarget, error) {
	if path == "" {
		return socketTraceTarget{}, errors.New(T("cli.trace.socket_path"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return socketTraceTarget{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return socketTraceTarget{}, errors.New(T("cli.trace.socket_missing", absolute, err))
	}
	key, ok := socketTargetKey(info)
	if info.Mode()&os.ModeSocket == 0 || !ok {
		return socketTraceTarget{}, errors.New(T("cli.trace.socket_not_socket", absolute))
	}
	paths := []string{absolute}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil && resolved != absolute {
		paths = append(paths, resolved)
	}
	if data, err := os.ReadFile("/proc/net/unix"); err == nil {
		if kind := unixSocketKind(data, paths); kind != "" && kind != "stream" {
			return socketTraceTarget{}, errors.New(T("cli.trace.socket_not_stream", absolute, kind))
		}
	}
	return socketTraceTarget{path: absolute, key: key}, nil
}

// validateSocketTarget은 trace를 시작하기 전에 경로를 확인한다. 잘못된 경로는 사용법 오류다.
func validateSocketTarget(path string) error {
	_, err := resolveSocketTarget(path)
	return err
}

func socketTargetKey(info os.FileInfo) (socketEventsSocketTarget, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketEventsSocketTarget{}, false
	}
	return socketEventsSocketTarget{Ino: stat.Ino, Dev: socketKernelDev(stat.Dev)}, true
}

// socketKernelDev는 stat의 장치 번호를 kernel의 s_dev 형식(major << 20 | minor)으로 바꾼다. 두 형식은 major가 0인
// tmpfs 같은 파일 시스템에서만 같다.
func socketKernelDev(dev uint64) uint32 {
	return unix.Major(dev)<<20 | unix.Minor(dev)
}

// unixSocketKind는 /proc/net/unix에서 paths에 묶인 socket의 종류다. stream이 하나라도 있으면 stream이고, 찾지 못하면
// 빈 문자열이다. 상대 경로로 bind한 socket은 그 상대 경로로 나오므로 찾지 못할 수 있다.
func unixSocketKind(data []byte, paths []string) string {
	kinds := map[string]string{"0001": "stream", "0002": "dgram", "0005": "seqpacket"}
	found := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || !slices.Contains(paths, fields[7]) {
			continue
		}
		kind := emptyAs(kinds[fields[4]], fields[4])
		if kind == "stream" {
			return kind
		}
		found = kind
	}
	return found
}

// socketTracePrerequisites는 eBPF 조건과 unix socket 함수를 확인한다. 이 함수들은 unix가 module로 build된 kernel에서는
// vmlinux BTF에 없다.
func socketTracePrerequisites() error {
	if err := captureBPFPrerequisites(); err != nil {
		return err
	}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.btf_missing"), err)
	}
	for _, name := range []string{"unix_stream_sendmsg", "unix_stream_recvmsg", "unix_stream_connect", "unix_release"} {
		var function *btf.Func
		if err := kernel.TypeByName(name, &function); err != nil {
			return errors.New(T("cli.trace.socket_hook_missing", name))
		}
	}
	return nil
}

// socketPayloadKernel은 사용자 버퍼를 읽을 iov_iter 필드를 확인한다. BPF는 없는 필드를 건너뛰므로, 확인하지 않으면 오래된
// kernel에서 payload가 모두 비어 있다.
func socketPayloadKernel() error {
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("read kernel BTF: %w", err)
	}
	var iter *btf.Struct
	if err := spec.TypeByName("iov_iter", &iter); err != nil {
		return fmt.Errorf("find iov_iter in kernel BTF: %w", err)
	}
	for _, field := range []string{"iter_type", "ubuf", "__iov"} {
		if !btfHasMember(iter, field) {
			return errors.New(T("cli.trace.socket_payload_kernel", field))
		}
	}
	return nil
}

func socketPayloadSupported() bool {
	return socketPayloadKernel() == nil
}

// socketPeerNames는 상대 pid의 process 이름이다. BPF는 상대의 pid만 알므로 /proc에서 읽는다.
type socketPeerNames map[uint32]string

func (names socketPeerNames) name(pid uint32) string {
	if pid == 0 {
		return ""
	}
	if name, ok := names[pid]; ok {
		return name
	}
	data, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/comm")
	name := strings.TrimSpace(string(data))
	if err != nil {
		name = ""
	}
	names[pid] = name
	return name
}

func socketErrorName(result int64) string {
	if name := unix.ErrnoName(syscall.Errno(-result)); name != "" {
		return name
	}
	return "errno " + strconv.FormatInt(-result, 10)
}

// collectSocketEvents는 path의 unix socket에서 일어나는 연결, 송수신, 닫기를 모은다. payloadLimit는 호출마다 받는
// payload byte 수이고 0이면 byte 수만 받는다.
func collectSocketEvents(path string, payloadLimit int, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	target, err := resolveSocketTarget(path)
	if err != nil {
		return captureSummary{}, err
	}
	if payloadLimit > 0 {
		if err := socketPayloadKernel(); err != nil {
			return captureSummary{}, err
		}
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return captureSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadSocketEvents()
	if err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	var variables socketEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	// 끝의 NUL을 남긴다. BPF는 NUL까지 비교한다. 이보다 긴 경로는 kernel의 sun_path에도 들어가지 않는다.
	var targetPath [socketPathSize]byte
	copy(targetPath[:socketPathSize-1], target.path)
	if err := errors.Join(variables.PayloadLimit.Set(uint32(payloadLimit)), variables.TargetPath.Set(targetPath)); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	objects := socketEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()

	targets := map[socketEventsSocketTarget]bool{}
	addTarget := func(key socketEventsSocketTarget) error {
		if targets[key] || len(targets) >= socketTargetMax {
			return nil
		}
		if err := objects.SocketTargets.Put(key, uint8(1)); err != nil {
			return fmt.Errorf("add socket target: %w", err)
		}
		targets[key] = true
		return nil
	}
	if err := addTarget(target.key); err != nil {
		return captureSummary{}, err
	}

	// 시작 hook을 끝 hook보다 먼저 붙인다. 끝 hook만 붙은 동안 끝난 호출은 기록이 없어 건너뛴다.
	hooks := []struct {
		name string
		prog *ebpf.Program
	}{
		{"fentry/unix_stream_sendmsg", objects.UnixStreamSendmsgEntry},
		{"fentry/unix_stream_recvmsg", objects.UnixStreamRecvmsgEntry},
		{"fexit/unix_stream_sendmsg", objects.UnixStreamSendmsgExit},
		{"fexit/unix_stream_recvmsg", objects.UnixStreamRecvmsgExit},
		{"fexit/unix_stream_connect", objects.UnixStreamConnectExit},
		{"fentry/unix_release", objects.UnixReleaseEntry},
	}
	links := make([]link.Link, 0, len(hooks))
	defer func() { closeCaptureLinks(links) }()
	for _, hook := range hooks {
		attached, err := link.AttachTracing(link.TracingOptions{Program: hook.prog})
		if err != nil {
			return captureSummary{}, fmt.Errorf("attach %s (needs BPF trampolines: x86_64 5.5+, arm64 6.0+): %w", hook.name, err)
		}
		links = append(links, attached)
	}

	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return captureSummary{}, fmt.Errorf("open event ring: %w", err)
	}
	defer reader.Close()
	clockOffset, err := captureClockOffset()
	if err != nil {
		return captureSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	deadline := time.Now().Add(duration)
	readerDone := make(chan struct{})
	if stop != nil {
		go func() {
			select {
			case <-stop:
				_ = reader.Close()
			case <-readerDone:
			}
		}()
		defer close(readerDone)
	}

	calls := newSocketCalls(payloadLimit)
	peers := socketPeerNames{}
	var eventCount uint64
	emit := func(done []socketCall) error {
		for _, call := range done {
			errorName := ""
			if call.record.result < 0 {
				errorName = socketErrorName(call.record.result)
			}
			event := socketTraceEvent(call, target.path, clockOffset, errorName, peers.name(call.record.peerPID))
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return err
				}
			}
			eventCount++
		}
		return nil
	}
	// ring buffer reader는 버퍼가 빌 때만 deadline을 본다. 그래서 event가 없어도 socket 파일을 다시 확인하도록 깨운다.
	wake := func() {
		next := time.Now().Add(socketTargetRefresh)
		if duration > 0 && deadline.Before(next) {
			next = deadline
		}
		reader.SetDeadline(next)
	}
	// 첫 deadline이 refreshed보다 먼저 지나면 그 사이 Read가 곧바로 돌아와 빈 반복을 돈다.
	refreshed := time.Now()
	wake()
	finish := func() (captureSummary, error) {
		if err := emit(calls.flush()); err != nil {
			return captureSummary{}, err
		}
		var lost uint64
		if err := objects.LostEvents.Lookup(uint32(0), &lost); err != nil {
			return captureSummary{}, fmt.Errorf("read lost event count: %w", err)
		}
		return captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, LostEvents: lost}, nil
	}
	for {
		now := time.Now()
		if duration > 0 && !now.Before(deadline) {
			return finish()
		}
		if now.Sub(refreshed) >= socketTargetRefresh {
			refreshed = now
			// 파일이 사라졌으면 이전 inode를 그대로 둔다. 이미 맺은 연결은 그 inode로 계속 보인다.
			if info, err := os.Stat(target.path); err == nil {
				if key, ok := socketTargetKey(info); ok {
					if err := addTarget(key); err != nil {
						return captureSummary{}, err
					}
				}
			}
			if err := emit(calls.expire(now)); err != nil {
				return captureSummary{}, err
			}
			wake()
		}
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if duration == 0 || time.Now().Before(deadline) {
				continue
			}
			return finish()
		}
		if errors.Is(err, os.ErrClosed) && traceStopRequested(stop) {
			return finish()
		}
		if err != nil {
			return captureSummary{}, err
		}
		parsed, ok := parseSocketRecord(record.RawSample)
		if !ok {
			continue
		}
		if err := emit(calls.add(parsed, time.Now())); err != nil {
			return captureSummary{}, err
		}
	}
}
