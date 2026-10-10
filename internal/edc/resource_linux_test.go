//go:build linux

package edc

import "testing"

// iowait이 유효하지 않으면 blocked 경고가 iowait 없이 켜진다. Linux는 /proc/stat의 iowait 조건을 지켜야 한다.
func TestCollectResourceSnapshotReadsIOWaitOnLinux(t *testing.T) {
	snapshot, err := collectResourceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.CPUIOWaitValid || !snapshot.ProcsBlockedValid || snapshot.MemoryPressureValid {
		t.Fatalf("iowait %v, procs_blocked %v, memory pressure %v", snapshot.CPUIOWaitValid, snapshot.ProcsBlockedValid, snapshot.MemoryPressureValid)
	}
}
