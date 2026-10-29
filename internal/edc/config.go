package edc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

const maxConfigBytes = 1024 * 1024

const legacyRecommendedLogOutput = "/var/log/job.log"

var configScalarTypes = map[string]string{
	"lang": "!!str", "defaults.common.json": "!!str", "defaults.doctor.profile": "!!str",
	"defaults.top.json": "!!str", "defaults.top.split": "!!str", "defaults.top.interface": "!!str",
	"defaults.top.view": "!!str", "defaults.top.panels": "!!str", "defaults.top.process_sort": "!!str", "defaults.top.columns": "!!str", "defaults.where.provider": "!!str",
	"defaults.capture.interface": "!!str", "defaults.capture.filter": "!!str", "defaults.capture.output": "!!str",
	"defaults.remote.inventory": "!!str", "defaults.remote.recipe": "!!str",
	"defaults.log.stream": "!!str", "defaults.log.output": "!!str", "defaults.log.command_display": "!!str", "defaults.log.restart": "!!str",
	"defaults.log.max_size_mb": "!!int", "defaults.log.keep_files": "!!int", "defaults.log.max_restarts": "!!int",
	"defaults.common.verbose": "!!bool", "defaults.common.redact": "!!bool",
	"defaults.tls.min_days": "!!int", "defaults.http.expect_status": "!!int",
	"defaults.top.count": "!!int", "defaults.top.no_header": "!!bool", "defaults.top.auto_record": "!!bool",
	"defaults.info.public": "!!bool", "defaults.info.verbose": "!!bool",
	"defaults.where.count": "!!int", "defaults.capture.count": "!!int",
	"defaults.remote.output_limit": "!!int", "defaults.remote.parallel": "!!int",
}

type configDuration struct {
	Duration time.Duration
}

func (duration *configDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("duration must be a string such as 15s")
	}
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	duration.Duration = value
	return nil
}

func (duration configDuration) MarshalYAML() (interface{}, error) {
	return duration.Duration.String(), nil
}

func (duration *configDuration) UnmarshalText(value []byte) error {
	parsed, err := time.ParseDuration(string(value))
	if err != nil {
		return err
	}
	duration.Duration = parsed
	return nil
}

func (duration configDuration) MarshalText() ([]byte, error) {
	return []byte(duration.Duration.String()), nil
}

type edcConfig struct {
	Lang     string         `yaml:"lang,omitempty" toml:"lang,omitempty"`
	Defaults configDefaults `yaml:"defaults,omitempty" toml:"defaults,omitempty"`
	Audit    auditConfig    `yaml:"audit,omitempty" toml:"audit,omitempty"`
	Check    checkConfig    `yaml:"check,omitempty" toml:"check,omitempty"`
	// commandSafety는 읽은 설정 파일로 check 루틴의 셸 명령을 실행해도 되는지다. 파일을 열 때 정하고 파일에는 쓰지 않는다.
	commandSafety error
}

// checkConfig는 check가 받아들일 결과와 반드시 확인할 unit, 판정 기준이다. 받아들인 항목도 숨기지 않고 개수와 이름을 남긴다.
type checkConfig struct {
	// Accept는 점검 하나를 통째로 받아들인다. 이름은 check. 뒤의 점검 이름이다.
	Accept []string `yaml:"accept,omitempty" toml:"accept,omitempty"`
	// AcceptUnits, AcceptContainers, AcceptMounts는 그 점검 안에서 판정하지 않을 이름이다. path.Match의 glob을 쓴다.
	AcceptUnits      []string          `yaml:"accept_units,omitempty" toml:"accept_units,omitempty"`
	AcceptContainers []string          `yaml:"accept_containers,omitempty" toml:"accept_containers,omitempty"`
	AcceptMounts     []string          `yaml:"accept_mounts,omitempty" toml:"accept_mounts,omitempty"`
	RequireUnits     []string          `yaml:"require_units,omitempty" toml:"require_units,omitempty"`
	Limits           checkLimitsConfig `yaml:"limits,omitempty" toml:"limits,omitempty"`
	// Routines는 edc check <이름>으로 고르는 묶음이다. 위의 값에 더해지고, limits는 루틴에 적은 값만 바꾼다.
	Routines map[string]checkRoutineConfig `yaml:"routines,omitempty" toml:"routines,omitempty"`
}

type checkRoutineConfig struct {
	// Checks가 비어 있으면 모든 점검을 한다.
	Checks           []string             `yaml:"checks,omitempty" toml:"checks,omitempty"`
	Accept           []string             `yaml:"accept,omitempty" toml:"accept,omitempty"`
	AcceptUnits      []string             `yaml:"accept_units,omitempty" toml:"accept_units,omitempty"`
	AcceptContainers []string             `yaml:"accept_containers,omitempty" toml:"accept_containers,omitempty"`
	AcceptMounts     []string             `yaml:"accept_mounts,omitempty" toml:"accept_mounts,omitempty"`
	RequireUnits     []string             `yaml:"require_units,omitempty" toml:"require_units,omitempty"`
	Limits           checkLimitsConfig    `yaml:"limits,omitempty" toml:"limits,omitempty"`
	Commands         []checkCommandConfig `yaml:"commands,omitempty" toml:"commands,omitempty"`
}

// checkCommandConfig는 루틴이 sh -c로 실행하는 사용자 명령이다. 종료 코드 0이 통과다.
type checkCommandConfig struct {
	Name    string          `yaml:"name" toml:"name"`
	Run     string          `yaml:"run" toml:"run"`
	Timeout *configDuration `yaml:"timeout,omitempty" toml:"timeout,omitempty"`
	// Level은 0이 아닌 종료 코드의 판정이다. 비어 있으면 fail이다.
	Level string `yaml:"level,omitempty" toml:"level,omitempty"`
}

// checkLimitsConfig는 [warn, fail] 백분율이다. memory_available은 남은 비율이라 warn이 fail보다 크다.
type checkLimitsConfig struct {
	Disk            []float64 `yaml:"disk,omitempty" toml:"disk,omitempty"`
	MemoryAvailable []float64 `yaml:"memory_available,omitempty" toml:"memory_available,omitempty"`
	Usage           []float64 `yaml:"usage,omitempty" toml:"usage,omitempty"`
}

// auditConfig는 사용자가 확인하고 받아들인 audit 결과다. 받아들인 결과도 숨기지 않고 accepted로 보인다.
type auditConfig struct {
	Accept     []string `yaml:"accept,omitempty" toml:"accept,omitempty"`
	AllowPorts []int    `yaml:"allow_ports,omitempty" toml:"allow_ports,omitempty"`
}

type configDefaults struct {
	Common   commonConfig   `yaml:"common,omitempty" toml:"common,omitempty"`
	Doctor   doctorConfig   `yaml:"doctor,omitempty" toml:"doctor,omitempty"`
	TLS      tlsConfig      `yaml:"tls,omitempty" toml:"tls,omitempty"`
	HTTP     httpConfig     `yaml:"http,omitempty" toml:"http,omitempty"`
	Top      topConfig      `yaml:"top,omitempty" toml:"top,omitempty"`
	Info     infoConfig     `yaml:"info,omitempty" toml:"info,omitempty"`
	Where    whereConfig    `yaml:"where,omitempty" toml:"where,omitempty"`
	Capture  captureConfig  `yaml:"capture,omitempty" toml:"capture,omitempty"`
	Remote   remoteConfig   `yaml:"remote,omitempty" toml:"remote,omitempty"`
	Update   updateConfig   `yaml:"update,omitempty" toml:"update,omitempty"`
	Quality  qualityConfig  `yaml:"quality,omitempty" toml:"quality,omitempty"`
	Log      logConfig      `yaml:"log,omitempty" toml:"log,omitempty"`
	Du       duConfig       `yaml:"du,omitempty" toml:"du,omitempty"`
	History  historyConfig  `yaml:"history,omitempty" toml:"history,omitempty"`
	Changes  changesConfig  `yaml:"changes,omitempty" toml:"changes,omitempty"`
	Snapshot snapshotConfig `yaml:"snapshot,omitempty" toml:"snapshot,omitempty"`
	Events   eventsConfig   `yaml:"events,omitempty" toml:"events,omitempty"`
}

type commonConfig struct {
	Timeout *configDuration `yaml:"timeout,omitempty" toml:"timeout,omitempty"`
	JSON    *string         `yaml:"json,omitempty" toml:"json,omitempty"`
	Verbose *bool           `yaml:"verbose,omitempty" toml:"verbose,omitempty"`
	Redact  *bool           `yaml:"redact,omitempty" toml:"redact,omitempty"`
}
type doctorConfig struct {
	Profile *string `yaml:"profile,omitempty" toml:"profile,omitempty"`
}
type tlsConfig struct {
	MinDays *int `yaml:"min_days,omitempty" toml:"min_days,omitempty"`
}
type httpConfig struct {
	ExpectStatus *int `yaml:"expect_status,omitempty" toml:"expect_status,omitempty"`
}
type topConfig struct {
	Interval *configDuration `yaml:"interval,omitempty" toml:"interval,omitempty"`
	Count    *int            `yaml:"count,omitempty" toml:"count,omitempty"`
	NoHeader *bool           `yaml:"no_header,omitempty" toml:"no_header,omitempty"`
	JSON     *string         `yaml:"json,omitempty" toml:"json,omitempty"`
	Split    *string         `yaml:"split,omitempty" toml:"split,omitempty"`
	// Interface는 net 값에 더할 interface다. 여러 host가 같은 설정을 쓰므로, macOS에서 physical은 경고만 내고 모두 더한다.
	Interface *string `yaml:"interface,omitempty" toml:"interface,omitempty"`
	// View, Panels, ProcessSort, Columns는 대시보드의 처음 보기, 아래 패널 목록, process 패널 기본 순위, all 보기 열 그룹이다.
	View        *string `yaml:"view,omitempty" toml:"view,omitempty"`
	Panels      *string `yaml:"panels,omitempty" toml:"panels,omitempty"`
	ProcessSort *string `yaml:"process_sort,omitempty" toml:"process_sort,omitempty"`
	Columns     *string `yaml:"columns,omitempty" toml:"columns,omitempty"`
	// AutoRecord는 대시보드가 이상 징후를 보면 스스로 기록을 시작하는지다. 기본은 꺼져 있다.
	AutoRecord *bool `yaml:"auto_record,omitempty" toml:"auto_record,omitempty"`
}
type infoConfig struct {
	Public  *bool           `yaml:"public,omitempty" toml:"public,omitempty"`
	Timeout *configDuration `yaml:"timeout,omitempty" toml:"timeout,omitempty"`
	Verbose *bool           `yaml:"verbose,omitempty" toml:"verbose,omitempty"`
}
type whereConfig struct {
	Provider *string `yaml:"provider,omitempty" toml:"provider,omitempty"`
	Count    *int    `yaml:"count,omitempty" toml:"count,omitempty"`
}
type captureConfig struct {
	Interface *string         `yaml:"interface,omitempty" toml:"interface,omitempty"`
	Duration  *configDuration `yaml:"duration,omitempty" toml:"duration,omitempty"`
	Count     *int            `yaml:"count,omitempty" toml:"count,omitempty"`
	Filter    *string         `yaml:"filter,omitempty" toml:"filter,omitempty"`
	Output    *string         `yaml:"output,omitempty" toml:"output,omitempty"`
}
type remoteConfig struct {
	Inventory      *string         `yaml:"inventory,omitempty" toml:"inventory,omitempty"`
	Recipe         *string         `yaml:"recipe,omitempty" toml:"recipe,omitempty"`
	ConnectTimeout *configDuration `yaml:"connect_timeout,omitempty" toml:"connect_timeout,omitempty"`
	OutputLimit    *int            `yaml:"output_limit,omitempty" toml:"output_limit,omitempty"`
	Parallel       *int            `yaml:"parallel,omitempty" toml:"parallel,omitempty"`
}
type updateConfig struct {
	Timeout *configDuration `yaml:"timeout,omitempty" toml:"timeout,omitempty"`
}

// duConfig의 workers는 du와 clean이 함께 읽는다. 둘 다 같은 스캐너로 크기를 잰다.
type duConfig struct {
	Workers *int `yaml:"workers,omitempty" toml:"workers,omitempty"`
}
type historyConfig struct {
	Limit *int `yaml:"limit,omitempty" toml:"limit,omitempty"`
}
type changesConfig struct {
	Since *configDuration `yaml:"since,omitempty" toml:"since,omitempty"`
}
type snapshotConfig struct {
	Since *configDuration `yaml:"since,omitempty" toml:"since,omitempty"`
}
type eventsConfig struct {
	Since *configDuration `yaml:"since,omitempty" toml:"since,omitempty"`
}
type qualityConfig struct {
	Timeout *configDuration `yaml:"timeout,omitempty" toml:"timeout,omitempty"`
	Server  *string         `yaml:"server,omitempty" toml:"server,omitempty"`
}
type logConfig struct {
	Stream         *string         `yaml:"stream,omitempty" toml:"stream,omitempty"`
	Output         *string         `yaml:"output,omitempty" toml:"output,omitempty"`
	CommandDisplay *string         `yaml:"command_display,omitempty" toml:"command_display,omitempty"`
	MaxSizeMB      *int            `yaml:"max_size_mb,omitempty" toml:"max_size_mb,omitempty"`
	KeepFiles      *int            `yaml:"keep_files,omitempty" toml:"keep_files,omitempty"`
	Restart        *string         `yaml:"restart,omitempty" toml:"restart,omitempty"`
	MaxRestarts    *int            `yaml:"max_restarts,omitempty" toml:"max_restarts,omitempty"`
	RestartDelay   *configDuration `yaml:"restart_delay,omitempty" toml:"restart_delay,omitempty"`
	Timeout        *configDuration `yaml:"timeout,omitempty" toml:"timeout,omitempty"`
	KillAfter      *configDuration `yaml:"kill_after,omitempty" toml:"kill_after,omitempty"`
	HistoryLimit   *int            `yaml:"history_limit,omitempty" toml:"history_limit,omitempty"`
}

var activeConfig edcConfig

func configReadPath(path string) string {
	legacyMac := ""
	if runtime.GOOS == "darwin" && path == configPath() {
		if directory, err := os.UserConfigDir(); err == nil {
			legacyMac = filepath.Join(directory, "edc", "config.yaml")
		}
	}
	return configReadPathFor(path, legacyMac)
}

func configReadPathFor(path, legacyMac string) string {
	if filepath.Ext(path) != ".toml" {
		return path
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return path
	}
	legacy := strings.TrimSuffix(path, ".toml") + ".yaml"
	if _, err := os.Lstat(legacy); err == nil {
		return legacy
	}
	if legacyMac != "" {
		if _, err := os.Lstat(legacyMac); err == nil {
			return legacyMac
		}
	}
	return path
}

func loadConfigAt(path string) (edcConfig, error) {
	var config edcConfig
	if path == "" {
		return config, nil
	}
	path = configReadPath(path)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if !info.Mode().IsRegular() {
		return config, errors.New("config must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return config, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return config, err
	}
	if len(data) > maxConfigBytes {
		return config, fmt.Errorf("config exceeds %d bytes", maxConfigBytes)
	}
	if filepath.Ext(path) == ".toml" {
		if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&config); err != nil {
			return edcConfig{}, err
		}
	} else {
		if err := validateConfigStringTypes(data); err != nil {
			return config, err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
			return edcConfig{}, err
		}
		var extra interface{}
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				err = errors.New("multiple YAML documents are not supported")
			}
			return edcConfig{}, err
		}
	}
	if err := validateConfig(config); err != nil {
		return edcConfig{}, err
	}
	config.commandSafety = checkConfigSafe(path, opened)
	return config, nil
}

func validateConfigStringTypes(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil || len(document.Content) == 0 {
		return err
	}
	var walk func(*yaml.Node, string) error
	walk = func(node *yaml.Node, prefix string) error {
		if node.Kind == yaml.AliasNode {
			return invalidConfig(prefix, "YAML aliases are not supported")
		}
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for index := 0; index < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Value == "<<" || key.Tag == "!!merge" {
				return invalidConfig(prefix, "YAML merge keys are not supported")
			}
			path := key.Value
			if prefix != "" {
				path = prefix + "." + path
			}
			if expectedTag, exists := configScalarTypes[path]; exists && (value.Kind != yaml.ScalarNode || value.Tag != expectedTag) {
				return invalidConfig(path, "has the wrong type")
			}
			if err := walk(value, path); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(document.Content[0], "")
}

func invalidConfig(field, rule string) error {
	return fmt.Errorf("%s: %s", field, rule)
}

// checkNamePattern은 루틴과 사용자 명령의 이름이다. 결과 id(check.cmd.<이름>)와 명령행 인자에 그대로 쓴다.
var checkNamePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

func validateCheckConfig(config checkConfig) error {
	if err := validateCheckRules("check", config.Accept, config.AcceptUnits, config.AcceptContainers, config.AcceptMounts, config.RequireUnits, config.Limits, nil); err != nil {
		return err
	}
	for name, routine := range config.Routines {
		prefix := "check.routines." + name
		if !checkNamePattern.MatchString(name) {
			return invalidConfig(prefix, "a routine name must use only a-z, 0-9, _ and -")
		}
		for _, id := range routine.Checks {
			if !slices.Contains(checkIDs, id) {
				return invalidConfig(prefix+".checks", fmt.Sprintf("%q is not a check name", id))
			}
		}
		commands := map[string]bool{}
		for index, command := range routine.Commands {
			field := fmt.Sprintf("%s.commands[%d]", prefix, index)
			switch {
			case !checkNamePattern.MatchString(command.Name):
				return invalidConfig(field+".name", "must use only a-z, 0-9, _ and -")
			case commands[command.Name]:
				return invalidConfig(field+".name", fmt.Sprintf("%q is used twice", command.Name))
			case strings.TrimSpace(command.Run) == "":
				return invalidConfig(field+".run", "must not be empty")
			case command.Timeout != nil && command.Timeout.Duration <= 0:
				return invalidConfig(field+".timeout", "must be greater than 0")
			case command.Level != "" && command.Level != "fail" && command.Level != "warn":
				return invalidConfig(field+".level", "must be fail or warn")
			}
			commands[command.Name] = true
		}
		if err := validateCheckRules(prefix, routine.Accept, routine.AcceptUnits, routine.AcceptContainers, routine.AcceptMounts, routine.RequireUnits, routine.Limits, commands); err != nil {
			return err
		}
	}
	return nil
}

// validateCheckRules는 전역 [check]와 루틴이 함께 쓰는 값을 검사한다. commands는 accept에 쓸 수 있는 cmd.<이름>이다.
func validateCheckRules(prefix string, accept, units, containers, mounts, require []string, limits checkLimitsConfig, commands map[string]bool) error {
	for _, id := range accept {
		command, isCommand := strings.CutPrefix(id, "cmd.")
		if !slices.Contains(checkIDs, id) && !(isCommand && commands[command]) {
			return invalidConfig(prefix+".accept", fmt.Sprintf("%q is not a check name", id))
		}
	}
	for field, patterns := range map[string][]string{".accept_units": units, ".accept_containers": containers, ".accept_mounts": mounts} {
		for _, pattern := range patterns {
			if _, err := path.Match(pattern, ""); err != nil || pattern == "" {
				return invalidConfig(prefix+field, fmt.Sprintf("%q is not a valid glob", pattern))
			}
		}
	}
	for _, unit := range require {
		if unit == "" {
			return invalidConfig(prefix+".require_units", "a unit name must not be empty")
		}
	}
	for field, values := range map[string][]float64{".limits.disk": limits.Disk, ".limits.memory_available": limits.MemoryAvailable, ".limits.usage": limits.Usage} {
		if values == nil {
			continue
		}
		// TOML은 nan을 숫자로 읽는다. NaN은 어떤 비교도 거짓이라 범위 검사를 지나고, 판정도 늘 통과가 된다.
		if len(values) != 2 || !(values[0] >= 0 && values[0] <= 100) || !(values[1] >= 0 && values[1] <= 100) {
			return invalidConfig(prefix+field, "must be [warn, fail] with two percentages from 0 to 100")
		}
		if field == ".limits.memory_available" && values[0] < values[1] {
			return invalidConfig(prefix+field, "warn must be at least fail, because it is the share left")
		}
		if field != ".limits.memory_available" && values[0] > values[1] {
			return invalidConfig(prefix+field, "warn must be at most fail")
		}
	}
	return nil
}

func validateConfig(config edcConfig) error {
	if config.Lang != "" {
		if _, ok := normalizeLanguage(config.Lang); !ok {
			return invalidConfig("lang", "must be en, ko, or ja")
		}
	}
	d := config.Defaults
	for field, value := range map[string]*string{
		"defaults.common.json": d.Common.JSON,
		"defaults.top.json":    d.Top.JSON,
	} {
		if value != nil && *value != "" && *value != "-" && !filepath.IsAbs(*value) {
			return invalidConfig(field, "must be an absolute path")
		}
	}
	for field, value := range map[string]*string{
		"defaults.capture.output": d.Capture.Output,
		"defaults.log.output":     d.Log.Output,
	} {
		if value != nil && *value != "" && !filepath.IsAbs(*value) {
			return invalidConfig(field, "must be an absolute path")
		}
	}
	if d.Common.Timeout != nil && d.Common.Timeout.Duration <= 0 {
		return invalidConfig("defaults.common.timeout", "must be greater than 0")
	}
	if d.Doctor.Profile != nil && *d.Doctor.Profile != "default" && *d.Doctor.Profile != "full" {
		return invalidConfig("defaults.doctor.profile", "must be default or full")
	}
	if d.TLS.MinDays != nil && *d.TLS.MinDays < 0 {
		return invalidConfig("defaults.tls.min_days", "must be at least 0")
	}
	if d.HTTP.ExpectStatus != nil && *d.HTTP.ExpectStatus != 0 && (*d.HTTP.ExpectStatus < 100 || *d.HTTP.ExpectStatus > 599) {
		return invalidConfig("defaults.http.expect_status", "must be 0 or between 100 and 599")
	}
	if d.Top.Interval != nil && d.Top.Interval.Duration < topMinInterval {
		return invalidConfig("defaults.top.interval", fmt.Sprintf("must be at least %s", topMinInterval))
	}
	if d.Top.Count != nil && *d.Top.Count < 0 {
		return invalidConfig("defaults.top.count", "must be at least 0")
	}
	if d.Top.Split != nil && *d.Top.Split != "" {
		if _, err := parseTopSplit(*d.Top.Split); err != nil {
			return invalidConfig("defaults.top.split", "must be all, none, or cpu, mem, disk, net, psi, comma-separated, each once")
		}
	}
	if d.Top.Interface != nil {
		if _, err := parseTopInterfaceFilter(*d.Top.Interface); err != nil {
			return invalidConfig("defaults.top.interface", "must be all, or interface names, globs, and physical, comma-separated: "+err.Error())
		}
	}
	if d.Top.View != nil && *d.Top.View != "" {
		if _, err := parseTopView(*d.Top.View); err != nil {
			return invalidConfig("defaults.top.view", "must be all, cpu, memory, disk, network, pressure, or split")
		}
	}
	if d.Top.Panels != nil && *d.Top.Panels != "" {
		if _, err := parseTopPanels(*d.Top.Panels); err != nil {
			return invalidConfig("defaults.top.panels", "must be none, or cpu, mem, disk, net, psi, ai, events, comma-separated, each once")
		}
	}
	if d.Top.Columns != nil && *d.Top.Columns != "" {
		if _, err := parseTopColumns(*d.Top.Columns); err != nil {
			return invalidConfig("defaults.top.columns", "must be all, or net, cpu, mem, disk, psi, ai with an optional +, comma-separated, each once")
		}
	}
	if d.Top.ProcessSort != nil && *d.Top.ProcessSort != "" {
		if _, err := parseTopProcessSort(*d.Top.ProcessSort); err != nil {
			return invalidConfig("defaults.top.process_sort", "must be cpu, mem, or io")
		}
	}
	if d.Info.Timeout != nil && d.Info.Timeout.Duration <= 0 {
		return invalidConfig("defaults.info.timeout", "must be greater than 0")
	}
	if d.Where.Provider != nil && *d.Where.Provider != "all" && *d.Where.Provider != "aws" && *d.Where.Provider != "gcp" {
		return invalidConfig("defaults.where.provider", "must be all, aws, or gcp")
	}
	if d.Where.Count != nil && *d.Where.Count < 1 {
		return invalidConfig("defaults.where.count", "must be at least 1")
	}
	if d.Capture.Duration != nil && (d.Capture.Duration.Duration <= 0 || d.Capture.Duration.Duration > maxCaptureDuration) {
		return invalidConfig("defaults.capture.duration", fmt.Sprintf("must be at most %s", maxCaptureDuration))
	}
	if d.Capture.Count != nil && (*d.Capture.Count <= 0 || *d.Capture.Count > maxCapturePackets) {
		return invalidConfig("defaults.capture.count", fmt.Sprintf("must be between 1 and %d", maxCapturePackets))
	}
	if d.Remote.ConnectTimeout != nil && (d.Remote.ConnectTimeout.Duration <= 0 || d.Remote.ConnectTimeout.Duration > remoteMaxTimeout) {
		return invalidConfig("defaults.remote.connect_timeout", fmt.Sprintf("must be at most %s", remoteMaxTimeout))
	}
	for field, value := range map[string]*string{
		"defaults.remote.inventory": d.Remote.Inventory,
		"defaults.remote.recipe":    d.Remote.Recipe,
	} {
		if value != nil && *value != "" && !filepath.IsAbs(*value) {
			return invalidConfig(field, "must be an absolute path")
		}
	}
	if d.Remote.OutputLimit != nil && (*d.Remote.OutputLimit <= 0 || *d.Remote.OutputLimit > remoteConfigLimit) {
		return invalidConfig("defaults.remote.output_limit", fmt.Sprintf("must be between 1 and %d", remoteConfigLimit))
	}
	if d.Remote.Parallel != nil && (*d.Remote.Parallel < 0 || *d.Remote.Parallel > remoteHostLimit) {
		return invalidConfig("defaults.remote.parallel", fmt.Sprintf("must be between 0 and %d", remoteHostLimit))
	}
	if d.Update.Timeout != nil && d.Update.Timeout.Duration <= 0 {
		return invalidConfig("defaults.update.timeout", "must be greater than 0")
	}
	if d.Du.Workers != nil && *d.Du.Workers < 1 {
		return invalidConfig("defaults.du.workers", "must be at least 1")
	}
	if d.History.Limit != nil && *d.History.Limit < 0 {
		return invalidConfig("defaults.history.limit", "must be at least 0")
	}
	if d.Log.HistoryLimit != nil && (*d.Log.HistoryLimit < 1 || *d.Log.HistoryLimit > logHistoryMaxLimit) {
		return invalidConfig("defaults.log.history_limit", fmt.Sprintf("must be between 1 and %d", logHistoryMaxLimit))
	}
	for field, value := range map[string]*configDuration{
		"defaults.changes.since":  d.Changes.Since,
		"defaults.snapshot.since": d.Snapshot.Since,
		"defaults.events.since":   d.Events.Since,
	} {
		if value != nil && value.Duration <= 0 {
			return invalidConfig(field, "must be greater than 0")
		}
	}
	if d.Quality.Timeout != nil && d.Quality.Timeout.Duration <= 0 {
		return invalidConfig("defaults.quality.timeout", "must be greater than 0")
	}
	if d.Quality.Server != nil && *d.Quality.Server != "" && validateQualityServer(*d.Quality.Server) != nil {
		return invalidConfig("defaults.quality.server", "must be empty or an absolute http or https URL with a host and no user info")
	}
	if d.Log.Stream != nil && *d.Log.Stream != "" && *d.Log.Stream != "stdout" && *d.Log.Stream != "stderr" && *d.Log.Stream != "both" {
		return invalidConfig("defaults.log.stream", "must be stdout, stderr, or both")
	}
	if d.Log.Output != nil && *d.Log.Output == "-" {
		return invalidConfig("defaults.log.output", "must be a file path")
	}
	if d.Log.CommandDisplay != nil && *d.Log.CommandDisplay != "full" && *d.Log.CommandDisplay != "name" && *d.Log.CommandDisplay != "none" {
		return invalidConfig("defaults.log.command_display", "must be full, name, or none")
	}
	for _, id := range config.Audit.Accept {
		if !slices.Contains(auditCheckIDs, id) {
			return invalidConfig("audit.accept", fmt.Sprintf("%q is not an audit check id", id))
		}
	}
	for _, port := range config.Audit.AllowPorts {
		if port < 1 || port > 65535 {
			return invalidConfig("audit.allow_ports", fmt.Sprintf("%d must be between 1 and 65535", port))
		}
	}
	if err := validateCheckConfig(config.Check); err != nil {
		return err
	}

	if err := validateLogPolicy(logOptions{
		maxSizeMB: configuredInt(d.Log.MaxSizeMB, defaultLogMaxSizeMB), keepFiles: configuredInt(d.Log.KeepFiles, defaultLogKeepFiles),
		restart: configuredString(d.Log.Restart, "never"), maxRestarts: configuredInt(d.Log.MaxRestarts, defaultLogMaxRestarts),
		restartDelay: configuredDuration(d.Log.RestartDelay, defaultLogRestartDelay), timeout: configuredDuration(d.Log.Timeout, 0), killAfter: configuredDuration(d.Log.KillAfter, defaultLogKillAfter),
	}); err != nil {
		return fmt.Errorf("defaults.log: %w", err)
	}
	return nil
}

func configuredString(value *string, fallback string) string {
	if value != nil {
		return *value
	}
	return fallback
}
func configuredBool(value *bool, fallback bool) bool {
	if value != nil {
		return *value
	}
	return fallback
}
func configuredInt(value *int, fallback int) int {
	if value != nil {
		return *value
	}
	return fallback
}
func configuredDuration(value *configDuration, fallback time.Duration) time.Duration {
	if value != nil {
		return value.Duration
	}
	return fallback
}

func configuredStringFallback(specific, common *string, fallback string) string {
	return configuredString(specific, configuredString(common, fallback))
}

func configuredBoolFallback(specific, common *bool, fallback bool) bool {
	return configuredBool(specific, configuredBool(common, fallback))
}

func configuredDurationFallback(specific, common *configDuration, fallback time.Duration) time.Duration {
	return configuredDuration(specific, configuredDuration(common, fallback))
}

func configuredCommon(timeout time.Duration) commonOptions {
	c := activeConfig.Defaults.Common
	return commonOptions{
		timeout:  configuredDuration(c.Timeout, timeout),
		jsonPath: configuredString(c.JSON, ""),
		verbose:  configuredBool(c.Verbose, false),
		redact:   configuredBool(c.Redact, false),
	}
}

func boolPointer(value bool) *bool                        { return &value }
func intPointer(value int) *int                           { return &value }
func stringPointer(value string) *string                  { return &value }
func durationPointer(value time.Duration) *configDuration { return &configDuration{Duration: value} }

func recommendedConfig() edcConfig {
	lang := currentLanguage()
	if _, ok := normalizeLanguage(lang); !ok {
		lang = defaultLanguage
	}
	return edcConfig{Lang: lang, Defaults: configDefaults{
		Common: commonConfig{Timeout: durationPointer(15 * time.Second), JSON: stringPointer(""), Verbose: boolPointer(false), Redact: boolPointer(false)},
		Doctor: doctorConfig{Profile: stringPointer("default")}, TLS: tlsConfig{MinDays: intPointer(14)}, HTTP: httpConfig{ExpectStatus: intPointer(200)},
		Top:      topConfig{Interval: durationPointer(2 * time.Second), Count: intPointer(10), NoHeader: boolPointer(false), JSON: stringPointer("")},
		Info:     infoConfig{Public: boolPointer(false), Timeout: durationPointer(3 * time.Second), Verbose: boolPointer(false)},
		Where:    whereConfig{Provider: stringPointer("all"), Count: intPointer(3)},
		Capture:  captureConfig{Interface: stringPointer(""), Duration: durationPointer(15 * time.Second), Count: intPointer(500), Filter: stringPointer(""), Output: stringPointer("")},
		Remote:   remoteConfig{Inventory: stringPointer(""), Recipe: stringPointer(""), ConnectTimeout: durationPointer(10 * time.Second), OutputLimit: intPointer(remoteOutputLimit), Parallel: intPointer(0)},
		Update:   updateConfig{Timeout: durationPointer(60 * time.Second)},
		Quality:  qualityConfig{Timeout: durationPointer(defaultQualityTimeout), Server: stringPointer("")},
		Du:       duConfig{Workers: intPointer(duDefaultWorkers)},
		History:  historyConfig{Limit: intPointer(historyDefaultLimit)},
		Changes:  changesConfig{Since: durationPointer(changesDefaultSince)},
		Snapshot: snapshotConfig{Since: durationPointer(snapshotDefaultSince)},
		Events:   eventsConfig{Since: durationPointer(eventsDefaultSince)},
		Log: logConfig{Stream: stringPointer("both"), Output: stringPointer(""), CommandDisplay: stringPointer("full"),
			MaxSizeMB: intPointer(defaultLogMaxSizeMB), KeepFiles: intPointer(defaultLogKeepFiles), Restart: stringPointer("never"),
			MaxRestarts: intPointer(defaultLogMaxRestarts), RestartDelay: durationPointer(defaultLogRestartDelay), Timeout: durationPointer(0), KillAfter: durationPointer(defaultLogKillAfter), HistoryLimit: intPointer(logHistoryDefaultLimit)},
	}}
}

func recommendedLogOutputPath() string {
	home, _ := os.UserHomeDir()
	return recommendedLogOutputPathFor(runtime.GOOS, home, os.Getenv("XDG_STATE_HOME"))
}

func recommendedLogOutputPathFor(goos, home, stateHome string) string {
	switch goos {
	case "darwin":
		if home != "" {
			return filepath.Join(home, "Library", "Logs", "edc.log")
		}
	case "linux":
		base := stateHome
		if base == "" || !filepath.IsAbs(base) {
			if home == "" {
				return ""
			}
			base = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(base, "edc", "edc.log")
	}
	return ""
}

func mergeConfigSection[T any](existing, fallback T) T {
	baseData, _ := yaml.Marshal(fallback)
	overlayData, _ := yaml.Marshal(existing)
	var baseNode, overlayNode yaml.Node
	_ = yaml.Unmarshal(baseData, &baseNode)
	_ = yaml.Unmarshal(overlayData, &overlayNode)
	mergeYAMLMapping(baseNode.Content[0], overlayNode.Content[0])
	mergedData, _ := yaml.Marshal(baseNode.Content[0])
	var merged T
	_ = yaml.Unmarshal(mergedData, &merged)
	return merged
}

func mergeYAMLMapping(base, overlay *yaml.Node) {
	if base.Kind != yaml.MappingNode || overlay.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index < len(overlay.Content); index += 2 {
		key, value := overlay.Content[index], overlay.Content[index+1]
		found := false
		for baseIndex := 0; baseIndex < len(base.Content); baseIndex += 2 {
			if base.Content[baseIndex].Value != key.Value {
				continue
			}
			found = true
			if base.Content[baseIndex+1].Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
				mergeYAMLMapping(base.Content[baseIndex+1], value)
			} else {
				base.Content[baseIndex+1] = value
			}
			break
		}
		if !found {
			base.Content = append(base.Content, key, value)
		}
	}
}

func configError(path string, err error) error {
	if strings.TrimSpace(path) == "" {
		return err
	}
	return fmt.Errorf("%s: %w", path, err)
}
