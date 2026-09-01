package edc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseResolvConf(t *testing.T) {
	text := "# generated\n; comment\nnameserver 10.0.0.1\nnameserver 10.0.0.2\nsearch corp.example internal\ndomain legacy.example\noptions ndots:2\nbroken\n"
	config := parseResolvConf(text)
	if strings.Join(config.Nameservers, ",") != "10.0.0.1,10.0.0.2" {
		t.Fatalf("nameservers = %#v", config.Nameservers)
	}
	if strings.Join(config.Search, ",") != "corp.example,internal,legacy.example" {
		t.Fatalf("search = %#v", config.Search)
	}
	empty := parseResolvConf("")
	if empty.Nameservers == nil || empty.Search == nil {
		t.Fatalf("empty config must keep empty slices for JSON: %#v", empty)
	}
}

func TestProbeResolvConfWarnsWithoutNameserver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("search example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := probeResolvConf(context.Background(), path)
	if result.Status != StatusWarn || !strings.Contains(result.Summary, "nameserver가 없습니다") {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Evidence) == 0 || result.Evidence[0].Label != path {
		t.Fatalf("evidence = %#v", result.Evidence)
	}
	missing := probeResolvConf(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if missing.Status != StatusFail || missing.Error == nil || missing.Error.Kind != "system" {
		t.Fatalf("missing file result = %#v", missing)
	}
}

func TestProbeResolvConfReadsNameservers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.53\nsearch example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := probeResolvConf(context.Background(), path)
	if result.Status != StatusPass || result.Summary != "nameserver 192.0.2.53" {
		t.Fatalf("result = %#v", result)
	}
	if nameservers, ok := result.Metrics["nameservers"].([]string); !ok || len(nameservers) != 1 {
		t.Fatalf("metrics = %#v", result.Metrics)
	}
}

func TestLinuxTraceCommandPrefersTracerouteThenTracepath(t *testing.T) {
	available := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, candidate := range names {
				if candidate == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	path, args, found := linuxTraceCommand(available("traceroute", "tracepath"), "example.com")
	if !found || path != "traceroute" || strings.Join(args, " ") != "-n -m 20 -w 2 example.com" {
		t.Fatalf("traceroute command = %q %v %v", path, args, found)
	}
	path, args, found = linuxTraceCommand(available("tracepath"), "example.com")
	if !found || path != "tracepath" || strings.Join(args, " ") != "-n -m 20 example.com" {
		t.Fatalf("tracepath command = %q %v %v", path, args, found)
	}
	if _, _, found = linuxTraceCommand(available(), "example.com"); found {
		t.Fatal("missing tools must report not found")
	}
}

func TestResolveRouteAddressKeepsLiterals(t *testing.T) {
	for _, literal := range []string{"192.0.2.1", "2001:db8::1"} {
		address, err := resolveRouteAddress(context.Background(), literal)
		if err != nil || address != literal {
			t.Fatalf("resolveRouteAddress(%q) = %q, %v", literal, address, err)
		}
	}
}
