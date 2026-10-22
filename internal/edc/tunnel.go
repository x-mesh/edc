package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	tunnelFileName  = "tunnels.yaml"
	tunnelNameLimit = 32
	// unix socket 경로는 macOS에서 104바이트를 넘으면 ssh가 연결하지 못한다.
	tunnelSocketPathLimit = 100
	tunnelAutoPort        = "auto"
	tunnelProbeWindow     = 300 * time.Millisecond
	tunnelAutoAttempts    = 3
)

var (
	tunnelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	tunnelPIDPattern  = regexp.MustCompile(`pid=(\d+)`)
)

type tunnelKind string

const (
	tunnelLocal   tunnelKind = "local"
	tunnelReverse tunnelKind = "reverse"
	tunnelSocks   tunnelKind = "socks"
)

// tunnelPort는 숫자 또는 auto다. 저장할 때 숫자는 따옴표 없이 쓴다.
type tunnelPort string

func (port tunnelPort) MarshalYAML() (interface{}, error) {
	if number, err := strconv.Atoi(string(port)); err == nil {
		return number, nil
	}
	return string(port), nil
}

type tunnelSpec struct {
	Host    string     `yaml:"host"`
	Local   tunnelPort `yaml:"local,omitempty"`
	Remote  string     `yaml:"remote,omitempty"`
	Reverse string     `yaml:"reverse,omitempty"`
	Socks   tunnelPort `yaml:"socks,omitempty"`
}

type tunnelDefinitions struct {
	Tunnels map[string]tunnelSpec `yaml:"tunnels"`
}

// tunnelPlan은 정의 하나를 ssh 인자로 풀어 낸 결과다. Port는 local과 socks에서는 로컬 포트, reverse에서는 원격 포트다.
type tunnelPlan struct {
	Name        string
	Host        string
	Target      string
	Kind        tunnelKind
	Port        int
	Destination string
	Forward     []string
}

func tunnelForward(kind tunnelKind, port int, destination string) []string {
	switch kind {
	case tunnelReverse:
		return []string{"-R", fmt.Sprintf("%d:%s", port, destination)}
	case tunnelSocks:
		return []string{"-D", fmt.Sprintf("127.0.0.1:%d", port)}
	default:
		return []string{"-L", fmt.Sprintf("127.0.0.1:%d:%s", port, destination)}
	}
}

var tunnelShellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_\[\]-]+$`)

// tunnelCommandLine은 같은 터널을 손으로 열 때 쓰는 ssh 명령이다. edc가 실제로 쓰는 -f, -M, -S 같은 관리용 옵션은 뺀다.
func tunnelCommandLine(target string, forward []string) string {
	words := append([]string{"ssh", "-N"}, forward...)
	words = append(words, target)
	for index, word := range words {
		if !tunnelShellSafe.MatchString(word) {
			words[index] = "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

func (plan tunnelPlan) Command() string { return tunnelCommandLine(plan.Target, plan.Forward) }

func tunnelError(key string, args ...interface{}) error {
	return errors.New(T(key, args...))
}

func parseTunnelPort(name, value string, allowAuto bool, pick func() (int, error)) (int, error) {
	if allowAuto && value == tunnelAutoPort {
		return pick()
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, tunnelError("tunnel.error.port_invalid", name, value)
	}
	return port, nil
}

func validTunnelHostPort(value string) (host string, ok bool) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" || strings.HasPrefix(host, "-") {
		return "", false
	}
	port, err := strconv.Atoi(portText)
	return host, err == nil && port >= 1 && port <= 65535
}

func buildTunnelPlan(name string, spec tunnelSpec, targets map[string]string, pick func() (int, error)) (tunnelPlan, error) {
	if len(name) > tunnelNameLimit || !tunnelNamePattern.MatchString(name) {
		return tunnelPlan{}, tunnelError("tunnel.error.name_invalid", name)
	}
	host := strings.TrimSpace(spec.Host)
	if host == "" {
		return tunnelPlan{}, tunnelError("tunnel.error.host_empty", name)
	}
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n") {
		return tunnelPlan{}, tunnelError("tunnel.error.host_invalid", name)
	}
	target := host
	if mapped, ok := targets[host]; ok {
		target = mapped
	}
	plan := tunnelPlan{Name: name, Host: host, Target: target}
	kinds := 0
	for _, set := range []bool{spec.Remote != "", spec.Reverse != "", spec.Socks != ""} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return tunnelPlan{}, tunnelError("tunnel.error.kind_count", name)
	}
	switch {
	case spec.Remote != "":
		if spec.Local == "" {
			return tunnelPlan{}, tunnelError("tunnel.error.local_required", name)
		}
		if _, ok := validTunnelHostPort(spec.Remote); !ok {
			return tunnelPlan{}, tunnelError("tunnel.error.remote_invalid", name, spec.Remote)
		}
		port, err := parseTunnelPort(name, string(spec.Local), true, pick)
		if err != nil {
			return tunnelPlan{}, err
		}
		plan.Kind, plan.Port, plan.Destination = tunnelLocal, port, spec.Remote
		plan.Forward = tunnelForward(tunnelLocal, port, spec.Remote)
	case spec.Reverse != "":
		if spec.Local != "" {
			return tunnelPlan{}, tunnelError("tunnel.error.local_unexpected", name)
		}
		// 대상은 host:port이고 IPv6 주소는 [::1]:3000처럼 대괄호로 감싸므로, 앞의 원격 포트만 첫 ':'에서 떼어 낸다.
		remotePort, destination, found := strings.Cut(spec.Reverse, ":")
		if _, valid := validTunnelHostPort(destination); !found || !valid {
			return tunnelPlan{}, tunnelError("tunnel.error.reverse_invalid", name, spec.Reverse)
		}
		port, err := parseTunnelPort(name, remotePort, false, nil)
		if err != nil {
			return tunnelPlan{}, err
		}
		plan.Kind, plan.Port, plan.Destination = tunnelReverse, port, destination
		plan.Forward = tunnelForward(tunnelReverse, port, plan.Destination)
	default:
		if spec.Local != "" {
			return tunnelPlan{}, tunnelError("tunnel.error.local_unexpected", name)
		}
		port, err := parseTunnelPort(name, string(spec.Socks), true, pick)
		if err != nil {
			return tunnelPlan{}, err
		}
		plan.Kind, plan.Port = tunnelSocks, port
		plan.Forward = tunnelForward(tunnelSocks, port, "")
	}
	return plan, nil
}

func loadTunnelDefinitions(path string, targets map[string]string) (map[string]tunnelSpec, error) {
	var definitions tunnelDefinitions
	if err := decodeRemoteYAML(path, &definitions); err != nil {
		return nil, fmt.Errorf("tunnels %q: %w", path, err)
	}
	placeholder := func() (int, error) { return 0, nil }
	for name, spec := range definitions.Tunnels {
		if _, err := buildTunnelPlan(name, spec, targets, placeholder); err != nil {
			return nil, fmt.Errorf("tunnels %q: %w", path, err)
		}
	}
	if definitions.Tunnels == nil {
		definitions.Tunnels = map[string]tunnelSpec{}
	}
	return definitions.Tunnels, nil
}

// tunnelHostTargets는 inventory가 있으면 host 이름을 ssh 대상으로 바꾸는 표를 만든다. 없거나 읽지 못해도 터널은 쓸 수 있다.
func tunnelHostTargets(cwd, configDir string) map[string]string {
	path, found := discoverRemoteInventory(cwd, configDir)
	if !found {
		return nil
	}
	inventory, err := loadRemoteInventory(path)
	if err != nil {
		return nil
	}
	targets := make(map[string]string, len(inventory.Hosts))
	for _, host := range inventory.Hosts {
		targets[host.Name] = host.Target
	}
	return targets
}

type tunnelRunner interface {
	Start(ctx context.Context, plan tunnelPlan, socket string) error
	Check(ctx context.Context, target, socket string) (int, error)
	Stop(ctx context.Context, target, socket string) error
	// Probe는 터널을 만들지 않고 ssh를 한 번 실행해 연결이 되는지만 본다. 출력과 오류를 그대로 돌려준다.
	Probe(ctx context.Context, args []string) (string, error)
}

type sshTunnelRunner struct {
	connectTimeout time.Duration
}

func (runner sshTunnelRunner) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", T("tunnel.error.ssh_not_found"), err)
	}
	return exec.CommandContext(ctx, path, args...), nil
}

func (runner sshTunnelRunner) Start(ctx context.Context, plan tunnelPlan, socket string) error {
	connectSeconds := int((runner.connectTimeout + time.Second - 1) / time.Second)
	args := []string{
		"-f", "-N", "-M", "-S", socket,
		"-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", connectSeconds),
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3",
	}
	args = append(args, plan.Forward...)
	args = append(args, plan.Target)
	command, err := runner.command(ctx, args...)
	if err != nil {
		return err
	}
	// -f로 남는 ssh가 stderr를 계속 쥐고 있어서 pipe를 쓰면 출력을 읽는 쪽이 끝나지 않는다. 파일로 받는다.
	log, err := os.CreateTemp("", "edc-tunnel-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(log.Name())
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Run(); err != nil {
		detail, _ := os.ReadFile(log.Name())
		reason := strings.TrimSpace(string(detail))
		if reason == "" {
			reason = err.Error()
		}
		return tunnelError("tunnel.error.start_failed", plan.Name, reason)
	}
	return nil
}

func (runner sshTunnelRunner) Probe(ctx context.Context, args []string) (string, error) {
	connectSeconds := int((runner.connectTimeout + time.Second - 1) / time.Second)
	full := append([]string{"-o", "BatchMode=yes", "-o", fmt.Sprintf("ConnectTimeout=%d", connectSeconds)}, args...)
	command, err := runner.command(ctx, full...)
	if err != nil {
		return "", err
	}
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func (runner sshTunnelRunner) Check(ctx context.Context, target, socket string) (int, error) {
	command, err := runner.command(ctx, "-S", socket, "-O", "check", target)
	if err != nil {
		return 0, err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return 0, err
	}
	if match := tunnelPIDPattern.FindStringSubmatch(string(output)); match != nil {
		pid, _ := strconv.Atoi(match[1])
		return pid, nil
	}
	return 0, nil
}

func (runner sshTunnelRunner) Stop(ctx context.Context, target, socket string) error {
	command, err := runner.command(ctx, "-S", socket, "-O", "exit", target)
	if err != nil {
		return err
	}
	if output, err := command.CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(output)))
	}
	return nil
}

// tunnelRecord는 실행 중인 터널이 남기는 상태 파일이다. 정의 파일이 없어도 down과 list가 동작하도록 필요한 값을 모두 담는다.
type tunnelRecord struct {
	Name        string     `json:"name"`
	Host        string     `json:"host"`
	Target      string     `json:"target"`
	Kind        tunnelKind `json:"kind"`
	Port        int        `json:"port"`
	Destination string     `json:"destination,omitempty"`
	Socket      string     `json:"socket"`
	PID         int        `json:"pid,omitempty"`
	Started     time.Time  `json:"started"`
}

func (record tunnelRecord) spec() tunnelSpec {
	port := strconv.Itoa(record.Port)
	switch record.Kind {
	case tunnelReverse:
		return tunnelSpec{Host: record.Host, Reverse: port + ":" + record.Destination}
	case tunnelSocks:
		return tunnelSpec{Host: record.Host, Socks: tunnelPort(port)}
	default:
		return tunnelSpec{Host: record.Host, Local: tunnelPort(port), Remote: record.Destination}
	}
}

type tunnelStore struct{ dir string }

func defaultTunnelStore() (tunnelStore, error) {
	path, err := defaultHistoryPath()
	if err != nil {
		return tunnelStore{}, err
	}
	return tunnelStore{dir: filepath.Join(filepath.Dir(path), "tunnels")}, nil
}

func (store tunnelStore) recordPath(name string) string {
	return filepath.Join(store.dir, name+".json")
}

func (store tunnelStore) socketPath(name string) (string, error) {
	path := filepath.Join(store.dir, name+".sock")
	if len(path) > tunnelSocketPathLimit {
		return "", tunnelError("tunnel.error.socket_path", path)
	}
	return path, nil
}

func (store tunnelStore) save(record tunnelRecord) error {
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeFileAtomic(store.recordPath(record.Name), data, 0o600)
}

func (store tunnelStore) remove(name string) {
	_ = os.Remove(store.recordPath(name))
	_ = os.Remove(filepath.Join(store.dir, name+".sock"))
}

func (store tunnelStore) load() []tunnelRecord {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return nil
	}
	var records []tunnelRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(store.dir, entry.Name()))
		if err != nil {
			continue
		}
		var record tunnelRecord
		if json.Unmarshal(data, &record) != nil || record.Name != strings.TrimSuffix(entry.Name(), ".json") {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool { return records[left].Name < records[right].Name })
	return records
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

type tunnelState string

const (
	tunnelStateUp    tunnelState = "up"
	tunnelStateDown  tunnelState = "down"
	tunnelStateStale tunnelState = "stale"
)

const (
	tunnelReachOK      = "ok"
	tunnelReachRefused = "refused"
	tunnelReachNone    = "-"
)

type tunnelRow struct {
	Name        string      `json:"name"`
	Host        string      `json:"host"`
	Kind        tunnelKind  `json:"kind"`
	Port        int         `json:"port"`
	Destination string      `json:"destination,omitempty"`
	State       tunnelState `json:"state"`
	Reach       string      `json:"reach"`
	Started     *time.Time  `json:"started,omitempty"`
	Defined     bool        `json:"defined"`
	Command     string      `json:"command,omitempty"`
	Warnings    []string    `json:"warnings,omitempty"`
}

type tunnelManager struct {
	store     tunnelStore
	runner    tunnelRunner
	targets   map[string]string
	now       func() time.Time
	pick      func() (int, error)
	available func(port int) error
	probe     func(port int) string
	dial      func(address string) error
}

func newTunnelManager(store tunnelStore, runner tunnelRunner, targets map[string]string) tunnelManager {
	return tunnelManager{store: store, runner: runner, targets: targets, now: time.Now,
		pick: pickTunnelPort, available: tunnelPortAvailable, probe: probeTunnelPort, dial: dialTunnelTarget}
}

func pickTunnelPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func tunnelPortAvailable(port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return tunnelError("tunnel.error.port_busy", port)
	}
	return listener.Close()
}

// probeTunnelPort는 터널 입구로 TCP 연결을 열고 짧게 기다린다. 목적지가 거절하면 ssh가 곧바로 연결을 끊으므로,
// 그 안에 끊기면 refused, 버티면 ok다. 연결 시간은 로컬 listener까지라서 지연으로 보이지 않는다.
func probeTunnelPort(port int) string {
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return tunnelReachRefused
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(tunnelProbeWindow))
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return tunnelReachOK
		}
		return tunnelReachRefused
	}
	return tunnelReachOK
}

func (manager tunnelManager) rows(ctx context.Context, specs map[string]tunnelSpec) []tunnelRow {
	records := make(map[string]tunnelRecord)
	names := make(map[string]struct{})
	for _, record := range manager.store.load() {
		records[record.Name] = record
		names[record.Name] = struct{}{}
	}
	for name := range specs {
		names[name] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	rows := make([]tunnelRow, 0, len(sorted))
	placeholder := func() (int, error) { return 0, nil }
	for _, name := range sorted {
		row := tunnelRow{Name: name, State: tunnelStateDown, Reach: tunnelReachNone}
		if spec, defined := specs[name]; defined {
			row.Defined = true
			if plan, err := buildTunnelPlan(name, spec, manager.targets, placeholder); err == nil {
				row.Host, row.Kind, row.Port, row.Destination = plan.Host, plan.Kind, plan.Port, plan.Destination
				if plan.Port > 0 {
					row.Command = plan.Command()
				}
			}
		}
		if record, running := records[name]; running {
			row.Host, row.Kind, row.Port, row.Destination = record.Host, record.Kind, record.Port, record.Destination
			row.Command = tunnelCommandLine(record.Target, tunnelForward(record.Kind, record.Port, record.Destination))
			started := record.Started
			row.Started = &started
			row.State = tunnelStateStale
			if _, err := manager.runner.Check(ctx, record.Target, record.Socket); err == nil {
				row.State = tunnelStateUp
				if record.Kind != tunnelReverse {
					row.Reach = manager.probe(record.Port)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (manager tunnelManager) up(ctx context.Context, name string, spec tunnelSpec) (tunnelRow, error) {
	plan, err := buildTunnelPlan(name, spec, manager.targets, manager.pick)
	if err != nil {
		return tunnelRow{}, err
	}
	socket, err := manager.store.socketPath(name)
	if err != nil {
		return tunnelRow{}, err
	}
	if err := manager.rejectConflicts(ctx, plan); err != nil {
		return tunnelRow{}, err
	}
	if err := os.MkdirAll(manager.store.dir, 0o700); err != nil {
		return tunnelRow{}, err
	}
	attempts := 1
	if spec.Local == tunnelAutoPort || spec.Socks == tunnelAutoPort {
		attempts = tunnelAutoAttempts
	}
	for attempt := 1; ; attempt++ {
		err = nil
		if plan.Kind != tunnelReverse {
			err = manager.available(plan.Port)
		}
		if err == nil {
			_ = os.Remove(socket)
			err = manager.runner.Start(ctx, plan, socket)
		}
		if err == nil {
			break
		}
		// auto로 고른 포트는 고른 뒤 ssh가 바인딩하기 전에 다른 프로세스가 가져갈 수 있다. 그때만 새 포트로 다시 시도한다.
		if attempt >= attempts || !tunnelPortRace(err) {
			return tunnelRow{}, tunnelStartFailure(plan, err)
		}
		if plan, err = buildTunnelPlan(name, spec, manager.targets, manager.pick); err != nil {
			return tunnelRow{}, err
		}
	}
	pid, _ := manager.runner.Check(ctx, plan.Target, socket)
	record := tunnelRecord{Name: name, Host: plan.Host, Target: plan.Target, Kind: plan.Kind, Port: plan.Port,
		Destination: plan.Destination, Socket: socket, PID: pid, Started: manager.now()}
	if err := manager.store.save(record); err != nil {
		_ = manager.runner.Stop(ctx, plan.Target, socket)
		return tunnelRow{}, err
	}
	started := record.Started
	row := tunnelRow{Name: name, Host: plan.Host, Kind: plan.Kind, Port: plan.Port, Destination: plan.Destination,
		State: tunnelStateUp, Reach: tunnelReachNone, Started: &started, Command: plan.Command()}
	if plan.Kind == tunnelReverse && manager.dial(plan.Destination) != nil {
		row.Warnings = append(row.Warnings, T("tunnel.warning.target_down", plan.Destination))
	}
	return row, nil
}

// rejectConflicts는 같은 이름의 실행 중인 터널과, 같은 포트를 쓰는 다른 실행 중인 터널을 시작 전에 막는다.
// 죽은(stale) 기록은 지운다.
func (manager tunnelManager) rejectConflicts(ctx context.Context, plan tunnelPlan) error {
	for _, record := range manager.store.load() {
		sameName := record.Name == plan.Name
		samePort := !sameName && record.Port == plan.Port && ((plan.Kind == tunnelReverse && record.Kind == tunnelReverse && record.Target == plan.Target) ||
			(plan.Kind != tunnelReverse && record.Kind != tunnelReverse))
		if !sameName && !samePort {
			continue
		}
		if _, err := manager.runner.Check(ctx, record.Target, record.Socket); err != nil {
			if sameName {
				manager.store.remove(record.Name)
			}
			continue
		}
		switch {
		case sameName:
			return tunnelError("tunnel.error.already_up", plan.Name, record.Port)
		case plan.Kind == tunnelReverse:
			return tunnelError("tunnel.error.remote_port_taken", plan.Port, plan.Target, record.Name)
		default:
			return tunnelError("tunnel.error.port_taken", plan.Port, record.Name)
		}
	}
	return nil
}

func tunnelPortRace(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "address already in use") || strings.Contains(text, "cannot listen to port") ||
		strings.Contains(text, "could not request local forwarding")
}

// tunnelStartFailure는 원격 포트가 막혔을 때 다음에 할 일을 덧붙인다. ssh 메시지만으로는 어느 포트를 바꿔야 하는지 알기 어렵다.
func tunnelStartFailure(plan tunnelPlan, err error) error {
	if plan.Kind == tunnelReverse && strings.Contains(err.Error(), "remote port forwarding failed") {
		return fmt.Errorf("%w. %s", err, T("tunnel.hint.remote_port", plan.Port))
	}
	return err
}

// tunnelSharedWarnings는 정의 파일에서 같은 포트를 쓰는 다른 터널을 알린다. 둘을 함께 켜면 나중 것이 실패한다.
func tunnelSharedWarnings(name string, specs map[string]tunnelSpec, targets map[string]string) []string {
	placeholder := func() (int, error) { return 0, nil }
	own, err := buildTunnelPlan(name, specs[name], targets, placeholder)
	if err != nil || own.Port == 0 {
		return nil
	}
	var others []string
	for other := range specs {
		if other != name {
			others = append(others, other)
		}
	}
	sort.Strings(others)
	var warnings []string
	for _, other := range others {
		plan, err := buildTunnelPlan(other, specs[other], targets, placeholder)
		if err != nil || plan.Port != own.Port {
			continue
		}
		switch {
		case own.Kind == tunnelReverse && plan.Kind == tunnelReverse && plan.Target == own.Target:
			warnings = append(warnings, T("tunnel.warning.shared_remote", other, own.Port))
		case own.Kind != tunnelReverse && plan.Kind != tunnelReverse:
			warnings = append(warnings, T("tunnel.warning.shared_local", other, own.Port))
		}
	}
	return warnings
}

func dialTunnelTarget(address string) error {
	connection, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
	if err != nil {
		return err
	}
	return connection.Close()
}

// down은 실행 중인 터널을 끝내고 상태 파일을 지운다. 이미 죽은(stale) 터널은 상태 파일만 지운다.
func (manager tunnelManager) down(ctx context.Context, name string) (bool, error) {
	for _, record := range manager.store.load() {
		if record.Name != name {
			continue
		}
		if _, err := manager.runner.Check(ctx, record.Target, record.Socket); err == nil {
			if err := manager.runner.Stop(ctx, record.Target, record.Socket); err != nil {
				return false, tunnelError("tunnel.error.stop_failed", name, err.Error())
			}
		}
		manager.store.remove(name)
		return true, nil
	}
	return false, nil
}

func tunnelListen(row tunnelRow) string {
	switch {
	case row.Kind == tunnelReverse:
		return fmt.Sprintf("remote:%d", row.Port)
	case row.Port == 0:
		return tunnelAutoPort
	case row.Kind == tunnelSocks:
		return fmt.Sprintf("socks 127.0.0.1:%d", row.Port)
	default:
		return fmt.Sprintf("127.0.0.1:%d", row.Port)
	}
}

func tunnelUptime(started *time.Time, now time.Time) string {
	if started == nil {
		return "-"
	}
	elapsed := now.Sub(*started).Round(time.Second)
	switch {
	case elapsed >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(elapsed.Hours())/24, int(elapsed.Hours())%24)
	case elapsed >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(elapsed.Hours()), int(elapsed.Minutes())%60)
	case elapsed >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(elapsed.Minutes()), int(elapsed.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	}
}

func printTunnelRows(writer io.Writer, rows []tunnelRow, now time.Time, verbose bool) {
	if len(rows) == 0 {
		fmt.Fprintln(writer, T("tunnel.list.empty"))
		return
	}
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', 0)
	headers := []string{
		T("tunnel.column.name"), T("tunnel.column.kind"), T("tunnel.column.listen"), T("tunnel.column.destination"),
		T("tunnel.column.host"), T("tunnel.column.state"), T("tunnel.column.reach"), T("tunnel.column.uptime"), T("tunnel.column.saved"),
	}
	if verbose {
		headers = append(headers, T("tunnel.column.command"))
	}
	fmt.Fprintln(table, strings.ToUpper(strings.Join(headers, "\t")))
	for _, row := range rows {
		destination := row.Destination
		if destination == "" {
			destination = "-"
		}
		cells := []string{row.Name, string(row.Kind), tunnelListen(row), destination,
			row.Host, string(row.State), row.Reach, tunnelUptime(row.Started, now), tunnelSavedMark(row.Defined)}
		if verbose {
			command := row.Command
			if command == "" {
				command = "-"
			}
			cells = append(cells, command)
		}
		fmt.Fprintln(table, strings.Join(cells, "\t"))
	}
	table.Flush()
}

// parseTunnelAdhoc는 -L, -R, -D 값을 정의 한 줄로 바꾼다. -L은 [local:]host:port 꼴이다.
func parseTunnelAdhoc(host, local, reverse, socks string) (tunnelSpec, error) {
	count := 0
	for _, value := range []string{local, reverse, socks} {
		if value != "" {
			count++
		}
	}
	if count != 1 {
		return tunnelSpec{}, tunnelError("tunnel.error.adhoc_conflict")
	}
	spec := tunnelSpec{Host: host}
	switch {
	case local != "":
		port, remote, found := strings.Cut(local, ":")
		if !found {
			return tunnelSpec{}, tunnelError("tunnel.error.remote_invalid", host, local)
		}
		spec.Local, spec.Remote = tunnelPort(port), remote
	case reverse != "":
		spec.Reverse = reverse
	default:
		spec.Socks = tunnelPort(socks)
	}
	return spec, nil
}

func tunnelAdhocName(host string, spec tunnelSpec) string {
	suffix := string(spec.Local)
	switch {
	case spec.Reverse != "":
		suffix = "r" + strings.SplitN(spec.Reverse, ":", 2)[0]
	case spec.Socks != "":
		suffix = "d" + string(spec.Socks)
	}
	name := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(host, "-") + "-" + suffix
	name = strings.TrimLeft(name, "._-")
	if len(name) > tunnelNameLimit {
		name = name[:tunnelNameLimit]
	}
	return name
}

type tunnelEnvironment struct {
	manager tunnelManager
	specs   map[string]tunnelSpec
	file    string
}

// allowMissing은 --save가 아직 없는 --file 경로에 첫 정의를 만들 수 있게 한다.
func newTunnelEnvironment(filePath string, connectTimeout time.Duration, allowMissing bool) (tunnelEnvironment, error) {
	store, err := defaultTunnelStore()
	if err != nil {
		return tunnelEnvironment{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return tunnelEnvironment{}, err
	}
	configDir, _ := os.UserConfigDir()
	targets := tunnelHostTargets(cwd, configDir)
	environment := tunnelEnvironment{manager: newTunnelManager(store, sshTunnelRunner{connectTimeout: connectTimeout}, targets), file: filePath}
	if environment.file == "" {
		if found, ok := discoverRemoteFile(cwd, configDir, tunnelFileName); ok {
			environment.file = found
		}
	}
	if environment.file != "" {
		if environment.specs, err = loadTunnelDefinitions(environment.file, targets); err != nil {
			switch {
			case errors.Is(err, os.ErrNotExist) && filePath == "":
				environment.file = ""
			case errors.Is(err, os.ErrNotExist) && allowMissing:
				environment.specs = nil
			default:
				return tunnelEnvironment{}, err
			}
		}
	}
	if environment.specs == nil {
		environment.specs = map[string]tunnelSpec{}
	}
	return environment, nil
}

func runTunnel(args []string, version string) int {
	subcommand := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcommand, args = args[0], args[1:]
	}
	options := configuredCommon(30 * time.Second)
	connectTimeout := 10 * time.Second
	var filePath, name, local, reverse, socks string
	var all, save bool
	set := newFlagSet("tunnel")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	set.StringVar(&filePath, "file", "", T("command.tunnel.option.file"))
	set.StringVar(&name, "name", "", T("command.tunnel.option.name"))
	set.StringVar(&local, "L", "", T("command.tunnel.option.local"))
	set.StringVar(&reverse, "R", "", T("command.tunnel.option.reverse"))
	set.StringVar(&socks, "D", "", T("command.tunnel.option.socks"))
	set.BoolVar(&all, "all", false, T("command.tunnel.option.all"))
	set.BoolVar(&save, "save", false, T("command.tunnel.option.save"))
	set.DurationVar(&connectTimeout, "connect-timeout", connectTimeout, T("command.tunnel.option.connect_timeout"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	if options.timeout <= 0 || connectTimeout <= 0 {
		fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.timeout_range"))
		return 2
	}
	environment, err := newTunnelEnvironment(filePath, connectTimeout, save && subcommand == "up")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	switch subcommand {
	case "":
		if options.jsonPath == "" && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
			return runTunnelScreen(environment, options.timeout)
		}
		return runTunnelList(ctx, environment, options)
	case "list":
		return runTunnelList(ctx, environment, options)
	case "up":
		return runTunnelUp(ctx, environment, set.Args(), name, [3]string{local, reverse, socks}, save)
	case "test":
		return runTunnelTest(ctx, environment, set.Args(), name, [3]string{local, reverse, socks}, options)
	case "down":
		return runTunnelDown(ctx, environment, set.Args(), all)
	default:
		fmt.Fprintln(os.Stderr, T("cli.error.unknown_command", "tunnel "+subcommand))
		fmt.Fprintln(os.Stderr, T("cli.error.help_hint"))
		return 2
	}
}

func runTunnelList(ctx context.Context, environment tunnelEnvironment, options commonOptions) int {
	rows := environment.manager.rows(ctx, environment.specs)
	if options.jsonPath != "" {
		return emitRemoteJSON(options, map[string]interface{}{"tunnels": rows})
	}
	printTunnelRows(os.Stdout, rows, time.Now(), options.verbose)
	return 0
}

// resolveTunnelSpec은 up과 test가 함께 쓴다. 이름이면 정의 파일에서, -L/-R/-D가 있으면 즉석 정의로 만든다.
func resolveTunnelSpec(environment tunnelEnvironment, positional []string, name string, adhoc [3]string) (string, tunnelSpec, bool) {
	if adhoc != [3]string{} {
		if len(positional) != 1 {
			fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.adhoc_host"))
			return "", tunnelSpec{}, false
		}
		spec, err := parseTunnelAdhoc(positional[0], adhoc[0], adhoc[1], adhoc[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return "", tunnelSpec{}, false
		}
		if name == "" {
			name = tunnelAdhocName(positional[0], spec)
		}
		return name, spec, true
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.name_required"))
		return "", tunnelSpec{}, false
	}
	spec, found := environment.specs[positional[0]]
	if !found {
		// 정의 없이 -L/-R/-D로 시작한 터널도 이름으로 다룰 수 있게 상태 파일에서 정의를 되살린다.
		for _, record := range environment.manager.store.load() {
			if record.Name == positional[0] {
				return record.Name, record.spec(), true
			}
		}
		fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.unknown", positional[0]))
		return "", tunnelSpec{}, false
	}
	return positional[0], spec, true
}

func runTunnelUp(ctx context.Context, environment tunnelEnvironment, positional []string, name string, adhoc [3]string, save bool) int {
	name, spec, ok := resolveTunnelSpec(environment, positional, name, adhoc)
	if !ok {
		return 2
	}
	savePath := ""
	if save {
		if adhoc == [3]string{} {
			fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.save_needs_adhoc"))
			return 2
		}
		if savePath = environment.file; savePath == "" {
			cwd, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			savePath = defaultTunnelFile(cwd)
		}
		if _, exists := environment.specs[name]; exists {
			fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.duplicate", name, savePath))
			return 2
		}
	}
	row, err := environment.manager.up(ctx, name, spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(T("tunnel.up.done", row.Name, tunnelListen(row), tunnelDestinationText(row)))
	fmt.Println("  $ " + row.Command)
	if adhoc == [3]string{} {
		row.Warnings = append(row.Warnings, tunnelSharedWarnings(name, environment.specs, environment.manager.targets)...)
	}
	for _, warning := range row.Warnings {
		fmt.Fprintln(os.Stderr, warning)
	}
	if save {
		if err := saveTunnelDefinition(savePath, name, spec); err != nil {
			fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.save_after_start", name, err.Error()))
			return 1
		}
		fmt.Println(T("tunnel.up.saved", name, savePath))
	}
	return 0
}

func runTunnelTest(ctx context.Context, environment tunnelEnvironment, positional []string, name string, adhoc [3]string, options commonOptions) int {
	name, spec, ok := resolveTunnelSpec(environment, positional, name, adhoc)
	if !ok {
		return 2
	}
	checks, err := environment.manager.check(ctx, name, spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	failed := tunnelFailedChecks(checks)
	if options.jsonPath != "" {
		if status := emitRemoteJSON(options, map[string]interface{}{"name": name, "ok": failed == 0, "checks": checks}); status != 0 {
			return status
		}
	} else {
		fmt.Println(T("tunnel.check.title", name))
		for _, check := range checks {
			fmt.Println("  " + tunnelCheckLine(check))
		}
		fmt.Println(tunnelCheckSummary(checks))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func tunnelDestinationText(row tunnelRow) string {
	if row.Destination != "" {
		return row.Destination + " via " + row.Host
	}
	return row.Host
}

func runTunnelDown(ctx context.Context, environment tunnelEnvironment, names []string, all bool) int {
	if all {
		if len(names) > 0 {
			fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.all_with_names"))
			return 2
		}
		for _, record := range environment.manager.store.load() {
			names = append(names, record.Name)
		}
	} else if len(names) == 0 {
		fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.name_required_down"))
		return 2
	}
	status := 0
	for _, name := range names {
		stopped, err := environment.manager.down(ctx, name)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, err)
			status = 1
		case !stopped:
			fmt.Fprintln(os.Stderr, tunnelError("tunnel.error.not_running", name))
			status = 1
		default:
			fmt.Println(T("tunnel.down.done", name))
		}
	}
	return status
}

type tunnelCheckStatus string

const (
	tunnelCheckPass tunnelCheckStatus = "pass"
	tunnelCheckFail tunnelCheckStatus = "fail"
	tunnelCheckWarn tunnelCheckStatus = "warn"
	tunnelCheckSkip tunnelCheckStatus = "skip"
)

// tunnelProbeTimeout은 시험 한 단계가 기다리는 최대 시간이다. 호스트가 응답하지 않아도 화면이 오래 묶이지 않게 한다.
const tunnelProbeTimeout = 15 * time.Second

type tunnelCheck struct {
	Step   string            `json:"step"`
	Label  string            `json:"label"`
	Status tunnelCheckStatus `json:"status"`
	Detail string            `json:"detail,omitempty"`
}

func tunnelProbeDetail(output string, err error) string {
	lines := strings.Split(output, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return err.Error()
}

func (manager tunnelManager) running(ctx context.Context, name string) (tunnelRecord, bool) {
	for _, record := range manager.store.load() {
		if record.Name != name {
			continue
		}
		if _, err := manager.runner.Check(ctx, record.Target, record.Socket); err == nil {
			return record, true
		}
	}
	return tunnelRecord{}, false
}

// check는 터널을 만들지 않고 시작에 필요한 조건을 하나씩 시험한다. 로컬 점검, SSH 로그인, 호스트에서 본 목적지(또는 원격 포트) 순이다.
// 실패해도 멈추지 않고 가능한 단계는 모두 시험하며, SSH 로그인이 실패하면 호스트가 필요한 단계만 건너뛴다.
func (manager tunnelManager) check(ctx context.Context, name string, spec tunnelSpec) ([]tunnelCheck, error) {
	plan, err := buildTunnelPlan(name, spec, manager.targets, func() (int, error) { return 0, nil })
	if err != nil {
		return nil, err
	}
	var checks []tunnelCheck
	add := func(step, label string, status tunnelCheckStatus, detail string) {
		checks = append(checks, tunnelCheck{Step: step, Label: label, Status: status, Detail: detail})
	}
	probe := func(args ...string) (string, error) {
		probeCtx, cancel := context.WithTimeout(ctx, tunnelProbeTimeout)
		defer cancel()
		return manager.runner.Probe(probeCtx, args)
	}
	record, isRunning := manager.running(ctx, name)
	if plan.Kind != tunnelReverse {
		switch {
		case isRunning:
			add("local_port", T("tunnel.check.label.local_port", record.Port), tunnelCheckPass, T("tunnel.check.detail.running"))
		case plan.Port == 0:
			add("local_port", T("tunnel.check.label.local_port_auto"), tunnelCheckPass, T("tunnel.check.detail.auto"))
		default:
			err := manager.rejectConflicts(ctx, plan)
			if err == nil {
				err = manager.available(plan.Port)
			}
			label := T("tunnel.check.label.local_port", plan.Port)
			if err != nil {
				add("local_port", label, tunnelCheckFail, err.Error())
			} else {
				add("local_port", label, tunnelCheckPass, T("tunnel.check.detail.free"))
			}
		}
	}
	loginLabel := T("tunnel.check.label.login", plan.Target)
	output, err := probe(plan.Target, "true")
	if err != nil {
		add("login", loginLabel, tunnelCheckFail, tunnelProbeDetail(output, err))
		skipped := T("tunnel.check.detail.skipped")
		switch plan.Kind {
		case tunnelLocal:
			add("destination", T("tunnel.check.label.destination", plan.Destination, plan.Host), tunnelCheckSkip, skipped)
		case tunnelReverse:
			add("remote_port", T("tunnel.check.label.remote_port", plan.Host, plan.Port), tunnelCheckSkip, skipped)
		}
	} else {
		add("login", loginLabel, tunnelCheckPass, "")
		switch plan.Kind {
		case tunnelLocal:
			label := T("tunnel.check.label.destination", plan.Destination, plan.Host)
			if output, err := probe("-W", plan.Destination, plan.Target); err != nil {
				add("destination", label, tunnelCheckFail, tunnelProbeDetail(output, err))
			} else {
				add("destination", label, tunnelCheckPass, T("tunnel.check.detail.reachable"))
			}
		case tunnelReverse:
			label := T("tunnel.check.label.remote_port", plan.Host, plan.Port)
			if isRunning {
				add("remote_port", label, tunnelCheckPass, T("tunnel.check.detail.running"))
			} else if err := manager.rejectConflicts(ctx, plan); err != nil {
				add("remote_port", label, tunnelCheckFail, err.Error())
			} else if output, err := probe(append(append([]string{"-o", "ExitOnForwardFailure=yes"}, plan.Forward...), plan.Target, "true")...); err != nil {
				add("remote_port", label, tunnelCheckFail, tunnelProbeDetail(output, err))
			} else {
				add("remote_port", label, tunnelCheckPass, T("tunnel.check.detail.bindable"))
			}
		}
	}
	if plan.Kind == tunnelReverse {
		label := T("tunnel.check.label.target", plan.Destination)
		if manager.dial(plan.Destination) != nil {
			add("target", label, tunnelCheckWarn, T("tunnel.check.detail.not_listening"))
		} else {
			add("target", label, tunnelCheckPass, T("tunnel.check.detail.listening"))
		}
	}
	return checks, nil
}

func tunnelFailedChecks(checks []tunnelCheck) int {
	failed := 0
	for _, check := range checks {
		if check.Status == tunnelCheckFail {
			failed++
		}
	}
	return failed
}

func tunnelCheckMark(status tunnelCheckStatus) string {
	switch status {
	case tunnelCheckPass:
		return "✔"
	case tunnelCheckFail:
		return "✖"
	case tunnelCheckWarn:
		return "!"
	default:
		return "–"
	}
}

func tunnelCheckLine(check tunnelCheck) string {
	line := tunnelCheckMark(check.Status) + " " + check.Label
	if check.Detail != "" {
		line += "  " + check.Detail
	}
	return line
}

func tunnelCheckSummary(checks []tunnelCheck) string {
	if failed := tunnelFailedChecks(checks); failed > 0 {
		return T("tunnel.check.summary_fail", failed)
	}
	for _, check := range checks {
		if check.Status == tunnelCheckWarn {
			return T("tunnel.check.summary_warn")
		}
	}
	return T("tunnel.check.summary_ok")
}
