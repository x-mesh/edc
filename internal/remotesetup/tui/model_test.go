package tui

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/x-mesh/edc/internal/remotesetup"
)

type memoryStore struct {
	inventory remotesetup.Inventory
	revision  int
	stale     bool
}

func (store *memoryStore) Load() (remotesetup.Inventory, remotesetup.Revision, error) {
	return store.inventory.Clone(), store.revision, nil
}

func (store *memoryStore) Save(inventory remotesetup.Inventory, revision remotesetup.Revision) error {
	if store.stale || revision != store.revision {
		return remotesetup.ErrStale
	}
	store.inventory = inventory.Clone()
	store.revision++
	return nil
}

type rules struct{}

func (rules) ValidateHost(remotesetup.Host) *remotesetup.Issue { return nil }
func (rules) ValidateGroupName(string) *remotesetup.Issue      { return nil }
func (rules) ValidateInventory(inventory remotesetup.Inventory) []remotesetup.Issue {
	if len(inventory.Hosts) == 0 {
		return []remotesetup.Issue{{Code: "test.no_host"}}
	}
	return nil
}

// echo renders a code with its arguments, so a test asserts on the code and not on a language.
type echo struct{}

func (echo) Text(code string, args ...any) string {
	if len(args) == 0 {
		return code
	}
	return code + fmt.Sprint(args...)
}

func newModel(t *testing.T, inventory remotesetup.Inventory) (Model, *memoryStore) {
	t.Helper()
	store := &memoryStore{inventory: inventory}
	editor, err := remotesetup.New(store, rules{})
	if err != nil {
		t.Fatal(err)
	}
	return New(editor, Options{Messages: echo{}, Location: "inventory.yaml"}), store
}

var named = map[string]tea.KeyPressMsg{
	"enter": {Code: tea.KeyEnter},
	"esc":   {Code: tea.KeyEscape},
	"tab":   {Code: tea.KeyTab},
	"down":  {Code: tea.KeyDown},
	"up":    {Code: tea.KeyUp},
	"space": {Code: tea.KeySpace, Text: " "},
}

// press sends keys one by one. A name in `named` is a special key, anything else is typed text.
func press(model Model, keys ...string) Model {
	for _, key := range keys {
		if message, ok := named[key]; ok {
			model = update(model, message)
			continue
		}
		for _, character := range key {
			model = update(model, tea.KeyPressMsg{Code: character, Text: string(character)})
		}
	}
	return model
}

func update(model Model, message tea.Msg) Model {
	next, _ := model.Update(message)
	return next.(Model)
}

func twoHosts() remotesetup.Inventory {
	return remotesetup.Inventory{Hosts: []remotesetup.Host{{Name: "web", Target: "web.example"}, {Name: "db"}}}
}

func TestAddHostThroughTheForm(t *testing.T) {
	model, _ := newModel(t, remotesetup.Inventory{})
	model = press(model, "a", "web", "tab", "web.example", "tab", "linux, prod", "enter")
	if model.mode != modeHosts || model.status != "" {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
	hosts := model.editor.Inventory().Hosts
	want := []remotesetup.Host{{Name: "web", Target: "web.example", Tags: []string{"linux", "prod"}}}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("hosts = %+v", hosts)
	}
	if !model.editor.Dirty() {
		t.Fatal("the editor must be dirty")
	}
}

func TestFormKeepsTheInputWhenTheHostIsRejected(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	model = press(model, "a", "web", "enter", "enter", "enter")
	if model.mode != modeForm || !model.statusError || !strings.Contains(model.status, remotesetup.IssueHostDuplicate) {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
	if got := model.form.inputs[hostName].Value(); got != "web" {
		t.Fatalf("the typed name was lost: %q", got)
	}
	model = press(model, "esc")
	if model.mode != modeHosts || len(model.editor.Inventory().Hosts) != 2 {
		t.Fatalf("esc must leave without adding: mode = %v", model.mode)
	}
}

func TestEditHostRenamesIt(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	model = press(model, "e")
	if got := model.form.inputs[hostName].Value(); got != "web" {
		t.Fatalf("the form did not load the host: %q", got)
	}
	model = press(model, "-new", "enter", "enter", "enter")
	if got := model.editor.Inventory().Hosts[0].Name; got != "web-new" {
		t.Fatalf("name = %q", got)
	}
}

func TestAddGroupPicksMembers(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	model = press(model, "g", "a")
	if model.mode != modeGroupName {
		t.Fatalf("mode = %v", model.mode)
	}
	model = press(model, "servers", "enter")
	if model.mode != modePick {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
	model = press(model, "space", "down", "space", "enter")
	groups := model.editor.Inventory().Groups
	if model.mode != modeGroups || !reflect.DeepEqual(groups, []remotesetup.Group{{Name: "servers", Members: []string{"web", "db"}}}) {
		t.Fatalf("mode = %v, groups = %+v", model.mode, groups)
	}
}

func TestGroupWithoutMembersIsRejected(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	model = press(model, "g", "a", "empty", "enter", "enter")
	if model.mode != modePick || !strings.Contains(model.status, remotesetup.IssueGroupNoMembers) {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
}

func TestGroupNeedsAHostFirst(t *testing.T) {
	model, _ := newModel(t, remotesetup.Inventory{})
	model = press(model, "g", "a")
	if model.mode != modeGroups || !strings.Contains(model.status, "add_host_first") {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
}

func TestDeleteHostAsksFirstAndReportsEmptiedGroups(t *testing.T) {
	inventory := twoHosts()
	inventory.Groups = []remotesetup.Group{{Name: "db-only", Members: []string{"db"}}, {Name: "all", Members: []string{"web", "db"}}}
	model, _ := newModel(t, inventory)
	model = press(model, "down", "d")
	if model.mode != modeConfirm {
		t.Fatalf("mode = %v", model.mode)
	}
	model = press(model, "n")
	if len(model.editor.Inventory().Hosts) != 2 {
		t.Fatal("n must keep the host")
	}
	model = press(model, "d", "y")
	got := model.editor.Inventory()
	if len(got.Hosts) != 1 || got.GroupIndex("db-only") >= 0 || !strings.Contains(model.status, "groups_removed") || !strings.Contains(model.status, "db-only") {
		t.Fatalf("inventory = %+v, status = %q", got, model.status)
	}
	if model.hostCursor != 0 {
		t.Fatalf("cursor = %d", model.hostCursor)
	}
}

func TestParallelAcceptsNumbersOnly(t *testing.T) {
	inventory := twoHosts()
	inventory.Groups = []remotesetup.Group{{Name: "all", Members: []string{"web", "db"}}}
	model, _ := newModel(t, inventory)
	model = press(model, "g", "p")
	if got := model.input.Value(); got != "0" {
		t.Fatalf("the prompt must start from the current value, got %q", got)
	}
	model = press(model, "x", "enter")
	if model.mode != modeNumber || !strings.Contains(model.status, "not_a_number") {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
	model.input.SetValue("4")
	model = press(model, "enter")
	if model.editor.Inventory().Parallel != 4 || model.mode != modeGroups {
		t.Fatalf("parallel = %d, mode = %v", model.editor.Inventory().Parallel, model.mode)
	}
	model = press(model, "o")
	model.input.SetValue("2")
	model = press(model, "enter")
	if got := model.editor.Inventory().GroupParallel["all"]; got != 2 {
		t.Fatalf("group parallel = %d", got)
	}
}

func TestQuitAsksOnlyWhenThereAreUnsavedChanges(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	if _, command := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); command == nil || !isQuit(command) {
		t.Fatal("q on a clean editor must quit")
	}
	model = press(model, "d", "y", "q")
	if model.mode != modeConfirm {
		t.Fatalf("mode = %v", model.mode)
	}
	model = press(model, "n")
	if model.mode != modeHosts {
		t.Fatalf("n must return, mode = %v", model.mode)
	}
	_, command := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if command != nil {
		t.Fatal("q on a dirty editor must ask first")
	}
	model = press(model, "q")
	if _, command := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}); command == nil || !isQuit(command) {
		t.Fatal("y must quit")
	}
}

func isQuit(command tea.Cmd) bool {
	_, ok := command().(tea.QuitMsg)
	return ok
}

func TestSave(t *testing.T) {
	model, store := newModel(t, twoHosts())
	model = press(model, "s")
	if !strings.Contains(model.status, "no_changes") {
		t.Fatalf("status = %q", model.status)
	}
	model = press(model, "d", "y", "d", "y")
	model = press(model, "s")
	if model.saved || !model.statusError || !strings.Contains(model.status, "blocked") || !strings.Contains(model.status, "test.no_host") {
		t.Fatalf("saved = %v, status = %q", model.saved, model.status)
	}
	model = press(model, "a", "cache", "enter", "enter", "enter")
	model = press(model, "s")
	if !model.saved || model.statusError || len(store.inventory.Hosts) != 1 || store.inventory.Hosts[0].Name != "cache" {
		t.Fatalf("saved = %v, status = %q, stored = %+v", model.saved, model.status, store.inventory)
	}
	if model.editor.Dirty() {
		t.Fatal("a saved editor is clean")
	}
}

func TestSaveReportsAStaleFile(t *testing.T) {
	model, store := newModel(t, twoHosts())
	store.stale = true
	model = press(model, "d", "y", "s")
	if model.saved || !model.statusError || !strings.Contains(model.status, "stale") {
		t.Fatalf("saved = %v, status = %q", model.saved, model.status)
	}
}

func TestViewShowsHostsAndGroups(t *testing.T) {
	inventory := twoHosts()
	inventory.Hosts[0].Tags = []string{"linux"}
	inventory.Groups = []remotesetup.Group{{Name: "all", Members: []string{"web", "db"}}}
	inventory.GroupParallel = map[string]int{"all": 2}
	model, _ := newModel(t, inventory)
	content := model.View().Content
	for _, want := range []string{"inventory.yaml", "web", "web.example", "[linux]", "db"} {
		if !strings.Contains(content, want) {
			t.Errorf("hosts view misses %q:\n%s", want, content)
		}
	}
	content = press(model, "g").View().Content
	for _, want := range []string{"all", "web, db", "remotesetup.tui.group_parallel"} {
		if !strings.Contains(content, want) {
			t.Errorf("groups view misses %q:\n%s", want, content)
		}
	}
}

func TestViewTruncatesALongTitleToTheWidth(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	model.location = strings.Repeat("x", 200)
	model = update(model, tea.WindowSizeMsg{Width: 60, Height: 24})
	first := strings.SplitN(model.View().Content, "\n", 2)[0]
	if got := len([]rune(first)); got > 60 {
		t.Fatalf("title is %d columns wide:\n%s", got, first)
	}
}

func TestIssueTextFallsBackToTheErrorMessage(t *testing.T) {
	model, _ := newModel(t, twoHosts())
	if got := model.issueText(errors.New("disk full")); got != "disk full" {
		t.Fatalf("text = %q", got)
	}
}

type recipeStore struct {
	recipe   remotesetup.Recipe
	revision int
}

func (store *recipeStore) Load() (remotesetup.Recipe, remotesetup.Revision, error) {
	return store.recipe.Clone(), store.revision, nil
}

func (store *recipeStore) Save(recipe remotesetup.Recipe, revision remotesetup.Revision) error {
	if revision != store.revision {
		return remotesetup.ErrStale
	}
	store.recipe = recipe.Clone()
	store.revision++
	return nil
}

type recipeRules struct{}

func (recipeRules) ValidateStep(step remotesetup.Step) *remotesetup.Issue {
	if step.Command == "" && step.Upload == nil {
		return &remotesetup.Issue{Code: "test.no_action"}
	}
	return nil
}

func (recipeRules) ValidateRecipe(recipe remotesetup.Recipe) []remotesetup.Issue {
	if recipe.Name == "" {
		return []remotesetup.Issue{{Code: "test.no_name"}}
	}
	return nil
}

func newRecipeModel(t *testing.T, recipe remotesetup.Recipe) (Model, *memoryStore, *recipeStore) {
	t.Helper()
	store := &memoryStore{inventory: twoHosts()}
	editor, err := remotesetup.New(store, rules{})
	if err != nil {
		t.Fatal(err)
	}
	steps := &recipeStore{recipe: recipe}
	recipeEditor, err := remotesetup.NewRecipe(steps, recipeRules{})
	if err != nil {
		t.Fatal(err)
	}
	return New(editor, Options{Messages: echo{}, Location: "inventory.yaml", Recipe: recipeEditor, RecipeLocation: "recipe.yaml"}), store, steps
}

func sampleRecipe() remotesetup.Recipe {
	return remotesetup.Recipe{Name: "daily", Steps: []remotesetup.Step{
		{Name: "update", Command: "apt-get update", Tags: []string{"linux"}},
		{Name: "copy", Upload: &remotesetup.Upload{Source: "a.conf", Destination: "/etc/a.conf", Mode: "0644"}},
		{Name: "check", Command: "uptime"},
	}}
}

func stepNames(model Model) []string {
	var names []string
	for _, step := range model.recipe.Recipe().Steps {
		names = append(names, step.Name)
	}
	return names
}

func TestStepsScreenExistsOnlyWithARecipe(t *testing.T) {
	plain, _ := newModel(t, twoHosts())
	if got := press(plain, "r"); got.mode != modeHosts {
		t.Fatalf("r without a recipe must do nothing, mode = %v", got.mode)
	}
	if strings.Contains(plain.View().Content, "screen.steps") {
		t.Fatal("the steps tab shows without a recipe")
	}
	model, _, _ := newRecipeModel(t, sampleRecipe())
	if got := press(model, "r"); got.mode != modeSteps {
		t.Fatalf("mode = %v", got.mode)
	}
	model = press(model, "tab")
	if model.screen() != modeGroups {
		t.Fatalf("tab from hosts = %v", model.screen())
	}
	model = press(model, "tab")
	if model.screen() != modeSteps {
		t.Fatalf("tab from groups = %v", model.screen())
	}
	model = press(model, "tab")
	if model.screen() != modeHosts {
		t.Fatalf("tab must wrap to hosts, got %v", model.screen())
	}
}

func TestAddCommandAndUploadSteps(t *testing.T) {
	model, _, _ := newRecipeModel(t, remotesetup.Recipe{Name: "daily"})
	model = press(model, "r", "a", "update", "enter", "apt-get update", "enter", "enter", "enter", "enter", "enter", "enter", "linux, prod", "enter")
	if model.mode != modeSteps || model.status != "" {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
	model = press(model, "a", "copy", "enter", "enter", "a.conf", "enter", "/etc/a.conf", "enter", "0644", "enter", "enter", "enter", "enter", "enter")
	steps := model.recipe.Recipe().Steps
	want := []remotesetup.Step{
		{Name: "update", Command: "apt-get update", Tags: []string{"linux", "prod"}},
		{Name: "copy", Upload: &remotesetup.Upload{Source: "a.conf", Destination: "/etc/a.conf", Mode: "0644"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps = %+v", steps)
	}
	if model.stepCursor != 1 {
		t.Fatalf("cursor = %d", model.stepCursor)
	}
}

func TestStepWithoutAnActionStaysInTheForm(t *testing.T) {
	model, _, _ := newRecipeModel(t, remotesetup.Recipe{Name: "daily"})
	model = press(model, "r", "a", "noop", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	if model.mode != modeForm || !model.statusError || !strings.Contains(model.status, "test.no_action") {
		t.Fatalf("mode = %v, status = %q", model.mode, model.status)
	}
	if got := model.form.inputs[stepName].Value(); got != "noop" {
		t.Fatalf("the typed name was lost: %q", got)
	}
}

func TestEditStepLoadsAndKeepsTheUpload(t *testing.T) {
	model, _, _ := newRecipeModel(t, sampleRecipe())
	model = press(model, "r", "down", "e")
	if got := model.form.inputs[stepSource].Value(); got != "a.conf" {
		t.Fatalf("the form did not load the upload: %q", got)
	}
	model = press(model, "-v2", "enter", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	if got := model.recipe.Recipe().Steps[1]; got.Name != "copy-v2" || got.Upload == nil || got.Upload.Destination != "/etc/a.conf" {
		t.Fatalf("step = %+v", got)
	}
}

func TestReorderAndDeleteSteps(t *testing.T) {
	model, _, _ := newRecipeModel(t, sampleRecipe())
	model = press(model, "r", "J")
	if got := strings.Join(stepNames(model), ","); got != "copy,update,check" || model.stepCursor != 1 {
		t.Fatalf("after J: %s, cursor %d", got, model.stepCursor)
	}
	model = press(model, "K", "K")
	if got := strings.Join(stepNames(model), ","); got != "update,copy,check" || model.stepCursor != 0 {
		t.Fatalf("after K K: %s, cursor %d", got, model.stepCursor)
	}
	model = press(model, "d")
	if model.mode != modeConfirm || !strings.Contains(model.statusLine(), "delete_step") {
		t.Fatalf("mode = %v, status line = %q", model.mode, model.statusLine())
	}
	model = press(model, "n")
	if len(stepNames(model)) != 3 {
		t.Fatal("n must keep the step")
	}
	model = press(model, "d", "y")
	if got := strings.Join(stepNames(model), ","); got != "copy,check" || model.mode != modeSteps {
		t.Fatalf("after delete: %s, mode %v", got, model.mode)
	}
}

func TestRecipeNamePrompt(t *testing.T) {
	model, _, _ := newRecipeModel(t, sampleRecipe())
	model = press(model, "r", "n")
	if model.mode != modeRecipeName || model.input.Value() != "daily" {
		t.Fatalf("mode = %v, value = %q", model.mode, model.input.Value())
	}
	model.input.SetValue(" weekly ")
	model = press(model, "enter")
	if model.recipe.Recipe().Name != "weekly" || model.mode != modeSteps {
		t.Fatalf("name = %q, mode = %v", model.recipe.Recipe().Name, model.mode)
	}
}

func TestSaveWritesBothFilesAndStopsAtTheFirstProblem(t *testing.T) {
	model, inventory, steps := newRecipeModel(t, remotesetup.Recipe{})
	model = press(model, "a", "cache", "enter", "enter", "enter")
	model = press(model, "r", "a", "update", "enter", "uptime", "enter", "enter", "enter", "enter", "enter", "enter", "enter")
	model = press(model, "s")
	if !model.saved || !model.statusError || !strings.Contains(model.status, "recipe_blocked") || !strings.Contains(model.status, "test.no_name") {
		t.Fatalf("saved = %v, status = %q", model.saved, model.status)
	}
	if len(inventory.inventory.Hosts) != 3 || model.editor.Dirty() {
		t.Fatalf("the inventory must be saved before the recipe fails: %+v", inventory.inventory)
	}
	if steps.revision != 0 {
		t.Fatal("a blocked recipe was written")
	}
	model = press(model, "n")
	model.input.SetValue("daily")
	model = press(model, "enter", "s")
	if model.statusError || steps.recipe.Name != "daily" || len(steps.recipe.Steps) != 1 || model.dirty() {
		t.Fatalf("status = %q, stored = %+v", model.status, steps.recipe)
	}
}

func TestQuitAsksWhenOnlyTheRecipeChanged(t *testing.T) {
	model, _, _ := newRecipeModel(t, sampleRecipe())
	model = press(model, "r", "d", "y", "q")
	if model.mode != modeConfirm || model.confirm != confirmDiscard {
		t.Fatalf("mode = %v", model.mode)
	}
	if !strings.Contains(model.View().Content, " *") {
		t.Fatal("the title must mark the unsaved recipe")
	}
}

func TestStepsViewShowsCommandsUploadsAndTags(t *testing.T) {
	model, _, _ := newRecipeModel(t, sampleRecipe())
	content := press(model, "r").View().Content
	for _, want := range []string{"remotesetup.tui.recipe_line", "daily", "recipe.yaml", "update", "apt-get update", "[linux]", "remotesetup.tui.upload_line", "uptime"} {
		if !strings.Contains(content, want) {
			t.Errorf("steps view misses %q:\n%s", want, content)
		}
	}
}
