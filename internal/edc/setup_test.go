package edc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupMinimalInput(save string) string {
	// language, eleven configure prompts, final save prompt.
	return "\n" + strings.Repeat("n\n", 11) + save + "\n"
}

func TestSetupCreatesConfigWithSecureModes(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "edc")
	path := filepath.Join(directory, "config.yaml")
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(setupMinimalInput("y")), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Lang == "" || config.Defaults.Log.Stream != nil || config.Defaults.Top.Count != nil {
		t.Fatalf("config=%#v", config)
	}
	fileInfo, _ := os.Stat(path)
	directoryInfo, _ := os.Stat(directory)
	if fileInfo.Mode().Perm() != 0o600 || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("modes file=%o dir=%o", fileInfo.Mode().Perm(), directoryInfo.Mode().Perm())
	}
	if !strings.Contains(output.String(), "---") || !strings.Contains(output.String(), path) {
		t.Fatalf("preview=%q", output.String())
	}
}

func TestSetupOnlyAddsSelectedSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edc", "config.yaml")
	// language; common no; log yes and retain its three recommended values; remaining sections no; save yes.
	input := "\nn\ny\n\n\n\n" + strings.Repeat("n\n", 9) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Defaults.Log.Stream == nil || *config.Defaults.Log.Stream != "stderr" || config.Defaults.Top.Count != nil || config.Defaults.Info.Public != nil {
		t.Fatalf("selected sections leaked defaults: %#v", config.Defaults)
	}
}

func TestSetupCanClearOptionalDefault(t *testing.T) {
	path := writeConfigFixture(t, "lang: en\ndefaults:\n  log: {stream: stdout, output: /tmp/old.log, command_display: name}\n")
	input := "\nn\ny\n\n!clear\n\n" + strings.Repeat("n\n", 9) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Defaults.Log.Output != nil || config.Defaults.Log.Stream == nil || *config.Defaults.Log.Stream != "stdout" {
		t.Fatalf("optional default was not cleared: %#v", config.Defaults.Log)
	}
}

func TestSetupCanClearTypedDefault(t *testing.T) {
	path := writeConfigFixture(t, "lang: en\ndefaults:\n  top: {interval: 2s, count: 10, no_header: true}\n")
	// language; common/log no; top yes; clear interval/count/bool and retain JSON; remaining sections no; save.
	input := "\nn\nn\ny\n!clear\n!clear\n!clear\n\n" + strings.Repeat("n\n", 8) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Defaults.Top.Interval != nil || config.Defaults.Top.Count != nil || config.Defaults.Top.NoHeader != nil {
		t.Fatalf("typed defaults remain: %#v", config.Defaults.Top)
	}
}

func TestSetupUpdatesLogAndPreservesExistingValues(t *testing.T) {
	path := writeConfigFixture(t, "lang: ja\ndefaults:\n  common: {timeout: 33s}\n  log: {stream: stdout, output: /tmp/old.log, command_display: name}\n")
	// language retain; common no; log yes + three values; remaining nine sections no; save yes.
	input := "\nn\ny\nstderr\n/tmp/new.log\nnone\n" + strings.Repeat("n\n", 9) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Lang != "ja" || config.Defaults.Common.Timeout.Duration.String() != "33s" || *config.Defaults.Log.Output != "/tmp/new.log" || *config.Defaults.Log.CommandDisplay != "none" {
		t.Fatalf("config=%#v", config)
	}
}

func TestSetupMigratesTheLegacyRecommendedLogPath(t *testing.T) {
	path := writeConfigFixture(t, "lang: en\ndefaults:\n  log: {stream: stderr, output: /var/log/job.log, command_display: full}\n")
	// language; common no; log yes, retain all migrated values; remaining sections no; save.
	input := "\nn\ny\n\n\n\n" + strings.Repeat("n\n", 9) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Defaults.Log.Output == nil || *config.Defaults.Log.Output != recommendedLogOutputPath() {
		t.Fatalf("legacy output was not migrated: %#v", config.Defaults.Log.Output)
	}
}

func TestSetupRepromptsInvalidValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edc", "config.yaml")
	// invalid language is followed by ko, then skip sections and save.
	input := "fr\nko\n" + strings.Repeat("n\n", 11) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(output.String(), T("cli.setup.invalid_value", T("cli.setup.validation.language"))) {
		t.Fatalf("no reprompt message: %q", output.String())
	}
}

func TestSetupCancelAndNonTTY(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edc", "config.yaml")
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(setupMinimalInput("n")), &output, &stderr, true, path); code != 4 {
		t.Fatalf("cancel exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancel wrote config: %v", err)
	}
	output.Reset()
	stderr.Reset()
	if code := runSetupWithIO(nil, strings.NewReader(""), &output, &stderr, false, path); code != 2 || !strings.Contains(stderr.String(), T("cli.setup.terminal_required")) {
		t.Fatalf("non-TTY exit=%d stderr=%q", code, stderr.String())
	}
}

func TestSetupEOFCancelsImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edc", "config.yaml")
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(""), &output, &stderr, true, path); code != 4 {
		t.Fatalf("EOF exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
}

func TestSetupRecoversInvalidConfigAndSwitchesLanguage(t *testing.T) {
	path := writeConfigFixture(t, "lang: ko\ndefaults: {top: {interval: 10ms}}\n")
	input := "ja\n" + strings.Repeat("n\n", 11) + "y\n"
	var output, stderr strings.Builder
	if code := runSetupWithIO(nil, strings.NewReader(input), &output, &stderr, true, path); code != 0 {
		t.Fatalf("exit=%d stderr=%q output=%q", code, stderr.String(), output.String())
	}
	if !strings.Contains(stderr.String(), path) {
		t.Fatalf("missing recovery warning: %q", stderr.String())
	}
	restore := currentLanguage()
	setLanguage("ja")
	wantPrompt := T("cli.setup.section.common")
	setLanguage(restore)
	if !strings.Contains(output.String(), wantPrompt) {
		t.Fatalf("language did not switch: %q", output.String())
	}
}

func TestSetupRejectsStaleWrite(t *testing.T) {
	path := writeConfigFixture(t, "lang: en\n")
	original, err := configFileSnapshot(path)
	if err != nil || !original.exists {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("lang: ja\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigIfUnchanged(path, []byte("lang: ko\n"), original); !errors.Is(err, errConfigChanged) {
		t.Fatalf("stale write error = %v", err)
	}
}

func TestSetupSnapshotsOversizedAndNonRegularConfigWithoutReading(t *testing.T) {
	oversized := writeConfigFixture(t, strings.Repeat("#", maxConfigBytes+1))
	snapshot, err := configFileSnapshot(oversized)
	if err != nil || !snapshot.exists || snapshot.hashed {
		t.Fatalf("oversized snapshot=%#v err=%v", snapshot, err)
	}
	link := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Symlink(oversized, link); err != nil {
		t.Fatal(err)
	}
	snapshot, err = configFileSnapshot(link)
	if err != nil || !snapshot.exists || snapshot.hashed || snapshot.mode.IsRegular() {
		t.Fatalf("non-regular snapshot=%#v err=%v", snapshot, err)
	}
}
