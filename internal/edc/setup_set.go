package edc

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

const setupSetUsage = "edc setup KEY [--add | --remove | --clear] [VALUE...]"

// setupKeyArgument는 edc setup의 첫 인자가 설정 키인지 본다. 키는 defaults 아래의 section.field이거나 lang이다.
// remote 같은 하위 명령 이름에는 .이 없다.
func setupKeyArgument(arg string) bool {
	return arg == "lang" || (strings.Contains(arg, ".") && !strings.HasPrefix(arg, "-"))
}

// runSetupSet은 설정 값 하나를 읽거나 바꾼다. 마법사와 같은 검증을 거쳐 같은 방식으로 저장한다. 터미널이 없어도 된다.
func runSetupSet(args []string, stdout, stderr io.Writer, path string) int {
	key, rest := args[0], args[1:]
	mode := ""
	if len(rest) > 0 && slices.Contains([]string{"--add", "--remove", "--clear"}, rest[0]) {
		mode, rest = strings.TrimPrefix(rest[0], "--"), rest[1:]
	}
	if path == "" {
		fmt.Fprintln(stderr, T("cli.setup.path_unavailable"))
		return 2
	}
	original, err := configFileSnapshot(path)
	if err != nil {
		fmt.Fprintln(stderr, T("cli.setup.input_failed", err))
		return 2
	}
	// 마법사는 잘못된 파일을 미리보기와 확인을 거쳐 새로 쓴다. 한 줄 설정에는 그 확인이 없으므로 파일을 덮지 않고 멈춘다.
	config, err := loadConfigAt(path)
	if err != nil {
		fmt.Fprintln(stderr, T("cli.config.invalid", configError(configReadPath(path), err)))
		return 2
	}
	field, ok := setupSetField(&config, key)
	if !ok {
		fmt.Fprintln(stderr, T("cli.setup.set.unknown_key", key))
		return 2
	}
	list := field.Kind() == reflect.Slice
	switch {
	case mode == "" && len(rest) == 0:
		values := setupSetShow(field)
		if len(values) == 0 {
			fmt.Fprintln(stderr, T("cli.setup.set.unset", key))
			return 1
		}
		for _, value := range values {
			fmt.Fprintln(stdout, value)
		}
		return 0
	case (mode == "add" || mode == "remove") && !list:
		fmt.Fprintln(stderr, T("cli.setup.set.not_list", key))
		return 2
	case mode == "clear" && len(rest) > 0, mode != "clear" && len(rest) == 0, mode == "" && !list && len(rest) != 1:
		fmt.Fprintln(stderr, T("cli.usage", setupSetUsage))
		return 2
	}
	switch mode {
	case "clear":
		field.Set(reflect.Zero(field.Type()))
	case "add":
		current := field.Interface().([]string)
		for _, value := range rest {
			if !slices.Contains(current, value) {
				current = append(current, value)
			}
		}
		field.Set(reflect.ValueOf(current))
	case "remove":
		current := field.Interface().([]string)
		for _, value := range rest {
			index := slices.Index(current, value)
			if index < 0 {
				fmt.Fprintln(stderr, T("cli.setup.set.not_in_list", key, value))
				return 1
			}
			current = slices.Delete(current, index, index+1)
		}
		if len(current) == 0 {
			current = nil
		}
		field.Set(reflect.ValueOf(current))
	default:
		if err := setupSetValue(field, rest); err != nil {
			fmt.Fprintln(stderr, T("cli.setup.invalid_value", fmt.Sprintf("%s: %v", key, err)))
			return 2
		}
	}
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

// setupSetField는 키가 가리키는 설정 필드다. 이름은 파일에 쓰는 yaml 태그와 같다. 한 값으로 쓸 수 없는 묶음은 받지 않는다.
func setupSetField(config *edcConfig, key string) (reflect.Value, bool) {
	if key == "lang" {
		return reflect.ValueOf(&config.Lang).Elem(), true
	}
	section, name, ok := strings.Cut(strings.TrimPrefix(key, "defaults."), ".")
	if !ok || strings.Contains(name, ".") {
		return reflect.Value{}, false
	}
	parent, ok := setupSetChild(reflect.ValueOf(&config.Defaults).Elem(), section)
	if !ok || parent.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	field, ok := setupSetChild(parent, name)
	if !ok {
		return reflect.Value{}, false
	}
	switch field.Type() {
	case reflect.TypeFor[*string](), reflect.TypeFor[*int](), reflect.TypeFor[*bool](), reflect.TypeFor[*configDuration](), reflect.TypeFor[[]string]():
		return field, true
	}
	return reflect.Value{}, false
}

func setupSetChild(parent reflect.Value, name string) (reflect.Value, bool) {
	for index := range parent.NumField() {
		tag, _, _ := strings.Cut(parent.Type().Field(index).Tag.Get("yaml"), ",")
		if tag == name {
			return parent.Field(index), true
		}
	}
	return reflect.Value{}, false
}

func setupSetShow(field reflect.Value) []string {
	switch value := field.Interface().(type) {
	case string:
		if value != "" {
			return []string{value}
		}
	case []string:
		return value
	case *configDuration:
		if value != nil {
			return []string{value.Duration.String()}
		}
	default:
		if !field.IsNil() {
			return []string{fmt.Sprint(field.Elem().Interface())}
		}
	}
	return nil
}

func setupSetValue(field reflect.Value, values []string) error {
	switch field.Interface().(type) {
	case string:
		field.SetString(values[0])
	case []string:
		field.Set(reflect.ValueOf(slices.Clone(values)))
	case *string:
		field.Set(reflect.ValueOf(stringPointer(values[0])))
	case *int:
		number, err := strconv.Atoi(values[0])
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(intPointer(number)))
	case *bool:
		boolean, err := strconv.ParseBool(values[0])
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(boolPointer(boolean)))
	case *configDuration:
		duration, err := time.ParseDuration(values[0])
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(durationPointer(duration)))
	}
	return nil
}

// marshalConfigFor는 설정을 파일 확장자에 맞는 형식으로 쓴다. 마법사와 한 줄 설정이 함께 쓴다.
func marshalConfigFor(path string, config edcConfig) ([]byte, error) {
	if filepath.Ext(path) == ".toml" {
		return toml.Marshal(config)
	}
	return yaml.Marshal(config)
}
