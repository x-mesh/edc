package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

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

func probeCommand(ctx context.Context, probe, path string, args ...string) Result {
	started := time.Now()
	if _, err := exec.LookPath(path); err != nil {
		return Result{Probe: probe, Status: StatusSkip, StartedAt: started.UTC(), Summary: path + " 명령을 찾을 수 없습니다"}
	}
	command := exec.CommandContext(ctx, path, args...)
	output, err := command.CombinedOutput()
	if len(output) > 1<<20 {
		output = output[:1<<20]
	}
	if err != nil {
		return resultFromError(probe, started, classifyCommandError(ctx, err), fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output))))
	}
	text := strings.TrimSpace(string(output))
	return Result{Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: firstLine(text), Evidence: []Evidence{{Label: "output", Value: text}}}
}

func probeRoute(ctx context.Context, target string) Result {
	if runtime.GOOS != "darwin" {
		return unsupported("net.route", "현재 macOS만 지원합니다")
	}
	return probeCommand(ctx, "net.route", "/sbin/route", "-n", "get", target)
}

func probeDNSConfig(ctx context.Context) Result {
	if runtime.GOOS != "darwin" {
		return unsupported("dns.config", "현재 macOS만 지원합니다")
	}
	return probeCommand(ctx, "dns.config", "/usr/sbin/scutil", "--dns")
}

func probeSockets(ctx context.Context) Result {
	if runtime.GOOS != "darwin" {
		return unsupported("sockets", "현재 macOS만 지원합니다")
	}
	return probeCommand(ctx, "sockets", "/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN")
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
	if runtime.GOOS != "darwin" {
		return unsupported("net.ping", "현재 macOS만 지원합니다")
	}
	return probeCommand(ctx, "net.ping", "/sbin/ping", "-n", "-c", "4", "-W", "2000", target)
}

func probeTrace(ctx context.Context, target string) Result {
	if runtime.GOOS != "darwin" {
		return unsupported("net.trace", "현재 macOS만 지원합니다")
	}
	return probeCommand(ctx, "net.trace", "/usr/sbin/traceroute", "-n", "-m", "20", "-w", "2", target)
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
