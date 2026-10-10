//go:build linux

package edc

import "testing"

// Linux의 blocked는 kernel의 procs_blocked다. process 목록에서 센 값으로 표시되면 blocked 경고가 iowait 없이 켜진다.
func TestCollectResourceSnapshotReadsKernelBlockedOnLinux(t *testing.T) {
	snapshot, err := collectResourceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.ProcsBlockedValid || snapshot.ProcsBlockedFromProcesses || snapshot.MemoryPressure.known() {
		t.Fatalf("procs_blocked %v, from processes %v, memory pressure %v", snapshot.ProcsBlockedValid, snapshot.ProcsBlockedFromProcesses, snapshot.MemoryPressure)
	}
	fillProcsBlocked(&snapshot)
	if snapshot.ProcsBlockedFromProcesses {
		t.Fatal("the kernel count must stay the kernel count")
	}
}
