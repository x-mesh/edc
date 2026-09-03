package edc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxConfigBytes = 1024 * 1024

var configScalarTypes = map[string]string{
	"lang": "!!str", "defaults.common.json": "!!str", "defaults.doctor.profile": "!!str",
	"defaults.top.json": "!!str", "defaults.where.provider": "!!str",
	"defaults.capture.interface": "!!str", "defaults.capture.filter": "!!str", "defaults.capture.output": "!!str",
	"defaults.remote.inventory": "!!str", "defaults.remote.recipe": "!!str",
	"defaults.log.stream": "!!str", "defaults.log.output": "!!str", "defaults.log.command_display": "!!str",
	"defaults.common.verbose": "!!bool", "defaults.common.redact": "!!bool",
	"defaults.tls.min_days": "!!int", "defaults.http.expect_status": "!!int",
	"defaults.top.count": "!!int", "defaults.top.no_header": "!!bool",
	"defaults.info.public": "!!bool", "defaults.info.verbose": "!!bool",
	"defaults.where.count": "!!int", "defaults.capture.count": "!!int",
	"defaults.remote.output_limit": "!!int", "defaults.remote.parallel": "!!int",
}

// configDuration keeps durations human-readable in YAML while still validating them at load time.
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

type edcConfig struct {
	Lang     string         `yaml:"lang,omitempty"`
	Defaults configDefaults `yaml:"defaults,omitempty"`
}

type configDefaults struct {
	Common  commonConfig  `yaml:"common,omitempty"`
	Doctor  doctorConfig  `yaml:"doctor,omitempty"`
	TLS     tlsConfig     `yaml:"tls,omitempty"`
	HTTP    httpConfig    `yaml:"http,omitempty"`
	Top     topConfig     `yaml:"top,omitempty"`
	Info    infoConfig    `yaml:"info,omitempty"`
	Where   whereConfig   `yaml:"where,omitempty"`
	Capture captureConfig `yaml:"capture,omitempty"`
	Remote  remoteConfig  `yaml:"remote,omitempty"`
	Update  updateConfig  `yaml:"update,omitempty"`
	Log     logConfig     `yaml:"log,omitempty"`
}

type commonConfig struct {
	Timeout *configDuration `yaml:"timeout,omitempty"`
	JSON    *string         `yaml:"json,omitempty"`
	Verbose *bool           `yaml:"verbose,omitempty"`
	Redact  *bool           `yaml:"redact,omitempty"`
}
type doctorConfig struct {
	Profile *string `yaml:"profile,omitempty"`
}
type tlsConfig struct {
	MinDays *int `yaml:"min_days,omitempty"`
}
type httpConfig struct {
	ExpectStatus *int `yaml:"expect_status,omitempty"`
}
type topConfig struct {
	Interval *configDuration `yaml:"interval,omitempty"`
	Count    *int            `yaml:"count,omitempty"`
	NoHeader *bool           `yaml:"no_header,omitempty"`
	JSON     *string         `yaml:"json,omitempty"`
}
type infoConfig struct {
	Public  *bool           `yaml:"public,omitempty"`
	Timeout *configDuration `yaml:"timeout,omitempty"`
	Verbose *bool           `yaml:"verbose,omitempty"`
}
type whereConfig struct {
	Provider *string `yaml:"provider,omitempty"`
	Count    *int    `yaml:"count,omitempty"`
}
type captureConfig struct {
	Interface *string         `yaml:"interface,omitempty"`
	Duration  *configDuration `yaml:"duration,omitempty"`
	Count     *int            `yaml:"count,omitempty"`
	Filter    *string         `yaml:"filter,omitempty"`
	Output    *string         `yaml:"output,omitempty"`
}
type remoteConfig struct {
	Inventory      *string         `yaml:"inventory,omitempty"`
	Recipe         *string         `yaml:"recipe,omitempty"`
	ConnectTimeout *configDuration `yaml:"connect_timeout,omitempty"`
	OutputLimit    *int            `yaml:"output_limit,omitempty"`
	Parallel       *int            `yaml:"parallel,omitempty"`
}
type updateConfig struct {
	Timeout *configDuration `yaml:"timeout,omitempty"`
}
type logConfig struct {
	Stream         *string `yaml:"stream,omitempty"`
	Output         *string `yaml:"output,omitempty"`
	CommandDisplay *string `yaml:"command_display,omitempty"`
}

var activeConfig edcConfig

func loadConfigAt(path string) (edcConfig, error) {
	var config edcConfig
	if path == "" {
		return config, nil
	}
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
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return config, err
	}
	if len(data) > maxConfigBytes {
		return config, fmt.Errorf("config exceeds %d bytes", maxConfigBytes)
	}
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
	if err := validateConfig(config); err != nil {
		return edcConfig{}, err
	}
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
	if d.Log.Stream != nil && *d.Log.Stream != "" && *d.Log.Stream != "stdout" && *d.Log.Stream != "stderr" {
		return invalidConfig("defaults.log.stream", "must be stdout or stderr")
	}
	if d.Log.Output != nil && *d.Log.Output == "-" {
		return invalidConfig("defaults.log.output", "must be a file path")
	}
	if d.Log.CommandDisplay != nil && *d.Log.CommandDisplay != "full" && *d.Log.CommandDisplay != "name" && *d.Log.CommandDisplay != "none" {
		return invalidConfig("defaults.log.command_display", "must be full, name, or none")
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
		redact:   configuredBool(c.Redact, true),
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
		Common: commonConfig{Timeout: durationPointer(15 * time.Second), JSON: stringPointer(""), Verbose: boolPointer(false), Redact: boolPointer(true)},
		Doctor: doctorConfig{Profile: stringPointer("default")}, TLS: tlsConfig{MinDays: intPointer(14)}, HTTP: httpConfig{ExpectStatus: intPointer(200)},
		Top:     topConfig{Interval: durationPointer(2 * time.Second), Count: intPointer(10), NoHeader: boolPointer(false), JSON: stringPointer("")},
		Info:    infoConfig{Public: boolPointer(false), Timeout: durationPointer(3 * time.Second), Verbose: boolPointer(false)},
		Where:   whereConfig{Provider: stringPointer("all"), Count: intPointer(3)},
		Capture: captureConfig{Interface: stringPointer(""), Duration: durationPointer(15 * time.Second), Count: intPointer(500), Filter: stringPointer(""), Output: stringPointer("")},
		Remote:  remoteConfig{Inventory: stringPointer(""), Recipe: stringPointer(""), ConnectTimeout: durationPointer(10 * time.Second), OutputLimit: intPointer(remoteOutputLimit), Parallel: intPointer(0)},
		Update:  updateConfig{Timeout: durationPointer(60 * time.Second)},
		Log:     logConfig{Stream: stringPointer("stderr"), Output: stringPointer("/var/log/job.log"), CommandDisplay: stringPointer("full")},
	}}
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
