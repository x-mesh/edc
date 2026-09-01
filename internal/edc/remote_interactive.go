package edc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

func discoverRemoteInventory(cwd, configDir string) (string, bool) {
	paths := []string{filepath.Join(cwd, "inventory.yaml")}
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "edc", "inventory.yaml"))
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

func promptRemoteOptions(input io.Reader, output io.Writer, cwd, configDir string, defaultTimeout time.Duration, forceVerbose ...bool) (remoteRunOptions, error) {
	reader := bufio.NewReader(input)
	inventoryPath, found := discoverRemoteInventory(cwd, configDir)
	if !found {
		var err error
		inventoryPath, err = promptRemoteText(reader, output, "inventory 경로", "")
		if err != nil {
			return remoteRunOptions{}, err
		}
	}
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		return remoteRunOptions{}, err
	}
	group, err := promptRemoteGroup(input, reader, output, inventory)
	if err != nil {
		return remoteRunOptions{}, err
	}
	fmt.Fprintf(output, "inventory 경로: %s\n", inventoryPath)
	recipeDefault := filepath.Join(cwd, "recipe.yaml")
	if _, err := os.Stat(recipeDefault); err != nil {
		recipeDefault = ""
	}
	recipePath, err := promptRemoteText(reader, output, "recipe 경로", recipeDefault)
	if err != nil {
		return remoteRunOptions{}, err
	}
	recipe, err := loadRemoteRecipe(recipePath, defaultTimeout)
	if err != nil {
		return remoteRunOptions{}, err
	}
	hosts, err := hostsForRemoteGroup(inventory, group)
	if err != nil {
		return remoteRunOptions{}, err
	}
	printRemotePlan(output, group, hosts, recipe)
	verbose := len(forceVerbose) > 0 && forceVerbose[0]
	if !verbose {
		verbose, err = promptRemoteYesNo(reader, output, "상세 출력을 streaming으로 볼까요? (y/N)")
		if err != nil {
			return remoteRunOptions{}, err
		}
	}
	confirmed, err := promptRemoteYesNo(reader, output, "실행할까요? (y/N)")
	if err != nil {
		return remoteRunOptions{}, err
	}
	if !confirmed {
		return remoteRunOptions{}, errRemoteCancelled
	}
	return remoteRunOptions{inventoryPath: inventoryPath, recipePath: recipePath, group: group, verbose: verbose}, nil
}

func promptRemoteGroup(input io.Reader, reader *bufio.Reader, output io.Writer, inventory remoteInventory) (string, error) {
	groups := make([]string, 0, len(inventory.Groups))
	for group := range inventory.Groups {
		groups = append(groups, group)
	}
	sort.Strings(groups)
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
		fmt.Fprintf(output, "     command  %s\n", step.Command)
		fmt.Fprintf(output, "     verify   %s\n", step.Verify)
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
