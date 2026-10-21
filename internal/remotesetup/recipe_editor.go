package remotesetup

import (
	"reflect"
	"strings"
)

// RecipeEditor holds a recipe under edit. It is the counterpart of Editor for recipes.
type RecipeEditor struct {
	store     RecipeStore
	validator RecipeValidator
	revision  Revision
	original  Recipe
	recipe    Recipe
}

func NewRecipe(store RecipeStore, validator RecipeValidator) (*RecipeEditor, error) {
	recipe, revision, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &RecipeEditor{store: store, validator: validator, revision: revision, original: recipe.Clone(), recipe: recipe.Clone()}, nil
}

func (editor *RecipeEditor) Recipe() Recipe { return editor.recipe.Clone() }

func (editor *RecipeEditor) Dirty() bool { return !reflect.DeepEqual(editor.original, editor.recipe) }

// SetName names the recipe. An empty name is allowed while editing and blocks the save.
func (editor *RecipeEditor) SetName(name string) { editor.recipe.Name = strings.TrimSpace(name) }

func (editor *RecipeEditor) AddStep(step Step) error {
	step, err := editor.checkStep(step, -1)
	if err != nil {
		return err
	}
	editor.recipe.Steps = append(editor.recipe.Steps, step)
	return nil
}

func (editor *RecipeEditor) UpdateStep(index int, step Step) error {
	if index < 0 || index >= len(editor.recipe.Steps) {
		return Issue{Code: IssueStepUnknown}
	}
	step, err := editor.checkStep(step, index)
	if err != nil {
		return err
	}
	editor.recipe.Steps[index] = step
	return nil
}

func (editor *RecipeEditor) RemoveStep(index int) {
	if index < 0 || index >= len(editor.recipe.Steps) {
		return
	}
	editor.recipe.Steps = append(editor.recipe.Steps[:index], editor.recipe.Steps[index+1:]...)
}

// MoveStep moves the step by delta places, the others keep their order, and returns its new index.
// A move past either end does nothing.
func (editor *RecipeEditor) MoveStep(index, delta int) int {
	target := index + delta
	if index < 0 || index >= len(editor.recipe.Steps) || target < 0 || target >= len(editor.recipe.Steps) {
		return index
	}
	step := editor.recipe.Steps[index]
	steps := append(editor.recipe.Steps[:index], editor.recipe.Steps[index+1:]...)
	steps = append(steps, Step{})
	copy(steps[target+1:], steps[target:])
	steps[target] = step
	editor.recipe.Steps = steps
	return target
}

func (editor *RecipeEditor) Problems() []Issue {
	return editor.validator.ValidateRecipe(editor.recipe.Clone())
}

// Save writes the recipe when it has no problem. It returns the problems instead when it has some.
func (editor *RecipeEditor) Save() ([]Issue, error) {
	if problems := editor.Problems(); len(problems) > 0 {
		return problems, nil
	}
	if editor.store == nil {
		return nil, Issue{Code: IssueNoStore}
	}
	if err := editor.store.Save(editor.recipe.Clone(), editor.revision); err != nil {
		return nil, err
	}
	reloaded, revision, err := editor.store.Load()
	if err != nil {
		return nil, err
	}
	editor.revision = revision
	editor.original = reloaded.Clone()
	editor.recipe = reloaded.Clone()
	return nil, nil
}

func (editor *RecipeEditor) checkStep(step Step, replacing int) (Step, error) {
	step.Name = strings.TrimSpace(step.Name)
	if step.Name == "" {
		return step, Issue{Code: IssueStepNameEmpty}
	}
	if step.Upload != nil && *step.Upload == (Upload{}) {
		step.Upload = nil
	}
	if issue := editor.validator.ValidateStep(step); issue != nil {
		return step, *issue
	}
	if index := editor.recipe.StepIndex(step.Name); index >= 0 && index != replacing {
		return step, Issue{Code: IssueStepDuplicate, Args: []any{step.Name}}
	}
	return step, nil
}
