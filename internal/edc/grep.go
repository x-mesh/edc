package edc

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const grepUsage = "edc grep [options] <pattern> [path...]"

const (
	// grepBinaryProbe는 binary 파일인지 보는 앞부분 길이다. NUL이 있으면 건너뛴다.
	grepBinaryProbe = 8 << 10
	// grepContextWindow는 -C의 앞뒤 줄을 읽는 범위다.
	grepContextWindow = 64 << 10
	// grepHintNames는 건너뛴 파일 이름을 hint에 보여 주는 최대 수다.
	grepHintNames = 5
)

type grepIndexMode int

const (
	grepIndexAuto grepIndexMode = iota
	grepIndexForce
	grepIndexOff
)

type grepOptions struct {
	pattern    string
	codeMode   bool
	askMode    bool
	askKeyword bool
	semantic   bool
	askJSON    bool
	askLimit   int
	pathMode   bool
	pathRegex  *regexp.Regexp
	hidden     bool
	noIgnore   bool
	regex      bool
	ignoreCase bool
	numbers    bool
	listFiles  bool
	context    int
	minLevel   int
	statuses   *grepStatusSet
	since      time.Time
	until      time.Time
	last       int
	// ordered는 로그가 시간 순서라는 뜻이다. 그때만 --since가 파일을 이진 탐색하고 끝 시각으로 파일을 건너뛴다.
	ordered bool
	index   grepIndexMode
	jobs    int
	verify  bool
	now     time.Time
	// serial은 파일 안을 구간으로 나누지 않는다는 뜻이다. 여러 파일을 worker로 나눠 찾을 때 worker 안에서 다시 worker를
	// 띄우지 않기 위해서다.
	serial bool
}

func runGrep(args []string) int {
	if lifecycle, ok := grepLifecycleArgs(args); ok {
		return grepSemanticLifecycle(lifecycle, os.Stdout, os.Stderr)
	}
	return grepRun(args, os.Stdin, os.Stdout, os.Stderr)
}

func grepLifecycleArgs(args []string) ([]string, bool) {
	for i, arg := range args {
		switch arg {
		case "--":
			return nil, false
		case "--semantic-init", "--semantic-status", "--semantic-files", "--semantic-clear", "--model-clear":
			ordered := append([]string{arg}, args[:i]...)
			return append(ordered, args[i+1:]...), true
		}
	}
	return nil, false
}

func parseGrepOptions(args []string, stderr io.Writer, now time.Time) (grepOptions, []string, error) {
	return parseGrepOptionsForInput(args, stderr, now, false)
}

func parseGrepOptionsForInput(args []string, stderr io.Writer, now time.Time, terminal bool) (grepOptions, []string, error) {
	set := newFlagSet("grep")
	set.SetOutput(stderr)
	options := grepOptions{minLevel: -1, now: now}
	set.BoolVar(&options.askKeyword, "keyword", false, T("command.grep.option.ask_keyword"))
	set.BoolVar(&options.semantic, "semantic", false, T("command.grep.option.semantic"))
	set.BoolVar(&options.askMode, "ask", false, T("command.grep.option.ask"))
	set.BoolVar(&options.askJSON, "json", false, T("command.grep.option.ask_json"))
	set.IntVar(&options.askLimit, "limit", 10, T("command.grep.option.ask_limit"))
	set.BoolVar(&options.codeMode, "code", false, T("command.grep.option.code"))
	set.BoolVar(&options.pathMode, "files", false, T("command.grep.option.paths"))
	set.BoolVar(&options.pathMode, "f", false, T("command.grep.option.paths"))
	set.BoolVar(&options.hidden, "hidden", false, T("command.grep.option.hidden"))
	set.BoolVar(&options.noIgnore, "no-ignore", false, T("command.grep.option.no_ignore"))
	set.BoolVar(&options.ignoreCase, "i", false, T("command.grep.option.ignore_case"))
	set.BoolVar(&options.regex, "E", false, T("command.grep.option.regex"))
	set.BoolVar(&options.regex, "regex", false, T("command.grep.option.regex"))
	set.BoolVar(&options.numbers, "n", false, T("command.grep.option.numbers"))
	set.BoolVar(&options.listFiles, "l", false, T("command.grep.option.files"))
	set.IntVar(&options.context, "C", 0, T("command.grep.option.context"))
	level := set.String("level", "", T("command.grep.option.level"))
	status := set.String("status", "", T("command.grep.option.status"))
	since := set.String("since", "", T("command.grep.option.since"))
	until := set.String("until", "", T("command.grep.option.until"))
	set.IntVar(&options.last, "last", 0, T("command.grep.option.last"))
	set.BoolVar(&options.ordered, "ordered", false, T("command.grep.option.ordered"))
	index := set.Bool("index", false, T("command.grep.option.index"))
	noIndex := set.Bool("no-index", false, T("command.grep.option.index"))
	set.IntVar(&options.jobs, "jobs", 0, T("command.grep.option.jobs"))
	set.BoolVar(&options.verify, "verify-index", false, T("command.grep.option.verify"))
	if err := parseReordered(set, args); err != nil {
		return options, nil, err
	}
	filtered := *level != "" || *status != "" || *since != "" || *until != ""
	if options.semantic {
		options.askMode = true
	}
	if options.askMode {
		if options.askKeyword && options.semantic {
			return options, nil, errors.New(T("cli.usage", grepUsage))
		}
		if !options.askKeyword {
			options.semantic = true
		}
	} else if options.askKeyword {
		return options, nil, errors.New(T("cli.usage", grepUsage))
	}
	logOptions := false
	set.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "level", "status", "since", "until", "last", "ordered", "index", "no-index", "jobs", "verify-index":
			logOptions = true
		}
	})
	if terminal && set.NArg() == 1 && !options.codeMode && !options.pathMode && !options.askMode && !logOptions {
		options.codeMode = true
	}
	if options.askMode {
		if options.codeMode || options.pathMode || logOptions || options.regex || options.ignoreCase || options.listFiles || options.numbers || options.context != 0 || set.NArg() == 0 || options.askLimit <= 0 || options.askLimit > 100 {
			return options, nil, errors.New(T("cli.usage", grepUsage))
		}
	} else if options.semantic || options.askJSON || options.askLimit != 10 {
		return options, nil, errors.New(T("cli.usage", grepUsage))
	}
	if options.codeMode || options.pathMode {
		if options.codeMode && options.pathMode || filtered || options.ordered || options.last != 0 || *index || options.verify || (options.pathMode && (options.listFiles || options.numbers || options.context != 0)) {
			return options, nil, errors.New(T("cli.usage", grepUsage))
		}
		options.index = grepIndexOff
		if options.codeMode {
			options.numbers = true
		}
	} else if !options.askMode && (options.hidden || options.noIgnore) {
		return options, nil, errors.New(T("cli.usage", grepUsage))
	}
	if (set.NArg() == 0 && !filtered) || options.context < 0 || options.last < 0 || options.jobs < 0 || (*index && *noIndex) {
		return options, nil, errors.New(T("cli.usage", grepUsage))
	}
	if options.pathMode || (options.codeMode && options.regex) {
		expression := set.Arg(0)
		if options.pathMode && !options.regex {
			expression = regexp.QuoteMeta(expression)
		}
		if options.pathMode && options.ignoreCase {
			expression = "(?i)" + expression
		}
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return options, nil, err
		}
		if options.pathMode {
			options.pathRegex = compiled
		}
	}

	paths := set.Args()
	if set.NArg() > 0 {
		options.pattern, paths = set.Arg(0), set.Args()[1:]
	}
	// 조건이 있으면 패턴 없이 쓰는 일이 많다. 첫 인자가 실제 파일이나 디렉터리면 패턴을 생략한 것으로 본다.
	if filtered && set.NArg() > 0 {
		if _, err := os.Stat(set.Arg(0)); err == nil {
			options.pattern, paths = "", set.Args()
		}
	}
	if *level != "" {
		if options.minLevel = grepLevelRank(viewNormalizeLevel(*level)); options.minLevel < 0 {
			return options, nil, errors.New(T("grep.bad_level", *level))
		}
	}
	if *status != "" {
		set, err := parseGrepStatuses(*status)
		if err != nil {
			return options, nil, errors.New(T("grep.bad_status", *status))
		}
		options.statuses = set
	}
	var err error
	if *since != "" {
		if options.since, err = grepParseWhen(*since, now); err != nil {
			return options, nil, errors.New(T("grep.bad_time", *since))
		}
	}
	if *until != "" {
		if options.until, err = grepParseWhen(*until, now); err != nil {
			return options, nil, errors.New(T("grep.bad_time", *until))
		}
	}
	switch {
	case *index:
		options.index = grepIndexForce
	case *noIndex:
		options.index = grepIndexOff
	}
	if options.jobs == 0 {
		options.jobs = grepSketchWorkers()
	}
	if options.jobs > grepMaxJobs {
		options.jobs = grepMaxJobs
	}
	return options, paths, nil
}

// grepRun은 grep의 본체다. stdout이 터미널이면 색을 쓰고, stderr가 터미널이면 hint를 낸다. 종료 코드는 grep과 같다.
// 찾으면 0, 없으면 1, 오류가 있으면 2다.
func grepRun(args []string, stdin, stdout, stderr *os.File) int {
	options, paths, err := parseGrepOptionsForInput(args, stderr, time.Now(), isTerminal(stdin))
	if err != nil {
		if !errors.Is(err, errFlagReported) && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	if (options.codeMode || options.pathMode || options.askMode) && len(paths) == 0 {
		paths = []string{"."}
	}
	if len(paths) == 0 && isTerminal(stdin) {
		fmt.Fprintln(stderr, T("cli.usage", grepUsage))
		return 2
	}
	if options.askMode {
		return grepAsk(paths, options, stdout, stderr)
	}
	var targets []string
	var directories int
	var problems []string
	if options.codeMode || options.pathMode {
		targets, directories, problems = grepCodeTargets(paths, options)
	} else {
		targets, directories, problems = grepTargets(paths)
	}
	failed := len(problems) != 0
	for _, problem := range problems {
		fmt.Fprintln(stderr, T("grep.failed", problem))
	}
	out := bufio.NewWriterSize(stdout, 64<<10)
	printer := &grepPrinter{out: out, color: richEnabled(stdout), showPath: options.codeMode || len(targets) > 1 || directories > 0, numbers: options.numbers, context: options.context}
	hints := &grepHints{options: options}
	found := false
	var pathRoots []grepPathRoot
	if options.pathMode && printer.color {
		pathRoots = grepPathRoots(paths)
	}
	matcher, err := newGrepMatcher(options.pattern, options.regex, options.ignoreCase, nil)
	if err != nil {
		fmt.Fprintln(stderr, T("grep.failed", err))
		return 2
	}
	// 코드 검색은 작은 파일 수만 개를 읽는다. 파일마다 2 MiB 읽기 버퍼를 새로 만들면 할당과 GC가 검색보다 오래 걸린다.
	buffers := &grepBuffers{}
	if len(paths) == 0 {
		ok, err := grepSequentialSearch(stdin, T("grep.stdin"), options, matcher, printer, hints)
		found, failed = found || ok, failed || err != nil
		if err != nil {
			fmt.Fprintln(stderr, T("grep.failed", err))
		}
	}
	if !options.pathMode && len(targets) > 1 {
		ok, searchFailed := grepSearchFiles(targets, options, matcher, printer, buffers, hints, stderr)
		found, failed = found || ok, failed || searchFailed
		targets = nil
	}
	for _, path := range targets {
		if options.pathMode {
			fmt.Fprintln(out, grepHighlightPath(path, pathRoots, options, printer.color))
			found = true
			continue
		}
		ok, err := grepSearchFile(path, options, matcher, buffers, printer, hints)
		found = found || ok
		if err != nil {
			failed = true
			fmt.Fprintln(stderr, T("grep.failed", fmt.Sprintf("%s: %v", path, err)))
		}
	}
	out.Flush()
	if hints.verifyFailed {
		failed = true
	}
	if isTerminal(stderr) && !options.codeMode && !options.pathMode {
		hints.render(stderr, richEnabled(stderr))
	}
	switch {
	case failed:
		return 2
	case found:
		return 0
	}
	return 1
}

// grepTargets는 경로를 파일 목록으로 푼다. 디렉터리는 안의 일반 파일을 이름 순서로 모두 넣고, 디렉터리로 가는 symlink는 따라가지
// 않는다. 돌려주는 값은 파일, 디렉터리 수, 문제가 있던 경로다.
func grepTargets(paths []string) ([]string, int, []string) {
	var files, problems []string
	directories := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		directories++
		filepath.WalkDir(path, func(entry string, dirent fs.DirEntry, err error) error {
			if err != nil {
				problems = append(problems, err.Error())
				return nil
			}
			if dirent.Type().IsRegular() {
				files = append(files, entry)
			}
			return nil
		})
	}
	return files, directories, problems
}

// grepDetectFormat은 파일 앞부분으로 로그 형식을 정한다. 마지막 조각은 잘린 줄일 수 있어 빼고 본다.
func grepDetectFormat(sample []byte, whole bool) viewFormat {
	if !whole {
		if last := bytes.LastIndexByte(sample, '\n'); last >= 0 {
			sample = sample[:last]
		}
	}
	return detectViewFormat(strings.Split(string(sample), "\n"))
}

// grepFilterFor는 패턴 밖의 조건이다. 조건이 하나도 없으면 nil이다.
func grepFilterFor(format viewFormat, options grepOptions) *grepFilter {
	filter := newGrepFilter(format, nil, options.now)
	filter.minLevel = options.minLevel
	if options.statuses != nil {
		filter.setStatuses(options.statuses)
	}
	filter.since, filter.until = options.since, options.until
	if !filter.active() {
		return nil
	}
	return filter
}

// grepFileReport는 파일 하나의 검색이 어떻게 진행됐는지다. hint는 이 값으로 만든다.
type grepFileReport struct {
	path       string
	size       int64
	index      grepIndexOutcome
	reason     string
	blocks     int
	blocksRead int
	bytesRead  int64
	indexBytes int64
	extended   int
	built      time.Duration
	noLetters  bool
	sequential bool
	skipped    int64
	unordered  bool
	seekFailed bool
	noTime     bool
	lastRead   int64
	counted    int64
}

type grepIndexOutcome int

const (
	grepIndexOutcomeNone grepIndexOutcome = iota
	grepIndexOutcomeOff
	grepIndexOutcomeSmall
	grepIndexOutcomeFirst
	grepIndexOutcomeBuilt
	grepIndexOutcomeUsed
	grepIndexOutcomeInvalid
	grepIndexOutcomeStale
	grepIndexOutcomeLast
	grepIndexOutcomeSequential
	grepIndexOutcomeUnavailable
	grepIndexOutcomeNotBuilt
)

// grepSearchFile은 파일 하나를 검색한다. .gz는 풀면서 차례로 읽고, 그 밖의 파일은 index와 건너뛰기를 쓴다.
func grepSearchFile(path string, options grepOptions, compiled *grepMatcher, buffers *grepBuffers, printer *grepPrinter, hints *grepHints) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return false, nil
	}
	if strings.HasSuffix(path, ".gz") && !options.codeMode {
		// 회전된 .gz는 내용이 다 쓰인 뒤에 만들어지므로 수정 시각이 마지막 줄보다 뒤다. --ordered면 풀지 않고 수정 시각으로 건너뛴다.
		if options.ordered && !options.since.IsZero() && info.ModTime().Add(grepTolerance).Before(options.since) {
			hints.skippedOld = append(hints.skippedOld, path)
			return false, nil
		}
		reader, err := gzip.NewReader(bufio.NewReaderSize(file, 1<<20))
		if err != nil {
			return false, err
		}
		defer reader.Close()
		return grepSequentialSearch(reader, path, options, compiled, printer, hints)
	}
	size := info.Size()
	sample := buffers.sampleOf(int(min(size, int64(grepSampleSize))))
	if _, err := io.ReadFull(io.NewSectionReader(file, 0, int64(len(sample))), sample); err != nil {
		return false, err
	}
	if bytes.IndexByte(sample[:min(len(sample), grepBinaryProbe)], 0) >= 0 {
		hints.binary = append(hints.binary, path)
		return false, nil
	}
	// 코드 검색은 로그 조건과 index를 쓰지 않아 형식이 필요 없다. 소스 파일은 대부분 작아서, pivot을 고르려고 표본의 바이트
	// 빈도를 세는 비용이 검색 비용과 비슷하다. 그래서 둘 다 건너뛰고 컴파일한 패턴을 그대로 쓴다.
	var format viewFormat
	matcher := compiled
	if !options.codeMode {
		format = grepDetectFormat(sample, int64(len(sample)) == size)
		matcher = compiled.withSample(sample)
	}
	filter := grepFilterFor(format, options)
	// 시간 순서라고 한 파일만, 안의 마지막 시각이 --since 앞일 때 건너뛴다. 표본으로 순서를 확인하므로 사용자가 보증해야 한다.
	if options.ordered && filter != nil && !options.since.IsZero() && filter.endsBefore(file, size, options.since.Add(-grepTolerance)) {
		hints.skippedOld = append(hints.skippedOld, path)
		return false, nil
	}
	scanner := newGrepScanner(matcher, filter)
	scanner.buffers = buffers
	// 표본이 파일 전체이면 다시 읽지 않는다. 작은 소스 파일 수만 개를 찾을 때 두 번째 pread가 시간의 상당 부분이었다.
	var source io.ReaderAt = file
	if int64(len(sample)) == size {
		source = bytes.NewReader(sample)
	}
	printer.begin(path, matcher)
	report := &grepFileReport{path: path, size: size}
	defer func() { hints.reports = append(hints.reports, *report) }()
	found := false
	emit := func(ref grepLineRef) bool {
		found = true
		if options.listFiles {
			return false
		}
		printer.match(ref, source)
		return true
	}
	defer func() {
		if found && options.listFiles {
			printer.file(path)
		}
	}()
	if options.last > 0 {
		report.index = grepIndexOutcomeLast
		lines, read, err := scanner.scanLast(source, size, options.last)
		report.lastRead = read
		// 끝에서 읽은 줄은 번호를 모른다. -n이면 처음부터 첫 일치까지, 그 뒤로는 일치 사이의 줄바꿈을 세어 번호를 매긴다.
		// 앞 블록에서 읽기가 실패했으면 처음부터 셀 수 없으므로 번호 없이 낸다.
		numbers := make([]int64, len(lines))
		if options.numbers && err == nil {
			var number, from int64
			for index, line := range lines {
				counted, err := grepCountNewlines(file, from, line.offset)
				if err != nil {
					return false, err
				}
				number += counted
				numbers[index], from = number+1, line.offset
			}
			report.counted = number
		}
		for index, line := range lines {
			if !emit(grepLineRef{offset: line.offset, number: numbers[index], text: line.text}) {
				break
			}
		}
		return found, err
	}
	plan := grepPlanIndex(options, info, size, format, report)
	if plan.open != nil {
		if err := grepIndexMatches(plan.open, file, size); err != nil {
			plan.open.close()
			os.Remove(plan.path)
			plan.open, plan.build = nil, true
			report.index, report.reason = grepIndexOutcomeInvalid, grepIndexReason(err)
		}
	}
	if plan.open != nil {
		defer plan.open.close()
		return grepSearchWithIndex(file, size, format, matcher, filter, scanner, options, plan, report, emit)
	}
	if plan.build {
		return grepSearchAndBuild(file, size, format, scanner, filter, options, plan, report, emit)
	}
	fresh := func() grepScanner { return newGrepScanner(matcher, grepFilterFor(format, options)) }
	return grepSearchPlain(source, size, scanner, filter, fresh, options, report, emit)
}

// grepSearchFiles는 대상 파일을 CPU 수만큼의 worker로 나눠 찾는다. 작은 파일 수만 개를 한 스레드에서 열고 읽으면 시스템
// 호출을 기다리는 시간이 검색보다 길고, 20 MiB 로그 52개는 rg보다 8배 느렸다. worker는 파일마다 출력과 힌트를 모아 두고,
// 여기서 대상 순서대로 내므로 결과는 차례로 찾을 때와 같다. worker 안에서는 파일을 구간으로 나누지 않는다.
// grepIndexMinSize 이상인 파일은 index를 만들거나 쓰고 파일 안에서도 병렬로 훑으므로, worker가 맡지 않고 순서가 되면
// 여기서 찾는다. 이 기준을 grepParallelMinSize로 낮추면 20 MiB 로그 52개가 121ms에서 277ms로 느려졌다.
func grepSearchFiles(targets []string, options grepOptions, matcher *grepMatcher, printer *grepPrinter, buffers *grepBuffers, hints *grepHints, stderr io.Writer) (bool, bool) {
	type fileResult struct {
		output []byte
		found  bool
		err    error
		// inline이면 worker가 찾지 않은 큰 파일이다.
		inline bool
		hints  grepHints
	}
	workers := min(runtime.GOMAXPROCS(0), len(targets))
	// 출력을 기다리는 파일 수를 window로 묶는다. 출력이 느리면(| less) worker가 앞서 나가 결과를 모두 메모리에 쌓기 때문이다.
	// worker는 자리를 얻은 뒤에 다음 순번을 받고, 여기서 낸 순번만큼 자리를 돌려준다. 그래서 받았지만 아직 내지 않은 순번은
	// window개 안에 들고, 결과 자리는 순번을 window로 나눈 나머지로 겹치지 않게 다시 쓴다.
	window := min(len(targets), workers*grepFilesWindowPerWorker)
	slots := make(chan struct{}, window)
	results := make([]chan fileResult, window)
	for index := range results {
		results[index] = make(chan fileResult, 1)
	}
	serial := options
	serial.serial = true
	var next atomic.Int64
	for range workers {
		go func() {
			buffers := &grepBuffers{}
			var output bytes.Buffer
			local := *printer
			local.out = bufio.NewWriterSize(&output, 64<<10)
			for {
				slots <- struct{}{}
				index := int(next.Add(1)) - 1
				if index >= len(targets) {
					<-slots
					return
				}
				if info, err := os.Stat(targets[index]); err == nil && info.Size() >= grepIndexMinSize {
					results[index%window] <- fileResult{inline: true}
					continue
				}
				hints := grepHints{options: options}
				ok, err := grepSearchFile(targets[index], serial, matcher, buffers, &local, &hints)
				local.out.Flush()
				var text []byte
				if output.Len() > 0 {
					text = bytes.Clone(output.Bytes())
				}
				results[index%window] <- fileResult{output: text, found: ok, err: err, hints: hints}
				output.Reset()
			}
		}()
	}
	found, failed := false, false
	for index, path := range targets {
		result := <-results[index%window]
		if result.inline {
			result.found, result.err = grepSearchFile(path, options, matcher, buffers, printer, hints)
		} else {
			printer.out.Write(result.output)
			hints.reports = append(hints.reports, result.hints.reports...)
			hints.skippedOld = append(hints.skippedOld, result.hints.skippedOld...)
			hints.binary = append(hints.binary, result.hints.binary...)
			hints.verifyFailed = hints.verifyFailed || result.hints.verifyFailed
		}
		<-slots
		found = found || result.found
		if result.err != nil {
			failed = true
			fmt.Fprintln(stderr, T("grep.failed", fmt.Sprintf("%s: %v", path, result.err)))
		}
	}
	return found, failed
}

// grepFilesWindowPerWorker는 worker 하나가 출력보다 앞서 찾아 둘 수 있는 파일 수다.
const grepFilesWindowPerWorker = 16

type grepIndexPlan struct {
	path  string
	open  *grepIndexFile
	build bool
}

// grepPlanIndex는 이 파일에 index를 쓸지, 만들지, 그냥 훑을지 정한다. 첫 검색은 index 없이 끝내고 표시만 남기며, 같은 파일을
// grepIndexSeenWindow 안에 다시 검색하면 그때 만든다. 반복해서 찾는 파일에만 비용을 들이기 위해서다.
func grepPlanIndex(options grepOptions, info os.FileInfo, size int64, format viewFormat, report *grepFileReport) grepIndexPlan {
	var plan grepIndexPlan
	if options.index == grepIndexOff {
		report.index = grepIndexOutcomeOff
		return plan
	}
	if size < grepIndexMinSize && options.index != grepIndexForce {
		report.index = grepIndexOutcomeSmall
		return plan
	}
	dir, err := grepIndexDir()
	key, ok := grepFileKey(info)
	if err != nil || !ok {
		report.index = grepIndexOutcomeUnavailable
		return plan
	}
	plan.path = filepath.Join(dir, key+".idx")
	index, err := grepOpenIndex(plan.path, grepBlockSize, format)
	switch {
	case err == nil:
		plan.open = index
		return plan
	case !errors.Is(err, fs.ErrNotExist):
		os.Remove(plan.path)
		report.index, report.reason = grepIndexOutcomeInvalid, grepIndexReason(err)
		plan.build = true
		return plan
	}
	marker := filepath.Join(dir, key+".seen")
	if options.index == grepIndexForce {
		plan.build = true
		return plan
	}
	if seen, err := os.Stat(marker); err == nil && options.now.Sub(seen.ModTime()) < grepIndexSeenWindow {
		plan.build = true
		return plan
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if touched, err := os.Create(marker); err == nil {
			touched.Close()
		}
	}
	report.index = grepIndexOutcomeFirst
	return plan
}

func grepIndexReason(err error) string {
	_, reason, found := strings.Cut(err.Error(), ": ")
	if !found {
		return err.Error()
	}
	return reason
}

// grepSearchWithIndex는 index로 블록을 건너뛰며 검색하고, 뒤에 쌓인 부분의 sketch로 index를 늘린다. 읽은 블록이 index와
// 다르면 index를 버리고 파일 전체를 다시 훑되, 이미 낸 줄은 다시 내지 않는다.
func grepSearchWithIndex(file *os.File, size int64, format viewFormat, matcher *grepMatcher, filter *grepFilter, scanner grepScanner, options grepOptions, plan grepIndexPlan, report *grepFileReport, emit func(grepLineRef) bool) (bool, error) {
	query := newGrepIndexQuery(matcher, options.ignoreCase, filter)
	report.noLetters = len(query.bits) == 0 && options.pattern != ""
	emitted := map[int64]bool{}
	var offsets []int64
	found := false
	search := &grepIndexedSearch{index: plan.open, file: file, size: size, scanner: scanner, filter: filter, query: query, numbers: options.numbers}
	record := func(ref grepLineRef) bool {
		found = true
		// 순서대로 읽는 모드는 블록을 건너뛰지 않아 index가 틀려도 다시 읽을 일이 없다. 그때는 낸 줄을 기억하지 않는다.
		if !search.sequential {
			emitted[ref.offset] = true
		}
		if options.verify {
			offsets = append(offsets, ref.offset)
		}
		return emit(ref)
	}
	pool := newGrepSketchPool(format, options.now, options.jobs, int(plan.open.header.Blocks))
	fresh, linesAt, err := search.run(pool, record)
	report.blocks, report.blocksRead, report.bytesRead, report.indexBytes, report.sequential = int(plan.open.header.Blocks), search.blocksRead, search.bytesRead, plan.open.size, search.sequential
	if errors.Is(err, errGrepIndexStale) {
		plan.open.close()
		os.Remove(plan.path)
		report.index = grepIndexOutcomeStale
		lineBase := int64(-1)
		if options.numbers {
			lineBase = 0
		}
		again := scanner
		again.verify = nil
		_, err := again.scanRange(file, 0, size, lineBase, func(ref grepLineRef) bool {
			if emitted[ref.offset] {
				return true
			}
			return record(ref)
		})
		return found, err
	}
	if err != nil {
		return found, err
	}
	report.index = grepIndexOutcomeUsed
	if len(fresh) != 0 {
		header := plan.open.header
		header.FileSize, header.Indexed, header.Lines, header.LastCRC = size, header.Indexed+int64(len(fresh))*int64(header.Block), linesAt, fresh[len(fresh)-1].CRC
		if err := grepWriteIndex(plan.path, header, plan.open.records(), int(plan.open.header.Blocks), fresh); err == nil {
			report.extended = len(fresh)
		}
	}
	now := time.Now()
	os.Chtimes(plan.path, now, now)
	if options.verify {
		if err := grepVerifyIndex(file, size, scanner, filter, offsets, plan.path, options.listFiles); err != nil {
			return found, err
		}
	}
	return found, nil
}

// grepVerifyIndex는 index를 거친 결과와 파일 전체를 훑은 결과를 비교한다. 다르면 index를 지우고 오류로 알린다.
// -l은 첫 일치에서 멈추므로 그때는 일치가 있는지만 비교한다.
func grepVerifyIndex(file *os.File, size int64, scanner grepScanner, filter *grepFilter, indexed []int64, indexPath string, listFiles bool) error {
	if filter != nil {
		if err := filter.prime(file, 0); err != nil {
			return err
		}
	}
	var full []int64
	scanner.verify = nil
	if _, err := scanner.scanRange(file, 0, size, -1, func(ref grepLineRef) bool {
		full = append(full, ref.offset)
		return true
	}); err != nil {
		return err
	}
	if fmt.Sprint(full) == fmt.Sprint(indexed) || (listFiles && (len(full) != 0) == (len(indexed) != 0)) {
		return nil
	}
	os.Remove(indexPath)
	return fmt.Errorf("%s", T("grep.verify_failed", len(indexed), len(full)))
}

// grepSearchAndBuild는 파일 전체를 훑으면서 그 바이트로 index를 만든다. 검색이 어차피 읽는 I/O에 sketch 계산만 더한다.
func grepSearchAndBuild(file *os.File, size int64, format viewFormat, scanner grepScanner, filter *grepFilter, options grepOptions, plan grepIndexPlan, report *grepFileReport, emit func(grepLineRef) bool) (bool, error) {
	started := time.Now()
	found := false
	pool := newGrepSketchPool(format, options.now, options.jobs, 0)
	read, indexed, linesAt, err := grepScanBlocks(file, size, scanner, filter, 0, 0, options.numbers, pool, func(ref grepLineRef) bool {
		found = true
		return emit(ref)
	})
	sketches := pool.finish()
	report.bytesRead = read
	if err != nil {
		return found, err
	}
	marker := strings.TrimSuffix(plan.path, ".idx") + ".seen"
	if indexed == 0 || len(sketches) != indexed {
		// 가득 찬 블록이 하나도 없는 파일은 index에 넣을 것이 없다.
		report.index, report.reason = grepIndexOutcomeNotBuilt, T("grep.reason.short")
		os.Remove(marker)
		return found, nil
	}
	header := grepIndexHeaderFor(format, scanner.block, size, &sketches[0], &sketches[len(sketches)-1], len(sketches), linesAt)
	if err := grepWriteIndex(plan.path, header, nil, 0, sketches); err != nil {
		// 표시를 지워 다음 검색이 또 9초를 들여 만들고 버리지 않게 한다.
		report.index, report.reason = grepIndexOutcomeNotBuilt, err.Error()
		os.Remove(marker)
		return found, nil
	}
	os.Remove(marker)
	if written, err := os.Stat(plan.path); err == nil {
		report.indexBytes = written.Size()
	}
	if report.index != grepIndexOutcomeInvalid {
		report.index = grepIndexOutcomeBuilt
	}
	report.blocks, report.built = indexed, time.Since(started)
	grepIndexTrim(filepath.Dir(plan.path), grepIndexCacheCap)
	return found, nil
}

// grepSearchPlain은 파일을 훑는다. --since가 있고 로그가 시간 순서면 그 자리를 이진 탐색해 앞부분을 건너뛴다.
// grepSearchPlain은 index 없이 [from, size)를 훑는다. fresh는 병렬로 훑을 때 구간마다 쓸 새 scanner를 만든다.
func grepSearchPlain(file io.ReaderAt, size int64, scanner grepScanner, filter *grepFilter, fresh func() grepScanner, options grepOptions, report *grepFileReport, emit func(grepLineRef) bool) (bool, error) {
	found := false
	wrapped := func(ref grepLineRef) bool {
		found = true
		return emit(ref)
	}
	from := int64(0)
	switch {
	case filter == nil || options.since.IsZero():
	case !options.ordered:
		// 표본 몇 곳으로는 표본 사이의 역행을 알 수 없다. 사용자가 순서를 보증하지 않으면 전부 읽고 거른다.
		report.unordered = true
	default:
		offset, _, ok := filter.seek(file, size, options.since.Add(-grepTolerance))
		switch {
		case ok:
			from = offset
			report.skipped = offset
		case filter.format == viewFormatPlain:
			report.noTime = true
		default:
			report.seekFailed = true
		}
	}
	lineBase := int64(-1)
	if options.numbers {
		lineBase = 0
		if from > 0 {
			counted, err := grepCountNewlines(file, 0, from)
			if err != nil {
				return false, err
			}
			lineBase, report.counted = counted, counted
		}
	}
	if filter != nil {
		if err := filter.prime(file, from); err != nil {
			return false, err
		}
	}
	// 앞 줄의 level과 시각을 물려받는 필터는 구간 경계에서 상태를 probe로만 이을 수 있어 결과가 달라질 수 있다. 그런
	// 필터는 처음부터 차례로 훑는다.
	if workers := runtime.GOMAXPROCS(0); !options.serial && workers > 1 && size-from >= grepParallelMinSize && (filter == nil || !filter.needsEveryLine()) {
		read, err := grepScanParallel(file, from, size, lineBase, workers, grepParallelSpan, fresh, wrapped)
		report.bytesRead = read
		return found, err
	}
	read, err := scanner.scanRange(file, from, size, lineBase, wrapped)
	report.bytesRead = read
	return found, err
}

const (
	// grepParallelMinSize보다 큰 범위는 구간으로 나눠 병렬로 훑는다. 한 스레드는 1 GiB 로그의 흔한 낱말을 0.9초에
	// 훑었고, 10개로 나누면 0.12초였다. 48 MiB 파일은 이 값이 64 MiB일 때 48ms, 16 MiB일 때 28ms였고, 8 MiB로 더
	// 낮춰도 12 MiB 파일이 26ms에서 22ms가 되는 정도였다.
	grepParallelMinSize = 16 << 20
	// grepParallelSpan은 worker 하나가 한 번에 맡는 구간이다. 일치한 줄은 내보낼 때까지 복사해 두므로, 모든 줄이
	// 맞아도 메모리는 구간 크기 x window 안에 든다.
	grepParallelSpan = 2 << 20
)

// grepScanParallel은 [from, to)를 span 단위 구간으로 나눠 workers개가 훑고, 맞는 줄을 구간 순서대로 emit에 넘긴다.
// 줄은 시작한 구간에만 속하므로 결과는 차례로 훑을 때와 같다. lineBase가 0 이상이면 구간마다 상대 번호로 센 뒤 앞
// 구간들의 줄바꿈 수를 더한다. emit이 false를 돌려주면 남은 구간을 훑지 않는다.
func grepScanParallel(file io.ReaderAt, from, to, lineBase int64, workers int, span int64, fresh func() grepScanner, emit func(grepLineRef) bool) (int64, error) {
	// 맞는 줄은 text 하나에 이어 붙이고 ends에 끝 위치를 둔다. 내보낸 구간의 배열은 free로 돌려 다음 구간이 다시 쓴다.
	// 모든 줄이 맞을 때 줄마다, 또는 구간마다 새로 할당하면 GC가 회수하기 전까지 쌓여 메모리가 window의 몇 배가 되었다.
	type spanResult struct {
		lines    []grepLineRef
		text     []byte
		ends     []int
		newlines int64
		read     int64
		err      error
	}
	count := int((to - from + span - 1) / span)
	// 내보내기를 기다리는 구간 수를 window로 묶어, 출력이 느려도 복사해 둔 줄이 쌓이지 않게 한다.
	window := min(count, workers+2)
	slots := make(chan struct{}, window)
	results := make([]chan *spanResult, window)
	for index := range results {
		results[index] = make(chan *spanResult, 1)
	}
	free := make(chan *spanResult, window)
	numbers := lineBase >= 0
	var next atomic.Int64
	var stopped atomic.Bool
	done := make(chan struct{})
	var group sync.WaitGroup
	for range min(workers, count) {
		group.Go(func() {
			scanner := fresh()
			var counting []byte
			if numbers {
				counting = make([]byte, grepBlockSize)
			}
			for {
				select {
				case slots <- struct{}{}:
				case <-done:
					return
				}
				index := int(next.Add(1)) - 1
				if index >= count {
					return
				}
				start := from + int64(index)*span
				end := min(start+span, to)
				relative := int64(-1)
				if numbers {
					relative = 0
				}
				result := &spanResult{}
				select {
				case result = <-free:
				default:
				}
				result.read, result.err = scanner.scanRange(file, start, end, relative, func(ref grepLineRef) bool {
					result.text = append(result.text, ref.text...)
					result.ends = append(result.ends, len(result.text))
					ref.text = nil
					result.lines = append(result.lines, ref)
					return !stopped.Load()
				})
				if numbers && result.err == nil {
					result.newlines, result.err = grepCountNewlinesWith(file, start, end, counting)
				}
				results[index%window] <- result
			}
		})
	}
	defer func() {
		stopped.Store(true)
		close(done)
		group.Wait()
	}()
	var read int64
	base := lineBase
	for index := range count {
		result := <-results[index%window]
		read += result.read
		start := 0
		for index, ref := range result.lines {
			ref.text, start = result.text[start:result.ends[index]], result.ends[index]
			if numbers {
				ref.number += base
			}
			if !emit(ref) {
				return read, nil
			}
		}
		if result.err != nil {
			return read, result.err
		}
		base += result.newlines
		result.lines, result.text, result.ends = result.lines[:0], result.text[:0], result.ends[:0]
		result.newlines, result.read = 0, 0
		select {
		case free <- result:
		default:
		}
		<-slots
	}
	return read, nil
}

// grepSequentialSearch는 stdin과 .gz처럼 되감을 수 없는 입력이다. 줄을 차례로 읽으며 조건을 대고, -C는 앞 줄을 ring에 둔다.
// --last는 마지막 N개만 남겨 끝에 낸다.
func grepSequentialSearch(reader io.Reader, name string, options grepOptions, compiled *grepMatcher, printer *grepPrinter, hints *grepHints) (bool, error) {
	buffered := bufio.NewReaderSize(reader, 1<<20)
	sample, _ := buffered.Peek(grepSampleSize)
	if bytes.IndexByte(sample[:min(len(sample), grepBinaryProbe)], 0) >= 0 {
		hints.binary = append(hints.binary, name)
		return false, nil
	}
	format := grepDetectFormat(sample, len(sample) < grepSampleSize)
	matcher := compiled.withSample(sample)
	filter := grepFilterFor(format, options)
	accept := matcher.match
	if filter != nil {
		filter.matcher = matcher
		accept = func(line []byte) bool { return filter.prefilter(line) && filter.accept(line) }
	}
	printer.begin(name, matcher)
	hints.reports = append(hints.reports, grepFileReport{path: name, index: grepIndexOutcomeSequential})
	var before [][]byte
	var last []grepLine
	after := 0
	found := false
	var offset, number int64
	// 마지막 줄과 함께 온 읽기 오류(잘린 gzip 등)는 줄을 처리한 뒤에 돌려준다. 성공으로 끝나면 잘린 입력을 온전한 결과로 믿게 된다.
	var readErr error
	for {
		line, consumed, err := grepReadLine(buffered, grepLineLimit)
		if consumed == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return found, err
		}
		number++
		ref := grepLineRef{offset: offset, number: number, text: line}
		matched := accept(line)
		switch {
		case options.last > 0:
			if matched {
				last = append(last, grepLine{offset, bytes.Clone(line)})
				if len(last) > options.last {
					last = last[1:]
				}
				found = true
			}
		case matched:
			found = true
			if options.listFiles {
				printer.file(name)
				return true, nil
			}
			printer.matchWithContext(before, ref)
			before, after = before[:0], options.context
		case after > 0:
			printer.after(ref)
			after--
		case options.context > 0:
			before = append(before, bytes.Clone(line))
			if len(before) > options.context {
				before = before[1:]
			}
		}
		offset += int64(consumed)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	for _, line := range last {
		printer.match(grepLineRef{offset: line.offset, text: line.text}, nil)
	}
	if found && options.listFiles {
		printer.file(name)
	}
	return found, readErr
}

// grepReadLine은 줄 하나를 줄바꿈 없이 돌려준다. limit보다 긴 줄은 앞부분만 남기고 나머지는 버린다. consumed는 줄바꿈까지
// 읽은 바이트다.
func grepReadLine(reader *bufio.Reader, limit int) ([]byte, int, error) {
	var line []byte
	consumed := 0
	for {
		chunk, err := reader.ReadSlice('\n')
		consumed += len(chunk)
		if len(line)+len(chunk) <= limit {
			line = append(line, chunk...)
		} else if len(line) < limit {
			line = append(line, chunk[:limit-len(line)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) != 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		// CR은 줄의 일부로 둔다. pipe 출력은 파일을 읽는 경로와 같이 원문 바이트를 지켜야 한다.
		return line, consumed, err
	}
}

// grepPrinter는 grep과 같은 모양으로 줄을 낸다. 파일이 여럿이면 경로를, -n이면 줄 번호를 앞에 붙이고, -C의 앞뒤 줄은 ':' 대신
// '-'로 잇는다. 색은 터미널에서만 쓰고, 그때는 제어 문자도 지운다.
type grepPrinter struct {
	out      *bufio.Writer
	color    bool
	showPath bool
	numbers  bool
	context  int
	path     string
	matcher  *grepMatcher
	// lastEnd는 마지막으로 낸 줄의 끝 offset이다. 그 뒤에 바로 이어지지 않는 줄을 -C로 내기 전에 "--"를 둔다.
	lastEnd int64
	printed bool
}

func (printer *grepPrinter) begin(path string, matcher *grepMatcher) {
	printer.path, printer.matcher, printer.lastEnd, printer.printed = path, matcher, -1, false
}

func (printer *grepPrinter) file(path string) {
	if printer.color {
		printer.out.WriteString(richPaint(path, richMagenta, false))
	} else {
		printer.out.WriteString(path)
	}
	printer.out.WriteByte('\n')
}

// match는 맞는 줄 하나를 낸다. file이 있으면 -C의 앞뒤 줄을 그 자리에서 읽는다.
func (printer *grepPrinter) match(ref grepLineRef, file io.ReaderAt) {
	if printer.context == 0 || file == nil {
		printer.gap(ref.offset)
		printer.write(ref.offset, ref.number, ref.text, ':', true)
		return
	}
	before, after := grepContextLines(file, ref.offset, len(ref.text), printer.context)
	printer.matchWithContext(before, ref)
	offset := ref.offset + int64(len(ref.text)) + 1
	for index, line := range after {
		number := int64(0)
		if ref.number > 0 {
			number = ref.number + int64(index) + 1
		}
		printer.write(offset, number, line, '-', false)
		offset += int64(len(line)) + 1
	}
}

// matchWithContext는 앞 줄 before와 맞는 줄을 낸다. 이미 낸 줄과 겹치는 앞 줄은 다시 내지 않는다.
func (printer *grepPrinter) matchWithContext(before [][]byte, ref grepLineRef) {
	offset := ref.offset
	for _, line := range before {
		offset -= int64(len(line)) + 1
	}
	printer.gap(offset)
	for index, line := range before {
		if offset >= printer.lastEnd {
			number := int64(0)
			if ref.number > 0 {
				number = ref.number - int64(len(before)-index)
			}
			printer.write(offset, number, line, '-', false)
		}
		offset += int64(len(line)) + 1
	}
	printer.write(ref.offset, ref.number, ref.text, ':', true)
}

func (printer *grepPrinter) after(ref grepLineRef) {
	if ref.offset >= printer.lastEnd {
		printer.write(ref.offset, ref.number, ref.text, '-', false)
	}
}

// gap은 -C에서 앞 묶음과 이어지지 않을 때 "--"를 낸다.
func (printer *grepPrinter) gap(offset int64) {
	if printer.context > 0 && printer.printed && offset > printer.lastEnd {
		if printer.color {
			printer.out.WriteString(richDim("--"))
		} else {
			printer.out.WriteString("--")
		}
		printer.out.WriteByte('\n')
	}
}

func (printer *grepPrinter) write(offset, number int64, text []byte, separator byte, matched bool) {
	if offset < printer.lastEnd {
		return
	}
	paint := func(value, color string) string {
		if printer.color {
			return richPaint(value, color, false)
		}
		return value
	}
	sep := string(separator)
	if printer.color {
		sep = richDim(sep)
	}
	if printer.showPath {
		printer.out.WriteString(paint(printer.path, richMagenta) + sep)
	}
	if printer.numbers && number > 0 {
		printer.out.WriteString(paint(strconv.FormatInt(number, 10), richGreen) + sep)
	}
	if printer.color {
		printer.out.WriteString(printer.highlight(text, matched))
	} else {
		printer.out.Write(text)
	}
	printer.out.WriteByte('\n')
	printer.lastEnd, printer.printed = offset+int64(len(text))+1, true
}

// highlight는 제어 문자를 지운 줄에서 패턴이 맞은 자리를 굵은 빨강으로 칠한다.
func (printer *grepPrinter) highlight(text []byte, matched bool) string {
	if !matched || printer.matcher == nil || (printer.matcher.regex == nil && len(printer.matcher.literal) == 0) {
		return viewSanitize(string(text))
	}
	var ranges [][]int
	if printer.matcher.regex != nil {
		ranges = printer.matcher.regex.FindAllIndex(text, -1)
	} else {
		position := 0
		for {
			index := bytes.Index(text[position:], printer.matcher.literal)
			if index < 0 {
				break
			}
			ranges = append(ranges, []int{position + index, position + index + len(printer.matcher.literal)})
			position += index + len(printer.matcher.literal)
		}
	}
	var builder strings.Builder
	position := 0
	for _, span := range ranges {
		if span[1] <= span[0] {
			continue
		}
		builder.WriteString(viewSanitize(string(text[position:span[0]])))
		builder.WriteString(richPaint(viewSanitize(string(text[span[0]:span[1]])), richRed, true))
		position = span[1]
	}
	builder.WriteString(viewSanitize(string(text[position:])))
	return builder.String()
}

// grepContextLines는 offset에서 시작하는 줄의 앞 count줄과 뒤 count줄이다. 앞뒤 grepContextWindow 안에서만 찾는다.
func grepContextLines(file io.ReaderAt, offset int64, length, count int) ([][]byte, [][]byte) {
	var before, after [][]byte
	if offset > 0 {
		start := offset - grepContextWindow
		if start < 0 {
			start = 0
		}
		buffer := make([]byte, offset-start)
		n, _ := file.ReadAt(buffer, start)
		buffer = buffer[:n]
		if start > 0 {
			if newline := bytes.IndexByte(buffer, '\n'); newline >= 0 {
				buffer = buffer[newline+1:]
			} else {
				buffer = nil
			}
		}
		lines := bytes.Split(bytes.TrimSuffix(buffer, []byte{'\n'}), []byte{'\n'})
		if len(buffer) == 0 {
			lines = nil
		}
		if len(lines) > count {
			lines = lines[len(lines)-count:]
		}
		before = lines
	}
	next := offset + int64(length) + 1
	buffer := make([]byte, grepContextWindow)
	n, _ := file.ReadAt(buffer, next)
	buffer = buffer[:n]
	for len(after) < count && len(buffer) != 0 {
		end := bytes.IndexByte(buffer, '\n')
		if end < 0 {
			if n < grepContextWindow {
				after = append(after, buffer)
			}
			break
		}
		after = append(after, buffer[:end])
		buffer = buffer[end+1:]
	}
	return before, after
}

// grepHints는 stderr가 터미널일 때 검색이 어떻게 진행됐는지 알리는 줄이다. 무엇을 건너뛰었는지 말해야 결과를 믿을 수 있다.
type grepHints struct {
	options      grepOptions
	reports      []grepFileReport
	skippedOld   []string
	binary       []string
	verifyFailed bool
}

func (hints *grepHints) render(out io.Writer, color bool) {
	emit := func(text string) {
		line := T("grep.hint.prefix") + text
		if color {
			line = richDim(line)
		}
		fmt.Fprintln(out, line)
	}
	small := 0
	for _, report := range hints.reports {
		name := report.path
		switch report.index {
		case grepIndexOutcomeOff:
			emit(T("grep.hint.index_off", name))
		case grepIndexOutcomeSmall:
			small++
		case grepIndexOutcomeFirst:
			emit(T("grep.hint.index_first", name))
		case grepIndexOutcomeBuilt:
			emit(T("grep.hint.index_built", name, formatSizeShort(uint64(report.indexBytes)), grepPercent(report.indexBytes, report.size), report.built.Round(100*time.Millisecond)))
		case grepIndexOutcomeUsed:
			if report.sequential {
				emit(T("grep.hint.index_sequential_read", name, grepIndexSequentialPercent))
			} else {
				emit(T("grep.hint.index_used", name, groupThousands(int64(report.blocksRead)), groupThousands(int64(report.blocks)), formatSizeShort(uint64(report.bytesRead))))
			}
			if report.extended > 0 {
				emit(T("grep.hint.index_extended", name, groupThousands(int64(report.extended))))
			}
			if report.noLetters {
				emit(T("grep.hint.index_no_letters", name))
			}
		case grepIndexOutcomeInvalid:
			emit(T("grep.hint.index_invalid", name, report.reason))
			if report.indexBytes != 0 {
				emit(T("grep.hint.index_built", name, formatSizeShort(uint64(report.indexBytes)), grepPercent(report.indexBytes, report.size), report.built.Round(100*time.Millisecond)))
			}
		case grepIndexOutcomeStale:
			emit(T("grep.hint.index_stale", name))
		case grepIndexOutcomeLast:
			emit(T("grep.hint.index_last", name, formatSizeShort(uint64(report.lastRead))))
		case grepIndexOutcomeSequential:
			emit(T("grep.hint.index_sequential", name))
		case grepIndexOutcomeUnavailable:
			emit(T("grep.hint.index_unavailable", name))
		case grepIndexOutcomeNotBuilt:
			emit(T("grep.hint.index_not_built", name, report.reason))
		default:
			emit(T("grep.hint.index_none", name))
		}
		switch {
		case report.skipped > 0:
			emit(T("grep.hint.seek", name, formatSizeShort(uint64(report.skipped)), formatSizeShort(uint64(report.bytesRead))))
		case report.noTime:
			emit(T("grep.hint.no_time", name))
		case report.seekFailed:
			emit(T("grep.hint.seek_failed", name))
		case report.unordered:
			emit(T("grep.hint.since_whole", name))
		}
		if report.counted > 0 {
			emit(T("grep.hint.counted", name, groupThousands(report.counted)))
		}
		if report.lastRead > 0 {
			emit(T("grep.hint.last", name, formatSizeShort(uint64(report.lastRead))))
		}
	}
	if small > 0 {
		emit(T("grep.hint.index_small", small, formatSizeShort(grepIndexMinSize)))
	}
	if len(hints.skippedOld) > 0 {
		emit(T("grep.hint.skipped_old", len(hints.skippedOld), grepJoinNames(hints.skippedOld)))
	}
	if len(hints.binary) > 0 {
		emit(T("grep.hint.binary", len(hints.binary), grepJoinNames(hints.binary)))
	}
}

func grepJoinNames(names []string) string {
	if len(names) <= grepHintNames {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:grepHintNames], ", ") + ", …"
}

func grepPercent(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}
