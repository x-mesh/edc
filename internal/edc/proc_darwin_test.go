//go:build darwin

package edc

import (
	"os/exec"
	"testing"
	"time"
)

func TestParseLsofFieldsCountsOnlyOpenFDs(t *testing.T) {
	output := "p990\nfcwd\ntDIR\nn/\nftxt\ntREG\nn/usr/libexec/app\nf3\ntREG\nn/var/log/app.log\nf4\ntIPv6\nn*:8080\nTST=LISTEN\nf5\ntIPv4\nn*:8080\nTST=LISTEN\nf6\ntIPv4\nn10.0.0.5:8080->10.0.0.9:51234\nTST=ESTABLISHED\nf7\ntIPv4\nn10.0.0.5:40000->10.0.0.7:5432\nTST=CLOSE_WAIT\n"
	var details procDetails
	parseLsofFields(output, &details)
	if details.FDTypes["REG"] != 1 || details.FDTypes["DIR"] != 0 || details.FDTypes["IPv4"] != 3 || details.FDTypes["IPv6"] != 1 {
		t.Fatalf("types = %v", details.FDTypes)
	}
	if len(details.Listen) != 2 || details.Conns["ESTABLISHED"] != 1 || details.Conns["CLOSE_WAIT"] != 1 {
		t.Fatalf("sockets = %+v", details)
	}
	if details.Remotes["10.0.0.9"] != 1 || details.Remotes["10.0.0.7:5432"] != 1 {
		t.Fatalf("remotes = %v", details.Remotes)
	}
}

// macOS에서는 kern.proc.pid의 시작 시각으로 같은 process인지 가른다. 거두기 전의 zombie도 시작 시각이 남고, 거두면 읽히지 않는다.
func TestProcHandleFollowsTheStartTimeOnMacOS(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	pid := child.Process.Pid
	handle, ok := openProcHandle(pid)
	start, _, _ := darwinProcessStart(pid)
	if !ok {
		child.Process.Kill()
		child.Wait()
		t.Fatal("the child must open")
	}
	process := topProcess{PID: pid, State: "S", Started: time.Unix(start.Sec, 0)}
	if !handle.sameProcess(process) {
		t.Fatal("a running child must pass")
	}
	// 표본의 시작 시각은 ps의 초 단위 값이다. 다른 초에 시작한 process는 같은 process가 아니다.
	if handle.sameProcess(topProcess{PID: pid, State: "S", Started: time.Unix(start.Sec-1, 0)}) {
		t.Fatal("another start second means another process")
	}
	child.Process.Kill()
	zombie := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !zombie; time.Sleep(10 * time.Millisecond) {
		_, zombie, _ = darwinProcessStart(pid)
	}
	if !zombie {
		child.Wait()
		t.Fatal("the killed child did not become a zombie")
	}
	if handle.sameProcess(process) {
		t.Fatal("a child that became a zombie must not pass")
	}
	process.State = "Z"
	if !handle.sameProcess(process) {
		t.Fatal("a zombie in the sample must pass")
	}
	child.Wait()
	if handle.sameProcess(process) {
		t.Fatal("a reaped child must not pass")
	}
	if _, ok := openProcHandle(pid); ok {
		t.Fatal("a reaped child must not open")
	}
}
