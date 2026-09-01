package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

const unsupportedOSReason = "현재 macOS와 Linux만 지원합니다"

func probeInterfaces() Result {
	started := time.Now()
	interfaces, err := net.Interfaces()
	if err != nil {
		return resultFromError("net.interfaces", started, "system", err)
	}
	rows := make([]map[string]interface{}, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses, _ := iface.Addrs()
		values := make([]string, 0, len(addresses))
		for _, address := range addresses {
			values = append(values, address.String())
		}
		rows = append(rows, map[string]interface{}{"name": iface.Name, "mtu": iface.MTU, "flags": iface.Flags.String(), "addresses": values})
	}
	return Result{Probe: "net.interfaces", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: fmt.Sprintf("interface %d개", len(rows)), Metrics: map[string]interface{}{"interfaces": rows}}
}

// commandOutput은 stdout과 stderr를 합쳐 1MB까지만 남기고 앞뒤 공백을 제거한다.
func commandOutput(ctx context.Context, path string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if len(output) > 1<<20 {
		output = output[:1<<20]
	}
	return strings.TrimSpace(string(output)), err
}

func probeCommand(ctx context.Context, probe, path string, args ...string) Result {
	started := time.Now()
	if _, err := exec.LookPath(path); err != nil {
		return Result{Probe: probe, Status: StatusSkip, StartedAt: started.UTC(), Summary: path + " 명령을 찾을 수 없습니다"}
	}
	text, err := commandOutput(ctx, path, args...)
	if err != nil {
		return resultFromError(probe, started, classifyCommandError(ctx, err), fmt.Errorf("%s: %s", err, text))
	}
	return Result{Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: firstLine(text), Evidence: []Evidence{{Label: "output", Value: text}}}
}

func probeRoute(ctx context.Context, target string) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "net.route", "/sbin/route", "-n", "get", target)
	case "linux":
		// ip route get은 host 이름을 받지 않으므로 먼저 주소로 바꾼다.
		address, err := resolveRouteAddress(ctx, target)
		if err != nil {
			return resultFromError("net.route", time.Now(), "dns", err)
		}
		return probeCommand(ctx, "net.route", "ip", "route", "get", address)
	default:
		return unsupported("net.route", unsupportedOSReason)
	}
}

// resolveRouteAddress는 IPv4 주소를 우선 고른다. 주소 literal은 그대로 돌려준다.
func resolveRouteAddress(ctx context.Context, target string) (string, error) {
	if net.ParseIP(target) != nil {
		return target, nil
	}
	addresses, err := net.DefaultResolver.LookupHost(ctx, target)
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", fmt.Errorf("%s의 주소를 찾을 수 없습니다", target)
	}
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
			return address, nil
		}
	}
	return addresses[0], nil
}

func probeDNSConfig(ctx context.Context) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "dns.config", "/usr/sbin/scutil", "--dns")
	case "linux":
		return probeResolvConf(ctx, "/etc/resolv.conf")
	default:
		return unsupported("dns.config", unsupportedOSReason)
	}
}

type resolvConfig struct {
	Nameservers []string
	Search      []string
}

// parseResolvConf는 nameserver, search, domain 줄만 읽는다. 나머지는 evidence 원문으로 남긴다.
func parseResolvConf(text string) resolvConfig {
	config := resolvConfig{Nameservers: []string{}, Search: []string{}}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			config.Nameservers = append(config.Nameservers, fields[1])
		case "search", "domain":
			config.Search = append(config.Search, fields[1:]...)
		}
	}
	return config
}

func probeResolvConf(ctx context.Context, path string) Result {
	started := time.Now()
	data, err := os.ReadFile(path)
	if err != nil {
		return resultFromError("dns.config", started, "system", err)
	}
	text := strings.TrimSpace(string(data))
	config := parseResolvConf(text)
	result := Result{
		Probe: "dns.config", Status: StatusPass, StartedAt: started.UTC(),
		Summary:  "nameserver " + strings.Join(config.Nameservers, ", "),
		Metrics:  map[string]interface{}{"path": path, "nameservers": config.Nameservers, "search": config.Search},
		Evidence: []Evidence{{Label: path, Value: text}},
	}
	if len(config.Nameservers) == 0 {
		result.Status = StatusWarn
		result.Summary = path + "에 nameserver가 없습니다"
		result.Warnings = append(result.Warnings, "nameserver 줄이 없으면 resolver는 localhost를 사용합니다")
	}
	// systemd-resolved는 resolv.conf에 stub 주소만 남기므로 실제 upstream은 resolvectl에서 읽는다.
	if _, err := exec.LookPath("resolvectl"); err == nil {
		output, err := commandOutput(ctx, "resolvectl", "status")
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("resolvectl status 실패: %s", firstLine(output)))
		} else {
			result.Evidence = append(result.Evidence, Evidence{Label: "resolvectl status", Value: output})
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

func probeSockets(ctx context.Context) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "sockets", "/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN")
	case "linux":
		return probeCommand(ctx, "sockets", "ss", "-tlnp")
	default:
		return unsupported("sockets", unsupportedOSReason)
	}
}

func probeQuality(ctx context.Context) Result {
	started := time.Now()
	if runtime.GOOS != "darwin" {
		return unsupported("net.quality", "networkQuality는 macOS 전용입니다")
	}
	command := exec.CommandContext(ctx, "/usr/bin/networkQuality", "-c")
	output, err := command.Output()
	if err != nil {
		return resultFromError("net.quality", started, classifyCommandError(ctx, err), err)
	}
	var metrics map[string]interface{}
	if err := json.Unmarshal(output, &metrics); err != nil {
		return resultFromError("net.quality", started, "parse", err)
	}
	return Result{Probe: "net.quality", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: "networkQuality 측정 완료", Metrics: metrics}
}

func probePing(ctx context.Context, target string) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "net.ping", "/sbin/ping", "-n", "-c", "4", "-W", "2000", target)
	case "linux":
		// Linux ping의 -W 단위는 초다.
		return probeCommand(ctx, "net.ping", "ping", "-n", "-c", "4", "-W", "2", target)
	default:
		return unsupported("net.ping", unsupportedOSReason)
	}
}

func probeTrace(ctx context.Context, target string) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "net.trace", "/usr/sbin/traceroute", "-n", "-m", "20", "-w", "2", target)
	case "linux":
		path, args, found := linuxTraceCommand(exec.LookPath, target)
		if !found {
			return Result{Probe: "net.trace", Status: StatusSkip, StartedAt: time.Now().UTC(), Summary: "traceroute 또는 tracepath 명령을 찾을 수 없습니다"}
		}
		return probeCommand(ctx, "net.trace", path, args...)
	default:
		return unsupported("net.trace", unsupportedOSReason)
	}
}

// linuxTraceCommand는 traceroute를 우선 쓰고, 없으면 iputils의 tracepath를 쓴다.
func linuxTraceCommand(lookPath func(string) (string, error), target string) (string, []string, bool) {
	if _, err := lookPath("traceroute"); err == nil {
		return "traceroute", []string{"-n", "-m", "20", "-w", "2", target}, true
	}
	if _, err := lookPath("tracepath"); err == nil {
		return "tracepath", []string{"-n", "-m", "20", target}, true
	}
	return "", nil, false
}

func unsupported(probe, reason string) Result {
	return Result{Probe: probe, Status: StatusSkip, StartedAt: time.Now().UTC(), Summary: reason}
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	if len(line) > 160 {
		return line[:160] + "…"
	}
	return line
}

func classifyCommandError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("exit_%d", exit.ExitCode())
	}
	return "command"
}

func sortResults(results []Result) {
	sort.Slice(results, func(i, j int) bool { return results[i].Probe < results[j].Probe })
}
