package edc

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
		{PID: 40, Comm: "p40", Exe: "/dev/shm/.k (deleted)", Reason: "temp", FileState: "removed"},
	}
	if processes.Err != nil || processes.Checked != 3 || !reflect.DeepEqual(processes.Suspicious, want) {
		t.Fatalf("processes = %+v", processes)
	}
	result := auditProcessExe(auditFacts{Processes: processes})
	if result.Status != StatusWarn || !strings.HasPrefix(result.Evidence[0].Value, "7 p7  /tmp/miner\n"+auditNotePrefix+T("audit.proc.reason.temp")) || !strings.Contains(result.Next, "/proc/7/exe") {
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
	items := slices.DeleteFunc(slices.Clone(lines), func(line string) bool { return strings.HasPrefix(line, auditDetailPrefix) })
	if len(items) != auditProcessShown+1 || lines[len(lines)-1] != "+2" || !reflect.DeepEqual(result.Warnings, []string{T("audit.proc.unreadable", 4)}) {
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
	if want := []auditProcess{{PID: 10, Comm: "p10", Exe: "/var/tmp/.x/libhide.so", Reason: "deleted_lib", FileState: "removed"}}; !reflect.DeepEqual(processes.Suspicious, want) {
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

func TestAuditProcessPreloadWithoutProc(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/ld.so.preload", "/lib/libsnoopy.so\n", 0o644)
	processes := collectAuditProcesses(auditSources{root: root})
	if processes.Err == nil || !reflect.DeepEqual(processes.PreloadFile, []string{"/lib/libsnoopy.so"}) {
		t.Fatalf("processes = %+v", processes)
	}
	result := auditProcessPreload(auditFacts{Processes: processes})
	if result.Status != StatusWarn || len(result.Warnings) != 1 || strings.HasPrefix(result.Warnings[0], "audit.") {
		t.Fatalf("result = %+v", result)
	}
	// 확인한 것이 없으면 pass가 아니라 skip이다.
	if result := auditProcessPreload(auditFacts{Processes: auditProcesses{Err: processes.Err}}); result.Status != StatusSkip {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectAuditProcessesCountsAnEnvironmentItCannotRead(t *testing.T) {
	root := t.TempDir()
	writeTestProcess(t, root, 10, "/usr/sbin/sshd", "", "HOME=/root\x00")
	writeTestProcess(t, root, 11, "/usr/bin/bash", "", "HOME=/root\x00")
	// 읽을 수 없는 environ은 파일 대신 디렉터리로 흉내 낸다. 권한 오류가 아닌 읽기 오류다.
	environ := filepath.Join(root, "proc/11/environ")
	if err := os.Remove(environ); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(environ, 0o755); err != nil {
		t.Fatal(err)
	}
	processes := collectAuditProcesses(auditSources{root: root})
	if processes.EnvUnreadable != 1 || processes.Checked != 2 {
		t.Fatalf("processes = %+v", processes)
	}
	if result := auditProcessPreload(auditFacts{Processes: processes}); !reflect.DeepEqual(result.Warnings, []string{T("audit.proc.env_unreadable", 1)}) {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

// 근거는 process가 지금 돌고 있다는 것과 언제부터, 누가 띄웠는지, 지워진 파일이 새 파일로 바뀌었는지 사라졌는지를 보인다.
func TestAuditProcessEvidenceDescribesTheState(t *testing.T) {
	root := t.TempDir()
	boot := time.Date(2026, 2, 25, 17, 2, 23, 0, time.Local)
	writeAuditFile(t, root, "/proc/stat", "cpu  1 2 3\nbtime "+strconv.FormatInt(boot.Unix(), 10)+"\n", 0o644)
	// starttime은 부팅 뒤 100분의 1초 단위다. 210일 13시간 20분 14초 뒤, 9월 24일에 시작했다.
	start := (210*24*3600 + 13*3600 + 20*60 + 14) * auditClockTicks
	writeAuditFile(t, root, "/proc/2518273/stat", "2518273 (term-meshd) S 1 2518273 2518273 0 -1 4194560 1 0 0 0 5 6 0 0 20 0 9 0 "+strconv.Itoa(start)+" 1000 50 18446744073709551615\n", 0o644)
	writeAuditFile(t, root, "/proc/1/comm", "systemd\n", 0o644)
	writeAuditFile(t, root, "/tmp/relay/term-meshd", "new", 0o755)
	replacedAt := time.Date(2026, 11, 13, 6, 41, 48, 0, time.Local)
	if err := os.Chtimes(filepath.Join(root, "tmp/relay/term-meshd"), replacedAt, replacedAt); err != nil {
		t.Fatal(err)
	}
	sources := auditSources{root: root}
	replaced := describeAuditProcess(sources, filepath.Join(root, "proc"), readAuditBootTime(filepath.Join(root, "proc/stat")), auditProcess{PID: 2518273, Comm: "term-meshd", Exe: "/tmp/relay/term-meshd (deleted)", Reason: "temp"})
	now := time.Date(2026, 11, 13, 9, 43, 0, 0, time.Local)
	started := boot.Add(time.Duration(start) * time.Second / auditClockTicks)
	want := "2518273 term-meshd  /tmp/relay/term-meshd\n" +
		auditDetailPrefix + T("audit.proc.started_days", auditLoginTime(started), 50) + "  " + T("audit.proc.parent", 1, "systemd") + "\n" +
		auditNotePrefix + T("audit.proc.reason.temp") + "\n" +
		auditNotePrefix + T("audit.proc.file_replaced", auditLoginTime(replacedAt))
	if got := auditProcessLine(replaced, now); got != want {
		t.Fatalf("replaced:\n got %q\nwant %q", got, want)
	}

	// 패키지 디렉터리 밖에서 지워진 binary는 경로의 상태가 이유를 대신한다. 오늘 시작한 process에는 일수를 붙이지 않는다.
	removed := auditProcess{PID: 9, Comm: "x", Exe: "/opt/x (deleted)", Reason: "deleted", FileState: "removed", Started: now.Add(-6 * time.Minute)}
	if got, want := auditProcessLine(removed, now), "9 x  /opt/x\n"+auditDetailPrefix+T("audit.proc.started", auditLoginTime(now.Add(-6*time.Minute)))+"\n"+auditNotePrefix+T("audit.proc.file_removed"); got != want {
		t.Fatalf("removed:\n got %q\nwant %q", got, want)
	}
	// 끝난 process처럼 아무것도 읽지 못하면 원래 이유만 적는다.
	if got, want := auditProcessLine(auditProcess{PID: 7, Comm: "p7", Exe: "/tmp/miner", Reason: "temp"}, now), "7 p7  /tmp/miner\n"+auditNotePrefix+T("audit.proc.reason.temp"); got != want {
		t.Fatalf("bare: got %q", got)
	}
}

// preload 라이브러리의 소속 패키지를 근거에 붙인다. okrr-intranet-slackbot-1의 fluentd는 unified-monitoring-agent의 jemalloc을 썼다.
func TestAuditPreloadNamesTheOwningPackage(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/var/lib/dpkg/info/unified-monitoring-agent.list", "/opt\n/opt/unified-monitoring-agent/embedded/lib/libjemalloc.so\n", 0o644)
	// usrmerge 전에 만든 패키지는 /lib 경로로 적혀 있다. 프로세스는 /usr/lib 경로로 넣는다.
	writeAuditFile(t, root, "/var/lib/dpkg/info/libsnoopy:arm64.list", "/lib/aarch64-linux-gnu/libsnoopy.so.0\n", 0o644)
	writeAuditFile(t, root, "/usr/lib/aarch64-linux-gnu/libsnoopy.so.0", "", 0o644)
	// 버전 없는 symlink를 넣어도 대상 파일로 소속을 찾는다.
	if err := os.Symlink("libsnoopy.so.0", filepath.Join(root, "usr/lib/aarch64-linux-gnu/libsnoopy.so")); err != nil {
		t.Fatal(err)
	}
	libraries := []string{"/opt/unified-monitoring-agent/embedded/lib/libjemalloc.so", "/usr/lib/aarch64-linux-gnu/libsnoopy.so", "/var/tmp/.x/libhide.so"}
	owners, found := auditDpkgOwners(auditSources{root: root}, libraries)
	want := map[string]string{libraries[0]: "unified-monitoring-agent", libraries[1]: "libsnoopy"}
	if !found || !reflect.DeepEqual(owners, want) {
		t.Fatalf("owners = %v, found = %v", owners, found)
	}
	processes := auditProcesses{Checked: 2, PackageDB: true, PreloadOwners: owners,
		Preloads: []auditProcess{{PID: 1264365, Comm: "fluentd", Exe: libraries[0]}, {PID: 9, Comm: "x", Exe: libraries[1] + ":" + libraries[2]}}}
	result := auditProcessPreload(auditFacts{Processes: processes})
	wantValue := "1264365 fluentd  LD_PRELOAD=" + libraries[0] + "\n" + auditNotePrefix + T("audit.proc.lib_package", "unified-monitoring-agent") + "\n" +
		"9 x  LD_PRELOAD=" + libraries[1] + ":" + libraries[2] + "\n" +
		auditNotePrefix + libraries[1] + ": " + T("audit.proc.lib_package", "libsnoopy") + "\n" +
		auditNotePrefix + libraries[2] + ": " + T("audit.proc.lib_no_package")
	if result.Summary != T("audit.proc.preload_env", 2) || result.Evidence[0].Value != wantValue {
		t.Fatalf("summary = %q\nvalue = %q\nwant  = %q", result.Summary, result.Evidence[0].Value, wantValue)
	}
	// dpkg 기록이 없으면 소속을 판단할 수 없어 설명을 붙이지 않는다.
	if _, found := auditDpkgOwners(auditSources{root: t.TempDir()}, libraries); found {
		t.Fatal("found without dpkg")
	}
	file := auditProcessPreload(auditFacts{Processes: auditProcesses{PreloadFile: []string{"/lib/libsnoopy.so"}}})
	if file.Summary != T("audit.proc.preload_file", 1) || file.Evidence[0].Value != "/etc/ld.so.preload  /lib/libsnoopy.so" {
		t.Fatalf("file = %+v", file)
	}
}
