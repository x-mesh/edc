//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	capNetAdmin = 12
	capPerfmon  = 38
	capBPF      = 39
)

func runCaptureEvents(options captureEventsOptions) int {
	if options.duration <= 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.capture.duration_range"))
		return 2
	}
	output, err := captureEventsOutputPath(options.output)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !options.yes {
		fmt.Fprintf(os.Stdout, T("cli.capture.events_plan"), output, options.duration)
		if !confirm(os.Stdin, os.Stdout, T("cli.confirm"), false) {
			fmt.Fprintln(os.Stderr, T("cli.capture.cancelled"))
			return 4
		}
	}
	if err := captureEventsPrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if err := captureEventsRun(options.duration, output); err != nil {
		fmt.Fprintln(os.Stderr, T("cli.capture.events_failed", err))
		return 1
	}
	fmt.Fprintln(os.Stdout, T("cli.capture.events_done", output))
	return 0
}

func captureEventsOutputPath(requested string) (string, error) {
	if requested != "" {
		absolute, err := filepath.Abs(requested)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err == nil {
			return "", errors.New(T("cli.capture.output_exists", absolute))
		} else if !os.IsNotExist(err) {
			return "", err
		}
		return absolute, nil
	}
	file, err := os.CreateTemp("", "edc-events-*.jsonl")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func captureEventsPrerequisites() error {
	if err := captureBPFPrerequisites(); err != nil {
		return err
	}
	return captureTraceHooksAvailable()
}

// captureBPFPrerequisites는 eBPF program을 불러오는 데 필요한 kernel BTF와 capability를 확인한다.
func captureBPFPrerequisites() error {
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return errors.New(T("cli.capture.btf_missing"))
	}
	capabilities, err := effectiveCapabilities()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.capability_check_failed"), err)
	}
	return captureCapabilityError(capabilities, currentRootCommand())
}

// currentRootCommand는 지금 명령을 sudo로 다시 실행하는 줄이다.
func currentRootCommand() string {
	executable, err := os.Executable()
	if err != nil {
		// 실행 경로를 모르면 명령줄의 이름을 쓴다. 안내 문구에만 들어간다.
		executable = os.Args[0]
	}
	return rootCommand(executable, os.Args[1:])
}

type traceCapability struct {
	number int
	name   string
}

// captureCapabilities는 eBPF program을 불러오고 붙이는 데 필요한 capability다.
var captureCapabilities = []traceCapability{{capBPF, "CAP_BPF"}, {capPerfmon, "CAP_PERFMON"}, {capNetAdmin, "CAP_NET_ADMIN"}}

// bpfTraceCapabilities는 network hook 없이 tp_btf만 붙이는 trace io와 trace sched에 필요한 capability다.
var bpfTraceCapabilities = []traceCapability{{capBPF, "CAP_BPF"}, {capPerfmon, "CAP_PERFMON"}}

func missingCapabilities(required []traceCapability, capabilities map[int]bool) string {
	var missing []string
	for _, capability := range required {
		if !capabilities[capability.number] {
			missing = append(missing, capability.name)
		}
	}
	return strings.Join(missing, ", ")
}

// captureCapabilityError는 빠진 capability를 모두 이름으로 알리고, 지금 명령을 root로 다시 실행하는 줄을 붙인다.
// root인데도 빠졌다면 컨테이너처럼 capability를 제한한 환경이라 sudo는 소용없으므로 capability를 더하라고 안내한다.
func captureCapabilityError(capabilities map[int]bool, command string) error {
	missing := missingCapabilities(captureCapabilities, capabilities)
	if missing == "" {
		return nil
	}
	if captureGeteuid() == 0 {
		return errors.New(T("cli.capture.capability_missing_root", missing))
	}
	return errors.New(T("cli.capture.capability_missing", missing, command))
}

// traceCapabilityError는 captureCapabilityError와 같은 안내를 network 밖의 trace에 쓴다. subject는 "trace io"처럼
// 명령 이름이다.
func traceCapabilityError(required []traceCapability, capabilities map[int]bool, subject, command string) error {
	missing := missingCapabilities(required, capabilities)
	if missing == "" {
		return nil
	}
	if captureGeteuid() == 0 {
		return errors.New(T("cli.trace.capability_missing_root", missing, subject))
	}
	return errors.New(T("cli.trace.capability_missing", missing, subject, command))
}

// traceBPFPrerequisites는 kernel이 trace를 지원하는지 먼저 보고, 그다음 권한을 본다. 권한이 없는 사용자도 kernel BTF는
// 읽을 수 있으므로, 지원되지 않는 host와 sudo로 실행하면 되는 host를 서로 다른 문구로 알린다.
func traceBPFPrerequisites(subject string, supported func(*btf.Spec) error) error {
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return kernelBTFError(subject, err)
	}
	if err := supported(kernel); err != nil {
		return errors.New(T("cli.trace.unsupported", subject, err.Error()))
	}
	return bpfTraceCapabilityCheck(subject)
}

// kernelBTFError는 BTF가 없는 kernel만 지원하지 않는 host로 알린다. 대체 BTF 파일을 읽을 권한이 없거나 파일이 깨진 경우는
// 지원되는 kernel에서도 생기므로 원래 오류를 그대로 남긴다.
func kernelBTFError(subject string, err error) error {
	if errors.Is(err, ebpf.ErrNotSupported) {
		return errors.New(T("cli.trace.unsupported", subject, T("cli.capture.btf_missing")))
	}
	return fmt.Errorf("read kernel BTF: %w", err)
}

// bpfTraceCapabilityCheck는 network hook 없이 tracing program만 붙이는 trace가 쓸 capability를 확인한다.
func bpfTraceCapabilityCheck(subject string) error {
	capabilities, err := effectiveCapabilities()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.capability_check_failed"), err)
	}
	return traceCapabilityError(bpfTraceCapabilities, capabilities, subject, currentRootCommand())
}

// traceTracepointsAvailable은 tp_btf가 붙을 btf_trace_<tracepoint> 형식이 kernel BTF에 있는지 본다. tracefs는
// root만 읽을 수 있고 container에는 없을 수 있어서 보지 않는다.
func traceTracepointsAvailable(kernel *btf.Spec, names []string) error {
	for _, name := range names {
		var typedef *btf.Typedef
		if err := kernel.TypeByName("btf_trace_"+name, &typedef); err != nil {
			return fmt.Errorf("kernel BTF has no tracepoint %s", name)
		}
	}
	return nil
}

// rootCommand는 지금 명령을 sudo로 다시 실행하는 줄이다. Ubuntu의 sudo는 secure_path만 PATH로 써서 ~/.local/bin의
// edc를 찾지 못하므로 실행 파일의 전체 경로를 쓴다.
func rootCommand(executable string, args []string) string {
	words := []string{"sudo", shellWord(executable)}
	for _, arg := range args {
		words = append(words, shellWord(arg))
	}
	return strings.Join(words, " ")
}

// shellWord는 셸에 그대로 붙여 넣을 수 있게, 특수 문자가 있을 때만 작은따옴표로 감싼다.
func shellWord(word string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/-_.=:,@+%", r)
	}
	if word != "" && strings.IndexFunc(word, func(r rune) bool { return !plain(r) }) < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// captureTraceHooksAvailable은 fentry와 fexit 대상이 kernel BTF에 있는지 본다. UDP 송신 함수는 static이라 kernel
// build에 따라 inline되어 사라질 수 있다. 확인하지 않으면 object load가 실패해 TCP trace까지 이유 없이 멈춘다.
func captureTraceHooksAvailable() error {
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.btf_missing"), err)
	}
	for _, hook := range []struct{ name, message string }{
		{"udp_send_skb", "cli.capture.udp_send_hook_missing"},
		{"udp_v6_send_skb", "cli.capture.udp_send_hook_missing"},
		{"inet_csk_accept", "cli.capture.tcp_accept_hook_missing"},
		{"tcp_create_openreq_child", "cli.capture.tcp_accept_hook_missing"},
	} {
		var function *btf.Func
		if err := kernel.TypeByName(hook.name, &function); err != nil {
			return errors.New(T(hook.message, hook.name))
		}
	}
	return nil
}

func inetCskAcceptArgumentCount(spec *btf.Spec) (int, error) {
	var function *btf.Func
	if err := spec.TypeByName("inet_csk_accept", &function); err != nil {
		return 0, fmt.Errorf("find inet_csk_accept in kernel BTF: %w", err)
	}
	prototype, ok := btf.UnderlyingType(function.Type).(*btf.FuncProto)
	if !ok || (len(prototype.Params) != 2 && len(prototype.Params) != 4) {
		return 0, fmt.Errorf("unsupported inet_csk_accept argument count: %d", len(prototype.Params))
	}
	return len(prototype.Params), nil
}

func effectiveCapabilities() (map[int]bool, error) {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "CapEff:" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			return nil, err
		}
		capabilities := map[int]bool{}
		for capability := 0; capability < 64; capability++ {
			capabilities[capability] = value&(uint64(1)<<capability) != 0
		}
		return capabilities, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("CapEff is missing")
}

var captureEventsCollect = collectCaptureEventsUntil

func captureEventsRun(duration time.Duration, output string) error {
	// Ctrl-C가 process를 바로 끝내면 그때까지 모은 event와 output file을 잃는다. trace처럼 수집만
	// 멈추고 모은 결과를 쓴다.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	events, summary, err := captureEventsCollect(duration, nil, ctx.Done())
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	return encoder.Encode(summary)
}

// closeCaptureLinks는 link를 동시에 닫는다. 떼어 낼 때마다 kernel 안에서 기다림이 있어, 순서대로 닫으면
// trace를 끝낼 때마다 1초 넘게 걸렸다. 동시에 닫으면 tracepoint link의 기다림은 겹친다. fentry와 fexit link는
// 동시에 닫아도 kernel이 하나씩 떼어 낸다.
func closeCaptureLinks(links []link.Link) {
	var wait sync.WaitGroup
	for _, current := range links {
		wait.Go(func() { _ = current.Close() })
	}
	wait.Wait()
}

// traceTLSPrograms는 traceTLSFunctions마다 진입과 반환 program이다. 진입은 인자를, 반환은 평문 길이를 본다. GnuTLS의
// 송수신 함수는 (session, buffer, size)를 받아 byte 수를 돌려주므로 SSL_write와 SSL_read의 program을 쓰고, gnutls_deinit은
// SSL_free처럼 그 session의 상태를 지운다.
func traceTLSPrograms(objects *captureEventsObjects) map[string][2]*ebpf.Program {
	return map[string][2]*ebpf.Program{
		"SSL_read":     {objects.SslReadEntry, objects.SslReadExit},
		"SSL_write":    {objects.SslWriteEntry, objects.SslWriteExit},
		"SSL_read_ex":  {objects.SslReadExEntry, objects.SslReadExExit},
		"SSL_write_ex": {objects.SslWriteExEntry, objects.SslWriteExExit},
		"SSL_free":     {objects.SslFreeEntry, nil},
		// gnutls_record_send와 _recv의 size는 size_t, 반환은 ssize_t다. BPF는 SSL_write처럼 하위 32 bit를 읽는데, 한 번의
		// 호출은 협상한 최대 record 크기(16KiB 이하)까지만 주고받으므로 반환값이 잘리지 않는다.
		"gnutls_record_send":        {objects.SslWriteEntry, objects.SslWriteExit},
		"gnutls_record_send2":       {objects.SslWriteEntry, objects.SslWriteExit},
		"gnutls_record_recv":        {objects.SslReadEntry, objects.SslReadExit},
		"gnutls_record_recv_seq":    {objects.SslReadEntry, objects.SslReadExit},
		"gnutls_deinit":             {objects.SslFreeEntry, nil},
		"wolfSSL_read":              {objects.SslReadEntry, objects.SslReadExit},
		"wolfSSL_write":             {objects.SslWriteEntry, objects.SslWriteExit},
		"wolfSSL_read_ex":           {objects.SslReadExEntry, objects.SslReadExExit},
		"wolfSSL_write_ex":          {objects.SslWriteExEntry, objects.SslWriteExExit},
		"wolfSSL_free":              {objects.SslFreeEntry, nil},
		"mbedtls_ssl_read":          {objects.MbedReadEntry, objects.SslReadExit},
		"mbedtls_ssl_write":         {objects.MbedWriteEntry, objects.SslWriteExit},
		"mbedtls_ssl_session_reset": {objects.SslFreeEntry, nil},
		"mbedtls_ssl_free":          {objects.SslFreeEntry, nil},
		"rustls_connection_read":    {objects.SslReadExEntry, objects.RustlsExit},
		"rustls_connection_write":   {objects.SslWriteExEntry, objects.RustlsExit},
		"rustls_connection_free":    {objects.SslFreeEntry, nil},
		"SSL_ImportFD":              {objects.NssImportEntry, objects.NssControlExit},
		"SSL_OptionSet":             {objects.NssOptionEntry, objects.NssControlExit},
		"SSL_OptionSetDefault":      {objects.NssDefaultEntry, objects.NssControlExit},
		"PR_Accept":                 {objects.NssAcceptEntry, objects.NssControlExit},
		"PR_Read":                   {objects.NssReadEntry, objects.NssIoExit},
		"PR_Recv":                   {objects.NssRecvEntry, objects.NssIoExit},
		"PR_Write":                  {objects.NssWriteEntry, objects.NssIoExit},
		"PR_Send":                   {objects.NssWriteEntry, objects.NssIoExit},
		"PR_Close":                  {objects.NssCloseEntry, nil},
	}
}

// traceTLSAttachGone은 탐색한 뒤 그 process가 끝나 경로가 사라져서 아무것도 붙이지 못한 경우다. 그 파일은 같은 파일을
// 적재한 다른 process에서 다시 고른다. 일부라도 붙었으면 다시 고르지 않아 같은 함수에 두 번 붙지 않는다.
func traceTLSAttachGone(attached []link.Link, err error) bool {
	return len(attached) == 0 && (err == nil || errors.Is(err, fs.ErrNotExist))
}

// attachTraceTLS는 --tls 대상 파일 하나의 TLS 함수에 uprobe를 붙인다. 고른 뒤 끝난 process의 container 파일은 열 수 없어
// 건너뛴다. 화면이 이미 열렸으므로 알리지 않는다. 붙인 link는 실패해도 돌려준다.
func attachTraceTLS(objects *captureEventsObjects, target traceTLSTarget) ([]link.Link, error) {
	if target.goReturns != nil {
		return attachTraceTLSGo(objects, target)
	}
	programs := traceTLSPrograms(objects)
	executable, err := link.OpenExecutable(target.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("--tls %s: %w", target.path, err)
	}
	var links []link.Link
	for _, symbol := range target.symbols {
		pair := programs[symbol]
		// 위치를 모르면 nil이라 cilium/ebpf가 심볼 표에서 찾는다.
		var options *link.UprobeOptions
		if offset, ok := target.offsets[symbol]; ok {
			options = &link.UprobeOptions{Address: offset}
		}
		if index := slices.Index(traceTLSNSSFunctions, symbol); index >= 0 {
			if options == nil {
				options = &link.UprobeOptions{}
			}
			options.Cookie = uint64(index + 1)
		}
		entry, err := executable.Uprobe(symbol, pair[0], options)
		if err != nil {
			return links, fmt.Errorf("--tls %s: attach %s: %w", target.path, symbol, err)
		}
		links = append(links, entry)
		if pair[1] == nil {
			continue
		}
		// amd64 kernel 6.11, 6.12.14 전의 6.12, 6.13.3 전의 6.13에서는 uretprobe가 seccomp filter 아래의 process를 끝낼
		// 수 있다. distro kernel은 수정을 따로 넣었을 수 있어 막지 않고, resolveTraceTLSTargets가 화면을 열기 전에 알린다.
		exit, err := executable.Uretprobe(symbol, pair[1], options)
		if err != nil {
			return links, fmt.Errorf("--tls %s: attach %s return: %w", target.path, symbol, err)
		}
		links = append(links, exit)
	}
	return links, nil
}

func attachTraceTLSGo(objects *captureEventsObjects, target traceTLSTarget) (links []link.Link, err error) {
	executable, err := link.OpenExecutable(target.path)
	if err != nil {
		return nil, fmt.Errorf("--tls %s: %w", target.path, err)
	}
	defer func() {
		if err != nil {
			for _, probe := range links {
				probe.Close()
			}
			links = nil
		}
	}()
	pairs := map[string][2]*ebpf.Program{
		traceTLSGoRead:  {objects.GoTlsReadEntry, objects.GoTlsReadExit},
		traceTLSGoWrite: {objects.GoTlsWriteEntry, objects.GoTlsWriteExit},
		traceTLSGoClose: {objects.GoTlsCloseEntry, nil},
	}
	attach := func(name string, program *ebpf.Program, offset uint64) error {
		probe, attachErr := executable.Uprobe("", program, &link.UprobeOptions{Address: offset})
		if attachErr != nil {
			return fmt.Errorf("--tls %s: attach %s at %#x: %w", target.path, name, offset, attachErr)
		}
		links = append(links, probe)
		return nil
	}
	for _, name := range []string{traceTLSGoRead, traceTLSGoWrite} {
		if len(target.goReturns[name]) == 0 {
			return links, fmt.Errorf("--tls %s: missing Go TLS RET offsets", target.path)
		}
		for _, offset := range target.goReturns[name] {
			if err = attach(name+" return", pairs[name][1], offset); err != nil {
				return links, err
			}
		}
	}
	for _, name := range []string{traceTLSGoClose, traceTLSGoRead, traceTLSGoWrite} {
		offset, exists := target.offsets[name]
		if !exists {
			return links, fmt.Errorf("--tls %s: missing Go TLS entry offset", target.path)
		}
		if err = attach(name, pairs[name][0], offset); err != nil {
			return links, err
		}
	}
	return links, nil
}

func collectCaptureEvents(duration time.Duration, onEvent func(captureEvent) error) ([]captureEvent, captureSummary, error) {
	return collectCaptureEventsUntil(duration, onEvent, nil)
}

// collectCaptureEventsUntil은 capture가 파일에 쓸 event를 모은다. trace는 event를 모으지 않고 요약만 쌓는다.
func collectCaptureEventsUntil(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) ([]captureEvent, captureSummary, error) {
	events := make([]captureEvent, 0)
	summary, err := collectCaptureEventsFor(traceScope{}, duration, func(event captureEvent) error {
		events = append(events, event)
		if onEvent != nil {
			return onEvent(event)
		}
		return nil
	}, stop)
	if err != nil {
		return nil, captureSummary{}, err
	}
	return events, summary, nil
}

// protocols는 hook을 쓰는 trace protocol이다. nil이면 모든 protocol이 쓴다.
type captureTracepoint struct {
	protocols   []string
	group, name string
	prog        *ebpf.Program
}

type captureTracing struct {
	protocols []string
	name      string
	prog      *ebpf.Program
}

var captureTCPLengthTracepointsAvailable = func() bool {
	for _, root := range []string{"/sys/kernel/tracing", "/sys/kernel/debug/tracing"} {
		if _, err := os.Stat(filepath.Join(root, "events/sock/sock_send_length/id")); err == nil {
			if _, err := os.Stat(filepath.Join(root, "events/sock/sock_recv_length/id")); err == nil {
				return true
			}
		}
	}
	return false
}

// captureAttachments는 protocol에 필요한 hook만 고른다. fentry와 fexit는 뗄 때 kernel이 하나씩 처리해 hook마다
// 0.1초 넘게 걸리므로, 쓰지 않는 hook을 붙이면 trace를 끝낼 때마다 그만큼 늦어진다. 빈 protocol은 모든 hook을 고른다.
func captureAttachments(objects *captureEventsObjects, protocol string) ([]captureTracepoint, []captureTracing) {
	tcp, udp := []string{"tcp"}, []string{"udp", "dns"}
	// DNS는 port 53으로 가는 TCP 연결을 inet_sock_set_state로, 질의를 UDP 송신 hook으로 본다.
	tracepoints := []captureTracepoint{
		{[]string{"tcp", "dns"}, "sock", "inet_sock_set_state", objects.InetSockSetState},
		{tcp, "tcp", "tcp_retransmit_skb", objects.TcpRetransmitSkb},
		{tcp, "tcp", "tcp_send_reset", objects.TcpSendReset},
		{tcp, "tcp", "tcp_receive_reset", objects.TcpReceiveReset},
		{tcp, "tcp", "tcp_destroy_sock", objects.TcpDestroySock},
		{tcp, "sock", "sock_send_length", objects.TcpSendLength},
		{tcp, "sock", "sock_recv_length", objects.TcpRecvLength},
	}
	// skb_consume_udp는 모든 protocol이 쓴다. UDP 수신 event와 함께, target 이름을 짓는 DNS 응답도 이 hook이 읽는다.
	tracing := []captureTracing{
		{udp, "fentry/udp_send_skb", objects.UdpSendSkbEntry},
		{udp, "fexit/udp_send_skb", objects.UdpSendSkbExit},
		{udp, "fentry/udp_v6_send_skb", objects.UdpV6SendSkbEntry},
		{udp, "fexit/udp_v6_send_skb", objects.UdpV6SendSkbExit},
		// DNS 응답 시간을 network와 읽기 지연으로 나누려고 수신 큐에 들어간 시각을 잰다.
		{[]string{"dns"}, "fentry/__udp_enqueue_schedule_skb", objects.UdpEnqueueEntry},
		{nil, "fentry/skb_consume_udp", objects.SkbConsumeUdpEntry},
		// HTTP, MySQL, DNS over TCP는 TCP로 주고받는 사용자 버퍼의 앞부분을 읽는다.
		{[]string{"http", "mysql", "dns"}, "fentry/tcp_sendmsg", objects.TcpSendmsgEntry},
		{[]string{"mysql"}, "fexit/tcp_sendmsg", objects.TcpSendmsgExit},
		{[]string{"http", "mysql", "dns"}, "fentry/tcp_recvmsg", objects.TcpRecvmsgEntry},
		{[]string{"http", "mysql", "dns"}, "fexit/tcp_recvmsg", objects.TcpRecvmsgExit},
		// trace http는 끝난 socket의 짝짓기 상태를 지운다. tracepoint와 달리 tracefs 없이 붙는다.
		{[]string{"http", "mysql", "dns"}, "tp_btf/tcp_destroy_sock", objects.HttpTcpDestroySock},
		// 서버 쪽 DNS over TCP도 받은 연결의 process를 알아야 해서 dns가 함께 쓴다.
		{[]string{"tcp", "dns"}, "fentry/inet_csk_accept", objects.InetCskAcceptEntry},
		{[]string{"tcp"}, "fexit/inet_csk_accept", objects.InetCskAcceptExit},
		{[]string{"tcp", "dns"}, "fexit/tcp_create_openreq_child", objects.TcpCreateOpenreqChildExit},
		// SYN_SENT 전에 실패한 connect도 process를 알려고 connect()에서 주인을 배운다. 이 함수는 export되어 inline으로
		// 사라지지 않으므로 captureTraceHooksAvailable에서 확인하지 않는다.
		{tcp, "fentry/__inet_stream_connect", objects.InetStreamConnectEntry},
	}
	// 빈 protocol은 capture다. capture는 TCP와 UDP hook을 모두 쓰고 DNS 전용 hook은 쓰지 않는다.
	wanted := []string{protocol}
	if protocol == "" {
		wanted = []string{"tcp", "udp"}
	}
	unwanted := func(protocols []string) bool {
		return protocols != nil && !slices.ContainsFunc(wanted, func(name string) bool { return slices.Contains(protocols, name) })
	}
	tracepoints = slices.DeleteFunc(tracepoints, func(hook captureTracepoint) bool { return unwanted(hook.protocols) })
	tracing = slices.DeleteFunc(tracing, func(hook captureTracing) bool { return unwanted(hook.protocols) })
	return tracepoints, tracing
}

// captureAttachmentsFor는 captureAttachments에 --payload=all의 hook을 더한다. tcp_sendmsg가 끝날 때의 hook은 실제로 보낸
// byte 수를 알려 주지만 모든 TCP 송신에 붙으므로, --payload=all일 때만 붙인다.
func captureAttachmentsFor(objects *captureEventsObjects, scope traceScope) ([]captureTracepoint, []captureTracing) {
	tracepoints, tracing := captureAttachments(objects, scope.protocol)
	if scope.protocol == "tcp" && !captureTCPLengthTracepointsAvailable() {
		tracepoints = slices.DeleteFunc(tracepoints, func(hook captureTracepoint) bool {
			return hook.group == "sock" && (hook.name == "sock_send_length" || hook.name == "sock_recv_length")
		})
		tracing = append(tracing,
			captureTracing{[]string{"tcp"}, "fentry/tcp_sendmsg", objects.TcpSendmsgEntry},
			captureTracing{[]string{"tcp"}, "fexit/tcp_sendmsg", objects.TcpSendmsgExit},
			captureTracing{[]string{"tcp"}, "fentry/tcp_cleanup_rbuf", objects.TcpCleanupRbufEntry},
		)
	}
	if scope.payloadAll && scope.protocol != "mysql" {
		tracing = append(tracing, captureTracing{[]string{"http"}, "fexit/tcp_sendmsg", objects.TcpSendmsgExit})
	}
	return tracepoints, tracing
}

// captureEventFilter는 BPF를 불러오기 전에 정하는 event 필터다. protocol이 쓰지 않을 event를 kernel에서 버린다.
type captureEventFilter struct {
	udpEvents    bool
	dnsSent      bool
	server       bool
	tcpStatePort uint16
	httpMessages bool
	dnsTCP       bool
	// httpPayload는 HTTP message를 httpPayloadHead까지 읽는다. 끄면 BPF 기본값인 512바이트만 읽는다.
	httpPayload bool
	// httpPort가 0이 아니면 로컬이나 상대 port가 이 값인 socket의 HTTP message만 본다.
	httpPort uint16
	// httpMessageLimit가 0이 아니면 HTTP message 하나를 이 byte 수까지 조각으로 따라간다.
	httpMessageLimit uint32
	// mysqlPort가 0이 아니면 로컬이나 상대 port가 이 값인 socket의 MySQL packet을 읽는다.
	mysqlPort uint16
	// tlsPlaintext는 trace http --tls다. OpenSSL uprobe가 넘기는 평문 레코드를 낸다.
	tlsPlaintext bool
}

func captureEventFilterFor(scope traceScope) captureEventFilter {
	filter := captureEventFilter{udpEvents: true, dnsSent: true}
	switch scope.protocol {
	case "tcp":
		// skb_consume_udp는 target 이름을 지을 DNS 응답만 보낸다.
		filter.udpEvents = false
	case "udp":
		filter.dnsSent = false
	case "dns":
		filter.udpEvents, filter.server, filter.tcpStatePort, filter.dnsTCP = false, scope.server, 53, true
	case "http":
		filter.udpEvents, filter.httpMessages, filter.httpPayload, filter.httpPort, filter.tlsPlaintext = false, true, scope.payload, scope.port, scope.tls != ""
		if scope.payloadAll {
			filter.httpMessageLimit = httpMessageMax
		}
	case "mysql":
		filter.udpEvents, filter.mysqlPort = false, cmp.Or(scope.port, traceMySQLDefaultPort)
	}
	return filter
}

func loadCaptureEventsFor(scope traceScope, objects *captureEventsObjects) error {
	spec, err := loadCaptureEvents()
	if err != nil {
		return err
	}
	var variables captureEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return err
	}
	filter := captureEventFilterFor(scope)
	tcpLengthFallback := scope.protocol == "tcp" && !captureTCPLengthTracepointsAvailable()
	recvArgs, err := tcpRecvmsgArgumentCount()
	if err != nil {
		return err
	}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("read kernel BTF: %w", err)
	}
	acceptArgs, err := inetCskAcceptArgumentCount(kernel)
	if err != nil {
		return err
	}
	flag := func(on bool) uint8 {
		if on {
			return 1
		}
		return 0
	}
	if err := errors.Join(variables.EmitUdpEvents.Set(flag(filter.udpEvents)), variables.EmitDnsSent.Set(flag(filter.dnsSent)), variables.EmitDnsServer.Set(flag(filter.server)), variables.TcpStatePort.Set(filter.tcpStatePort), variables.TcpLengthFallback.Set(flag(tcpLengthFallback)),
		variables.EmitHttpMessages.Set(flag(filter.httpMessages)), variables.EmitDnsTcpMessages.Set(flag(filter.dnsTCP)), variables.HttpPort.Set(filter.httpPort), variables.MysqlPort.Set(filter.mysqlPort)); err != nil {
		return err
	}
	if filter.httpPayload {
		if err := variables.HttpPayloadLimit.Set(uint32(httpPayloadHead)); err != nil {
			return err
		}
	}
	if filter.httpMessageLimit != 0 {
		if err := variables.HttpMessageLimit.Set(filter.httpMessageLimit); err != nil {
			return err
		}
	}
	if filter.tlsPlaintext {
		// resolveTraceTLSTargets가 amd64와 arm64만 받으므로 여기서는 둘 중 하나다.
		arch := uint8(0)
		if runtime.GOARCH == "arm64" {
			arch = 1
		}
		if err := errors.Join(variables.EmitTlsPlaintext.Set(uint8(1)), variables.UprobeArch.Set(arch)); err != nil {
			return err
		}
	}
	selected := "tcp_recvmsg_exit"
	if recvArgs == 6 {
		selected = "tcp_recvmsg_exit_legacy"
	}
	recvSpec := spec.Programs[selected]
	if recvSpec == nil {
		return fmt.Errorf("missing eBPF program %s", selected)
	}
	spec.Programs["tcp_recvmsg_exit"] = recvSpec.Copy()
	spec.Programs["tcp_recvmsg_exit"].Name = "tcp_recvmsg_exit"
	spec.Programs["tcp_recvmsg_exit_legacy"] = recvSpec.Copy()
	spec.Programs["tcp_recvmsg_exit_legacy"].Name = "tcp_recvmsg_exit_legacy"
	selected = "inet_csk_accept_exit"
	if acceptArgs == 4 {
		selected = "inet_csk_accept_exit_legacy"
	}
	acceptSpec := spec.Programs[selected]
	if acceptSpec == nil {
		return fmt.Errorf("missing eBPF program %s", selected)
	}
	spec.Programs["inet_csk_accept_exit"] = acceptSpec.Copy()
	spec.Programs["inet_csk_accept_exit"].Name = "inet_csk_accept_exit"
	spec.Programs["inet_csk_accept_exit_legacy"] = acceptSpec.Copy()
	spec.Programs["inet_csk_accept_exit_legacy"].Name = "inet_csk_accept_exit_legacy"
	return spec.LoadAndAssign(objects, nil)
}

func tcpRecvmsgArgumentCount() (int, error) {
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return 0, fmt.Errorf("read kernel BTF: %w", err)
	}
	var function *btf.Func
	if err := spec.TypeByName("tcp_recvmsg", &function); err != nil {
		return 0, fmt.Errorf("find tcp_recvmsg in kernel BTF: %w", err)
	}
	prototype, ok := btf.UnderlyingType(function.Type).(*btf.FuncProto)
	if !ok || (len(prototype.Params) != 5 && len(prototype.Params) != 6) {
		return 0, fmt.Errorf("unsupported tcp_recvmsg argument count: %d", len(prototype.Params))
	}
	return len(prototype.Params), nil
}

func collectCaptureEventsFor(scope traceScope, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	protocol := scope.protocol
	if err := rlimit.RemoveMemlock(); err != nil {
		return captureSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	// 프로그램을 붙이기 전에 채워서, 붙인 뒤 첫 event를 읽는 시점을 늦추지 않는다.
	names := newDNSNameCache()
	seedResolverCache(names)
	objects := captureEventsObjects{}
	if err := loadCaptureEventsFor(scope, &objects); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()

	attachments, tracing := captureAttachmentsFor(&objects, scope)
	links := make([]link.Link, 0, len(attachments)+len(tracing))
	closeLinks := func() { closeCaptureLinks(links) }
	for _, attachment := range attachments {
		attached, err := link.Tracepoint(attachment.group, attachment.name, attachment.prog, nil)
		if err != nil {
			closeLinks()
			return captureSummary{}, fmt.Errorf("attach %s/%s: %w", attachment.group, attachment.name, err)
		}
		links = append(links, attached)
	}
	for _, attachment := range tracing {
		attached, err := link.AttachTracing(link.TracingOptions{Program: attachment.prog})
		if err != nil {
			closeLinks()
			// fentry와 fexit는 BPF trampoline이 필요하다. UDP 목적지를 socket 기준으로 대신 기록하면 틀린 값이 나오므로 멈춘다.
			return captureSummary{}, fmt.Errorf("attach %s (needs BPF trampolines: x86_64 5.5+, arm64 6.0+): %w", attachment.name, err)
		}
		links = append(links, attached)
	}
	// uprobe도 attach 시각을 재기 전에 붙인다. 그 뒤의 레코드만 읽으므로 요청 쪽 평문만 보이는 구간이 없다.
	if scope.tlsFinder != nil {
		for _, target := range scope.tlsFinder.targets {
			attached, err := attachTraceTLS(&objects, target)
			links = append(links, attached...)
			gone := traceTLSAttachGone(attached, err)
			if err != nil && !gone {
				closeLinks()
				return captureSummary{}, err
			}
			if gone {
				scope.tlsFinder.retry(target)
			}
		}
	}
	defer closeLinks()
	// stopTLSWatch는 trace 중의 TLS 탐색을 멈추고 끝나기를 기다린다. 그 뒤에 tlsExecProblem을 읽는다.
	stopTLSWatch, tlsExecProblem := func() {}, ""
	if finder := scope.tlsFinder; finder != nil && finder.rescan {
		// 감시는 links에 붙인 link를 더하므로, closeLinks보다 먼저 멈춘다. defer는 나중에 건 것이 먼저 돈다.
		stopWatch, watched := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(watched)
			// 화면이 이미 열렸으므로 exec 알림을 받지 못한 이유는 trace가 끝난 뒤 알린다. nil channel이면 전체 탐색만 한다.
			execs, failed, err := traceTLSExecEvents(stopWatch)
			if err != nil {
				tlsExecProblem = err.Error()
			}
			if watchTraceTLS(finder, execs, func(target traceTLSTarget) bool {
				// 붙이지 못한 파일은 화면에 알리지 않는다. 일부만 붙었으면 그 link는 닫을 때 쓴다.
				attached, err := attachTraceTLS(&objects, target)
				links = append(links, attached...)
				return traceTLSAttachGone(attached, err)
			}, stopWatch) {
				tlsExecProblem = "process events stopped"
				select {
				case err := <-failed:
					tlsExecProblem = err.Error()
				default:
				}
			}
		}()
		var stopOnce sync.Once
		stopTLSWatch = func() {
			stopOnce.Do(func() {
				close(stopWatch)
				<-watched
			})
		}
		defer stopTLSWatch()
	}
	// hook은 하나씩 붙는다. 요청을 보내는 hook만 붙은 동안 보낸 요청은 응답을 놓치므로, HTTP 레코드는 모두 붙은 뒤부터 읽는다.
	var attached unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &attached); err != nil {
		return captureSummary{}, fmt.Errorf("read monotonic clock: %w", err)
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
	if duration > 0 {
		reader.SetDeadline(deadline)
	}
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

	targets := newCommandTargetCache(commandTarget)
	sockets := newSocketTargetCache()
	owners := newPIDTargetCache()
	queries := newDNSQueryTracker(scope.server)
	// --payload=all은 message가 끝날 때 payload를 붙이므로 tracker는 첫 조각에 payload를 붙이지 않는다.
	requests := newHTTPTracker(scope.side, scope.payload && !scope.payloadAll, scope.showSecrets)
	requests.keepGzip = scope.keepGzip
	requests.h2PayloadLimit = traceHTTP2PayloadLimit(scope)
	splits := httpSplitStarts{}
	var messages *httpMessages
	if scope.payloadAll {
		messages = newHTTPMessages(requests, httpMessageMax, scope.showSecrets)
	}
	streams := newDNSTCPStreams()
	mysql := newMySQLTracker(scope.side, scope.showSecrets)
	var goOrder *goTLSOrder
	if scope.tlsFinder != nil && slices.ContainsFunc(scope.tlsFinder.targets, func(target traceTLSTarget) bool { return target.goReturns != nil }) {
		goOrder = newGoTLSOrder()
	}
	var ready [][]byte
	var eventCount uint64
	emit := func(events []captureEvent) error {
		for _, event := range events {
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return err
				}
			}
			eventCount++
		}
		return nil
	}
	// 끝나기를 기다리는 HTTP message가 있으면 reader가 오래 막히지 않게 deadline을 짧게 둔다. ring buffer reader는 버퍼가
	// 빌 때만 deadline을 보므로, event가 계속 올 때는 레코드를 읽을 때마다 시각을 확인한다.
	swept := time.Now()
	sweepDeadline := func() {
		if messages == nil {
			return
		}
		next := time.Now().Add(httpMessageIdle / 4)
		if duration > 0 && deadline.Before(next) {
			next = deadline
		}
		reader.SetDeadline(next)
	}
	sweepDeadline()
	finish := func() (captureSummary, error) {
		if err := emit(requests.finishHTTP2Payloads(func(http2PayloadKey) bool { return true })); err != nil {
			return captureSummary{}, err
		}
		if messages != nil {
			if err := emit(messages.flush()); err != nil {
				return captureSummary{}, err
			}
		}
		var lost, unmapped uint64
		if lookupErr := objects.LostEvents.Lookup(uint32(0), &lost); lookupErr != nil {
			return captureSummary{}, fmt.Errorf("read lost event count: %w", lookupErr)
		}
		if lookupErr := objects.TlsUnmapped.Lookup(uint32(0), &unmapped); lookupErr != nil {
			return captureSummary{}, fmt.Errorf("read unmapped TLS count: %w", lookupErr)
		}
		if goOrder != nil {
			lost += goOrder.finish() + uint64(len(ready))
		}
		// 감시를 기다리는 동안에도 hook은 붙어 있고 남은 레코드는 읽지 않으므로, 잃은 수를 읽은 뒤에 멈춘다.
		stopTLSWatch()
		return captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, LostEvents: lost, TLSUnmapped: unmapped, TLSExecProblem: tlsExecProblem}, nil
	}
	for {
		// ring buffer reader는 버퍼가 비었을 때만 deadline을 본다. event가 계속 쌓이면 버퍼가 비지 않아
		// --duration이 지나도 끝나지 않으므로 여기서 직접 확인한다.
		if duration > 0 && !time.Now().Before(deadline) {
			return finish()
		}
		var record ringbuf.Record
		var err error
		if len(ready) != 0 {
			record.RawSample = ready[0]
			ready = ready[1:]
		} else {
			record, err = reader.Read()
			if errors.Is(err, os.ErrDeadlineExceeded) {
				if messages != nil && (duration == 0 || time.Now().Before(deadline)) {
					if err := emit(messages.expire(time.Now())); err != nil {
						return captureSummary{}, err
					}
					swept = time.Now()
					sweepDeadline()
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
			if goOrder != nil {
				if len(record.RawSample) >= 12 && binary.LittleEndian.Uint32(record.RawSample[8:12]) != goTLSOrderRecord && !captureRecordAfterAttached(binary.LittleEndian.Uint64(record.RawSample[:8]), attached) {
					continue
				}
				ready = goOrder.add(record.RawSample)
				if len(ready) == 0 {
					continue
				}
				record.RawSample = ready[0]
				ready = ready[1:]
			}
		}
		if packet, ok := parseDNSRecord(record.RawSample); ok {
			if !packet.sent {
				names.rememberAnswer(packet.pid, dnsAnswerNames(packet.payload))
			}
			// DNS 레코드는 다른 protocol에서 target 이름에만 쓴다. capture와 trace tcp/udp의 출력에 섞지 않는다.
			if protocol != "dns" {
				continue
			}
			// 서버 쪽 레코드는 --side server일 때만 BPF가 보낸다. client 쪽 레코드는 늘 오므로 tracker가 다른 쪽을 거른다.
			// TCP 조각은 길이와 message를 맞춘 뒤 UDP message와 같은 방법으로 읽는다.
			messages := [][]byte{packet.payload}
			if packet.tcp {
				messages = streams.messages(dnsTCPStreamKey{socket: packet.socket, sent: packet.sent}, packet.payload, dnsRecordPayloadSize)
			}
			for _, message := range messages {
				packet.payload = message
				event, ok := queries.event(packet, clockOffset)
				if !ok {
					continue
				}
				if packet.tcp {
					event.Transport = "tcp"
				}
				if onEvent != nil {
					if err := onEvent(event); err != nil {
						return captureSummary{}, err
					}
				}
				eventCount++
			}
			continue
		}
		if packet, ok := parseHTTPRecord(record.RawSample); ok {
			if !captureRecordAfterAttached(packet.bootTimeNS, attached) {
				continue
			}
			// h2c와 --tls 평문의 HTTP/2는 frame을 이어 읽어야 해서 조각 결합보다 먼저 받는다. 새 연결이 이전 연결의
			// 상태를 비우면 HTTP/2로 읽지 않은 레코드에서도 본문을 기다리던 event가 나온다.
			events, claimed := requests.http2Events(packet, clockOffset)
			if err := emit(events); err != nil {
				return captureSummary{}, err
			}
			if claimed {
				continue
			}
			if packet, ok = splits.join(packet); !ok {
				continue
			}
			if messages != nil {
				now := time.Now()
				events := messages.add(packet, clockOffset, now)
				if now.Sub(swept) >= httpMessageIdle/4 {
					events = append(events, messages.expire(now)...)
					swept = now
				}
				if err := emit(events); err != nil {
					return captureSummary{}, err
				}
				continue
			}
			// 첫 조각에 잇지 못한 조각은 요청이나 응답으로 읽지 않는다.
			if packet.continued {
				continue
			}
			event, ok := requests.event(packet, clockOffset)
			if !ok {
				continue
			}
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return captureSummary{}, err
				}
			}
			eventCount++
			continue
		}
		if packet, ok := parseMySQLRecord(record.RawSample); ok {
			if !captureRecordAfterAttached(packet.bootTimeNS, attached) {
				continue
			}
			if err := emit(mysql.events(packet, clockOffset)); err != nil {
				return captureSummary{}, err
			}
			continue
		}
		if socket, ok := parseTCPDestroyRecord(record.RawSample); ok {
			if protocol == "http" {
				if err := emit(requests.forget(socket)); err != nil {
					return captureSummary{}, err
				}
				splits.forget(socket)
			}
			if protocol == "mysql" {
				mysql.forgetSocket(socket)
			}
			if protocol == "dns" {
				streams.forgetSocket(socket)
			}
			continue
		}
		if owner, ok := parseOwnerAnnouncement(record.RawSample); ok {
			if owner.readable {
				owners.remember(owner.pid, owner.target)
			}
			continue
		}
		var raw captureEventRaw
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &raw); err != nil {
			return captureSummary{}, fmt.Errorf("decode event: %w", err)
		}
		event := raw.event(clockOffset)
		event.Source, event.Destination = sockets.addresses(event)
		commandTarget := ""
		if event.PID != 0 {
			if target, ok := owners.target(event.PID); ok {
				commandTarget = target
			} else {
				commandTarget = targets.target(event.PID, time.Now())
			}
		}
		event.Target, event.TargetSource = resolveTraceTarget(event, commandTarget, names)
		event.Target, event.TargetSource = sockets.target(event)
		if protocol == "dns" {
			if dnsEvent, ok := queries.tcpEvent(event); ok {
				event = dnsEvent
			}
		}
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return captureSummary{}, err
			}
		}
		eventCount++
	}
}

func captureRecordAfterAttached(bootTimeNS uint64, attached unix.Timespec) bool {
	return bootTimeNS >= uint64(attached.Nano())
}

func traceStopRequested(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

type captureEventRaw struct {
	TimestampNS     uint64
	EventType       uint32
	PID             uint32
	CgroupID        uint64
	SkAddr          uint64
	OldState        uint32
	NewState        uint32
	Family          uint16
	Sport           uint16
	Dport           uint16
	Protocol        uint16
	Source          [16]byte
	Destination     [16]byte
	Comm            [16]byte
	Bytes           uint64
	TCPValid        uint32
	RTTUS           uint32
	RTTVarUS        uint32
	CWND            uint32
	SSThresh        uint32
	Unacked         uint32
	Lost            uint32
	ZeroWindow      uint32
	AcceptLatencyNS uint64
	AcceptQueueUsed uint32
	AcceptQueueMax  uint32
}

// captureClockOffset은 CLOCK_MONOTONIC 값에 더하면 Unix epoch 시각이 되는 차이다.
// bpf_ktime_get_ns는 부팅 후 monotonic 시간이라, 그대로 두면 요약 줄의 epoch 시각과 기준이 다르다.
func captureClockOffset() (int64, error) {
	var monotonic unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		return 0, err
	}
	return time.Now().UnixNano() - monotonic.Nano(), nil
}

// tcpDestroyEventType은 capture_events_bpf.c의 tcp_destroy_sock과 http_tcp_destroy_sock이 내는 struct event 종류다.
const tcpDestroyEventType = 5

// parseTCPDestroyRecord는 끝난 TCP socket의 주소를 읽는다. trace http에서 --port가 없으면 모든 TCP 연결마다 오므로
// struct 전체를 binary.Read로 풀지 않는다.
func parseTCPDestroyRecord(sample []byte) (uint64, bool) {
	if len(sample) < 32 || binary.LittleEndian.Uint32(sample[8:12]) != tcpDestroyEventType {
		return 0, false
	}
	return binary.LittleEndian.Uint64(sample[24:32]), true
}

func (raw captureEventRaw) event(clockOffset int64) captureEvent {
	name, protocol := captureEventTypeName(raw.EventType, raw.Protocol)
	switch raw.EventType {
	case 1:
		name = captureEventName(raw.OldState, raw.NewState)
	}
	return captureEvent{
		SocketID:        raw.SkAddr,
		TimestampNS:     uint64(int64(raw.TimestampNS) + clockOffset),
		BootTimeNS:      raw.TimestampNS,
		Event:           name,
		Protocol:        protocol,
		PID:             raw.PID,
		Process:         strings.TrimRight(string(raw.Comm[:]), "\x00"),
		CgroupID:        raw.CgroupID,
		Source:          formatCaptureAddress(raw.Family, raw.Source, raw.Sport),
		Destination:     formatCaptureAddress(raw.Family, raw.Destination, raw.Dport),
		OldState:        tcpStateName(raw.OldState),
		NewState:        tcpStateName(raw.NewState),
		Bytes:           raw.Bytes,
		TCPValid:        raw.TCPValid,
		RTTUS:           raw.RTTUS,
		RTTVarUS:        raw.RTTVarUS,
		CWND:            raw.CWND,
		SSThresh:        raw.SSThresh,
		Unacked:         raw.Unacked,
		Lost:            raw.Lost,
		ZeroWindow:      raw.ZeroWindow,
		AcceptLatencyNS: raw.AcceptLatencyNS,
		AcceptQueueUsed: raw.AcceptQueueUsed,
		AcceptQueueMax:  raw.AcceptQueueMax,
	}
}

func formatCaptureAddress(family uint16, address [16]byte, port uint16) string {
	if family == 2 {
		return fmt.Sprintf("%d.%d.%d.%d:%d", address[0], address[1], address[2], address[3], port)
	}
	if family == 10 {
		// dual-stack socket의 IPv4 상대는 ::ffff:a.b.c.d로 담겨 온다. IPv4로 써야 같은 상대가 한 주소로 보인다.
		if netip.AddrFrom16(address).Is4In6() {
			return fmt.Sprintf("%d.%d.%d.%d:%d", address[12], address[13], address[14], address[15], port)
		}
		// RFC 5952 축약형으로 쓴다. 풀어 쓰면 ::1이 0:0:0:0:0:0:0:1이 되어 읽기 어렵다.
		return netip.AddrPortFrom(netip.AddrFrom16(address), port).String()
	}
	return ""
}
