package edc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCatSyntaxOfFileNames(t *testing.T) {
	for name, want := range map[string]string{
		"main.go":         "go",
		"deploy.sh":       "bash",
		"Dockerfile":      "docker",
		"change.diff":     "diff",
		"nginx.conf":      "nginx",
		"app.conf":        "ini",
		"app.service":     "ini",
		".env.production": "dotenv",
		"values.yaml":     "yaml",
		"notes.txt":       "",
		"id_rsa":          "",
	} {
		if got := catSyntaxOf(filepath.Join("dir", name)).name(); got != want {
			t.Errorf("catSyntaxOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCatSyntaxNamed(t *testing.T) {
	for name, want := range map[string]string{"yaml": "yaml", "YML": "yaml", "env": "dotenv", "go": "go", "sh": "bash", "text": ""} {
		syntax, ok := catSyntaxNamed(name)
		if !ok || syntax.name() != want {
			t.Errorf("catSyntaxNamed(%q) = %q, %v, want %q", name, syntax.name(), ok, want)
		}
	}
	if _, ok := catSyntaxNamed("no-such-format"); ok {
		t.Error("an unknown name must not select a format")
	}
}

// --mask는 edc가 직접 읽는 설정 형식만 가린다. 소스 코드는 문자열과 숫자를 가려도 주석과 따옴표 없는 값이 남는다.
func TestCatSourceCodeIsNotMaskable(t *testing.T) {
	for _, name := range []string{"deploy.sh", "main.go", "nginx.conf", "notes.txt"} {
		if catSyntaxOf(name).maskable() {
			t.Errorf("%s must not be maskable", name)
		}
	}
	for _, name := range []string{".env", "app.json", "app.yaml", "app.toml", "app.ini"} {
		if !catSyntaxOf(name).maskable() {
			t.Errorf("%s must be maskable", name)
		}
	}
}

func TestCatTerminalColorsSourceCode(t *testing.T) {
	root := writeCatFiles(t, map[string]string{"main.go": "// note\nfunc main() { return \"x\" + 1 }\n"})
	path := filepath.Join(root, "main.go")
	run := runCatTest(t, catOutput{terminal: true, color: true}, catOptions{}, path)
	want := richDim("==>") + " " + richBold(path) + richDim(" (go · 39 B)") + "\n" +
		richDim("// note") + "\n" +
		richPaint("func", richBlue, false) + " main() { " + richPaint("return", richBlue, false) + " " +
		richPaint(`"x"`, richGreen, false) + " + " + richPaint("1", richYellow, false) + " }\n"
	if run.stdout.String() != want {
		t.Fatalf("stdout:\n%q\nwant:\n%q", run.stdout.String(), want)
	}
}

// 색을 입혀도 줄 수와 글자는 원본과 같아야 한다. 마지막 줄바꿈이 있든 없든, 빈 줄과 CRLF가 있어도 마찬가지다.
func TestCatSourceCodeKeepsEveryLine(t *testing.T) {
	for _, content := range []string{
		"package main\n\nfunc main() {}\n",
		"package main\n\nfunc main() {}",
		"\n\n",
		"x = 'open\nstill open\n",
		"a := 1\r\n\tb := `raw\r\nline`\r\n",
		"",
	} {
		root := writeCatFiles(t, map[string]string{"main.go": content})
		run := runCatTest(t, catOutput{terminal: true, color: true}, catOptions{}, filepath.Join(root, "main.go"))
		_, body, _ := strings.Cut(ansi.Strip(run.stdout.String()), "\n")
		want := ""
		for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
			if content != "" {
				want += viewSanitize(strings.TrimSuffix(line, "\r")) + "\n"
			}
		}
		if body != want {
			t.Errorf("content %q:\n got %q\nwant %q", content, body, want)
		}
	}
}

func TestCatSourceCodeOverTheLimitPrintsWithoutColor(t *testing.T) {
	content := strings.Repeat("var name = \"value\" // note\n", catCodeLimit/27+1)
	root := writeCatFiles(t, map[string]string{"big.go": content})
	path := filepath.Join(root, "big.go")
	run := runCatTest(t, catOutput{terminal: true, color: true}, catOptions{}, path)
	header, body, _ := strings.Cut(run.stdout.String(), "\n")
	if want := richDim("==>") + " " + richBold(path) + richDim(" (go · "+formatSizeShort(uint64(len(content)))+" · "+T("cat.color_omitted")+")"); header != want {
		t.Errorf("header %q, want %q", header, want)
	}
	if body != content {
		t.Errorf("the content must print unchanged and without color: %d bytes, want %d", len(body), len(content))
	}
}

// pipe에서는 소스 코드도 나누지 않고 바이트를 그대로 낸다.
func TestCatPipeDoesNotColorSourceCode(t *testing.T) {
	content := "func main() {\r\n\treturn\x1b[0m\n}"
	root := writeCatFiles(t, map[string]string{"main.go": content})
	run := runCatTest(t, catOutput{}, catOptions{}, filepath.Join(root, "main.go"))
	if !run.ok || run.stdout.String() != content {
		t.Fatalf("ok=%v stdout=%q", run.ok, run.stdout.String())
	}
}
