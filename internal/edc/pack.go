package edc

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

const packUsage = "edc pack [-z | -x] [-l] [--zstd] [--level N] [--exclude PATTERN] [--gitignore] [-o path] [--force] [--rm] [-v] <path...>"

var (
	errPackUnknown   = errors.New("not gzip, zstd, xz, bzip2, zip, or tar data")
	errPackDirectory = errors.New("a directory is not an archive")
	errPackRemoveDir = errors.New("--rm works with files only")
	errPackInside    = errors.New("the output is inside the directory")
	errPackRoot      = errors.New("edc pack does not pack /")
	errPackListDir   = errors.New("-l reads an archive, not a directory")
	errPackNoReplace = errors.New("this file system cannot rename without replacing")
)

// packUnsafeError는 tar 항목이 풀 디렉터리 밖을 가리키는 경우다. name은 그 항목 이름이다.
type packUnsafeError struct {
	name string
	link bool
}

func (err packUnsafeError) Error() string {
	if err.link {
		return "link outside the output: " + err.name
	}
	return "entry outside the output: " + err.name
}

// packKeptError는 --rm이 건너뛴 항목 때문에 압축 파일을 남긴 경우다. 푼 결과는 제자리에 있다.
type packKeptError struct{ skipped int }

func (err packKeptError) Error() string {
	return fmt.Sprintf("%d entries were skipped; kept the archive", err.skipped)
}

type packMode int

const (
	packAuto packMode = iota
	packCompress
	packExtract
)

type packOptions struct {
	mode    packMode
	list    bool
	useZstd bool
	output  string
	remove  bool
	// force는 이미 있는 출력 파일을 바꾼다. 풀어 놓은 디렉터리는 지우지 않으므로 바꾸지 않는다.
	force bool
	// exclude와 gitignore는 디렉터리를 묶을 때 뺄 항목을 고른다.
	exclude   packExcludes
	gitignore bool
	level     int
	jobs      int
	stdout    io.Writer
	stderr    io.Writer
	// report는 -v의 출력이다. -v가 없으면 아무것도 쓰지 않는다.
	report *packReporter
}

// packKind는 입력을 앞부분 바이트로 본 결과다. format은 gzip, zstd, xz, bzip2, zip 가운데 하나이고, tar는 그것을 푼 내용
// (압축하지 않은 tar면 그 자체)이 tar라는 뜻이다. zip은 항목을 여럿 담으므로 tar처럼 디렉터리로 푼다. foreign은 edc가 풀지
// 못하는 압축 형식(7z, rar, lz4)이다.
type packKind struct {
	format  string
	tar     bool
	foreign string
}

// many는 디렉터리로 푸는 입력이다.
func (kind packKind) many() bool { return kind.tar || kind.format == packFormatZip }

func (kind packKind) archive() bool { return kind.format != "" || kind.tar }

func runPack(args []string) int {
	// Ctrl-C에 바로 죽으면 반쯤 쓴 임시 출력이 남는다. 취소를 받아 지우고 끝낸다.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return packRun(ctx, args, os.Stdout, os.Stderr)
}

func packRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	set := newFlagSet("pack")
	set.SetOutput(stderr)
	options := packOptions{stdout: stdout, stderr: stderr, jobs: runtime.GOMAXPROCS(0)}
	compress := set.Bool("z", false, T("command.pack.option.compress"))
	extract := set.Bool("x", false, T("command.pack.option.extract"))
	set.BoolVar(&options.list, "l", false, T("command.pack.option.list"))
	set.BoolVar(&options.useZstd, "zstd", false, T("command.pack.option.zstd"))
	// -l은 목록이라 edc compress의 -l N을 쓰지 못한다.
	level := set.Int("level", 0, T("command.pack.option.level"))
	set.StringVar(&options.output, "o", "", T("command.pack.option.output"))
	set.BoolVar(&options.remove, "rm", false, T("command.pack.option.remove"))
	set.BoolVar(&options.force, "force", false, T("command.pack.option.force"))
	set.Var(&options.exclude, "exclude", T("command.pack.option.exclude"))
	set.BoolVar(&options.gitignore, "gitignore", false, T("command.pack.option.gitignore"))
	report := &packReporter{out: stderr}
	set.BoolVar(&report.verbose, "v", false, T("command.pack.option.verbose"))
	set.BoolVar(&report.verbose, "verbose", false, T("command.pack.option.verbose"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	// 진행 줄은 고쳐 쓰는 한 줄이라 터미널에서만 그린다. pipe나 파일로 남기는 로그에는 판단 줄과 항목 줄만 쓴다.
	if file, ok := stderr.(*os.File); ok && isTerminal(file) {
		report.live, report.color = true, richEnabled(file)
	}
	options.report = report
	paths := set.Args()
	// -o와 여러 경로는 묶음 하나를 만든다. 묶음은 늘 압축하는 쪽이라 -x와 맞지 않고, 여러 입력을 한꺼번에 지우는 --rm은
	// 받지 않는다.
	bundle := options.output != "" && len(paths) > 1
	switch {
	case *compress && *extract, options.list && (*compress || options.output != "" || options.remove || options.force),
		*level != 0 && (*extract || options.list),
		(len(options.exclude) > 0 || options.gitignore) && (*extract || options.list),
		len(paths) == 0, bundle && (*extract || options.remove):
		fmt.Fprintln(stderr, T("cli.usage", packUsage))
		return 2
	case *compress:
		options.mode = packCompress
	case *extract:
		options.mode = packExtract
	}
	options.level = compressGzipLevel
	if options.useZstd {
		options.level = compressZstdLevel
	}
	if *level != 0 {
		if *level < 1 || *level > 9 {
			fmt.Fprintln(stderr, T("pack.error.level", *level))
			return 2
		}
		options.level = *level
	}
	if bundle {
		return packRunBundle(ctx, paths, options)
	}
	failed := false
	compressed := 0
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		line, packed, err := packPath(ctx, path, options)
		if packed {
			compressed++
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintln(stderr, T("compress.failed", "pack", viewSanitize(path), packErrorText(err)))
			failed = true
			continue
		}
		if line != "" {
			fmt.Fprintln(stderr, line)
		}
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, T("pack.cancelled"))
		return 4
	}
	// 셸이 *를 펼치면 경로마다 따로 압축된다. 묶음 하나를 바랐을 사용자에게 -o로 묶는 방법을 알린다.
	if len(paths) > 1 && compressed > 0 {
		hint := "edc pack -o NAME.tar.gz <paths>"
		if options.useZstd {
			hint = "edc pack --zstd -o NAME.tar.zst <paths>"
		}
		fmt.Fprintln(stderr, T("pack.hint_bundle", hint))
	}
	if failed {
		return 1
	}
	return 0
}

func packRunBundle(ctx context.Context, paths []string, options packOptions) int {
	line, err := packBundle(ctx, paths, options)
	if ctx.Err() != nil {
		fmt.Fprintln(options.stderr, T("pack.cancelled"))
		return 4
	}
	if err != nil {
		name := options.output
		// 경로 때문에 실패했으면 그 경로를 이름 칸에 적고, 오류 문구에는 경로를 다시 붙이지 않는다.
		var input packBundleError
		if errors.As(err, &input) {
			name, err = input.path, input.err
		}
		fmt.Fprintln(options.stderr, T("compress.failed", "pack", viewSanitize(name), packErrorText(err)))
		return 1
	}
	fmt.Fprintln(options.stderr, line)
	return 0
}

func packErrorText(err error) string {
	var kept packKeptError
	if errors.As(err, &kept) {
		return T("pack.error.kept", kept.skipped)
	}
	var same packSameNameError
	if errors.As(err, &same) {
		return T("pack.error.same_name", viewSanitize(same.first), viewSanitize(same.second), viewSanitize(same.name))
	}
	var foreign packForeignError
	if errors.As(err, &foreign) {
		return T("pack.error.foreign", foreign.format)
	}
	var method packZipMethodError
	if errors.As(err, &method) {
		return T("pack.error.zip_method", viewSanitize(method.name))
	}
	if errors.Is(err, errPackZipEncrypted) {
		name, _ := strings.CutSuffix(err.Error(), ": "+errPackZipEncrypted.Error())
		return T("pack.error.zip_encrypted", viewSanitize(name))
	}
	var unsafe packUnsafeError
	if errors.As(err, &unsafe) {
		if unsafe.link {
			return T("pack.error.unsafe_link", viewSanitize(unsafe.name))
		}
		return T("pack.error.unsafe_path", viewSanitize(unsafe.name))
	}
	switch {
	case errors.Is(err, errPackUnknown), errors.Is(err, errCodecUnknown):
		return T("pack.error.unknown")
	case errors.Is(err, errPackZipName):
		return T("pack.error.zip_name")
	case errors.Is(err, errPackDirectory):
		return T("pack.error.directory_extract")
	case errors.Is(err, errPackRemoveDir):
		return T("pack.error.remove_directory")
	case errors.Is(err, errPackInside):
		return T("pack.error.inside")
	case errors.Is(err, errPackRoot):
		return T("pack.error.root")
	case errors.Is(err, errPackListDir):
		return T("pack.error.list_directory")
	case errors.Is(err, errPackNoReplace):
		return T("pack.error.no_replace")
	case errors.Is(err, errCodecExists):
		// codecPath와 packPlace는 겹친 이름을 "이름: output exists"로 감싼다. 압축 파일 안의 최상위 이름은 압축 파일 이름과 다를
		// 수 있어 무엇이 겹쳤는지 알려야 한다.
		if name, ok := strings.CutSuffix(err.Error(), ": "+errCodecExists.Error()); ok {
			var directory packDirExistsError
			if errors.As(err, &directory) {
				return T("pack.error.exists_named", viewSanitize(name))
			}
			return packExistsText(name)
		}
		return T("pack.error.exists")
	}
	return viewSanitize(codecErrorText(err))
}

// packExistsText는 출력 파일 이름이 겹친 경우의 문구다. 비어 있는 다른 이름을 찾아 -o에 바로 쓸 수 있게 알린다. 그 이름이
// 디렉터리면 --force가 바꾸지 않으므로 권하지 않는다.
func packExistsText(name string) string {
	if info, err := os.Lstat(name); err == nil && info.IsDir() {
		return T("pack.error.exists_named", viewSanitize(name))
	}
	free := packFreeName(name)
	if free == "" {
		return T("pack.error.exists_named", viewSanitize(name))
	}
	return T("pack.error.exists_file", viewSanitize(name), viewSanitize(free))
}

// packNameSuffixes는 다른 이름을 지을 때 그대로 두는 끝부분이다. 긴 것부터 본다.
var packNameSuffixes = []string{".tar.gz", ".tar.zst", ".tar.xz", ".tar.bz2", ".tgz", ".tzst", ".txz", ".tbz2", ".gz", ".zst", ".xz", ".bz2", ".out"}

// packFreeName은 name과 같은 디렉터리에서 아직 없는 이름이다. 압축 확장자나 마지막 확장자 앞에 -1, -2를 붙인다(d.tar.gz →
// d-1.tar.gz, app.log → app-1.log). 99까지 찾지 못하면 빈 문자열이다.
func packFreeName(name string) string {
	dir, base := filepath.Split(name)
	stem, suffix := packSplitSuffix(base)
	for number := 1; number < 100; number++ {
		candidate := fmt.Sprintf("%s%s-%d%s", dir, stem, number, suffix)
		if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
	}
	return ""
}

// packSplitSuffix는 base를 이름과 끝부분으로 나눈다. ".bashrc"처럼 점으로만 시작하는 이름은 나누지 않는다.
func packSplitSuffix(base string) (string, string) {
	for _, known := range packNameSuffixes {
		if stem, ok := strings.CutSuffix(base, known); ok && stem != "" {
			return stem, known
		}
	}
	if ext := filepath.Ext(base); ext != "" && ext != base {
		return strings.TrimSuffix(base, ext), ext
	}
	return base, ""
}

// packPath는 입력 하나를 처리하고 결과 줄과 압축했는지를 돌려준다. 이름이 아니라 내용으로 형식을 본다. 자동이면 압축 파일과 tar는 풀고
// 나머지는 압축한다. -z는 무엇이든 압축하고, -x는 무엇이든 풀되 압축 형식이 아니면 실패한다.
func packPath(ctx context.Context, path string, options packOptions) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		switch {
		case options.list:
			return "", false, errPackListDir
		case options.mode == packExtract:
			return "", false, errPackDirectory
		case options.remove:
			return "", false, errPackRemoveDir
		}
		options.report.line(T("pack.verbose.decision", viewSanitize(path), T("pack.verbose.kind_directory"), T("pack.verbose.action_pack")))
		line, err := packDirectory(ctx, path, options)
		return line, err == nil, err
	}
	if !info.Mode().IsRegular() {
		return "", false, errCodecNotRegular
	}
	kind, err := packDetect(path)
	if err != nil {
		return "", false, err
	}
	// 자동으로는 이름이 .zip이 아닌 zip(Office 문서 등)과 풀지 못하는 압축 형식을 풀지도, 다시 압축하지도 않는다.
	if options.mode == packAuto && !options.list {
		switch {
		case kind.format == packFormatZip && !packZipNamed(path):
			return "", false, errPackZipName
		case kind.foreign != "":
			return "", false, packForeignError{format: kind.foreign}
		}
	}
	action := "pack.verbose.action_extract"
	switch {
	case options.list:
		action = "pack.verbose.action_list"
	case options.mode == packCompress || options.mode == packAuto && !kind.archive():
		action = "pack.verbose.action_compress"
	}
	decided := T(action)
	if forced := map[packMode]string{packCompress: " (-z)", packExtract: " (-x)"}[options.mode]; forced != "" {
		decided += forced
	}
	options.report.line(T("pack.verbose.decision", viewSanitize(path), packKindText(kind), decided))
	switch {
	case options.list:
		return "", false, packList(path, kind, options.stdout)
	case options.mode == packCompress || options.mode == packAuto && !kind.archive():
		line, err := packCompressFile(ctx, path, info.Size(), options)
		return line, err == nil, err
	case !kind.archive():
		return "", false, errPackUnknown
	case kind.many():
		line, err := packExtractTar(ctx, path, kind, options)
		return line, false, err
	}
	line, err := packDecompressFile(ctx, path, info.Size(), kind.format, options)
	return line, false, err
}

// packDetect는 파일 앞부분으로 형식을 본다. 한 줄로 읽는 압축 형식이면 앞 블록을 풀어 tar인지도 본다. tar의 첫 블록 257번째
// 바이트에는 "ustar"가 있다. 그 표시가 없는 아주 오래된 v7 tar는 파일 하나로 본다.
func packDetect(path string) (packKind, error) {
	input, _, err := codecOpenInput(path)
	if err != nil {
		return packKind{}, err
	}
	defer input.Close()
	magic := make([]byte, packMagicSize)
	n, err := io.ReadFull(input, magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return packKind{}, err
	}
	kind := packKind{format: packFormat(magic[:n]), foreign: packForeignFormat(magic[:n])}
	if kind.format == packFormatZip {
		return kind, nil
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return packKind{}, err
	}
	var block io.Reader = input
	if kind.format != "" {
		reader, err := packOpenStream(input, kind.format)
		if err != nil {
			return packKind{}, err
		}
		defer reader.Close()
		block = reader
	}
	head := make([]byte, 512)
	// 512바이트보다 짧게 풀리면 tar가 아니라 파일 하나다. 푸는 오류는 실제로 풀 때 다시 나므로 여기서는 묻지 않는다.
	if n, _ := io.ReadFull(block, head); n == 512 {
		kind.tar = packTarBlock(head) || packEmptyTar(head, block)
	}
	return kind, nil
}

// packTarBlock은 tar의 첫 헤더인지 본다. ustar 표시가 있거나, 표시가 없는 v7 tar면 헤더 체크섬이 맞아야 한다. 체크섬은
// 체크섬 칸(148~155)을 공백으로 보고 512바이트를 더한 값이고 8진수로 적힌다.
func packTarBlock(block []byte) bool {
	if len(block) < 512 {
		return false
	}
	if bytes.HasPrefix(block[257:], []byte("ustar")) {
		return true
	}
	if block[0] == 0 {
		return false
	}
	field := strings.TrimRight(strings.TrimSpace(string(bytes.TrimRight(block[148:156], "\x00 "))), "\x00")
	stored, err := strconv.ParseInt(field, 8, 64)
	if err != nil {
		return false
	}
	// 옛 tar 가운데는 바이트를 부호 있는 수로 더한 것이 있다. archive/tar도 두 합을 모두 받으므로 여기서도 둘 다 받는다.
	var unsigned, signed int64
	for index, value := range block[:512] {
		if index >= 148 && index < 156 {
			value = ' '
		}
		unsigned += int64(value)
		signed += int64(int8(value))
	}
	return stored == unsigned || stored == signed
}

// packEmptyTarLimit는 빈 tar로 볼 수 있는 가장 긴 길이다. tar는 끝에 0 블록 두 개를 쓰고 10240바이트 단위로 채운다.
const packEmptyTarLimit = 10240

// packEmptyTar는 항목 없이 끝 표시만 있는 tar인지 본다. 앞 블록이 0이면 나머지도 짧은 0으로 끝나야 빈 tar다. 0으로만 찬 큰
// 파일(지운 디스크 이미지 등)을 빈 tar로 보고 풀지 않도록 길이를 묶는다.
func packEmptyTar(head []byte, rest io.Reader) bool {
	if !bytes.Equal(head, make([]byte, len(head))) {
		return false
	}
	tail, err := io.ReadAll(io.LimitReader(rest, packEmptyTarLimit))
	if err != nil || len(head)+len(tail) < 1024 || len(head)+len(tail) > packEmptyTarLimit {
		return false
	}
	if extra, _ := rest.Read(make([]byte, 1)); extra > 0 {
		return false
	}
	return bytes.Equal(tail, make([]byte, len(tail)))
}

// packCompressFile은 파일 하나를 .gz나 .zst로 압축한다. edc compress와 같은 경로로 쓰므로 임시 파일, fsync, 덮어쓰지 않기,
// 수정 시각과 권한 유지, --rm의 확인이 같다. -z면 이미 압축한 파일도 한 번 더 압축한다.
func packCompressFile(ctx context.Context, path string, size int64, options packOptions) (string, error) {
	suffix := ".gz"
	if options.useZstd {
		suffix = ".zst"
	}
	codec := codecOptions{command: "compress", output: options.output, remove: options.remove, replace: options.force, stderr: options.stderr}
	codec.convert = func(dst io.Writer, src io.Reader) error {
		var read atomic.Int64
		defer options.report.track(viewSanitize(path), size, &read, nil)()
		src = packContextReader{ctx: ctx, reader: packCountingReader{reader: src, count: &read}}
		if options.useZstd {
			return compressZstd(dst, src, options.level)
		}
		return compressGzip(dst, src, options.level, options.jobs)
	}
	codec.target = func(path string) (string, error) { return path + suffix, nil }
	// 자동으로 고른 압축이 줄지 않았으면 이미 압축된 데이터로 보고 --rm이어도 원본을 남긴다. -z는 사용자가 고른 방향이라 지운다.
	codec.keep = func(read, written int64) bool { return options.mode == packAuto && packNotSmaller(read, written) }
	result, err := codecPath(path, codec, false)
	if err != nil {
		return "", err
	}
	line := packSanitizedLine(result)
	switch {
	case result.kept:
		line += T("pack.not_smaller_kept", viewSanitize(path))
	case packNotSmaller(result.read, result.written):
		line += T("pack.not_smaller")
	}
	return line, nil
}

// packNotSmaller는 압축한 결과가 원본보다 작지 않은지 본다. 빈 파일은 비교하지 않는다.
func packNotSmaller(read, written int64) bool { return read > 0 && written >= read }

// packDecompressFile은 tar가 아닌 압축 파일 하나를 푼다. 이름이 압축 확장자로 끝나면 떼고, 아니면 .out을 붙인다.
func packDecompressFile(ctx context.Context, path string, size int64, format string, options packOptions) (string, error) {
	codec := codecOptions{command: "decompress", output: options.output, remove: options.remove, replace: options.force, stderr: options.stderr}
	codec.convert = func(dst io.Writer, src io.Reader) error {
		var read atomic.Int64
		defer options.report.track(viewSanitize(path), size, &read, nil)()
		reader, err := packOpenStream(packContextReader{ctx: ctx, reader: packCountingReader{reader: src, count: &read}}, format)
		if err != nil {
			return err
		}
		defer reader.Close()
		_, err = io.CopyBuffer(dst, reader, make([]byte, codecBufferSize))
		return err
	}
	codec.target = func(path string) (string, error) { return packPlainName(path), nil }
	result, err := codecPath(path, codec, false)
	if err != nil {
		return "", err
	}
	return packSanitizedLine(result), nil
}

func packSanitizedLine(result codecResult) string {
	result.input, result.output = viewSanitize(result.input), viewSanitize(result.output)
	return result.line()
}

// packPlainName은 압축을 푼 파일의 이름이다. 압축 확장자가 없으면 원래 이름 그대로 쓸 수 없으므로 .out을 붙인다.
func packPlainName(path string) string {
	for _, suffix := range []string{".gz", ".zst", ".xz", ".bz2"} {
		if name, ok := strings.CutSuffix(path, suffix); ok && packNamed(name) {
			return name
		}
	}
	return path + ".out"
}

// packArchiveName은 최상위가 여럿인 tar를 풀 디렉터리의 이름이다.
func packArchiveName(path string) string {
	for _, suffix := range []string{".tar.gz", ".tar.zst", ".tar.xz", ".tar.bz2", ".tgz", ".tzst", ".txz", ".tbz2", ".tbz", ".tar"} {
		if name, ok := strings.CutSuffix(path, suffix); ok && packNamed(name) {
			return name
		}
	}
	if packZipNamed(path) && packNamed(path[:len(path)-4]) {
		return path[:len(path)-4]
	}
	return path + ".out"
}

// packNamed는 확장자를 뗀 이름이 쓸 수 있는 이름인지다. ".gz"나 "dir/.gz"를 떼면 빈 이름이 남는다.
func packNamed(name string) bool {
	return name != "" && !strings.HasSuffix(name, "/")
}

// packContextReader는 읽을 때마다 취소를 본다. 큰 파일 하나를 압축하는 도중에도 Ctrl-C가 바로 멈추게 한다.
type packContextReader struct {
	ctx    context.Context
	reader io.Reader
}

// packReadHook은 시험만 쓴다. 읽을 때마다 불려, 임시 출력을 만든 뒤에 취소되는 경우를 만든다.
var packReadHook func()

func (reader packContextReader) Read(p []byte) (int, error) {
	if packReadHook != nil {
		packReadHook()
	}
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(p)
}

// packTarHeaderName은 목록과 결과 줄에 쓰는 tar 항목 이름이다. 항목 이름은 압축 파일을 만든 쪽이 정하므로 터미널을 움직이는
// 문자를 지운다.
func packTarHeaderName(header *tar.Header) string {
	name := viewSanitize(header.Name)
	if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
		name += " -> " + viewSanitize(header.Linkname)
	}
	return name
}
