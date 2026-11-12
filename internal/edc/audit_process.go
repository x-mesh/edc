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
)

// auditTempDirs는 누구나 쓸 수 있는 디렉터리다. 내려받은 도구를 여기서 바로 실행하는 일이 많다.
var auditTempDirs = []string{"/tmp/", "/var/tmp/", "/dev/shm/"}

// auditMemfdAllowed는 memfd에서 실행해도 정상인 이름이다. runc는 container escape(CVE-2019-5736)를 막으려고 자기 binary를 memfd로 복제해 실행한다.
var auditMemfdAllowed = []string{"runc_cloned:"}

// auditPackageDirs는 패키지가 binary를 두는 곳이다. usrmerge를 하지 않은 시스템은 /sbin과 /lib에도 daemon을 둔다.
var auditPackageDirs = []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib64/"}

// auditProcessShown은 근거에 적는 process 수다.
const auditProcessShown = 20

type auditProcess struct {
	PID       int
	Comm, Exe string
	Reason    string
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
}

func collectAuditProcesses(sources auditSources) auditProcesses {
	var processes auditProcesses
	// /etc/ld.so.preload는 /proc를 읽지 못해도 판정할 수 있다.
	processes.PreloadFile, processes.PreloadFileErr = readAuditPreloadFile(sources.file("/etc/ld.so.preload"))
	procRoot := sources.file("/proc")
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
			processes.Suspicious = append(processes.Suspicious, auditProcess{PID: pid, Comm: comm(), Exe: exe, Reason: reason})
		}
		for _, library := range auditDeletedLibraries(filepath.Join(dir, "maps")) {
			processes.Suspicious = append(processes.Suspicious, auditProcess{PID: pid, Comm: comm(), Exe: library, Reason: "deleted_lib"})
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
	return processes
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
		lines = append(lines, "/etc/ld.so.preload  "+entry)
	}
	for _, process := range processes.Preloads[:min(len(processes.Preloads), auditProcessShown)] {
		lines = append(lines, fmt.Sprintf("%d %s  LD_PRELOAD=%s", process.PID, process.Comm, process.Exe))
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
		result = auditResult(id, StatusWarn, T("audit.proc.preload_bad", len(processes.PreloadFile), len(processes.Preloads)))
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
			lines = append(lines, fmt.Sprintf("%d %s  %s  %s", process.PID, process.Comm, process.Exe, T("audit.proc.reason."+process.Reason)))
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
