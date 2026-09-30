//go:build linux

package edc

import (
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDropTraceEventFormatsTheAddresses(t *testing.T) {
	record, _ := parseDropRecord(dropSample(3, 4, []byte{10, 0, 0, 9}, []byte{10, 0, 0, 5}, 40022, 9999))
	event := dropTraceEvent(record, 0, "NO_SOCKET", "tcp_v4_rcv", dropOwner{pid: 7, process: "nginx", cgroup: 99})
	if event.Protocol != "drop" || event.Event != "drop_no_socket" || event.Reason != "NO_SOCKET" || event.Location != "tcp_v4_rcv" || event.Source != "10.0.0.9:40022" || event.Destination != "10.0.0.5:9999" || event.Bytes != 60 || event.PID != 7 || event.Process != "nginx" || event.CgroupID != 99 {
		t.Fatalf("event = %+v", event)
	}
	six := make([]byte, 16)
	six[15] = 1
	record, _ = parseDropRecord(dropSample(3, 6, six, six, 1, 2))
	if event := dropTraceEvent(record, 0, "NO_SOCKET", "", dropOwner{}); event.Destination != "[::1]:2" {
		t.Fatalf("IPv6 destination = %q", event.Destination)
	}
	record, _ = parseDropRecord(dropSample(2, 0, nil, nil, 0, 0))
	record.protocol = 0x0004
	if event := dropTraceEvent(record, 0, "NOT_SPECIFIED", "br_stp_rcv", dropOwner{}); event.Destination != "" || event.Target != "llc" {
		t.Fatalf("non-IP event = %+v", event)
	}
	record.protocol = 0x1234
	if event := dropTraceEvent(record, 0, "NOT_SPECIFIED", "", dropOwner{}); event.Target != "ethertype 0x1234" {
		t.Fatalf("unknown ethertype = %q", event.Target)
	}
	// BTF에 이름이 없는 이유도 공백 없는 event 이름이 된다. group 보기의 key로 쓴다.
	if event := dropTraceEvent(record, 0, "reason 99", "", dropOwner{}); event.Event != "drop_reason_99" {
		t.Fatalf("unnamed reason event = %q", event.Event)
	}
}

func TestDropOwnersFindTheProcessOfASocket(t *testing.T) {
	connection, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	file, err := connection.(*net.UDPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	owners := newDropOwners()
	defer owners.close()
	if owner := owners.lookup(stat.Ino, time.Now()); owner.pid != 0 {
		t.Fatalf("first lookup = %+v", owner)
	}
	deadline := time.Now().Add(time.Second)
	owner := dropOwner{}
	for time.Now().Before(deadline) {
		owner = owners.lookup(stat.Ino, time.Now())
		if owner.pid != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if owner.pid != uint32(os.Getpid()) || owner.process == "" {
		t.Fatalf("owner = %+v, want pid %d", owner, os.Getpid())
	}
	// 찾지 못한 inode는 기록해 두고, 그 사이에는 /proc을 다시 읽지 않는다.
	missing := uint64(1) << 62
	now := time.Now().Add(time.Minute)
	if owner := owners.lookup(missing, now); owner.pid != 0 {
		t.Fatalf("missing inode owner = %+v", owner)
	}
	scanned := owners.scanned
	owners.lookup(missing, now.Add(dropOwnerRescan))
	if !owners.scanned.Equal(scanned) {
		t.Fatal("a recent miss must not rescan /proc")
	}
}

func TestDropOwnerRefreshDoesNotBlockAndCoalescesMisses(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var scans atomic.Int32
	owners := newDropOwnersWithScan(func() map[uint64]uint32 {
		if scans.Add(1) == 1 {
			close(started)
		}
		<-release
		return map[uint64]uint32{}
	})
	defer owners.close()

	now := time.Now().Add(time.Minute)
	returned := make(chan struct{})
	go func() {
		owners.lookup(1, now)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("lookup waited for the scan")
	}
	<-started
	for inode := uint64(2); inode < 100; inode++ {
		now = now.Add(dropOwnerRescan)
		owners.lookup(inode, now)
	}
	if queued := len(owners.refresh); queued != 1 {
		t.Fatalf("queued refreshes = %d", queued)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for scans.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := scans.Load(); got != 2 {
		t.Fatalf("scans = %d", got)
	}
}

func TestTraceReasonOptionNeedsTraceDrop(t *testing.T) {
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"tcp", "--reason", "NO_SOCKET"}, "--reason is not available for trace tcp"},
		{[]string{"drop", "--port", "80"}, "--port is not available for trace drop"},
		{[]string{"drop", "--payload"}, "--payload is not available for trace drop"},
	} {
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(test.args) })
		if code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("trace %q exit = %d, stderr %q", test.args, code, stderr)
		}
	}
}
