package edc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type remotePlanHost struct {
	Name   string   `json:"name"`
	Target string   `json:"target"`
	Tags   []string `json:"tags"`
}

type remotePlanStep struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Verify  string   `json:"verify,omitempty"`
	Timeout string   `json:"timeout"`
	Tags    []string `json:"tags"`
	Hosts   []string `json:"hosts"`
}

// remotePlan은 --dry-run --json이 내는 실행 계획이다. 실행 결과 report와 schema를 공유하지 않는다.
type remotePlan struct {
	SchemaVersion string           `json:"schema_version"`
	Group         string           `json:"group"`
	Inventory     string           `json:"inventory"`
	Recipe        string           `json:"recipe"`
	RecipeName    string           `json:"recipe_name"`
	Parallel      int              `json:"parallel"`
	Hosts         []remotePlanHost `json:"hosts"`
	Steps         []remotePlanStep `json:"steps"`
}

type remoteListingHost struct {
	Name   string   `json:"name"`
	Target string   `json:"target"`
	Tags   []string `json:"tags"`
}

type remoteListingGroup struct {
	Name     string              `json:"name"`
	Parallel int                 `json:"parallel"`
	Hosts    []remoteListingHost `json:"hosts"`
}

type remoteListing struct {
	Inventory string               `json:"inventory"`
	Groups    []remoteListingGroup `json:"groups"`
}

func buildRemotePlan(options remoteRunOptions, hosts []remoteHost, recipe remoteRecipe, parallel int) remotePlan {
	plan := remotePlan{
		SchemaVersion: "1.0", Group: options.group, Inventory: options.inventoryPath, Recipe: options.recipePath,
		RecipeName: recipe.Name, Parallel: parallel, Hosts: make([]remotePlanHost, 0, len(hosts)), Steps: make([]remotePlanStep, 0, len(recipe.Steps)),
	}
	for _, host := range hosts {
		plan.Hosts = append(plan.Hosts, remotePlanHost{Name: host.Name, Target: host.Target, Tags: nonNilStrings(host.Tags)})
	}
	for _, step := range recipe.Steps {
		plan.Steps = append(plan.Steps, remotePlanStep{
			Name: step.Name, Command: step.Command, Verify: step.Verify, Timeout: step.Timeout.String(),
			Tags: nonNilStrings(step.Tags), Hosts: stepHostNames(step, hosts),
		})
	}
	return plan
}

func emitRemotePlan(options commonOptions, remoteOptions remoteRunOptions, hosts []remoteHost, recipe remoteRecipe, parallel int) int {
	if options.jsonPath == "" {
		printRemotePlan(os.Stdout, remoteOptions.group, hosts, recipe)
		return 0
	}
	return emitRemoteJSON(options, buildRemotePlan(remoteOptions, hosts, recipe, parallel))
}

// emitRemoteJSON은 redaction이 켜져 있으면 IP 주소만 가린다. host 이름은 실행 대상을 식별하는 데 필요하므로 남긴다.
func emitRemoteJSON(options commonOptions, value interface{}) int {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if options.redact {
		data = []byte(redactIPAddresses(string(data)))
	}
	if err := writeJSONOutput(options.jsonPath, json.RawMessage(data)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

func runRemoteList(options commonOptions, remoteOptions remoteRunOptions, cwd, configDir string) int {
	path := remoteOptions.inventoryPath
	if path == "" {
		found := false
		if path, found = discoverRemoteInventory(cwd, configDir); !found {
			fmt.Fprintln(os.Stderr, remoteInventoryNotFound(cwd))
			return 2
		}
	}
	inventory, err := loadRemoteInventory(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	listing, err := buildRemoteListing(path, inventory, remoteOptions.group)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if options.jsonPath == "" {
		printRemoteListing(os.Stdout, listing)
		return 0
	}
	return emitRemoteJSON(options, listing)
}

// buildRemoteListing은 group을 주면 그 group만, 비우면 inventory의 모든 group을 이름순으로 담는다.
func buildRemoteListing(path string, inventory remoteInventory, group string) (remoteListing, error) {
	names := remoteGroupNames(inventory)
	if group != "" {
		if _, exists := inventory.Groups[group]; !exists {
			return remoteListing{}, fmt.Errorf("알 수 없는 group: %s", group)
		}
		names = []string{group}
	}
	listing := remoteListing{Inventory: path, Groups: make([]remoteListingGroup, 0, len(names))}
	for _, name := range names {
		hosts, err := hostsForRemoteGroup(inventory, name)
		if err != nil {
			return remoteListing{}, err
		}
		entry := remoteListingGroup{Name: name, Parallel: remoteParallelForGroup(inventory, name, 0), Hosts: make([]remoteListingHost, 0, len(hosts))}
		for _, host := range hosts {
			entry.Hosts = append(entry.Hosts, remoteListingHost{Name: host.Name, Target: host.Target, Tags: nonNilStrings(host.Tags)})
		}
		listing.Groups = append(listing.Groups, entry)
	}
	return listing, nil
}

func printRemoteListing(writer io.Writer, listing remoteListing) {
	fmt.Fprintf(writer, "inventory  %s\n", listing.Inventory)
	for _, group := range listing.Groups {
		fmt.Fprintf(writer, "group  %s  (parallel %d, host %d)\n", group.Name, group.Parallel, len(group.Hosts))
		for _, host := range group.Hosts {
			line := "  " + host.Name
			if host.Target != host.Name {
				line += "  → " + host.Target
			}
			if len(host.Tags) > 0 {
				line += "  [" + strings.Join(host.Tags, ", ") + "]"
			}
			fmt.Fprintln(writer, line)
		}
	}
}

// nonNilStrings는 JSON에서 null 대신 빈 배열이 나오게 한다.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
