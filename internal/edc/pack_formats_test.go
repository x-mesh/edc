package edc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/base64"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// bzip2 CLI로 만든 시험 자료다. compress/bzip2에는 쓰는 쪽이 없다. packBzip2Tar는 b/one.txt("one\n")를 담은 ustar tar다.
const (
	packBzip2Plain = "QlpoOTFBWSZTWTEM96YAAAVZgAAQQAAQADAlQBAgACIANqEAMJ5LNLGR4u5IpwoSBiGe9MA="
	packBzip2Tar   = "QlpoOTFBWSZTWe3ofgAAAGr7kNGQAYBAAf+AAIFydZ7ABAAACCAAkglTRGmpoYnpHqHqPUHptQKqE0RoxNAGj1DQHdRnduwva/0FohGJDGysZpwQ0Cki8sRcZVSYxlXRbpEJt6Jl1kmRdLFUCLC5OWTkhJHox95NjhwQcyA2Y9piEhF/n01LFSA4oVEIUwKlhVLKKaEYgbi7kinChIdvQ/AA"
)

type packZipItem struct {
	name, body string
	mode       fs.FileMode
	method     uint16
	flags      uint16
}

func packZip(t *testing.T, path string, items ...packZipItem) {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	// 14는 LZMA다. archive/zip은 이 방법을 읽지 못하므로, 쓸 때만 내용을 그대로 담는 압축기를 붙인다.
	archive.RegisterCompressor(14, func(out io.Writer) (io.WriteCloser, error) { return packNopWriteCloser{out}, nil })
	for _, item := range items {
		header := &zip.FileHeader{Name: item.name, Method: item.method, Flags: item.flags, Modified: time.Unix(1700000000, 0)}
		if header.Method == 0 && !strings.HasSuffix(item.name, "/") {
			header.Method = zip.Deflate
		}
		mode := item.mode
		if strings.HasSuffix(item.name, "/") {
			mode |= fs.ModeDir | 0o755
		} else if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(item.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

type packNopWriteCloser struct{ io.Writer }

func (packNopWriteCloser) Close() error { return nil }

func packTarBytes(t *testing.T, entries ...packEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0o644, ModTime: time.Unix(1700000000, 0)}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if entry.kind == tar.TypeDir {
			header.Mode = 0o755
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func packXZ(t *testing.T, path string, data []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := xz.NewWriter(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func packBase64(t *testing.T, path, encoded string) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// xz와 bzip2는 gzip처럼 내용으로 알아보고, tar면 디렉터리로, 파일 하나면 확장자를 떼어 푼다. -l도 읽는다.
func TestPackExtractsXZAndBzip2(t *testing.T) {
	dir := t.TempDir()
	packXZ(t, filepath.Join(dir, "logs.tar.xz"), packTarBytes(t, packEntry{name: "logs/", kind: tar.TypeDir}, packEntry{name: "logs/a.log", body: "aaa", kind: tar.TypeReg}))
	if code, _, stderr := packTest(t, dir, "logs.tar.xz"); code != 0 || packRead(t, filepath.Join(dir, "logs", "a.log")) != "aaa" {
		t.Fatalf("tar.xz: %d %s", code, stderr)
	}
	packXZ(t, filepath.Join(dir, "note.txt.xz"), []byte("plain xz\n"))
	if code, _, stderr := packTest(t, dir, "note.txt.xz"); code != 0 || packRead(t, filepath.Join(dir, "note.txt")) != "plain xz\n" {
		t.Fatalf("xz file: %d %s", code, stderr)
	}
	packBase64(t, filepath.Join(dir, "b.tar.bz2"), packBzip2Tar)
	if code, stdout, stderr := packTest(t, dir, "-l", "b.tar.bz2"); code != 0 || !strings.Contains(stdout, "b/one.txt") {
		t.Fatalf("list tar.bz2: %d %q %s", code, stdout, stderr)
	}
	if code, _, stderr := packTest(t, dir, "b.tar.bz2"); code != 0 || packRead(t, filepath.Join(dir, "b", "one.txt")) != "one\n" {
		t.Fatalf("tar.bz2: %d %s", code, stderr)
	}
	packBase64(t, filepath.Join(dir, "plain.bz2"), packBzip2Plain)
	if code, _, stderr := packTest(t, dir, "plain.bz2"); code != 0 || packRead(t, filepath.Join(dir, "plain")) != "plain bzip2\n" {
		t.Fatalf("bzip2 file: %d %s", code, stderr)
	}
	if kind, err := packDetect(filepath.Join(dir, "b.tar.bz2")); err != nil || kind.format != packFormatBzip2 || !kind.tar {
		t.Fatalf("bzip2 kind %+v %v", kind, err)
	}
	if left := packLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("leftovers %v", left)
	}
}

// .zip은 tar와 같이 푼다. 최상위가 디렉터리 하나면 그 이름으로, 아니면 압축 파일 이름으로 감싼다. 디렉터리 항목 없이 적힌
// 부모도 만들고, __MACOSX/는 AppleDouble처럼 건너뛴 항목으로 세며, group과 other의 쓰기 권한은 뺀다. --rm은 다 푼 뒤 zip을
// 지운다.
func TestPackExtractsZip(t *testing.T) {
	dir := t.TempDir()
	packZip(t, filepath.Join(dir, "site.zip"),
		packZipItem{name: "site/"},
		packZipItem{name: "site/index.html", body: "<p>hi</p>", mode: 0o666},
		packZipItem{name: "site/css/app.css", body: "p{}", method: zip.Store},
		packZipItem{name: "__MACOSX/site/._index.html", body: "attr"},
	)
	code, _, stderr := packTest(t, dir, "site.zip")
	if code != 0 || packRead(t, filepath.Join(dir, "site", "index.html")) != "<p>hi</p>" || packRead(t, filepath.Join(dir, "site", "css", "app.css")) != "p{}" {
		t.Fatalf("zip: %d %s", code, stderr)
	}
	if !strings.Contains(stderr, "site.zip → site/") || strings.Contains(stderr, " 0 B → ") || !strings.Contains(stderr, "1 special files skipped") {
		t.Fatalf("result line %q", stderr)
	}
	if packExists(filepath.Join(dir, "__MACOSX")) {
		t.Fatal("__MACOSX is skipped")
	}
	info, err := os.Stat(filepath.Join(dir, "site", "index.html"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v %v", info, err)
	}
	packZip(t, filepath.Join(dir, "Flat.ZIP"), packZipItem{name: "a.txt", body: "a"}, packZipItem{name: "b.txt", body: "b"})
	if code, stdout, _ := packTest(t, dir, "-l", "Flat.ZIP"); code != 0 || !strings.Contains(stdout, "a.txt") || !strings.Contains(stdout, "2 files") {
		t.Fatalf("list zip: %d %q", code, stdout)
	}
	if code, _, stderr := packTest(t, dir, "--rm", "Flat.ZIP"); code != 0 || packRead(t, filepath.Join(dir, "Flat", "b.txt")) != "b" || packExists(filepath.Join(dir, "Flat.ZIP")) {
		t.Fatalf("flat zip with --rm: %d %s", code, stderr)
	}
	if left := packLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("leftovers %v", left)
	}
}

// 이름이 .zip이 아닌 zip(Office 문서 등)과 7z 같은 형식은 자동으로 풀지도 다시 압축하지도 않는다. -x와 -z가 방향을 정한다.
func TestPackRefusesCompressedDataItCannotJudge(t *testing.T) {
	dir := t.TempDir()
	packZip(t, filepath.Join(dir, "report.docx"), packZipItem{name: "[Content_Types].xml", body: "<t/>"}, packZipItem{name: "word/document.xml", body: "<w/>"})
	if code, _, stderr := packTest(t, dir, "report.docx"); code != 1 || !strings.Contains(stderr, "the name does not end in .zip") || packExists(filepath.Join(dir, "report.docx.gz")) {
		t.Fatalf("docx: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "-x", "report.docx"); code != 0 || packRead(t, filepath.Join(dir, "report.docx.out", "word", "document.xml")) != "<w/>" {
		t.Fatalf("-x docx: %d %s", code, stderr)
	}
	packWrite(t, filepath.Join(dir, "a.7z"), "7z\xbc\xaf\x27\x1c rest")
	if code, _, stderr := packTest(t, dir, "a.7z"); code != 1 || !strings.Contains(stderr, "7z data is already compressed") {
		t.Fatalf("7z: %d %s", code, stderr)
	}
	if code, _, _ := packTest(t, dir, "-z", "a.7z"); code != 0 || !packExists(filepath.Join(dir, "a.7z.gz")) {
		t.Fatal("-z compresses 7z anyway")
	}
	if code, _, stderr := packTest(t, dir, "-x", "a.7z"); code != 1 || !strings.Contains(stderr, "not gzip, zstd, xz, bzip2, zip, or tar data") {
		t.Fatalf("-x 7z: %d %s", code, stderr)
	}
}

// zip 안의 이름과 링크도 tar와 같은 검사를 받는다. 암호화된 항목과 읽지 못하는 압축 방법은 이름을 대고 실패하고, 반쯤 푼 결과를
// 남기지 않는다.
func TestPackRejectsUnsafeAndUnreadableZips(t *testing.T) {
	dir := t.TempDir()
	packZip(t, filepath.Join(dir, "evil.zip"), packZipItem{name: "../evil.txt", body: "x"})
	if code, _, stderr := packTest(t, dir, "evil.zip"); code != 1 || !strings.Contains(stderr, "entry outside the output directory") {
		t.Fatalf("path: %d %s", code, stderr)
	}
	packZip(t, filepath.Join(dir, "link.zip"), packZipItem{name: "d/", body: ""}, packZipItem{name: "d/up", body: "../../etc", mode: fs.ModeSymlink | 0o777})
	if code, _, stderr := packTest(t, dir, "link.zip"); code != 1 || !strings.Contains(stderr, "link that points outside") {
		t.Fatalf("link: %d %s", code, stderr)
	}
	packZip(t, filepath.Join(dir, "secret.zip"), packZipItem{name: "s.txt", body: "x", flags: 0x1})
	if code, _, stderr := packTest(t, dir, "secret.zip"); code != 1 || !strings.Contains(stderr, "encrypted entry") || !strings.Contains(stderr, "s.txt") {
		t.Fatalf("encrypted: %d %s", code, stderr)
	}
	packZip(t, filepath.Join(dir, "lzma.zip"), packZipItem{name: "l.txt", body: "x", method: 14})
	if code, _, stderr := packTest(t, dir, "lzma.zip"); code != 1 || !strings.Contains(stderr, "compression method that edc does not read: l.txt") {
		t.Fatalf("method: %d %s", code, stderr)
	}
	for _, name := range []string{"evil", "link", "secret", "lzma"} {
		if packExists(filepath.Join(dir, name)) {
			t.Fatalf("%s was extracted", name)
		}
	}
	if left := packLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("leftovers %v", left)
	}
}

// unix로 만들고 속성을 0으로 둔 zip 항목은 권한 0으로 읽힌다. 읽을 수 없는 파일로 풀지 않고 보통 권한을 준다.
func TestPackZipWithoutModeGetsADefault(t *testing.T) {
	dir := t.TempDir()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	header := &zip.FileHeader{Name: "z/zero.txt", Method: zip.Store, CreatorVersion: 3 << 8}
	writer, err := archive.CreateRaw(&zip.FileHeader{Name: header.Name, Method: zip.Store, CreatorVersion: header.CreatorVersion, CRC32: 0x3610a686, CompressedSize64: 5, UncompressedSize64: 5})
	if err != nil {
		t.Fatal(err)
	}
	writer.Write([]byte("hello"))
	archive.Close()
	if err := os.WriteFile(filepath.Join(dir, "zero.zip"), buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := packTest(t, dir, "zero.zip"); code != 0 || packRead(t, filepath.Join(dir, "z", "zero.txt")) != "hello" {
		t.Fatalf("zero mode: %d %s", code, stderr)
	}
	info, err := os.Stat(filepath.Join(dir, "z", "zero.txt"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v %v", info, err)
	}
}
