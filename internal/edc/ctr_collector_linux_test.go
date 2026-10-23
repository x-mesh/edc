//go:build linux

package edc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCtrCgroupMatchesFullContainerIDOnly(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, path := range []string{"/docker/" + id, "/system.slice/docker-" + id + ".scope", "/" + id + ".scope"} {
		if !ctrCgroupMatchesID(path, id) {
			t.Errorf("did not match %q", path)
		}
	}
	for _, path := range []string{"/docker/prefix" + id, "/docker/" + id[:63], "/other/" + id[:32]} {
		if ctrCgroupMatchesID(path, id) {
			t.Errorf("matched non-exact ID in %q", path)
		}
	}
}

func TestParseCtrProcNetAddressByteOrder(t *testing.T) {
	for _, test := range []struct {
		address string
		want    string
	}{
		{"0100007F:1F90", "127.0.0.1"},
		{"00000000000000000000000001000000:0050", "::1"},
	} {
		address, port, err := parseCtrProcAddress(test.address)
		if err != nil || address != test.want {
			t.Errorf("parseCtrProcAddress(%q) = %q:%d, %v", test.address, address, port, err)
		}
	}
}

func TestParseCtrProcNetTableFiltersAndRejectsMalformedRows(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tcp")
	content := "sl local_address rem_address st tx_queue tr tm->when retrnsmt uid timeout inode\n" +
		"0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 321\n" +
		"1: 00000000:1F90 00000000:0000 01 00000000:00000000 00:00000000 00000000 1000 0 322\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := parseCtrProcNetTable(path, "tcp", time.Now().Add(time.Second))
	if err != nil || len(rows) != 1 || rows[0].Inode != 321 || rows[0].Port != 8080 || rows[0].Address != "127.0.0.1" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if err := os.WriteFile(path, []byte(content+"bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = parseCtrProcNetTable(path, "tcp", time.Now().Add(time.Second)); err == nil {
		t.Fatal("accepted malformed proc net row")
	}
}

func TestCtrCollectorBoundedReadStopsAfterLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCtrCollectorBounded(path, 4096); err == nil {
		t.Fatal("accepted oversized file")
	}
}

func TestCtrCollectorFDLimitAcceptsEmptyDescriptorDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "limits"), []byte("Max open files 16 64 files\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ctrCollectorFDLimitForPID(base)
	if got.Status != "available" || got.Used == nil || *got.Used != 0 || got.Soft == nil || *got.Soft != 16 {
		t.Fatalf("empty FD directory result = %+v", got)
	}
}

func TestCtrCollectorOOMRejectsDuplicateAndOversizedCounters(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "memory.events")
	if err := os.WriteFile(path, []byte("oom 1\noom_kill 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	OOM, OOMKill, status := ctrCollectorOOM(directory)
	if status.Status != "available" || OOM == nil || *OOM != 1 || OOMKill == nil || *OOMKill != 2 {
		t.Fatalf("valid counters = %v %v %+v", OOM, OOMKill, status)
	}
	if err := os.WriteFile(path, []byte("oom 1\noom 2\noom_kill 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, status = ctrCollectorOOM(directory); status.Status != "error" {
		t.Fatalf("duplicate counter status = %+v", status)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, status = ctrCollectorOOM(directory); status.Status != "error" {
		t.Fatalf("oversized counter status = %+v", status)
	}
}

func TestCtrCollectorProcessIdentityRejectsPIDReuseAndOutsideScope(t *testing.T) {
	root := "/host/cgroup/system.slice/docker-" + strings.Repeat("c", 64) + ".scope"
	member := "/system.slice/docker-" + strings.Repeat("c", 64) + ".scope/child"
	if !ctrCollectorProcessStable(10, 10, member, root) {
		t.Fatal("accepted a stable PID in the target cgroup as invalid")
	}
	if ctrCollectorProcessStable(10, 11, member, root) {
		t.Fatal("accepted a changed PID start time")
	}
	if ctrCollectorProcessStable(10, 10, "/system.slice/other", root) {
		t.Fatal("accepted a PID outside the target cgroup")
	}
}

func TestCtrCollectorCgroupDirectoryBoundDoesNotRegisterPastLimit(t *testing.T) {
	root := t.TempDir()
	for index := range ctrCollectorProcessLimit {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("child-%04d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, partial, err := ctrCollectorProcesses(root, time.Now().Add(10*time.Second))
	if !partial || err == nil {
		t.Fatalf("limited cgroup traversal partial=%t err=%v", partial, err)
	}
}
