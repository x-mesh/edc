package edc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// aiClaudeSnapshotName은 statusline이 남긴 Claude 사용량이다. ai-claude.json과 같은 형식이고 token은 담지 않는다.
	aiClaudeSnapshotName     = "ai-claude-statusline.json"
	aiStatuslineBackupSuffix = ".edc-backup"
	aiStatuslineInputLimit   = 1 << 20
)

var errAIStatuslineMissing = errors.New(`no Claude usage from the status line · run "edc ai statusline install"`)

// aiStatuslineWindows는 Claude Code가 statusline에 넘기는 한도 창이다. spend_limit은 gateway 전용이라 뺀다.
var aiStatuslineWindows = []struct{ key, name string }{
	{"five_hour", "5h"},
	{"seven_day", "7d"},
}

func runAIStatusline(args []string) int {
	if len(args) == 1 && (args[0] == "install" || args[0] == "uninstall") {
		return runAIStatuslineSetup(args[0] == "install")
	}
	if len(args) > 0 && args[0] != "--" {
		fmt.Fprintln(os.Stderr, T("observe.ai.statusline.usage"))
		return 2
	}
	var command []string
	if len(args) > 0 {
		command = args[1:]
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, aiStatuslineInputLimit))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	provider, ok := parseAIStatuslineInput(input, time.Now())
	if ok {
		if err := saveAIClaudeSnapshot(provider); err != nil {
			fmt.Fprintln(os.Stderr, T("observe.ai.statusline.write", err))
		}
	}
	if len(command) == 0 {
		fmt.Println(formatAIStatusline(provider))
		return 0
	}
	// 감싼 명령이 원래 상태 줄을 그린다. 사용량을 남기지 못해도 상태 줄은 그대로 보여야 한다.
	run := exec.Command(command[0], command[1:]...)
	run.Stdin, run.Stdout, run.Stderr = bytes.NewReader(input), os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// parseAIStatuslineInput은 statusline 입력의 rate_limits를 창 목록으로 바꾼다.
// rate_limits는 구독자에게만, 세션의 첫 응답 뒤에만 오므로 창이 없으면 false를 돌려준다.
func parseAIStatuslineInput(data []byte, now time.Time) (aiProvider, bool) {
	provider := aiProvider{Name: "claude", FetchedAt: now, Windows: []aiWindow{}}
	var input struct {
		RateLimits map[string]*struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *float64 `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(data, &input) != nil {
		return provider, false
	}
	for _, known := range aiStatuslineWindows {
		value := input.RateLimits[known.key]
		if value == nil || value.UsedPercentage == nil {
			continue
		}
		window := aiWindow{Name: known.name, Used: *value.UsedPercentage}
		if value.ResetsAt != nil {
			window.ResetsAt = time.Unix(int64(math.Round(*value.ResetsAt)), 0).UTC()
		}
		provider.Windows = append(provider.Windows, window)
	}
	return provider, len(provider.Windows) > 0
}

func formatAIStatusline(provider aiProvider) string {
	parts := make([]string, 0, len(provider.Windows))
	for _, window := range provider.Windows {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", window.Name, window.Used))
	}
	return strings.Join(parts, " · ")
}

func aiClaudeSnapshotPath() (string, error) {
	historyPath, err := defaultHistoryPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(historyPath), aiClaudeSnapshotName), nil
}

func saveAIClaudeSnapshot(provider aiProvider) error {
	path, err := aiClaudeSnapshotPath()
	if err != nil {
		return err
	}
	return saveAIClaudeState(path, aiClaudeState{aiProvider: provider})
}

// readAIClaudeSnapshot은 statusline이 남긴 값을 읽는다. 리셋 시각이 지난 창은 Claude Code처럼 뺀다.
func readAIClaudeSnapshot(path string, now time.Time) aiProvider {
	state, ok := loadAIClaudeState(path)
	if !ok || state.FetchedAt.IsZero() {
		provider := aiProvider{Name: "claude", Windows: []aiWindow{}, Err: "unreadable Claude usage from the status line"}
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			provider.Err = errAIStatuslineMissing.Error()
		}
		return provider
	}
	provider := state.aiProvider
	windows := []aiWindow{}
	for _, window := range provider.Windows {
		if window.ResetsAt.IsZero() || now.Before(window.ResetsAt) {
			windows = append(windows, window)
		}
	}
	provider.Windows = windows
	return provider
}

func runAIStatuslineSetup(install bool) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	path := filepath.Join(aiClaudeDir(home), "settings.json")
	// 심볼릭 링크를 풀지 않는다. 패키지 관리자는 버전마다 실제 경로를 바꾸고 링크만 유지한다.
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, T("observe.ai.statusline.settings", path, err))
		return 1
	}
	edit, err := editAIStatusline(data, executable, install)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.ai.statusline.settings", path, err))
		return 1
	}
	if !edit.changed {
		if install {
			fmt.Println(T("observe.ai.statusline.already", edit.before))
		} else {
			fmt.Println(T("observe.ai.statusline.not_installed"))
		}
		return 0
	}
	backup, err := writeAIClaudeSettings(path, data, edit.settings)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.ai.statusline.settings", path, err))
		return 1
	}
	fmt.Println(T("observe.ai.statusline.changed", aiStatuslineShown(edit.before), aiStatuslineShown(edit.after)))
	if backup != "" {
		fmt.Println(T("observe.ai.statusline.backup", backup))
	}
	return 0
}

func aiStatuslineShown(command string) string {
	if command == "" {
		return T("observe.ai.statusline.none")
	}
	return command
}

// writeAIClaudeSettings는 원래 파일을 백업한 뒤 임시 파일을 바꿔 넣는다. Claude Code가 반쯤 쓴 설정을 읽지 않게 한다.
func writeAIClaudeSettings(path string, original, settings []byte) (string, error) {
	mode := fs.FileMode(0o600)
	backup := ""
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		backup = path + aiStatuslineBackupSuffix
		if err := os.WriteFile(backup, original, mode); err != nil {
			return "", err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	if _, err := temp.Write(settings); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return "", err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return "", err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return "", err
	}
	return backup, os.Rename(temp.Name(), path)
}

type aiStatuslineEdit struct {
	before, after string
	settings      []byte
	changed       bool
}

// editAIStatusline은 statusLine.command만 바꾼다. 다른 키와 순서는 그대로 두고 들여쓰기만 다시 맞춘다.
func editAIStatusline(data []byte, executable string, install bool) (aiStatuslineEdit, error) {
	members, err := decodeAIJSONObject(data)
	if err != nil {
		return aiStatuslineEdit{}, err
	}
	index := aiJSONIndex(members, "statusLine")
	var line []aiJSONMember
	if index >= 0 {
		if line, err = decodeAIJSONObject(members[index].value); err != nil {
			return aiStatuslineEdit{}, fmt.Errorf("statusLine: %w", err)
		}
	}
	var edit aiStatuslineEdit
	commandIndex := aiJSONIndex(line, "command")
	if commandIndex >= 0 {
		if err := json.Unmarshal(line[commandIndex].value, &edit.before); err != nil {
			return aiStatuslineEdit{}, fmt.Errorf("statusLine.command: %w", err)
		}
	}
	inner, wrapped := unwrapAIStatusline(edit.before)
	switch {
	case install && wrapped, !install && !wrapped:
		return edit, nil
	case install:
		edit.after = wrapAIStatusline(executable, edit.before)
	default:
		edit.after = inner
	}
	edit.changed = true

	if !install && inner == "" {
		// install이 statusLine을 새로 만들었으면 지운다. 다른 키가 있으면 command만 지운다.
		line = append(line[:commandIndex], line[commandIndex+1:]...)
		if len(line) == 1 && line[0].key == "type" || len(line) == 0 {
			members = append(members[:index], members[index+1:]...)
		} else if members[index].value, err = encodeAIJSONObject(line); err != nil {
			return aiStatuslineEdit{}, err
		}
		edit.settings, err = encodeAIJSONObject(members)
		return edit, err
	}
	value, err := marshalAIJSONString(edit.after)
	if err != nil {
		return aiStatuslineEdit{}, err
	}
	if commandIndex >= 0 {
		line[commandIndex].value = value
	} else {
		if aiJSONIndex(line, "type") < 0 {
			line = append(line, aiJSONMember{"type", json.RawMessage(`"command"`)})
		}
		line = append(line, aiJSONMember{"command", value})
	}
	encoded, err := encodeAIJSONObject(line)
	if err != nil {
		return aiStatuslineEdit{}, err
	}
	if index >= 0 {
		members[index].value = encoded
	} else {
		members = append(members, aiJSONMember{"statusLine", encoded})
	}
	edit.settings, err = encodeAIJSONObject(members)
	return edit, err
}

// aiStatuslinePlainWord는 shell이 따옴표 없이 그대로 넘기는 낱말이다. ~는 shell이 home으로 펼치도록 둔다.
var aiStatuslinePlainWord = regexp.MustCompile(`^[A-Za-z0-9_./~:@%+=,-]+$`)

// aiStatuslineWrapper는 install이 쓴 command다. 실행 파일 경로는 따옴표로 감쌌을 수 있다.
var aiStatuslineWrapper = regexp.MustCompile(`^('[^']*'|\S+) ai statusline(?: -- (.*))?$`)

// wrapAIStatusline은 원래 command를 edc ai statusline 뒤에 붙인다. Claude Code는 command를 shell로 실행하므로
// 낱말만 있으면 그대로 붙이고, pipe나 따옴표가 있으면 sh -c로 감싸 원래 shell 문법을 지킨다.
func wrapAIStatusline(executable, command string) string {
	prefix := quoteAIShell(executable) + " ai statusline"
	switch {
	case command == "":
		return prefix
	case aiStatuslineIsPlain(command):
		return prefix + " -- " + command
	default:
		return prefix + " -- sh -c " + quoteAIShell(command)
	}
}

func unwrapAIStatusline(command string) (string, bool) {
	match := aiStatuslineWrapper.FindStringSubmatch(command)
	if match == nil || filepath.Base(strings.Trim(match[1], "'")) != "edc" {
		return "", false
	}
	inner := match[2]
	if quoted, ok := strings.CutPrefix(inner, "sh -c "); ok {
		if unquoted, ok := unquoteAIShell(quoted); ok {
			return unquoted, true
		}
	}
	return inner, true
}

func aiStatuslineIsPlain(command string) bool {
	for _, word := range strings.Split(command, " ") {
		if !aiStatuslinePlainWord.MatchString(word) {
			return false
		}
	}
	return true
}

func quoteAIShell(text string) string {
	if aiStatuslinePlainWord.MatchString(text) && !strings.HasPrefix(text, "~") {
		return text
	}
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// unquoteAIShell은 quoteAIShell이 만든 형식만 되돌린다. 사용자가 고친 다른 형식은 그대로 둔다.
func unquoteAIShell(text string) (string, bool) {
	if len(text) < 2 || text[0] != '\'' || text[len(text)-1] != '\'' {
		return "", false
	}
	unquoted := strings.ReplaceAll(text[1:len(text)-1], `'\''`, "'")
	if quoteAIShell(unquoted) != text {
		return "", false
	}
	return unquoted, true
}

type aiJSONMember struct {
	key   string
	value json.RawMessage
}

func aiJSONIndex(members []aiJSONMember, key string) int {
	for index, member := range members {
		if member.key == key {
			return index
		}
	}
	return -1
}

// decodeAIJSONObject는 객체의 키를 파일 순서대로 읽는다. map으로 읽으면 다시 쓸 때 키 순서가 바뀐다.
func decodeAIJSONObject(data []byte) ([]aiJSONMember, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var members []aiJSONMember
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var member aiJSONMember
		member.key, _ = token.(string)
		if err := decoder.Decode(&member.value); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON object")
	}
	return members, nil
}

func encodeAIJSONObject(members []aiJSONMember) ([]byte, error) {
	var compact bytes.Buffer
	compact.WriteByte('{')
	for index, member := range members {
		if index > 0 {
			compact.WriteByte(',')
		}
		key, err := marshalAIJSONString(member.key)
		if err != nil {
			return nil, err
		}
		compact.Write(key)
		compact.WriteByte(':')
		if err := json.Compact(&compact, member.value); err != nil {
			return nil, err
		}
	}
	compact.WriteByte('}')
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	indented.WriteByte('\n')
	return indented.Bytes(), nil
}

// marshalAIJSONString은 &, <, >를 그대로 둔다. shell command를 &으로 바꾸면 사람이 고치기 어렵다.
func marshalAIJSONString(text string) (json.RawMessage, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(text); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}
