package edc

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeConfigFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigRoundTripAndLegacyLanguage(t *testing.T) {
	path := writeConfigFixture(t, `lang: ko
defaults:
  common: {timeout: 15s, json: "", verbose: false, redact: true}
  top: {interval: 2s, count: 10, no_header: false, json: ""}
  log: {stream: stderr, output: /tmp/job.log, command_display: name}
`)
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Lang != "ko" || config.Defaults.Common.Timeout.Duration != 15*time.Second || *config.Defaults.Log.CommandDisplay != "name" {
		t.Fatalf("config = %#v", config)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	second := writeConfigFixture(t, string(data))
	if _, err := loadConfigAt(second); err != nil {
		t.Fatalf("round trip: %v\n%s", err, data)
	}

	legacy, err := loadConfigAt(writeConfigFixture(t, "lang: ja\n"))
	if err != nil || legacy.Lang != "ja" {
		t.Fatalf("legacy: config=%#v err=%v", legacy, err)
	}
	missing, err := loadConfigAt(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || missing.Lang != "" {
		t.Fatalf("missing: config=%#v err=%v", missing, err)
	}
	empty, err := loadConfigAt(writeConfigFixture(t, ""))
	if err != nil || empty.Lang != "" {
		t.Fatalf("empty: config=%#v err=%v", empty, err)
	}
}

func TestRecommendedLogOutputPathFollowsThePlatform(t *testing.T) {
	if got := recommendedLogOutputPathFor("darwin", "/Users/one", ""); got != "/Users/one/Library/Logs/edc.log" {
		t.Fatalf("darwin path = %q", got)
	}
	if got := recommendedLogOutputPathFor("linux", "/home/one", "/state"); got != "/state/edc/edc.log" {
		t.Fatalf("linux XDG path = %q", got)
	}
	if got := recommendedLogOutputPathFor("linux", "/home/one", "relative"); got != "/home/one/.local/state/edc/edc.log" {
		t.Fatalf("linux fallback path = %q", got)
	}
	if got := recommendedLogOutputPathFor("windows", "C:/Users/one", ""); got != "" {
		t.Fatalf("unsupported path = %q", got)
	}
}

func TestConfigRejectsUnknownTypeAndRange(t *testing.T) {
	for _, content := range []string{
		"defaults: {log: {unknown: true}}\n",
		"defaults: {top: {count: nope}}\n",
		"defaults: {http: {expect_status: 99}}\n",
		"defaults: {top: {interval: 10ms}}\n",
		"defaults: {remote: {parallel: -1}}\n",
		"defaults: {common: {json: false}}\n",
		"defaults: {remote: {inventory: false}}\n",
		"defaults: {log: {output: false}}\n",
		"defaults: {top: {count: 1.9}}\n",
		"defaults: {common: {verbose: yes}}\n",
		"defaults: {common: {json: report.json}}\n",
		"defaults: {top: {json: report.json}}\n",
		"defaults: {capture: {output: capture.pcap}}\n",
		"defaults: {capture: {output: '-'}}\n",
		"defaults: {log: {output: job.log}}\n",
		"defaults: {remote: {inventory: inventory.yaml}}\n",
		"defaults: {remote: {group: daily}}\n",
		"defaults:\n  capture:\n    <<: &bad\n      filter: false\n",
		"shared: &bad false\ndefaults: {capture: {filter: *bad}}\n",
	} {
		if _, err := loadConfigAt(writeConfigFixture(t, content)); err == nil {
			t.Errorf("accepted invalid config: %s", content)
		}
	}
	if _, err := loadConfigAt(writeConfigFixture(t, "defaults: {http: {expect_status: 0}}\n")); err != nil {
		t.Fatalf("expect_status 0 must disable the check: %v", err)
	}
}

func TestConfigRejectsOversizedAndNonRegularFiles(t *testing.T) {
	oversized := writeConfigFixture(t, strings.Repeat("#", maxConfigBytes+1))
	if _, err := loadConfigAt(oversized); err == nil {
		t.Fatal("oversized config was accepted")
	}
	link := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Symlink(oversized, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigAt(link); err == nil {
		t.Fatal("symlink config was accepted")
	}
}

func TestConfigPrecedence(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	parse := func(args []string) commonOptions {
		options := configuredCommon(9 * time.Second)
		set := flag.NewFlagSet("test", flag.ContinueOnError)
		set.SetOutput(io.Discard)
		bindCommon(set, &options)
		if err := set.Parse(args); err != nil {
			t.Fatal(err)
		}
		return options
	}
	activeConfig = edcConfig{}
	builtIn := parse(nil)
	if builtIn.timeout != 9*time.Second || builtIn.verbose || !builtIn.redact {
		t.Fatalf("built-in = %#v", builtIn)
	}
	activeConfig.Defaults.Common = commonConfig{Timeout: durationPointer(12 * time.Second), JSON: stringPointer("report.json"), Verbose: boolPointer(true), Redact: boolPointer(true)}
	configured := parse(nil)
	if configured.timeout != 12*time.Second || configured.jsonPath != "report.json" || !configured.verbose || !configured.redact {
		t.Fatalf("configured = %#v", configured)
	}
	explicit := parse([]string{"--timeout", "2s", "--json", "-", "--verbose=false", "--redact=false"})
	if explicit.timeout != 2*time.Second || explicit.jsonPath != "-" || explicit.verbose || explicit.redact {
		t.Fatalf("explicit = %#v", explicit)
	}
}

func TestCommandSpecificConfigPrecedence(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	activeConfig.Defaults.TLS.MinDays = intPointer(14)
	activeConfig.Defaults.HTTP.ExpectStatus = intPointer(204)
	activeConfig.Defaults.Remote.Parallel = intPointer(7)

	var minDays, expectStatus, parallel int
	tlsFlags := flag.NewFlagSet("tls", flag.ContinueOnError)
	minDays = configuredInt(activeConfig.Defaults.TLS.MinDays, 0)
	tlsFlags.IntVar(&minDays, "min-days", minDays, "")
	if err := tlsFlags.Parse(nil); err != nil || minDays != 14 {
		t.Fatalf("tls config default = %d, err=%v", minDays, err)
	}
	if err := tlsFlags.Parse([]string{"--min-days", "3"}); err != nil || minDays != 3 {
		t.Fatalf("tls CLI override = %d, err=%v", minDays, err)
	}

	httpFlags := flag.NewFlagSet("http", flag.ContinueOnError)
	expectStatus = configuredInt(activeConfig.Defaults.HTTP.ExpectStatus, 0)
	httpFlags.IntVar(&expectStatus, "expect-status", expectStatus, "")
	if err := httpFlags.Parse(nil); err != nil || expectStatus != 204 {
		t.Fatalf("http config default = %d, err=%v", expectStatus, err)
	}

	remoteFlags := flag.NewFlagSet("remote", flag.ContinueOnError)
	parallel = configuredInt(activeConfig.Defaults.Remote.Parallel, 0)
	remoteFlags.IntVar(&parallel, "parallel", parallel, "")
	if err := remoteFlags.Parse(nil); err != nil || parallel != 7 {
		t.Fatalf("remote config default = %d, err=%v", parallel, err)
	}
}

func TestNestedOptionsFallBackToCommonConfig(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	activeConfig.Defaults.Common = commonConfig{Timeout: durationPointer(22 * time.Second), JSON: stringPointer("common.json"), Verbose: boolPointer(true)}
	if got := configuredDurationFallback(activeConfig.Defaults.Info.Timeout, activeConfig.Defaults.Common.Timeout, 3*time.Second); got != 22*time.Second {
		t.Fatalf("info timeout fallback = %s", got)
	}
	if got := configuredBoolFallback(activeConfig.Defaults.Info.Verbose, activeConfig.Defaults.Common.Verbose, false); !got {
		t.Fatal("info verbose did not fall back to common")
	}
	if got := configuredStringFallback(activeConfig.Defaults.Top.JSON, activeConfig.Defaults.Common.JSON, ""); got != "common.json" {
		t.Fatalf("top JSON fallback = %q", got)
	}
	activeConfig.Defaults.Info.Timeout = durationPointer(4 * time.Second)
	if got := configuredDurationFallback(activeConfig.Defaults.Info.Timeout, activeConfig.Defaults.Common.Timeout, 3*time.Second); got != 4*time.Second {
		t.Fatalf("specific timeout = %s", got)
	}
}

func TestLogUsesConfigDefaults(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	directory := t.TempDir()
	configuredPath := filepath.Join(directory, "configured.log")
	overridePath := filepath.Join(directory, "override.log")
	activeConfig.Defaults.Log = logConfig{Stream: stringPointer("stdout"), Output: stringPointer(configuredPath), CommandDisplay: stringPointer("none")}
	command := logHelperCommand("emit")
	var stdout, stderr strings.Builder
	args := append([]string{"--"}, command...)
	if code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}); code != 0 {
		t.Fatalf("short syntax exit=%d stderr=%q", code, stderr.String())
	}
	content, err := os.ReadFile(configuredPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "stdout-data") || strings.Contains(string(content), "command=") || stderr.String() != "stderr-data" {
		t.Fatalf("configured log=%q stderr=%q", content, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	args = append([]string{"--stream", "stderr", "--output", overridePath, "--command-display", "name", "--"}, command...)
	if code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}); code != 0 {
		t.Fatalf("override exit=%d stderr=%q", code, stderr.String())
	}
	content, err = os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "stderr-data") || !strings.Contains(string(content), "command=") || stdout.String() != "stdout-data" {
		t.Fatalf("override log=%q stdout=%q", content, stdout.String())
	}
}
