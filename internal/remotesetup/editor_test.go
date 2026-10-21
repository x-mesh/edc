package remotesetup

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type memoryStore struct {
	inventory Inventory
	revision  int
	saves     int
	stale     bool
}

func (store *memoryStore) Load() (Inventory, Revision, error) {
	return store.inventory.Clone(), store.revision, nil
}

func (store *memoryStore) Save(inventory Inventory, revision Revision) error {
	if store.stale || revision != store.revision {
		return ErrStale
	}
	store.inventory = inventory.Clone()
	store.revision++
	store.saves++
	return nil
}

// rulesValidator stands in for a consumer: it rejects a target that starts with a dash,
// the reserved group name, and an inventory without a host or a group.
type rulesValidator struct{}

func (rulesValidator) ValidateHost(host Host) *Issue {
	if strings.HasPrefix(host.Target, "-") {
		return &Issue{Code: "test.target_dash", Args: []any{host.Name}}
	}
	return nil
}

func (rulesValidator) ValidateGroupName(name string) *Issue {
	if name == "run" {
		return &Issue{Code: "test.reserved", Args: []any{name}}
	}
	return nil
}

func (rulesValidator) ValidateInventory(inventory Inventory) []Issue {
	var issues []Issue
	if len(inventory.Hosts) == 0 {
		issues = append(issues, Issue{Code: "test.no_host"})
	}
	if len(inventory.Groups) == 0 {
		issues = append(issues, Issue{Code: "test.no_group"})
	}
	return issues
}

func newTestEditor(t *testing.T, inventory Inventory) (*Editor, *memoryStore) {
	t.Helper()
	store := &memoryStore{inventory: inventory}
	editor, err := New(store, rulesValidator{})
	if err != nil {
		t.Fatal(err)
	}
	return editor, store
}

func sampleInventory() Inventory {
	return Inventory{
		Hosts:  []Host{{Name: "web", Target: "web.example", Tags: []string{"linux"}}, {Name: "db"}, {Name: "mac", Tags: []string{"mac"}}},
		Groups: []Group{{Name: "servers", Members: []string{"web", "db"}}, {Name: "all", Members: []string{"web", "db", "mac"}}},
	}
}

func issueCode(err error) string {
	var issue Issue
	if errors.As(err, &issue) {
		return issue.Code
	}
	return ""
}

func TestEditorAddHostRejectsEmptyDuplicateAndConsumerRules(t *testing.T) {
	editor, _ := newTestEditor(t, sampleInventory())
	for _, tc := range []struct {
		host Host
		code string
	}{
		{Host{Name: "  "}, IssueHostNameEmpty},
		{Host{Name: "web"}, IssueHostDuplicate},
		{Host{Name: "new", Target: "-oProxyCommand=x"}, "test.target_dash"},
	} {
		if got := issueCode(editor.AddHost(tc.host)); got != tc.code {
			t.Errorf("AddHost(%+v) = %q, want %q", tc.host, got, tc.code)
		}
	}
	if err := editor.AddHost(Host{Name: " cache ", Target: " cache.example "}); err != nil {
		t.Fatal(err)
	}
	if got := editor.Inventory().Hosts[3]; got.Name != "cache" || got.Target != "cache.example" {
		t.Fatalf("host was not trimmed: %+v", got)
	}
}

func TestEditorRenameKeepsGroupMembership(t *testing.T) {
	editor, _ := newTestEditor(t, sampleInventory())
	if err := editor.UpdateHost(0, Host{Name: "front", Target: "web.example"}); err != nil {
		t.Fatal(err)
	}
	inventory := editor.Inventory()
	if !reflect.DeepEqual(inventory.Groups[0].Members, []string{"front", "db"}) || !reflect.DeepEqual(inventory.Groups[1].Members, []string{"front", "db", "mac"}) {
		t.Fatalf("groups = %+v", inventory.Groups)
	}
	if got := issueCode(editor.UpdateHost(0, Host{Name: "db"})); got != IssueHostDuplicate {
		t.Fatalf("renaming onto another host = %q", got)
	}
	if err := editor.UpdateHost(0, Host{Name: "front", Target: "other.example"}); err != nil {
		t.Fatalf("keeping the same name must not count as a duplicate: %v", err)
	}
}

func TestEditorRemoveHostDropsGroupsItEmpties(t *testing.T) {
	inventory := sampleInventory()
	inventory.Groups = append(inventory.Groups, Group{Name: "macs", Members: []string{"mac"}})
	inventory.GroupParallel = map[string]int{"macs": 2, "all": 3}
	editor, _ := newTestEditor(t, inventory)
	removed := editor.RemoveHost(2)
	if !reflect.DeepEqual(removed, []string{"macs"}) {
		t.Fatalf("removed groups = %v", removed)
	}
	got := editor.Inventory()
	if got.GroupIndex("macs") >= 0 || got.HostIndex("mac") >= 0 {
		t.Fatalf("inventory = %+v", got)
	}
	if _, exists := got.GroupParallel["macs"]; exists || got.GroupParallel["all"] != 3 {
		t.Fatalf("group parallel = %v", got.GroupParallel)
	}
	if !reflect.DeepEqual(got.Groups[1].Members, []string{"web", "db"}) {
		t.Fatalf("all = %v", got.Groups[1].Members)
	}
}

func TestEditorGroups(t *testing.T) {
	editor, _ := newTestEditor(t, sampleInventory())
	for _, tc := range []struct {
		name    string
		members []string
		code    string
	}{
		{"", []string{"web"}, IssueGroupNameEmpty},
		{"run", []string{"web"}, "test.reserved"},
		{"servers", []string{"web"}, IssueGroupDuplicate},
		{"empty", nil, IssueGroupNoMembers},
		{"ghost", []string{"nobody"}, IssueHostUnknown},
		{"twice", []string{"web", "web"}, IssueMemberDuplicate},
	} {
		if got := issueCode(editor.AddGroup(tc.name, tc.members)); got != tc.code {
			t.Errorf("AddGroup(%q, %v) = %q, want %q", tc.name, tc.members, got, tc.code)
		}
	}
	if err := editor.AddGroup("web-only", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetMembers("web-only", []string{"web", "mac"}); err != nil {
		t.Fatal(err)
	}
	if got := issueCode(editor.SetMembers("missing", []string{"web"})); got != IssueGroupUnknown {
		t.Fatalf("SetMembers on a missing group = %q", got)
	}
	editor.RemoveGroup("web-only")
	if editor.Inventory().GroupIndex("web-only") >= 0 {
		t.Fatal("group was not removed")
	}
}

func TestEditorParallel(t *testing.T) {
	editor, _ := newTestEditor(t, sampleInventory())
	if got := issueCode(editor.SetParallel(-1)); got != IssueParallelNegative {
		t.Fatalf("negative parallel = %q", got)
	}
	if err := editor.SetParallel(4); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetGroupParallel("servers", 2); err != nil {
		t.Fatal(err)
	}
	if got := editor.Inventory(); got.Parallel != 4 || got.GroupParallel["servers"] != 2 {
		t.Fatalf("inventory = %+v", got)
	}
	if err := editor.SetGroupParallel("servers", 0); err != nil {
		t.Fatal(err)
	}
	if _, exists := editor.Inventory().GroupParallel["servers"]; exists {
		t.Fatal("zero must remove the override")
	}
	if got := issueCode(editor.SetGroupParallel("missing", 1)); got != IssueGroupUnknown {
		t.Fatalf("parallel on a missing group = %q", got)
	}
}

func TestEditorSaveValidatesThenStores(t *testing.T) {
	editor, store := newTestEditor(t, Inventory{})
	if editor.Dirty() {
		t.Fatal("a freshly loaded inventory is not dirty")
	}
	problems, err := editor.Save()
	if err != nil || len(problems) != 2 || store.saves != 0 {
		t.Fatalf("problems = %v, err = %v, saves = %d", problems, err, store.saves)
	}
	if err := editor.AddHost(Host{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := editor.AddGroup("servers", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if !editor.Dirty() {
		t.Fatal("an edited inventory is dirty")
	}
	if problems, err := editor.Save(); err != nil || len(problems) != 0 {
		t.Fatalf("problems = %v, err = %v", problems, err)
	}
	if editor.Dirty() || store.saves != 1 || len(store.inventory.Hosts) != 1 {
		t.Fatalf("dirty = %v, saves = %d, stored = %+v", editor.Dirty(), store.saves, store.inventory)
	}
	// The editor must hold the new revision, or a second save would look stale.
	if err := editor.AddHost(Host{Name: "db"}); err != nil {
		t.Fatal(err)
	}
	if _, err := editor.Save(); err != nil || store.saves != 2 {
		t.Fatalf("second save: err = %v, saves = %d", err, store.saves)
	}
}

func TestEditorSaveReportsAStaleStore(t *testing.T) {
	editor, store := newTestEditor(t, sampleInventory())
	store.stale = true
	if err := editor.AddHost(Host{Name: "cache"}); err != nil {
		t.Fatal(err)
	}
	if _, err := editor.Save(); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if !editor.Dirty() {
		t.Fatal("a failed save must keep the edits")
	}
}

func TestEditorInventoryReturnsACopy(t *testing.T) {
	editor, _ := newTestEditor(t, sampleInventory())
	copy := editor.Inventory()
	copy.Hosts[0].Name = "changed"
	copy.Groups[0].Members[0] = "changed"
	if editor.Dirty() {
		t.Fatal("mutating the returned inventory changed the editor")
	}
}
