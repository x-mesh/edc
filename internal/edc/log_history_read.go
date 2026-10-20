package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	historyFamilyLimit  = 1000
	historyByteLimit    = 128 * 1024 * 1024
	historyAttemptLimit = 10000
	historyMarkerLimit  = 64 * 1024
)

type logHistoryOutcome string

const (
	historyOutcomeSuccess logHistoryOutcome = "SUCCESS"
	historyOutcomeFail    logHistoryOutcome = "FAIL"
	historyOutcomeTimeout logHistoryOutcome = "TIMEOUT"
	historyOutcomeSignal  logHistoryOutcome = "SIGNAL"
	historyOutcomeError   logHistoryOutcome = "ERROR"
	historyOutcomeUnknown logHistoryOutcome = "UNKNOWN"
	historyOutcomeRunning logHistoryOutcome = "RUNNING"
)

type logHistoryAttempt struct {
	Key, CWD, Path, Display              string
	Outcome                              logHistoryOutcome
	Resources                            *logHistoryResources
	Command                              []string
	Started, Ended                       time.Time
	Duration                             time.Duration
	Exit                                 *int
	Attempt, Ordinal                     int
	damaged, processSeen, startErrorSeen bool
	open                                 bool
}

// logHistoryResources is what the end marker recorded about resource use. CPU is
// user plus system time from rusage, or the sampled CPU time of a container.
type logHistoryResources struct {
	CPU, CPUUser, CPUSys time.Duration
	MaxRSS               uint64
	Samples              int
	MemPeak, MemAvg      uint64
	CPUPeak              float64
	DiskRead, DiskWrite  uint64
	Scope, Container     string
	cpu, disk            bool
}

// memory is the peak a baseline compares: the sampled total when there is one,
// otherwise the largest single process from rusage.
func (resources *logHistoryResources) memory() uint64 {
	if resources.Samples > 0 && resources.MemPeak > 0 {
		return resources.MemPeak
	}
	return resources.MaxRSS
}

// parseHistoryResources returns nil without resource fields. A field that is
// present but malformed reports ok=false.
func parseHistoryResources(fields map[string]string) (*logHistoryResources, bool) {
	resources := &logHistoryResources{}
	found, ok := false, true
	number := func(key string, target *uint64) bool {
		value, exists := fields[key]
		if !exists {
			return false
		}
		found = true
		parsed, err := strconv.ParseUint(value, 10, 64)
		ok = ok && err == nil
		*target = parsed
		return err == nil
	}
	var user, sys, cpuTime, samples uint64
	if number("cpu_user_us", &user) && number("cpu_sys_us", &sys) {
		resources.CPUUser, resources.CPUSys = time.Duration(user)*time.Microsecond, time.Duration(sys)*time.Microsecond
		resources.CPU, resources.cpu = resources.CPUUser+resources.CPUSys, true
	}
	number("max_rss", &resources.MaxRSS)
	if number("cpu_time_us", &cpuTime) && !resources.cpu {
		resources.CPU, resources.cpu = time.Duration(cpuTime)*time.Microsecond, true
	}
	if number("samples", &samples) {
		resources.Samples = int(min(samples, 1<<31))
		number("mem_peak", &resources.MemPeak)
		number("mem_avg", &resources.MemAvg)
		peak, err := strconv.ParseFloat(fields["cpu_peak"], 64)
		ok = ok && err == nil && peak >= 0
		resources.CPUPeak = peak
		switch resources.Scope = fields["scope"]; resources.Scope {
		case logResourceScopeGroup, logResourceScopeContainer, logResourceScopeExec:
		default:
			ok = false
		}
	}
	resources.disk = number("disk_read", &resources.DiskRead) && number("disk_write", &resources.DiskWrite)
	if container, exists := fields["container"]; exists {
		resources.Container = container
		ok = ok && container != "" && strings.Trim(container, "0123456789abcdef") == ""
	}
	if !found {
		return nil, true
	}
	return resources, ok
}

func (row logHistoryAttempt) failed() bool {
	return row.Outcome != historyOutcomeSuccess && row.Outcome != historyOutcomeUnknown && row.Outcome != historyOutcomeRunning
}

type logHistorySnapshot struct {
	Rows   []logHistoryAttempt
	Issues []string
	// Notices are about how the history was read, such as an index that could
	// not be used. Unlike Issues they leave the results whole and the exit code
	// alone.
	Notices []string
	// Archive holds the daily summaries of runs that prune deleted, by key.
	Archive map[string][]logHistoryDay
}

func (snapshot *logHistorySnapshot) issue(path, kind string) {
	snapshot.Issues = append(snapshot.Issues, T("cli.log_history."+kind, reportIdentityValue(path)))
}

type historyParser struct {
	rows        []logHistoryAttempt
	current     *logHistoryAttempt
	path        string
	invalid     bool
	ordinal     int
	nextAttempt int
	holdTail    bool
	tail        string
}

func parseHistoryMarker(line string) (string, map[string]string, error) {
	const prefix = "=== edc log "
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " ===") {
		return "", nil, errors.New("invalid marker")
	}
	text := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " ===")
	kind, text, ok := strings.Cut(text, " ")
	if !ok {
		return "", nil, errors.New("missing fields")
	}
	fields := map[string]string{}
	for text != "" {
		text = strings.TrimLeft(text, " ")
		if text == "" {
			break
		}
		key, rest, found := strings.Cut(text, "=")
		if !found || key == "" || strings.ContainsAny(key, " \t\r\n") {
			return "", nil, errors.New("invalid field")
		}
		if _, exists := fields[key]; exists {
			return "", nil, errors.New("duplicate field")
		}
		if rest == "" {
			return "", nil, errors.New("missing value")
		}
		length := strings.IndexByte(rest, ' ')
		if rest[0] == '[' || rest[0] == '"' {
			decoder := json.NewDecoder(strings.NewReader(rest))
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return "", nil, err
			}
			length = int(decoder.InputOffset())
			if length < len(rest) && rest[length] != ' ' {
				return "", nil, errors.New("invalid JSON boundary")
			}
		} else if length < 0 {
			length = len(rest)
		}
		fields[key] = rest[:length]
		text = rest[length:]
	}
	return kind, fields, nil
}

func (parser *historyParser) finish() {
	if parser.current == nil {
		return
	}
	row := *parser.current
	if row.Outcome == "" || row.damaged {
		row.Outcome = historyOutcomeUnknown
	}
	parser.rows = append(parser.rows, row)
	parser.current = nil
}

func (parser *historyParser) damage() {
	parser.invalid = true
	if parser.current != nil {
		parser.current.damaged = true
	}
}

func validHistoryKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, ch := range key {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return false
		}
	}
	return true
}

func (parser *historyParser) line(line string) {
	if !strings.HasPrefix(line, "=== edc log ") {
		return
	}
	kind, fields, err := parseHistoryMarker(line)
	if err != nil {
		parser.damage()
		return
	}
	switch kind {
	case "start":
		parser.finish()
		parser.ordinal++
		row := &logHistoryAttempt{Path: parser.path, Ordinal: parser.ordinal, Attempt: max(1, parser.nextAttempt), Display: fields["command_display"]}
		parser.current = row
		parser.nextAttempt = 1
		row.Started, err = time.Parse(time.RFC3339Nano, fields["time"])
		if err != nil || row.Started.IsZero() {
			parser.damage()
		}
		if err := json.Unmarshal([]byte(fields["cwd"]), &row.CWD); err != nil {
			parser.damage()
		}
		if raw, ok := fields["command"]; ok {
			if err := json.Unmarshal([]byte(raw), &row.Command); err != nil || len(row.Command) == 0 {
				parser.damage()
			}
		}
		pid, pidErr := strconv.Atoi(fields["pid"])
		if pidErr != nil || pid < 1 {
			parser.damage()
		}
		switch fields["stream"] {
		case "stdout", "stderr", "both":
		default:
			parser.damage()
		}
		stored := fields["command_key"]
		switch row.Display {
		case "none":
			if stored != "" || len(row.Command) != 0 {
				parser.damage()
			}
		case "name":
			if len(row.Command) != 1 {
				parser.damage()
			}
			if stored != "" && validHistoryKey(stored) && fields["command_key_version"] == "1" {
				row.Key = stored
			} else if stored != "" {
				parser.damage()
			}
		case "full":
			// writeLogStart omits the key for argv that is not valid UTF-8, and JSON
			// has already replaced those bytes with U+FFFD, so the exact argv is lost.
			if stored == "" && fields["command_key_version"] == "" && slices.ContainsFunc(row.Command, func(arg string) bool { return strings.ContainsRune(arg, utf8.RuneError) }) {
				break
			}
			row.Key, err = commandKey(row.Command)
			if err != nil {
				parser.damage()
			}
			if stored != row.Key || fields["command_key_version"] != "1" {
				row.Key = ""
				parser.damage()
			}
		case "":
			if len(row.Command) > 1 || len(row.Command) == 1 && strings.ContainsRune(row.Command[0], filepath.Separator) {
				row.Key, err = commandKey(row.Command)
				if err != nil {
					parser.damage()
				}
			} else {
				row.Display = "unknown"
			}
			if stored != "" {
				row.Key = ""
				parser.damage()
			}
		default:
			parser.damage()
		}
	case "process":
		if parser.current == nil {
			parser.damage()
			return
		}
		row := parser.current
		if row.processSeen {
			parser.damage()
		}
		row.processSeen = true
		pid, pidErr := strconv.Atoi(fields["pid"])
		var executable string
		if pidErr != nil || pid < 1 || json.Unmarshal([]byte(fields["executable"]), &executable) != nil || executable == "" || row.startErrorSeen {
			parser.damage()
		}
		row.Attempt, err = strconv.Atoi(fields["attempt"])
		if err != nil || row.Attempt < 1 {
			parser.damage()
		}
	case "start_error":
		if parser.current == nil {
			parser.damage()
			return
		}
		if parser.current.processSeen || parser.current.startErrorSeen {
			parser.damage()
		}
		parser.current.startErrorSeen = true
		var cause string
		if json.Unmarshal([]byte(fields["cause"]), &cause) != nil {
			parser.damage()
		}
	case "end":
		if parser.current == nil {
			if len(parser.rows) > 0 {
				parser.rows[len(parser.rows)-1].damaged = true
				parser.rows[len(parser.rows)-1].Outcome = historyOutcomeUnknown
			}
			parser.damage()
			return
		}
		row := parser.current
		row.Ended, err = time.Parse(time.RFC3339Nano, fields["time"])
		if err != nil || row.Ended.Before(row.Started) {
			parser.damage()
		}
		row.Duration, err = time.ParseDuration(fields["duration"])
		if err != nil || row.Duration < 0 {
			parser.damage()
		}
		exit, err := strconv.Atoi(fields["exit"])
		if err != nil || exit < 0 || exit > 255 {
			parser.damage()
		} else {
			row.Exit = &exit
		}
		resources, valid := parseHistoryResources(fields)
		if !valid {
			parser.damage()
		}
		row.Resources = resources
		if row.startErrorSeen && fields["status"] != "start_error" || row.Display != "" && row.Display != "unknown" && !row.processSeen && fields["status"] != "start_error" {
			parser.damage()
		}
		switch fields["status"] {
		case "exit":
			row.Outcome = historyOutcomeSuccess
			if exit != 0 {
				row.Outcome = historyOutcomeFail
			}
		case "timeout":
			row.Outcome = historyOutcomeTimeout
			if exit != 124 {
				parser.damage()
			}
		case "signal":
			row.Outcome = historyOutcomeSignal
			if exit < 129 || fields["signal"] == "" {
				parser.damage()
			}
		case "start_error", "log_error", "wait_error", "signal_error":
			row.Outcome = historyOutcomeError
			if exit != 2 {
				parser.damage()
			}
		default:
			parser.damage()
		}
		parser.finish()
	case "restart":
		if parser.current != nil {
			parser.damage()
		}
		parser.nextAttempt, err = strconv.Atoi(fields["next_attempt"])
		if err != nil || parser.nextAttempt < 2 {
			parser.damage()
		}
	case "stopped":
		if parser.current != nil {
			parser.damage()
		}
		parser.nextAttempt = 1
	default:
		parser.damage()
	}
}

func readHistoryLines(reader io.Reader, parser *historyParser) error {
	input := bufio.NewReaderSize(reader, historyMarkerLimit)
	var line []byte
	discard := false
	for {
		chunk, err := input.ReadSlice('\n')
		if !discard {
			if len(line)+len(chunk) > historyMarkerLimit {
				if strings.HasPrefix(string(line)+string(chunk[:min(len(chunk), 16)]), "=== edc log ") {
					parser.damage()
				}
				line = nil
				discard = true
			} else {
				line = append(line, chunk...)
			}
		}
		if err != bufio.ErrBufferFull {
			if !discard && len(line) > 0 {
				text := strings.TrimRight(string(line), "\r\n")
				if err == io.EOF && parser.holdTail {
					parser.tail = text
				} else {
					parser.line(text)
				}
			}
			line = nil
			discard = false
			if len(parser.rows) > historyAttemptLimit {
				return errHistoryScanLimit
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil && err != bufio.ErrBufferFull {
			return err
		}
	}
}

type historyPart struct {
	path  string
	info  os.FileInfo
	index int
}

func historyFamily(path string) ([]historyPart, error) {
	var parts []historyPart
	base := filepath.Base(path)
	examined := 0
	err := walkHistoryDirectory(filepath.Dir(path), true, func(entry os.DirEntry) error {
		examined++
		if examined > 10000 {
			return errHistoryScanLimit
		}
		index := 0
		if entry.Name() != base {
			prefix := base + ".edc."
			if !strings.HasPrefix(entry.Name(), prefix) {
				return nil
			}
			suffix := strings.TrimPrefix(entry.Name(), prefix)
			var err error
			index, err = strconv.Atoi(suffix)
			if err != nil || index < 1 || strconv.Itoa(index) != suffix {
				return nil
			}
		}
		if len(parts) > logMaxKeepFiles {
			return errHistoryScanLimit
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink log")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("nonregular log")
		}
		parts = append(parts, historyPart{path: filepath.Join(filepath.Dir(path), entry.Name()), info: info, index: index})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].index > parts[j].index })
	if len(parts) == 0 {
		return nil, os.ErrNotExist
	}
	return parts, nil
}

func compareHistoryFamily(before, after []historyPart) (appended, ok bool) {
	if len(before) != len(after) {
		return false, false
	}
	for i, part := range before {
		other := after[i]
		if part.path != other.path || !os.SameFile(part.info, other.info) {
			return false, false
		}
		if part.info.Size() == other.info.Size() && part.info.ModTime().Equal(other.info.ModTime()) {
			continue
		}
		// edc only appends to the active file; rotation renames it and changes the
		// inode, so growth of index 0 leaves every byte read so far intact.
		if part.index != 0 || other.info.Size() <= part.info.Size() {
			return false, false
		}
		appended = true
	}
	return appended, true
}

func readHistoryFamily(path string, parts []historyPart) ([]logHistoryAttempt, bool, error) {
	var files []*os.File
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	readers := make([]io.Reader, 0, len(parts))
	for _, part := range parts {
		file, err := os.OpenFile(part.path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, false, err
		}
		files = append(files, file)
		info, err := file.Stat()
		if err != nil {
			return nil, false, err
		}
		if !info.Mode().IsRegular() || !os.SameFile(info, part.info) {
			return nil, false, errHistoryChanged
		}
		readers = append(readers, io.LimitReader(file, part.info.Size()))
	}
	parser := &historyParser{path: path, holdTail: true}
	if err := readHistoryLines(io.MultiReader(readers...), parser); err != nil {
		return nil, false, err
	}
	after, err := restatHistoryParts(parts)
	if err != nil {
		return nil, false, errHistoryChanged
	}
	appended, ok := compareHistoryFamily(parts, after)
	if !ok {
		return nil, false, errHistoryChanged
	}
	if parser.tail != "" && !appended {
		parser.line(parser.tail)
	}
	open := parser.current != nil && parser.current.Outcome == "" && !parser.current.damaged
	parser.finish()
	if open {
		parser.rows[len(parser.rows)-1].open = true
	}
	return parser.rows, parser.invalid, nil
}

var errHistoryScanLimit = errors.New("history scan limit")

// errHistoryChanged means that a log changed while it was read; reading it
// again later gives a consistent result.
var errHistoryChanged = errors.New("changed file")

// restatHistoryParts looks at the parts that were read again, one lstat each.
// edc only appends to the active file and rotates by renaming it, so a new
// archive always comes with a new inode at the base path, which this catches
// without walking the directory again.
func restatHistoryParts(parts []historyPart) ([]historyPart, error) {
	after := make([]historyPart, 0, len(parts))
	for _, part := range parts {
		info, err := os.Lstat(part.path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("nonregular log")
		}
		after = append(after, historyPart{path: part.path, info: info, index: part.index})
	}
	return after, nil
}

// historyFamilies maps a log path to its parts, newest archive last; a family
// whose parts cannot be read safely maps to nil.
type historyFamilies map[string][]historyPart

// scanHistoryDirectory adds the families of one directory in a single pass.
// Reading every family with its own walk made a refresh quadratic in the
// number of files of a folder, and its per-walk limit emptied the history of
// a folder with more than 10,000 entries.
func scanHistoryDirectory(directory string, followLink bool, families historyFamilies, examined *int, descend func(string) error) error {
	return walkHistoryDirectory(directory, followLink, func(entry os.DirEntry) error {
		*examined++
		if *examined > historyEntryLimit {
			return errHistoryScanLimit
		}
		if entry.IsDir() {
			if descend != nil {
				return descend(filepath.Join(directory, entry.Name()))
			}
			return nil
		}
		name, index := entry.Name(), 0
		if offset := strings.LastIndex(name, ".log.edc."); offset >= 0 {
			suffix := name[offset+len(".log.edc."):]
			if n, err := strconv.Atoi(suffix); err == nil && n > 0 && strconv.Itoa(n) == suffix {
				name, index = name[:offset+4], n
			}
		}
		if !strings.HasSuffix(name, ".log") {
			return nil
		}
		path := filepath.Join(directory, name)
		parts, known := families[path]
		// Discovery never follows a log symlink; an archive that is one makes
		// the family unreadable, as a direct lookup of that log would.
		if known && parts == nil || entry.Type()&os.ModeSymlink != 0 && index == 0 {
			return nil
		}
		info, err := entry.Info()
		if entry.Type()&os.ModeSymlink != 0 || err != nil || !info.Mode().IsRegular() || len(parts) > logMaxKeepFiles {
			families[path] = nil
			return nil
		}
		families[path] = append(parts, historyPart{path: filepath.Join(directory, entry.Name()), info: info, index: index})
		return nil
	})
}

func (families historyFamilies) parts(path string) ([]historyPart, error) {
	parts, ok := families[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	if parts == nil {
		return nil, errors.New("unreadable log family")
	}
	sorted := slices.Clone(parts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].index > sorted[j].index })
	return sorted, nil
}

// familyOf reports whether a scan covered path. A log outside the scanned
// directories, such as defaults.log.output, is looked up on its own.
func familyOf(families []historyFamilies, path string) ([]historyPart, bool) {
	if len(families) == 0 || families[0] == nil {
		return nil, false
	}
	parts, known := families[0][path]
	return parts, known
}

func (families historyFamilies) modified(path string) time.Time {
	var newest time.Time
	for _, part := range families[path] {
		if part.info.ModTime().After(newest) {
			newest = part.info.ModTime()
		}
	}
	return newest
}

// historyFamiliesIn scans each directory that holds one of the paths once.
func historyFamiliesIn(paths []string) historyFamilies {
	families := historyFamilies{}
	scanned := map[string]bool{}
	examined := 0
	for _, path := range paths {
		directory := filepath.Dir(path)
		if scanned[directory] {
			continue
		}
		scanned[directory] = true
		_ = scanHistoryDirectory(directory, true, families, &examined, nil)
	}
	return families
}

// A directory the user named may itself be a symlink, such as /tmp on macOS.
// Directories reached while walking are opened without following links so a
// swapped-in symlink cannot redirect discovery outside the named tree.
func walkHistoryDirectory(directory string, followLink bool, visit func(os.DirEntry) error) error {
	flags := os.O_RDONLY | syscall.O_DIRECTORY
	if !followLink {
		flags |= syscall.O_NOFOLLOW
	}
	file, err := os.OpenFile(directory, flags, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		entries, err := file.ReadDir(128)
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// historyEntryLimit bounds the directory walk. A cron job that runs every
// minute leaves one file per run in the default mode, about 43,000 a month.
const historyEntryLimit = 200000

// historyPaths returns the newest historyFamilyLimit families of root and one
// level below it, with the parts that the scan found. The walk lists every
// entry first, because directory order is not time order and a cut made while
// walking could drop the newest runs. Each entry costs one lstat.
func historyPaths(root string) ([]string, historyFamilies, error) {
	families := historyFamilies{}
	examined := 0
	err := scanHistoryDirectory(root, true, families, &examined, func(directory string) error {
		return scanHistoryDirectory(directory, false, families, &examined, nil)
	})
	paths := make([]string, 0, len(families))
	modified := map[string]time.Time{}
	for path := range families {
		paths = append(paths, path)
		modified[path] = families.modified(path)
	}
	sortHistoryNewestFirst(paths, modified)
	if len(paths) > historyFamilyLimit {
		paths, err = paths[:historyFamilyLimit], errHistoryScanLimit
	}
	return paths, families, err
}

func sortHistoryNewestFirst(paths []string, modified map[string]time.Time) {
	sort.Slice(paths, func(i, j int) bool {
		if a, b := modified[paths[i]], modified[paths[j]]; !a.Equal(b) {
			return a.After(b)
		}
		return paths[i] > paths[j]
	})
}

// logHistoryCache keeps parsed families between refreshes. An entry holds the
// parts as they were before reading, so a family that grew while it was read is
// read again on the next refresh instead of keeping its dropped tail line.
type logHistoryCache map[string]logHistoryCacheEntry

type logHistoryCacheEntry struct {
	parts   []historyPart
	rows    []logHistoryAttempt
	invalid bool
	index   *logIndexState
}

// logIndexCacheKey holds the index state in the same cache; no log path can
// start with a NUL byte.
const logIndexCacheKey = "\x00index"

func (cache logHistoryCache) indexState() *logIndexState {
	if cache == nil {
		return nil
	}
	entry := cache[logIndexCacheKey]
	if entry.index == nil {
		entry.index = &logIndexState{}
		cache[logIndexCacheKey] = entry
	}
	return entry.index
}

// collectLogHistory reads the families of paths. families holds the parts
// that a directory scan already found; without it each path is looked up on
// its own, which suits --file and a few named logs.
func collectLogHistory(paths []string, cache logHistoryCache, families ...historyFamilies) logHistorySnapshot {
	snapshot := logHistorySnapshot{}
	unique := map[string]bool{}
	var ordered []string
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			snapshot.issue(path, "read_error")
			continue
		}
		if !unique[absolute] {
			unique[absolute] = true
			ordered = append(ordered, absolute)
		}
	}
	// The byte, family, and attempt limits below stop the scan, so the newest
	// files go first and a cut drops the oldest runs.
	modified := map[string]time.Time{}
	for _, path := range ordered {
		if info, err := os.Lstat(path); err == nil {
			modified[path] = info.ModTime()
		}
	}
	sortHistoryNewestFirst(ordered, modified)
	budget := int64(historyByteLimit)
	visited := map[string]bool{}
	for index, path := range ordered {
		if index >= historyFamilyLimit {
			snapshot.issue(path, "limit_error")
			break
		}
		var parts []historyPart
		var err error
		if _, known := familyOf(families, path); known {
			parts, err = families[0].parts(path)
		} else {
			parts, err = historyFamily(path)
		}
		if err != nil {
			if errors.Is(err, errHistoryScanLimit) {
				snapshot.issue(path, "limit_error")
				break
			}
			snapshot.issue(path, "read_error")
			continue
		}
		var size int64
		for _, part := range parts {
			if part.info.Size() > budget-size {
				size = budget + 1
				break
			}
			size += part.info.Size()
		}
		if size > budget {
			snapshot.issue(path, "limit_error")
			break
		}
		budget -= size
		entry, cached := cache[path]
		if appended, ok := compareHistoryFamily(entry.parts, parts); !cached || appended || !ok {
			rows, invalid, err := readHistoryFamily(path, parts)
			if err != nil {
				if errors.Is(err, errHistoryScanLimit) {
					snapshot.issue(path, "limit_error")
					break
				}
				snapshot.issue(path, "read_error")
				continue
			}
			entry = logHistoryCacheEntry{parts: parts, rows: rows, invalid: invalid}
		}
		if cache != nil {
			cache[path] = entry
			visited[path] = true
		}
		rows, invalid := entry.rows, entry.invalid
		if len(snapshot.Rows)+len(rows) > historyAttemptLimit {
			snapshot.issue(path, "limit_error")
			break
		}
		snapshot.Rows = append(snapshot.Rows, rows...)
		if last := len(snapshot.Rows) - 1; len(rows) > 0 && snapshot.Rows[last].open && logFileLocked(path+".lock") {
			snapshot.Rows[last].Outcome = historyOutcomeRunning
		}
		if invalid {
			snapshot.issue(path, "invalid_log")
		}
	}
	for path := range cache {
		if !visited[path] && path != logIndexCacheKey {
			delete(cache, path)
		}
	}
	sort.SliceStable(snapshot.Rows, func(i, j int) bool {
		a, b := snapshot.Rows[i], snapshot.Rows[j]
		if !a.Started.Equal(b.Started) {
			return a.Started.After(b.Started)
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Ordinal > b.Ordinal
	})
	return snapshot
}

func historyCommand(row logHistoryAttempt) string {
	if len(row.Command) == 0 {
		return T("cli.log_history.hidden")
	}
	parts := make([]string, len(row.Command))
	for i, arg := range row.Command {
		if arg != "" && !strings.ContainsAny(arg, " \t\r\n\"'\\") {
			parts[i] = reportIdentityValue(arg)
		} else {
			parts[i] = strconv.QuoteToGraphic(arg)
		}
	}
	text := strings.Join(parts, " ")
	if row.Display == "name" || row.Display == "unknown" {
		text += " " + T("cli.log_history.args_unknown")
	}
	return text
}

func historyOutcome(row logHistoryAttempt) string {
	return T("cli.log_history.outcome." + strings.ToLower(string(row.Outcome)))
}

func historyTime(at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return at.Local().Format("2006-01-02 15:04:05 MST")
}

func historyDuration(row logHistoryAttempt) string {
	if row.Outcome == historyOutcomeRunning {
		return fmt.Sprint(time.Since(row.Started).Truncate(time.Second))
	}
	if row.Ended.IsZero() || row.damaged {
		return "—"
	}
	return fmt.Sprint(row.Duration)
}
