package edc

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type catTestRun struct {
	stdout, stderr bytes.Buffer
	ok             bool
}

func runCatTest(t *testing.T, output catOutput, options catOptions, paths ...string) *catTestRun {
	t.Helper()
	run := &catTestRun{}
	writer := bufio.NewWriter(&run.stdout)
	output.stdout, output.stderr = writer, &run.stderr
	run.ok = catPaths(&output, paths, options)
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	return run
}

func writeCatFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// pipe에서는 위치 줄, 색, escape 제거가 없어야 `edc cat a b > out`이 원본을 이어 붙인 파일이 된다.
func TestCatPipeWritesBytesUnchanged(t *testing.T) {
	files := map[string]string{".env": "A=1\r\nB=\x1b[31m2", "blob.bin": "a\x00b", "note.txt": "last line"}
	root := writeCatFiles(t, files)
	run := runCatTest(t, catOutput{}, catOptions{}, filepath.Join(root, ".env"), filepath.Join(root, "blob.bin"), filepath.Join(root, "note.txt"))
	if want := files[".env"] + files["blob.bin"] + files["note.txt"]; !run.ok || run.stdout.String() != want {
		t.Fatalf("ok=%v stdout=%q, want %q", run.ok, run.stdout.String(), want)
	}
}

// 터미널에서는 NO_COLOR여도 위치 줄로 파일을 구분하고, 내용의 escape를 지우며 tab을 4칸 경계에 맞춘다.
func TestCatTerminalWritesPathLinesWithoutColor(t *testing.T) {
	root := writeCatFiles(t, map[string]string{".env": "A=1\nB=\x1b[31mred\x1b[0m\tx", "empty.txt": "", "note.txt": "text\n"})
	env, empty, note := filepath.Join(root, ".env"), filepath.Join(root, "empty.txt"), filepath.Join(root, "note.txt")
	run := runCatTest(t, catOutput{terminal: true}, catOptions{}, env, empty, note)
	want := "==> " + env + " (dotenv · 20 B)\nA=1\nB=red   x\n\n==> " + empty + " (0 B)\n\n==> " + note + " (5 B)\ntext\n"
	if !run.ok || run.stdout.String() != want {
		t.Fatalf("ok=%v stdout:\n%q\nwant:\n%q", run.ok, run.stdout.String(), want)
	}
}

func TestCatTerminalColorsConfigFiles(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"app.toml": "# note\n[owner]\nname = \"Tom\"\nage = 3\n"})
	path := filepath.Join(root, "app.toml")
	run := runCatTest(t, catOutput{terminal: true, color: true}, catOptions{}, path)
	want := richDim("==>") + " " + richBold(path) + richDim(" (toml · 36 B)") + "\n" +
		richDim("# note") + "\n" +
		richPaint("[owner]", richMagenta, true) + "\n" +
		richPaint("name", richCyan, false) + " = " + richPaint(`"Tom"`, richGreen, false) + "\n" +
		richPaint("age", richCyan, false) + " = " + richPaint("3", richYellow, false) + "\n"
	if run.stdout.String() != want {
		t.Fatalf("stdout:\n%q\nwant:\n%q", run.stdout.String(), want)
	}
}

func TestCatTerminalOmitsBinaryContent(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"blob.bin": "ab\x00cd"})
	path := filepath.Join(root, "blob.bin")
	run := runCatTest(t, catOutput{terminal: true}, catOptions{}, path)
	if want := "==> " + path + " (binary · 5 B · " + T("cat.binary_omitted") + ")\n"; !run.ok || run.stdout.String() != want {
		t.Fatalf("ok=%v stdout=%q, want %q", run.ok, run.stdout.String(), want)
	}
}

func TestCatDirectoryFiles(t *testing.T) {
	root := writeCatFiles(t, map[string]string{
		".env": "A=1\n", ".gitignore": "ignored.txt\n", "b.txt": "b\n", "a.txt": "a\n", "ignored.txt": "i\n",
		"sub/c.txt": "c\n", "sub/.hidden": "h\n", ".git/config": "[core]\n",
	})
	if err := os.Symlink(filepath.Join(root, "sub", "c.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken.txt")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options catOptions
		want    []string
	}{
		{"direct files, with the file a symlink names", catOptions{}, []string{"a.txt", "b.txt", "ignored.txt", "link.txt"}},
		{"hidden files", catOptions{hidden: true}, []string{".env", ".gitignore", "a.txt", "b.txt", "ignored.txt", "link.txt"}},
		{"recursive applies .gitignore", catOptions{recursive: true}, []string{"a.txt", "b.txt", "sub/c.txt"}},
		{"recursive with hidden files", catOptions{recursive: true, hidden: true}, []string{".env", ".gitignore", "a.txt", "b.txt", "sub/.hidden", "sub/c.txt"}},
		{"recursive without .gitignore", catOptions{recursive: true, noIgnore: true}, []string{"a.txt", "b.txt", "ignored.txt", "sub/c.txt"}},
	} {
		files, problems := catDirectoryFiles(root, test.options)
		if len(problems) > 0 {
			t.Errorf("%s: problems %v", test.name, problems)
		}
		var got []string
		for _, file := range files {
			relative, err := filepath.Rel(root, file)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, filepath.ToSlash(relative))
		}
		if strings.Join(got, " ") != strings.Join(test.want, " ") {
			t.Errorf("%s:\n got %v\nwant %v", test.name, got, test.want)
		}
	}
}

func TestCatDirectoryWithoutFilesSaysHowToIncludeMore(t *testing.T) {
	root := writeCatFiles(t, map[string]string{".env": "A=1\n", "sub/c.txt": "c\n"})
	run := runCatTest(t, catOutput{terminal: true}, catOptions{}, root)
	if want := T("cat.no_files", root) + "\n"; !run.ok || run.stdout.Len() != 0 || run.stderr.String() != want {
		t.Fatalf("ok=%v stdout=%q stderr=%q, want stderr %q", run.ok, run.stdout.String(), run.stderr.String(), want)
	}
}

// 디렉터리에서 읽은 파일 이름이 stderr로 나가므로, 이름에 든 escape가 터미널을 움직이면 안 된다.
func TestCatProblemLinesHoldNoEscape(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"a\x1b[2Jb.bin": "s3cr3t\n"})
	run := runCatTest(t, catOutput{terminal: true, mask: true}, catOptions{}, root)
	if run.ok || strings.Contains(run.stderr.String(), "\x1b") || !strings.Contains(run.stderr.String(), "ab.bin") {
		t.Fatalf("ok=%v stderr=%q", run.ok, run.stderr.String())
	}
}

func TestCatMissingFileFailsAndKeepsGoing(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"note.txt": "text\n"})
	run := runCatTest(t, catOutput{}, catOptions{}, filepath.Join(root, "missing"), filepath.Join(root, "note.txt"))
	if run.ok || run.stdout.String() != "text\n" || !strings.Contains(run.stderr.String(), "missing") {
		t.Fatalf("ok=%v stdout=%q stderr=%q", run.ok, run.stdout.String(), run.stderr.String())
	}
}

// --mask는 pipe에서도 값을 가리고, 가리지 않는 부분의 바이트와 줄 끝은 그대로 둔다.
func TestCatMaskHidesValuesInAPipe(t *testing.T) {
	root := writeCatFiles(t, map[string]string{".env": "# note\r\nTOKEN=s3cr3t\r\nKEY=\"s3cr3t\ns3cr3t=\"\tx"})
	run := runCatTest(t, catOutput{mask: true}, catOptions{}, filepath.Join(root, ".env"))
	want := "# note\r\nTOKEN=" + catMaskText + "\r\nKEY=" + catMaskText + "\n" + catMaskText
	if !run.ok || run.stdout.String() != want {
		t.Fatalf("ok=%v stdout=%q, want %q", run.ok, run.stdout.String(), want)
	}
}

// 형식을 모르는 파일은 어디가 값인지 알 수 없다. 가리지 못한 채 내보내면 --mask를 준 뜻과 어긋난다.
func TestCatMaskSkipsFilesItCannotMask(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"id_rsa": "s3cr3t\n", "blob.json": "{\"a\": \"s3cr3t\x00\"}", "app.json": "{\"a\": \"s3cr3t\"}\n"})
	key, blob, app := filepath.Join(root, "id_rsa"), filepath.Join(root, "blob.json"), filepath.Join(root, "app.json")
	for _, terminal := range []bool{false, true} {
		run := runCatTest(t, catOutput{terminal: terminal, mask: true}, catOptions{}, key, blob, app)
		if run.ok || strings.Contains(run.stdout.String(), "s3cr3t") || !strings.Contains(run.stdout.String(), `{"a": `+catMaskText+"}") {
			t.Errorf("terminal=%v ok=%v stdout=%q", terminal, run.ok, run.stdout.String())
		}
		if want := T("cat.mask_unsupported", key) + "\n" + T("cat.mask_unsupported", blob) + "\n"; run.stderr.String() != want {
			t.Errorf("terminal=%v stderr=%q, want %q", terminal, run.stderr.String(), want)
		}
	}
}

func TestCatNumbersRestartInEachFile(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"a.txt": "one\ntwo\n", ".env": "A=1"})
	text, env := filepath.Join(root, "a.txt"), filepath.Join(root, ".env")
	terminal := runCatTest(t, catOutput{terminal: true, numbers: true}, catOptions{}, text, env)
	if want := "==> " + text + " (8 B)\n   1  one\n   2  two\n\n==> " + env + " (dotenv · 3 B)\n   1  A=1\n"; terminal.stdout.String() != want {
		t.Errorf("terminal:\n%q\nwant:\n%q", terminal.stdout.String(), want)
	}
	colored := runCatTest(t, catOutput{terminal: true, color: true, numbers: true}, catOptions{}, env)
	if want := richDim("   1") + "  " + richPaint("A", richCyan, false) + "=" + richPaint("1", richYellow, false) + "\n"; !strings.HasSuffix(colored.stdout.String(), "\n"+want) {
		t.Errorf("colored: %q, want the suffix %q", colored.stdout.String(), want)
	}
	pipe := runCatTest(t, catOutput{numbers: true}, catOptions{}, text, env)
	if want := "     1\tone\n     2\ttwo\n     1\tA=1"; pipe.stdout.String() != want {
		t.Errorf("pipe: %q, want %q", pipe.stdout.String(), want)
	}
}

func TestCatReadsStdin(t *testing.T) {
	const content = "token: s3cr3t # note\nport: 80\n"
	yaml, _ := catSyntaxNamed("yaml")
	bash, _ := catSyntaxNamed("bash")
	for _, test := range []struct {
		name   string
		output catOutput
		ok     bool
		want   string
	}{
		{"pipe", catOutput{}, true, content},
		{"terminal without a format", catOutput{terminal: true}, true, "==> stdin\n" + content},
		{"terminal with a format", catOutput{terminal: true, as: &yaml}, true, "==> stdin (yaml)\n" + content},
		{"mask with a format", catOutput{mask: true, as: &yaml}, true, "token: " + catMaskText + "\nport: " + catMaskText + "\n"},
		{"mask without a format", catOutput{mask: true}, false, ""},
		{"mask with a source code format", catOutput{mask: true, as: &bash}, false, ""},
		{"mask with a source code format in a terminal", catOutput{terminal: true, mask: true, as: &bash}, false, ""},
	} {
		test.output.stdin = strings.NewReader(content)
		run := runCatTest(t, test.output, catOptions{}, catStdinName)
		if run.ok != test.ok || run.stdout.String() != test.want {
			t.Errorf("%s: ok=%v stdout=%q, want ok=%v stdout=%q", test.name, run.ok, run.stdout.String(), test.ok, test.want)
		}
		if !test.ok && run.stderr.String() != T("cat.mask_unsupported", catStdinTitle)+"\n" {
			t.Errorf("%s: stderr=%q", test.name, run.stderr.String())
		}
	}
}

// --as는 파일 이름으로 정한 형식보다 먼저다.
func TestCatAsOverridesTheFileName(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"settings": "A=1\n"})
	path := filepath.Join(root, "settings")
	env, _ := catSyntaxNamed("dotenv")
	run := runCatTest(t, catOutput{terminal: true, as: &env}, catOptions{}, path)
	if want := "==> " + path + " (dotenv · 4 B)\nA=1\n"; run.stdout.String() != want {
		t.Fatalf("stdout=%q, want %q", run.stdout.String(), want)
	}
}

// 소스 코드는 --mask가 가릴 수 없다. 색을 입히는 lexer가 생겼다고 값을 가린 것처럼 내보내면 안 된다.
func TestCatMaskSkipsSourceCode(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"deploy.sh": "export TOKEN=s3cr3t # s3cr3t\n"})
	path := filepath.Join(root, "deploy.sh")
	for _, output := range []catOutput{{mask: true}, {terminal: true, mask: true}, {terminal: true, color: true, mask: true}} {
		run := runCatTest(t, output, catOptions{}, path)
		if run.ok || run.stdout.Len() != 0 || run.stderr.String() != T("cat.mask_unsupported", path)+"\n" {
			t.Errorf("terminal=%v color=%v: ok=%v stdout=%q stderr=%q", output.terminal, output.color, run.ok, run.stdout.String(), run.stderr.String())
		}
	}
}

func TestCatPagerCommand(t *testing.T) {
	for _, test := range []struct {
		term  string
		value string
		set   bool
		want  string
	}{
		{"xterm-256color", "", false, "less"},
		{"xterm-256color", "", true, ""},
		{"xterm-256color", " cat ", true, ""},
		{"xterm-256color", "less -S", true, "less -S"},
		{"dumb", "", false, ""},
		{"", "less -S", true, ""},
	} {
		got := catPagerCommand(func(name string) (string, bool) {
			if name == "TERM" {
				return test.term, test.term != ""
			}
			return test.value, test.set
		})
		if got != test.want {
			t.Errorf("TERM=%q PAGER=%q set=%v: %q, want %q", test.term, test.value, test.set, got, test.want)
		}
	}
}

// 쓰다가 실패하면 남은 파일을 읽지 않는다. pager를 일찍 닫았을 때 큰 디렉터리를 끝까지 읽지 않게 한다.
func TestCatStopsAfterAWriteFails(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	var stderr bytes.Buffer
	output := &catOutput{stdout: bufio.NewWriterSize(catFailingWriter{}, 16), stderr: &stderr, terminal: true}
	if !catPaths(output, []string{filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")}, catOptions{}) {
		t.Error("a write failure is not a problem with a file")
	}
	if output.failed == nil || output.shown != 1 || stderr.Len() != 0 {
		t.Fatalf("failed=%v shown=%d stderr=%q", output.failed, output.shown, stderr.String())
	}
}

type catFailingWriter struct{}

func (catFailingWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }
