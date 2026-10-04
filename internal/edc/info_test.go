package edc

import (
	"errors"
	"strings"
	"testing"
)

func TestInfoResourcesRanksRSSWithoutChangingTheProcessList(t *testing.T) {
	processes := []topProcess{{PID: 1, CPU: 100, RSS: 1 << 20, Threads: 2, Command: "cpu-heavy"}, {PID: 2, RSS: 4 << 30, Threads: 3, Command: "/apps/memory-heavy"}, {PID: 3, RSS: 2 << 30, Command: "second"}, {PID: 4, RSS: 1 << 30, Command: "third"}}
	var output strings.Builder
	printInfoResources(&output, infoMemory{Total: 8 << 30, Available: 3 << 30, Basis: "test estimate", Details: []infoMemoryDetail{{"Cache", 1 << 30}}}, nil, processes, nil, []infoCapability{{"CPU wait / I/O latency", "permission required", "missing CAP_BPF\nCAP_PERFMON"}}, true)
	text := output.String()
	for _, want := range []string{"Memory Status", "Available: 3.00 GB", "Basis: test estimate", "Cache: 1.00 GB", "Processes observed: 4", "Threads observed: 5 (2/4 processes)", "memory-heavy", "permission required", "missing CAP_BPF CAP_PERFMON"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "cpu-heavy") || strings.Contains(text, "\033") || processes[0].PID != 1 {
		t.Fatalf("only RSS leaders must appear, without color or input mutation: %s", text)
	}
	if strings.Index(text, "memory-heavy") > strings.Index(text, "second") || strings.Index(text, "second") > strings.Index(text, "third") {
		t.Fatalf("RSS order is incorrect: %s", text)
	}
}

func TestInfoResourcesDistinguishesErrorsAndUnsupportedFeatures(t *testing.T) {
	var output strings.Builder
	printInfoResources(&output, infoMemory{}, errors.New("Mach read failed"), nil, errors.New("process read failed"), []infoCapability{{"PSI", "unsupported", "Linux-only"}}, false)
	text := output.String()
	for _, want := range []string{"Unavailable: Mach read failed", "Unavailable: process read failed", "PSI: unsupported"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "Used:") || strings.Contains(text, "Processes observed: 0") || strings.Contains(text, "NaN") {
		t.Fatalf("unreadable data must not look like zero usage: %s", text)
	}
}

func TestInfoSnapshotCollectorsReadLocalData(t *testing.T) {
	memory, err := collectInfoMemory()
	if err != nil || memory.Total == 0 || memory.Available > memory.Total || memory.Basis == "" {
		t.Fatalf("memory = %+v, %v", memory, err)
	}
	processes, err := collectInfoProcesses()
	if err != nil || len(processes) == 0 {
		t.Fatalf("processes = %d, %v", len(processes), err)
	}
	if capabilities := collectInfoCapabilities(); len(capabilities) != 3 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestInfoCompactMemoryKeepsValuesAndMovesExplanationToVerbose(t *testing.T) {
	memory := infoMemory{Total: 8 << 30, Available: 3 << 30, Basis: "availability estimate, not pressure", Details: []infoMemoryDetail{{"File-backed", 1 << 30}, {"Wired", 2 << 30}, {"Compressed physical", 0}, {"Compressed original", 0}}}
	var compact, verbose strings.Builder
	printInfoResources(&compact, memory, nil, nil, nil, nil, false)
	printInfoResources(&verbose, memory, nil, nil, nil, nil, true)
	section := strings.Split(strings.Split(compact.String(), "Memory Status\n")[1], "\n\n")[0]
	if len(strings.Split(section, "\n")) != 3 {
		t.Fatalf("compact memory must use three value rows: %q", section)
	}
	for _, want := range []string{"Used: 5.00 GB / 8.00 GB", "Available: 3.00 GB", "File-backed: 1.00 GB", "Wired: 2.00 GB", "Compressed physical: 0.00 GB", "Compressed original: 0.00 GB"} {
		if !strings.Contains(section, want) {
			t.Fatalf("compact output lost %q: %q", want, section)
		}
	}
	if strings.Contains(compact.String(), "Basis:") || !strings.Contains(verbose.String(), "Basis: availability estimate, not pressure") || !strings.Contains(verbose.String(), "counters can overlap") {
		t.Fatal("verbose must retain the calculation and overlap notes")
	}
}

func TestInfoCompactSystemKeepsHostIdentityAndVerboseKernel(t *testing.T) {
	details := hostDetails{Hostname: "host", System: "darwin", OS: "macOS", Version: "27", Machine: "arm64", Processor: "long kernel build string", Release: "27.0", Model: "M4", Cores: 16, MemoryTotal: 64 << 30}
	public := &publicNetworkInfo{IP: "192.0.2.1", City: "city", Region: "region", Country: "country", Timezone: "zone", Org: "provider"}
	var compact, verbose strings.Builder
	printInfo(&compact, "test", details, nil, nil, public, false, false)
	printInfo(&verbose, "test", details, nil, nil, public, false, true)
	for _, want := range []string{"host", "macOS 27", "arm64", "M4", "16 cores", "64.00 GB", "Files (edc soft/hard)", "Swap:", "192.0.2.1", "city", "region", "country", "zone", "provider"} {
		if !strings.Contains(compact.String(), want) {
			t.Fatalf("compact system lost %q", want)
		}
	}
	if strings.Contains(compact.String(), "Description :") || strings.Contains(compact.String(), details.Processor) || !strings.Contains(verbose.String(), details.Processor) || !strings.Contains(verbose.String(), details.Release) {
		t.Fatal("verbose must retain the full kernel data")
	}
}
