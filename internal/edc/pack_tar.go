package edc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// packDirectory는 디렉터리를 tar로 묶어 .tar.gz나 .tar.zst로 쓴다. 항목은 WalkDir 순서(이름순)라 같은 디렉터리는 같은 순서로
// 묶인다. 안의 파일은 codecOpenChecked로 열어, 걷는 사이 symlink나 FIFO로 바뀐 것을 따라가지 않는다. 읽지 못하는 파일이 있으면
// 빠진 채로 묶지 않고 실패한다. 소켓, 장치, FIFO는 tar에 담아도 다른 host에서 쓸 수 없어 건너뛰고 개수를 알린다.
func packDirectory(ctx context.Context, dir string, options packOptions) (string, error) {
	started := time.Now()
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	base := filepath.Base(absolute)
	if absolute == string(filepath.Separator) {
		return "", errPackRoot
	}
	suffix := ".tar.gz"
	if options.useZstd {
		suffix = ".tar.zst"
	}
	target := options.output
	if target == "" {
		// "."이나 ".."에 확장자를 붙이면 숨김 파일이 되므로 그때만 실제 디렉터리 이름을 쓴다.
		target = filepath.Clean(dir) + suffix
		if clean := filepath.Base(filepath.Clean(dir)); clean == "." || clean == ".." {
			target = filepath.Join(filepath.Dir(absolute), base+suffix)
		}
	}
	// 출력이 묶는 디렉터리 안이면 쓰는 중인 자기 임시 파일까지 묶는다. -o의 부모가 symlink로 그 디렉터리를 가리킬 수 있어
	// 양쪽 모두 링크를 푼 실제 경로로 비교한다.
	if inside, err := packInside(absolute, target); err != nil {
		return "", err
	} else if inside {
		return "", errPackInside
	}
	label := viewSanitize(strings.TrimSuffix(dir, "/")) + "/"
	return packWriteArchive(ctx, []packRoot{{path: dir, base: base}}, target, label, started, options)
}

// packRoot는 tar의 최상위 항목 하나다. path를 걸어 base 아래 이름으로 담는다.
type packRoot struct {
	path, base string
}

// packBundle은 -o와 함께 받은 여러 경로를 tar 하나로 묶는다. 각 경로의 base 이름이 최상위가 되고, 인자 순서대로 담는다.
// 압축 파일도 풀지 않고 그대로 담는다. 임시 출력을 만들기 전에 모든 경로를 확인해, 하나라도 맞지 않으면 아무것도 쓰지 않는다.
func packBundle(ctx context.Context, paths []string, options packOptions) (string, error) {
	started := time.Now()
	roots := make([]packRoot, 0, len(paths))
	seen := map[string]string{}
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		info, err := os.Lstat(name)
		if err != nil {
			return "", packBundleError{path: name, err: err}
		}
		absolute, err := filepath.Abs(name)
		if err != nil {
			return "", packBundleError{path: name, err: err}
		}
		if absolute == string(filepath.Separator) {
			return "", packBundleError{path: name, err: errPackRoot}
		}
		base := filepath.Base(absolute)
		// 같은 최상위 이름이 둘이면 풀 때 뒤의 것이 앞의 것을 덮거나 두 디렉터리가 섞인다.
		if first, ok := seen[base]; ok {
			return "", packSameNameError{name: base, first: first, second: name}
		}
		seen[base] = name
		if info.IsDir() {
			if inside, err := packInside(absolute, options.output); err != nil {
				return "", packBundleError{path: name, err: err}
			} else if inside {
				return "", packBundleError{path: name, err: errPackInside}
			}
		}
		roots = append(roots, packRoot{path: name, base: base})
	}
	label := T("pack.paths", len(paths))
	options.report.line(T("pack.verbose.decision", label, T("pack.verbose.kind_bundle"), T("pack.verbose.action_pack")))
	return packWriteArchive(ctx, roots, options.output, label, started, options)
}

// packBundleError는 묶을 경로 가운데 하나 때문에 실패한 경우다. 실패 줄에 출력 대신 그 경로를 적는다.
type packBundleError struct {
	path string
	err  error
}

func (err packBundleError) Error() string { return err.path + ": " + err.err.Error() }

func (err packBundleError) Unwrap() error { return err.err }

// packSameNameError는 묶을 두 경로의 base 이름이 같은 경우다.
type packSameNameError struct {
	name, first, second string
}

func (err packSameNameError) Error() string {
	return fmt.Sprintf("%s and %s have the same name in the archive: %s", err.first, err.second, err.name)
}

// packWriteArchive는 roots를 target에 .tar.gz나 .tar.zst로 쓴다. 옆에 만든 임시 파일에 다 쓰고 fsync한 뒤 링크로 옮기므로,
// 실패하거나 취소되면 반쯤 쓴 출력이 남지 않고 그사이 생긴 같은 이름의 파일도 덮지 않는다.
func packWriteArchive(ctx context.Context, roots []packRoot, target, label string, started time.Time, options packOptions) (string, error) {
	if existing, err := os.Lstat(target); err == nil {
		if !options.force || existing.IsDir() {
			return "", fmt.Errorf("%s: %w", target, errCodecExists)
		}
		// 묶을 파일 가운데 하나를 출력으로 바꾸면 그 파일의 원래 내용을 잃는다.
		for _, root := range roots {
			if input, err := os.Lstat(root.path); err == nil && os.SameFile(input, existing) {
				return "", fmt.Errorf("%s: %w", target, errCodecSameFile)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	out, temp, err := codecCreateTemp(target, 0o644)
	if err != nil {
		return "", err
	}
	written := &codecWriteCounter{writer: out}
	var tally packTally
	stop := options.report.track(label, 0, &tally.read, &tally.files)
	files, read, skipped, err := packWriteTar(ctx, roots, written, options, &tally)
	stop()
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && options.force {
		err = os.Rename(temp, target)
	} else if err == nil {
		if err = os.Link(temp, target); errors.Is(err, fs.ErrExist) {
			err = fmt.Errorf("%s: %w", target, errCodecExists)
		}
	}
	os.Remove(temp)
	if err != nil {
		return "", err
	}
	line := T("pack.packed", label, viewSanitize(target), files, formatSizeShort(uint64(read)), formatSizeShort(uint64(written.count)), packRatio(written.count, read), fmt.Sprintf("%.1fs", time.Since(started).Seconds()))
	if skipped > 0 {
		line += T("pack.skipped", skipped)
	}
	if tally.excluded > 0 {
		line += T("pack.excluded", tally.excluded)
	}
	return line, nil
}

func packRatio(packed, plain int64) string {
	if plain <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(packed)*100/float64(plain))
}

// packWriteTar는 roots를 차례로 걸어 각자의 base 아래 tar 항목으로 dst에 압축해 쓴다. 최상위가 파일이나 symlink여도 같은
// 규칙(symlink는 링크로, 특수 파일은 건너뜀)을 따른다. 파일 수, 읽은 파일 바이트, 건너뛴 특수 파일 수를 돌려준다.
// packTally는 -v의 진행 줄이 읽는 수다. 진행 줄은 다른 goroutine이 그리므로 원자적으로 센다.
type packTally struct {
	read, files atomic.Int64
	// excluded는 --exclude와 --gitignore로 뺀 항목 수다. 걷는 goroutine만 쓴다.
	excluded int
}

func packWriteTar(ctx context.Context, roots []packRoot, dst io.Writer, options packOptions, tally *packTally) (files int, read int64, skipped int, err error) {
	var compressor io.WriteCloser
	if options.useZstd {
		compressor = newZstdWriter(dst, options.level)
	} else if compressor, err = newGzipWriter(dst, options.level, options.jobs); err != nil {
		return 0, 0, 0, err
	}
	archive := tar.NewWriter(compressor)
	buffer := make([]byte, codecBufferSize)
	for _, root := range roots {
		if err = packWalkRoot(ctx, root, archive, buffer, options, tally, &files, &read, &skipped); err != nil {
			break
		}
	}
	if err == nil {
		err = archive.Close()
	}
	if closeErr := compressor.Close(); err == nil {
		err = closeErr
	}
	return files, read, skipped, err
}

// packWalkRoot는 최상위 하나를 이름순으로 걸어 archive에 담는다. filepath.WalkDir는 최상위도 Lstat으로 보므로, 최상위
// symlink는 따라가지 않고 링크로 담는다.
func packWalkRoot(ctx context.Context, root packRoot, archive *tar.Writer, buffer []byte, options packOptions, tally *packTally, files *int, read *int64, skipped *int) error {
	dir, base := root.path, root.base
	filter, err := newPackFilter(dir, options)
	if err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		entryName := path.Join(base, filepath.ToSlash(relative))
		var parts []string
		if relative != "." {
			parts = strings.Split(filepath.ToSlash(relative), "/")
		}
		if filter.skip(parts, entry.IsDir()) {
			tally.excluded++
			if entry.IsDir() {
				options.report.line("- " + viewSanitize(entryName) + "/ (" + T("pack.verbose.excluded") + ")")
				return filepath.SkipDir
			}
			options.report.line("- " + viewSanitize(entryName) + " (" + T("pack.verbose.excluded") + ")")
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := filter.enter(name, parts); err != nil {
				return err
			}
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = entryName + "/"
			options.report.line("+ " + viewSanitize(header.Name))
			return archive.WriteHeader(header)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			header, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			header.Name = entryName
			*files++
			tally.files.Add(1)
			options.report.line("+ " + viewSanitize(entryName) + " -> " + viewSanitize(link))
			return archive.WriteHeader(header)
		case !info.Mode().IsRegular():
			*skipped++
			options.report.line("- " + viewSanitize(entryName) + " (" + packSpecialText(info.Mode()) + ")")
			return nil
		}
		file, opened, err := codecOpenChecked(name, info)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		defer file.Close()
		header, err := tar.FileInfoHeader(opened, "")
		if err != nil {
			return err
		}
		header.Name = entryName
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		// 헤더에 적은 크기만큼만 쓴다. 묶는 사이 파일이 줄면 tar가 깨지므로 실패하고, 늘어난 뒤쪽은 담지 않는다.
		copied, err := io.CopyBuffer(archive, packContextReader{ctx: ctx, reader: packCountingReader{reader: io.LimitReader(file, header.Size), count: &tally.read}}, buffer)
		if err == nil && copied != header.Size {
			err = fmt.Errorf("%s: %w", name, errCodecChanged)
		}
		*files++
		tally.files.Add(1)
		*read += copied
		if err == nil {
			options.report.line("+ " + viewSanitize(entryName) + "  " + formatSizeShort(uint64(copied)))
		}
		return err
	})
}

// packExtractTar는 tar(압축했든 안 했든)를 푼다. 출력 옆에 만든 임시 디렉터리에 os.Root로 풀어서, symlink를 거쳐도 그 밖에는
// 쓰지 못한다. 다 풀면 최상위가 디렉터리 하나일 때 그 이름으로, 아니면 압축 파일 이름(확장자를 뗀)으로 옮긴다. -o면 그 디렉터리
// 안에 푼다. 실패하거나 취소되면 임시 디렉터리를 지우므로 반쯤 푼 결과가 남지 않는다.
func packExtractTar(ctx context.Context, archivePath string, kind packKind, options packOptions) (string, error) {
	started := time.Now()
	input, info, err := codecOpenInput(archivePath)
	if err != nil {
		return "", err
	}
	defer input.Close()
	var digest maphash.Hash
	var src io.Reader = input
	if options.remove {
		src = io.TeeReader(input, &digest)
	}
	var progress atomic.Int64
	counted := &codecCounter{reader: packCountingReader{reader: src, count: &progress}}
	stop := options.report.track(viewSanitize(archivePath), info.Size(), &progress, nil)
	defer stop()
	var stream io.Reader = packContextReader{ctx: ctx, reader: counted}
	var zipped *packZipStream
	switch kind.format {
	case "":
	case packFormatZip:
		// zip은 offset으로 읽어 TeeReader를 거치지 않는다. --rm이 지우기 전에 비교할 hash는 푸는 데 쓰는 바이트보다 먼저 만든다.
		// 다 푼 뒤에 만들면 그사이 제자리에서 바뀐 zip이 기준이 되어, 푼 결과와 다른 zip을 지운다.
		if options.remove {
			// 끝의 목록만 읽어 zip이 아닌 큰 입력을 다 읽기 전에 실패한다.
			if _, err := zip.NewReader(io.NewSectionReader(input, 0, info.Size()), info.Size()); err != nil && !errors.Is(err, zip.ErrInsecurePath) {
				return "", err
			}
			if _, err := io.Copy(io.Discard, packContextReader{ctx: ctx, reader: counted}); err != nil {
				return "", err
			}
		}
		zipped = newPackZipStream(ctx, input, info.Size(), &progress)
		defer zipped.Close()
		stream = packContextReader{ctx: ctx, reader: zipped}
	default:
		reader, err := packOpenStream(stream, kind.format)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		stream = reader
	}
	parent := filepath.Dir(archivePath)
	if options.output != "" {
		parent = filepath.Dir(filepath.Clean(options.output))
	}
	temp, err := os.MkdirTemp(parent, "."+filepath.Base(archivePath)+".*.unpack")
	if err != nil {
		return "", err
	}
	finished := false
	defer func() {
		if !finished {
			packRemoveTemp(temp)
		}
	}()
	root, err := os.OpenRoot(temp)
	if err != nil {
		return "", err
	}
	counts, err := packUntar(ctx, root, stream, options.remove, options.report)
	if closeErr := root.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	// tar의 끝 표시 뒤에도 압축 스트림을 끝까지 읽어야 gzip과 zstd의 체크섬을 확인한다. --rm도 입력을 다 읽었는지 본다.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return "", err
	}
	if zipped != nil {
		counts.skipped += int(zipped.skipped.Load())
		if packZipExtractedHook != nil {
			packZipExtractedHook()
		}
	}
	if zipped == nil {
		if _, err := io.Copy(io.Discard, packContextReader{ctx: ctx, reader: counted}); err != nil {
			return "", err
		}
	}
	final, err := packPlace(temp, archivePath, options.output, counts.crossTop)
	if errors.Is(err, errCodecExists) {
		return "", packDirExistsError{err: err}
	}
	if err != nil {
		return "", err
	}
	finished = true
	if options.remove && counts.skipped > 0 {
		// 건너뛴 항목은 푼 결과에 없다. 압축 파일까지 지우면 그 항목을 되찾을 곳이 없으므로 남긴다.
		return "", packKeptError{skipped: counts.skipped}
	}
	if options.remove {
		if err := codecSyncDir(filepath.Dir(final)); err != nil {
			return "", err
		}
		if err := codecRemoveInput(archivePath, input, info, counted.count, &digest); err != nil {
			return "", err
		}
	}
	packed := counted.count
	// zip은 offset으로 읽어 counted를 거치지 않으므로 파일 크기를 쓴다.
	if zipped != nil {
		packed = info.Size()
	}
	line := T("pack.unpacked", viewSanitize(archivePath), viewSanitize(final)+"/", counts.files, counts.dirs, formatSizeShort(uint64(packed)), formatSizeShort(uint64(counts.bytes)), fmt.Sprintf("%.1fs", time.Since(started).Seconds()))
	if counts.skipped > 0 {
		line += T("pack.skipped", counts.skipped)
	}
	return line, nil
}

// packZipExtractedHook은 시험만 쓴다. zip을 다 푼 뒤 옮기기 전에 불려, 푼 뒤에 zip이 제자리에서 바뀌는 경우를 만든다.
var packZipExtractedHook func()

// packDirExistsError는 풀어 놓을 디렉터리 이름이 이미 있는 경우다. --force는 디렉터리를 바꾸지 않고, 최상위가 하나인 tar에
// -o를 주면 그 안에 한 단계 더 들어가므로 다른 이름을 권하지 않는다.
type packDirExistsError struct{ err error }

func (err packDirExistsError) Error() string { return err.err.Error() }

func (err packDirExistsError) Unwrap() error { return err.err }

type packUntarCounts struct {
	files, dirs, skipped int
	bytes                int64
	// crossTop은 어떤 링크가 자기 최상위 디렉터리 밖(다른 최상위나 root 자체)을 가리킨다는 뜻이다. 그러면 최상위 하나만 꺼내
	// 옮길 수 없다. 옮긴 뒤에는 그 링크가 풀어 놓은 디렉터리의 이웃을 가리킨다.
	crossTop bool
}

// packUntar는 stream의 tar 항목을 root 안에 쓴다. 항목 이름은 root 기준 상대 경로여야 한다. os.Root가 쓰기마다 경로를
// root 안으로 묶지만, 밖을 가리키는 링크를 남기면 나중에 그 디렉터리를 쓰는 사람이 따라간다. 그래서 symlink는 맨 마지막에
// 만들고, 대상이 root 안이며 다른 symlink를 거쳐 가지 않을 때만 만든다. "d/a -> .."와 "b -> d/a/.."처럼 글자로는 안쪽이지만
// 링크를 따라가면 밖으로 나가는 연쇄를 이것으로 막는다. 앞서 symlink로 적힌 이름을 거쳐 가는 항목도 거부한다.
// 디렉터리의 권한과 시각은 안의 항목을 다 쓴 뒤 깊은 것부터 정한다. 0555 디렉터리를 먼저 만들면 그 안에 쓸 수 없다.
func packUntar(ctx context.Context, root *os.Root, stream io.Reader, durable bool, report *packReporter) (packUntarCounts, error) {
	var counts packUntarCounts
	// dirs는 디렉터리 항목의 헤더다. 같은 이름이 다시 나오면 뒤의 헤더가 이기고, 디렉터리가 아닌 항목이 그 이름을 대체하면
	// 지운다. 남겨 두면 마지막에 새 파일이나 symlink(가 가리키는 항목)에 옛 디렉터리의 권한과 시각을 덮어쓴다.
	dirs := map[string]*tar.Header{}
	var links []*tar.Header
	symlinks := map[string]bool{}
	owner := os.Geteuid() == 0
	reader := tar.NewReader(stream)
	buffer := make([]byte, codecBufferSize)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return counts, err
		}
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		name, ok := packEntryName(header.Name)
		if !ok {
			return counts, packUnsafeError{name: header.Name}
		}
		if name == "." {
			continue
		}
		if packThroughLink(name, symlinks) {
			return counts, packUnsafeError{name: header.Name, link: true}
		}
		// 같은 이름이 뒤에 다시 나오면 뒤의 항목이 이긴다. 미뤄 둔 symlink가 뒤의 파일을 덮지 않도록 지운다.
		if symlinks[name] && header.Typeflag != tar.TypeSymlink {
			delete(symlinks, name)
			links = packDropLink(links, name)
		}
		if header.Typeflag != tar.TypeDir {
			delete(dirs, name)
		}
		// tar에 따로 적히지 않은 부모는 시스템 tar처럼 보통 권한(umask를 거친 0755)으로 만든다.
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return counts, err
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return counts, err
			}
			dirs[name] = header
			counts.dirs++
			report.line("x " + viewSanitize(name) + "/")
		// archive/tar는 GNU sparse 항목의 빈 구간을 0으로 채워 읽어 준다. 내용은 일반 파일과 같다.
		case tar.TypeReg, tar.TypeGNUSparse:
			data, appleDouble, err := packAppleDouble(name, header.Size, reader)
			if err != nil {
				return counts, err
			}
			if appleDouble {
				counts.skipped++
				report.line("- " + viewSanitize(name) + " (" + T("pack.verbose.apple_double") + ")")
				continue
			}
			written, err := packUntarFile(root, name, header, data, buffer, owner, durable)
			if err != nil {
				return counts, err
			}
			counts.files++
			counts.bytes += written
			report.line("x " + viewSanitize(name) + "  " + formatSizeShort(uint64(written)))
		case tar.TypeSymlink:
			symlinks[name] = true
			links = packDropLink(links, name)
			copied := *header
			copied.Name = name
			links = append(links, &copied)
			counts.files++
		case tar.TypeLink:
			target, ok := packEntryName(header.Linkname)
			if !ok || target == "." || packThroughLink(target, symlinks) || symlinks[target] {
				return counts, packUnsafeError{name: header.Name, link: true}
			}
			if err := packReplace(root, name); err != nil {
				return counts, err
			}
			if err := root.Link(target, name); err != nil {
				return counts, err
			}
			counts.files++
			report.line("x " + viewSanitize(name) + " => " + viewSanitize(target))
		case tar.TypeXGlobalHeader:
		default:
			counts.skipped++
			report.line("- " + viewSanitize(name) + " (" + packTypeText(header.Typeflag) + ")")
		}
	}
	for _, header := range links {
		target, ok := packLinkTarget(header.Name, header.Linkname, symlinks)
		if !ok {
			return counts, packUnsafeError{name: header.Name, link: true}
		}
		if packTop(target) != packTop(header.Name) {
			counts.crossTop = true
		}
		if err := packReplace(root, header.Name); err != nil {
			return counts, err
		}
		if err := root.Symlink(header.Linkname, header.Name); err != nil {
			return counts, err
		}
		report.line("x " + viewSanitize(header.Name) + " -> " + viewSanitize(header.Linkname))
		if owner {
			if err := root.Lchown(header.Name, header.Uid, header.Gid); err != nil {
				return counts, err
			}
		}
	}
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Slice(names, func(left, right int) bool {
		if depth := strings.Count(names[left], "/") - strings.Count(names[right], "/"); depth != 0 {
			return depth > 0
		}
		return names[left] < names[right]
	})
	for _, name := range names {
		if err := packKeepHeader(root, name, dirs[name], owner); err != nil {
			return counts, err
		}
	}
	return counts, nil
}

// packUntarFile은 일반 파일 하나를 쓴다. 같은 이름이 앞에 있었으면(뒤에 덧붙인 tar) 지우고 새로 만든다. 이미 있던 이름이
// symlink면 O_TRUNC가 링크를 따라가 다른 항목을 고쳐 쓰므로 열기 전에 지운다. setuid와 setgid는 남기지 않는다.
func packUntarFile(root *os.Root, name string, header *tar.Header, reader io.Reader, buffer []byte, owner, durable bool) (int64, error) {
	if err := packReplace(root, name); err != nil {
		return 0, err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	written, err := io.CopyBuffer(file, reader, buffer)
	// --rm은 압축 파일을 지우므로, 그 전에 푼 파일이 디스크에 있어야 전원이 꺼져도 둘 다 잃지 않는다.
	if err == nil && durable {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = packKeepHeader(root, name, header, owner)
	}
	return written, err
}

// packKeepHeader는 tar에 적힌 권한(rwx만), 수정 시각, root면 소유자를 옮긴다.
func packKeepHeader(root *os.Root, name string, header *tar.Header, owner bool) error {
	if owner {
		if err := root.Chown(name, header.Uid, header.Gid); err != nil {
			return err
		}
	}
	if err := root.Chmod(name, fs.FileMode(header.Mode).Perm()); err != nil {
		return err
	}
	return root.Chtimes(name, header.ModTime, header.ModTime)
}

func packReplace(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// packEntryName은 tar 항목 이름을 root 기준 상대 경로로 바꾼다. 절대 경로나 ..로 root 밖에 닿는 이름이면 false다.
// "./a"와 "a/"는 "a"가 된다.
func packEntryName(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", false
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// packLinkTarget은 name 자리의 symlink가 linkname을 가리킬 때 그 대상의 root 기준 경로이고, root 밖이면 false다. 절대 경로는 root 밖이다. 경로를
// path.Join으로 정리하면 "d/a/.."가 "d"가 되어, symlink d/a를 거쳐 간다는 것이 지워진다. 그래서 정리하지 않고 한 마디씩
// 따라간다. symlink로 적힌 이름 뒤에 마디가 더 오면 그 링크가 어디로 가는지 글자로는 알 수 없으므로 거부한다. 대상 자체가
// symlink인 것은 그 링크도 같은 검사를 받으므로 받는다.
func packLinkTarget(name, linkname string, symlinks map[string]bool) (string, bool) {
	if linkname == "" || strings.HasPrefix(linkname, "/") {
		return "", false
	}
	current := path.Dir(name)
	parts := strings.Split(linkname, "/")
	for index, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if current == "." {
				return "", false
			}
			current = path.Dir(current)
			continue
		}
		current = path.Join(current, part)
		if symlinks[current] && index < len(parts)-1 {
			return "", false
		}
	}
	return current, true
}

// packTop은 root 기준 경로의 첫 마디다. root 자체는 "."이다.
func packTop(name string) string {
	top, _, _ := strings.Cut(name, "/")
	return top
}

func packDropLink(links []*tar.Header, name string) []*tar.Header {
	kept := links[:0]
	for _, header := range links {
		if header.Name != name {
			kept = append(kept, header)
		}
	}
	return kept
}

// packThroughLink는 name의 부모 가운데 symlink로 적힌 이름이 있는지 본다.
func packThroughLink(name string, symlinks map[string]bool) bool {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if symlinks[parent] {
			return true
		}
	}
	return false
}

// packPlace는 다 푼 temp를 최종 이름으로 옮긴다. 이미 있는 이름은 덮지 않는다. 최상위가 실제 디렉터리 하나면 그 이름으로,
// 아니면 압축 파일 이름(확장자를 뗀)이나 -o 이름으로 temp 자체를 옮긴다.
func packPlace(temp, archivePath, output string, crossTop bool) (string, error) {
	entries, err := os.ReadDir(temp)
	if err != nil {
		return "", err
	}
	if output == "" && len(entries) == 1 && !crossTop {
		single := filepath.Join(temp, entries[0].Name())
		if info, err := os.Lstat(single); err == nil && info.IsDir() {
			final := filepath.Join(filepath.Dir(temp), entries[0].Name())
			if err := packRenameNoReplace(single, final); err != nil {
				return "", err
			}
			os.Remove(temp)
			return final, nil
		}
	}
	final := output
	if final == "" {
		final = packArchiveName(archivePath)
	}
	if err := packRenameNoReplace(temp, final); err != nil {
		return "", err
	}
	// 임시 디렉터리는 0700으로 만들었다. 여러 항목을 담는 이 디렉터리는 tar에 없으므로 보통 디렉터리 권한을 준다.
	return final, os.Chmod(final, 0o755)
}

// packList는 -l이다. tar면 항목을, 압축한 파일 하나면 형식과 푼 크기를 stdout에 쓴다. 끝까지 읽어 체크섬도 확인한다.
func packList(archivePath string, kind packKind, stdout io.Writer) error {
	if !kind.archive() {
		return errPackUnknown
	}
	input, _, err := codecOpenInput(archivePath)
	if err != nil {
		return err
	}
	defer input.Close()
	var stream io.Reader = input
	switch kind.format {
	case "":
	case packFormatZip:
		info, err := input.Stat()
		if err != nil {
			return err
		}
		var read atomic.Int64
		zipped := newPackZipStream(context.Background(), input, info.Size(), &read)
		defer zipped.Close()
		stream = zipped
	default:
		reader, err := packOpenStream(input, kind.format)
		if err != nil {
			return err
		}
		defer reader.Close()
		stream = reader
	}
	if !kind.many() {
		size, err := io.Copy(io.Discard, stream)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, T("pack.list_single", viewSanitize(archivePath), kind.format, formatSizeShort(uint64(size))))
		return err
	}
	reader := tar.NewReader(stream)
	var counts packUntarCounts
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			counts.dirs++
		} else {
			counts.files++
			counts.bytes += header.Size
		}
		fmt.Fprintf(stdout, "%s %9s %s  %s\n", packModeText(header), formatSizeShort(uint64(header.Size)), header.ModTime.Format("2006-01-02 15:04"), packTarHeaderName(header))
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, T("pack.list_summary", counts.files, counts.dirs, formatSizeShort(uint64(counts.bytes))))
	return err
}

// packModeText는 ls처럼 종류 글자(d, l, -)와 rwx를 쓴다. FileMode.String은 symlink를 L로 쓴다.
func packModeText(header *tar.Header) string {
	kind := "-"
	switch header.Typeflag {
	case tar.TypeDir:
		kind = "d"
	case tar.TypeSymlink:
		kind = "l"
	case tar.TypeLink:
		kind = "h"
	}
	return kind + fs.FileMode(header.Mode).Perm().String()[1:]
}

// packRemoveTemp는 반쯤 푼 임시 디렉터리를 지운다. tar대로 0555가 된 디렉터리 안은 RemoveAll이 지우지 못하므로 먼저 쓰기
// 권한을 준다. WalkDir은 symlink를 따라가지 않으므로 temp 밖의 권한은 바꾸지 않는다.
func packRemoveTemp(temp string) {
	filepath.WalkDir(temp, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			os.Chmod(name, 0o700)
		}
		return nil
	})
	os.RemoveAll(temp)
}

// packAppleDoubleMagic은 AppleDouble 파일의 앞 4바이트다.
var packAppleDoubleMagic = []byte{0x00, 0x05, 0x16, 0x07}

// packAppleDoubleHeader는 AppleDouble 고정 머리말의 길이다. 표시 4바이트, 버전 4바이트, filler 16바이트, 항목 수 2바이트다.
// 그 뒤에 항목마다 id, offset, 길이가 4바이트씩인 descriptor가 온다.
const (
	packAppleDoubleHeader     = 26
	packAppleDoubleDescriptor = 12
	// packAppleDoubleMaxEntries는 AppleDouble로 볼 항목 수의 상한이다. macOS tar는 2개를 쓴다. 큰 수로 긴 표를 읽게
	// 하지 않으려는 것이다.
	packAppleDoubleMaxEntries = 16
)

// packAppleDouble은 macOS tar가 확장 속성을 담으려고 넣는 "._이름" 항목인지 본다. macOS tar는 이것을 보여 주지 않고 속성으로
// 되돌리지만, 그대로 풀면 "._made" 같은 파일이 생기고 최상위가 둘로 보여 압축 파일 이름의 디렉터리에 한 번 더 감싸 푼다.
// 이름만으로 고르면 "._"로 시작하는 보통 파일을 잃으므로 내용도 본다. 건너뛴 항목은 풀지 않으므로, 앞부분만 닮은 보통
// 파일까지 버리지 않게 버전(1 또는 2), 항목 수, 항목 표가 다 있고 각 항목의 데이터가 표 뒤에서 항목 크기(size) 안에 있어야
// AppleDouble로 본다. 아니면 읽은 앞부분을 되돌린 reader를 준다.
func packAppleDouble(name string, size int64, reader io.Reader) (io.Reader, bool, error) {
	if !strings.HasPrefix(path.Base(name), "._") {
		return reader, false, nil
	}
	head := make([]byte, packAppleDoubleHeader)
	n, err := io.ReadFull(reader, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	read := head[:n]
	if n == len(head) && bytes.Equal(head[:4], packAppleDoubleMagic) {
		version := binary.BigEndian.Uint32(head[4:8])
		count := int(binary.BigEndian.Uint16(head[24:26]))
		if (version == 0x00010000 || version == 0x00020000) && count > 0 && count <= packAppleDoubleMaxEntries {
			table := make([]byte, count*packAppleDoubleDescriptor)
			got, err := io.ReadFull(reader, table)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				return nil, false, err
			}
			read = append(read, table[:got]...)
			if got == len(table) && packAppleDoubleTable(table, packAppleDoubleHeader+len(table), size) {
				return nil, true, nil
			}
		}
	}
	return io.MultiReader(bytes.NewReader(read), reader), false, nil
}

// packAppleDoubleTable은 descriptor마다 id가 0이 아니고, 데이터가 항목 표(end) 뒤에서 시작해 항목 크기(size) 안에서 끝나는지
// 본다. offset과 길이는 4바이트라 더해도 int64를 넘지 않는다.
func packAppleDoubleTable(table []byte, end int, size int64) bool {
	for at := 0; at < len(table); at += packAppleDoubleDescriptor {
		offset := int64(binary.BigEndian.Uint32(table[at+4:]))
		length := int64(binary.BigEndian.Uint32(table[at+8:]))
		if binary.BigEndian.Uint32(table[at:]) == 0 || offset < int64(end) || offset+length > size {
			return false
		}
	}
	return true
}

// packSpecialText는 묶을 때 건너뛴 특수 파일의 종류다.
func packSpecialText(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSocket != 0:
		return T("pack.verbose.socket")
	case mode&fs.ModeNamedPipe != 0:
		return "FIFO"
	case mode&fs.ModeCharDevice != 0:
		return T("pack.verbose.char_device")
	case mode&fs.ModeDevice != 0:
		return T("pack.verbose.block_device")
	}
	return mode.Type().String()
}

// packTypeText는 풀 때 건너뛴 tar 항목의 종류다.
func packTypeText(flag byte) string {
	switch flag {
	case tar.TypeChar:
		return T("pack.verbose.char_device")
	case tar.TypeBlock:
		return T("pack.verbose.block_device")
	case tar.TypeFifo:
		return "FIFO"
	}
	return fmt.Sprintf("tar type %q", flag)
}

// packInside는 target이 dir 안(dir 자체 포함)에 놓이는지 본다. target은 아직 없으므로 부모 디렉터리의 링크를 푼다. 부모가 없으면
// 출력을 만들 때 실패하므로 글자 그대로 비교한다.
func packInside(dir, target string) (bool, error) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, err
	}
	absoluteTarget, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	parent, base := filepath.Split(absoluteTarget)
	if realParent, err := filepath.EvalSymlinks(parent); err == nil {
		absoluteTarget = filepath.Join(realParent, base)
	}
	inside, err := filepath.Rel(realDir, absoluteTarget)
	if err != nil {
		return false, nil
	}
	return inside != ".." && !strings.HasPrefix(inside, "../"), nil
}
