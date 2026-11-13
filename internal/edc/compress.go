package edc

import (
	"bytes"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
	errCodecChanged    = errors.New("input changed")
	errCodecNoProc     = errors.New("/proc is not available")
	errCodecUnchecked  = errors.New("cannot check every process that may write the input")
)

// codecInUseError는 --rm이 지우려던 입력을 다른 프로세스가 쓰기 모드로 열어 둔 경우다. pid를 알 수 없으면 0이다.
type codecInUseError struct{ pid int }

func (err codecInUseError) Error() string {
	if err.pid == 0 {
		return "another process has the input open for writing"
	}
	return "process " + strconv.Itoa(err.pid) + " has the input open for writing"
}

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
	// keep은 --rm이어도 입력을 남길지 읽은 크기와 쓴 크기로 정한다. nil이면 늘 지운다.
	keep func(read, written int64) bool
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
	// kept는 --rm이었지만 keep이 입력을 남겼다는 뜻이다.
	kept bool
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
	var inUse codecInUseError
	if errors.As(err, &inUse) {
		if inUse.pid == 0 {
			return T("compress.error.in_use_unknown")
		}
		return T("compress.error.in_use", inUse.pid)
	}
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
	case errors.Is(err, errCodecChanged):
		return T("compress.error.changed")
	case errors.Is(err, errCodecNoProc):
		return T("compress.error.no_proc")
	case errors.Is(err, errCodecUnchecked):
		return T("compress.error.unchecked")
	}
	return err.Error()
}

// codecPath는 입력 하나를 처리한다. 파일로 쓸 때는 같은 디렉터리의 임시 파일에 다 쓰고 fsync한 뒤 출력 이름을 붙이고,
// 그다음에만 원본을 지운다. 출력 이름에 반쯤 쓴 파일이 남으면 다음 실행이 덮어쓰기를 거부하고, --rm이 원본을 지운 뒤라면
// 로그를 잃는다. 프로세스가 죽어도 남는 것은 점으로 시작하는 임시 파일뿐이다.
func codecPath(path string, options codecOptions, toStdout bool) (codecResult, error) {
	started := time.Now()
	result := codecResult{input: codecName(path), output: "stdout", decompress: options.command == "decompress"}
	src := options.stdin
	var input *os.File
	var info fs.FileInfo
	var digest maphash.Hash
	if path != "-" {
		var err error
		if input, info, err = codecOpenInput(path); err != nil {
			return result, err
		}
		defer input.Close()
		src = input
		if options.remove {
			src = io.TeeReader(input, &digest)
		}
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
	// 끝까지 변환한 뒤에야 이름이 겹친다고 알리지 않도록 먼저 본다. 그 사이에 생긴 파일은 아래 os.Link가 거부한다.
	if _, err := os.Lstat(target); err == nil {
		return result, fmt.Errorf("%s: %w", target, errCodecExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return result, err
	}
	mode := fs.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	out, temp, err := codecCreateTemp(target, mode)
	if err != nil {
		return result, err
	}
	written := &codecWriteCounter{writer: out}
	err = options.convert(written, counted)
	if err == nil {
		err = codecKeepMetadata(out, temp, info)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	// rename은 그 사이에 생긴 출력을 덮어쓰므로, 이름이 없을 때만 성공하는 hard link로 붙이고 임시 이름을 지운다.
	if err == nil {
		if err = os.Link(temp, target); errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("%s: %w", target, errCodecExists)
		}
	}
	os.Remove(temp)
	if err != nil {
		return result, err
	}
	result.read, result.written, result.elapsed = counted.count, written.count, time.Since(started)
	if options.remove && options.keep != nil && options.keep(result.read, result.written) {
		result.kept = true
		return result, nil
	}
	if options.remove {
		// 출력 이름이 디스크에 남기 전에 원본을 지운 상태로 전원이 꺼지면 둘 다 잃을 수 있다.
		if err := codecSyncDir(filepath.Dir(target)); err != nil {
			return result, err
		}
		if err := codecRemoveInput(path, input, info, counted.count, &digest); err != nil {
			return result, err
		}
	}
	return result, nil
}

// codecOpenInput은 일반 파일만 연다. 장치 파일은 열기만 해도 동작할 수 있어 Lstat으로 먼저 거른다. Lstat과 open 사이에
// 경로가 심볼릭 링크로 바뀌어도 따라가지 않도록 O_NOFOLLOW로 열고, 연 파일이 Lstat으로 본 그 파일인지 다시 확인한다.
// O_NONBLOCK은 그 사이에 FIFO로 바뀌었을 때 쓰는 쪽을 기다리며 멈추지 않게 한다. 일반 파일의 읽기에는 영향이 없다.
func codecOpenInput(path string) (*os.File, fs.FileInfo, error) {
	checked, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	// 심볼릭 링크를 따라가 압축하고 --rm으로 링크만 지우면 원본은 그대로 남고 압축본만 늘어난다.
	if !checked.Mode().IsRegular() {
		return nil, nil, errCodecNotRegular
	}
	return codecOpenChecked(path, checked)
}

func codecOpenChecked(path string, checked fs.FileInfo) (*os.File, fs.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, nil, errCodecNotRegular
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || !os.SameFile(checked, info)) {
		err = errCodecNotRegular
	}
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

// codecCreateTemp는 target과 같은 디렉터리에 임시 파일을 만든다. 같은 파일 시스템이어야 hard link로 이름을 붙일 수 있다.
// os.CreateTemp는 권한을 0600으로 고정하므로, stdin에서 받은 출력도 umask를 거친 mode를 받도록 직접 만든다.
func codecCreateTemp(target string, mode fs.FileMode) (*os.File, string, error) {
	dir, base := filepath.Split(target)
	for range 16 {
		temp := filepath.Join(dir, "."+base+"."+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
		file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, temp, err
	}
	return nil, "", fmt.Errorf("%s: %w", target, errCodecExists)
}

func codecSyncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// codecRemoveHook은 시험만 쓴다. --rm이 입력을 옮기기 직전("move"), 지울지 마지막으로 보기 직전("delete"), 원래 이름으로
// 되돌리기 직전("restore")에 불려, 그 사이에 입력이 바뀌는 경우를 만든다.
var codecRemoveHook func(stage string)

// codecRemoveInput은 변환한 그 파일만 지운다. 경로를 확인하고 경로로 지우면 그 사이에 같은 이름으로 생긴 파일을 지우므로,
// 입력을 같은 디렉터리에 새로 만든 빈 디렉터리로 옮긴 뒤 옮겨진 파일이 연 파일 그대로일 때만 지운다. 아니면 원래 이름으로
// 되돌린다. 옮긴 뒤에는 경로로 그 파일을 열 수 없으므로, 열린 fd로 다시 읽어 변환한 데이터 그대로인지 본다. 같은 크기로
// 고쳐 쓰고 수정 시각을 되돌린 입력도 이것으로 잡는다. 크기와 수정 시각은 아직 쓰이는 로그를 옮기기 전에 거르는 데 쓴다.
// 다시 읽기 전에 그 파일을 쓰기 모드로 연 프로세스가 없는지 본다. 열린 fd로 쓰는 것은 이름을 옮겨도 막을 수 없으므로,
// 그런 프로세스가 있으면 다시 읽은 뒤에도 쓸 수 있다.
func codecRemoveInput(path string, input *os.File, opened fs.FileInfo, read int64, digest *maphash.Hash) error {
	if err := codecInputUnchanged(input, opened, read); err != nil {
		return err
	}
	if codecRemoveHook != nil {
		codecRemoveHook("move")
	}
	// filepath.Split은 상대 경로 "app.log"에 빈 디렉터리를 주고, MkdirTemp는 빈 디렉터리를 시스템 임시 디렉터리로 읽는다.
	// 그 디렉터리가 다른 파일 시스템이면 rename이 실패하므로 입력과 같은 디렉터리를 Dir로 정한다.
	dir, base := filepath.Dir(path), filepath.Base(path)
	hold, err := os.MkdirTemp(dir, "."+base+".*.rm")
	if err != nil {
		return err
	}
	held := filepath.Join(hold, base)
	if err := os.Rename(path, held); err != nil {
		os.Remove(hold)
		return err
	}
	moved, err := os.Lstat(held)
	if err == nil && !os.SameFile(opened, moved) {
		err = errCodecChanged
	}
	leased := false
	if err == nil {
		leased, err = codecCheckWriters(input, held, moved)
	}
	if err == nil {
		err = codecSameData(input, read, digest)
	}
	if err == nil && codecRemoveHook != nil {
		codecRemoveHook("delete")
	}
	// 다시 읽는 동안 쓰기 모드로 연 open은 lease가 풀릴 때까지 기다린다. 지운 뒤에 열리면 그 데이터는 지운 inode로 가므로,
	// 지우기 직전에 lease가 깨지는 중인지 본다.
	if err == nil && leased && codecLeaseBroken(input) {
		err = codecInUseError{}
	}
	if err == nil {
		if err = os.Remove(held); err == nil {
			return os.Remove(hold)
		}
	}
	// rename으로 되돌리면 그 사이에 같은 이름으로 생긴 파일을 덮어쓰므로, 이름이 없을 때만 성공하는 hard link를 쓴다.
	// 실패하면 입력을 옮긴 자리에 남기고, 두 경로가 든 LinkError로 알린다.
	if codecRemoveHook != nil {
		codecRemoveHook("restore")
	}
	if linkErr := os.Link(held, path); linkErr != nil {
		return linkErr
	}
	os.Remove(held)
	os.Remove(hold)
	return err
}

// codecInputUnchanged는 변환하는 동안 입력이 더 쓰였거나 같은 크기로 고쳐 쓰였는지 크기와 수정 시각으로 본다.
func codecInputUnchanged(input *os.File, opened fs.FileInfo, read int64) error {
	now, err := input.Stat()
	if err != nil {
		return err
	}
	if now.Size() != read || !now.ModTime().Equal(opened.ModTime()) {
		return errCodecChanged
	}
	return nil
}

// codecSameData는 입력을 처음부터 다시 읽어 변환할 때 읽은 데이터와 같은지 hash로 본다. 한 바이트 더 읽어 그사이 늘어난
// 입력도 잡는다.
func codecSameData(input *os.File, read int64, digest *maphash.Hash) error {
	var again maphash.Hash
	again.SetSeed(digest.Seed())
	n, err := io.CopyBuffer(&again, io.NewSectionReader(input, 0, read+1), make([]byte, codecBufferSize))
	if err != nil {
		return err
	}
	if n != read || again.Sum64() != digest.Sum64() {
		return errCodecChanged
	}
	return nil
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
	writer, err := newGzipWriter(dst, level, jobs)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(writer, src, make([]byte, codecBufferSize)); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}

func newZstdWriter(dst io.Writer, level int) io.WriteCloser { return zstd.NewWriter(dst, level) }

// newGzipWriter는 jobs개 goroutine으로 압축하는 gzip writer다. edc pack은 tar를 이 writer에 바로 쓴다.
func newGzipWriter(dst io.Writer, level, jobs int) (io.WriteCloser, error) {
	writer, err := gzip.NewWriterLevel(dst, level)
	if err != nil {
		return nil, err
	}
	if jobs > 1 {
		if err := writer.SetConcurrency(jobs); err != nil {
			return nil, err
		}
	}
	return writer, nil
}

func compressZstd(dst io.Writer, src io.Reader, level int) error {
	writer := newZstdWriter(dst, level)
	if _, err := io.CopyBuffer(writer, src, make([]byte, codecBufferSize)); err != nil {
		writer.Close()
		return err
	}
	return writer.Close()
}

// decompressStream은 앞 4바이트로 형식을 고른다. 이름을 바꾼 파일이나 stdin은 확장자로 알 수 없다.
func decompressStream(dst io.Writer, src io.Reader) error {
	reader, err := codecOpenStream(src)
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = io.CopyBuffer(dst, reader, make([]byte, codecBufferSize))
	return err
}

// codecFormat은 앞 4바이트가 가리키는 압축 형식이다. gzip, zstd가 아니면 빈 문자열이다. zstd의 skippable frame도 zstd로 본다.
func codecFormat(magic []byte) string {
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return "gzip"
	case len(magic) == 4 && (bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}) || magic[0]&0xf0 == 0x50 && bytes.Equal(magic[1:], []byte{0x2a, 0x4d, 0x18})):
		return "zstd"
	}
	return ""
}

// codecOpenStream은 src의 앞 4바이트로 형식을 골라 푸는 reader를 연다. 읽은 4바이트는 되돌려 넣는다.
func codecOpenStream(src io.Reader) (io.ReadCloser, error) {
	magic := make([]byte, 4)
	n, err := io.ReadFull(src, magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	magic = magic[:n]
	whole := io.MultiReader(bytes.NewReader(magic), src)
	var reader io.ReadCloser
	switch codecFormat(magic) {
	case "gzip":
		reader, err = gzip.NewReader(whole)
	case "zstd":
		reader, err = zstd.NewReader(whole)
	default:
		return nil, errCodecUnknown
	}
	// NewReader는 실패하면 nil 포인터를 돌려준다. 그대로 interface에 담으면 nil이 아닌 값이 되어 Close를 부를 수 있다.
	if err != nil {
		return nil, err
	}
	return reader, nil
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
