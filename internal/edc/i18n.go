package edc

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed locale/*.yaml
var localeFiles embed.FS

const (
	// defaultLanguage는 설정이 없을 때 쓰는 언어다.
	defaultLanguage = "en"
	// languageEnv는 config 파일보다 우선한다. 한 번만 다른 언어로 보고 싶을 때 쓴다.
	languageEnv = "EDC_LANG"
)

// supportedLanguages는 locale 디렉터리에 파일이 있는 언어다.
var supportedLanguages = []string{"en", "ja", "ko"}

// catalog는 언어 하나의 메시지 나무다. 키는 점으로 계층을 나눈다.
type catalog struct {
	language string
	tree     map[string]interface{}
}

var (
	catalogOnce sync.Once
	catalogs    map[string]catalog
	activeLang  = defaultLanguage
)

func loadCatalogs() {
	catalogOnce.Do(func() {
		catalogs = make(map[string]catalog, len(supportedLanguages))
		for _, language := range supportedLanguages {
			data, err := localeFiles.ReadFile("locale/" + language + ".yaml")
			if err != nil {
				continue
			}
			var tree map[string]interface{}
			if err := yaml.Unmarshal(data, &tree); err != nil {
				continue
			}
			catalogs[language] = catalog{language: language, tree: tree}
		}
	})
}

// initLanguage는 환경변수와 config 파일을 보고 쓸 언어를 정한다.
// 우선순위는 EDC_LANG, config의 lang, 그리고 기본값 en이다.
func initLanguage() {
	loadCatalogs()
	if language, ok := normalizeLanguage(os.Getenv(languageEnv)); ok {
		activeLang = language
		return
	}
	if language, ok := normalizeLanguage(readConfigLanguage()); ok {
		activeLang = language
		return
	}
	activeLang = defaultLanguage
}

// normalizeLanguage는 ko_KR.UTF-8 같은 값에서 앞의 언어 코드만 본다.
func normalizeLanguage(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "", false
	}
	for _, separator := range []string{".", "_", "-"} {
		if index := strings.Index(value, separator); index > 0 {
			value = value[:index]
		}
	}
	for _, language := range supportedLanguages {
		if value == language {
			return language, true
		}
	}
	return "", false
}

func currentLanguage() string {
	loadCatalogs()
	return activeLang
}

// setLanguage는 test가 언어를 바꿔 볼 때 쓴다.
func setLanguage(language string) {
	loadCatalogs()
	if _, ok := catalogs[language]; ok {
		activeLang = language
	}
}

func (c catalog) lookup(key string) (interface{}, bool) {
	var current interface{} = c.tree
	for _, part := range strings.Split(key, ".") {
		branch, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = branch[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func (c catalog) text(key string) (string, bool) {
	value, ok := c.lookup(key)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func (c catalog) list(key string) ([]string, bool) {
	value, ok := c.lookup(key)
	if !ok {
		return nil, false
	}
	items, ok := value.([]interface{})
	if !ok {
		return nil, false
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		result = append(result, text)
	}
	return result, true
}

// T는 키의 메시지를 현재 언어로 돌려준다.
// 번역이 없으면 영어로 내려가고, 영어에도 없으면 키를 그대로 보여 준다.
// 키가 화면에 보이면 번역이 빠졌다는 뜻이므로 조용히 넘어가지 않는다.
func T(key string, args ...interface{}) string {
	loadCatalogs()
	text, ok := catalogs[activeLang].text(key)
	if !ok {
		text, ok = catalogs[defaultLanguage].text(key)
	}
	if !ok {
		return key
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// TList는 여러 줄짜리 메시지를 돌려준다. usage와 notes가 이 형태다.
func TList(key string) []string {
	loadCatalogs()
	items, ok := catalogs[activeLang].list(key)
	if !ok {
		items, ok = catalogs[defaultLanguage].list(key)
	}
	if !ok {
		return nil
	}
	return items
}

// hasMessage는 키가 어느 언어에든 있는지 본다. test가 빠진 번역을 찾을 때 쓴다.
func hasMessage(language, key string) bool {
	loadCatalogs()
	if _, ok := catalogs[language].text(key); ok {
		return true
	}
	_, ok := catalogs[language].list(key)
	return ok
}

// catalogKeys는 언어 하나가 가진 모든 키를 평탄하게 편다.
func catalogKeys(language string) []string {
	loadCatalogs()
	var keys []string
	var walk func(prefix string, node interface{})
	walk = func(prefix string, node interface{}) {
		branch, ok := node.(map[string]interface{})
		if !ok {
			keys = append(keys, prefix)
			return
		}
		for name, child := range branch {
			next := name
			if prefix != "" {
				next = prefix + "." + name
			}
			walk(next, child)
		}
	}
	walk("", catalogs[language].tree)
	sort.Strings(keys)
	return keys
}

// edcConfig는 사용자 설정이다. 지금은 언어만 담는다.
type edcConfig struct {
	Lang string `yaml:"lang"`
}

// configPath는 os.UserConfigDir 아래의 설정 경로다. inventory와 같은 자리를 쓴다.
func configPath() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "edc", "config.yaml")
}

// readConfigLanguage는 설정 파일의 lang을 읽는다.
// 파일이 없거나 읽지 못하면 빈 값을 주고, 다음 단계가 기본값을 쓴다.
func readConfigLanguage() string {
	path := configPath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var config edcConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	return config.Lang
}
