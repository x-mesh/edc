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

// remoteMessageError는 문구를 출력하는 시점의 언어로 만든다.
// 패키지 변수는 언어 설정보다 먼저 만들어지므로 메시지를 미리 굳히지 않는다.
var (
	errRemoteCancelled error = &messageError{key: "remote.error.cancelled"}
	// doctor와 probe의 취소는 tui_doctor.go, tui_probe.go가 쓴다. 정의만 여기 모아 둔다.
	errDoctorCancelled error = &messageError{key: "observe.doctor.cancelled"}
	errProbeCancelled  error = &messageError{key: "observe.probe.cancelled"}
)

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
	live        bool // 계획과 확인을 실시간 화면이 맡는다
	interactive bool // stdin과 stdout이 모두 terminal이다
}

func remoteInventoryNotFound(cwd string) error {
	return errors.New(T("remote.error.inventory_not_found", filepath.Join(cwd, "inventory.yaml")))
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
			path, err = promptRemoteFile(input, reader, output, remoteFilePrompt{
				titleKey:   selectInventoryTitle,
				labelKey:   selectInventoryLabel,
				candidates: remoteInventoryCandidates(cwd, configDir),
			})
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
	if resolved.recipePath == "" {
		path, found := discoverRemoteRecipe(cwd, configDir)
		if askQuestions {
			if !found {
				path = ""
			}
			path, err = promptRemoteFile(input, reader, output, remoteFilePrompt{
				titleKey:     selectRecipeTitle,
				labelKey:     selectRecipeLabel,
				candidates:   remoteRecipeCandidates(cwd, configDir, defaultTimeout),
				defaultValue: path,
			})
			if err != nil {
				return remoteRunOptions{}, err
			}
		} else {
			if !found {
				return remoteRunOptions{}, errors.New(T("remote.error.recipe_not_found", filepath.Join(cwd, "recipe.yaml")))
			}
		}
		resolved.recipePath = path
	}
	recipe, err := loadRemoteRecipe(resolved.recipePath, defaultTimeout)
	if err != nil {
		return remoteRunOptions{}, err
	}
	if !flags.interactive || flags.dryRun || flags.live {
		return resolved, nil
	}
	printRemotePlan(output, remotePlanView{
		group: resolved.group, inventoryPath: resolved.inventoryPath, recipePath: resolved.recipePath,
		cwd: cwd, hosts: hosts, recipe: recipe, width: terminalWidth(),
	})
	if !resolved.verbose && askQuestions {
		resolved.verbose, err = askRemoteYesNo(input, reader, output, T("remote.confirm.stream"))
		if err != nil {
			return remoteRunOptions{}, err
		}
	}
	if flags.force {
		return resolved, nil
	}
	confirmed, err := askRemoteYesNo(input, reader, output, T("remote.confirm.run"))
	if err != nil {
		return remoteRunOptions{}, err
	}
	if !confirmed {
		return remoteRunOptions{}, errRemoteCancelled
	}
	return resolved, nil
}

// remoteFilePrompt는 경로 질문 하나를 설명한다. candidates가 비면 경로를 직접 입력받는다.
// titleKey와 labelKey는 언어팩 키다.
type remoteFilePrompt struct {
	titleKey     string
	labelKey     string
	candidates   []selectItem
	defaultValue string
}

// promptRemoteFile은 terminal에서는 목록에서 고르고, 그 외에는 경로를 입력받는다.
func promptRemoteFile(input io.Reader, reader *bufio.Reader, output io.Writer, prompt remoteFilePrompt) (string, error) {
	if file, ok := terminalInput(input); ok && len(prompt.candidates) > 0 {
		model := newSelectModel(prompt.titleKey, prompt.labelKey, prompt.candidates)
		return runSelect(file, output, model.withCursorAt(prompt.defaultValue))
	}
	return promptRemoteText(reader, output, T(prompt.labelKey), prompt.defaultValue)
}

// askRemoteYesNo는 terminal에서는 확인 위젯을, 그 외에는 y/N 입력을 쓴다.
func askRemoteYesNo(input io.Reader, reader *bufio.Reader, output io.Writer, question string) (bool, error) {
	if file, ok := terminalInput(input); ok {
		return runConfirm(file, output, question, false)
	}
	return promptRemoteYesNo(reader, output, question+" (y/N)")
}

// terminalInput은 bubbletea에 넘길 수 있는 terminal 입력인지 확인한다.
func terminalInput(input io.Reader) (*os.File, bool) {
	file, ok := input.(*os.File)
	if !ok {
		return nil, false
	}
	return file, term.IsTerminal(int(file.Fd()))
}

func promptRemoteGroup(input io.Reader, reader *bufio.Reader, output io.Writer, inventory remoteInventory, flags remotePromptFlags) (string, error) {
	groups := remoteGroupNames(inventory)
	if flags.force {
		if len(groups) != 1 {
			return "", errors.New(T("remote.error.force_needs_single_group"))
		}
		if flags.interactive {
			fmt.Fprintf(output, "%s: %s\n", T(selectGroupLabel), groups[0])
		}
		return groups[0], nil
	}
	if !flags.interactive {
		return "", errors.New(T("remote.error.group_required", strings.Join(groups, ", ")))
	}
	if file, ok := terminalInput(input); ok {
		return selectRemoteGroup(file, output, groups)
	}
	fmt.Fprintln(output, T("remote.prompt.choose_group", T(selectGroupLabel)))
	for index, group := range groups {
		fmt.Fprintf(output, "  %d) %s\n", index+1, group)
	}
	value, err := promptRemoteText(reader, output, T("remote.prompt.group_number"), "")
	if err != nil {
		return "", err
	}
	index, err := strconv.Atoi(value)
	if err != nil || index < 1 || index > len(groups) {
		return "", errors.New(T("remote.error.invalid_group_number", value))
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
		return "", errors.New(T("remote.error.value_empty", label))
	}
	return value, nil
}

// remotePlanView는 실행 전에 보여 줄 내용이다. 실행 결과 표와 같은 배치를 쓴다.
type remotePlanView struct {
	group         string
	inventoryPath string
	recipePath    string
	cwd           string
	hosts         []remoteHost
	recipe        remoteRecipe
	width         int
}

// printRemotePlan은 머리말, 빈 표, 명령 범례를 차례로 출력한다.
// 실행이 시작되면 같은 배치의 표가 채워지므로 눈이 대응을 다시 만들지 않아도 된다.
func printRemotePlan(output io.Writer, plan remotePlanView) {
	fmt.Fprint(output, remoteRunHeader(plan.group, plan.inventoryPath, plan.recipePath, plan.cwd, plan.hosts, plan.recipe))
	table := newRemoteTable(plan.hosts, plan.recipe, plan.width)
	fmt.Fprintf(output, "\n%s\n", table.header())
	for _, host := range plan.hosts {
		cells := make([]string, 0, len(plan.recipe.Steps))
		for _, step := range plan.recipe.Steps {
			if stepRunsOnHost(step, host) {
				cells = append(cells, remoteGlyphPending)
				continue
			}
			cells = append(cells, remoteGlyphAbsent)
		}
		fmt.Fprintln(output, table.row(host.Name, cells))
	}
	fmt.Fprintln(output)
	for _, line := range remoteStepLegend(plan.recipe, plan.hosts) {
		fmt.Fprintln(output, line)
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
