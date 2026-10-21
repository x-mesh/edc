package edc

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/x-mesh/edc/internal/remotesetup"
	"github.com/x-mesh/edc/internal/remotesetup/tui"
)

// remoteSetupIssueMessage carries text that edc already localized, such as the error of the inventory loader.
const remoteSetupIssueMessage = "remotesetup.issue.message"

// remoteSetupValidator applies the rules of edc remote, so setup accepts exactly what edc remote loads.
type remoteSetupValidator struct{}

func (remoteSetupValidator) ValidateHost(host remotesetup.Host) *remotesetup.Issue {
	checked := remoteHost{Name: host.Name, Target: host.Target, Tags: append([]string(nil), host.Tags...)}
	return remoteSetupIssue(validateRemoteHost(&checked, 0))
}

func (remoteSetupValidator) ValidateGroupName(name string) *remotesetup.Issue {
	if remoteReservedGroup(name) {
		return remoteSetupIssue(fmt.Errorf("%s", T("remote.error.group_reserved", name)))
	}
	return nil
}

func (remoteSetupValidator) ValidateInventory(inventory remotesetup.Inventory) []remotesetup.Issue {
	converted := remoteInventory{Parallel: inventory.Parallel, Groups: make(map[string][]string, len(inventory.Groups))}
	for _, host := range inventory.Hosts {
		converted.Hosts = append(converted.Hosts, remoteHost{Name: host.Name, Target: host.Target, Tags: host.Tags})
	}
	for _, group := range inventory.Groups {
		converted.Groups[group.Name] = group.Members
	}
	for name, value := range inventory.GroupParallel {
		if converted.Options == nil {
			converted.Options = map[string]remoteGroupOpts{}
		}
		converted.Options[name] = remoteGroupOpts{Parallel: value}
	}
	if issue := remoteSetupIssue(validateRemoteInventory(&converted)); issue != nil {
		return []remotesetup.Issue{*issue}
	}
	return nil
}

func remoteSetupIssue(err error) *remotesetup.Issue {
	if err == nil {
		return nil
	}
	return &remotesetup.Issue{Code: remoteSetupIssueMessage, Args: []any{err.Error()}}
}

type remoteSetupMessages struct{}

func (remoteSetupMessages) Text(code string, args ...any) string { return T(code, args...) }

// remoteSetupStyler paints with the same colors as the other edc screens: strong cyan keys, gray words,
// and the chip background for the row in use.
type remoteSetupStyler struct{ color bool }

func (styler remoteSetupStyler) Key(text string) string {
	if !styler.color {
		return text
	}
	return richPaint(text, richCyan, true)
}

func (styler remoteSetupStyler) Dim(text string) string {
	if !styler.color {
		return text
	}
	return richDim(text)
}

func (styler remoteSetupStyler) Selected(text string) string {
	if !styler.color {
		return "> " + text
	}
	return liveSelected(liveSelectedBar+" "+text+" ", true)
}

func (styler remoteSetupStyler) Error(text string) string {
	if !styler.color {
		return text
	}
	return richPaint(text, richRed, true)
}

// remoteSetupPath picks the file to edit: the flag, then the configured path, then the file that
// edc remote would find, and last a new file under ./.edc/.
func remoteSetupPath(flagValue string, config edcConfig, cwd, configDir string) string {
	if flagValue != "" {
		return flagValue
	}
	if configured := configuredString(config.Defaults.Remote.Inventory, ""); configured != "" {
		return configured
	}
	if found, ok := discoverRemoteInventory(cwd, configDir); ok {
		return found
	}
	return filepath.Join(cwd, ".edc", "inventory.yaml")
}

// remoteSetupRecipePath picks the recipe file the same way: the flag, the configured path,
// the file that edc remote would find, and last a new file under ./.edc/.
func remoteSetupRecipePath(flagValue string, config edcConfig, cwd, configDir string) string {
	if flagValue != "" {
		return flagValue
	}
	if configured := configuredString(config.Defaults.Remote.Recipe, ""); configured != "" {
		return configured
	}
	if found, ok := discoverRemoteRecipe(cwd, configDir); ok {
		return found
	}
	return filepath.Join(cwd, ".edc", "recipe.yaml")
}

func runRemoteSetup(args []string) int {
	set := newFlagSet("setup remote")
	set.SetOutput(os.Stderr)
	inventoryPath := set.String("inventory", "", T("command.setup.option.inventory"))
	recipePath := set.String("recipe", "", T("command.setup.option.recipe"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() > 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc setup remote [--inventory FILE] [--recipe FILE]"))
		return 2
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, T("cli.setup.terminal_required"))
		return 2
	}
	config, err := loadConfigAt(configPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.config.invalid", configError(configReadPath(configPath()), err)))
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = ""
	}
	path, err := filepath.Abs(remoteSetupPath(*inventoryPath, config, cwd, configDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	recipeFile, err := filepath.Abs(remoteSetupRecipePath(*recipePath, config, cwd, configDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	editor, err := remotesetup.New(&remoteInventoryStore{path: path}, remoteSetupValidator{})
	if err != nil {
		fmt.Fprintln(os.Stderr, T("remotesetup.error.read", path, err))
		return 2
	}
	recipe, err := remotesetup.NewRecipe(&remoteRecipeStore{path: recipeFile}, remoteSetupRecipeValidator{})
	if err != nil {
		fmt.Fprintln(os.Stderr, T("remotesetup.error.read", recipeFile, err))
		return 2
	}
	result, err := tui.Run(editor, tui.Options{
		Messages:       remoteSetupMessages{},
		Styler:         remoteSetupStyler{color: liveRich(true)},
		Location:       path,
		Recipe:         recipe,
		RecipeLocation: recipeFile,
	}, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if editor.Dirty() || recipe.Dirty() {
		fmt.Fprintln(os.Stderr, T("cli.setup.cancelled"))
		return 4
	}
	if result.Inventory {
		fmt.Fprintln(os.Stdout, T("remotesetup.saved", path))
	}
	if result.Recipe {
		fmt.Fprintln(os.Stdout, T("remotesetup.saved", recipeFile))
	}
	return 0
}
