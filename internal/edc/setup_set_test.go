package edc

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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
		{[]string{"clean.projects", "~other/work"}, 2, `defaults.clean.projects: "~other/work" is not an absolute path`},
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

// audit과 check는 defaults 밖에 있다. 정수 목록, 실수 목록, check.limits 아래의 키도 한 줄로 바꾼다.
func TestSetupSetReachesAuditAndCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, args := range [][]string{
		{"audit.accept", "net.exposed"},
		{"audit.allow_ports", "--add", "5432", "8080"},
		{"check.require_units", "nginx.service"},
		{"check.limits.disk", "80", "95"},
		{"defaults.du.workers", "3"},
	} {
		if code, _, stderr := setupSetRun(t, path, args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(config.Audit.Accept, ",") != "net.exposed" || len(config.Audit.AllowPorts) != 2 || config.Audit.AllowPorts[1] != 8080 ||
		strings.Join(config.Check.RequireUnits, ",") != "nginx.service" || len(config.Check.Limits.Disk) != 2 || config.Check.Limits.Disk[1] != 95 || *config.Defaults.Du.Workers != 3 {
		t.Fatalf("config %+v", config)
	}
	if code, stdout, _ := setupSetRun(t, path, "audit.allow_ports"); code != 0 || stdout != "5432\n8080\n" {
		t.Fatalf("show ports: %d %q", code, stdout)
	}
	if code, _, _ := setupSetRun(t, path, "audit.allow_ports", "--remove", "5432"); code != 0 {
		t.Fatal("remove port")
	}
	for _, tc := range []struct {
		args []string
		text string
	}{
		{[]string{"audit.allow_ports", "http"}, "audit.allow_ports"},
		{[]string{"audit.allow_ports", "70000"}, "audit.allow_ports"},
		{[]string{"audit.accept", "no.such"}, "audit.accept"},
		{[]string{"notify.webhooks", "x"}, "unknown setting: notify.webhooks"},
		{[]string{"check.routines", "x"}, "unknown setting: check.routines"},
	} {
		if code, _, stderr := setupSetRun(t, path, tc.args...); code != 2 || !strings.Contains(stderr, tc.text) {
			t.Fatalf("%v: %d %q", tc.args, code, stderr)
		}
	}
}

// 경로 키는 ~와 상대 경로를 절대 경로로 바꿔 저장한다. 경로가 아닌 키의 값과 -는 그대로 둔다.
func TestSetupSetResolvesPathValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	t.Chdir(work)
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, args := range [][]string{
		{"clean.projects", "~/work", ".", "sub"},
		{"remote.inventory", "~"},
		{"common.json", "-"},
		{"capture.filter", "./not-a-path"},
	} {
		if code, _, stderr := setupSetRun(t, path, args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
	}
	config, err := loadConfigAt(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(home, "work"), work, filepath.Join(work, "sub")}
	if strings.Join(config.Defaults.Clean.Projects, ",") != strings.Join(want, ",") || *config.Defaults.Remote.Inventory != home ||
		*config.Defaults.Common.JSON != "-" || *config.Defaults.Capture.Filter != "./not-a-path" {
		t.Fatalf("config %+v", config.Defaults)
	}
	if code, _, _ := setupSetRun(t, path, "clean.projects", "--remove", "sub"); code != 0 {
		t.Fatal("--remove resolves the path like --add")
	}
}

func TestSetupListShowsEveryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[defaults.du]\nworkers = 6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runSetupList(&stdout, &stderr, path); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{"lang", "du.workers", "6", "clean.projects", "audit.allow_ports", "check.limits.disk", "log.restart_delay"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, unwanted := range []string{"defaults.du", "notify", "routines"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("unexpected %q in %s", unwanted, text)
		}
	}
	keys := setupSetKeyNames()
	if !slices.Contains(keys, "clean.projects") || !slices.Contains(keys, "check.limits.usage") || slices.Contains(keys, "notify.webhooks") {
		t.Fatalf("keys %v", keys)
	}
	for _, shell := range []string{renderCompletion(zshCompletion, zshCommandList()), renderCompletion(bashCompletion, bashCommandList())} {
		if !strings.Contains(shell, "clean.projects") || strings.Contains(shell, "@@") {
			t.Fatal("completion does not offer the setup keys")
		}
	}
}
