package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// auditTempDirs는 누구나 쓸 수 있는 디렉터리다. 내려받은 도구를 여기서 바로 실행하는 일이 많다.
var auditTempDirs = []string{"/tmp/", "/var/tmp/", "/dev/shm/"}

// auditMemfdAllowed는 memfd에서 실행해도 정상인 이름이다. runc는 container escape(CVE-2019-5736)를 막으려고 자기 binary를 memfd로 복제해 실행한다.
var auditMemfdAllowed = []string{"runc_cloned:"}

// auditPackageDirs는 패키지가 binary를 두는 곳이다. usrmerge를 하지 않은 시스템은 /sbin과 /lib에도 daemon을 둔다.
var auditPackageDirs = []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib64/"}

// auditProcessShown은 근거에 적는 process 수다.
const auditProcessShown = 20

// auditClockTicks는 /proc/<pid>/stat의 starttime 단위(USER_HZ)다. linuxClockTicks와 같은 값이지만 그 상수는 Linux 전용 파일에 있다.
const auditClockTicks = 100

type auditProcess struct {
	PID       int
	Comm, Exe string
	Reason    string
	// Started는 process가 시작한 시각이고, Parent는 부모 process다. 읽지 못하면 비어 있다.
	Started    time.Time
	ParentPID  int
	ParentComm string
	// FileState는 지워진 실행 파일이나 라이브러리의 경로가 지금 어떤지다. replaced면 같은 경로에 새 파일이 있어
	// process가 재시작 전까지 옛 binary로 돌고 있고, removed면 그 경로에 파일이 없다.
	FileState  string
	ReplacedAt time.Time
}

type auditProcesses struct {
	Suspicious []auditProcess
	Checked    int
	// Unreadable는 권한이 없어 실행 파일을 읽지 못한 process 수다.
	Unreadable int
	Err        error
	// Preloads는 환경에 LD_PRELOAD가 있는 process다. Process.Exe에 그 값을 담는다. EnvUnreadable은 환경을 읽지 못한 process 수다.
	Preloads      []auditProcess
	EnvUnreadable int
	// PreloadFile은 /etc/ld.so.preload의 항목이다. 이 파일은 모든 동적 링크 process에 라이브러리를 넣는다.
	PreloadFile    []string
	PreloadFileErr error
	// PreloadOwners는 미리 넣은 라이브러리를 가진 dpkg 패키지다. PackageDB가 false면 dpkg 기록이 없어 소속을 모른다.
	PreloadOwners map[string]string
	PackageDB     bool
}

func collectAuditProcesses(sources auditSources) auditProcesses {
	var processes auditProcesses
	// /etc/ld.so.preload는 /proc를 읽지 못해도 판정할 수 있다.
	processes.PreloadFile, processes.PreloadFileErr = readAuditPreloadFile(sources.file("/etc/ld.so.preload"))
	procRoot := sources.file("/proc")
	boot := readAuditBootTime(sources.file("/proc/stat"))
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		processes.Err = err
		return processes
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || (sources.pid != 0 && pid == sources.pid) {
			continue
		}
		exe, err := os.Readlink(filepath.Join(procRoot, entry.Name(), "exe"))
		if errors.Is(err, fs.ErrPermission) {
			processes.Unreadable++
			continue
		}
		// kernel thread에는 실행 파일이 없고, 그 사이에 끝난 process도 있다.
		if err != nil {
			continue
		}
		processes.Checked++
		dir := filepath.Join(procRoot, entry.Name())
		comm := func() string {
			value, _ := readTrimmed(filepath.Join(dir, "comm"))
			return value
		}
		if reason := auditExeReason(exe); reason != "" {
			processes.Suspicious = append(processes.Suspicious, describeAuditProcess(sources, procRoot, boot, auditProcess{PID: pid, Comm: comm(), Exe: exe, Reason: reason}))
		}
		for _, library := range auditDeletedLibraries(filepath.Join(dir, "maps")) {
			process := describeAuditProcess(sources, procRoot, boot, auditProcess{PID: pid, Comm: comm(), Exe: library, Reason: "deleted_lib"})
			process.FileState, process.ReplacedAt = auditFileState(sources, library)
			processes.Suspicious = append(processes.Suspicious, process)
		}
		environ, err := os.ReadFile(filepath.Join(dir, "environ"))
		// 그 사이에 끝난 process는 확인할 것이 없다. 다른 오류는 확인하지 못한 것으로 센다.
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				processes.EnvUnreadable++
			}
			continue
		}
		for _, variable := range strings.Split(string(environ), "\x00") {
			if value, ok := strings.CutPrefix(variable, "LD_PRELOAD="); ok && strings.TrimSpace(value) != "" {
				processes.Preloads = append(processes.Preloads, auditProcess{PID: pid, Comm: comm(), Exe: value})
			}
		}
	}
	slices.SortFunc(processes.Suspicious, func(a, b auditProcess) int { return a.PID - b.PID })
	if libraries := auditPreloadLibraries(processes); len(libraries) > 0 {
		processes.PreloadOwners, processes.PackageDB = auditDpkgOwners(sources, libraries)
	}
	return processes
}

// auditSplitPreload는 LD_PRELOAD 값의 라이브러리다. ld.so는 공백과 콜론으로 나눈다.
func auditSplitPreload(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ':' || r == ' ' || r == '\t' })
}

func auditPreloadLibraries(processes auditProcesses) []string {
	libraries := slices.Clone(processes.PreloadFile)
	for _, process := range processes.Preloads {
		libraries = append(libraries, auditSplitPreload(process.Exe)...)
	}
	slices.Sort(libraries)
	return slices.Compact(libraries)
}

// auditDpkgOwners는 라이브러리를 가진 dpkg 패키지를 /var/lib/dpkg/info/*.list에서 찾는다. 패키지가 가진 라이브러리는 대개 정상이고,
// 어느 패키지에도 속하지 않은 라이브러리는 누가 직접 넣은 것이다. dpkg는 usrmerge 전의 /lib 경로로 적을 수 있고
// LD_PRELOAD는 버전 없는 symlink를 가리키는 일이 많아, 두 경로와 symlink의 대상도 함께 찾는다.
func auditDpkgOwners(sources auditSources, libraries []string) (map[string]string, bool) {
	lists, _ := filepath.Glob(sources.file("/var/lib/dpkg/info/*.list"))
	if len(lists) == 0 {
		return nil, false
	}
	// root 자체가 symlink 아래에 있으면 대상 경로와 비교하기 전에 root도 실제 경로로 푼다.
	root, err := filepath.EvalSymlinks(sources.file("/"))
	if err != nil {
		root = sources.file("/")
	}
	wanted := map[string]string{}
	for _, library := range libraries {
		candidates := []string{library}
		if target, err := filepath.EvalSymlinks(sources.file(library)); err == nil {
			if relative, err := filepath.Rel(root, target); err == nil && !strings.HasPrefix(relative, "..") {
				candidates = append(candidates, "/"+relative)
			}
		}
		for _, path := range slices.Clone(candidates) {
			candidates = append(candidates, auditUsrMergePath(path))
		}
		for _, path := range candidates {
			if _, seen := wanted[path]; !seen {
				wanted[path] = library
			}
		}
	}
	// 목록 전체는 1000개, 12 MB쯤이다. 깨끗한 호스트에서도 systemd-networkd의 소속을 찾느라 매번 다 읽으면 cache가 빈 첫 실행에서
	// 0.7초가 든다(jw-server). 파일 이름과 패키지 이름이 비슷한 목록을 먼저 읽고, 모든 소속을 찾으면 멈춘다. 결과는 같다.
	names := make([]string, 0, len(wanted))
	for path := range wanted {
		names = append(names, filepath.Base(path))
	}
	likely := func(list string) bool {
		name := auditDpkgListName(list)
		return slices.ContainsFunc(names, func(base string) bool { return strings.HasPrefix(base, name) || strings.HasPrefix(name, base) })
	}
	slices.SortStableFunc(lists, func(a, b string) int {
		switch {
		case likely(a) == likely(b):
			return 0
		case likely(a):
			return -1
		}
		return 1
	})
	unique := map[string]bool{}
	for _, library := range libraries {
		unique[library] = true
	}
	owners := map[string]string{}
	for _, list := range lists {
		data, err := os.ReadFile(list)
		if err != nil {
			continue
		}
		name := auditDpkgListName(list)
		for _, line := range strings.Split(string(data), "\n") {
			if library, ok := wanted[line]; ok {
				if _, found := owners[library]; !found {
					owners[library] = name
				}
			}
		}
		if len(owners) == len(unique) {
			break
		}
	}
	return owners, true
}

// auditDpkgListName은 /var/lib/dpkg/info/<package>[:<arch>].list의 패키지 이름이다.
func auditDpkgListName(list string) string {
	name, _, _ := strings.Cut(strings.TrimSuffix(filepath.Base(list), ".list"), ":")
	return name
}

// auditUsrMergePath는 usrmerge 앞뒤의 다른 경로다. /lib와 /usr/lib는 같은 디렉터리지만 dpkg 기록은 한쪽에만 있다.
func auditUsrMergePath(path string) string {
	for _, dir := range []string{"/lib/", "/lib64/", "/bin/", "/sbin/"} {
		if rest, ok := strings.CutPrefix(path, "/usr"+dir); ok {
			return dir + rest
		}
		if rest, ok := strings.CutPrefix(path, dir); ok {
			return "/usr" + dir + rest
		}
	}
	return path
}

// describeAuditProcess는 시작 시각, 부모, 지워진 실행 파일의 현재 상태를 채운다. 훑는 사이에 끝난 process는 읽은 값만 남는다.
func describeAuditProcess(sources auditSources, procRoot string, boot time.Time, process auditProcess) auditProcess {
	dir := filepath.Join(procRoot, strconv.Itoa(process.PID))
	if data, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
		if stat, ok := parseLinuxProcessStat(process.PID, string(data)); ok {
			if !boot.IsZero() {
				process.Started = boot.Add(time.Duration(stat.StartTicks) * time.Second / auditClockTicks)
			}
			process.ParentPID = stat.PPID
			if stat.PPID > 0 {
				process.ParentComm, _ = readTrimmed(filepath.Join(procRoot, strconv.Itoa(stat.PPID), "comm"))
			}
		}
	}
	if path, deleted := strings.CutSuffix(process.Exe, deletedOpenSuffix); deleted && !strings.HasPrefix(path, "/memfd:") {
		process.FileState, process.ReplacedAt = auditFileState(sources, path)
	}
	return process
}

// auditFileState는 지워진 파일의 경로가 지금 어떤지다. 패키지 업그레이드나 재배포는 같은 경로에 새 파일을 두고, 흔적을 지우려는 도구는 파일만 없앤다.
func auditFileState(sources auditSources, path string) (string, time.Time) {
	info, err := os.Stat(sources.file(path))
	if err != nil {
		return "removed", time.Time{}
	}
	return "replaced", info.ModTime()
}

// auditDeletedLibraries는 지워진 채 메모리에 남은 공유 라이브러리다. 패키지 디렉터리 밖의 것만 돌려준다.
// 침입 도구는 라이브러리를 넣은 뒤 파일을 지워 흔적을 숨긴다. 패키지 업그레이드도 /usr 아래에 같은 모양을 남긴다.
func auditDeletedLibraries(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var libraries []string
	for _, line := range strings.Split(string(data), "\n") {
		library, ok := strings.CutSuffix(line, deletedOpenSuffix)
		if !ok {
			continue
		}
		fields := strings.Fields(library)
		if len(fields) < 6 {
			continue
		}
		library = strings.Join(fields[5:], " ")
		if !strings.Contains(filepath.Base(library), ".so") || strings.HasPrefix(library, "/memfd:") || slices.Contains(libraries, library) ||
			slices.ContainsFunc(auditPackageDirs, func(dir string) bool { return strings.HasPrefix(library, dir) }) {
			continue
		}
		libraries = append(libraries, library)
	}
	return libraries
}

func readAuditPreloadFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			entries = append(entries, strings.Fields(line)...)
		}
	}
	return entries, nil
}

func auditProcessPreload(facts auditFacts) Result {
	const id = "proc.preload"
	processes := facts.Processes
	if processes.PreloadFileErr != nil {
		return auditSkip(id, "/etc/ld.so.preload", processes.PreloadFileErr)
	}
	var lines []string
	for _, entry := range processes.PreloadFile {
		lines = append(lines, auditItem("/etc/ld.so.preload  "+entry, nil, auditPreloadNotes(processes, []string{entry})))
	}
	for _, process := range processes.Preloads[:min(len(processes.Preloads), auditProcessShown)] {
		lines = append(lines, auditItem(fmt.Sprintf("%d %s  LD_PRELOAD=%s", process.PID, process.Comm, process.Exe), nil, auditPreloadNotes(processes, auditSplitPreload(process.Exe))))
	}
	if extra := len(processes.Preloads) - auditProcessShown; extra > 0 {
		lines = append(lines, fmt.Sprintf("+%d", extra))
	}
	if processes.Err != nil && len(lines) == 0 {
		return auditSkip(id, "/proc", processes.Err)
	}
	var result Result
	if len(lines) == 0 {
		result = auditResult(id, StatusPass, T("audit.proc.preload_ok", processes.Checked))
	} else {
		switch {
		case len(processes.PreloadFile) == 0:
			result = auditResult(id, StatusWarn, T("audit.proc.preload_env", len(processes.Preloads)))
		case len(processes.Preloads) == 0:
			result = auditResult(id, StatusWarn, T("audit.proc.preload_file", len(processes.PreloadFile)))
		default:
			result = auditResult(id, StatusWarn, T("audit.proc.preload_bad", len(processes.PreloadFile), len(processes.Preloads)))
		}
		result.Evidence = []Evidence{{Label: T("audit.evidence.processes"), Value: strings.Join(lines, "\n")}}
		result.Next = "cat /etc/ld.so.preload"
		if len(processes.Preloads) > 0 {
			result.Next = fmt.Sprintf("tr '\\0' '\\n' < /proc/%d/environ | grep LD_PRELOAD", processes.Preloads[0].PID)
		}
	}
	if processes.Err != nil {
		result.Warnings = append(result.Warnings, T("audit.proc.proc_unreadable", infoSingleLine(processes.Err.Error())))
	}
	if processes.EnvUnreadable > 0 {
		result.Warnings = append(result.Warnings, T("audit.proc.env_unreadable", processes.EnvUnreadable))
	}
	return result
}

// auditProcessLine은 process 하나의 근거다. 지금 실행 중인지, 언제부터인지, 누가 띄웠는지, 파일이 바뀌었는지 지워졌는지를 적는다.
func auditProcessLine(process auditProcess, now time.Time) string {
	var notes []string
	// 지워진 binary는 그 경로의 지금 상태가 이유를 대신한다. 상태를 모를 때만 원래 이유를 쓴다.
	if process.Reason != "deleted" || process.FileState == "" {
		notes = append(notes, T("audit.proc.reason."+process.Reason))
	}
	switch process.FileState {
	case "replaced":
		notes = append(notes, T("audit.proc.file_replaced", auditLoginTime(process.ReplacedAt)))
	case "removed":
		notes = append(notes, T("audit.proc.file_removed"))
	}
	return auditItem(fmt.Sprintf("%d %s  %s", process.PID, process.Comm, strings.TrimSuffix(process.Exe, deletedOpenSuffix)), []string{strings.Join(auditProcessFacts(process, now), "  ")}, notes)
}

// auditProcessFacts는 process의 시작 시각과 부모다. 하루가 넘었으면 며칠째인지 붙인다.
func auditProcessFacts(process auditProcess, now time.Time) []string {
	var facts []string
	if !process.Started.IsZero() {
		if days := int(now.Sub(process.Started) / (24 * time.Hour)); days >= 1 {
			facts = append(facts, T("audit.proc.started_days", auditLoginTime(process.Started), days))
		} else {
			facts = append(facts, T("audit.proc.started", auditLoginTime(process.Started)))
		}
	}
	if process.ParentPID > 0 {
		facts = append(facts, T("audit.proc.parent", process.ParentPID, process.ParentComm))
	}
	return facts
}

// auditPreloadNotes는 라이브러리마다 소속 패키지다. dpkg 기록이 없으면 판단할 근거가 없어 적지 않는다.
func auditPreloadNotes(processes auditProcesses, libraries []string) []string {
	if !processes.PackageDB {
		return nil
	}
	notes := make([]string, 0, len(libraries))
	for _, library := range libraries {
		note := T("audit.proc.lib_no_package")
		if owner, ok := processes.PreloadOwners[library]; ok {
			note = T("audit.proc.lib_package", owner)
		}
		if len(libraries) > 1 {
			note = library + ": " + note
		}
		notes = append(notes, note)
	}
	return notes
}

// auditExeReason은 실행 파일 경로가 의심스러운 이유다. /usr 아래에서 지워진 binary는 패키지 업그레이드 뒤 재시작하지 않은 daemon이라 판정하지 않는다.
func auditExeReason(exe string) string {
	path, deleted := strings.CutSuffix(exe, deletedOpenSuffix)
	if name, ok := strings.CutPrefix(path, "/memfd:"); ok {
		for _, allowed := range auditMemfdAllowed {
			if strings.HasPrefix(name, allowed) {
				return ""
			}
		}
		return "memfd"
	}
	for _, dir := range auditTempDirs {
		if strings.HasPrefix(path, dir) {
			return "temp"
		}
	}
	if deleted && !slices.ContainsFunc(auditPackageDirs, func(dir string) bool { return strings.HasPrefix(path, dir) }) {
		return "deleted"
	}
	return ""
}

func auditProcessExe(facts auditFacts) Result {
	const id = "proc.unusual_exe"
	processes := facts.Processes
	if processes.Err != nil {
		return auditSkip(id, "/proc", processes.Err)
	}
	var result Result
	if len(processes.Suspicious) == 0 {
		result = auditResult(id, StatusPass, T("audit.proc.exe_ok", processes.Checked))
	} else {
		lines := make([]string, 0, min(len(processes.Suspicious), auditProcessShown)+1)
		for _, process := range processes.Suspicious[:min(len(processes.Suspicious), auditProcessShown)] {
			lines = append(lines, auditProcessLine(process, facts.Now))
		}
		if extra := len(processes.Suspicious) - auditProcessShown; extra > 0 {
			lines = append(lines, fmt.Sprintf("+%d", extra))
		}
		pids := map[int]bool{}
		for _, process := range processes.Suspicious {
			pids[process.PID] = true
		}
		result = auditResult(id, StatusWarn, T("audit.proc.exe_bad", len(pids)))
		result.Evidence = []Evidence{{Label: T("audit.evidence.processes"), Value: strings.Join(lines, "\n")}}
		result.Next = fmt.Sprintf("ls -l /proc/%d/exe; cat /proc/%d/cmdline | tr '\\0' ' '", processes.Suspicious[0].PID, processes.Suspicious[0].PID)
	}
	if processes.Unreadable > 0 {
		result.Warnings = []string{T("audit.proc.unreadable", processes.Unreadable)}
	}
	return result
}
