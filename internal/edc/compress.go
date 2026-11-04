package edc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/x-mesh/fastcompress/gzip"
	"github.com/x-mesh/fastcompress/zstd"
)

const (
	compressUsage   = "edc compress [--zstd] [-l N] [-j N] [-c | -o path] [--rm] [file...]"
	decompressUsage = "edc decompress [-c | -o path] [--rm] [file...]"
	// gzip의 기본 수준은 gzip CLI와 같은 6이다. zstd는 3부터 double fast로 압축하고 그보다 높여도 더 줄지 않는다.
	compressGzipLevel = 6
	compressZstdLevel = 3
	// codecBufferSize는 한 번에 읽어 압축기에 넘기는 양이다. 병렬 gzip은 약 1MB씩 나눠 압축하므로 그보다 작게 넘기면
	// 조각을 모으는 동안 goroutine이 논다.
	codecBufferSize = 1 << 20
)

var (
	errCodecExists     = errors.New("output exists")
	errCodecSuffix     = errors.New("already compressed")
	errCodecNotRegular = errors.New("not a regular file")
	errCodecUnknown    = errors.New("not gzip or zstd data")
	errCodecNoName     = errors.New("no output name")
)

type codecOptions struct {
	command string
	stdin   io.Reader
	stdout  *os.File
	stderr  io.Writer
	toPipe  bool
	output  string
	remove  bool
	// convert는 src를 끝까지 읽어 dst에 압축하거나 푼다.
	convert func(dst io.Writer, src io.Reader) error
	// target은 입력 경로로 출력 경로를 정한다.
	target func(path string) (string, error)
}

func runCompress(args []string) int {
	return compressRun(args, os.Stdin, os.Stdout, os.Stderr)
}

func runDecompress(args []string) int {
	return decompressRun(args, os.Stdin, os.Stdout, os.Stderr)
}

func compressRun(args []string, stdin, stdout, stderr *os.File) int {
	set := newFlagSet("compress")
	set.SetOutput(stderr)
	useZstd := set.Bool("zstd", false, T("command.compress.option.zstd"))
	level := set.Int("l", 0, T("command.compress.option.level"))
	jobs := set.Int("j", runtime.GOMAXPROCS(0), T("command.compress.option.jobs"))
	options := codecOptions{command: "compress", stdin: stdin, stdout: stdout, stderr: stderr}
	set.BoolVar(&options.toPipe, "c", false, T("command.compress.option.stdout"))
	set.StringVar(&options.output, "o", "", T("command.compress.option.output"))
	set.BoolVar(&options.remove, "rm", false, T("command.compress.option.remove"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	if *level == 0 {
		*level = compressGzipLevel
		if *useZstd {
			*level = compressZstdLevel
		}
	}
	if *level < 1 || *level > 9 {
		fmt.Fprintln(stderr, T("compress.error.level", *level))
		return 2
	}
	if *jobs < 1 {
		fmt.Fprintln(stderr, T("compress.error.jobs", *jobs))
		return 2
	}
	suffix := ".gz"
	if *useZstd {
		suffix = ".zst"
	}
	options.convert = func(dst io.Writer, src io.Reader) error {
		if *useZstd {
			return compressZstd(dst, src, *level)
		}
		return compressGzip(dst, src, *level, *jobs)
	}
	options.target = func(path string) (string, error) {
		if logDecompressor(path) != nil {
			return "", errCodecSuffix
		}
		return path + suffix, nil
	}
	return runCodec(set.Args(), options, compressUsage)
}

func decompressRun(args []string, stdin, stdout, stderr *os.File) int {
	set := newFlagSet("decompress")
	set.SetOutput(stderr)
	options := codecOptions{command: "decompress", stdin: stdin, stdout: stdout, stderr: stderr, convert: decompressStream}
	set.BoolVar(&options.toPipe, "c", false, T("command.decompress.option.stdout"))
	set.StringVar(&options.output, "o", "", T("command.decompress.option.output"))
	set.BoolVar(&options.remove, "rm", false, T("command.decompress.option.remove"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	options.target = func(path string) (string, error) {
		for _, suffix := range []string{".gz", ".zst"} {
			if name, ok := strings.CutSuffix(path, suffix); ok && name != "" && !strings.HasSuffix(name, "/") {
				return name, nil
			}
		}
		return "", errCodecNoName
	}
	return runCodec(set.Args(), options, decompressUsage)
}

// runCodec은 파일로 쓴 입력마다 결과를 한 줄씩 stderr에 쓰고, 실패한 입력이 있어도 나머지를 처리한 뒤 1로 끝난다.
func runCodec(paths []string, options codecOptions, usage string) int {
	if options.toPipe && options.output != "" || options.output != "" && len(paths) > 1 {
		fmt.Fprintln(options.stderr, T("cli.usage", usage))
		return 2
	}
	if len(paths) == 0 {
		paths = []string{"-"}
	}
	for _, path := range paths {
		if path == "-" && (options.remove || len(paths) > 1) {
			fmt.Fprintln(options.stderr, T("cli.usage", usage))
			return 2
		}
	}
	if options.remove && options.toPipe {
		fmt.Fprintln(options.stderr, T("cli.usage", usage))
		return 2
	}
	toStdout := options.toPipe || options.output == "" && paths[0] == "-"
	// 압축한 바이트가 터미널에 쏟아지면 화면이 깨지고 쓸 데가 없다. 압축을 푼 글자는 cat처럼 터미널에 내도 된다.
	if toStdout && options.command == "compress" && isTerminal(options.stdout) {
		fmt.Fprintln(options.stderr, T("compress.error.terminal"))
		return 2
	}
	failed := false
	for _, path := range paths {
		result, err := codecPath(path, options, toStdout)
		if err != nil {
			fmt.Fprintln(options.stderr, T("compress.failed", options.command, codecName(path), codecErrorText(err)))
			failed = true
			continue
		}
		// stdout으로 쓸 때는 받는 쪽이 less 같은 화면일 수 있어 결과 줄을 섞지 않는다.
		if !toStdout {
			fmt.Fprintln(options.stderr, result.line())
		}
	}
	if failed {
		return 1
	}
	return 0
}

type codecResult struct {
	input, output string
	read, written int64
	elapsed       time.Duration
	// decompress면 read가 압축본이다. 비율은 어느 쪽이든 압축본 크기를 원본 크기로 나눈 값이다.
	decompress bool
}

func (result codecResult) line() string {
	plain, packed := result.read, result.written
	if result.decompress {
		plain, packed = packed, plain
	}
	ratio := "-"
	if plain > 0 {
		ratio = fmt.Sprintf("%.1f%%", float64(packed)*100/float64(plain))
	}
	return T("compress.result", result.input, result.output, formatSizeShort(uint64(result.read)), formatSizeShort(uint64(result.written)), ratio, fmt.Sprintf("%.1fs", result.elapsed.Seconds()))
}

func codecName(path string) string {
	if path == "-" {
		return "stdin"
	}
	return path
}

func codecErrorText(err error) string {
	switch {
	case errors.Is(err, errCodecExists):
		return T("compress.error.exists")
	case errors.Is(err, errCodecSuffix):
		return T("compress.error.suffix")
	case errors.Is(err, errCodecNotRegular):
		return T("compress.error.not_regular")
	case errors.Is(err, errCodecUnknown):
		return T("compress.error.unknown_format")
	case errors.Is(err, errCodecNoName):
		return T("compress.error.no_name")
	}
	return err.Error()
}

// codecPath는 입력 하나를 처리한다. 파일로 쓸 때는 다 쓰고 fsync한 뒤에만 원본을 지우고, 도중에 실패하면 만들던 파일을
// 지운다. 반쯤 쓴 출력이 남으면 다음 실행이 덮어쓰기를 거부하고, --rm이 원본을 지운 뒤라면 로그를 잃는다.
func codecPath(path string, options codecOptions, toStdout bool) (codecResult, error) {
	started := time.Now()
	result := codecResult{input: codecName(path), output: "stdout", decompress: options.command == "decompress"}
	src := options.stdin
	var info fs.FileInfo
	if path != "-" {
		var err error
		if info, err = os.Lstat(path); err != nil {
			return result, err
		}
		// 심볼릭 링크를 따라가 압축하고 --rm으로 링크만 지우면 원본은 그대로 남고 압축본만 늘어난다.
		if !info.Mode().IsRegular() {
			return result, errCodecNotRegular
		}
		file, err := os.Open(path)
		if err != nil {
			return result, err
		}
		defer file.Close()
		src = file
	}
	counted := &codecCounter{reader: src}
	if toStdout {
		written := &codecWriteCounter{writer: options.stdout}
		err := options.convert(written, counted)
		result.read, result.written, result.elapsed = counted.count, written.count, time.Since(started)
		return result, err
	}
	target := options.output
	if target == "" {
		var err error
		if target, err = options.target(path); err != nil {
			return result, err
		}
	}
	result.output = target
	mode := fs.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrExist) {
		return result, fmt.Errorf("%s: %w", target, errCodecExists)
	}
	if err != nil {
		return result, err
	}
	written := &codecWriteCounter{writer: out}
	err = options.convert(written, counted)
	if err == nil {
		err = codecKeepMetadata(out, target, info)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(target)
		return result, err
	}
	if options.remove {
		if err := os.Remove(path); err != nil {
			return result, err
		}
	}
	result.read, result.written, result.elapsed = counted.count, written.count, time.Since(started)
	return result, nil
}

// codecKeepMetadata는 원본의 권한과 수정 시각을 출력에 옮긴다. 만들 때 준 권한은 umask가 깎으므로 다시 정한다.
// edc grep --ordered는 회전된 .gz와 .zst의 수정 시각을 마지막 줄의 시각으로 보고 건너뛰므로, 압축한 시각이 남으면 오래된
// 로그를 다시 읽는다. root로 실행하면 소유자도 옮긴다. 일반 사용자는 남의 소유로 바꿀 수 없다.
func codecKeepMetadata(out *os.File, target string, info fs.FileInfo) error {
	if info == nil {
		return nil
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		uid, gid, err := fileOwner(info)
		if err != nil {
			return err
		}
		if err := out.Chown(uid, gid); err != nil {
			return err
		}
	}
	return os.Chtimes(target, info.ModTime(), info.ModTime())
}

func compressGzip(dst io.Writer, src io.Reader, level, jobs int) error {
	writer, err := gzip.NewWriterLevel(dst, level)
	if err != nil {
		return err
	}
	if jobs > 1 {
		if err := writer.SetConcurrency(jobs); err != nil {
			return err
		}
	}
	if _, err := io.CopyBuffer(writer, src, make([]byte, codecBufferSize)); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}

func compressZstd(dst io.Writer, src io.Reader, level int) error {
	writer := zstd.NewWriter(dst, level)
	if _, err := io.CopyBuffer(writer, src, make([]byte, codecBufferSize)); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}

// decompressStream은 앞 4바이트로 형식을 고른다. 이름을 바꾼 파일이나 stdin은 확장자로 알 수 없다.
func decompressStream(dst io.Writer, src io.Reader) error {
	magic := make([]byte, 4)
	n, err := io.ReadFull(src, magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	magic = magic[:n]
	whole := io.MultiReader(bytes.NewReader(magic), src)
	var reader io.ReadCloser
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		reader, err = gzip.NewReader(whole)
	case len(magic) == 4 && (bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}) || magic[0]&0xf0 == 0x50 && bytes.Equal(magic[1:], []byte{0x2a, 0x4d, 0x18})):
		reader, err = zstd.NewReader(whole)
	default:
		return errCodecUnknown
	}
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = io.CopyBuffer(dst, reader, make([]byte, codecBufferSize))
	return err
}

type codecCounter struct {
	reader io.Reader
	count  int64
}

func (counter *codecCounter) Read(p []byte) (int, error) {
	n, err := counter.reader.Read(p)
	counter.count += int64(n)
	return n, err
}

type codecWriteCounter struct {
	writer io.Writer
	count  int64
}

func (counter *codecWriteCounter) Write(p []byte) (int, error) {
	n, err := counter.writer.Write(p)
	counter.count += int64(n)
	return n, err
}
