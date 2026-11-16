package edc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

const setupSetUsage = "edc setup KEY [--add | --remove | --clear] [VALUE...]"

// setupPathKeys는 값이 파일이나 폴더 경로인 키다. 설정 파일은 ~를 풀지 않고 상대 경로는 실행한 폴더마다 다른 곳을
// 가리키므로, 한 줄 설정이 저장하기 전에 절대 경로로 바꾼다.
var setupPathKeys = []string{
	"defaults.common.json", "defaults.top.json", "defaults.capture.output", "defaults.log.output",
	"defaults.remote.inventory", "defaults.remote.recipe", "defaults.clean.projects", "check.cert_paths",
}

// setupKeyArgument는 edc setup의 첫 인자가 설정 키인지 본다. 키는 section.field 꼴이거나 lang이다.
// remote 같은 하위 명령 이름에는 .이 없다.
func setupKeyArgument(arg string) bool {
	return arg == "lang" || (strings.Contains(arg, ".") && !strings.HasPrefix(arg, "-"))
}

// setupKey는 한 줄 설정으로 바꿀 수 있는 필드 하나다. full은 파일 안의 위치이고 name은 사용자가 쓰는 이름이다.
// defaults 아래의 키는 defaults.를 떼고 쓴다.
type setupKey struct {
	name, full string
	field      reflect.Value
}

// setupSetKeys는 설정 구조체를 yaml 태그로 훑어 한 값으로 쓸 수 있는 필드를 모두 모은다. 표를 따로 두지 않으므로 설정에
// 필드를 더하면 그대로 키가 된다. map(check.routines)과 구조체 목록(notify.webhooks)은 한 값으로 쓸 수 없어 뺀다.
func setupSetKeys(config *edcConfig) []setupKey {
	var keys []setupKey
	var walk func(value reflect.Value, prefix string)
	walk = func(value reflect.Value, prefix string) {
		for index := range value.NumField() {
			tag, _, _ := strings.Cut(value.Type().Field(index).Tag.Get("yaml"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			full := tag
			if prefix != "" {
				full = prefix + "." + tag
			}
			field := value.Field(index)
			switch {
			case setupSetSupported(field.Type()):
				keys = append(keys, setupKey{name: strings.TrimPrefix(full, "defaults."), full: full, field: field})
			case field.Kind() == reflect.Struct:
				walk(field, full)
			}
		}
	}
	walk(reflect.ValueOf(config).Elem(), "")
	return keys
}

func setupSetSupported(kind reflect.Type) bool {
	switch kind {
	case reflect.TypeFor[string](), reflect.TypeFor[*string](), reflect.TypeFor[*int](), reflect.TypeFor[*bool](), reflect.TypeFor[*configDuration](),
		reflect.TypeFor[[]string](), reflect.TypeFor[[]int](), reflect.TypeFor[[]float64]():
		return true
	}
	return false
}

// setupSetKeyNames는 자동완성이 제안하는 키 이름이다.
func setupSetKeyNames() []string {
	var names []string
	for _, key := range setupSetKeys(&edcConfig{}) {
		names = append(names, key.name)
	}
	return names
}

func setupSetFind(config *edcConfig, name string) (setupKey, bool) {
	for _, key := range setupSetKeys(config) {
		if key.name == name || key.full == name {
			return key, true
		}
	}
	return setupKey{}, false
}

// runSetupSet은 설정 값 하나를 읽거나 바꾼다. 마법사와 같은 검증을 거쳐 같은 방식으로 저장한다. 터미널이 없어도 된다.
func runSetupSet(args []string, stdout, stderr io.Writer, path string) int {
	name, rest := args[0], slices.Clone(args[1:])
	mode := ""
	if len(rest) > 0 && slices.Contains([]string{"--add", "--remove", "--clear"}, rest[0]) {
		mode, rest = strings.TrimPrefix(rest[0], "--"), rest[1:]
	}
	config, original, code := setupSetLoad(stderr, path)
	if code != 0 {
		return code
	}
	key, ok := setupSetFind(&config, name)
	if !ok {
		fmt.Fprintln(stderr, T("cli.setup.set.unknown_key", name))
		return 2
	}
	field := key.field
	list := field.Kind() == reflect.Slice
	switch {
	case mode == "" && len(rest) == 0:
		values := setupSetShow(field)
		if len(values) == 0 {
			fmt.Fprintln(stderr, T("cli.setup.set.unset", name))
			return 1
		}
		for _, value := range values {
			fmt.Fprintln(stdout, value)
		}
		return 0
	case (mode == "add" || mode == "remove") && !list:
		fmt.Fprintln(stderr, T("cli.setup.set.not_list", name))
		return 2
	case mode == "clear" && len(rest) > 0, mode != "clear" && len(rest) == 0, mode == "" && !list && len(rest) != 1:
		fmt.Fprintln(stderr, T("cli.usage", setupSetUsage))
		return 2
	}
	if slices.Contains(setupPathKeys, key.full) {
		for index, value := range rest {
			resolved, err := setupAbsolutePath(value)
			if err != nil {
				fmt.Fprintln(stderr, T("cli.setup.invalid_value", fmt.Sprintf("%s: %v", name, err)))
				return 2
			}
			rest[index] = resolved
		}
	}
	if mode == "clear" {
		field.Set(reflect.Zero(field.Type()))
	} else if err := setupSetApply(field, mode, rest); err != nil {
		var missing setupMissingError
		if errors.As(err, &missing) {
			fmt.Fprintln(stderr, T("cli.setup.set.not_in_list", name, missing.value))
			return 1
		}
		fmt.Fprintln(stderr, T("cli.setup.invalid_value", fmt.Sprintf("%s: %v", name, err)))
		return 2
	}
	return setupSetSave(stdout, stderr, path, config, original)
}

// runSetupList는 바꿀 수 있는 키를 모두 형식, 현재 값과 함께 보인다.
func runSetupList(stdout, stderr io.Writer, path string) int {
	config, _, code := setupSetLoad(stderr, path)
	if code != 0 {
		return code
	}
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, key := range setupSetKeys(&config) {
		fmt.Fprintf(table, "%s\t%s\t%s\n", key.name, setupSetTypeName(key.field.Type()), strings.Join(setupSetShow(key.field), ", "))
	}
	table.Flush()
	return 0
}

// setupSetLoad는 지금 설정과 저장할 때 비교할 파일 상태를 읽는다. 마법사는 잘못된 파일을 미리보기와 확인을 거쳐 새로
// 쓰지만, 한 줄 설정에는 그 확인이 없으므로 파일을 덮지 않고 멈춘다.
func setupSetLoad(stderr io.Writer, path string) (edcConfig, configSnapshot, int) {
	if path == "" {
		fmt.Fprintln(stderr, T("cli.setup.path_unavailable"))
		return edcConfig{}, configSnapshot{}, 2
	}
	original, err := configFileSnapshot(path)
	if err != nil {
		fmt.Fprintln(stderr, T("cli.setup.input_failed", err))
		return edcConfig{}, configSnapshot{}, 2
	}
	config, err := loadConfigAt(path)
	if err != nil {
		fmt.Fprintln(stderr, T("cli.config.invalid", configError(configReadPath(path), err)))
		return edcConfig{}, configSnapshot{}, 2
	}
	return config, original, 0
}

func setupSetSave(stdout, stderr io.Writer, path string, config edcConfig, original configSnapshot) int {
	if err := validateConfig(config); err != nil {
		fmt.Fprintln(stderr, T("cli.config.invalid", err))
		return 2
	}
	data, err := marshalConfigFor(path, config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := writeConfigIfUnchanged(path, data, original); err != nil {
		if errors.Is(err, errConfigChanged) {
			fmt.Fprintln(stderr, T("cli.setup.config_changed"))
			return 2
		}
		fmt.Fprintln(stderr, T("cli.setup.write_failed", err))
		return 2
	}
	fmt.Fprintln(stdout, T("cli.setup.saved", path))
	return 0
}

// setupAbsolutePath는 경로 값을 절대 경로로 바꾼다. -는 stdout이라는 뜻이고 빈 값은 기본값을 끄는 뜻이라 그대로 둔다.
// ~user는 풀지 않으므로 검증이 절대 경로가 아니라고 거부한다.
func setupAbsolutePath(value string) (string, error) {
	switch {
	case value == "" || value == "-" || filepath.IsAbs(value):
		return value, nil
	case value == "~" || strings.HasPrefix(value, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(value, "~")), nil
	case strings.HasPrefix(value, "~"):
		return value, nil
	}
	return filepath.Abs(value)
}

type setupMissingError struct{ value string }

func (err setupMissingError) Error() string { return "not in the list: " + err.value }

// setupSetApply는 값을 필드 형식으로 읽어 넣는다. 목록은 바꾸기, --add, --remove를 받고 나머지는 값 하나를 받는다.
func setupSetApply(field reflect.Value, mode string, values []string) error {
	if field.Kind() != reflect.Slice {
		parsed, err := setupParse(setupSetElem(field.Type()), values[0])
		if err != nil {
			return err
		}
		if field.Kind() == reflect.Pointer {
			pointer := reflect.New(parsed.Type())
			pointer.Elem().Set(parsed)
			parsed = pointer
		}
		field.Set(parsed)
		return nil
	}
	elements := reflect.MakeSlice(field.Type(), 0, len(values))
	for _, value := range values {
		parsed, err := setupParse(field.Type().Elem(), value)
		if err != nil {
			return err
		}
		elements = reflect.Append(elements, parsed)
	}
	current := field
	switch mode {
	case "add":
		for index := range elements.Len() {
			if setupIndex(current, elements.Index(index)) < 0 {
				current = reflect.Append(current, elements.Index(index))
			}
		}
	case "remove":
		for index := range elements.Len() {
			at := setupIndex(current, elements.Index(index))
			if at < 0 {
				return setupMissingError{value: values[index]}
			}
			current = reflect.AppendSlice(current.Slice(0, at), current.Slice(at+1, current.Len()))
		}
	default:
		current = elements
	}
	if current.Len() == 0 {
		current = reflect.Zero(field.Type())
	}
	field.Set(current)
	return nil
}

func setupIndex(list, element reflect.Value) int {
	for index := range list.Len() {
		if list.Index(index).Interface() == element.Interface() {
			return index
		}
	}
	return -1
}

func setupSetElem(kind reflect.Type) reflect.Type {
	if kind.Kind() == reflect.Pointer {
		return kind.Elem()
	}
	return kind
}

func setupParse(kind reflect.Type, value string) (reflect.Value, error) {
	switch kind {
	case reflect.TypeFor[string]():
		return reflect.ValueOf(value), nil
	case reflect.TypeFor[int]():
		number, err := strconv.Atoi(value)
		return reflect.ValueOf(number), err
	case reflect.TypeFor[float64]():
		number, err := strconv.ParseFloat(value, 64)
		return reflect.ValueOf(number), err
	case reflect.TypeFor[bool]():
		boolean, err := strconv.ParseBool(value)
		return reflect.ValueOf(boolean), err
	case reflect.TypeFor[configDuration]():
		duration, err := time.ParseDuration(value)
		return reflect.ValueOf(configDuration{Duration: duration}), err
	}
	return reflect.Value{}, fmt.Errorf("unsupported type %s", kind)
}

func setupSetShow(field reflect.Value) []string {
	switch field.Kind() {
	case reflect.String:
		if field.String() != "" {
			return []string{field.String()}
		}
		return nil
	case reflect.Pointer:
		if field.IsNil() {
			return nil
		}
		return []string{setupFormat(field.Elem())}
	}
	var values []string
	for index := range field.Len() {
		values = append(values, setupFormat(field.Index(index)))
	}
	return values
}

func setupFormat(value reflect.Value) string {
	switch item := value.Interface().(type) {
	case configDuration:
		return item.Duration.String()
	case float64:
		return strconv.FormatFloat(item, 'g', -1, 64)
	}
	return fmt.Sprint(value.Interface())
}

func setupSetTypeName(kind reflect.Type) string {
	element := setupSetElem(kind)
	prefix := ""
	if kind.Kind() == reflect.Slice {
		prefix, element = "list of ", kind.Elem()
	}
	switch element {
	case reflect.TypeFor[configDuration]():
		return prefix + "duration"
	case reflect.TypeFor[float64]():
		return prefix + "number"
	}
	return prefix + element.String()
}

// marshalConfigFor는 설정을 파일 확장자에 맞는 형식으로 쓴다. 마법사와 한 줄 설정이 함께 쓴다.
func marshalConfigFor(path string, config edcConfig) ([]byte, error) {
	if filepath.Ext(path) == ".toml" {
		return toml.Marshal(config)
	}
	return yaml.Marshal(config)
}
