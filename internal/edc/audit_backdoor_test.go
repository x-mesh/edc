package edc

import (
	"crypto/md5"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type testBackdoorProc struct {
	pid                int
	comm, exe, cmdline string
	cgroup             string
	fds                map[int]uint64
}

func writeBackdoorProcs(t *testing.T, root string, procs ...testBackdoorProc) {
	t.Helper()
	for _, proc := range procs {
		dir := filepath.Join(root, "proc", strconv.Itoa(proc.pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(proc.exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		writeAuditFile(t, root, "/proc/"+strconv.Itoa(proc.pid)+"/comm", proc.comm+"\n", 0o644)
		writeAuditFile(t, root, "/proc/"+strconv.Itoa(proc.pid)+"/cmdline", proc.cmdline, 0o644)
		if proc.cgroup != "" {
			writeAuditFile(t, root, "/proc/"+strconv.Itoa(proc.pid)+"/cgroup", proc.cgroup+"\n", 0o644)
		}
		for fd, inode := range proc.fds {
			if err := os.Symlink("socket:["+strconv.FormatUint(inode, 10)+"]", filepath.Join(dir, "fd", strconv.Itoa(fd))); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// 정상인 것(ssh 서비스의 sshd, container의 sshd, -L client, X11 forwarding, 패키지의 systemd-networkd)과
// 오늘 jw-server에서 재현한 것(다른 설정의 sshd, -R 터널, bash -i >& /dev/tcp, /tmp에서 도는 packet 소켓)을 함께 둔다.
func testBackdoorHost(t *testing.T) (auditSources, auditFacts) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBackdoorProcs(t, root,
		testBackdoorProc{pid: 100, comm: "sshd", exe: "/usr/sbin/sshd", cmdline: "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups\x00", cgroup: "0::/system.slice/ssh.service"},
		testBackdoorProc{pid: 200, comm: "sshd", exe: "/usr/sbin/sshd", cmdline: "sshd: /usr/sbin/sshd -f /root/p/sshd.conf -E /root/p/log [listener] 0 of 10-100 startups\x00", cgroup: "0::/user.slice/user-0.slice/session-101473.scope"},
		testBackdoorProc{pid: 300, comm: "sshd", exe: "/usr/sbin/sshd", cmdline: "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups\x00", cgroup: "0::/system.slice/docker-0123abcd.scope"},
		testBackdoorProc{pid: 400, comm: "sshd-session", exe: "/usr/lib/openssh/sshd-session", cmdline: "sshd-session: root\x00"},
		testBackdoorProc{pid: 500, comm: "ssh", exe: "/usr/bin/ssh", cmdline: "ssh\x00-i\x00/root/key\x00-fNR\x00127.0.0.1:42231:127.0.0.1:22\x00root@127.0.0.1\x00"},
		testBackdoorProc{pid: 501, comm: "ssh", exe: "/usr/bin/ssh", cmdline: "ssh\x00-N\x00-L\x00127.0.0.1:18053:127.0.0.1:8080\x00root@ubuntu-53\x00"},
		testBackdoorProc{pid: 502, comm: "ssh", exe: "/usr/bin/ssh", cmdline: "ssh\x00-o\x00RemoteForward 2222 localhost:22\x00relay\x00"},
		testBackdoorProc{pid: 600, comm: "bash", exe: "/usr/bin/bash", cmdline: "bash\x00-i\x00", fds: map[int]uint64{0: 7000, 1: 7000, 2: 7000, 255: 7000}},
		testBackdoorProc{pid: 601, comm: "agent", exe: "/usr/bin/agent", cmdline: "agent\x00", fds: map[int]uint64{2: 7001}},
		testBackdoorProc{pid: 700, comm: "x", exe: "/tmp/x", cmdline: "/tmp/x\x00", fds: map[int]uint64{3: 8000}},
		testBackdoorProc{pid: 701, comm: "systemd-network", exe: "/usr/lib/systemd/systemd-networkd", cmdline: "/usr/lib/systemd/systemd-networkd\x00", fds: map[int]uint64{3: 8001, 4: 8003}},
		testBackdoorProc{pid: 702, comm: "sniff", exe: "/opt/agent/bin/sniff", cmdline: "/opt/agent/bin/sniff\x00--token\x00s3cret\x00", fds: map[int]uint64{5: 8002}},
	)
	writeAuditFile(t, root, "/proc/net/packet", "sk       RefCnt Type Proto  Iface R Rmem   User   Inode\n0000 3 3 0003 0 1 0 0 8000\n0000 3 2 0806 2 1 0 101 8001\n", 0o644)
	writeAuditFile(t, root, "/proc/net/raw", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n   1: 00000000:0001 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 8002 2 0000 0\n", 0o644)
	writeAuditFile(t, root, "/proc/net/raw6", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n   1: 00000000000000000000000000000000:003A 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 8003 2 0000 0\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/systemd.list", "/usr/lib/systemd/systemd-networkd\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/openssh-server.list", "/usr/sbin/sshd\n/usr/lib/openssh/sshd-session\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/openssh-client.list", "/usr/bin/ssh\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/bash.list", "/bin/bash\n", 0o644)
	sources := auditSources{root: root, tcpConnected: func() ([]auditTCPSocket, error) {
		return []auditTCPSocket{testTCP("127.0.0.1:38376", "127.0.0.1:42240", 7000), testTCP("10.0.0.5:50000", "10.0.0.9:443", 7001)}, nil
	}}
	listen := func(port int, pid, process string) listenSocket {
		return listenSocket{Proto: "tcp", Address: "127.0.0.1", Port: port, PID: pid, Process: process}
	}
	facts := auditFacts{Now: testLoginAt(120), Listen: []listenSocket{
		{Proto: "tcp", Address: "0.0.0.0", Port: 22, PID: "100", Process: "sshd"},
		listen(42230, "200", "sshd"), listen(42223, "400", "sshd-session"), listen(6010, "400", "sshd-session"),
	}}
	collectAuditBackdoor(sources, &facts)
	return sources, facts
}

func TestAuditBackdoorFindsTunnelsShellsRawSocketsAndExtraSSHD(t *testing.T) {
	_, facts := testBackdoorHost(t)
	backdoor := facts.Backdoor
	if backdoor.Err != nil || backdoor.TCPErr != nil || backdoor.RawErr != nil || !backdoor.Systemd || !backdoor.PackageDB {
		t.Fatalf("backdoor = %+v", backdoor)
	}

	tunnel := auditReverseTunnel(facts)
	if tunnel.Status != StatusWarn || tunnel.Summary != T("audit.net.reverse_bad", 1, 2) || !slices.Contains(tunnel.Warnings, T("audit.net.reverse_x11", 1)) {
		t.Fatalf("tunnel = %+v", tunnel)
	}
	if want := "tcp 127.0.0.1:42223  pid 400 sshd-session\n" + auditDetailPrefix + "sshd-session: root\n" + auditNotePrefix + T("audit.net.reverse_listener_note"); tunnel.Evidence[0].Value != want {
		t.Fatalf("listener = %q", tunnel.Evidence[0].Value)
	}
	clients := tunnel.Evidence[1].Value
	if !strings.HasPrefix(clients, "500 ssh  root@127.0.0.1\n") || !strings.Contains(clients, "-R 127.0.0.1:42231:127.0.0.1:22") ||
		!strings.Contains(clients, "502 ssh  relay\n") || !strings.Contains(clients, "-o RemoteForward=2222 localhost:22") || strings.Contains(clients, "18053") {
		t.Fatalf("clients = %q", clients)
	}

	shell := auditSocketShellResult(facts)
	if shell.Status != StatusWarn || shell.Summary != T("audit.proc.shell_bad", 1) {
		t.Fatalf("shell = %+v", shell)
	}
	// stderr만 소켓인 agent는 reverse shell이 아니다. bash는 usrmerge 전의 /bin/bash로 패키지에 적혀 있다.
	value := shell.Evidence[0].Value
	if !strings.HasPrefix(value, "600 bash  /usr/bin/bash\n") || !strings.Contains(value, T("audit.proc.shell_peer", "127.0.0.1:42240", "0,1,2")) ||
		!strings.Contains(value, T("audit.proc.lib_package", "bash")) || strings.Contains(value, "agent") {
		t.Fatalf("shell evidence = %q", value)
	}

	raw := auditRawSocketResult(facts)
	if raw.Status != StatusWarn || raw.Summary != T("audit.proc.raw_bad", 2) || backdoor.RawSockets != 4 {
		t.Fatalf("raw = %+v (%d sockets)", raw, backdoor.RawSockets)
	}
	value = raw.Evidence[0].Value
	if !strings.HasPrefix(value, "700 x  /tmp/x\n") || !strings.Contains(value, T("audit.proc.reason.temp")) ||
		!strings.Contains(value, "702 sniff  /opt/agent/bin/sniff") || !strings.Contains(value, T("audit.proc.lib_no_package")) ||
		!strings.Contains(value, "--token ***") || strings.Contains(value, "s3cret") || strings.Contains(value, "systemd-networkd") {
		t.Fatalf("raw evidence = %q", value)
	}

	extra := auditExtraSSHDResult(facts)
	if extra.Status != StatusWarn || extra.Summary != T("audit.ssh.extra_bad", 1) || len(backdoor.Daemons) != 3 {
		t.Fatalf("extra = %+v, daemons = %d", extra, len(backdoor.Daemons))
	}
	value = extra.Evidence[0].Value
	if !strings.HasPrefix(value, "200 sshd  127.0.0.1:42230\n") || !strings.Contains(value, "-f /root/p/sshd.conf") || !strings.Contains(value, "cgroup /user.slice/user-0.slice/session-101473.scope") {
		t.Fatalf("extra evidence = %q", value)
	}
}

func TestAuditBackdoorPassesOnACleanHost(t *testing.T) {
	root := t.TempDir()
	writeBackdoorProcs(t, root, testBackdoorProc{pid: 100, comm: "sshd", exe: "/usr/sbin/sshd", cmdline: "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups\x00", cgroup: "0::/system.slice/ssh.service"})
	writeAuditFile(t, root, "/proc/net/packet", "sk       RefCnt Type Proto  Iface R Rmem   User   Inode\n", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	facts := auditFacts{Listen: []listenSocket{{Proto: "tcp", Address: "0.0.0.0", Port: 22, PID: "100", Process: "sshd"}}}
	collectAuditBackdoor(auditSources{root: root, tcpConnected: func() ([]auditTCPSocket, error) { return nil, nil }}, &facts)
	for _, result := range []Result{auditReverseTunnel(facts), auditSocketShellResult(facts), auditRawSocketResult(facts), auditExtraSSHDResult(facts)} {
		if result.Status != StatusPass || len(result.Evidence) != 0 {
			t.Errorf("%s = %+v", result.Probe, result)
		}
	}
	// /proc나 TCP 표를 읽지 못하면 통과가 아니라 skip이다.
	broken := auditFacts{}
	collectAuditBackdoor(auditSources{root: t.TempDir()}, &broken)
	if result := auditReverseTunnel(broken); result.Status != StatusSkip {
		t.Fatalf("no /proc = %+v", result)
	}
	facts.Backdoor.TCPErr = errAuditNoSource
	if result := auditSocketShellResult(facts); result.Status != StatusSkip {
		t.Fatalf("no tcp = %+v", result)
	}
}

// systemd가 없는 호스트(Alpine, container)는 sshd의 서비스를 알 수 없어, listener가 둘 이상일 때만 알린다.
func TestAuditExtraSSHDWithoutSystemd(t *testing.T) {
	for _, count := range []int{1, 2} {
		root := t.TempDir()
		for index := range count {
			writeBackdoorProcs(t, root, testBackdoorProc{pid: 100 + index, comm: "sshd", exe: "/usr/sbin/sshd", cmdline: "sshd: /usr/sbin/sshd [listener] 0 of 10-100 startups\x00", cgroup: "0::/"})
		}
		facts := auditFacts{}
		collectAuditBackdoor(auditSources{root: root}, &facts)
		result := auditExtraSSHDResult(facts)
		want := StatusPass
		if count > 1 {
			want = StatusWarn
		}
		if result.Status != want || (count > 1 && result.Summary != T("audit.ssh.extra_multi", 2)) {
			t.Errorf("%d listeners = %+v", count, result)
		}
	}
}

func TestAuditSSHForwards(t *testing.T) {
	for _, test := range []struct {
		args        string
		destination string
		forwards    []string
	}{
		{"ssh -N -R 127.0.0.1:2222:127.0.0.1:22 root@host", "root@host", []string{"-R 127.0.0.1:2222:127.0.0.1:22"}},
		{"ssh -fNR8080:localhost:80 host", "host", []string{"-R 8080:localhost:80"}},
		{"ssh -w 0:0 -i key host ls -R /", "host", []string{"-w 0:0"}},
		{"ssh -oRemoteForward=9000:localhost:22 -o Tunnel=no host", "host", []string{"-o RemoteForward=9000:localhost:22"}},
		{"ssh -N -L 8080:localhost:80 -D 1080 host", "host", nil},
		{"ssh -p 22 -- host -R x", "host", nil},
	} {
		destination, forwards := auditSSHForwards(strings.Fields(test.args))
		if destination != test.destination || !slices.Equal(forwards, test.forwards) {
			t.Errorf("%q = %q %q", test.args, destination, forwards)
		}
	}
}

// ssh -R의 포트는 listen 목록에서만 보이므로, 그 목록을 읽지 못하면 터널이 없다고 판정하지 않는다.
func TestAuditReverseTunnelSkipsWithoutListeners(t *testing.T) {
	facts := auditFacts{ListenErr: fs.ErrPermission}
	if result := auditReverseTunnel(facts); result.Status != StatusSkip {
		t.Fatalf("no listeners = %+v", result)
	}
	facts.Backdoor.Clients = []auditSSHClient{{Process: auditBackdoorProcess{auditProcess: auditProcess{PID: 7}}, Destination: "root@203.0.113.9"}}
	if result := auditReverseTunnel(facts); result.Status != StatusWarn || !slices.Contains(result.Warnings, T("audit.net.reverse_no_listen", fs.ErrPermission.Error())) {
		t.Fatalf("client found = %+v", result)
	}
}

func TestAuditKernelTaint(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) { writeAuditFile(t, root, path, content, 0o644) }
	write("/proc/sys/kernel/osrelease", "6.8.0-45-generic\n")
	// O(12)와 E(13)가 켜져 있다. P(0)와 K(15)는 판정에 쓰지 않는다.
	write("/proc/sys/kernel/tainted", strconv.Itoa(1<<12|1<<13|1<<15|1)+"\n")
	write("/sys/module/nvidia/taint", "POE\n")
	write("/sys/module/hidden_mod/taint", "OE\n")
	write("/sys/module/ext4/taint", "\n")
	write("/lib/modules/6.8.0-45-generic/updates/dkms/nvidia.ko.zst", "")
	facts := auditFacts{}
	collectAuditTaint(auditSources{root: root}, &facts)
	result := auditKernelTaint(facts)
	if result.Status != StatusWarn || result.Summary != T("audit.kernel.taint_bad", "OE", 1<<12|1<<13|1<<15|1) {
		t.Fatalf("result = %+v", result)
	}
	value := result.Evidence[0].Value
	if !strings.Contains(value, "hidden_mod  (OE)\n"+auditDetailPrefix+T("audit.kernel.module_no_file", "/lib/modules/6.8.0-45-generic")+"\n"+auditNotePrefix+T("audit.kernel.module_missing")) ||
		!strings.Contains(value, "nvidia  (POE)\n"+auditDetailPrefix+"/lib/modules/6.8.0-45-generic/updates/dkms/nvidia.ko.zst\n"+auditNotePrefix+T("audit.kernel.module_dkms")) || strings.Contains(value, "ext4") {
		t.Fatalf("evidence = %q", value)
	}
	for value, want := range map[int]Status{0: StatusPass, 1 << 15: StatusPass, 1: StatusPass} {
		write("/proc/sys/kernel/tainted", strconv.Itoa(value)+"\n")
		facts := auditFacts{}
		collectAuditTaint(auditSources{root: root}, &facts)
		if result := auditKernelTaint(facts); result.Status != want {
			t.Errorf("tainted %d = %+v", value, result)
		}
	}
	// 모듈을 내린 뒤에도 taint는 재부팅 전까지 남는다.
	empty := t.TempDir()
	writeAuditFile(t, empty, "/proc/sys/kernel/tainted", "4096\n", 0o644)
	facts = auditFacts{}
	collectAuditTaint(auditSources{root: empty}, &facts)
	if result := auditKernelTaint(facts); result.Status != StatusWarn || !slices.Contains(result.Warnings, T("audit.kernel.taint_kept")) {
		t.Fatalf("unloaded = %+v", result)
	}
	if result := auditKernelTaint(auditFacts{Taint: auditTaint{Err: os.ErrPermission}}); result.Status != StatusSkip {
		t.Fatalf("unreadable = %+v", result)
	}
}

func TestAuditPkgModified(t *testing.T) {
	root := t.TempDir()
	sum := func(content string) string {
		hash := md5.Sum([]byte(content))
		return hex.EncodeToString(hash[:])
	}
	write := func(path, content string) { writeAuditFile(t, root, path, content, 0o755) }
	write("/usr/bin/sudo", "sudo binary")
	write("/usr/bin/sudoedit", "replaced")
	write("/usr/bin/ls", "ls binary")
	write("/usr/sbin/ss", "local build")
	write("/usr/lib/x86_64-linux-gnu/security/pam_unix.so", "pam")
	write("/usr/share/doc/sudo/README", "edited doc")
	// usrmerge 전의 coreutils는 /bin/ls로 적혀 있다. ss는 dpkg-divert로 옮긴 경로라 md5가 달라도 정상이다.
	writeAuditFile(t, root, "/var/lib/dpkg/info/sudo.md5sums", sum("sudo binary")+"  usr/bin/sudo\n"+sum("sudoedit")+"  usr/bin/sudoedit\n"+sum("readme")+"  usr/share/doc/sudo/README\n"+sum("gone")+"  usr/bin/cvtsudoers\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/coreutils.md5sums", sum("ls binary")+"  bin/ls\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/iproute2.md5sums", sum("ss")+"  usr/sbin/ss\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/info/libpam-modules:amd64.md5sums", sum("pam")+"  usr/lib/x86_64-linux-gnu/security/pam_unix.so\n", 0o644)
	writeAuditFile(t, root, "/var/lib/dpkg/diversions", "/usr/sbin/ss\n/usr/sbin/ss.distrib\nlocal\n", 0o644)
	facts := auditFacts{}
	collectAuditPkgVerify(auditSources{root: root}, &facts)
	verify := facts.PkgVerify
	if verify.Packages != 4 || verify.Checked != 4 || len(verify.Modified) != 1 || verify.Modified[0].Path != "/usr/bin/sudoedit" || len(verify.Missing) != 1 {
		t.Fatalf("verify = %+v", verify)
	}
	result := auditPkgModified(facts)
	if result.Status != StatusFail || result.Next != "dpkg --verify sudo" || !strings.HasPrefix(result.Evidence[0].Value, "/usr/bin/sudoedit\n"+auditDetailPrefix+T("audit.file.pkg_package", "sudo")) ||
		!reflect.DeepEqual(result.Warnings, []string{T("audit.file.pkg_missing", 1, "/usr/bin/cvtsudoers")}) {
		t.Fatalf("result = %+v", result)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/sudoedit"), []byte("sudoedit"), 0o755); err != nil {
		t.Fatal(err)
	}
	facts = auditFacts{}
	collectAuditPkgVerify(auditSources{root: root}, &facts)
	// 바뀐 파일이 없어도 패키지 파일이 없으면 통과가 아니다.
	if result := auditPkgModified(facts); result.Status != StatusWarn || result.Summary != T("audit.file.pkg_missing", 1, "/usr/bin/cvtsudoers") ||
		result.Next != "dpkg --verify sudo" || len(result.Warnings) != 0 {
		t.Fatalf("missing = %+v", result)
	}
	write("/usr/bin/cvtsudoers", "gone")
	facts = auditFacts{}
	collectAuditPkgVerify(auditSources{root: root}, &facts)
	if result := auditPkgModified(facts); result.Status != StatusPass || result.Summary != T("audit.file.pkg_ok", 5, 4) {
		t.Fatalf("pass = %+v", result)
	}
	// dpkg가 없는 호스트(RHEL, Alpine)는 skip이다.
	facts = auditFacts{}
	collectAuditPkgVerify(auditSources{root: t.TempDir()}, &facts)
	if result := auditPkgModified(facts); result.Status != StatusSkip || result.Summary != T("audit.skip.no_dpkg") {
		t.Fatalf("no dpkg = %+v", result)
	}
}
