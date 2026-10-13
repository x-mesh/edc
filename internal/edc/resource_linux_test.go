//go:build linux

package edc

import (
	"reflect"
	"testing"
)

func TestParseLinuxNetDevSkipsLoopbackAndAddsBothDirections(t *testing.T) {
	data := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9000      90    0    0    0     0          0         0     9000      90    0    0    0     0       0          0
  eth0: 1000      10    1    2    0     0          0         0     2000      20    3    4    0     0       0          0
docker0:  300       3    0    0    0     0          0         0      400       4    0    1    0     0       0          0
`
	want := []netInterfaceCounters{
		{Name: "eth0", BytesIn: 1000, PacketsIn: 10, Errors: 4, Drops: 6, BytesOut: 2000, PacketsOut: 20},
		{Name: "docker0", BytesIn: 300, PacketsIn: 3, Drops: 1, BytesOut: 400, PacketsOut: 4},
	}
	if got := parseLinuxNetDev(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("interfaces = %+v, want %+v", got, want)
	}
}

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
