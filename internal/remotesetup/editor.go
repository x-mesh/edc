package remotesetup

import (
	"reflect"
	"strings"
)

// Editor holds an inventory under edit. Every change keeps hosts, groups, and members consistent,
// so a screen can call it without checking the result itself.
type Editor struct {
	store     Store
	validator Validator
	revision  Revision
	original  Inventory
	inventory Inventory
}

// New loads the inventory from store.
func New(store Store, validator Validator) (*Editor, error) {
	inventory, revision, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &Editor{store: store, validator: validator, revision: revision, original: inventory.Clone(), inventory: inventory.Clone()}, nil
}

func (editor *Editor) Inventory() Inventory { return editor.inventory.Clone() }

func (editor *Editor) Dirty() bool { return !reflect.DeepEqual(editor.original, editor.inventory) }

func (editor *Editor) AddHost(host Host) error {
	host, err := editor.checkHost(host, -1)
	if err != nil {
		return err
	}
	editor.inventory.Hosts = append(editor.inventory.Hosts, host)
	return nil
}

// UpdateHost replaces the host at index. A renamed host keeps its place in every group.
func (editor *Editor) UpdateHost(index int, host Host) error {
	if index < 0 || index >= len(editor.inventory.Hosts) {
		return Issue{Code: IssueHostUnknown}
	}
	host, err := editor.checkHost(host, index)
	if err != nil {
		return err
	}
	previous := editor.inventory.Hosts[index].Name
	editor.inventory.Hosts[index] = host
	if previous != host.Name {
		for _, group := range editor.inventory.Groups {
			for position, member := range group.Members {
				if member == previous {
					group.Members[position] = host.Name
				}
			}
		}
	}
	return nil
}

// RemoveHost deletes the host at index and returns the groups that this emptied and removed.
func (editor *Editor) RemoveHost(index int) []string {
	if index < 0 || index >= len(editor.inventory.Hosts) {
		return nil
	}
	name := editor.inventory.Hosts[index].Name
	editor.inventory.Hosts = append(editor.inventory.Hosts[:index], editor.inventory.Hosts[index+1:]...)
	var removed []string
	groups := editor.inventory.Groups[:0]
	for _, group := range editor.inventory.Groups {
		members := group.Members[:0]
		for _, member := range group.Members {
			if member != name {
				members = append(members, member)
			}
		}
		group.Members = members
		if len(members) == 0 {
			removed = append(removed, group.Name)
			delete(editor.inventory.GroupParallel, group.Name)
			continue
		}
		groups = append(groups, group)
	}
	editor.inventory.Groups = groups
	return removed
}

func (editor *Editor) AddGroup(name string, members []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return Issue{Code: IssueGroupNameEmpty}
	}
	if issue := editor.validator.ValidateGroupName(name); issue != nil {
		return *issue
	}
	if editor.inventory.GroupIndex(name) >= 0 {
		return Issue{Code: IssueGroupDuplicate, Args: []any{name}}
	}
	members, err := editor.checkMembers(members)
	if err != nil {
		return err
	}
	editor.inventory.Groups = append(editor.inventory.Groups, Group{Name: name, Members: members})
	return nil
}

func (editor *Editor) SetMembers(name string, members []string) error {
	index := editor.inventory.GroupIndex(name)
	if index < 0 {
		return Issue{Code: IssueGroupUnknown, Args: []any{name}}
	}
	members, err := editor.checkMembers(members)
	if err != nil {
		return err
	}
	editor.inventory.Groups[index].Members = members
	return nil
}

func (editor *Editor) RemoveGroup(name string) {
	index := editor.inventory.GroupIndex(name)
	if index < 0 {
		return
	}
	editor.inventory.Groups = append(editor.inventory.Groups[:index], editor.inventory.Groups[index+1:]...)
	delete(editor.inventory.GroupParallel, name)
}

func (editor *Editor) SetParallel(value int) error {
	if value < 0 {
		return Issue{Code: IssueParallelNegative}
	}
	editor.inventory.Parallel = value
	return nil
}

// SetGroupParallel sets the parallel hosts of one group. Zero removes the override.
func (editor *Editor) SetGroupParallel(name string, value int) error {
	if editor.inventory.GroupIndex(name) < 0 {
		return Issue{Code: IssueGroupUnknown, Args: []any{name}}
	}
	if value < 0 {
		return Issue{Code: IssueParallelNegative}
	}
	if value == 0 {
		delete(editor.inventory.GroupParallel, name)
		return nil
	}
	if editor.inventory.GroupParallel == nil {
		editor.inventory.GroupParallel = map[string]int{}
	}
	editor.inventory.GroupParallel[name] = value
	return nil
}

// Problems lists what blocks a save. An empty result means the inventory can be saved.
func (editor *Editor) Problems() []Issue {
	return editor.validator.ValidateInventory(editor.inventory.Clone())
}

// Save writes the inventory when it has no problem. It returns the problems instead when it has some.
func (editor *Editor) Save() ([]Issue, error) {
	if problems := editor.Problems(); len(problems) > 0 {
		return problems, nil
	}
	if editor.store == nil {
		return nil, Issue{Code: IssueNoStore}
	}
	if err := editor.store.Save(editor.inventory.Clone(), editor.revision); err != nil {
		return nil, err
	}
	// The store may have assigned a new revision, so read it back before the next save.
	reloaded, revision, err := editor.store.Load()
	if err != nil {
		return nil, err
	}
	editor.revision = revision
	editor.original = reloaded.Clone()
	editor.inventory = reloaded.Clone()
	return nil, nil
}

func (editor *Editor) checkHost(host Host, replacing int) (Host, error) {
	host.Name = strings.TrimSpace(host.Name)
	host.Target = strings.TrimSpace(host.Target)
	if host.Name == "" {
		return host, Issue{Code: IssueHostNameEmpty}
	}
	if issue := editor.validator.ValidateHost(host); issue != nil {
		return host, *issue
	}
	if index := editor.inventory.HostIndex(host.Name); index >= 0 && index != replacing {
		return host, Issue{Code: IssueHostDuplicate, Args: []any{host.Name}}
	}
	return host, nil
}

func (editor *Editor) checkMembers(members []string) ([]string, error) {
	if len(members) == 0 {
		return nil, Issue{Code: IssueGroupNoMembers}
	}
	seen := make(map[string]struct{}, len(members))
	checked := make([]string, 0, len(members))
	for _, member := range members {
		if editor.inventory.HostIndex(member) < 0 {
			return nil, Issue{Code: IssueHostUnknown, Args: []any{member}}
		}
		if _, exists := seen[member]; exists {
			return nil, Issue{Code: IssueMemberDuplicate, Args: []any{member}}
		}
		seen[member] = struct{}{}
		checked = append(checked, member)
	}
	return checked, nil
}
