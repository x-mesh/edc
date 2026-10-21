package edc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x-mesh/edc/internal/remotesetup"
)

func writeInventoryFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func newSetupEditor(t *testing.T, path string) *remotesetup.Editor {
	t.Helper()
	editor, err := remotesetup.New(&remoteInventoryStore{path: path}, remoteSetupValidator{})
	if err != nil {
		t.Fatal(err)
	}
	return editor
}

func TestRemoteInventoryStoreCreatesAPrivateFileThatEdcRemoteLoads(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".edc", "inventory.yaml")
	editor := newSetupEditor(t, path)
	for _, host := range []remotesetup.Host{{Name: "web", Target: "web.example", Tags: []string{"linux"}}, {Name: "db"}} {
		if err := editor.AddHost(host); err != nil {
			t.Fatal(err)
		}
	}
	if err := editor.AddGroup("servers", []string{"web", "db"}); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetParallel(2); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetGroupParallel("servers", 3); err != nil {
		t.Fatal(err)
	}
	if problems, err := editor.Save(); err != nil || len(problems) != 0 {
		t.Fatalf("problems = %v, err = %v", problems, err)
	}
	want := `hosts:
  - name: web
    target: web.example
    tags: [linux]
  - name: db
groups:
  servers:
    - web
    - db
parallel: 2
group_options:
  servers:
    parallel: 3
`
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("file =\n%s\nwant\n%s", data, want)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(root, ".edc")); info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v", info.Mode().Perm())
	}
	loaded, err := loadRemoteInventory(path)
	if err != nil || len(loaded.Hosts) != 2 || loaded.Options["servers"].Parallel != 3 {
		t.Fatalf("edc remote cannot load the file: %+v, %v", loaded, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

const commentedInventory = `# fleet of the team
hosts:
  - name: web   # front door
    target: web.example
    tags: [linux]
  - name: db
groups:
  # everything that serves
  servers:
    - web   # first
    - db
parallel: 2
`

func TestRemoteInventoryStoreKeepsCommentsAndUntouchedEntries(t *testing.T) {
	path := writeInventoryFixture(t, commentedInventory)
	editor := newSetupEditor(t, path)
	if err := editor.AddHost(remotesetup.Host{Name: "cache", Tags: []string{"mac"}}); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetMembers("servers", []string{"web", "db", "cache"}); err != nil {
		t.Fatal(err)
	}
	if err := editor.AddGroup("macs", []string{"cache"}); err != nil {
		t.Fatal(err)
	}
	if problems, err := editor.Save(); err != nil || len(problems) != 0 {
		t.Fatalf("problems = %v, err = %v", problems, err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"# fleet of the team", "# front door", "# everything that serves", "# first", "  - name: cache", "tags: [mac]", "  macs:", "parallel: 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lost %q:\n%s", want, text)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("an existing file must keep its mode, got %v", info.Mode().Perm())
	}
	if _, err := loadRemoteInventory(path); err != nil {
		t.Fatalf("edc remote cannot load the edited file: %v\n%s", err, text)
	}
}

func TestRemoteInventoryStoreRemovesWhatWasDeleted(t *testing.T) {
	path := writeInventoryFixture(t, commentedInventory+"group_options:\n  servers:\n    parallel: 2\n")
	editor := newSetupEditor(t, path)
	editor.RemoveHost(1)
	if err := editor.SetParallel(0); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetGroupParallel("servers", 0); err != nil {
		t.Fatal(err)
	}
	if problems, err := editor.Save(); err != nil || len(problems) != 0 {
		t.Fatalf("problems = %v, err = %v", problems, err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, gone := range []string{"name: db", "- db", "parallel", "group_options"} {
		if strings.Contains(text, gone) {
			t.Errorf("the file still has %q:\n%s", gone, text)
		}
	}
	if !strings.Contains(text, "# front door") {
		t.Errorf("the comment of a kept host was lost:\n%s", text)
	}
	if _, err := loadRemoteInventory(path); err != nil {
		t.Fatalf("edc remote cannot load the edited file: %v\n%s", err, text)
	}
}

func TestRemoteInventoryStoreRefusesAChangedFile(t *testing.T) {
	path := writeInventoryFixture(t, commentedInventory)
	editor := newSetupEditor(t, path)
	if err := editor.AddHost(remotesetup.Host{Name: "cache"}); err != nil {
		t.Fatal(err)
	}
	changed := commentedInventory + "# someone else edited this\n"
	if err := os.WriteFile(path, []byte(changed), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := editor.Save(); !errors.Is(err, remotesetup.ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if data, _ := os.ReadFile(path); string(data) != changed {
		t.Fatalf("a stale save overwrote the file:\n%s", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

func TestRemoteInventoryStoreLoadsEmptyAndRejectsBadFiles(t *testing.T) {
	store := &remoteInventoryStore{path: writeInventoryFixture(t, "")}
	if inventory, _, err := store.Load(); err != nil || len(inventory.Hosts) != 0 {
		t.Fatalf("empty file: %+v, %v", inventory, err)
	}
	for name, content := range map[string]string{
		"unknown key":    "hosts: []\nextra: 1\n",
		"not a mapping":  "- a\n- b\n",
		"two documents":  "hosts: []\n---\nhosts: []\n",
		"wrong type":     "hosts: 3\n",
		"broken yaml":    "hosts: [\n",
		"unknown option": "group_options: {a: {parallel: 1, speed: 2}}\n",
	} {
		store := &remoteInventoryStore{path: writeInventoryFixture(t, content)}
		if _, _, err := store.Load(); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
	if _, _, err := (&remoteInventoryStore{path: t.TempDir()}).Load(); err == nil {
		t.Error("a directory was accepted")
	}
}

func TestRemoteSetupValidatorAppliesTheRulesOfEdcRemote(t *testing.T) {
	validator := remoteSetupValidator{}
	if issue := validator.ValidateHost(remotesetup.Host{Name: "web", Target: "-oProxyCommand=x"}); issue == nil || issue.Code != remoteSetupIssueMessage {
		t.Errorf("a dash target was accepted: %v", issue)
	}
	if issue := validator.ValidateHost(remotesetup.Host{Name: "web", Tags: []string{"a", "a"}}); issue == nil {
		t.Error("a duplicate tag was accepted")
	}
	if issue := validator.ValidateHost(remotesetup.Host{Name: "web", Target: "web.example", Tags: []string{"linux"}}); issue != nil {
		t.Errorf("a valid host was rejected: %v", issue)
	}
	if issue := validator.ValidateGroupName("run"); issue == nil {
		t.Error("a reserved group name was accepted")
	}
	if issue := validator.ValidateGroupName("daily"); issue != nil {
		t.Errorf("a valid group name was rejected: %v", issue)
	}
	if issues := validator.ValidateInventory(remotesetup.Inventory{}); len(issues) != 1 {
		t.Errorf("an empty inventory = %v", issues)
	}
	valid := remotesetup.Inventory{Hosts: []remotesetup.Host{{Name: "web"}}, Groups: []remotesetup.Group{{Name: "daily", Members: []string{"web"}}}, Parallel: 2}
	if issues := validator.ValidateInventory(valid); len(issues) != 0 {
		t.Errorf("a valid inventory = %v", issues)
	}
	valid.GroupParallel = map[string]int{"missing": 1}
	if issues := validator.ValidateInventory(valid); len(issues) != 1 {
		t.Errorf("an option for a missing group = %v", issues)
	}
}

func TestRemoteSetupPathPrecedence(t *testing.T) {
	cwd := t.TempDir()
	configDir := t.TempDir()
	configured := edcConfig{Defaults: configDefaults{Remote: remoteConfig{Inventory: stringPointer("/etc/edc/fleet.yaml")}}}
	if got := remoteSetupPath("/tmp/flag.yaml", configured, cwd, configDir); got != "/tmp/flag.yaml" {
		t.Errorf("flag = %q", got)
	}
	if got := remoteSetupPath("", configured, cwd, configDir); got != "/etc/edc/fleet.yaml" {
		t.Errorf("config = %q", got)
	}
	if got := remoteSetupPath("", edcConfig{}, cwd, configDir); got != filepath.Join(cwd, ".edc", "inventory.yaml") {
		t.Errorf("default = %q", got)
	}
	found := filepath.Join(cwd, "inventory.yaml")
	if err := os.WriteFile(found, []byte("hosts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := remoteSetupPath("", edcConfig{}, cwd, configDir); got != found {
		t.Errorf("discovered = %q", got)
	}
}
