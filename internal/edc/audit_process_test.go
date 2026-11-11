package edc

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestAuditExeReason(t *testing.T) {
	for exe, want := range map[string]string{
		"/usr/sbin/sshd":                              "",
		"/usr/bin/python3.13 (deleted)":               "",
		"/tmp/x/release/term-meshd (deleted)":         "temp",
		"/dev/shm/.k":                                 "temp",
		"/var/tmp/agent":                              "temp",
		"/root/.local/bin/edc (deleted)":              "deleted",
		"/sbin/agetty (deleted)":                      "",
		"/lib/systemd/systemd-logind (deleted)":       "",
		"/opt/app/bin/app":                            "",
		"/memfd: (deleted)":                           "memfd",
		"/memfd:runc_cloned:/proc/self/exe (deleted)": "",
		"/tmpfoo/bin":                                 "",
	} {
		if got := auditExeReason(exe); got != want {
			t.Errorf("%q = %q, want %q", exe, got, want)
		}
	}
}

func TestCollectAuditProcesses(t *testing.T) {
	root := t.TempDir()
	for pid, exe := range map[int]string{1: "/usr/lib/systemd/systemd", 40: "/dev/shm/.k (deleted)", 7: "/tmp/miner"} {
		dir := filepath.Join(root, "proc", strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		writeAuditFile(t, root, "/proc/"+strconv.Itoa(pid)+"/comm", "p"+strconv.Itoa(pid)+"\n", 0o644)
	}
	// kernel thread에는 exe link가 없다.
	if err := os.MkdirAll(filepath.Join(root, "proc", "2"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAuditFile(t, root, "/proc/sys/kernel/pid_max", "4194304\n", 0o644)
	processes := collectAuditProcesses(auditSources{root: root})
	want := []auditProcess{
		{PID: 7, Comm: "p7", Exe: "/tmp/miner", Reason: "temp"},
		{PID: 40, Comm: "p40", Exe: "/dev/shm/.k (deleted)", Reason: "temp"},
	}
	if processes.Err != nil || processes.Checked != 3 || !reflect.DeepEqual(processes.Suspicious, want) {
		t.Fatalf("processes = %+v", processes)
	}
	result := auditProcessExe(auditFacts{Processes: processes})
	if result.Status != StatusWarn || !strings.HasPrefix(result.Evidence[0].Value, "7 p7  /tmp/miner  "+T("audit.proc.reason.temp")) || !strings.Contains(result.Next, "/proc/7/exe") {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuditProcessExeReportsUnreadableAndCapsEvidence(t *testing.T) {
	processes := auditProcesses{Checked: 30, Unreadable: 4}
	for pid := 1; pid <= auditProcessShown+2; pid++ {
		processes.Suspicious = append(processes.Suspicious, auditProcess{PID: pid, Comm: "x", Exe: "/tmp/x", Reason: "memfd"})
	}
	result := auditProcessExe(auditFacts{Processes: processes})
	lines := strings.Split(result.Evidence[0].Value, "\n")
	if len(lines) != auditProcessShown+1 || lines[len(lines)-1] != "+2" || !reflect.DeepEqual(result.Warnings, []string{T("audit.proc.unreadable", 4)}) {
		t.Fatalf("result = %+v", result)
	}
	if result := auditProcessExe(auditFacts{Processes: auditProcesses{Checked: 3}}); result.Status != StatusPass {
		t.Fatalf("pass = %+v", result)
	}
}

func writeTestProcess(t *testing.T, root string, pid int, exe, maps, environ string) {
	t.Helper()
	dir := filepath.Join(root, "proc", strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	writeAuditFile(t, root, "/proc/"+strconv.Itoa(pid)+"/comm", "p"+strconv.Itoa(pid)+"\n", 0o644)
	writeAuditFile(t, root, "/proc/"+strconv.Itoa(pid)+"/maps", maps, 0o644)
	writeAuditFile(t, root, "/proc/"+strconv.Itoa(pid)+"/environ", environ, 0o644)
}

func TestCollectAuditProcessesFindsDeletedLibrariesAndPreload(t *testing.T) {
	root := t.TempDir()
	maps := strings.Join([]string{
		"7f00-7f01 r-xp 00000000 08:01 123 /usr/lib/x86_64-linux-gnu/libssl.so.3 (deleted)",
		"7f02-7f03 r-xp 00000000 08:01 124 /var/tmp/.x/libhide.so (deleted)",
		"7f04-7f05 r--p 00001000 08:01 124 /var/tmp/.x/libhide.so (deleted)",
		"7f06-7f07 r-xp 00000000 08:01 125 /opt/app/lib/libapp.so.1",
		"7f08-7f09 rw-s 00000000 00:01 126 /memfd:shm.so (deleted)",
		"7f0a-7f0b r--p 00000000 08:01 127 /home/a/data.bin (deleted)",
	}, "\n") + "\n"
	writeTestProcess(t, root, 10, "/usr/sbin/sshd", maps, "PATH=/usr/bin\x00LD_PRELOAD=/var/tmp/.x/libhide.so\x00")
	writeTestProcess(t, root, 11, "/usr/bin/bash", "", "LD_PRELOAD=\x00HOME=/root\x00")
	writeAuditFile(t, root, "/etc/ld.so.preload", "# comment\n/lib/libsnoopy.so\n", 0o644)
	processes := collectAuditProcesses(auditSources{root: root})
	if want := []auditProcess{{PID: 10, Comm: "p10", Exe: "/var/tmp/.x/libhide.so", Reason: "deleted_lib"}}; !reflect.DeepEqual(processes.Suspicious, want) {
		t.Fatalf("suspicious = %+v", processes.Suspicious)
	}
	if want := []auditProcess{{PID: 10, Comm: "p10", Exe: "/var/tmp/.x/libhide.so"}}; !reflect.DeepEqual(processes.Preloads, want) {
		t.Fatalf("preloads = %+v", processes.Preloads)
	}
	if !reflect.DeepEqual(processes.PreloadFile, []string{"/lib/libsnoopy.so"}) {
		t.Fatalf("preload file = %v", processes.PreloadFile)
	}
	result := auditProcessPreload(auditFacts{Processes: processes})
	if result.Status != StatusWarn || result.Evidence[0].Value != "/etc/ld.so.preload  /lib/libsnoopy.so\n10 p10  LD_PRELOAD=/var/tmp/.x/libhide.so" {
		t.Fatalf("result = %+v", result)
	}
	if result := auditProcessExe(auditFacts{Processes: processes}); result.Summary != T("audit.proc.exe_bad", 1) {
		t.Fatalf("exe = %+v", result)
	}
	if result := auditProcessPreload(auditFacts{Processes: auditProcesses{Checked: 2}}); result.Status != StatusPass {
		t.Fatalf("pass = %+v", result)
	}
}
