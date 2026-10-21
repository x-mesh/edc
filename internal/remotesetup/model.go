// Package remotesetup edits a remote inventory without knowing where it is stored,
// how it is validated, how text is localized, or how it is drawn. Those concerns
// sit behind the interfaces in ports.go, so a caller supplies its own adapters.
package remotesetup

import "maps"

type Host struct {
	Name   string
	Target string
	Tags   []string
}

type Group struct {
	Name    string
	Members []string
}

// Inventory keeps groups as an ordered slice so that a file round trip does not reorder them.
type Inventory struct {
	Hosts         []Host
	Groups        []Group
	Parallel      int
	GroupParallel map[string]int
}

func (inventory Inventory) Clone() Inventory {
	clone := Inventory{Parallel: inventory.Parallel}
	clone.Hosts = make([]Host, len(inventory.Hosts))
	for index, host := range inventory.Hosts {
		host.Tags = append([]string(nil), host.Tags...)
		clone.Hosts[index] = host
	}
	clone.Groups = make([]Group, len(inventory.Groups))
	for index, group := range inventory.Groups {
		group.Members = append([]string(nil), group.Members...)
		clone.Groups[index] = group
	}
	if len(inventory.GroupParallel) > 0 {
		clone.GroupParallel = maps.Clone(inventory.GroupParallel)
	}
	return clone
}

func (inventory Inventory) HostIndex(name string) int {
	for index, host := range inventory.Hosts {
		if host.Name == name {
			return index
		}
	}
	return -1
}

func (inventory Inventory) GroupIndex(name string) int {
	for index, group := range inventory.Groups {
		if group.Name == name {
			return index
		}
	}
	return -1
}
