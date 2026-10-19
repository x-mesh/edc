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
	// buildx 로컬 캐시는 홈 아래 이 깊이까지만 찾는다. ~/.devbox-cache/buildx-rust가 2단계다.
	cleanBuildCacheDepth = 2
	// 찾는 동안 열어 보는 폴더 수의 상한이다. 홈이 아주 크면 여기서 멈추고 찾은 만큼만 보인다.
	cleanBuildCacheExamined = 50000
)

// cleanJournaldConfigs는 journald 설정을 읽는 순서다. 디렉터리는 안의 *.conf를 이름 순으로 읽는다.
var cleanJournaldConfigs = []string{"/usr/lib/systemd/journald.conf.d", "/etc/systemd/journald.conf", "/etc/systemd/journald.conf.d"}

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
	BuildCaches      []cleanBuildCache   `json:"build_caches,omitempty"`
	BuildCachesTotal int                 `json:"build_caches_total,omitempty"`
	BuildCachesBytes uint64              `json:"build_caches_bytes,omitempty"`
	BuildCachesMore  int                 `json:"build_caches_omitted,omitempty"`
}

// cleanBuildCache는 docker buildx build --cache-to type=local이 쓴 OCI 레이아웃 폴더다.
type cleanBuildCache struct {
	Path          string     `json:"path"`
	Bytes         uint64     `json:"bytes"`
	NewestWrite   *time.Time `json:"newest_write,omitempty"`
	NewestPartial bool       `json:"newest_partial,omitempty"`
	// OpenFiles가 nil이면 /proc을 읽지 못한 것이다.
	OpenFiles *int   `json:"open_files,omitempty"`
	Cleanup   string `json:"cleanup,omitempty"`
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
func cleanJournaldDetail(paths ...string) func() string {
	return func() string {
		found := map[string]string{}
		for _, path := range paths {
			for _, file := range cleanConfigFiles(path) {
				text, err := os.ReadFile(file)
				if err != nil {
					continue
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

// cleanConfigFiles는 파일이면 그 파일을, 디렉터리이면 안의 *.conf를 이름 순으로 돌려준다.
func cleanConfigFiles(path string) []string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return []string{path}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	return files
}

func cleanSystemScan(ctx context.Context, deps cleanDeps, goos string, paths cleanSystemPaths) cleanSystem {
	if goos != "linux" {
		return cleanSystem{}
	}
	if err := ctx.Err(); err != nil {
		return cleanSystem{Status: cleanNotScanned}
	}
	section := cleanSystem{Status: cleanAbsent}
	patterns, known := cleanLogrotatePatterns(paths)
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
	snaps, unknown := cleanFindSnapRevisions(paths)
	section.SnapsUnknown = unknown
	section.SnapsTotal = len(snaps)
	for index, snap := range snaps {
		if index >= cleanSystemLimit {
			section.SnapsOmitted++
			continue
		}
		section.Snaps = append(section.Snaps, snap)
	}
	caches := cleanFindBuildCaches(ctx, deps)
	section.BuildCachesTotal = len(caches)
	for index, cache := range caches {
		section.BuildCachesBytes += cache.Bytes
		if index >= cleanSystemLimit {
			section.BuildCachesMore++
			continue
		}
		section.BuildCaches = append(section.BuildCaches, cache)
	}
	cleanDescribeBuildCaches(ctx, section.BuildCaches)
	if len(logs) > 0 || len(snaps) > 0 || unknown > 0 || len(caches) > 0 {
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

// cleanLogrotatePatterns는 logrotate 설정에서 규칙의 로그 경로 패턴을 모은다. 설정을 하나도 읽지 못하면 known이 false다.
func cleanLogrotatePatterns(paths cleanSystemPaths) (patterns []cleanLogrotatePattern, known bool) {
	files := cleanConfigFilesAny(paths.logrotateConf)
	if entries, err := os.ReadDir(paths.logrotateDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || strings.Contains(name, ".dpkg-") || strings.Contains(name, ".rpm") {
				continue
			}
			files = append(files, filepath.Join(paths.logrotateDir, name))
		}
	}
	for _, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		known = true
		patterns = append(patterns, cleanParseLogrotate(string(text), file)...)
	}
	return patterns, known
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

// cleanFindBuildCaches는 홈 아래에서 OCI 이미지 레이아웃 폴더를 찾아 크기가 큰 순서로 돌려준다. 이름이 아니라 구조로 찾으므로
// 도구나 폴더 이름과 상관없이 buildx의 로컬 캐시 내보내기를 잡는다. 심볼릭 링크는 따라가지 않는다.
func cleanFindBuildCaches(ctx context.Context, deps cleanDeps) []cleanBuildCache {
	if deps.home == "" || deps.measure == nil {
		return nil
	}
	var paths []string
	examined := 0
	var visit func(dir string, depth int)
	visit = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if ctx.Err() != nil || examined >= cleanBuildCacheExamined {
				return
			}
			if !entry.IsDir() || entry.Name() == ".git" || entry.Name() == "node_modules" {
				continue
			}
			examined++
			path := filepath.Join(dir, entry.Name())
			if cleanIsOCILayout(path) {
				paths = append(paths, path)
			} else if depth < cleanBuildCacheDepth {
				visit(path, depth+1)
			}
		}
	}
	visit(deps.home, 1)
	var caches []cleanBuildCache
	for _, path := range paths {
		scan, err := deps.measure(ctx, duOptions{root: path, workers: duDefaultWorkers}, deps.sources)
		if err != nil {
			continue
		}
		caches = append(caches, cleanBuildCache{Path: path, Bytes: scan.root.total})
	}
	sort.Slice(caches, func(i, j int) bool {
		if caches[i].Bytes != caches[j].Bytes {
			return caches[i].Bytes > caches[j].Bytes
		}
		return caches[i].Path < caches[j].Path
	})
	return caches
}

// cleanIsOCILayout은 OCI 이미지 레이아웃의 세 구성 요소가 모두 있는지 본다.
func cleanIsOCILayout(path string) bool {
	for _, name := range []string{"oci-layout", "index.json"} {
		if info, err := os.Stat(filepath.Join(path, name)); err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	info, err := os.Stat(filepath.Join(path, "blobs"))
	return err == nil && info.IsDir()
}

// cleanDescribeBuildCaches는 보일 캐시에만 마지막 쓰기와 열린 파일을 채운다. 열린 파일이 없을 때만 명령을 둔다.
func cleanDescribeBuildCaches(ctx context.Context, caches []cleanBuildCache) {
	for index := range caches {
		cache := &caches[index]
		newest, partial := cleanNewestWrite(ctx, cache.Path)
		if !newest.IsZero() {
			utc := newest.UTC()
			cache.NewestWrite = &utc
		}
		cache.NewestPartial = partial
		count, _, readable := cleanOpenUnder(ctx, cleanProcDir, cache.Path)
		if !readable {
			continue
		}
		cache.OpenFiles = &count
		if count == 0 {
			cache.Cleanup = "rm -rf " + cleanShellQuote(cache.Path)
		}
	}
}

// cleanFindSnapRevisions는 현재 리비전이 아닌 snap 파일을 큰 순서로 돌려준다. 현재 리비전을 알 수 없는 snap은 세어 두기만 한다.
func cleanFindSnapRevisions(paths cleanSystemPaths) (revisions []cleanSnapRevision, unknown int) {
	entries, err := os.ReadDir(paths.snapFiles)
	if err != nil {
		return nil, 0
	}
	for _, entry := range entries {
		base, ok := strings.CutSuffix(entry.Name(), ".snap")
		cut := strings.LastIndex(base, "_")
		if !ok || entry.IsDir() || cut <= 0 {
			continue
		}
		name, revision := base[:cut], base[cut+1:]
		link, err := os.Readlink(filepath.Join(paths.snapMounts, name, "current"))
		if err != nil {
			unknown++
			continue
		}
		current := filepath.Base(link)
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

func formatCleanSystem(section cleanSystem, verbose bool, width int, home string, style cleanStyle) string {
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
	if section.BuildCachesTotal > 0 {
		line("  ", T("clean.system.build_caches", section.BuildCachesTotal, formatSizeShort(section.BuildCachesBytes)))
		for _, cache := range section.BuildCaches {
			line("    ", style.size(cleanPadLeft(formatSizeShort(cache.Bytes), 9), cache.Bytes, true)+"  "+cleanHomePath(cache.Path, home))
			for _, detail := range cleanBuildCacheDetails(cache) {
				line("               ", style.dim(detail))
			}
			if verbose && cache.Cleanup != "" {
				line("               ", style.phrase("clean.cleanup", cache.Cleanup, richCyan))
			}
		}
		if section.BuildCachesMore > 0 {
			line("    ", style.dim(T("clean.system.build_caches_omitted", section.BuildCachesMore)))
		}
		line("  ", style.dim(T("clean.system.build_caches_note")))
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

func cleanBuildCacheDetails(cache cleanBuildCache) []string {
	var details []string
	if cache.NewestWrite != nil {
		text := T("clean.system.modified", cleanDockerDate(cache.NewestWrite))
		if cache.NewestPartial {
			text = "≥ " + text
		}
		details = append(details, text)
	}
	switch {
	case cache.OpenFiles == nil:
		details = append(details, T("clean.system.open_unknown"))
	case *cache.OpenFiles > 0:
		details = append(details, T("clean.system.open_some", *cache.OpenFiles))
	default:
		details = append(details, T("clean.system.open_none"))
	}
	return details
}
