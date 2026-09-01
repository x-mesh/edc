package edc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	remoteConfigLimit = 1 << 20
	remoteHostLimit   = 1000
	remoteGroupLimit  = 100
	remoteStepLimit   = 100
	remoteMaxTimeout  = 24 * time.Hour
)

type remoteHost struct {
	Name   string `yaml:"name"`
	Target string `yaml:"target"`
}

type remoteInventory struct {
	Hosts    []remoteHost               `yaml:"hosts"`
	Groups   map[string][]string        `yaml:"groups"`
	Parallel int                        `yaml:"parallel"`
	Options  map[string]remoteGroupOpts `yaml:"group_options"`
}

type remoteGroupOpts struct {
	Parallel int `yaml:"parallel"`
}

type remoteStep struct {
	Name    string
	Command string
	Verify  string
	Timeout time.Duration
}

type remoteRecipe struct {
	Name  string
	Steps []remoteStep
}

type remoteRecipeFile struct {
	Name  string           `yaml:"name"`
	Steps []remoteStepFile `yaml:"steps"`
}

type remoteStepFile struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
	Verify  string `yaml:"verify"`
	Timeout string `yaml:"timeout"`
}

func loadRemoteInventory(path string) (remoteInventory, error) {
	var inventory remoteInventory
	if err := decodeRemoteYAML(path, &inventory); err != nil {
		return inventory, fmt.Errorf("inventory %q: %w", path, err)
	}
	if len(inventory.Hosts) == 0 {
		return inventory, errors.New("inventory에는 host가 하나 이상 필요합니다")
	}
	if len(inventory.Hosts) > remoteHostLimit {
		return inventory, fmt.Errorf("inventory host 수는 %d개를 초과할 수 없습니다", remoteHostLimit)
	}
	if len(inventory.Groups) == 0 {
		return inventory, errors.New("inventory에는 group이 하나 이상 필요합니다")
	}
	if len(inventory.Groups) > remoteGroupLimit {
		return inventory, fmt.Errorf("inventory group 수는 %d개를 초과할 수 없습니다", remoteGroupLimit)
	}
	if inventory.Parallel < 0 || inventory.Parallel > remoteHostLimit {
		return inventory, fmt.Errorf("inventory parallel은 0에서 %d 사이여야 합니다", remoteHostLimit)
	}
	hosts := make(map[string]struct{}, len(inventory.Hosts))
	for index := range inventory.Hosts {
		host := &inventory.Hosts[index]
		host.Name = strings.TrimSpace(host.Name)
		host.Target = strings.TrimSpace(host.Target)
		if host.Name == "" || host.Target == "" {
			return inventory, fmt.Errorf("host %d의 name과 target은 비어 있을 수 없습니다", index+1)
		}
		if strings.HasPrefix(host.Target, "-") {
			return inventory, fmt.Errorf("host %q의 target은 '-'로 시작할 수 없습니다", host.Name)
		}
		if _, exists := hosts[host.Name]; exists {
			return inventory, fmt.Errorf("중복 host: %s", host.Name)
		}
		hosts[host.Name] = struct{}{}
	}
	for group, members := range inventory.Groups {
		if strings.TrimSpace(group) == "" || len(members) == 0 {
			return inventory, errors.New("group 이름과 member는 비어 있을 수 없습니다")
		}
		seen := make(map[string]struct{}, len(members))
		for _, member := range members {
			if _, exists := hosts[member]; !exists {
				return inventory, fmt.Errorf("group %q의 알 수 없는 host: %s", group, member)
			}
			if _, exists := seen[member]; exists {
				return inventory, fmt.Errorf("group %q의 중복 host: %s", group, member)
			}
			seen[member] = struct{}{}
		}
	}
	for group, options := range inventory.Options {
		if _, exists := inventory.Groups[group]; !exists {
			return inventory, fmt.Errorf("group_options의 알 수 없는 group: %s", group)
		}
		if options.Parallel < 1 || options.Parallel > remoteHostLimit {
			return inventory, fmt.Errorf("group %q의 parallel은 1에서 %d 사이여야 합니다", group, remoteHostLimit)
		}
	}
	return inventory, nil
}

func remoteParallelForGroup(inventory remoteInventory, group string, override int) int {
	if override > 0 {
		return override
	}
	if options, exists := inventory.Options[group]; exists {
		return options.Parallel
	}
	if inventory.Parallel > 0 {
		return inventory.Parallel
	}
	return 1
}

func loadRemoteRecipe(path string, defaultTimeout time.Duration) (remoteRecipe, error) {
	if defaultTimeout <= 0 || defaultTimeout > remoteMaxTimeout {
		return remoteRecipe{}, fmt.Errorf("기본 timeout은 0보다 크고 %s 이하여야 합니다", remoteMaxTimeout)
	}
	var file remoteRecipeFile
	if err := decodeRemoteYAML(path, &file); err != nil {
		return remoteRecipe{}, fmt.Errorf("recipe %q: %w", path, err)
	}
	if strings.TrimSpace(file.Name) == "" {
		return remoteRecipe{}, errors.New("recipe name은 비어 있을 수 없습니다")
	}
	if len(file.Steps) == 0 || len(file.Steps) > remoteStepLimit {
		return remoteRecipe{}, fmt.Errorf("recipe step 수는 1에서 %d 사이여야 합니다", remoteStepLimit)
	}
	recipe := remoteRecipe{Name: strings.TrimSpace(file.Name), Steps: make([]remoteStep, 0, len(file.Steps))}
	seen := make(map[string]struct{}, len(file.Steps))
	for index, value := range file.Steps {
		step := remoteStep{Name: strings.TrimSpace(value.Name), Command: strings.TrimSpace(value.Command), Verify: strings.TrimSpace(value.Verify), Timeout: defaultTimeout}
		if step.Name == "" || step.Command == "" || step.Verify == "" {
			return remoteRecipe{}, fmt.Errorf("step %d의 name, command, verify는 비어 있을 수 없습니다", index+1)
		}
		if _, exists := seen[step.Name]; exists {
			return remoteRecipe{}, fmt.Errorf("중복 step: %s", step.Name)
		}
		seen[step.Name] = struct{}{}
		if value.Timeout != "" {
			duration, err := time.ParseDuration(value.Timeout)
			if err != nil || duration <= 0 || duration > remoteMaxTimeout {
				return remoteRecipe{}, fmt.Errorf("step %q의 timeout이 올바르지 않습니다: %s", step.Name, value.Timeout)
			}
			step.Timeout = duration
		}
		recipe.Steps = append(recipe.Steps, step)
	}
	return recipe, nil
}

func decodeRemoteYAML(path string, target interface{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(&io.LimitedReader{R: file, N: remoteConfigLimit + 1})
	if err != nil {
		return err
	}
	if len(data) > remoteConfigLimit {
		return fmt.Errorf("파일 크기는 %d bytes를 초과할 수 없습니다", remoteConfigLimit)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("YAML document는 하나만 허용됩니다")
		}
		return err
	}
	return nil
}

func hostsForRemoteGroup(inventory remoteInventory, group string) ([]remoteHost, error) {
	members, exists := inventory.Groups[group]
	if !exists {
		return nil, fmt.Errorf("알 수 없는 group: %s", group)
	}
	selected := make(map[string]struct{}, len(members))
	for _, member := range members {
		selected[member] = struct{}{}
	}
	hosts := make([]remoteHost, 0, len(members))
	for _, host := range inventory.Hosts {
		if _, exists := selected[host.Name]; exists {
			hosts = append(hosts, host)
		}
	}
	return hosts, nil
}
