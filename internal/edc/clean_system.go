package edc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 큰 로그와 쓰지 않는 snap 리비전은 edc가 지워도 되는지 판정하지 않는다. 지운다고 판단할 근거만 읽는다.
// 파일은 읽기만 하고 snap, logrotate, journalctl 같은 명령은 실행하지 않는다.

const (
	cleanLargeLogBytes = 100 << 20
	cleanSystemLimit   = 10
)

// journald 설정은 systemd의 규칙을 따른다. 주 설정 파일은 /etc에 있으면 그것을, 없으면 /usr/lib의 것을 읽는다. drop-in은 높은
// 우선순위 디렉터리부터 /etc, /run, /usr/lib 순이고, 같은 이름은 높은 디렉터리의 파일만 남는다. 남은 파일을 이름 순으로 적용하며
// 나중 값이 앞의 값과 주 설정을 덮는다. 빈 파일이 아니라 /dev/null로 이어진 링크가 같은 이름의 파일을 가린다.
var (
	cleanJournaldMains   = []string{"/etc/systemd/journald.conf", "/usr/lib/systemd/journald.conf"}
	cleanJournaldDropIns = []string{"/etc/systemd/journald.conf.d", "/run/systemd/journald.conf.d", "/usr/lib/systemd/journald.conf.d"}
)

// 시험이 호스트의 실제 /var 대신 빈 폴더를 가리키도록 바꾼다.
var (
	cleanJournalPath = "/var/log/journal"
	cleanAptPath     = "/var/cache/apt/archives"
)

var cleanJournaldKeys = []string{"SystemMaxUse", "SystemKeepFree", "SystemMaxFileSize", "MaxRetentionSec"}

type cleanSystemPaths struct {
	logs          string
	logrotateConf string
	logrotateDir  string
	snapFiles     string
	snapMounts    string
}

var cleanSystemDefaults = cleanSystemPaths{logs: "/var/log", logrotateConf: "/etc/logrotate.conf", logrotateDir: "/etc/logrotate.d",
	snapFiles: "/var/lib/snapd/snaps", snapMounts: "/snap"}

type cleanSystem struct {
	Status string `json:"status"`
	// LogrotateKnown이 false이면 logrotate 설정을 하나도 읽지 못한 것이다. 규칙이 없다는 뜻이 아니다.
	LogrotateKnown   bool                `json:"logrotate_known"`
	Logs             []cleanLargeLog     `json:"large_logs,omitempty"`
	LogsTotal        int                 `json:"large_logs_total,omitempty"`
	LogsOmitted      int                 `json:"large_logs_omitted,omitempty"`
	LogsOmittedBytes uint64              `json:"large_logs_omitted_bytes,omitempty"`
	Snaps            []cleanSnapRevision `json:"snap_revisions,omitempty"`
	SnapsTotal       int                 `json:"snap_revisions_total,omitempty"`
	SnapsOmitted     int                 `json:"snap_revisions_omitted,omitempty"`
	SnapsUnknown     int                 `json:"snaps_unknown,omitempty"`
}

type cleanLargeLog struct {
	Path     string     `json:"path"`
	Bytes    uint64     `json:"bytes"`
	Modified *time.Time `json:"modified,omitempty"`
	// OpenFiles가 nil이면 /proc을 읽지 못한 것이다. OpenFilesComplete가 false이면 읽지 못한 process가 있다.
	OpenFiles         *int   `json:"open_files,omitempty"`
	OpenFilesComplete bool   `json:"open_files_complete"`
	Logrotate         string `json:"logrotate,omitempty"`
	Cleanup           string `json:"cleanup,omitempty"`
}

type cleanSnapRevision struct {
	Name     string     `json:"name"`
	Revision string     `json:"revision"`
	Current  string     `json:"current"`
	Bytes    uint64     `json:"bytes"`
	Modified *time.Time `json:"modified,omitempty"`
	Cleanup  string     `json:"cleanup"`
}

type cleanLogrotatePattern struct {
	glob   string
	source string
}

// cleanJournaldDetail은 journald 설정에서 크기와 보관 기간 제한을 찾아 한 줄로 돌려준다.
func cleanJournaldDetail(mains, dropIns []string) func() string {
	return func() string {
		found := map[string]string{}
		apply := func(path string) {
			text, err := os.ReadFile(path)
			if err != nil {
				return
			}
			section := ""
			for _, line := range strings.Split(string(text), "\n") {
				line = strings.TrimSpace(line)
				switch {
				case line == "" || line[0] == '#' || line[0] == ';':
				case line[0] == '[':
					section = line
				case section == "[Journal]":
					if key, value, ok := strings.Cut(line, "="); ok {
						found[strings.TrimSpace(key)] = strings.TrimSpace(value)
					}
				}
			}
		}
		for _, main := range mains {
			if info, err := os.Stat(main); err == nil && info.Mode().IsRegular() {
				apply(main)
				break
			}
		}
		chosen := map[string]string{}
		for _, dir := range dropIns {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				name := entry.Name()
				if _, taken := chosen[name]; taken || entry.IsDir() || !strings.HasSuffix(name, ".conf") {
					continue
				}
				path := filepath.Join(dir, name)
				if target, err := os.Readlink(path); err == nil && target == "/dev/null" {
					path = ""
				}
				chosen[name] = path
			}
		}
		names := make([]string, 0, len(chosen))
		for name := range chosen {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if chosen[name] != "" {
				apply(chosen[name])
			}
		}
		var limits []string
		for _, key := range cleanJournaldKeys {
			if value := found[key]; value != "" {
				limits = append(limits, key+"="+value)
			}
		}
		if len(limits) == 0 {
			return T("clean.system.journald_no_limit")
		}
		return T("clean.system.journald_limits", strings.Join(limits, ", "))
	}
}

func cleanSystemScan(ctx context.Context, deps cleanDeps, goos string, paths cleanSystemPaths) cleanSystem {
	if goos != "linux" {
		return cleanSystem{}
	}
	if err := ctx.Err(); err != nil {
		return cleanSystem{Status: cleanNotScanned}
	}
	section := cleanSystem{Status: cleanAbsent}
	patterns, known := cleanLogrotatePatterns(ctx, paths)
	section.LogrotateKnown = known
	logs := cleanFindLargeLogs(ctx, paths.logs)
	section.LogsTotal = len(logs)
	if len(logs) > 0 {
		counts, complete, readable := cleanOpenCounts(ctx, cleanProcDir, logs)
		for index := range logs {
			log := &logs[index]
			log.OpenFilesComplete = complete
			if readable {
				count := counts[log.Path]
				log.OpenFiles = &count
				switch {
				case count > 0:
					log.Cleanup = "truncate -s 0 " + cleanShellQuote(log.Path)
				case complete:
					log.Cleanup = "rm " + cleanShellQuote(log.Path)
				}
			}
			log.Logrotate = cleanMatchLogrotate(patterns, log.Path)
		}
	}
	for index, log := range logs {
		if index >= cleanSystemLimit {
			section.LogsOmitted++
			section.LogsOmittedBytes += log.Bytes
			continue
		}
		section.Logs = append(section.Logs, log)
	}
	snaps, unknown := cleanFindSnapRevisions(ctx, paths)
	section.SnapsUnknown = unknown
	section.SnapsTotal = len(snaps)
	for index, snap := range snaps {
		if index >= cleanSystemLimit {
			section.SnapsOmitted++
			continue
		}
		section.Snaps = append(section.Snaps, snap)
	}
	if len(logs) > 0 || len(snaps) > 0 || unknown > 0 {
		section.Status = cleanMeasured
	}
	if ctx.Err() != nil {
		section.Status = cleanPartial
	}
	return section
}

// cleanFindLargeLogs는 dir 아래에서 기준 이상인 일반 파일을 큰 순서로 돌려준다. journal 폴더는 journald 후보가 따로 잰다.
func cleanFindLargeLogs(ctx context.Context, dir string) []cleanLargeLog {
	var logs []cleanLargeLog
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path == filepath.Join(dir, "journal") {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() < cleanLargeLogBytes {
			return nil
		}
		modified := info.ModTime().UTC()
		logs = append(logs, cleanLargeLog{Path: path, Bytes: uint64(info.Size()), Modified: &modified})
		return nil
	})
	sort.Slice(logs, func(i, j int) bool {
		if logs[i].Bytes != logs[j].Bytes {
			return logs[i].Bytes > logs[j].Bytes
		}
		return logs[i].Path < logs[j].Path
	})
	return logs
}

const (
	// logrotate 설정 한 파일에서 읽는 크기의 상한과, include를 따라가며 읽는 파일 수와 깊이의 상한이다.
	cleanLogrotateFileMax  = 1 << 20
	cleanLogrotateFiles    = 500
	cleanLogrotateIncluded = 8
)

// cleanLogrotatePatterns는 logrotate 설정에서 규칙의 로그 경로 패턴을 모은다. 주 설정 파일과 설정 디렉터리에서 시작해
// include 줄을 절대 경로일 때만 따라간다. 설정을 하나도 읽지 못하면 known이 false다.
func cleanLogrotatePatterns(ctx context.Context, paths cleanSystemPaths) (patterns []cleanLogrotatePattern, known bool) {
	type pending struct {
		path  string
		depth int
	}
	var queue []pending
	for _, file := range cleanConfigFilesAny(paths.logrotateConf) {
		queue = append(queue, pending{file, 0})
	}
	for _, file := range cleanLogrotateDirFiles(paths.logrotateDir) {
		queue = append(queue, pending{file, 0})
	}
	visited := map[string]bool{}
	for len(queue) > 0 && len(visited) < cleanLogrotateFiles && ctx.Err() == nil {
		current := queue[0]
		queue = queue[1:]
		if visited[current.path] {
			continue
		}
		visited[current.path] = true
		text, ok := cleanReadLimited(current.path, cleanLogrotateFileMax)
		if !ok {
			continue
		}
		known = true
		patterns = append(patterns, cleanParseLogrotate(text, current.path)...)
		if current.depth >= cleanLogrotateIncluded {
			continue
		}
		for _, include := range cleanLogrotateIncludes(text) {
			info, err := os.Stat(include)
			switch {
			case err != nil:
			case info.IsDir():
				for _, file := range cleanLogrotateDirFiles(include) {
					queue = append(queue, pending{file, current.depth + 1})
				}
			default:
				queue = append(queue, pending{include, current.depth + 1})
			}
		}
	}
	return patterns, known
}

// cleanLogrotateDirFiles는 logrotate가 읽는 설정 디렉터리의 파일을 돌려준다. 백업이나 패키지 관리자가 남긴 파일은 읽지 않는다.
func cleanLogrotateDirFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || strings.Contains(name, ".dpkg-") || strings.Contains(name, ".rpm") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}

// cleanReadLimited는 max를 넘는 파일을 읽지 않는다. 설정 파일 하나가 메모리를 크게 쓰지 못하게 한다.
func cleanReadLimited(path string, max int64) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > max {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// cleanLogrotateIncludes는 중괄호 밖의 include 줄에서 절대 경로를 모은다. logrotate는 상대 경로를 실행 위치 기준으로 읽으므로 따라가지 않는다.
func cleanLogrotateIncludes(text string) []string {
	var includes []string
	depth := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if fields := strings.Fields(line); depth == 0 && len(fields) == 2 && fields[0] == "include" && strings.HasPrefix(fields[1], "/") {
			includes = append(includes, fields[1])
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	return includes
}

func cleanConfigFilesAny(path string) []string {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return []string{path}
	}
	return nil
}

// cleanParseLogrotate는 중괄호 밖에서 /로 시작하는 줄의 경로를 패턴으로 모은다. include와 전역 옵션 줄은 /로 시작하지 않는다.
func cleanParseLogrotate(text, source string) []cleanLogrotatePattern {
	var patterns []cleanLogrotatePattern
	depth := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if depth == 0 {
			head := line
			if brace := strings.Index(head, "{"); brace >= 0 {
				head = head[:brace]
			}
			fields := strings.Fields(head)
			if len(fields) > 0 && strings.HasPrefix(strings.Trim(fields[0], `"'`), "/") {
				for _, field := range fields {
					if field = strings.Trim(field, `"'`); strings.HasPrefix(field, "/") {
						patterns = append(patterns, cleanLogrotatePattern{glob: field, source: source})
					}
				}
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	return patterns
}

func cleanMatchLogrotate(patterns []cleanLogrotatePattern, path string) string {
	for _, pattern := range patterns {
		if ok, err := filepath.Match(pattern.glob, path); err == nil && ok {
			return pattern.source
		}
	}
	return ""
}

// cleanOpenCounts는 paths 각각을 열고 있는 fd 수를 한 번의 /proc 순회로 센다.
func cleanOpenCounts(ctx context.Context, procDir string, logs []cleanLargeLog) (counts map[string]int, complete, readable bool) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, false, false
	}
	wanted := make(map[string]bool, len(logs))
	for _, log := range logs {
		wanted[log.Path] = true
	}
	counts, complete = map[string]int{}, true
	for _, entry := range entries {
		if ctx.Err() != nil {
			return counts, false, true
		}
		if !entry.IsDir() || !cleanNumeric(entry.Name()) {
			continue
		}
		fdDir := filepath.Join(procDir, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			if !os.IsNotExist(err) {
				complete = false
			}
			continue
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil {
				target = strings.TrimSuffix(target, " (deleted)")
				if wanted[target] {
					counts[target]++
				}
			}
		}
	}
	return counts, complete, true
}

// cleanFindSnapRevisions는 현재 리비전이 아닌 snap 파일을 큰 순서로 돌려준다. 현재 리비전을 알 수 없는 snap은 이름마다 한 번만 세어 둔다.
func cleanFindSnapRevisions(ctx context.Context, paths cleanSystemPaths) (revisions []cleanSnapRevision, unknown int) {
	entries, err := os.ReadDir(paths.snapFiles)
	if err != nil {
		return nil, 0
	}
	currents := map[string]string{}
	unreadable := map[string]bool{}
	for _, entry := range entries {
		if ctx.Err() != nil {
			break
		}
		base, ok := strings.CutSuffix(entry.Name(), ".snap")
		cut := strings.LastIndex(base, "_")
		if !ok || entry.IsDir() || cut <= 0 {
			continue
		}
		name, revision := base[:cut], base[cut+1:]
		if unreadable[name] {
			continue
		}
		current, known := currents[name]
		if !known {
			link, err := os.Readlink(filepath.Join(paths.snapMounts, name, "current"))
			if err != nil {
				unreadable[name] = true
				unknown++
				continue
			}
			current = filepath.Base(link)
			currents[name] = current
		}
		if current == revision {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		modified := info.ModTime().UTC()
		revisions = append(revisions, cleanSnapRevision{Name: name, Revision: revision, Current: current, Bytes: uint64(info.Size()),
			Modified: &modified, Cleanup: fmt.Sprintf("snap remove %s --revision=%s", name, revision)})
	}
	sort.Slice(revisions, func(i, j int) bool {
		if revisions[i].Bytes != revisions[j].Bytes {
			return revisions[i].Bytes > revisions[j].Bytes
		}
		return revisions[i].Name+revisions[i].Revision < revisions[j].Name+revisions[j].Revision
	})
	return revisions, unknown
}

func formatCleanSystem(section cleanSystem, verbose bool, width int, style cleanStyle) string {
	if section.Status != cleanMeasured && section.Status != cleanPartial {
		return ""
	}
	var builder strings.Builder
	line := func(indent, text string) { builder.WriteString(style.row(indent, text, width)) }
	builder.WriteString(style.open(T("clean.system.title"), T("clean.system.panel"), T("clean.system.note"), width))
	if section.LogsTotal > 0 {
		line("  ", T("clean.system.logs", formatSizeShort(cleanLargeLogBytes), section.LogsTotal))
		for _, log := range section.Logs {
			line("    ", style.size(cleanPadLeft(formatSizeShort(log.Bytes), 9), log.Bytes, true)+"  "+log.Path)
			for _, detail := range cleanLogDetails(log, section.LogrotateKnown) {
				line("               ", style.dim(detail))
			}
			if verbose && log.Cleanup != "" {
				line("               ", style.phrase("clean.cleanup", log.Cleanup, richCyan))
			}
		}
		if section.LogsOmitted > 0 {
			line("    ", style.dim(T("clean.system.omitted", section.LogsOmitted, formatSizeShort(section.LogsOmittedBytes))))
		}
	}
	if section.SnapsTotal > 0 {
		line("  ", T("clean.system.snaps", section.SnapsTotal))
		for _, snap := range section.Snaps {
			line("    ", style.size(cleanPadLeft(formatSizeShort(snap.Bytes), 9), snap.Bytes, true)+"  "+
				T("clean.system.snap", snap.Name, snap.Revision, snap.Current))
			if verbose {
				line("               ", style.phrase("clean.cleanup", snap.Cleanup, richCyan))
			}
		}
		if section.SnapsOmitted > 0 {
			line("    ", style.dim(T("clean.system.snaps_omitted", section.SnapsOmitted)))
		}
	}
	if section.SnapsUnknown > 0 {
		line("  ", style.warn(T("clean.system.snaps_unknown", section.SnapsUnknown)))
	}
	builder.WriteString(style.close())
	return builder.String()
}

func cleanLogDetails(log cleanLargeLog, logrotateKnown bool) []string {
	var details []string
	if log.Modified != nil {
		details = append(details, T("clean.system.modified", cleanDockerDate(log.Modified)))
	}
	switch {
	case log.OpenFiles == nil:
		details = append(details, T("clean.system.open_unknown"))
	case *log.OpenFiles > 0:
		details = append(details, T("clean.system.open_some", *log.OpenFiles))
	case log.OpenFilesComplete:
		details = append(details, T("clean.system.open_none"))
	default:
		details = append(details, T("clean.system.open_unreadable"))
	}
	switch {
	case log.Logrotate != "":
		details = append(details, T("clean.system.logrotate_match", log.Logrotate))
	case logrotateKnown:
		details = append(details, T("clean.system.logrotate_none"))
	default:
		details = append(details, T("clean.system.logrotate_unknown"))
	}
	return details
}
