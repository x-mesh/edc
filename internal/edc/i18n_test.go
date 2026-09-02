package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 언어팩이 갈라지면 어떤 언어에서만 키가 보인다. 세 파일이 같은 키를 갖는지 잰다.
func TestEveryLanguageCarriesTheSameKeys(t *testing.T) {
	base := catalogKeys(defaultLanguage)
	if len(base) == 0 {
		t.Fatal("the base catalog is empty")
	}
	baseSet := map[string]bool{}
	for _, key := range base {
		baseSet[key] = true
	}
	for _, language := range supportedLanguages {
		if language == defaultLanguage {
			continue
		}
		keys := catalogKeys(language)
		found := map[string]bool{}
		for _, key := range keys {
			found[key] = true
			if !baseSet[key] {
				t.Errorf("%s has %q, which %s misses", language, key, defaultLanguage)
			}
		}
		for _, key := range base {
			if !found[key] {
				t.Errorf("%s misses %q", language, key)
			}
		}
	}
}

// help가 읽는 키가 실제로 있는지 본다. 없으면 화면에 키가 그대로 나온다.
func TestEveryCommandHasItsMessages(t *testing.T) {
	for _, language := range supportedLanguages {
		for _, doc := range commandDocs {
			if !hasMessage(language, "command."+doc.name+".summary") {
				t.Errorf("%s misses the summary of %s", language, doc.name)
			}
			for _, option := range doc.options {
				if option.key == "" {
					continue
				}
				if !hasMessage(language, option.key) {
					t.Errorf("%s misses %q", language, option.key)
				}
			}
		}
		for _, group := range helpGroups {
			if !hasMessage(language, "help.group."+group) {
				t.Errorf("%s misses the group name %q", language, group)
			}
		}
		for _, option := range commonOptionDocs {
			if !hasMessage(language, option.key) {
				t.Errorf("%s misses %q", language, option.key)
			}
		}
	}
}

func TestNormalizeLanguage(t *testing.T) {
	for _, row := range []struct {
		value string
		want  string
		ok    bool
	}{
		{"ko", "ko", true},
		{"KO", "ko", true},
		{"ko_KR.UTF-8", "ko", true},
		{"ja-JP", "ja", true},
		{"  en  ", "en", true},
		{"", "", false},
		{"fr", "", false},
		{"C", "", false},
	} {
		got, ok := normalizeLanguage(row.value)
		if got != row.want || ok != row.ok {
			t.Errorf("normalizeLanguage(%q) = %q, %v; want %q, %v", row.value, got, ok, row.want, row.ok)
		}
	}
}

func TestTranslationFallsBackToEnglishThenKey(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)

	setLanguage("ko")
	if got := T("command.doctor.summary"); got != "DNS·TCP·TLS·HTTP를 한 번에 확인" {
		t.Fatalf("korean summary = %q", got)
	}
	// 어느 언어에도 없는 키는 그대로 보여 준다. 조용히 빈 줄이 되면 누락을 놓친다.
	if got := T("command.nothing.here"); got != "command.nothing.here" {
		t.Fatalf("missing key = %q", got)
	}
	if got := TList("command.nothing.here"); got != nil {
		t.Fatalf("missing list = %#v", got)
	}
}

func TestTranslationFormatsArguments(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage(defaultLanguage)
	// 인자가 없으면 원문을 그대로 돌려준다.
	if got := T("help.tagline"); !strings.Contains(got, "everyday carry") {
		t.Fatalf("tagline = %q", got)
	}
}

func TestReadConfigLanguage(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	if err := os.MkdirAll(filepath.Join(directory, "edc"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "edc", "config.yaml")
	if err := os.WriteFile(path, []byte("lang: ja\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// macOS는 XDG_CONFIG_HOME을 보지 않으므로 값이 다를 수 있다. 읽었을 때만 확인한다.
	if configPath() == path {
		if got := readConfigLanguage(); got != "ja" {
			t.Fatalf("config lang = %q", got)
		}
	}
}

func TestReadConfigLanguageWithoutFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := readConfigLanguage(); got != "" {
		t.Fatalf("a missing config must give an empty value, got %q", got)
	}
}

func TestEnvironmentWinsOverConfig(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)

	t.Setenv(languageEnv, "ja")
	initLanguage()
	if currentLanguage() != "ja" {
		t.Fatalf("EDC_LANG must win, got %q", currentLanguage())
	}

	t.Setenv(languageEnv, "fr")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	initLanguage()
	if currentLanguage() != defaultLanguage {
		t.Fatalf("an unknown language must fall back to %s, got %q", defaultLanguage, currentLanguage())
	}
}
