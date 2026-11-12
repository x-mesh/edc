package edc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// findDefaultTimeout는 du와 같다. 응답 없는 디렉터리는 findStall로 따로 끊으므로, 이 시간은 큰 트리 전체의 상한이다.
	findDefaultTimeout = 60 * time.Second
	// findStall은 남은 일이 응답 없는 디렉터리뿐일 때 기다리는 시간이다. 죽은 NFS에서 전체 시간 제한까지 기다리지 않게 한다.
	findStall = 5 * time.Second
	// findDefaultWorkers는 du와 같다. 운영 호스트에서 CPU를 덜 쓰는 쪽을 기본으로 둔다.
	findDefaultWorkers = 4
	// findCandidateLimit는 메모리에 두는 후보 수의 상한이다. 경로 하나에 약 100바이트를 쓰므로 400만 개면 약 400 MB다.
	findCandidateLimit = 4_000_000
	// findStdinLineLimit는 stdin 줄 하나의 상한이다. 더 긴 줄은 잘라서 후보로 쓴다.
	findStdinLineLimit = 64 << 10
)

const findUsage = "edc find [options] [pattern] [path...]"

type findOptions struct {
	common      commonOptions
	pattern     string
	paths       []string
	explicit    bool
	regex       bool
	types       map[findKind]bool
	minSize     int64
	maxSize     int64
	sizeSet     bool
	changed     time.Duration
	noIgnore    bool
	crossMounts bool
	layers      bool
	null        bool
	workers     int
	// group과 inventory는 --group이다. sizeText와 changedText는 원격으로 넘길 원래 값이다.
	group, inventory      string
	sizeText, changedText string
}

// findTypeValue는 --type이다. 여러 번 주거나 쉼표로 이을 수 있다.
type findTypeValue struct{ kinds map[findKind]bool }

func (value *findTypeValue) String() string { return "" }

func (value *findTypeValue) Set(text string) error {
	for _, word := range strings.Split(text, ",") {
		switch strings.TrimSpace(word) {
		case "f", "file":
			value.kinds[findKindFile] = true
		case "d", "dir", "directory":
			value.kinds[findKindDir] = true
		case "l", "link", "symlink":
			value.kinds[findKindLink] = true
		default:
			return errors.New(T("find.error.type", word))
		}
	}
	return nil
}

// parseFindSize는 --size다. +N이나 N은 N 이상, -N은 N 미만이다. 단위는 1024배다.
func parseFindSize(text string) (minimum, maximum int64, err error) {
	invalid := errors.New(T("find.error.size", text))
	body := text
	less := false
	switch {
	case strings.HasPrefix(body, "+"):
		body = body[1:]
	case strings.HasPrefix(body, "-"):
		body, less = body[1:], true
	}
	body = strings.TrimSuffix(strings.ToUpper(body), "B")
	unit := int64(1)
	if body != "" {
		if index := strings.Index("KMGT", body[len(body)-1:]); index >= 0 {
			unit = int64(1) << (10 * (index + 1))
			body = body[:len(body)-1]
		}
	}
	number, parseErr := strconv.ParseFloat(body, 64)
	if parseErr != nil || number < 0 || number*float64(unit) > float64(1<<62) {
		return 0, 0, invalid
	}
	limit := int64(number * float64(unit))
	if less {
		return 0, limit, nil
	}
	return limit, 0, nil
}

func parseFindOptions(args []string, stderr io.Writer, stdinPipe bool) (findOptions, error) {
	options := findOptions{common: configuredCommon(findDefaultTimeout), workers: findDefaultWorkers}
	types := &findTypeValue{kinds: map[findKind]bool{}}
	var size, changed string
	set := newFlagSet("find")
	set.SetOutput(stderr)
	bindCommon(set, &options.common)
	set.Var(types, "t", T("command.find.option.type"))
	set.Var(types, "type", T("command.find.option.type"))
	set.StringVar(&size, "size", "", T("command.find.option.size"))
	set.StringVar(&changed, "changed", "", T("command.find.option.changed"))
	set.BoolVar(&options.regex, "E", false, T("command.find.option.regex"))
	set.BoolVar(&options.regex, "regex", false, T("command.find.option.regex"))
	set.BoolVar(&options.noIgnore, "no-ignore", false, T("command.find.option.no_ignore"))
	set.BoolVar(&options.crossMounts, "cross-mounts", false, T("command.find.option.cross_mounts"))
	set.BoolVar(&options.layers, "layers", false, T("command.find.option.layers"))
	set.BoolVar(&options.null, "0", false, T("command.find.option.nul"))
	set.IntVar(&options.workers, "workers", options.workers, T("command.find.option.workers"))
	set.StringVar(&options.group, "group", "", T("command.find.option.group"))
	set.StringVar(&options.inventory, "inventory", "", T("command.find.option.inventory"))
	if err := parseReordered(set, args); err != nil {
		return options, err
	}
	if len(types.kinds) > 0 {
		options.types = types.kinds
	}
	options.sizeText, options.changedText = size, changed
	if options.group == "" && options.inventory != "" {
		err := errors.New(T("find.error.inventory_without_group"))
		fmt.Fprintln(stderr, err)
		return options, flagReported(err)
	}
	if size != "" {
		minimum, maximum, err := parseFindSize(size)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return options, flagReported(err)
		}
		options.minSize, options.maxSize, options.sizeSet = minimum, maximum, true
	}
	if changed != "" {
		age, ok := grepParseAge(changed)
		if !ok {
			err := errors.New(T("find.error.changed", changed))
			fmt.Fprintln(stderr, err)
			return options, flagReported(err)
		}
		options.changed = age
	}
	if options.workers < 1 || options.workers > 64 || options.common.timeout <= 0 {
		err := errors.New(T("find.error.range"))
		fmt.Fprintln(stderr, err)
		return options, flagReported(err)
	}
	positional := set.Args()
	switch {
	case len(positional) == 0:
	case len(positional) == 1 && (positional[0] == "." || positional[0] == ".." || strings.Contains(positional[0], "/") && findIsDir(positional[0])):
		// edc find /etc처럼 /가 든 기존 디렉터리 하나는 패턴이 아니라 찾을 곳이다. find .에 익숙한 사용자를 위해 .과 ..도 경로다.
		options.paths = positional
	default:
		options.pattern, options.paths = positional[0], positional[1:]
	}
	options.explicit = len(options.paths) > 0
	if !options.explicit {
		options.paths = nil
	}
	if !options.explicit && !stdinPipe {
		options.paths = []string{"."}
	}
	// stdin의 줄은 경로가 아닐 수 있어 종류, 크기, 시각을 보지 않는다. 조건을 주면 아무것도 맞지 않으므로 오류로 알린다.
	if !options.explicit && stdinPipe && options.group == "" && (options.types != nil || size != "" || changed != "") {
		err := errors.New(T("find.error.stdin_filter"))
		fmt.Fprintln(stderr, err)
		return options, flagReported(err)
	}
	return options, nil
}

func findIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// findStdinPipe는 stdin이 파이프나 파일인지 본다. cron의 /dev/null이나 터미널은 후보 목록이 아니다.
func findStdinPipe(stdin *os.File) bool {
	info, err := stdin.Stat()
	if err != nil {
		return false
	}
	mode := info.Mode()
	return mode&os.ModeNamedPipe != 0 || mode.IsRegular()
}

// findEnv는 명령이 쓰는 바깥 세계다. 시험이 바꿔 끼운다.
type findEnv struct {
	version        string
	stdin          *os.File
	stdout, stderr io.Writer
	stdinPipe      bool
	stdoutTerminal bool
	stderrTerminal bool
	color          bool
	sources        findSources
	now            func() time.Time
	// pick은 선택 화면이다. nil이거나 /dev/tty를 열지 못하면 목록을 쓴다.
	pick func(options findOptions, env findEnv) (int, bool)
}

func runFind(args []string, version string) int {
	color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	env := findEnv{
		version: version, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, stdinPipe: findStdinPipe(os.Stdin),
		stdoutTerminal: isTerminal(os.Stdout), stderrTerminal: isTerminal(os.Stderr), color: color,
		sources: hostFindSources(), now: time.Now, pick: findPicker,
	}
	return runFindWith(args, env)
}

// findPicker는 선택 화면이다. tui_find.go가 채운다.
var findPicker func(options findOptions, env findEnv) (int, bool)

func runFindWith(args []string, env findEnv) int {
	options, err := parseFindOptions(args, env.stderr, env.stdinPipe)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if options.group != "" {
		return runFindGroup(options, env, findRemote)
	}
	if options.pattern == "" && options.common.jsonPath == "" && env.pick != nil {
		if code, shown := env.pick(options, env); shown {
			return code
		}
	}
	return runFindList(options, env)
}

// findMatch는 목록에 쓸 후보와 점수다.
type findMatch struct {
	entry findEntry
	score int
}

// findFilter는 --type, --size, --changed다. size는 일반 파일에만 적용한다. 디렉터리의 크기는 목록의 크기라 찾는 대상이 아니다.
type findFilter struct {
	types            map[findKind]bool
	minSize, maxSize int64
	sizeSet          bool
	since            int64
}

func newFindFilter(options findOptions, now time.Time) findFilter {
	filter := findFilter{types: options.types, minSize: options.minSize, maxSize: options.maxSize, sizeSet: options.sizeSet}
	if options.changed > 0 {
		filter.since = now.Add(-options.changed).UnixNano()
	}
	return filter
}

func (filter findFilter) needsStat() bool { return filter.sizeSet || filter.since != 0 }

// apply는 묶음에서 조건에 맞는 후보만 새 묶음으로 돌려준다. 조건이 없으면 받은 묶음을 그대로 돌려준다.
func (filter findFilter) apply(batch []findEntry) []findEntry {
	if filter.types == nil && !filter.needsStat() {
		return batch
	}
	kept := make([]findEntry, 0, len(batch))
	for _, entry := range batch {
		if filter.keep(entry) {
			kept = append(kept, entry)
		}
	}
	return kept
}

func (filter findFilter) keep(entry findEntry) bool {
	if filter.types != nil && !filter.types[entry.kind] {
		return false
	}
	if filter.sizeSet {
		if entry.kind != findKindFile || entry.size < filter.minSize || filter.maxSize > 0 && entry.size >= filter.maxSize {
			return false
		}
	}
	return filter.since == 0 || entry.mtime >= filter.since
}

func runFindList(options findOptions, env findEnv) int {
	matcher, truncated, err := newFindMatcher(options.pattern, options.regex)
	if err != nil {
		fmt.Fprintln(env.stderr, T("find.error.regex", err.Error()))
		return 2
	}
	started := env.now()
	filter := newFindFilter(options, started)
	var matches []findMatch
	seen := 0
	limited := false
	keep := func(batch []findEntry) bool {
		for _, entry := range batch {
			seen++
			if !filter.keep(entry) {
				continue
			}
			score, ok := matcher.score(entry.path, findBase(entry.path))
			if !ok {
				continue
			}
			if len(matches) >= findCandidateLimit {
				limited = true
				return false
			}
			matches = append(matches, findMatch{entry: entry, score: score})
		}
		return true
	}
	var report findWalkReport
	if !options.explicit && env.stdinPipe {
		report = findReadStdin(env.stdin, keep)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), options.common.timeout)
		report = findWalk(ctx, findWalkOptions{
			roots: options.paths, noIgnore: options.noIgnore, crossMounts: options.crossMounts, layers: options.layers,
			workers: options.workers, stat: filter.needsStat(), stall: findStall,
		}, env.sources, keep)
		cancel()
	}
	findSortMatches(matches, matcher.ranked())
	elapsed := env.now().Sub(started)
	for _, problem := range report.Problems {
		fmt.Fprintln(env.stderr, "edc find: "+problem)
	}
	code := 0
	switch {
	case len(report.Problems) == len(options.paths) && len(options.paths) > 0 && seen == 0:
		code = 2
	case len(matches) == 0:
		code = 1
	}
	if options.common.jsonPath != "" {
		if err := writeFindJSON(options, env.version, report, matches, limited, truncated, elapsed); err != nil {
			fmt.Fprintln(env.stderr, err)
			return 2
		}
		return code
	}
	writer := bufio.NewWriterSize(env.stdout, 64<<10)
	for _, match := range matches {
		path := match.entry.path
		switch {
		case options.null:
			writer.WriteString(path)
			writer.WriteByte(0)
		case env.stdoutTerminal:
			writer.WriteString(findHighlight(path, matcher, env.color))
			writer.WriteByte('\n')
		default:
			writer.WriteString(path)
			writer.WriteByte('\n')
		}
	}
	if err := writer.Flush(); err != nil && !errors.Is(err, os.ErrClosed) {
		// 파이프를 먼저 닫은 head 같은 경우다. 쓰지 못한 나머지는 버린다.
		return code
	}
	findWriteNotes(env.stderr, options, report, findNoteCounts{matches: len(matches), seen: seen, elapsed: elapsed, limited: limited, truncated: truncated}, env.stderrTerminal)
	return code
}

// findSortMatches는 점수가 높은 것부터, 같으면 짧은 경로, 경로순이다. 점수가 없으면 경로순이다.
func findSortMatches(matches []findMatch, ranked bool) {
	slices.SortFunc(matches, func(a, b findMatch) int {
		if ranked {
			if a.score != b.score {
				return b.score - a.score
			}
			if len(a.entry.path) != len(b.entry.path) {
				return len(a.entry.path) - len(b.entry.path)
			}
		}
		return strings.Compare(a.entry.path, b.entry.path)
	})
}

// findHighlight는 터미널에 쓸 경로다. 제어 문자를 escape하고, color면 맞은 글자를 칠한다.
func findHighlight(path string, matcher findMatcher, color bool) string {
	spans := matcher.spans(path, findBase(path))
	if !color || len(spans) == 0 {
		return viewSanitize(path)
	}
	var builder strings.Builder
	position := 0
	for _, span := range spans {
		builder.WriteString(viewSanitize(path[position:span[0]]))
		builder.WriteString(richPaint(viewSanitize(path[span[0]:span[1]]), richRed, true))
		position = span[1]
	}
	builder.WriteString(viewSanitize(path[position:]))
	return builder.String()
}

// findReadStdin은 stdin의 줄을 후보로 넘긴다. 줄은 경로가 아닐 수 있으므로 종류를 정하지 않고 stat하지 않는다.
func findReadStdin(stdin io.Reader, emit func([]findEntry) bool) findWalkReport {
	var report findWalkReport
	reader := bufio.NewReaderSize(stdin, findStdinLineLimit)
	batch := make([]findEntry, 0, findReadBatch)
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		ok := emit(batch)
		batch = make([]findEntry, 0, findReadBatch)
		return ok
	}
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// 너무 긴 줄은 앞부분만 쓰고 나머지는 버린다. ReadSlice의 결과는 reader의 버퍼라서, 나머지를 읽기 전에 복사한다.
			line = append(line[:0:0], line...)
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
			report.Problems = append(report.Problems, T("find.note.long_line", findStdinLineLimit))
		}
		text := strings.TrimRight(string(line), "\r\n")
		if text != "" {
			batch = append(batch, findEntry{path: text, kind: findKindOther})
			if len(batch) == cap(batch) && !flush() {
				report.Stopped = true
				return report
			}
		}
		if err != nil {
			break
		}
	}
	if !flush() {
		report.Stopped = true
	}
	return report
}

type findNoteCounts struct {
	matches, seen      int
	elapsed            time.Duration
	limited, truncated bool
}

// findWriteNotes는 stderr에 결과의 범위를 알린다. 터미널이 아니면 결과를 바꾸는 사실(시간 제한, 응답 없는 디렉터리, 상한)만 쓴다.
func findWriteNotes(stderr io.Writer, options findOptions, report findWalkReport, counts findNoteCounts, terminal bool) {
	var lines []string
	if terminal {
		lines = append(lines, T("find.note.summary", counts.matches, counts.seen, counts.elapsed.Round(10*time.Millisecond)))
	}
	if terminal || options.common.verbose {
		if skipped := findSkippedSummary(report.Skipped); skipped != "" {
			lines = append(lines, T("find.note.skipped", skipped))
		}
	}
	if report.MountErr != nil && !errors.Is(report.MountErr, errors.ErrUnsupported) {
		lines = append(lines, T("find.note.mount_error", report.MountErr.Error()))
	}
	if report.UnreadableCount > 0 {
		lines = append(lines, T("find.note.unreadable", report.UnreadableCount))
	}
	if len(report.Stalled) > 0 {
		lines = append(lines, T("find.note.stalled", len(report.Stalled), strings.Join(findFirst(report.Stalled, 3), ", ")))
	} else if report.Partial {
		lines = append(lines, T("find.note.partial", options.common.timeout))
	}
	if counts.limited {
		lines = append(lines, T("find.note.limited", findCandidateLimit))
	}
	if counts.truncated {
		lines = append(lines, T("find.note.query_cut", findQueryLimit))
	}
	if options.common.verbose {
		for _, skip := range report.Skipped {
			lines = append(lines, "  "+T("find.note.skip_"+skip.Reason)+"  "+strings.TrimSpace(skip.FSType+"  "+viewSanitize(skip.Path)))
		}
		for _, path := range report.Unreadable {
			lines = append(lines, "  "+T("find.note.unreadable_path")+"  "+viewSanitize(path))
		}
	}
	for _, line := range lines {
		fmt.Fprintln(stderr, line)
	}
}

func findFirst(values []string, count int) []string {
	return values[:min(len(values), count)]
}

// findSkippedSummary는 건너뛴 디렉터리를 이유별로 센다.
func findSkippedSummary(skipped []findSkip) string {
	counts := map[string]int{}
	for _, skip := range skipped {
		counts[skip.Reason]++
	}
	var parts []string
	for _, reason := range []string{findSkipMount, findSkipPseudo, findSkipLayer} {
		if counts[reason] > 0 {
			parts = append(parts, T("find.note.count_"+reason, counts[reason]))
		}
	}
	return strings.Join(parts, ", ")
}

type findJSONMatch struct {
	Path  string     `json:"path"`
	Type  string     `json:"type"`
	Score *int       `json:"score,omitempty"`
	Size  *int64     `json:"size,omitempty"`
	MTime *time.Time `json:"mtime,omitempty"`
}

type findJSONReport struct {
	SchemaVersion   string            `json:"schema_version"`
	Tool            map[string]string `json:"tool"`
	Pattern         string            `json:"pattern"`
	Paths           []string          `json:"paths"`
	Matches         []findJSONMatch   `json:"matches"`
	Skipped         []findSkip        `json:"skipped"`
	Unreadable      []string          `json:"unreadable"`
	UnreadableCount int               `json:"unreadable_count"`
	Stalled         []string          `json:"stalled"`
	Partial         bool              `json:"partial"`
	Limited         bool              `json:"limited"`
	QueryCut        bool              `json:"query_cut"`
	MountError      string            `json:"mount_error,omitempty"`
	Problems        []string          `json:"problems"`
	DurationMS      int64             `json:"duration_ms"`
}

func findKindName(kind findKind) string {
	switch kind {
	case findKindFile:
		return "file"
	case findKindDir:
		return "dir"
	case findKindLink:
		return "link"
	}
	return "other"
}

func writeFindJSON(options findOptions, version string, report findWalkReport, matches []findMatch, limited, truncated bool, elapsed time.Duration) error {
	out := findJSONReport{
		SchemaVersion: "1.0", Tool: map[string]string{"name": "edc", "version": version}, Pattern: options.pattern, Paths: options.paths,
		Matches: make([]findJSONMatch, 0, len(matches)), Skipped: report.Skipped, Unreadable: report.Unreadable,
		UnreadableCount: report.UnreadableCount, Stalled: report.Stalled, Partial: report.Partial, Limited: limited,
		QueryCut: truncated, Problems: report.Problems, DurationMS: elapsed.Milliseconds(),
	}
	if report.MountErr != nil && !errors.Is(report.MountErr, errors.ErrUnsupported) {
		out.MountError = report.MountErr.Error()
	}
	ranked := options.pattern != "" && !options.regex
	for _, match := range matches {
		item := findJSONMatch{Path: match.entry.path, Type: findKindName(match.entry.kind)}
		if ranked {
			score := match.score
			item.Score = &score
		}
		if match.entry.statted {
			size, mtime := match.entry.size, time.Unix(0, match.entry.mtime).UTC()
			item.Size, item.MTime = &size, &mtime
		}
		out.Matches = append(out.Matches, item)
	}
	for _, field := range []*[]string{&out.Unreadable, &out.Stalled, &out.Problems} {
		if *field == nil {
			*field = []string{}
		}
	}
	if out.Skipped == nil {
		out.Skipped = []findSkip{}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if options.common.redact {
		data = []byte(redactIPAddresses(string(data)))
	}
	return writeJSONOutput(options.common.jsonPath, json.RawMessage(data))
}
