package edc

import (
	"bytes"
	stdgzip "compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/x-mesh/fastcompress/zstd"
)

func codecCommand(t *testing.T, run func([]string, *os.File, *os.File, *os.File) int, stdin []byte, args ...string) (int, []byte, string) {
	t.Helper()
	dir := t.TempDir()
	open := func(name string, content []byte) *os.File {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	in, out, errOut := open("stdin", stdin), open("stdout", nil), open("stderr", nil)
	defer in.Close()
	defer out.Close()
	defer errOut.Close()
	code := run(args, in, out, errOut)
	stdout, _ := os.ReadFile(out.Name())
	stderr, _ := os.ReadFile(errOut.Name())
	return code, stdout, string(stderr)
}

// codecLog는 병렬 gzip이 여러 조각으로 나눌 만큼 큰 로그다.
func codecLog() []byte {
	var content bytes.Buffer
	for index := range 60000 {
		content.WriteString("2026-11-04 10:00:00 INFO request " + strconv.Itoa(index) + " served in 12ms\n")
	}
	return content.Bytes()
}

func codecWrite(t *testing.T, path string, content []byte, mode os.FileMode, modified time.Time) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func codecUnpack(t *testing.T, data []byte) []byte {
	t.Helper()
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		reader, err := stdgzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	plain, err := zstd.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

// 압축하고 다시 풀면 같은 바이트다. 출력은 입력의 권한과 수정 시각을 이어받고 입력은 남는다.
func TestCompressRoundTripsGzipAndZstd(t *testing.T) {
	content := codecLog()
	modified := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	for _, test := range []struct{ suffix, flag string }{{".gz", ""}, {".zst", "--zstd"}} {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, content, 0o640, modified)
		args := []string{path}
		if test.flag != "" {
			args = []string{test.flag, path}
		}
		code, _, stderr := codecCommand(t, compressRun, nil, args...)
		target := path + test.suffix
		if code != 0 || !strings.Contains(stderr, path+" → "+target) {
			t.Fatalf("%s: code %d stderr %q", test.suffix, code, stderr)
		}
		packed, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(codecUnpack(t, packed), content) || len(packed) >= len(content)/4 {
			t.Fatalf("%s: %d bytes do not hold the input", test.suffix, len(packed))
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 || !info.ModTime().Equal(modified) {
			t.Fatalf("%s: mode %v modified %v", test.suffix, info.Mode().Perm(), info.ModTime())
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: the input is gone: %v", test.suffix, err)
		}
		os.Remove(path)
		code, _, stderr = codecCommand(t, decompressRun, nil, target)
		plain, err := os.ReadFile(path)
		if code != 0 || err != nil || !bytes.Equal(plain, content) {
			t.Fatalf("%s: decompress code %d err %v stderr %q", test.suffix, code, err, stderr)
		}
		if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(modified) {
			t.Fatalf("%s: decompressed modified %v err %v", test.suffix, info.ModTime(), err)
		}
	}
}

// 병렬 gzip의 출력은 goroutine 수와 관계없이 같다. -j 1은 한 스트림으로 압축하므로 조각 경계만큼 다를 수 있다.
func TestCompressGzipJobsGiveTheSameBytes(t *testing.T) {
	content := codecLog()
	var outputs [][]byte
	for _, jobs := range []string{"1", "2", "8"} {
		code, stdout, stderr := codecCommand(t, compressRun, content, "-j", jobs)
		if code != 0 || !bytes.Equal(codecUnpack(t, stdout), content) {
			t.Fatalf("-j %s: code %d stderr %q", jobs, code, stderr)
		}
		outputs = append(outputs, stdout)
	}
	if !bytes.Equal(outputs[1], outputs[2]) {
		t.Fatal("-j 2 and -j 8 wrote different bytes")
	}
}

// 이미 있는 출력은 덮어쓰지 않는다. 이미 압축된 파일과 심볼릭 링크는 압축하지 않는다.
func TestCompressRefusesToOverwriteOrRecompress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now())
	codecWrite(t, path+".gz", []byte("keep"), 0o644, time.Now())
	code, _, stderr := codecCommand(t, compressRun, nil, path)
	if kept, _ := os.ReadFile(path + ".gz"); code != 1 || string(kept) != "keep" || !strings.Contains(stderr, T("compress.error.exists")) {
		t.Fatalf("code %d kept %q stderr %q", code, kept, stderr)
	}
	code, _, stderr = codecCommand(t, compressRun, nil, path+".gz")
	if code != 1 || !strings.Contains(stderr, T("compress.error.suffix")) {
		t.Fatalf("recompress: code %d stderr %q", code, stderr)
	}
	link := filepath.Join(dir, "current.log")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = codecCommand(t, compressRun, nil, "--rm", link)
	if _, err := os.Lstat(link); code != 1 || err != nil || !strings.Contains(stderr, T("compress.error.not_regular")) {
		t.Fatalf("symlink: code %d err %v stderr %q", code, err, stderr)
	}
}

// --rm은 출력을 다 쓴 뒤에만 입력을 지운다. 잘린 입력을 풀다 실패하면 만들던 출력을 지우고 입력은 남긴다.
func TestCodecRemoveOnlyAfterACompleteOutput(t *testing.T) {
	content := codecLog()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	codecWrite(t, path, content, 0o644, time.Now())
	code, _, stderr := codecCommand(t, compressRun, nil, "--rm", "--zstd", path)
	if _, err := os.Stat(path); code != 0 || err == nil {
		t.Fatalf("code %d stderr %q, the input is still there: %v", code, stderr, err)
	}
	if held, _ := filepath.Glob(filepath.Join(dir, ".*.rm")); len(held) != 0 {
		t.Fatalf("left %v", held)
	}
	packed, err := os.ReadFile(path + ".zst")
	if err != nil || !bytes.Equal(codecUnpack(t, packed), content) {
		t.Fatalf("output err %v", err)
	}
	for _, name := range []string{"cut.log.zst", "cut.log.gz"} {
		data := packed
		if strings.HasSuffix(name, ".gz") {
			var buffer bytes.Buffer
			writer := stdgzip.NewWriter(&buffer)
			writer.Write(content)
			writer.Close()
			data = buffer.Bytes()
		}
		cut := filepath.Join(dir, name)
		codecWrite(t, cut, data[:len(data)/2], 0o644, time.Now())
		code, _, stderr := codecCommand(t, decompressRun, nil, "--rm", cut)
		if code != 1 || stderr == "" {
			t.Fatalf("%s: code %d stderr %q", name, code, stderr)
		}
		if _, err := os.Stat(strings.TrimSuffix(cut, filepath.Ext(cut))); err == nil {
			t.Fatalf("%s: the partial output is left", name)
		}
		if _, err := os.Stat(cut); err != nil {
			t.Fatalf("%s: the input is gone: %v", name, err)
		}
	}
}

// Lstat으로 본 뒤 경로가 심볼릭 링크, 다른 파일, FIFO로 바뀌면 열지 않는다. FIFO를 열 때 쓰는 쪽을 기다리지 않는다.
func TestCodecOpenRefusesAPathSwappedAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now())
	checked, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.log")
	codecWrite(t, other, []byte("other\n"), 0o644, time.Now())
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, swapped := range []string{link, other, fifo} {
		done := make(chan error, 1)
		go func() {
			file, _, err := codecOpenChecked(swapped, checked)
			if file != nil {
				file.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, errCodecNotRegular) {
				t.Fatalf("%s: err %v", filepath.Base(swapped), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: open waits", filepath.Base(swapped))
		}
	}
	file, _, err := codecOpenChecked(path, checked)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

// 변환하는 동안 출력 이름은 비어 있고, 끝나면 임시 파일이 남지 않는다. 그 사이에 출력 이름이 생기면 덮어쓰지 않는다.
func TestCodecWritesUnderATemporaryName(t *testing.T) {
	for _, test := range []struct {
		name string
		fail bool
		race bool
	}{{"done", false, false}, {"failed", true, false}, {"raced", false, true}} {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log")
		codecWrite(t, path, []byte("line\n"), 0o640, time.Now())
		target := path + ".out"
		options := codecOptions{
			command: "compress",
			target:  func(string) (string, error) { return target, nil },
			convert: func(dst io.Writer, src io.Reader) error {
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s: the output name exists during the run: %v", test.name, err)
				}
				if test.race {
					os.WriteFile(target, []byte("keep"), 0o644)
				}
				if _, err := io.Copy(dst, src); err != nil {
					return err
				}
				if test.fail {
					return io.ErrUnexpectedEOF
				}
				return nil
			},
		}
		_, err := codecPath(path, options, false)
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		want := []string{"app.log", "app.log.out"}
		switch {
		case test.fail:
			want = []string{"app.log"}
			if err == nil {
				t.Fatalf("%s: no error", test.name)
			}
		case test.race:
			if kept, _ := os.ReadFile(target); !errors.Is(err, errCodecExists) || string(kept) != "keep" {
				t.Fatalf("%s: err %v kept %q", test.name, err, kept)
			}
		case err != nil:
			t.Fatalf("%s: %v", test.name, err)
		}
		if strings.Join(names, " ") != strings.Join(want, " ") {
			t.Fatalf("%s: files %v", test.name, names)
		}
	}
}

// --rm은 변환하는 동안 입력이 더 쓰였거나, 같은 크기로 고쳐 쓰였거나, 경로가 다른 파일로 바뀌었으면 지우지 않는다. 출력은 읽은 만큼 남는다.
func TestCodecRemoveKeepsAChangedInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(path string)
	}{
		{"appended", func(path string) {
			file, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			file.WriteString("late line\n")
			file.Close()
		}},
		{"rewritten", func(path string) {
			file, _ := os.OpenFile(path, os.O_WRONLY, 0)
			file.WriteAt([]byte("LINE\n"), 0)
			file.Close()
			// 파일 시스템의 시각 단위와 관계없이 수정 시각만 달라지게 한다.
			later := time.Now().Add(time.Hour)
			os.Chtimes(path, later, later)
		}},
		{"replaced", func(path string) {
			os.Rename(path, path+".old")
			os.WriteFile(path, []byte("new log\n"), 0o644)
		}},
		{"rewritten with the old mtime", func(path string) {
			before, _ := os.Stat(path)
			file, _ := os.OpenFile(path, os.O_WRONLY, 0)
			file.WriteAt([]byte("LINE\n"), 0)
			file.Close()
			os.Chtimes(path, before.ModTime(), before.ModTime())
		}},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		options := codecOptions{
			command: "compress",
			remove:  true,
			target:  func(path string) (string, error) { return path + ".gz", nil },
			convert: func(dst io.Writer, src io.Reader) error {
				_, err := io.Copy(dst, src)
				test.change(path)
				return err
			},
		}
		_, err := codecPath(path, options, false)
		if !errors.Is(err, errCodecChanged) || codecErrorText(err) != T("compress.error.changed") {
			t.Fatalf("%s: err %v", test.name, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: the input is gone: %v", test.name, err)
		}
		if packed, err := os.ReadFile(path + ".gz"); err != nil || string(packed) != "line\n" {
			t.Fatalf("%s: output %q err %v", test.name, packed, err)
		}
		if held, _ := filepath.Glob(filepath.Join(dir, ".*.rm")); len(held) != 0 {
			t.Fatalf("%s: left %v", test.name, held)
		}
	}
}

func codecRemoveOptions() codecOptions {
	return codecOptions{
		command: "compress",
		remove:  true,
		target:  func(path string) (string, error) { return path + ".gz", nil },
		convert: func(dst io.Writer, src io.Reader) error {
			_, err := io.Copy(dst, src)
			return err
		},
	}
}

// 마지막 확인 뒤 지우기 전에 다른 파일이 경로를 차지해도 그 파일을 지우지 않고 원래 이름에 남긴다.
func TestCodecRemoveKeepsAFileThatTakesThePathBeforeTheDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	codecRemoveHook = func(stage string) {
		if stage == "move" {
			os.Rename(path, path+".old")
			os.WriteFile(path, []byte("new log\n"), 0o644)
		}
	}
	t.Cleanup(func() { codecRemoveHook = nil })
	_, err := codecPath(path, codecRemoveOptions(), false)
	if !errors.Is(err, errCodecChanged) {
		t.Fatalf("err %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new log\n" {
		t.Fatalf("path holds %q err %v", data, err)
	}
	if data, err := os.ReadFile(path + ".old"); err != nil || string(data) != "line\n" {
		t.Fatalf("the converted input holds %q err %v", data, err)
	}
	if held, _ := filepath.Glob(filepath.Join(dir, ".*.rm")); len(held) != 0 {
		t.Fatalf("left %v", held)
	}
}

// 크기와 수정 시각을 본 뒤 옮기기 전에 같은 크기로 고쳐 쓰고 수정 시각을 되돌려도, 옮긴 뒤 다시 읽어 지우지 않는다.
func TestCodecRemoveKeepsAnInputRewrittenBeforeTheMove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	modified := time.Now().Add(-time.Hour)
	codecWrite(t, path, []byte("line\n"), 0o644, modified)
	codecRemoveHook = func(stage string) {
		if stage == "move" {
			file, _ := os.OpenFile(path, os.O_WRONLY, 0)
			file.WriteAt([]byte("LINE\n"), 0)
			file.Close()
			os.Chtimes(path, modified, modified)
		}
	}
	t.Cleanup(func() { codecRemoveHook = nil })
	_, err := codecPath(path, codecRemoveOptions(), false)
	if !errors.Is(err, errCodecChanged) {
		t.Fatalf("err %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "LINE\n" {
		t.Fatalf("path holds %q err %v", data, err)
	}
	if held, _ := filepath.Glob(filepath.Join(dir, ".*.rm")); len(held) != 0 {
		t.Fatalf("left %v", held)
	}
}

// 되돌리는 사이에 또 다른 파일이 원래 이름을 차지하면 덮어쓰지 않는다. 옮긴 파일은 .rm 디렉터리에 남고 오류가 두 경로를 알린다.
func TestCodecRemoveDoesNotOverwriteANameTakenDuringTheRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	codecRemoveHook = func(stage string) {
		switch stage {
		case "move":
			os.Rename(path, path+".old")
			os.WriteFile(path, []byte("new log\n"), 0o644)
		case "restore":
			os.WriteFile(path, []byte("newer log\n"), 0o644)
		}
	}
	t.Cleanup(func() { codecRemoveHook = nil })
	_, err := codecPath(path, codecRemoveOptions(), false)
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "newer log\n" {
		t.Fatalf("path holds %q err %v", data, err)
	}
	held, _ := filepath.Glob(filepath.Join(dir, ".*.rm", "app.log.1"))
	if len(held) != 1 || linkErr.Old != held[0] {
		t.Fatalf("held %v, error names %q", held, linkErr.Old)
	}
	if data, err := os.ReadFile(held[0]); err != nil || string(data) != "new log\n" {
		t.Fatalf("the moved file holds %q err %v", data, err)
	}
}

// 상대 경로의 입력도 입력과 같은 디렉터리로 옮긴다. 시스템 임시 디렉터리는 다른 파일 시스템일 수 있어 rename이 실패한다.
func TestCodecRemoveMovesARelativeInputBesideIt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", filepath.Join(dir, "missing"))
	codecWrite(t, "app.log.1", []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	if _, err := codecPath("app.log.1", codecRemoveOptions(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat("app.log.1"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the input is still there: %v", err)
	}
	if held, _ := filepath.Glob(".*.rm"); len(held) != 0 {
		t.Fatalf("left %v", held)
	}
}

// 형식은 이름이 아니라 앞 바이트로 고른다. 이름으로 출력을 정할 수 없으면 -o를 받는다.
func TestDecompressPicksTheFormatByMagic(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup")
	codecWrite(t, backup, zstd.Encode(nil, []byte("rows\n"), 3), 0o644, time.Now())
	code, _, stderr := codecCommand(t, decompressRun, nil, backup)
	if code != 1 || !strings.Contains(stderr, T("compress.error.no_name")) {
		t.Fatalf("no name: code %d stderr %q", code, stderr)
	}
	out := filepath.Join(dir, "rows.txt")
	code, _, stderr = codecCommand(t, decompressRun, nil, "-o", out, backup)
	if plain, _ := os.ReadFile(out); code != 0 || string(plain) != "rows\n" {
		t.Fatalf("-o: code %d plain %q stderr %q", code, plain, stderr)
	}
	renamed := filepath.Join(dir, "rows.gz")
	codecWrite(t, renamed, zstd.Encode(nil, []byte("zstd under a gz name\n"), 3), 0o644, time.Now())
	code, stdout, stderr := codecCommand(t, decompressRun, nil, "-c", renamed)
	if code != 0 || string(stdout) != "zstd under a gz name\n" || stderr != "" {
		t.Fatalf("-c: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	plain := filepath.Join(dir, "plain.gz")
	codecWrite(t, plain, []byte("not compressed"), 0o644, time.Now())
	code, _, stderr = codecCommand(t, decompressRun, nil, plain)
	if code != 1 || !strings.Contains(stderr, T("compress.error.unknown_format")) {
		t.Fatalf("plain: code %d stderr %q", code, stderr)
	}
}

func TestCodecPipesStdinToStdout(t *testing.T) {
	content := codecLog()
	for _, flag := range []string{"-l", "--zstd"} {
		args := []string{"-l", "1"}
		if flag == "--zstd" {
			args = []string{"--zstd"}
		}
		code, packed, stderr := codecCommand(t, compressRun, content, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%s: code %d stderr %q", flag, code, stderr)
		}
		code, plain, stderr := codecCommand(t, decompressRun, packed)
		if code != 0 || stderr != "" || !bytes.Equal(plain, content) {
			t.Fatalf("%s: decompress code %d stderr %q", flag, code, stderr)
		}
	}
}

func TestCodecRejectsConflictingOptions(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")
	codecWrite(t, a, []byte("a\n"), 0o644, time.Now())
	codecWrite(t, b, []byte("b\n"), 0o644, time.Now())
	for _, args := range [][]string{
		{"-c", "-o", filepath.Join(dir, "x.gz"), a},
		{"-o", filepath.Join(dir, "x.gz"), a, b},
		{"--rm"},
		{"--rm", "-c", a},
		{"-l", "10", a},
		{"-j", "0", a},
	} {
		if code, _, _ := codecCommand(t, compressRun, nil, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("a usage error wrote files: %v", entries)
	}
}

// 시스템 gzip과 zstd가 edc의 출력을 읽고, edc가 그들의 출력을 읽는다.
func TestCodecReadsAndWritesWhatTheSystemToolsDo(t *testing.T) {
	content := codecLog()
	for _, test := range []struct{ tool, suffix, flag string }{{"gzip", ".gz", ""}, {"zstd", ".zst", "--zstd"}} {
		tool, err := exec.LookPath(test.tool)
		if err != nil {
			t.Logf("%s is not installed", test.tool)
			continue
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log")
		codecWrite(t, path, content, 0o644, time.Now())
		args := []string{path}
		if test.flag != "" {
			args = []string{test.flag, path}
		}
		if code, _, stderr := codecCommand(t, compressRun, nil, args...); code != 0 {
			t.Fatalf("%s: code %d stderr %q", test.tool, code, stderr)
		}
		if output, err := exec.Command(tool, "-t", path+test.suffix).CombinedOutput(); err != nil {
			t.Fatalf("%s -t: %v %s", test.tool, err, output)
		}
		other := filepath.Join(dir, "other.log")
		codecWrite(t, other, content, 0o644, time.Now())
		if output, err := exec.Command(tool, "-q", other).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", test.tool, err, output)
		}
		code, plain, stderr := codecCommand(t, decompressRun, nil, "-c", other+test.suffix)
		if code != 0 || !bytes.Equal(plain, content) {
			t.Fatalf("%s output: code %d stderr %q", test.tool, code, stderr)
		}
	}
}
