//go:build linux

package edc

import "testing"

// Linux의 blocked는 kernel의 procs_blocked다. process 목록에서 센 값으로 표시되면 blocked 경고가 iowait 없이 켜진다.
func TestCollectResourceSnapshotReadsKernelBlockedOnLinux(t *testing.T) {
	snapshot, err := collectResourceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ProcsBlockedSource != topBlockedKernelTasks || snapshot.MemoryPressure.known() {
		t.Fatalf("procs_blocked from %v, memory pressure %v", snapshot.ProcsBlockedSource, snapshot.MemoryPressure)
	}
	fillProcsBlocked(&snapshot)
	if snapshot.ProcsBlockedSource != topBlockedKernelTasks {
		t.Fatal("the kernel count must stay the kernel count")
	}
}
