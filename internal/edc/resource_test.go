package edc

import (
	"strings"
	"testing"
	"time"
)

func TestCalculateRate(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, CPUUser: 10, CPUSystem: 5, CPUIOWait: 2, CPUTotal: 100, NetInBytes: 1000, NetOutBytes: 2000, PacketsIn: 10, PacketsOut: 20, DiskRead: 100, DiskWrite: 200}
	current := resourceSnapshot{TakenAt: start.Add(2 * time.Second), CPUUser: 30, CPUSystem: 15, CPUIOWait: 4, CPUTotal: 200, NetInBytes: 3000, NetOutBytes: 6000, PacketsIn: 30, PacketsOut: 60, DiskRead: 2100, DiskWrite: 4200, MemoryUsed: 25, MemoryTotal: 100, Load1: 1.5}
	rate := calculateRate(previous, current)
	if rate.NetIn != 1000 || rate.NetOut != 2000 || rate.PacketsIn != 10 || rate.DiskRead != 1000 {
		t.Fatalf("unexpected rate: %#v", rate)
	}
	if rate.CPUUser != 20 || rate.CPUSystem != 10 || rate.CPUIOWait != 2 || rate.MemoryPercent != 25 {
		t.Fatalf("unexpected percent: %#v", rate)
	}
}

func TestCalculateInstantCPU(t *testing.T) {
	previous := resourceSnapshot{TakenAt: time.Now()}
	current := resourceSnapshot{TakenAt: previous.TakenAt.Add(time.Second), CPUInstant: true, CPUUser: 1234, CPUSystem: 567, CPUIOWait: 89}
	rate := calculateRate(previous, current)
	if rate.CPUUser != 12.34 || rate.CPUSystem != 5.67 || rate.CPUIOWait != .89 {
		t.Fatalf("instant CPU = %#v", rate)
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := formatBytes(1024 * 1024 * 1024); got != "1.00 GB" {
		t.Fatalf("formatBytes = %s", got)
	}
	if got := formatDuration(49*time.Hour + 3*time.Minute); got != "2 days, 1 hours, 3 minutes" {
		t.Fatalf("formatDuration = %s", got)
	}
}

func TestVisibleDisks(t *testing.T) {
	disks := []diskDetails{{Mount: "/", Total: 100}, {Mount: "/System/Volumes/VM", Total: 100}, {Mount: t.TempDir(), Total: 100}, {Mount: "/etc/hosts", Total: 100}}
	visible := visibleDisks(disks)
	if len(visible) != 2 || visible[1].Mount == "/etc/hosts" {
		t.Fatalf("visible disks = %#v", visible)
	}
}

func TestPrintTopRow(t *testing.T) {
	var output strings.Builder
	printTopRow(&output, time.Date(2026, 1, 1, 11, 36, 44, 0, time.UTC), resourceRate{NetIn: 0.04 * 1024 * 1024, CPUUser: 1, MemoryPercent: 11.8})
	if !strings.Contains(output.String(), "11:36:44") || !strings.Contains(output.String(), "0.04M") || !strings.Contains(output.String(), "11.8%") {
		t.Fatalf("row = %s", output.String())
	}
}
