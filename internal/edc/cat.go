package edc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const catUsage = "edc cat [-n] [-r] [--hidden] [--no-ignore] [--mask] [--as <format>] [-p] [path...]"

// catStdinName은 stdin을 읽으라는 경로 인자이고, catStdinTitle은 그 입력의 위치 줄에 쓰는 이름이다.
const (
	catStdinName  = "-"
	catStdinTitle = "stdin"
)

type catOptions struct {
	recursive bool
	hidden    bool
	noIgnore  bool
}

func runCat(args []string) int {
	set := newFlagSet("cat")
	set.SetOutput(os.Stderr)
	var options catOptions
	set.BoolVar(&options.recursive, "r", false, T("command.cat.option.recursive"))
	set.BoolVar(&options.hidden, "hidden", false, T("command.cat.option.hidden"))
	set.BoolVar(&options.noIgnore, "no-ignore", false, T("command.cat.option.no_ignore"))
	numbers := set.Bool("n", false, T("command.cat.option.numbers"))
	mask := set.Bool("mask", false, T("command.cat.option.mask"))
	as := set.String("as", "", T("command.cat.option.as"))
	pagerWanted := set.Bool("p", false, T("command.cat.option.pager"))
	set.BoolVar(pagerWanted, "pager", false, T("command.cat.option.pager"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	paths := set.Args()
	if len(paths) == 0 {
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, T("cli.usage", catUsage))
			return 2
		}
		paths = []string{catStdinName}
	}
	output := &catOutput{stdin: os.Stdin, stderr: os.Stderr, terminal: isTerminal(os.Stdout), color: richEnabled(os.Stdout), mask: *mask, numbers: *numbers}
	if *as != "" {
		syntax, ok := catSyntaxNamed(*as)
		if !ok {
			fmt.Fprintln(os.Stderr, T("cat.as_unknown", *as))
			return 2
		}
		output.as = &syntax
	}
	var target io.Writer = os.Stdout
	var pager *catPager
	// 키보드로 stdin을 받는 중에는 pager를 띄우지 않는다. edc와 pager가 같은 키 입력을 나눠 읽게 된다.
	typed := slices.Contains(paths, catStdinName) && isTerminal(os.Stdin)
	if *pagerWanted && output.terminal && !typed {
		if pager = startCatPager(catPagerCommand(os.LookupEnv)); pager != nil {
			// pager가 화면을 쓰는 동안 stderr에 쓰면 화면이 깨진다. 문제 줄은 모아 두었다가 pager가 끝난 뒤에 낸다.
			target, output.stderr = pager.stdin, &pager.held
		}
	}
	output.stdout = bufio.NewWriter(target)
	ok := catPaths(output, paths, options)
	err := output.failed
	if err == nil {
		err = output.stdout.Flush()
	}
	if pager != nil {
		pager.wait()
		// pager를 끝까지 보지 않고 닫으면 남은 내용을 쓸 곳이 없다. 사용자가 고른 일이라 실패로 보지 않는다.
		if errors.Is(err, syscall.EPIPE) {
			err = nil
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cat.failed", err))
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

// catPagerCommand는 -p로 출력을 넘길 pager 명령이다. 빈 문자열이면 pager를 쓰지 않는다. PAGER를 비우거나 cat으로 두면
// 쓰지 않고, PAGER가 없으면 less다.
func catPagerCommand(lookup func(string) (string, bool)) string {
	// TERM이 없거나 dumb이면 less가 "terminal is not fully functional"을 내고 Enter를 기다린다.
	if term, _ := lookup("TERM"); term == "" || term == "dumb" {
		return ""
	}
	value, ok := lookup("PAGER")
	if !ok {
		return "less"
	}
	if value = strings.TrimSpace(value); value == "cat" {
		return ""
	}
	return value
}

type catPager struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	held    bytes.Buffer
	exited  chan struct{}
}

// startCatPager는 pager를 띄운다. 명령이 비었으면 nil이다. shell을 거치지 않고 공백으로 나눈 명령을 직접 실행해서,
// 명령이 없을 때 여기서 바로 알 수 있다.
func startCatPager(command string) *catPager {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil
	}
	process := exec.Command(fields[0], fields[1:]...)
	process.Stdout, process.Stderr = os.Stdout, os.Stderr
	// git과 같은 기본값이다. F는 한 화면에 들어가면 바로 끝내고, R은 색을 그대로 보이며, X는 끝난 뒤에도 내용을 화면에 남긴다.
	if _, ok := os.LookupEnv("LESS"); !ok {
		process.Env = append(os.Environ(), "LESS=FRX")
	}
	stdin, err := process.StdinPipe()
	if err != nil {
		return nil
	}
	// less가 없는 container에서도 내용은 나와야 한다. pager를 띄우지 못하면 stdout에 바로 쓴다.
	if err := process.Start(); err != nil {
		return nil
	}
	// Ctrl-C는 pager와 edc가 함께 받는다. edc가 먼저 죽으면 pager가 터미널을 쥔 채 남으므로 pager가 끝날 때까지 무시한다.
	// pager가 끝나면 바로 되돌린다. 끝나지 않는 입력을 읽고 있으면 그 뒤의 Ctrl-C로 edc를 멈출 수 있어야 한다.
	// signal.Ignore는 쓰지 않는다. 한 번 무시한 SIGINT는 signal.Reset으로 기본 동작에 돌아오지 않는다.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	pager := &catPager{command: process, stdin: stdin, exited: make(chan struct{})}
	go func() {
		_ = process.Wait()
		signal.Stop(interrupts)
		close(pager.exited)
	}()
	return pager
}

// wait는 pager가 끝나기를 기다린 뒤, 모아 둔 문제 줄을 stderr에 쓴다. 종료 상태는 edc가 정하므로 pager의 것은 버린다.
func (pager *catPager) wait() {
	pager.stdin.Close()
	<-pager.exited
	os.Stderr.Write(pager.held.Bytes())
}

// catOutput은 입력을 내는 방식이다. 터미널에서는 위치 줄을 쓰고 내용의 escape를 지운다. 터미널이 아니면 cat처럼
// 바이트를 그대로 내서 `edc cat a b > out`이 원본을 이어 붙인 파일이 된다. mask와 numbers는 어느 쪽에서도 적용한다.
type catOutput struct {
	stdout   *bufio.Writer
	stderr   io.Writer
	stdin    io.Reader
	terminal bool
	color    bool
	mask     bool
	numbers  bool
	// as는 --as로 정한 형식이다. nil이면 파일 이름으로 정한다.
	as *catSyntax
	// shown은 위치 줄을 쓴 입력 수다. 둘째부터 위치 줄 앞에 빈 줄을 둔다.
	shown int
	// failed는 stdout에 쓰다 난 첫 오류다. 그 뒤로는 읽지 않는다.
	failed error
}

func (output *catOutput) write(text string) {
	if output.failed != nil {
		return
	}
	if _, err := output.stdout.WriteString(text); err != nil {
		output.failed = err
	}
}

// problem은 stderr에 한 줄을 쓴다. 터미널에서 앞 파일의 내용보다 먼저 보이지 않도록 stdout을 먼저 비운다.
// 디렉터리에서 읽은 파일 이름이 들어가므로, 이름에 든 escape가 터미널을 움직이지 않게 지운다.
func (output *catOutput) problem(message string) {
	output.stdout.Flush()
	fmt.Fprintln(output.stderr, viewSanitize(message))
}

// catPaths는 인자를 순서대로 낸다. 하나라도 읽지 못하거나 건너뛰면 false다. 나머지 입력은 계속 낸다.
func catPaths(output *catOutput, paths []string, options catOptions) bool {
	ok := true
	for _, path := range paths {
		if output.failed != nil {
			break
		}
		if path == catStdinName {
			ok = output.content(catStdinTitle, "", output.stdin, -1) && ok
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			output.problem(T("cat.failed", err))
			ok = false
			continue
		}
		if !info.IsDir() {
			ok = output.file(path) && ok
			continue
		}
		files, problems := catDirectoryFiles(path, options)
		for _, problem := range problems {
			output.problem(T("cat.failed", problem))
			ok = false
		}
		if len(files) == 0 && len(problems) == 0 {
			output.problem(T("cat.no_files", path))
		}
		for _, file := range files {
			if output.failed != nil {
				break
			}
			ok = output.file(file) && ok
		}
	}
	return ok
}

// catDirectoryFiles는 디렉터리에서 낼 파일을 이름순으로 고른다. 기본은 바로 아래의 파일이고, symlink가 가리키는 파일도
// 포함한다(/etc/nginx/sites-enabled). recursive면 edc grep --code와 같은 탐색을 써서 .gitignore를 적용하고 symlink는
// 따라가지 않는다.
func catDirectoryFiles(dir string, options catOptions) ([]string, []string) {
	if options.recursive {
		files, _, problems := grepCodeTargets([]string{dir}, grepOptions{hidden: options.hidden, noIgnore: options.noIgnore})
		return files, problems
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var files []string
	for _, entry := range entries {
		if !options.hidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			files = append(files, path)
		}
	}
	return files, nil
}

// file은 파일 하나를 낸다. 읽지 못하거나 건너뛰면 false다.
func (output *catOutput) file(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		output.problem(T("cat.failed", err))
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err == nil && info.IsDir() {
		err = fmt.Errorf("%s: %w", path, syscall.EISDIR)
	}
	if err != nil {
		output.problem(T("cat.failed", err))
		return false
	}
	return output.content(path, path, file, info.Size())
}

// content는 입력 하나를 낸다. title은 위치 줄에 쓰는 이름이고, name은 형식을 정하는 파일 이름이다(stdin은 빈 문자열).
// size가 -1이면 크기를 모른다. 읽지 못하거나 건너뛰면 false다.
func (output *catOutput) content(title, name string, source io.Reader, size int64) bool {
	if !output.terminal && !output.mask && !output.numbers {
		if _, err := io.Copy(output.stdout, source); err != nil {
			output.problem(T("cat.failed", err))
			return false
		}
		return true
	}
	if !output.terminal && !output.mask && !output.color {
		return output.numbered(source)
	}
	// 앞부분은 한 번만 읽는다. pipe에서 8 KiB가 찰 때까지 기다리면 느리게 들어오는 입력이 그동안 보이지 않는다.
	sample := make([]byte, grepBinaryProbe)
	count, err := io.ReadAtLeast(source, sample, 1)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		output.problem(T("cat.failed", err))
		return false
	}
	sample = sample[:count]
	binary := bytes.IndexByte(sample, 0) >= 0
	syntax := catSyntax{}
	switch {
	case output.as != nil:
		syntax = *output.as
	case name != "":
		syntax = catSyntaxOf(name)
	}
	// 형식을 모르는 입력은 어디가 값인지 알 수 없다. 가리지 못한 채 내보내지 않고 건너뛴다.
	if output.mask && (binary || !syntax.maskable()) {
		output.problem(T("cat.mask_unsupported", title))
		return false
	}
	var rest io.Reader = io.MultiReader(bytes.NewReader(sample), source)
	var code []byte
	colored := output.terminal && output.color && syntax.code != nil && !binary
	if colored {
		if code, err = io.ReadAll(io.LimitReader(rest, catCodeLimit+1)); err != nil {
			output.problem(T("cat.failed", err))
			return false
		}
		if len(code) > catCodeLimit {
			rest, colored = io.MultiReader(bytes.NewReader(code), rest), false
		}
	}
	if output.terminal {
		// /proc 아래 파일처럼 크기를 0으로 알리지만 내용이 있는 파일은 크기를 적지 않는다.
		if size == 0 && len(sample) > 0 {
			size = -1
		}
		output.header(title, syntax.name(), size, binary, code != nil && !colored)
		if binary {
			return true
		}
	}
	if colored {
		output.code(syntax, string(code))
		return true
	}
	reader := bufio.NewReader(rest)
	lexer := catLexer{format: syntax.format, mask: output.mask}
	for number, continued := 1, false; ; {
		line, cut, err := readCatLine(reader)
		if line != "" {
			// 나누어 읽은 줄은 lexer가 값의 끝을 알 수 없다. 가리지 못한 값을 내보내지 않고 멈춘다.
			if cut && output.mask {
				output.problem(T("cat.failed", fmt.Errorf("%s: a line is longer than %d bytes and cannot be masked", title, catLineLimit)))
				return false
			}
			body, ending := line, ""
			if !cut {
				body = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
				ending = line[len(body):]
			}
			if output.terminal {
				// 파일에 든 escape가 터미널을 움직이지 못하게 지운다. 마지막 줄에 줄바꿈이 없어도 다음 위치 줄이 붙지 않게 한다.
				body = viewSanitize(body)
				if !cut {
					ending = "\n"
				}
			}
			switch {
			case continued:
				output.write(body + ending)
			case cut:
				output.line(number, body)
			default:
				output.line(number, catRender(lexer.line(body), output.color)+ending)
			}
		}
		if continued = cut; !cut {
			number++
		}
		if err == io.EOF || output.failed != nil {
			return true
		}
		if err != nil {
			output.problem(T("cat.failed", err))
			return false
		}
		// 읽어 둔 줄을 다 냈으면 내보낸다. 느리게 들어오는 pipe 입력이 buffer가 찰 때까지 묶이지 않는다.
		if reader.Buffered() == 0 {
			output.stdout.Flush()
		}
	}
}

// numbered는 터미널이 아니고 색과 mask가 없을 때 cat -n처럼 줄마다 번호를 붙인다. 이때는 구문을 나눠도 글자가 그대로라
// 읽은 byte를 문자열로 바꾸지 않고 쓴다. 줄마다 할당하던 경로는 470MB 로그에 5초가 걸렸고 cat -n은 0.36초였다.
func (output *catOutput) numbered(source io.Reader) bool {
	reader := bufio.NewReaderSize(source, 64<<10)
	prefix := make([]byte, 0, 32)
	start := true
	for number := 1; ; {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 && output.failed == nil {
			if start {
				prefix = strconv.AppendInt(prefix[:0], int64(number), 10)
				if pad := 6 - len(prefix); pad > 0 {
					prefix = append(prefix[:0], "      "[:pad]...)
					prefix = strconv.AppendInt(prefix, int64(number), 10)
				}
				prefix = append(prefix, '\t')
				_, output.failed = output.stdout.Write(prefix)
			}
			if output.failed == nil {
				_, output.failed = output.stdout.Write(chunk)
			}
		}
		// 버퍼보다 긴 줄은 나누어 읽는다. 이어지는 조각에는 번호를 붙이지 않는다.
		if start = err != bufio.ErrBufferFull; start && len(chunk) > 0 {
			number++
		}
		if err == io.EOF || output.failed != nil {
			return true
		}
		if err != nil && err != bufio.ErrBufferFull {
			output.problem(T("cat.failed", err))
			return false
		}
		// 읽어 둔 줄을 다 냈으면 내보낸다. 느리게 들어오는 pipe 입력이 buffer가 찰 때까지 묶이지 않는다.
		if reader.Buffered() == 0 {
			output.stdout.Flush()
		}
	}
}

// catLineLimit은 한 번에 모으는 줄의 크기 상한이다. minified 파일처럼 줄바꿈이 없는 큰 입력도 이만큼씩 나누어 내서
// 파일 크기만큼 메모리를 쓰지 않는다.
const catLineLimit = 1 << 20

// readCatLine은 줄 하나를 읽는다. 줄이 catLineLimit보다 길면 그만큼에서 끊고 cut을 참으로 돌려준다.
func readCatLine(reader *bufio.Reader) (line string, cut bool, err error) {
	var data []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		data = append(data, chunk...)
		if err != bufio.ErrBufferFull {
			return string(data), false, err
		}
		if len(data) >= catLineLimit {
			return string(data), true, nil
		}
	}
}

// code는 소스 코드 전체를 한 번에 나누어 낸다. 터미널에 색을 쓸 때만 온다.
func (output *catOutput) code(syntax catSyntax, text string) {
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for index, line := range lines {
		lines[index] = viewSanitize(strings.TrimSuffix(line, "\r"))
	}
	for index, spans := range catCodeLines(syntax.code, lines) {
		output.line(index+1, catRender(spans, true)+"\n")
	}
}

// line은 내용 한 줄을 쓴다. 줄 번호는 입력마다 1부터 센다. 터미널이 아니면 cat -n과 같은 모양으로 쓴다.
func (output *catOutput) line(number int, text string) {
	if output.numbers {
		switch {
		case !output.terminal:
			output.write(fmt.Sprintf("%6d\t", number))
		case output.color:
			output.write(richDim(fmt.Sprintf("%4d", number)) + "  ")
		default:
			output.write(fmt.Sprintf("%4d  ", number))
		}
	}
	output.write(text)
}

// header는 입력의 위치 줄이다. 색을 쓰지 않아도 ==> 로 내용과 구분된다.
func (output *catOutput) header(title, syntax string, size int64, binary, uncolored bool) {
	if output.shown > 0 {
		output.write("\n")
	}
	output.shown++
	var details []string
	if binary {
		details = append(details, "binary")
	} else if syntax != "" {
		details = append(details, syntax)
	}
	if size >= 0 {
		details = append(details, formatSizeShort(uint64(size)))
	}
	if binary {
		details = append(details, T("cat.binary_omitted"))
	}
	if uncolored {
		details = append(details, T("cat.color_omitted"))
	}
	name := viewSanitize(title)
	detail := ""
	if len(details) > 0 {
		detail = " (" + strings.Join(details, " · ") + ")"
	}
	if output.color {
		output.write(richDim("==>") + " " + richBold(name) + richDim(detail) + "\n")
		return
	}
	output.write("==> " + name + detail + "\n")
}
