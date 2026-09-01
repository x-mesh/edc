package edc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

var errRemoteCancelled = errors.New("원격 실행을 취소했습니다")

type remoteRunOptions struct {
	inventoryPath string
	recipePath    string
	group         string
	verbose       bool
}

// remotePromptFlags는 비어 있는 option을 프롬프트로 채울지 결정한다.
type remotePromptFlags struct {
	force       bool // 선택과 확인 프롬프트를 생략한다
	dryRun      bool // 계획 출력과 확인을 caller에 맡기고 option만 채운다
	interactive bool // stdin과 stdout이 모두 terminal이다
}

func remoteInventoryNotFound(cwd string) error {
	return fmt.Errorf("inventory.yaml을 찾을 수 없습니다: %s. --inventory로 경로를 지정하세요", filepath.Join(cwd, "inventory.yaml"))
}

func discoverRemoteInventory(cwd, configDir string) (string, bool) {
	return discoverRemoteFile(cwd, configDir, "inventory.yaml")
}

func discoverRemoteRecipe(cwd, configDir string) (string, bool) {
	return discoverRemoteFile(cwd, configDir, "recipe.yaml")
}

func discoverRemoteFile(cwd, configDir, name string) (string, bool) {
	paths := []string{filepath.Join(cwd, name)}
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "edc", name))
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// promptRemoteOptions는 seed에서 비어 있는 항목만 자동 탐색이나 프롬프트로 채운다.
func promptRemoteOptions(input io.Reader, output io.Writer, cwd, configDir string, defaultTimeout time.Duration, seed remoteRunOptions, flags remotePromptFlags) (remoteRunOptions, error) {
	reader := bufio.NewReader(input)
	resolved := seed
	// group을 인자로 받은 실행은 경로와 streaming을 묻지 않고 확인만 거친다.
	askQuestions := seed.group == "" && flags.interactive && !flags.force
	if resolved.inventoryPath == "" {
		path, found := discoverRemoteInventory(cwd, configDir)
		if !found {
			if !askQuestions {
				return remoteRunOptions{}, remoteInventoryNotFound(cwd)
			}
			var err error
			path, err = promptRemoteText(reader, output, "inventory 경로", "")
			if err != nil {
				return remoteRunOptions{}, err
			}
		}
		resolved.inventoryPath = path
	}
	inventory, err := loadRemoteInventory(resolved.inventoryPath)
	if err != nil {
		return remoteRunOptions{}, err
	}
	if resolved.group == "" {
		resolved.group, err = promptRemoteGroup(input, reader, output, inventory, flags)
		if err != nil {
			return remoteRunOptions{}, err
		}
	}
	hosts, err := hostsForRemoteGroup(inventory, resolved.group)
	if err != nil {
		return remoteRunOptions{}, err
	}
	if flags.interactive {
		fmt.Fprintf(output, "inventory 경로: %s\n", resolved.inventoryPath)
	}
	if resolved.recipePath == "" {
		path, found := discoverRemoteRecipe(cwd, configDir)
		if askQuestions {
			if !found {
				path = ""
			}
			path, err = promptRemoteText(reader, output, "recipe 경로", path)
			if err != nil {
				return remoteRunOptions{}, err
			}
		} else {
			if !found {
				return remoteRunOptions{}, fmt.Errorf("recipe.yaml을 찾을 수 없습니다: %s. --recipe로 경로를 지정하세요", filepath.Join(cwd, "recipe.yaml"))
			}
			if flags.interactive {
				fmt.Fprintf(output, "recipe 경로: %s\n", path)
			}
		}
		resolved.recipePath = path
	}
	recipe, err := loadRemoteRecipe(resolved.recipePath, defaultTimeout)
	if err != nil {
		return remoteRunOptions{}, err
	}
	if !flags.interactive || flags.dryRun {
		return resolved, nil
	}
	printRemotePlan(output, resolved.group, hosts, recipe)
	if !resolved.verbose && askQuestions {
		resolved.verbose, err = promptRemoteYesNo(reader, output, "상세 출력을 streaming으로 볼까요? (y/N)")
		if err != nil {
			return remoteRunOptions{}, err
		}
	}
	if flags.force {
		return resolved, nil
	}
	confirmed, err := promptRemoteYesNo(reader, output, "실행할까요? (y/N)")
	if err != nil {
		return remoteRunOptions{}, err
	}
	if !confirmed {
		return remoteRunOptions{}, errRemoteCancelled
	}
	return resolved, nil
}

func promptRemoteGroup(input io.Reader, reader *bufio.Reader, output io.Writer, inventory remoteInventory, flags remotePromptFlags) (string, error) {
	groups := remoteGroupNames(inventory)
	if flags.force {
		if len(groups) != 1 {
			return "", errors.New("-f 자동 실행에는 inventory group이 하나만 있어야 합니다. edc remote <group>으로 지정하세요")
		}
		if flags.interactive {
			fmt.Fprintf(output, "group(대상): %s\n", groups[0])
		}
		return groups[0], nil
	}
	if !flags.interactive {
		return "", fmt.Errorf("group을 지정하세요: edc remote <group>. inventory group: %s", strings.Join(groups, ", "))
	}
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return selectRemoteGroup(file, output, groups)
	}
	fmt.Fprintln(output, "group(대상)을 선택하세요:")
	for index, group := range groups {
		fmt.Fprintf(output, "  %d) %s\n", index+1, group)
	}
	value, err := promptRemoteText(reader, output, "group 번호", "")
	if err != nil {
		return "", err
	}
	index, err := strconv.Atoi(value)
	if err != nil || index < 1 || index > len(groups) {
		return "", fmt.Errorf("올바르지 않은 group 번호: %s", value)
	}
	return groups[index-1], nil
}

func promptRemoteText(reader *bufio.Reader, output io.Writer, label, defaultValue string) (string, error) {
	if defaultValue == "" {
		fmt.Fprintf(output, "%s: ", label)
	} else {
		fmt.Fprintf(output, "%s [%s]: ", label, defaultValue)
	}
	value, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultValue
	}
	if value == "" {
		return "", fmt.Errorf("%s은 비어 있을 수 없습니다", label)
	}
	return value, nil
}

func printRemotePlan(output io.Writer, group string, hosts []remoteHost, recipe remoteRecipe) {
	hostNames := make([]string, 0, len(hosts))
	for _, host := range hosts {
		hostNames = append(hostNames, host.Name)
	}
	fmt.Fprintf(output, "\n실행 계획  %s → %s\n", group, strings.Join(hostNames, ", "))
	for index, step := range recipe.Steps {
		fmt.Fprintf(output, "  %d. %s\n", index+1, step.Name)
		if len(step.Tags) > 0 {
			targets := stepHostNames(step, hosts)
			if len(targets) == 0 {
				fmt.Fprintf(output, "     hosts    대상 없음  tag %s\n", strings.Join(step.Tags, ", "))
			} else {
				fmt.Fprintf(output, "     hosts    %s\n", strings.Join(targets, ", "))
			}
		}
		fmt.Fprintf(output, "     command  %s\n", step.Command)
		if step.Verify != "" {
			fmt.Fprintf(output, "     verify   %s\n", step.Verify)
		}
	}
	fmt.Fprintln(output)
}

func promptRemoteYesNo(reader *bufio.Reader, output io.Writer, label string) (bool, error) {
	value, err := promptRemoteText(reader, output, label, "N")
	if err != nil {
		return false, err
	}
	value = strings.ToLower(value)
	return value == "y" || value == "yes", nil
}
