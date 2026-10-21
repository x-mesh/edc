package remotesetup

import "errors"

// Issue is a problem the editor reports by code. A Messages adapter turns it into text,
// so the core carries no language.
type Issue struct {
	Code string
	Args []any
}

func (issue Issue) Error() string { return issue.Code }

const (
	IssueHostNameEmpty    = "remotesetup.issue.host_name_empty"
	IssueHostDuplicate    = "remotesetup.issue.host_duplicate"
	IssueHostUnknown      = "remotesetup.issue.host_unknown"
	IssueGroupNameEmpty   = "remotesetup.issue.group_name_empty"
	IssueGroupDuplicate   = "remotesetup.issue.group_duplicate"
	IssueGroupUnknown     = "remotesetup.issue.group_unknown"
	IssueGroupNoMembers   = "remotesetup.issue.group_no_members"
	IssueMemberDuplicate  = "remotesetup.issue.member_duplicate"
	IssueParallelNegative = "remotesetup.issue.parallel_negative"
	IssueNoStore          = "remotesetup.issue.no_store"
	IssueStepNameEmpty    = "remotesetup.issue.step_name_empty"
	IssueStepDuplicate    = "remotesetup.issue.step_duplicate"
	IssueStepUnknown      = "remotesetup.issue.step_unknown"
)

// ErrStale is what Store.Save returns when the stored inventory changed after Load.
var ErrStale = errors.New("remotesetup: the inventory changed after it was loaded")

// Revision is an opaque token that a Store hands out on Load and takes back on Save.
type Revision any

// Store reads and writes one inventory. A missing inventory loads as an empty one.
type Store interface {
	Load() (Inventory, Revision, error)
	Save(inventory Inventory, revision Revision) error
}

// Validator holds the rules of the consumer. The editor never encodes them itself.
type Validator interface {
	// ValidateHost checks one host as the user types it.
	ValidateHost(host Host) *Issue
	// ValidateGroupName checks a group name, for example against reserved words.
	ValidateGroupName(name string) *Issue
	// ValidateInventory checks the whole inventory before it is saved.
	ValidateInventory(inventory Inventory) []Issue
}

// Messages renders an Issue code or a screen text code. Unknown codes may render as the code.
type Messages interface {
	Text(code string, args ...any) string
}

// RecipeStore reads and writes one recipe. A missing recipe loads as an empty one.
type RecipeStore interface {
	Load() (Recipe, Revision, error)
	Save(recipe Recipe, revision Revision) error
}

// RecipeValidator holds the recipe rules of the consumer.
type RecipeValidator interface {
	// ValidateStep checks one step as the user types it.
	ValidateStep(step Step) *Issue
	// ValidateRecipe checks the whole recipe before it is saved.
	ValidateRecipe(recipe Recipe) []Issue
}
