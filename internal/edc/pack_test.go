package edc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func packTest(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	return packTestContext(t, context.Background(), dir, args...)
}

func packTestContext(t *testing.T, ctx context.Context, dir string, args ...string) (int, string, string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)
	var stdout, stderr bytes.Buffer
	code := packRun(ctx, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func packWrite(t *testing.T, name string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func packRead(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// packLeftovers는 dir에 남은 임시 출력(.tmp, .unpack)이다. 실패하거나 취소해도 남지 않아야 한다.
func packLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".unpack") || strings.HasSuffix(entry.Name(), ".tmp") {
			left = append(left, entry.Name())
		}
	}
	return left
}

type packEntry struct {
	name, body, link string
	kind             byte
	mode             int64
}

func packTarGz(t *testing.T, path string, entries ...packEntry) {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: mode, Linkname: entry.link, ModTime: time.Unix(1700000000, 0)}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := archive.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	archive.Close()
	compressed.Close()
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 내용으로 방향을 정한다. 압축 파일은 풀고, 아니면 압축한다. -z는 무엇이든 압축하고, -x는 이름과 관계없이 풀되 압축 형식이
// 아니면 실패한다.
func TestPackFileDirection(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("hello pack\n", 500)
	packWrite(t, filepath.Join(dir, "app.log"), body)
	if code, _, stderr := packTest(t, dir, "app.log"); code != 0 || !strings.Contains(stderr, "app.log → app.log.gz") {
		t.Fatalf("compress: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "app.log.gz"); code != 1 || !strings.Contains(stderr, "app.log exists; delete it or give -o") {
		t.Fatalf("an existing output is kept: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "-o", "again.log", "app.log.gz"); code != 0 || packRead(t, filepath.Join(dir, "again.log")) != body {
		t.Fatalf("decompress: %d %s", code, stderr)
	}
	if code, _, _ := packTest(t, dir, "-z", "app.log.gz"); code != 0 || !packExists(filepath.Join(dir, "app.log.gz.gz")) {
		t.Fatal("-z compresses a compressed file again")
	}
	if code, _, stderr := packTest(t, dir, "-x", "app.log"); code != 1 || !strings.Contains(stderr, "not gzip, zstd, or tar data") {
		t.Fatalf("-x on plain data: %d %s", code, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "app.log.gz"))
	packWrite(t, filepath.Join(dir, "backup.bin"), string(data))
	if code, _, _ := packTest(t, dir, "-x", "backup.bin"); code != 0 || packRead(t, filepath.Join(dir, "backup.bin.out")) != body {
		t.Fatal("-x reads gzip whatever the name is, and adds .out")
	}
	if code, _, _ := packTest(t, dir, "--zstd", "-o", "app.zst", "app.log"); code != 0 {
		t.Fatal("--zstd")
	}
	if kind, err := packDetect(filepath.Join(dir, "app.zst")); err != nil || kind.format != "zstd" || kind.tar {
		t.Fatalf("zstd kind %+v %v", kind, err)
	}
	if code, _, _ := packTest(t, dir, "-z", "-x", "app.log"); code != 2 {
		t.Fatal("-z and -x together are a usage error")
	}
}

func packExists(name string) bool {
	_, err := os.Lstat(name)
	return err == nil
}

// 디렉터리를 묶고 풀면 내용, 권한, symlink가 그대로다. 항목은 이름순이다. FIFO는 건너뛰고 개수를 알린다.
func TestPackDirectoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "logs")
	packWrite(t, filepath.Join(tree, "b.log"), "bbb")
	packWrite(t, filepath.Join(tree, "a.log"), "aaa")
	packWrite(t, filepath.Join(tree, "sub", "c.txt"), "ccc")
	if err := os.Symlink("a.log", filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(tree, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(tree, "sub"), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(tree, "sub"), 0o755)
	code, _, stderr := packTest(t, dir, "logs")
	if code != 0 || !strings.Contains(stderr, "logs/ → logs.tar.gz  4 files") || !strings.Contains(stderr, "1 special files skipped") {
		t.Fatalf("pack: %d %s", code, stderr)
	}
	names := packTarNames(t, filepath.Join(dir, "logs.tar.gz"))
	want := []string{"logs/", "logs/a.log", "logs/b.log", "logs/link", "logs/sub/", "logs/sub/c.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries %v", names)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "logs.tar.gz"), filepath.Join(out, "logs.tar.gz")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = packTest(t, out, "logs.tar.gz")
	if code != 0 || !strings.Contains(stderr, "logs.tar.gz → logs/  4 files, 2 directories") {
		t.Fatalf("unpack: %d %s", code, stderr)
	}
	defer os.Chmod(filepath.Join(out, "logs", "sub"), 0o755)
	if packRead(t, filepath.Join(out, "logs", "sub", "c.txt")) != "ccc" || packRead(t, filepath.Join(out, "logs", "link")) != "aaa" {
		t.Fatal("content")
	}
	if link, err := os.Readlink(filepath.Join(out, "logs", "link")); err != nil || link != "a.log" {
		t.Fatalf("symlink %q %v", link, err)
	}
	if info, err := os.Stat(filepath.Join(out, "logs", "sub")); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("a read-only directory keeps its mode: %v %v", info.Mode(), err)
	}
	if left := packLeftovers(t, out); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

func packTarNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(reader)
	var names []string
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	return names
}

// 최상위가 디렉터리 하나면 그 이름으로, 여럿이면 압축 파일 이름으로, -o면 그 디렉터리에 푼다. 이미 있는 이름은 덮지 않고
// 임시 디렉터리도 남기지 않는다.
func TestPackExtractPlacement(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "one.tar.gz"), packEntry{name: "top/", kind: tar.TypeDir, mode: 0o755}, packEntry{name: "top/a", kind: tar.TypeReg, body: "a"})
	packTarGz(t, filepath.Join(dir, "many.tgz"), packEntry{name: "a", kind: tar.TypeReg, body: "a"}, packEntry{name: "b/c", kind: tar.TypeReg, body: "c"})
	if code, _, stderr := packTest(t, dir, "one.tar.gz"); code != 0 || packRead(t, filepath.Join(dir, "top", "a")) != "a" {
		t.Fatalf("single top: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "many.tgz"); code != 0 || packRead(t, filepath.Join(dir, "many", "b", "c")) != "c" {
		t.Fatalf("many: %d %s", code, stderr)
	}
	// tar에 적히지 않은 부모 b는 보통 권한이다. 여러 항목을 담은 디렉터리도 그렇다.
	for _, name := range []string{"many", filepath.Join("many", "b")} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Mode().Perm()&0o755 != 0o755 {
			t.Fatalf("%s mode %v %v", name, info.Mode(), err)
		}
	}
	if code, _, _ := packTest(t, dir, "-o", "chosen", "one.tar.gz"); code != 0 || packRead(t, filepath.Join(dir, "chosen", "top", "a")) != "a" {
		t.Fatal("-o extracts into that directory")
	}
	if code, _, stderr := packTest(t, dir, "one.tar.gz"); code != 1 || !strings.Contains(stderr, "top exists; delete it or give -o") {
		t.Fatalf("existing output: %d %s", code, stderr)
	}
	if left := packLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

// 출력 디렉터리 밖에 쓰거나 밖을 가리키는 tar는 거부하고, 임시 디렉터리와 그 밖 어디에도 남기지 않는다.
func TestPackRejectsUnsafeArchives(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []packEntry
	}{
		{"dotdot", []packEntry{{name: "../evil", kind: tar.TypeReg, body: "x"}}},
		{"absolute", []packEntry{{name: "/tmp/evil", kind: tar.TypeReg, body: "x"}}},
		{"absolute link", []packEntry{{name: "etc", kind: tar.TypeSymlink, link: "/etc"}}},
		{"dotdot link", []packEntry{{name: "a/up", kind: tar.TypeSymlink, link: "../../x"}}},
		// 글자로는 둘 다 안쪽이지만, d/a가 root를 가리키므로 b는 root의 부모를 가리킨다.
		{"link chain", []packEntry{{name: "d/a", kind: tar.TypeSymlink, link: ".."}, {name: "b", kind: tar.TypeSymlink, link: "d/a/.."}, {name: "b/evil", kind: tar.TypeReg, body: "x"}}},
		// 파일 없이 링크만 남겨도 풀린 디렉터리의 b가 밖을 가리킨다.
		{"link chain only", []packEntry{{name: "d/a", kind: tar.TypeSymlink, link: ".."}, {name: "b", kind: tar.TypeSymlink, link: "d/a/.."}}},
		{"through link", []packEntry{{name: "x", kind: tar.TypeSymlink, link: "sub"}, {name: "x/file", kind: tar.TypeReg, body: "x"}}},
		{"hard link out", []packEntry{{name: "h", kind: tar.TypeLink, link: "../outside"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "work")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			packTarGz(t, filepath.Join(dir, "bad.tar.gz"), test.entries...)
			code, _, stderr := packTest(t, dir, "bad.tar.gz")
			if code != 1 || !strings.Contains(stderr, "outside the output directory") {
				t.Fatalf("%d %s", code, stderr)
			}
			for _, place := range []string{parent, dir} {
				entries, _ := os.ReadDir(place)
				var names []string
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				sort.Strings(names)
				want := "work"
				if place == dir {
					want = "bad.tar.gz"
				}
				if strings.Join(names, ",") != want {
					t.Fatalf("%s has %v", place, names)
				}
			}
		})
	}
}

// 깨진 압축 파일은 실패하고 반쯤 푼 결과를 남기지 않는다. 체크섬은 tar 끝 표시 뒤의 압축 스트림까지 읽어야 확인된다.
func TestPackRejectsACorruptArchive(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "ok.tar.gz"), packEntry{name: "a", kind: tar.TypeReg, body: strings.Repeat("a", 4096)})
	data := packRead(t, filepath.Join(dir, "ok.tar.gz"))
	packWrite(t, filepath.Join(dir, "cut.tar.gz"), data[:len(data)-12])
	broken := []byte(data)
	broken[len(broken)-6] ^= 0xff
	packWrite(t, filepath.Join(dir, "sum.tar.gz"), string(broken))
	for _, name := range []string{"cut.tar.gz", "sum.tar.gz"} {
		if code, _, stderr := packTest(t, dir, name); code != 1 {
			t.Fatalf("%s: %d %s", name, code, stderr)
		}
		if packExists(filepath.Join(dir, strings.TrimSuffix(name, ".tar.gz"))) {
			t.Fatalf("%s left an output", name)
		}
	}
	if left := packLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

// --rm은 다 푼 뒤 압축 파일을 지운다. 디렉터리에는 쓰지 않는다. 출력이 묶는 디렉터리 안이면 거부한다.
func TestPackRemoveAndInsideOutput(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "x.tar.gz"), packEntry{name: "x/a", kind: tar.TypeReg, body: "a"})
	if code, _, stderr := packTest(t, dir, "--rm", "x.tar.gz"); code != 0 || packExists(filepath.Join(dir, "x.tar.gz")) || packRead(t, filepath.Join(dir, "x", "a")) != "a" {
		t.Fatalf("--rm: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "--rm", "x"); code != 1 || !strings.Contains(stderr, "--rm works with files only") || !packExists(filepath.Join(dir, "x", "a")) {
		t.Fatalf("--rm on a directory: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "-o", "x/self.tar.gz", "x"); code != 1 || !strings.Contains(stderr, "inside the directory") {
		t.Fatalf("output inside: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "-x", "x"); code != 1 || !strings.Contains(stderr, "not an archive") {
		t.Fatalf("-x on a directory: %d %s", code, stderr)
	}
}

// 취소하면 4로 끝나고 반쯤 쓴 출력을 지운다.
func TestPackCancelDeletesThePartialOutput(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "big", "a"), strings.Repeat("x", 1<<20))
	packTarGz(t, filepath.Join(dir, "a.tar.gz"), packEntry{name: "a/b", kind: tar.TypeReg, body: "b"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range []string{"big", "a.tar.gz", filepath.Join("big", "a")} {
		if code, _, stderr := packTestContext(t, ctx, dir, name); code != 4 || !strings.Contains(stderr, "cancelled") {
			t.Fatalf("%s: %d %s", name, code, stderr)
		}
	}
	if left := packLeftovers(t, dir); len(left) != 0 || packExists(filepath.Join(dir, "big.tar.gz")) || packExists(filepath.Join(dir, "a")) || packExists(filepath.Join(dir, "big", "a.gz")) {
		t.Fatalf("left %v", left)
	}
}

// setuid는 떼고, 같은 이름이 뒤에 다시 나오면 뒤의 항목이 이긴다(미뤄 둔 symlink도). 항목 이름의 escape는 목록에 나오지 않는다.
func TestPackExtractModesOrderAndList(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "m.tar.gz"),
		packEntry{name: "m/run", kind: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4755},
		packEntry{name: "m/a", kind: tar.TypeSymlink, link: "run"},
		packEntry{name: "m/a", kind: tar.TypeReg, body: "later"},
		packEntry{name: "m/\x1b]0;owned\x07name", kind: tar.TypeReg, body: "e"},
	)
	code, stdout, stderr := packTest(t, dir, "-l", "m.tar.gz")
	if code != 0 || strings.ContainsAny(stdout, "\x1b\x07") || !strings.Contains(stdout, "lrw-r--r--") || !strings.Contains(stdout, "m/a -> run") || !strings.Contains(stdout, "4 files, 0 directories") {
		t.Fatalf("list: %d %q %s", code, stdout, stderr)
	}
	if code, _, stderr := packTest(t, dir, "m.tar.gz"); code != 0 {
		t.Fatalf("unpack: %d %s", code, stderr)
	}
	if info, err := os.Stat(filepath.Join(dir, "m", "run")); err != nil || info.Mode()&fs.ModeSetuid != 0 || info.Mode().Perm() != 0o755 {
		t.Fatalf("setuid kept: %v %v", info.Mode(), err)
	}
	if info, err := os.Lstat(filepath.Join(dir, "m", "a")); err != nil || !info.Mode().IsRegular() || packRead(t, filepath.Join(dir, "m", "a")) != "later" {
		t.Fatalf("the later entry wins: %v %v", info, err)
	}
	gz := filepath.Join(dir, "one.gz")
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Write([]byte(strings.Repeat("z", 3000)))
	writer.Close()
	packWrite(t, gz, buffer.String())
	if code, stdout, _ := packTest(t, dir, "-l", "one.gz"); code != 0 || !strings.Contains(stdout, "one.gz: gzip, one file, 2.9 KB unpacked") {
		t.Fatalf("list one: %d %q", code, stdout)
	}
}

// 시스템 tar가 edc의 출력을 읽고, edc가 시스템 tar의 출력을 푼다. 자기 코드끼리의 왕복은 디렉터리 이름 끝의 /나 형식 표시가
// 틀려도 통과하므로 따로 본다.
func TestPackWorksWithSystemTar(t *testing.T) {
	system, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar on PATH")
	}
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "data", "a.txt"), "a")
	packWrite(t, filepath.Join(dir, "data", "deep", "b.txt"), "b")
	if code, _, stderr := packTest(t, dir, "data"); code != 0 {
		t.Fatalf("pack: %s", stderr)
	}
	listed, err := exec.Command(system, "-tzf", filepath.Join(dir, "data.tar.gz")).CombinedOutput()
	if err != nil || !strings.Contains(string(listed), "data/deep/b.txt") {
		t.Fatalf("system tar list: %v %s", err, listed)
	}
	other := t.TempDir()
	if out, err := exec.Command(system, "-xzf", filepath.Join(dir, "data.tar.gz"), "-C", other).CombinedOutput(); err != nil || packRead(t, filepath.Join(other, "data", "deep", "b.txt")) != "b" {
		t.Fatalf("system tar extract: %v %s", err, out)
	}
	source := t.TempDir()
	packWrite(t, filepath.Join(source, "made", "c.txt"), "c")
	if out, err := exec.Command(system, "-czf", filepath.Join(source, "made.tgz"), "-C", source, "made").CombinedOutput(); err != nil {
		t.Fatalf("system tar create: %v %s", err, out)
	}
	if out, err := exec.Command(system, "-cf", filepath.Join(source, "plain.tar"), "-C", source, "made").CombinedOutput(); err != nil {
		t.Fatalf("system tar create plain: %v %s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(source, "made")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := packTest(t, source, "made.tgz"); code != 0 || packRead(t, filepath.Join(source, "made", "c.txt")) != "c" {
		t.Fatalf("edc extracts system tar.gz: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, source, "-o", "plain", "plain.tar"); code != 0 || packRead(t, filepath.Join(source, "plain", "made", "c.txt")) != "c" {
		t.Fatalf("edc extracts a plain tar: %d %s", code, stderr)
	}
}

// macOS tar가 넣는 AppleDouble 항목(._이름)은 건너뛴다. 이름만 "._"로 시작하는 보통 파일은 푼다.
func TestPackSkipsAppleDoubleEntries(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "mac.tgz"),
		packEntry{name: "._made", kind: tar.TypeReg, body: "\x00\x05\x16\x07rest"},
		packEntry{name: "made/", kind: tar.TypeDir, mode: 0o755},
		packEntry{name: "made/._note", kind: tar.TypeReg, body: "plain text"},
		packEntry{name: "made/c.txt", kind: tar.TypeReg, body: "c"},
	)
	code, _, stderr := packTest(t, dir, "mac.tgz")
	if code != 0 || !strings.Contains(stderr, "mac.tgz → made/") || !strings.Contains(stderr, "1 special files skipped") {
		t.Fatalf("%d %s", code, stderr)
	}
	if packRead(t, filepath.Join(dir, "made", "._note")) != "plain text" || packExists(filepath.Join(dir, "._made")) {
		t.Fatal("a plain ._ file stays, the AppleDouble entry goes")
	}
}

// 최상위가 디렉터리 하나여도 그 안의 링크가 최상위 밖(풀어 놓을 디렉터리의 이웃)을 가리키면, 꺼내 옮기지 않고 압축 파일 이름의
// 디렉터리로 감싸 푼다. 그래야 링크가 풀린 결과 안을 가리킨다.
func TestPackWrapsATopWithAnEscapingLink(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "neighbor"), "secret")
	packTarGz(t, filepath.Join(dir, "release.tgz"),
		packEntry{name: "top/", kind: tar.TypeDir, mode: 0o755},
		packEntry{name: "top/peek", kind: tar.TypeSymlink, link: "../neighbor"},
		packEntry{name: "top/up", kind: tar.TypeSymlink, link: ".."},
	)
	code, _, stderr := packTest(t, dir, "release.tgz")
	if code != 0 || !strings.Contains(stderr, "release.tgz → release/") {
		t.Fatalf("%d %s", code, stderr)
	}
	if packExists(filepath.Join(dir, "top")) {
		t.Fatal("top must not move out on its own")
	}
	if _, err := os.ReadFile(filepath.Join(dir, "release", "top", "peek")); err == nil {
		t.Fatal("peek must not reach the neighbor")
	}
}

// --zstd로 묶은 .tar.zst를 판별해 푼다.
func TestPackZstdTarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "logs", "a.log"), strings.Repeat("a", 5000))
	if code, _, stderr := packTest(t, dir, "--zstd", "logs"); code != 0 || !strings.Contains(stderr, "logs/ → logs.tar.zst") {
		t.Fatalf("pack: %d %s", code, stderr)
	}
	if kind, err := packDetect(filepath.Join(dir, "logs.tar.zst")); err != nil || kind.format != "zstd" || !kind.tar {
		t.Fatalf("kind %+v %v", kind, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := packTest(t, dir, "logs.tar.zst"); code != 0 || packRead(t, filepath.Join(dir, "logs", "a.log")) != strings.Repeat("a", 5000) {
		t.Fatalf("unpack: %d %s", code, stderr)
	}
}

// 건너뛴 항목이 있으면 --rm은 압축 파일을 남기고 1로 끝난다. 겹친 이름은 오류 문구에 나온다.
func TestPackRemoveKeepsAnArchiveWithSkippedEntries(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "dev.tgz"), packEntry{name: "d/a", kind: tar.TypeReg, body: "a"}, packEntry{name: "d/fifo", kind: tar.TypeFifo})
	code, _, stderr := packTest(t, dir, "--rm", "dev.tgz")
	if code != 1 || !strings.Contains(stderr, "1 entries were skipped, so edc kept the archive") || !packExists(filepath.Join(dir, "dev.tgz")) || packRead(t, filepath.Join(dir, "d", "a")) != "a" {
		t.Fatalf("%d %s", code, stderr)
	}
	packTarGz(t, filepath.Join(dir, "release.tgz"), packEntry{name: "d/b", kind: tar.TypeReg, body: "b"})
	if code, _, stderr := packTest(t, dir, "release.tgz"); code != 1 || !strings.Contains(stderr, "d exists; delete it or give -o") {
		t.Fatalf("named exists: %d %s", code, stderr)
	}
}

// -v는 판단 줄과 항목마다 한 줄을 쓴다. -v가 없으면 결과 줄만 쓴다.
func TestPackVerboseLines(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "logs", "a.log"), "aaa")
	if err := os.Symlink("a.log", filepath.Join(dir, "logs", "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "logs", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := packTest(t, dir, "-v", "logs")
	for _, want := range []string{"logs: directory → pack\n", "+ logs/\n", "+ logs/a.log  3 B\n", "+ logs/link -> a.log\n", "- logs/pipe (FIFO)\n", "logs/ → logs.tar.gz"} {
		if code != 0 || !strings.Contains(stderr, want) {
			t.Fatalf("pack -v misses %q: %d\n%s", want, code, stderr)
		}
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "logs.tar.gz"), filepath.Join(out, "logs.tar.gz")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = packTest(t, out, "-v", "logs.tar.gz")
	for _, want := range []string{"logs.tar.gz: gzip tar → extract\n", "x logs/\n", "x logs/a.log  3 B\n", "x logs/link -> a.log\n"} {
		if code != 0 || !strings.Contains(stderr, want) {
			t.Fatalf("unpack -v misses %q: %d\n%s", want, code, stderr)
		}
	}
	packWrite(t, filepath.Join(dir, "plain.txt"), "p")
	if code, _, stderr := packTest(t, dir, "-v", "-x", "plain.txt"); code != 1 || !strings.Contains(stderr, "plain.txt: not compressed → extract (-x)") {
		t.Fatalf("forced decision: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "plain.txt"); code != 0 || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("without -v only the result line: %d %q", code, stderr)
	}
}

// 터미널이면 진행 줄을 고쳐 쓰고, 항목 줄을 쓸 때 지웠다가 다시 그린다. 끝나면 지운다.
func TestPackReporterProgressLine(t *testing.T) {
	var out bytes.Buffer
	reporter := &packReporter{out: &out, verbose: true, live: true}
	var read atomic.Int64
	read.Store(512)
	stop := reporter.track("big.log", 2048, &read, nil)
	time.Sleep(3 * packProgressInterval / 2)
	reporter.line("+ entry")
	stop()
	text := out.String()
	if !strings.Contains(text, "big.log  512 B / 2.0 KB (25%)") || !strings.Contains(text, "\r\033[K+ entry\n") || !strings.HasSuffix(text, "\r\033[K") {
		t.Fatalf("progress %q", text)
	}
	quiet := &packReporter{out: &out, verbose: true}
	out.Reset()
	quiet.track("big.log", 2048, &read, nil)()
	if out.Len() != 0 {
		t.Fatalf("no progress line outside a terminal: %q", out.String())
	}
}

// -o의 부모가 symlink로 묶는 디렉터리를 가리켜도 안쪽 출력으로 본다.
func TestPackRejectsAnOutputThroughALinkIntoTheDirectory(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "logs", "a.log"), "a")
	if err := os.Symlink("logs", filepath.Join(dir, "backup-link")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := packTest(t, dir, "-o", "backup-link/snapshot.tar.gz", "logs"); code != 1 || !strings.Contains(stderr, "inside the directory") {
		t.Fatalf("%d %s", code, stderr)
	}
	if packExists(filepath.Join(dir, "logs", "snapshot.tar.gz")) || len(packLeftovers(t, filepath.Join(dir, "logs"))) != 0 {
		t.Fatal("nothing is written inside the input")
	}
}

// 같은 이름의 디렉터리 항목을 뒤의 파일이나 symlink가 대체하면 옛 디렉터리의 권한과 시각을 적용하지 않는다. 같은 디렉터리가
// 두 번 나오면 뒤의 헤더가 이긴다.
func TestPackLaterEntriesWinOverDirectoryHeaders(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "dup.tgz"),
		packEntry{name: "t/", kind: tar.TypeDir, mode: 0o755},
		packEntry{name: "t/d/", kind: tar.TypeDir, mode: 0o700},
		packEntry{name: "t/d", kind: tar.TypeReg, body: "file", mode: 0o644},
		packEntry{name: "t/run", kind: tar.TypeReg, body: "#!", mode: 0o644},
		packEntry{name: "t/s/", kind: tar.TypeDir, mode: 0o700},
		packEntry{name: "t/s", kind: tar.TypeSymlink, link: "run"},
		packEntry{name: "t/twice/", kind: tar.TypeDir, mode: 0o700},
		packEntry{name: "t/twice/", kind: tar.TypeDir, mode: 0o750},
	)
	if code, _, stderr := packTest(t, dir, "dup.tgz"); code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	for name, want := range map[string]fs.FileMode{"t/d": 0o644, "t/run": 0o644, "t/twice": 0o750} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v (%v)", name, info.Mode().Perm(), want, err)
		}
	}
	if link, err := os.Readlink(filepath.Join(dir, "t", "s")); err != nil || link != "run" {
		t.Fatalf("symlink %q %v", link, err)
	}
}

// ustar 표시가 없는 v7 tar와 끝 표시만 있는 빈 tar도 tar다. 0으로만 찬 큰 파일은 tar가 아니다.
func TestPackDetectsV7AndEmptyTar(t *testing.T) {
	dir := t.TempDir()
	header := make([]byte, 512)
	copy(header, "v7/a.txt")
	copy(header[100:], "0000644\x00")
	copy(header[108:], "0000000\x00")
	copy(header[116:], "0000000\x00")
	copy(header[124:], "00000000003\x00")
	copy(header[136:], "14760000000\x00")
	header[156] = '0'
	copy(header[148:], "        ")
	sum := 0
	for _, value := range header {
		sum += int(value)
	}
	copy(header[148:], fmt.Sprintf("%06o\x00 ", sum))
	data := append(header, append([]byte("abc"), make([]byte, 509+1024)...)...)
	packWrite(t, filepath.Join(dir, "old.tar"), string(data))
	if kind, err := packDetect(filepath.Join(dir, "old.tar")); err != nil || !kind.tar {
		t.Fatalf("v7 kind %+v %v", kind, err)
	}
	if code, _, stderr := packTest(t, dir, "-o", "old", "old.tar"); code != 0 || packRead(t, filepath.Join(dir, "old", "v7", "a.txt")) != "abc" {
		t.Fatalf("v7 extract: %d %s", code, stderr)
	}
	packWrite(t, filepath.Join(dir, "empty.tar"), string(make([]byte, 10240)))
	if code, _, stderr := packTest(t, dir, "empty.tar"); code != 0 || !strings.Contains(stderr, "empty.tar → empty/") {
		t.Fatalf("empty tar: %d %s", code, stderr)
	}
	packWrite(t, filepath.Join(dir, "zeros.img"), string(make([]byte, 20480)))
	if kind, err := packDetect(filepath.Join(dir, "zeros.img")); err != nil || kind.tar {
		t.Fatalf("a large zero file is not a tar: %+v %v", kind, err)
	}
}

// 임시 출력을 만든 뒤에 취소해도 4로 끝나고 그 출력을 지운다.
func TestPackCancelAfterTheOutputStarted(t *testing.T) {
	defer func() { packReadHook = nil }()
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "big", "a"), strings.Repeat("x", 4<<20))
	packWrite(t, filepath.Join(dir, "single.log"), strings.Repeat("y", 4<<20))
	packTarGz(t, filepath.Join(dir, "a.tar.gz"), packEntry{name: "a/b", kind: tar.TypeReg, body: strings.Repeat("b", 1<<20)}, packEntry{name: "a/c", kind: tar.TypeReg, body: "c"})
	for _, name := range []string{"big", "single.log", "a.tar.gz"} {
		ctx, cancel := context.WithCancel(context.Background())
		reads := 0
		packReadHook = func() {
			if reads++; reads == 2 {
				cancel()
			}
		}
		if code, _, stderr := packTestContext(t, ctx, dir, name); code != 4 || reads < 2 {
			t.Fatalf("%s: %d reads %d %s", name, code, reads, stderr)
		}
		cancel()
	}
	if left := packLeftovers(t, dir); len(left) != 0 || packExists(filepath.Join(dir, "big.tar.gz")) || packExists(filepath.Join(dir, "single.log.gz")) || packExists(filepath.Join(dir, "a")) {
		t.Fatalf("left %v", left)
	}
}
