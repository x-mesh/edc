package edc

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 무결성 점검은 침입자가 흔적을 숨기려고 바꾸는 두 곳을 본다. 커널 모듈(LKM rootkit)과 로그인·process를 보여 주는 binary다.

// auditTaintBits는 판정에 쓰는 /proc/sys/kernel/tainted 비트다. F는 강제로 넣은 모듈, O는 커널 트리 밖 모듈, E는 서명 없는 모듈이다.
// P(상용 드라이버)와 K(livepatch)는 정상 운영에서 흔해 보지 않는다. nvidia처럼 트리 밖 상용 드라이버는 O도 함께 켠다.
var auditTaintBits = []struct {
	bit    uint
	letter string
}{{1, "F"}, {12, "O"}, {13, "E"}}

type auditTaintModule struct {
	Name, Flags, Path, Owner string
}

type auditTaint struct {
	Err       error
	Value     uint64
	Letters   []string
	Release   string
	Modules   []auditTaintModule
	PackageDB bool
}

func collectAuditTaint(sources auditSources, facts *auditFacts) {
	taint := &facts.Taint
	text, err := readTrimmed(sources.file("/proc/sys/kernel/tainted"))
	if err != nil {
		taint.Err = err
		return
	}
	taint.Value, err = strconv.ParseUint(text, 10, 64)
	if err != nil {
		taint.Err = fmt.Errorf("/proc/sys/kernel/tainted: %q: %w", text, err)
		return
	}
	for _, bit := range auditTaintBits {
		if taint.Value&(1<<bit.bit) != 0 {
			taint.Letters = append(taint.Letters, bit.letter)
		}
	}
	if len(taint.Letters) == 0 {
		return
	}
	taint.Release, _ = readTrimmed(sources.file("/proc/sys/kernel/osrelease"))
	entries, _ := os.ReadDir(sources.file("/sys/module"))
	for _, entry := range entries {
		flags, err := readTrimmed(sources.file(filepath.Join("/sys/module", entry.Name(), "taint")))
		if err == nil && flags != "" {
			taint.Modules = append(taint.Modules, auditTaintModule{Name: entry.Name(), Flags: flags})
		}
	}
	if len(taint.Modules) == 0 || taint.Release == "" {
		return
	}
	// 모듈 파일 이름은 -와 _를 섞어 쓰고 압축 확장자가 붙는다. 커널은 이름을 _로 적는다.
	wanted := map[string]int{}
	for index, module := range taint.Modules {
		wanted[module.Name] = index
	}
	root := filepath.Join("/lib/modules", taint.Release)
	var paths []string
	_ = filepath.WalkDir(sources.file(root), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		for _, suffix := range []string{".zst", ".xz", ".gz"} {
			name = strings.TrimSuffix(name, suffix)
		}
		base, ok := strings.CutSuffix(name, ".ko")
		if !ok {
			return nil
		}
		if index, found := wanted[strings.ReplaceAll(base, "-", "_")]; found && taint.Modules[index].Path == "" {
			relative, _ := filepath.Rel(sources.file("/"), path)
			taint.Modules[index].Path = "/" + relative
			paths = append(paths, "/"+relative)
		}
		return nil
	})
	if len(paths) == 0 {
		return
	}
	owners, found := auditDpkgOwners(sources, paths)
	taint.PackageDB = found
	for index := range taint.Modules {
		taint.Modules[index].Owner = owners[taint.Modules[index].Path]
	}
}

func auditKernelTaint(facts auditFacts) Result {
	const id = "kernel.taint"
	taint := facts.Taint
	if taint.Err != nil {
		return auditSkip(id, "/proc/sys/kernel/tainted", taint.Err)
	}
	if len(taint.Letters) == 0 {
		return auditResult(id, StatusPass, T("audit.kernel.taint_ok", taint.Value))
	}
	result := auditResult(id, StatusWarn, T("audit.kernel.taint_bad", strings.Join(taint.Letters, ""), taint.Value))
	if len(taint.Modules) == 0 {
		result.Warnings = append(result.Warnings, T("audit.kernel.taint_kept"))
		result.Next = "cat /proc/sys/kernel/tainted; dmesg | grep -i taint"
		return result
	}
	items := make([]string, 0, len(taint.Modules))
	for _, module := range taint.Modules {
		var details, notes []string
		switch {
		case module.Path == "":
			details = append(details, T("audit.kernel.module_no_file", filepath.Join("/lib/modules", taint.Release)))
			notes = append(notes, T("audit.kernel.module_missing"))
		case strings.Contains(module.Path, "/updates/dkms/"):
			details = append(details, module.Path)
			notes = append(notes, T("audit.kernel.module_dkms"))
		default:
			details = append(details, module.Path)
			if module.Owner != "" {
				notes = append(notes, T("audit.proc.lib_package", module.Owner))
			} else if taint.PackageDB {
				notes = append(notes, T("audit.proc.lib_no_package"))
			}
		}
		items = append(items, auditItem(module.Name+"  ("+module.Flags+")", details, notes))
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.modules"), Value: strings.Join(items, "\n")}}
	result.Next = "modinfo " + taint.Modules[0].Name
	return result
}

// auditVerifyPackages는 변조 여부를 보는 패키지다. 로그인, 권한 상승, process와 파일과 소켓 목록을 보여 주는 binary를 가진다.
// 침입자가 흔적을 숨기려고 바꾸는 binary가 여기 모여 있다(sshd, sudo, ps, ls, ss, login, pam_unix.so).
var auditVerifyPackages = []string{
	"openssh-server", "openssh-client", "openssh-sftp-server", "sudo", "login", "passwd", "libpam-modules", "libpam-modules-bin",
	"procps", "coreutils", "util-linux", "findutils", "iproute2", "net-tools", "bash", "libc-bin", "cron", "systemd",
}

// auditVerifyPrefixes는 실행 파일과 PAM 모듈이 있는 경로다. 문서와 설정은 보지 않는다. 설정(conffile)은 md5sums에 없다.
var auditVerifyPrefixes = []string{"usr/bin/", "usr/sbin/", "bin/", "sbin/", "usr/libexec/", "usr/lib/openssh/"}

type auditPkgFile struct {
	Path, Package string
	ModTime       time.Time
	ChangeTime    time.Time
}

type auditPkgVerify struct {
	Err      error
	NoDB     bool
	Packages int
	Checked  int
	Modified []auditPkgFile
	Missing  []auditPkgFile
}

func collectAuditPkgVerify(sources auditSources, facts *auditFacts) {
	verify := &facts.PkgVerify
	info := sources.file("/var/lib/dpkg/info")
	if _, err := os.Stat(info); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			verify.NoDB = true
		} else {
			verify.Err = err
		}
		return
	}
	diverted := readAuditDiversions(sources.file("/var/lib/dpkg/diversions"))
	for _, name := range auditVerifyPackages {
		lists, _ := filepath.Glob(filepath.Join(info, name+".md5sums"))
		arch, _ := filepath.Glob(filepath.Join(info, name+":*.md5sums"))
		lists = append(lists, arch...)
		if len(lists) == 0 {
			continue
		}
		verify.Packages++
		for _, list := range lists {
			verifyAuditMD5Sums(sources, list, name, diverted, verify)
		}
	}
}

// readAuditDiversions는 dpkg-divert로 옮긴 원래 경로다. 그 경로에는 다른 패키지의 파일이 있어 md5가 달라도 정상이다.
// 파일은 원래 경로, 옮긴 경로, 패키지의 세 줄이 반복된다.
func readAuditDiversions(path string) map[string]bool {
	diverted := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return diverted
	}
	lines := strings.Split(string(data), "\n")
	for index := 0; index+2 < len(lines); index += 3 {
		diverted[lines[index]] = true
	}
	return diverted
}

func verifyAuditMD5Sums(sources auditSources, list, name string, diverted map[string]bool, verify *auditPkgVerify) {
	file, err := os.Open(list)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		sum, relative, ok := strings.Cut(scanner.Text(), "  ")
		if !ok || !slices.ContainsFunc(auditVerifyPrefixes, func(prefix string) bool { return strings.HasPrefix(relative, prefix) }) &&
			!strings.Contains(relative, "/security/pam_") {
			continue
		}
		path := "/" + relative
		if diverted[path] || diverted[auditUsrMergePath(path)] {
			continue
		}
		full := sources.file(path)
		info, err := os.Stat(full)
		// usrmerge 전에 만든 패키지는 /bin처럼 옛 경로로 적혀 있다. 두 경로는 같은 파일이다.
		if err != nil {
			full = sources.file(auditUsrMergePath(path))
			info, err = os.Stat(full)
		}
		record := auditPkgFile{Path: path, Package: name}
		if err != nil {
			verify.Missing = append(verify.Missing, record)
			continue
		}
		actual, err := auditFileMD5(full)
		if err != nil {
			continue
		}
		verify.Checked++
		if actual == sum {
			continue
		}
		record.ModTime = info.ModTime()
		if sources.ctime != nil {
			record.ChangeTime, _ = sources.ctime(info)
		}
		verify.Modified = append(verify.Modified, record)
	}
}

func auditFileMD5(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func auditPkgModified(facts auditFacts) Result {
	const id = "file.pkg_modified"
	verify := facts.PkgVerify
	if verify.NoDB {
		return auditResult(id, StatusSkip, T("audit.skip.no_dpkg"))
	}
	if verify.Err != nil {
		return auditSkip(id, "/var/lib/dpkg/info", verify.Err)
	}
	var missing []string
	for _, file := range verify.Missing[:min(len(verify.Missing), 3)] {
		missing = append(missing, file.Path)
	}
	var result Result
	switch {
	case len(verify.Modified) == 0 && len(verify.Missing) > 0:
		// 관리자가 지운 파일일 수도 있어 fail이 아니라 warn이다.
		result = auditResult(id, StatusWarn, T("audit.file.pkg_missing", len(verify.Missing), strings.Join(missing, ", ")))
		result.Next = "dpkg --verify " + verify.Missing[0].Package
		return result
	case len(verify.Modified) == 0:
		result = auditResult(id, StatusPass, T("audit.file.pkg_ok", verify.Checked, verify.Packages))
	default:
		result = auditResult(id, StatusFail, T("audit.file.pkg_bad", len(verify.Modified)))
		result.Error = &DiagnosticError{Kind: "integrity", Message: result.Summary}
		items := make([]string, 0, len(verify.Modified))
		for _, file := range verify.Modified {
			changed := auditLater(file.ChangeTime, file.ModTime)
			items = append(items, auditItem(file.Path, []string{T("audit.file.pkg_package", file.Package), T("audit.ssh.key_changed_at", auditLoginTime(changed))},
				[]string{T("audit.file.pkg_note")}))
		}
		result.Evidence = []Evidence{{Label: T("audit.evidence.files"), Value: strings.Join(items, "\n")}}
		result.Next = "dpkg --verify " + verify.Modified[0].Package
	}
	if len(verify.Missing) > 0 {
		result.Warnings = append(result.Warnings, T("audit.file.pkg_missing", len(verify.Missing), strings.Join(missing, ", ")))
	}
	return result
}

// auditLater는 둘 가운데 늦은 시각이다. 내용을 바꾸면 ctime이 움직이고, touch로 mtime만 되돌릴 수 있다.
func auditLater(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
