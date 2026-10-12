package edc

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// changes는 장애 직전에 호스트에서 무엇이 바뀌었는지를 읽기만 해서 보인다. 패키지 로그, /etc의 mtime,
// systemd unit 상태만 본다. dnf history나 journalctl처럼 느린 명령은 쓰지 않는다.

const (
	// changesEtcEntryBudget은 /etc를 도는 항목 수의 상한이다. 보통 /etc는 수천 개라 이 안에 끝난다.
	// 설정 관리 도구가 큰 트리를 만들어 둔 호스트에서도 1초 안에 끝내려고 멈추고 잘렸다고 알린다.
	changesEtcEntryBudget = 20000
	// changesTableLimit은 터미널 표의 줄 수 상한이다. -v와 JSON은 모두 담는다.
	changesTableLimit   = 50
	changesLogLineLimit = 1 << 20
)

// changeEvent는 시간 창 안에서 일어난 변화 하나다.
type changeEvent struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	Detail string    `json:"detail,omitempty"`
	Source string    `json:"source"`
}

// changesSources는 changes가 읽는 원천이다. 테스트는 root를 임시 디렉터리로 바꾸고 함수들을 가짜로 넣는다.
// systemctl이 nil이면 systemd가 없는 호스트다.
type changesSources struct {
	root      string
	now       time.Time
	systemctl func(ctx context.Context, args ...string) ([]byte, error)
	bootTime  func() (time.Time, error)
}

func runChanges(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("changes", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	since := set.Duration("since", 24*time.Hour, T("command.changes.option.since"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 || *since <= 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc changes [--since 24h] [options]"))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	sources, ok := hostChangesSources()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("changes", T("changes.skip.linux_only"))}, options.redact))
	}
	results := evaluateChanges(ctx, sources, *since)
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, nil, results, options.redact))
	}
	color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	var output io.Writer = os.Stdout
	var buffer strings.Builder
	if options.redact {
		output = &buffer
	}
	for _, result := range results {
		fmt.Fprint(output, formatResultLine(result, color))
		printResultDetail(output, result, false, color)
	}
	if table := formatChangesTable(changesEventsOf(results), options.verbose); table != "" {
		fmt.Fprint(output, "\n"+table)
	}
	if options.redact {
		fmt.Fprint(os.Stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode(results)
}

func (sources changesSources) file(path string) string { return filepath.Join(sources.root, path) }

// changesSourceResult는 원천 하나를 읽은 결과다. skip이 비어 있지 않으면 그 원천을 읽지 못한 것이다.
type changesSourceResult struct {
	events    []changeEvent
	warnings  []string
	summary   string
	truncated bool
	skip      string
}

func sortChangeEvents(events []changeEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Time.Equal(events[j].Time) {
			return events[i].Time.After(events[j].Time)
		}
		if events[i].Kind != events[j].Kind {
			return events[i].Kind < events[j].Kind
		}
		return events[i].Name < events[j].Name
	})
}

// changesSkip은 원천을 읽지 못한 이유를 고른다. 권한 때문이면 root가 필요하다고 알린다.
// 읽지 못한 원천을 빈 pass로 보이면 "바뀐 것 없음"으로 오해하므로 언제나 skip으로 둔다.
func changesSkip(source string, err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return T("changes.skip.needs_root", source)
	}
	return T("changes.skip.unreadable", source, infoSingleLine(err.Error()))
}

// evaluateChanges는 세 원천을 읽어 Result로 만든다. 각 Result의 Metrics["events"]에 그 원천의 변화를 담는다.
func evaluateChanges(ctx context.Context, sources changesSources, since time.Duration) []Result {
	cutoff := sources.now.Add(-since)
	started := time.Now()
	packages := changesResult("packages", started, collectPackageChanges(sources, cutoff))
	started = time.Now()
	etc := changesResult("etc", started, collectEtcChanges(sources.root, cutoff, changesEtcEntryBudget))
	started = time.Now()
	services := changesResult("services", started, collectServiceChanges(ctx, sources, cutoff))
	return []Result{packages, etc, services}
}

func changesResult(id string, started time.Time, source changesSourceResult) Result {
	result := Result{Probe: "changes." + id, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Warnings: source.warnings}
	if source.skip != "" {
		result.Status = StatusSkip
		result.Summary = source.skip
		return result
	}
	sortChangeEvents(source.events)
	if source.events == nil {
		source.events = []changeEvent{}
	}
	result.Status = StatusPass
	if source.truncated {
		result.Status = StatusWarn
	}
	result.Summary = source.summary
	result.Metrics = map[string]interface{}{"count": len(source.events), "events": source.events}
	return result
}

// changesEventsOf는 Result들에 담긴 변화를 한 목록으로 모아 최근 것부터 놓는다.
func changesEventsOf(results []Result) []changeEvent {
	var events []changeEvent
	for _, result := range results {
		if list, ok := result.Metrics["events"].([]changeEvent); ok {
			events = append(events, list...)
		}
	}
	sortChangeEvents(events)
	return events
}

// changesPackageLog는 패키지 관리자가 남기는 로그 하나다. 압축된 회전 로그는 풀지 않는다.
type changesPackageLog struct {
	base   string
	source string
	parse  func(io.Reader, changesSources, time.Time) ([]changeEvent, error)
}

var changesPackageLogs = []changesPackageLog{
	{"/var/log/dpkg.log", "dpkg", func(r io.Reader, s changesSources, cutoff time.Time) ([]changeEvent, error) {
		return parseDpkgLog(r, s.now.Location(), cutoff)
	}},
	{"/var/log/dnf.rpm.log", "dnf", func(r io.Reader, _ changesSources, cutoff time.Time) ([]changeEvent, error) {
		return parseDnfRPMLog(r, cutoff)
	}},
	{"/var/log/yum.log", "yum", func(r io.Reader, s changesSources, cutoff time.Time) ([]changeEvent, error) {
		return parseYumLog(r, s.now, cutoff)
	}},
}

func collectPackageChanges(sources changesSources, cutoff time.Time) changesSourceResult {
	var result changesSourceResult
	var checked, read, unread []string
	var unreadErrs []error
	for _, log := range changesPackageLogs {
		checked = append(checked, log.base)
		files, compressed, err := packageLogFiles(sources.root, log.base, cutoff)
		for _, path := range compressed {
			result.warnings = append(result.warnings, T("changes.warning.compressed", path))
		}
		if err != nil {
			unread, unreadErrs = append(unread, log.base), append(unreadErrs, err)
		}
		for _, path := range files {
			events, err := readPackageLog(sources, log, path, cutoff)
			if err != nil {
				unread, unreadErrs = append(unread, path), append(unreadErrs, err)
				continue
			}
			read = append(read, path)
			result.events = append(result.events, events...)
		}
	}
	switch {
	case len(read) == 0 && len(unread) > 0:
		result.skip = changesSkip(unread[0], unreadErrs[0])
	case len(read) == 0:
		result.skip = T("changes.skip.none_found", strings.Join(checked, ", "))
	default:
		for index, path := range unread {
			result.warnings = append(result.warnings, changesSkip(path, unreadErrs[index]))
		}
		result.summary = T("changes.summary.packages", len(result.events), strings.Join(read, ", "))
	}
	return result
}

func readPackageLog(sources changesSources, log changesPackageLog, path string, cutoff time.Time) ([]changeEvent, error) {
	file, err := os.Open(sources.file(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events, err := log.parse(file, sources, cutoff)
	if err != nil {
		return nil, err
	}
	for index := range events {
		events[index].Source = log.source
	}
	return events, nil
}

// packageLogFiles는 읽을 로그를 고른다. 본 로그와, mtime이 창 안에 있는 압축되지 않은 회전 로그다.
// 회전 로그의 mtime은 마지막으로 쓴 때이므로 그보다 오래된 파일에는 창 안의 기록이 없다.
// 압축된 회전 로그는 풀지 않고, mtime이 창 안이면 놓친 기록이 있을 수 있어 따로 돌려준다.
// 본 로그가 없으면 files에 넣지 않는다. 본 로그를 stat하지 못하면 err를 돌려준다.
func packageLogFiles(root, base string, cutoff time.Time) (files, compressed []string, err error) {
	if _, statErr := os.Stat(filepath.Join(root, base)); statErr == nil {
		files = append(files, base)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		err = statErr
	}
	dir, name := filepath.Dir(base), filepath.Base(base)
	entries, readErr := os.ReadDir(filepath.Join(root, dir))
	if readErr != nil {
		return files, compressed, err
	}
	for _, entry := range entries {
		sibling := entry.Name()
		rest, ok := strings.CutPrefix(sibling, name)
		if !ok || len(rest) < 2 || (rest[0] != '.' && rest[0] != '-') || !entry.Type().IsRegular() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, sibling)
		if isCompressedLog(sibling) {
			compressed = append(compressed, path)
			continue
		}
		files = append(files, path)
	}
	return files, compressed, err
}

func isCompressedLog(name string) bool {
	for _, suffix := range []string{".gz", ".xz", ".bz2", ".zst", ".lz4", ".Z"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func scanChangesLog(r io.Reader, line func(string)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), changesLogLineLimit)
	for scanner.Scan() {
		line(scanner.Text())
	}
	return scanner.Err()
}

// parseDpkgLog는 "2024-01-15 10:23:45 upgrade libc6:amd64 2.35-0ubuntu3.5 2.35-0ubuntu3.6" 꼴을 읽는다.
// dpkg는 현지 시각으로 적는다. status, configure, trigproc 줄과 깨진 줄은 건너뛴다.
func parseDpkgLog(r io.Reader, loc *time.Location, cutoff time.Time) ([]changeEvent, error) {
	var events []changeEvent
	err := scanChangesLog(r, func(line string) {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			return
		}
		switch fields[2] {
		case "install", "upgrade", "remove", "purge":
		default:
			return
		}
		at, err := time.ParseInLocation("2006-01-02 15:04:05", fields[0]+" "+fields[1], loc)
		if err != nil || at.Before(cutoff) {
			return
		}
		previous, next := fields[4], fields[5]
		detail := next
		switch fields[2] {
		case "upgrade":
			detail = previous + " -> " + next
		case "remove", "purge":
			detail = previous
		}
		events = append(events, changeEvent{Time: at, Kind: fields[2], Name: fields[3], Detail: detail})
	})
	return events, err
}

// changesDnfVerbs는 dnf.rpm.log의 동사를 종류로 바꾼다. Upgraded와 Downgraded는 같은 트랜잭션에서
// 빠지는 옛 패키지라 넣지 않는다. 넣으면 한 번의 업그레이드가 두 줄이 된다.
var changesDnfVerbs = map[string]string{
	"Installed":   "install",
	"Upgrade":     "upgrade",
	"Downgrade":   "downgrade",
	"Reinstall":   "reinstall",
	"Reinstalled": "reinstall",
	"Erase":       "remove",
	"Erased":      "remove",
}

// parseDnfRPMLog는 "2024-01-15T10:23:45+0000 SUBDEBUG Upgrade: bash-5.1.8-6.el9.x86_64" 꼴을 읽는다.
// dnf 버전에 따라 오프셋을 Z나 +0000이나 +00:00으로 적는다.
func parseDnfRPMLog(r io.Reader, cutoff time.Time) ([]changeEvent, error) {
	var events []changeEvent
	err := scanChangesLog(r, func(line string) {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "SUBDEBUG" {
			return
		}
		kind, ok := changesDnfVerbs[strings.TrimSuffix(fields[2], ":")]
		if !ok || !strings.HasSuffix(fields[2], ":") {
			return
		}
		var at time.Time
		var err error
		for _, layout := range []string{"2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07:00"} {
			if at, err = time.Parse(layout, fields[0]); err == nil {
				break
			}
		}
		if err != nil || at.Before(cutoff) {
			return
		}
		name, version := splitNEVRA(fields[3])
		events = append(events, changeEvent{Time: at, Kind: kind, Name: name, Detail: version})
	})
	return events, err
}

var changesYumVerbs = map[string]string{
	"Installed": "install",
	"Updated":   "upgrade",
	"Erased":    "remove",
}

// parseYumLog는 "Jan 15 10:23:45 Installed: bash-4.2.46-34.el7.x86_64" 꼴을 읽는다. yum.log에는 연도가 없어
// 지금 연도를 붙이고, 그 결과가 지금보다 뒤면 해가 바뀌기 전의 기록이라 한 해를 뺀다.
func parseYumLog(r io.Reader, now, cutoff time.Time) ([]changeEvent, error) {
	var events []changeEvent
	err := scanChangesLog(r, func(line string) {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return
		}
		kind, ok := changesYumVerbs[strings.TrimSuffix(fields[3], ":")]
		if !ok || !strings.HasSuffix(fields[3], ":") {
			return
		}
		at, err := time.ParseInLocation("Jan 2 15:04:05 2006", strings.Join(append(fields[:3:3], strconv.Itoa(now.Year())), " "), now.Location())
		if err != nil {
			return
		}
		if at.After(now) {
			at = at.AddDate(-1, 0, 0)
		}
		if at.Before(cutoff) {
			return
		}
		name, version := splitNEVRA(fields[4])
		events = append(events, changeEvent{Time: at, Kind: kind, Name: name, Detail: version})
	})
	return events, err
}

// splitNEVRA는 "bash-5.1.8-6.el9.x86_64"를 이름 bash와 버전 5.1.8-6.el9로 나눈다.
// yum의 Erased처럼 이름만 있거나 꼴이 다르면 전체를 이름으로 둔다.
func splitNEVRA(nevra string) (string, string) {
	rest := nevra
	if dot := strings.LastIndexByte(rest, '.'); dot > 0 {
		rest = rest[:dot]
	}
	release := strings.LastIndexByte(rest, '-')
	if release <= 0 {
		return nevra, ""
	}
	version := strings.LastIndexByte(rest[:release], '-')
	if version <= 0 {
		return nevra, ""
	}
	return rest[:version], rest[version+1:]
}

// collectEtcChanges는 <root>/etc에서 mtime이 창 안인 일반 파일과 symlink를 찾는다. 파일 내용은 열지 않고
// lstat만 쓴다. WalkDir는 symlink를 따라가지 않으므로 고리가 있어도 끝난다.
func collectEtcChanges(root string, cutoff time.Time, budget int) changesSourceResult {
	var result changesSourceResult
	etc := filepath.Join(root, "etc")
	var rootErr error
	entries, unreadable := 0, 0
	_ = filepath.WalkDir(etc, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == etc {
				rootErr = err
				return fs.SkipAll
			}
			unreadable++
			return nil
		}
		if path == etc {
			return nil
		}
		if entries >= budget {
			result.truncated = true
			return fs.SkipAll
		}
		entries++
		if !entry.Type().IsRegular() && entry.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		name := "/" + filepath.ToSlash(relative)
		event := changeEvent{Time: info.ModTime(), Kind: "file", Name: name, Source: "etc"}
		if entry.Type()&fs.ModeSymlink != 0 {
			event.Detail = "symlink"
		}
		result.events = append(result.events, event)
		return nil
	})
	if rootErr != nil {
		result.skip = changesSkip("/etc", rootErr)
		return result
	}
	if unreadable > 0 {
		result.warnings = append(result.warnings, T("changes.warning.unreadable_dirs", unreadable))
	}
	if result.truncated {
		result.summary = T("changes.summary.etc_truncated", len(result.events), budget)
		return result
	}
	result.summary = T("changes.summary.etc", len(result.events), entries)
	return result
}

// changesServiceUnit은 systemctl show가 준 unit 하나의 값이다. 시각은 부팅 뒤 지난 마이크로초다.
type changesServiceUnit struct {
	ID        string
	StartMono uint64
	Restarts  int
}

// parseSystemctlShow는 빈 줄로 나뉜 key=value 블록을 읽는다. 요청한 숫자 속성만 보고, 0이거나 읽을 수 없는
// 시각은 버린다. 그런 unit은 이번 부팅에서 시작한 적이 없다.
func parseSystemctlShow(text string) []changesServiceUnit {
	var units []changesServiceUnit
	var unit changesServiceUnit
	flush := func() {
		if unit.ID != "" && unit.StartMono > 0 {
			units = append(units, unit)
		}
		unit = changesServiceUnit{}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Id":
			unit.ID = value
		case "ExecMainStartTimestampMonotonic":
			unit.StartMono, _ = strconv.ParseUint(value, 10, 64)
		case "NRestarts":
			unit.Restarts, _ = strconv.Atoi(value)
		}
	}
	flush()
	return units
}

// parseSystemctlMonotonic은 manager 속성 하나의 값을 읽는다. 없거나 읽을 수 없으면 0이다.
func parseSystemctlMonotonic(text, key string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			parsed, _ := strconv.ParseUint(value, 10, 64)
			return parsed
		}
	}
	return 0
}

// parseProcStatBtime은 /proc/stat의 btime 줄에서 부팅 시각을 읽는다.
func parseProcStatBtime(data []byte) (time.Time, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			seconds, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(seconds, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime line")
}

// parseSystemctlUnits는 list-units --plain --no-legend 출력에서 service unit 이름을 모은다.
func parseSystemctlUnits(text string) []string {
	var units []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		// --plain이 없는 판에서는 실패한 unit 앞에 ●가 붙는다.
		if len(fields) > 0 && fields[0] == "●" {
			fields = fields[1:]
		}
		if len(fields) > 0 && strings.HasSuffix(fields[0], ".service") {
			units = append(units, fields[0])
		}
	}
	return units
}

// collectServiceChanges는 unit 상태에서 마지막 시작 시각을 읽는다. unit마다 시작 시각은 하나만 남으므로
// 여러 번 재시작했으면 NRestarts로 알린다. 부팅이 창 안이면 부팅 중에 시작한 service를 boot 한 줄로 묶는다.
func collectServiceChanges(ctx context.Context, sources changesSources, cutoff time.Time) changesSourceResult {
	var result changesSourceResult
	if sources.systemctl == nil {
		result.skip = T("changes.skip.no_systemd")
		return result
	}
	if sources.bootTime == nil {
		result.skip = changesSkip("/proc/stat", errors.New("no boot time"))
		return result
	}
	boot, err := sources.bootTime()
	if err != nil {
		result.skip = changesSkip("/proc/stat", err)
		return result
	}
	manager, err := sources.systemctl(ctx, "show", "--property=FinishTimestampMonotonic")
	if err != nil {
		result.skip = changesSkip("systemctl show", err)
		return result
	}
	listed, err := sources.systemctl(ctx, "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		result.skip = changesSkip("systemctl list-units", err)
		return result
	}
	names := parseSystemctlUnits(string(listed))
	if len(names) == 0 {
		result.skip = T("changes.skip.none_found", "systemctl list-units --type=service")
		return result
	}
	args := append([]string{"show", "--property=Id,ExecMainStartTimestampMonotonic,NRestarts", "--"}, names...)
	shown, err := sources.systemctl(ctx, args...)
	if err != nil {
		result.skip = changesSkip("systemctl show", err)
		return result
	}
	finish := parseSystemctlMonotonic(string(manager), "FinishTimestampMonotonic")
	bootInWindow := !boot.Before(cutoff)
	folded := 0
	for _, unit := range parseSystemctlShow(string(shown)) {
		if bootInWindow && finish > 0 && unit.StartMono <= finish {
			folded++
			continue
		}
		started := boot.Add(time.Duration(unit.StartMono) * time.Microsecond)
		if started.Before(cutoff) {
			continue
		}
		event := changeEvent{Time: started, Kind: "service", Name: unit.ID, Source: "systemd"}
		if unit.Restarts > 0 {
			event.Detail = T("changes.detail.restarts", unit.Restarts)
		}
		result.events = append(result.events, event)
	}
	if bootInWindow {
		result.events = append(result.events, changeEvent{Time: boot, Kind: "boot", Name: "boot", Detail: T("changes.detail.boot", folded), Source: "systemd"})
	}
	result.summary = T("changes.summary.services", len(result.events), len(names))
	return result
}

// formatChangesTable은 시각, 종류, 이름, 내용을 최근 것부터 그린다. all이 아니면 changesTableLimit 줄에서
// 자르고 남은 수를 알린다. 마지막 열은 채우지 않아 줄이 필요 이상 넓어지지 않는다.
func formatChangesTable(events []changeEvent, all bool) string {
	if len(events) == 0 {
		return ""
	}
	shown := events
	if !all && len(shown) > changesTableLimit {
		shown = shown[:changesTableLimit]
	}
	headers := []string{T("changes.column.time"), T("changes.column.kind"), T("changes.column.name"), T("changes.column.detail")}
	rows := make([][]string, 0, len(shown))
	for _, event := range shown {
		rows = append(rows, []string{event.Time.Format("2006-01-02 15:04:05"), event.Kind, event.Name, listenCell(event.Detail)})
	}
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = liveWidth(header)
	}
	for _, row := range rows {
		for index, cell := range row {
			if width := liveWidth(cell); width > widths[index] {
				widths[index] = width
			}
		}
	}
	var builder strings.Builder
	writeRow := func(cells []string) {
		for index, cell := range cells {
			if index == len(cells)-1 {
				builder.WriteString(cell)
				break
			}
			builder.WriteString(liveCell(cell, widths[index]+2))
		}
		builder.WriteString("\n")
	}
	writeRow(headers)
	for _, row := range rows {
		writeRow(row)
	}
	if hidden := len(events) - len(shown); hidden > 0 {
		builder.WriteString(T("changes.more", hidden))
		builder.WriteString("\n")
	}
	return builder.String()
}
