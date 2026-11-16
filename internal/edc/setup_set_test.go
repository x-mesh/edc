package edc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupSetRun(t *testing.T, path string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runSetupSet(args, &stdout, &stderr, path)
	return code, stdout.String(), stderr.String()
}

func TestSetupSetEditsAListAndKeepsOtherValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[defaults.du]\nworkers = 6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		args []string
		want []string
	}{
		{[]string{"clean.projects", "/srv/a", "/srv/b"}, []string{"/srv/a", "/srv/b"}},
		{[]string{"clean.projects", "--add", "/srv/c", "/srv/a"}, []string{"/srv/a", "/srv/b", "/srv/c"}},
		{[]string{"clean.projects", "--remove", "/srv/b"}, []string{"/srv/a", "/srv/c"}},
	}
	for _, step := range steps {
		if code, stdout, stderr := setupSetRun(t, path, step.args...); code != 0 || !strings.Contains(stdout, path) {
			t.Fatalf("%v: %d %q %q", step.args, code, stdout, stderr)
		}
		config, err := loadConfigAt(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(config.Defaults.Clean.Projects, ",") != strings.Join(step.want, ",") {
			t.Fatalf("%v: projects %v", step.args, config.Defaults.Clean.Projects)
		}
		if config.Defaults.Du.Workers == nil || *config.Defaults.Du.Workers != 6 {
			t.Fatalf("%v: du.workers changed: %v", step.args, config.Defaults.Du.Workers)
		}
	}
	if code, stdout, _ := setupSetRun(t, path, "clean.projects"); code != 0 || stdout != "/srv/a\n/srv/c\n" {
		t.Fatalf("show: %d %q", code, stdout)
	}
	if code, _, _ := setupSetRun(t, path, "clean.projects", "--clear"); code != 0 {
		t.Fatalf("clear: %d", code)
	}
	if code, stdout, stderr := setupSetRun(t, path, "clean.projects"); code != 1 || stdout != "" || !strings.Contains(stderr, "clean.projects is not set") {
		t.Fatalf("show after clear: %d %q %q", code, stdout, stderr)
	}
}

func TestSetupSetParsesScalarTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, args := range [][]string{{"du.workers", "8"}, {"common.verbose", "true"}, {"common.timeout", "20s"}, {"doctor.profile", "full"}, {"lang", "ko"}} {
		if code, _, stderr := setupSetRun(t, path, args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	d := config.Defaults
	if *d.Du.Workers != 8 || !*d.Common.Verbose || d.Common.Timeout.Duration.String() != "20s" || *d.Doctor.Profile != "full" || config.Lang != "ko" {
		t.Fatalf("config %+v", config)
	}
	if code, stdout, _ := setupSetRun(t, path, "common.timeout"); code != 0 || stdout != "20s\n" {
		t.Fatalf("show duration: %d %q", code, stdout)
	}
}

// 잘못된 값, 모르는 키, 목록이 아닌 키의 --add는 저장하지 않는다.
func TestSetupSetRefusesBadInputWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[defaults.du]\nworkers = 6\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		code int
		text string
	}{
		{[]string{"clean.projects", "~/work"}, 2, `defaults.clean.projects: "~/work" is not an absolute path`},
		{[]string{"du.workers", "many"}, 2, "du.workers"},
		{[]string{"du.workers", "1", "2"}, 2, "usage"},
		{[]string{"du.nothing", "1"}, 2, "unknown setting: du.nothing"},
		{[]string{"du.workers", "--add", "3"}, 2, "du.workers is not a list"},
		{[]string{"clean.projects", "--remove", "/srv/none"}, 1, "clean.projects does not have /srv/none"},
		{[]string{"doctor.profile", "huge"}, 2, "defaults.doctor.profile"},
	}
	for _, tc := range cases {
		code, _, stderr := setupSetRun(t, path, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.text) {
			t.Fatalf("%v: %d %q", tc.args, code, stderr)
		}
		data, _ := os.ReadFile(path)
		if !bytes.Equal(data, original) {
			t.Fatalf("%v wrote %q", tc.args, data)
		}
	}
}

// 지금 설정 파일이 잘못되었으면 한 줄 설정은 그 파일을 덮지 않는다. 마법사와 달리 무엇을 잃는지 보여 줄 미리보기가 없다.
func TestSetupSetKeepsAnInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[defaults.du]\nworkers = \"six\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := setupSetRun(t, path, "du.workers", "4"); code != 2 || !strings.Contains(stderr, "invalid config") {
		t.Fatalf("%d %q", code, stderr)
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, original) {
		t.Fatalf("wrote %q", data)
	}
}

func TestSetupKeyArgument(t *testing.T) {
	for arg, want := range map[string]bool{"clean.projects": true, "lang": true, "remote": false, "--help": false, "-x.y": false} {
		if setupKeyArgument(arg) != want {
			t.Fatalf("%q", arg)
		}
	}
}
