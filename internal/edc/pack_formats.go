package edc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"

	"github.com/ulikunitz/xz"
)

const (
	packFormatBzip2 = "bzip2"
	packFormatXZ    = "xz"
	packFormatZip   = "zip"
	// packMagicSize는 형식을 고르려고 읽는 앞부분의 길이다. xz와 7z의 표시가 6바이트로 가장 길다.
	packMagicSize = 6
	// packZipLinkLimit는 zip symlink 항목에서 읽는 대상 경로의 상한이다. 내용이 대상 경로인데, 큰 항목을 경로로 읽지 않는다.
	packZipLinkLimit = 4096
)

var (
	errPackZipName      = errors.New("zip data whose name does not end in .zip")
	errPackZipEncrypted = errors.New("the zip has an encrypted entry")
)

// packForeignError는 압축된 형식으로 알아보지만 edc가 풀지 못하는 입력이다. 다시 압축해도 거의 줄지 않는다.
type packForeignError struct{ format string }

func (err packForeignError) Error() string { return err.format + " data is already compressed" }

// packZipMethodError는 archive/zip이 읽지 못하는 압축 방법(LZMA, bzip2 등)을 쓴 zip 항목이다.
type packZipMethodError struct{ name string }

func (err packZipMethodError) Error() string { return "unsupported zip method: " + err.name }

// packFormat은 앞부분 바이트가 가리키는 압축 형식이다. gzip과 zstd는 edc compress와 같이 고른다. 빈 zip은 끝 표시만 있다.
func packFormat(magic []byte) string {
	if format := codecFormat(magic[:min(len(magic), 4)]); format != "" {
		return format
	}
	switch {
	case len(magic) >= 4 && bytes.HasPrefix(magic, []byte("BZh")) && magic[3] >= '1' && magic[3] <= '9':
		return packFormatBzip2
	case bytes.HasPrefix(magic, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		return packFormatXZ
	case bytes.HasPrefix(magic, []byte("PK\x03\x04")), bytes.HasPrefix(magic, []byte("PK\x05\x06")):
		return packFormatZip
	}
	return ""
}

// packForeignFormat은 edc가 풀지 못하지만 이미 압축된 형식이다. 자동으로는 이런 파일을 다시 압축하지 않는다.
func packForeignFormat(magic []byte) string {
	switch {
	case bytes.HasPrefix(magic, []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}):
		return "7z"
	case bytes.HasPrefix(magic, []byte("Rar!\x1a\x07")):
		return "rar"
	case bytes.HasPrefix(magic, []byte{0x04, 0x22, 0x4d, 0x18}):
		return "lz4"
	}
	return ""
}

// packZipNamed는 이름이 .zip으로 끝나는지 본다. docx, xlsx, jar, apk, epub도 내용은 zip이라 내용만 보고 풀면 문서를
// 디렉터리로 바꾸고, --rm이면 문서를 지운다. Windows에서 만든 .ZIP도 받는다.
func packZipNamed(path string) bool {
	return len(path) > 4 && strings.EqualFold(path[len(path)-4:], ".zip")
}

// packOpenStream은 한 줄로 읽는 압축 형식을 푸는 reader를 연다. gzip과 zstd는 codecOpenStream이 연다.
func packOpenStream(src io.Reader, format string) (io.ReadCloser, error) {
	switch format {
	case packFormatBzip2:
		return io.NopCloser(bzip2.NewReader(src)), nil
	case packFormatXZ:
		reader, err := xz.NewReader(src)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(reader), nil
	}
	return codecOpenStream(src)
}

// packZipStream은 zip을 tar 스트림으로 바꿔 읽는다. 푸는 쪽(packUntar, packList)이 tar와 같은 검사(이름, 링크, 권한)를
// 그대로 쓰게 하려는 것이다. zip은 끝의 목록을 먼저 읽어야 해서 파일을 offset으로 읽는다. read는 읽은 바이트 수이고
// skipped는 바꾸지 않고 건너뛴 항목 수다.
type packZipStream struct {
	*io.PipeReader
	skipped atomic.Int64
}

func newPackZipStream(ctx context.Context, input *os.File, size int64, read *atomic.Int64) *packZipStream {
	reader, writer := io.Pipe()
	stream := &packZipStream{PipeReader: reader}
	source := io.NewSectionReader(packCountingReaderAt{reader: input, count: read}, 0, size)
	go func() { writer.CloseWithError(packWriteZipAsTar(ctx, source, size, writer, &stream.skipped)) }()
	return stream
}

// packWriteZipAsTar는 zip 항목을 차례로 tar 항목으로 쓴다. macOS가 넣는 __MACOSX/ 아래 항목은 확장 속성의 사본이라 건너뛴다.
// zip의 권한은 만든 쪽 umask를 거치지 않은 경우가 많아(Windows는 0666) group과 other의 쓰기 권한을 뺀다. 항목 내용의 크기와
// CRC는 archive/zip이 확인하고, 적힌 크기와 다르면 tar writer가 실패한다.
func packWriteZipAsTar(ctx context.Context, source io.ReaderAt, size int64, dst io.Writer, skipped *atomic.Int64) error {
	archive, err := zip.NewReader(source, size)
	// ErrInsecurePath는 GODEBUG로 켤 때만 나고 reader도 함께 준다. 밖을 가리키는 이름은 packUntar가 거부한다.
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return err
	}
	out := tar.NewWriter(dst)
	for _, file := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.Name == "__MACOSX" || strings.HasPrefix(file.Name, "__MACOSX/") {
			skipped.Add(1)
			continue
		}
		if file.Flags&0x1 != 0 {
			return fmt.Errorf("%s: %w", file.Name, errPackZipEncrypted)
		}
		if err := packZipEntry(file, out); err != nil {
			return err
		}
	}
	return out.Close()
}

func packZipEntry(file *zip.File, out *tar.Writer) error {
	info := file.FileInfo()
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		content, err := packZipRead(file, packZipLinkLimit)
		if err != nil {
			return err
		}
		link = string(content)
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = file.Name
	header.Mode &^= 0o022
	if strings.HasSuffix(file.Name, "/") {
		header.Typeflag, header.Size = tar.TypeDir, 0
	}
	// unix로 만들었지만 속성을 0으로 둔 zip은 권한이 0으로 읽힌다. unzip처럼 보통 권한을 준다.
	if header.Mode&0o777 == 0 {
		header.Mode |= 0o644
		if header.Typeflag == tar.TypeDir {
			header.Mode |= 0o111
		}
	}
	if err := out.WriteHeader(header); err != nil {
		return err
	}
	if header.Typeflag != tar.TypeReg {
		return nil
	}
	content, err := file.Open()
	if errors.Is(err, zip.ErrAlgorithm) {
		return packZipMethodError{name: file.Name}
	}
	if err != nil {
		return err
	}
	defer content.Close()
	_, err = io.Copy(out, content)
	return err
}

func packZipRead(file *zip.File, limit int64) ([]byte, error) {
	content, err := file.Open()
	if errors.Is(err, zip.ErrAlgorithm) {
		return nil, packZipMethodError{name: file.Name}
	}
	if err != nil {
		return nil, err
	}
	defer content.Close()
	return io.ReadAll(io.LimitReader(content, limit))
}

// packCountingReaderAt은 -v의 진행 줄이 zip을 읽은 양을 보게 한다. zip은 offset으로 읽어 같은 곳을 두 번 읽을 수 있다.
type packCountingReaderAt struct {
	reader io.ReaderAt
	count  *atomic.Int64
}

func (reader packCountingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := reader.reader.ReadAt(p, offset)
	reader.count.Add(int64(n))
	return n, err
}
