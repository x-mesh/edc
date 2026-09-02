package edc

import (
	"os"
	"path/filepath"
	"sort"
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

// 경로를 직접 넘겨 개발자의 실제 설정을 읽지 않게 한다.
func TestReadConfigLanguageAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("lang: ja\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readConfigLanguageAt(path); got != "ja" {
		t.Fatalf("config lang = %q", got)
	}
	if got := readConfigLanguageAt(filepath.Join(t.TempDir(), "absent.yaml")); got != "" {
		t.Fatalf("a missing config must give an empty value, got %q", got)
	}
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("lang: [1, 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readConfigLanguageAt(broken); got != "" {
		t.Fatalf("a broken config must give an empty value, got %q", got)
	}
}

func TestConfigPathSitsUnderTheUserConfigDirectory(t *testing.T) {
	path := configPath()
	if path == "" {
		t.Skip("this platform has no user config directory")
	}
	if filepath.Base(path) != "config.yaml" || filepath.Base(filepath.Dir(path)) != "edc" {
		t.Fatalf("config path = %q", path)
	}
}

func TestResolveLanguagePrefersTheEnvironment(t *testing.T) {
	for _, row := range []struct{ env, config, want string }{
		{"ja", "ko", "ja"},
		{"", "ko", "ko"},
		{"", "", defaultLanguage},
		{"fr", "ko", "ko"},
		{"fr", "de", defaultLanguage},
		{"ko_KR.UTF-8", "", "ko"},
	} {
		if got := resolveLanguage(row.env, row.config); got != row.want {
			t.Errorf("resolveLanguage(%q, %q) = %q, want %q", row.env, row.config, got, row.want)
		}
	}
}

// 포맷 지정자가 언어마다 다르면 인자가 어긋나 %!d(MISSING) 같은 출력이 사용자에게 간다.
// 키 집합만 맞추는 검사로는 잡히지 않으므로 지정자까지 잰다.
func TestFormatVerbsMatchAcrossLanguages(t *testing.T) {
	loadCatalogs()
	for _, key := range catalogKeys(defaultLanguage) {
		base, ok := catalogs[defaultLanguage].text(key)
		if !ok {
			continue
		}
		want := formatVerbs(base)
		for _, language := range supportedLanguages {
			if language == defaultLanguage {
				continue
			}
			text, ok := catalogs[language].text(key)
			if !ok {
				continue
			}
			if got := formatVerbs(text); !equalStrings(got, want) {
				t.Errorf("%s %q has verbs %v, but %s has %v", language, key, got, defaultLanguage, want)
			}
		}
	}
}

// 목록 길이가 다르면 어떤 언어에서만 줄이 사라진다. capture의 개인정보 경고가 그런 줄이다.
func TestListLengthsMatchAcrossLanguages(t *testing.T) {
	loadCatalogs()
	for _, key := range catalogKeys(defaultLanguage) {
		base, ok := catalogs[defaultLanguage].list(key)
		if !ok {
			continue
		}
		for _, language := range supportedLanguages {
			if language == defaultLanguage {
				continue
			}
			items, ok := catalogs[language].list(key)
			if !ok {
				t.Errorf("%s misses the list %q", language, key)
				continue
			}
			if len(items) != len(base) {
				t.Errorf("%s %q has %d lines, but %s has %d", language, key, len(items), defaultLanguage, len(base))
			}
		}
	}
}

// help가 읽는 notes는 없어도 화면이 조용히 짧아질 뿐이라 누락을 알아채기 어렵다.
func TestCommandNotesResolveInEveryLanguage(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)

	for _, doc := range commandDocs {
		key := "command." + doc.name + ".notes"
		if !hasMessage(defaultLanguage, key) {
			continue
		}
		for _, language := range supportedLanguages {
			setLanguage(language)
			notes := doc.notes()
			if len(notes) == 0 {
				t.Errorf("%s resolves no notes for %s", language, doc.name)
			}
			for index, note := range notes {
				if strings.TrimSpace(note) == "" || strings.HasPrefix(note, "command.") {
					t.Errorf("%s note %d of %s is unresolved: %q", language, index, doc.name, note)
				}
			}
		}
	}
}

func formatVerbs(text string) []string {
	var verbs []string
	for index := 0; index < len(text)-1; index++ {
		if text[index] != '%' {
			continue
		}
		rest := text[index+1:]
		if rest[0] == '%' {
			index++
			continue
		}
		end := 0
		for end < len(rest) && !isVerbLetter(rest[end]) {
			end++
		}
		if end < len(rest) {
			// 인덱스(%[1]s)는 순서를 바꾸려는 표시이므로 지정자 글자만 남긴다.
			verbs = append(verbs, string(rest[end]))
			index += end
		}
	}
	sort.Strings(verbs)
	return verbs
}

func isVerbLetter(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
