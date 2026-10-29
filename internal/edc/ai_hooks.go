package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const (
	aiEventsName        = "ai-events.jsonl"
	aiHooksBackupSuffix = ".edc-hooks-backup"
	// aiEventsMax를 넘으면 hook이 파일을 .1로 넘긴다. sessions는 두 파일만 읽으므로 기록이 끝없이 늘지 않는다.
	aiEventsMax = 8 << 20
	// aiHookInputLimit은 hook 입력의 상한이다. Stop은 마지막 응답 전체를 넘기므로 statusline보다 크게 둔다.
	aiHookInputLimit = 4 << 20
	// aiHookDetailLimit은 기록에 남기는 설명의 길이다. 명령과 응답 원문을 통째로 남기지 않는다.
	aiHookDetailLimit = 160
	// aiHookTimeout은 설정에 적는 hook 제한 시간(초)이다. 기록은 수 ms에 끝나지만 디스크가 멈춰도 에이전트를 붙잡지 않는다.
	aiHookTimeout = 5
	// aiApprovedSlack 뒤에도 대화 기록이 늘었으면 승인 요청이 끝난 것으로 본다. 요청 직전 기록이 늦게 닫히는 경우를 넘긴다.
	aiApprovedSlack = 3 * time.Second
	// aiSessionKeep은 PID를 모르는 세션을 보이는 시간이다. hook을 설치하기 전에 시작한 세션은 SessionStart가 없다.
	aiSessionKeep = 12 * time.Hour
)

// aiHookEvents는 도구마다 설치하는 hook 이벤트다. Codex에는 실행해서 이벤트가 오는 것을 확인한 것만 단다.
// Codex 0.165는 PermissionRequest라는 이름을 갖고 있지만 승인 요청에서 부르는지 확인하지 못했다.
var aiHookEvents = map[string][]string{
	"claude": {"SessionStart", "SessionEnd", "UserPromptSubmit", "PermissionRequest", "Notification", "Stop", "StopFailure"},
	"codex":  {"SessionStart", "UserPromptSubmit", "Stop"},
}

var aiHookTools = []string{"claude", "codex"}

// aiHookWrapper는 install이 쓴 command다. 실행 파일 경로는 따옴표로 감쌌을 수 있다.
var aiHookWrapper = regexp.MustCompile(`^('[^']*'|\S+) ai hook (claude|codex)$`)

type aiHookEvent struct {
	At         time.Time `json:"at"`
	Tool       string    `json:"tool"`
	Event      string    `json:"event"`
	Session    string    `json:"session"`
	Transcript string    `json:"transcript,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	PID        int       `json:"pid,omitempty"`
	TTY        string    `json:"tty,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// runAIHook은 에이전트가 부르는 hook이다. 실패해도 0으로 끝낸다. 0이 아니면 Claude Code와 Codex가 작업을 멈추거나
// 오류를 보이고, stdout에 쓰면 hook 응답으로 읽힌다.
func runAIHook(args []string) int {
	if len(args) != 1 || aiHookEvents[args[0]] == nil {
		fmt.Fprintln(os.Stderr, T("observe.ai.hook.usage"))
		return 0
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, aiHookInputLimit))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 0
	}
	event, ok := parseAIHookInput(args[0], input, time.Now())
	if !ok {
		return 0
	}
	if event.Event == "SessionStart" {
		event.PID, event.TTY = aiAgentProcess(os.Getppid())
	}
	path, err := aiEventsPath()
	if err == nil {
		err = appendAIHookEvent(path, event)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.ai.hook.write", err))
	}
	return 0
}

func parseAIHookInput(tool string, data []byte, now time.Time) (aiHookEvent, bool) {
	var input struct {
		SessionID        string          `json:"session_id"`
		TranscriptPath   string          `json:"transcript_path"`
		Cwd              string          `json:"cwd"`
		Event            string          `json:"hook_event_name"`
		NotificationType string          `json:"notification_type"`
		Message          string          `json:"message"`
		ToolName         string          `json:"tool_name"`
		ToolInput        json.RawMessage `json:"tool_input"`
		LastMessage      string          `json:"last_assistant_message"`
		Reason           string          `json:"reason"`
		Error            string          `json:"error"`
		Prompt           string          `json:"prompt"`
	}
	if json.Unmarshal(data, &input) != nil || input.SessionID == "" || input.Event == "" {
		return aiHookEvent{}, false
	}
	event := aiHookEvent{
		At: now.UTC(), Tool: tool, Event: input.Event, Session: input.SessionID,
		Transcript: input.TranscriptPath, Cwd: input.Cwd,
	}
	switch input.Event {
	case "PermissionRequest":
		event.Detail = aiHookToolDetail(input.ToolName, input.ToolInput)
		// Claude Code는 AskUserQuestion의 선택 창도 승인 요청으로 보낸다. 승인할 명령이 아니라 답할 질문이다.
		if input.ToolName == "AskUserQuestion" {
			event.Kind, event.Detail = "ask", aiHookQuestion(input.ToolInput)
		}
	case "Notification":
		event.Kind, event.Detail = input.NotificationType, aiHookClip(input.Message, false)
	case "Stop":
		event.Detail = aiHookClip(input.LastMessage, true)
		if aiHookAsks(input.LastMessage) {
			event.Kind = "question"
		}
	case "StopFailure":
		event.Detail = aiHookClip(input.Error, false)
	case "UserPromptSubmit":
		event.Detail = aiHookClip(input.Prompt, false)
	case "SessionEnd", "SessionStart":
		event.Kind = input.Reason
	}
	return event, true
}

// aiHookToolDetail은 승인 요청이 무엇을 하려는지 한 줄로 줄인다. 도구 입력 전체는 파일 내용을 담을 수 있어 남기지 않는다.
func aiHookToolDetail(tool string, raw json.RawMessage) string {
	var input map[string]any
	_ = json.Unmarshal(raw, &input)
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "description"} {
		if value, ok := input[key].(string); ok && value != "" {
			return aiHookClip(tool+": "+value, false)
		}
	}
	return aiHookClip(tool, false)
}

// aiHookQuestion은 AskUserQuestion의 첫 질문을 꺼낸다. 질문이 없으면 도구 이름을 남긴다.
func aiHookQuestion(raw json.RawMessage) string {
	var input struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &input) != nil || len(input.Questions) == 0 || input.Questions[0].Question == "" {
		return "AskUserQuestion"
	}
	return aiHookClip(input.Questions[0].Question, false)
}

// aiHookAsks는 응답이 사용자에게 묻고 끝났는지 본다. 질문은 대개 응답의 마지막 줄에 있다.
func aiHookAsks(message string) bool {
	text := strings.TrimRight(strings.TrimSpace(message), "*_`)")
	return strings.HasSuffix(text, "?") || strings.HasSuffix(text, "？")
}

// aiHookClip은 여러 줄을 한 줄로 합치고 aiHookDetailLimit 글자로 자른다. tail이면 끝을 남긴다. 응답의 결론과 질문은 끝에 있다.
func aiHookClip(text string, tail bool) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) <= aiHookDetailLimit {
		return string(runes)
	}
	if tail {
		return "…" + string(runes[len(runes)-aiHookDetailLimit+1:])
	}
	return string(runes[:aiHookDetailLimit-1]) + "…"
}

// aiAgentProcess는 hook을 띄운 에이전트 process를 찾는다. 에이전트는 hook을 shell로 실행하므로 shell을 건너 올라간다.
func aiAgentProcess(pid int) (int, string) {
	for range 4 {
		output, err := exec.Command("ps", "-o", "ppid=,tty=,comm=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, ""
		}
		fields := strings.Fields(string(output))
		if len(fields) < 3 {
			return 0, ""
		}
		switch filepath.Base(strings.Join(fields[2:], " ")) {
		case "sh", "bash", "zsh", "dash", "fish", "-sh", "-bash", "-zsh", "edc":
			parent, err := strconv.Atoi(fields[0])
			if err != nil || parent <= 1 {
				return 0, ""
			}
			pid = parent
			continue
		}
		tty := fields[1]
		if tty == "??" || tty == "?" {
			tty = ""
		}
		return pid, tty
	}
	return 0, ""
}

func aiEventsPath() (string, error) {
	historyPath, err := defaultHistoryPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(historyPath), aiEventsName), nil
}

// appendAIHookEvent는 한 줄을 한 번의 write로 붙인다. O_APPEND의 한 write는 다른 hook의 줄과 섞이지 않는다.
func appendAIHookEvent(path string, event aiHookEvent) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > aiEventsMax {
		_ = os.Rename(path, path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func runAIHooksSetup(args []string) int {
	if len(args) != 1 || args[0] != "install" && args[0] != "uninstall" {
		fmt.Fprintln(os.Stderr, T("observe.ai.hooks.usage"))
		return 2
	}
	install := args[0] == "install"
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// 심볼릭 링크를 풀지 않는다. 패키지 관리자는 버전마다 실제 경로를 바꾸고 링크만 유지한다.
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	status := 0
	for _, tool := range aiHookTools {
		path := filepath.Join(aiClaudeDir(home), "settings.json")
		if tool == "codex" {
			path = filepath.Join(aiCodexDir(home), "hooks.json")
			if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
				continue
			}
		}
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, T("observe.ai.statusline.settings", path, err))
			status = 1
			continue
		}
		settings, changed, err := editAIHooks(data, tool, quoteAIShell(executable)+" ai hook "+tool, install)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("observe.ai.statusline.settings", path, err))
			status = 1
			continue
		}
		if len(changed) == 0 {
			fmt.Println(T("observe.ai.hooks.unchanged", path))
			continue
		}
		backup, err := writeAIClaudeSettings(path, data, settings, aiHooksBackupSuffix)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("observe.ai.statusline.settings", path, err))
			status = 1
			continue
		}
		key := "observe.ai.hooks.removed"
		if install {
			key = "observe.ai.hooks.added"
		}
		fmt.Println(T(key, path, strings.Join(changed, ", ")))
		if backup != "" {
			fmt.Println(T("observe.ai.statusline.backup", backup))
		}
	}
	return status
}

// editAIHooks는 hooks 아래 이벤트 배열에 edc 항목만 더하거나 뺀다. 다른 hook과 키 순서는 그대로 두고 들여쓰기만 다시 맞춘다.
// 바뀐 이벤트 이름을 돌려준다.
func editAIHooks(data []byte, tool, command string, install bool) ([]byte, []string, error) {
	members, err := decodeAIJSONObject(data)
	if err != nil {
		return nil, nil, err
	}
	index := aiJSONIndex(members, "hooks")
	var events []aiJSONMember
	if index >= 0 {
		if events, err = decodeAIJSONObject(members[index].value); err != nil {
			return nil, nil, fmt.Errorf("hooks: %w", err)
		}
	}
	var changed []string
	for _, name := range aiHookEvents[tool] {
		at := aiJSONIndex(events, name)
		var groups []json.RawMessage
		if at >= 0 {
			if err := json.Unmarshal(events[at].value, &groups); err != nil {
				return nil, nil, fmt.Errorf("hooks.%s: %w", name, err)
			}
		}
		kept, found, err := withoutAIHookCommand(groups)
		if err != nil {
			return nil, nil, fmt.Errorf("hooks.%s: %w", name, err)
		}
		if install == found {
			continue
		}
		changed = append(changed, name)
		if install {
			entry, err := aiHookGroup(command)
			if err != nil {
				return nil, nil, err
			}
			kept = append(groups, entry)
		}
		switch {
		case len(kept) == 0:
			events = append(events[:at], events[at+1:]...)
		case at >= 0:
			if events[at].value, err = json.Marshal(kept); err != nil {
				return nil, nil, err
			}
		default:
			value, err := json.Marshal(kept)
			if err != nil {
				return nil, nil, err
			}
			events = append(events, aiJSONMember{name, value})
		}
	}
	if len(changed) == 0 {
		return data, nil, nil
	}
	encoded, err := encodeAIJSONObject(events)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case index >= 0:
		members[index].value = encoded
	default:
		members = append(members, aiJSONMember{"hooks", encoded})
	}
	settings, err := encodeAIJSONObject(members)
	return settings, changed, err
}

func aiHookGroup(command string) (json.RawMessage, error) {
	value, err := marshalAIJSONString(command)
	if err != nil {
		return nil, err
	}
	return encodeAIJSONObject([]aiJSONMember{{"hooks", json.RawMessage(
		`[{"type":"command","command":` + string(value) + `,"timeout":` + strconv.Itoa(aiHookTimeout) + `}]`)}})
}

// withoutAIHookCommand는 edc ai hook을 부르는 항목을 뺀 배열을 돌려준다. 다른 hook과 같은 묶음에 있으면 그 항목만 빼고,
// 묶음이 비면 묶음도 뺀다.
func withoutAIHookCommand(groups []json.RawMessage) ([]json.RawMessage, bool, error) {
	kept := make([]json.RawMessage, 0, len(groups))
	found := false
	for _, group := range groups {
		fields, err := decodeAIJSONObject(group)
		if err != nil {
			return nil, false, err
		}
		at := aiJSONIndex(fields, "hooks")
		if at < 0 {
			kept = append(kept, group)
			continue
		}
		var hooks []json.RawMessage
		if err := json.Unmarshal(fields[at].value, &hooks); err != nil {
			return nil, false, err
		}
		others := make([]json.RawMessage, 0, len(hooks))
		for _, hook := range hooks {
			var item struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(hook, &item)
			if match := aiHookWrapper.FindStringSubmatch(item.Command); match != nil &&
				filepath.Base(strings.Trim(match[1], "'")) == "edc" {
				found = true
				continue
			}
			others = append(others, hook)
		}
		switch {
		case len(others) == len(hooks):
			kept = append(kept, group)
		case len(others) > 0:
			if fields[at].value, err = json.Marshal(others); err != nil {
				return nil, false, err
			}
			encoded, err := encodeAIJSONObject(fields)
			if err != nil {
				return nil, false, err
			}
			kept = append(kept, encoded)
		}
	}
	return kept, found, nil
}

type aiSessionState string

const (
	aiStateNeeds   aiSessionState = "needs"
	aiStateFailed  aiSessionState = "failed"
	aiStateWorking aiSessionState = "working"
	aiStateWaiting aiSessionState = "waiting"
	aiStateEnded   aiSessionState = "ended"
)

// aiStateOrder는 화면 순서다. 사용자를 기다리는 세션과 실패를 먼저 보인다.
var aiStateOrder = map[aiSessionState]int{aiStateNeeds: 0, aiStateFailed: 1, aiStateWorking: 2, aiStateWaiting: 3, aiStateEnded: 4}

type aiSession struct {
	Tool       string         `json:"tool"`
	Session    string         `json:"session"`
	State      aiSessionState `json:"state"`
	Reason     string         `json:"reason,omitempty"`
	Detail     string         `json:"detail,omitempty"`
	Since      time.Time      `json:"since"`
	Last       time.Time      `json:"last"`
	Cwd        string         `json:"cwd,omitempty"`
	PID        int            `json:"pid,omitempty"`
	TTY        string         `json:"tty,omitempty"`
	Transcript string         `json:"transcript,omitempty"`
	// Task는 마지막 프롬프트의 앞부분이다. 상태가 바뀌어도 남겨 무슨 일을 하는 세션인지 보인다.
	Task     string         `json:"task,omitempty"`
	Activity *aiActivity    `json:"activity,omitempty"`
	Info     *aiSessionInfo `json:"info,omitempty"`
}

func (session *aiSession) set(state aiSessionState, reason, detail string, at time.Time) {
	session.State, session.Reason, session.Detail, session.Since = state, reason, detail, at
}

// foldAIHookEvents는 이벤트를 세션마다 마지막 상태로 접는다. 이벤트는 기록 순서대로 온다.
func foldAIHookEvents(events []aiHookEvent) map[string]*aiSession {
	sessions := map[string]*aiSession{}
	for _, event := range events {
		key := event.Tool + "/" + event.Session
		session := sessions[key]
		if session == nil {
			session = &aiSession{Tool: event.Tool, Session: event.Session, State: aiStateWaiting, Since: event.At}
			sessions[key] = session
		}
		session.Last = event.At
		if event.Cwd != "" {
			session.Cwd = event.Cwd
		}
		if event.Transcript != "" {
			session.Transcript = event.Transcript
		}
		switch event.Event {
		case "SessionStart":
			if event.PID > 0 {
				session.PID, session.TTY = event.PID, event.TTY
			}
			session.set(aiStateWaiting, "started", "", event.At)
		case "UserPromptSubmit":
			if event.Detail != "" {
				session.Task = event.Detail
			}
			session.set(aiStateWorking, "", "", event.At)
		case "PermissionRequest":
			reason := "permission"
			if event.Kind == "ask" {
				reason = "ask"
			}
			session.set(aiStateNeeds, reason, event.Detail, event.At)
		case "Notification":
			switch event.Kind {
			case "permission_prompt":
				// PermissionRequest가 이미 같은 승인 요청을 도구 이름과 함께 남겼으면 그 설명을 둔다.
				if session.State != aiStateNeeds || session.Reason != "permission" && session.Reason != "ask" {
					session.set(aiStateNeeds, "permission", event.Detail, event.At)
				}
			case "elicitation_dialog":
				session.set(aiStateNeeds, "input", event.Detail, event.At)
			case "idle_prompt":
				if session.State != aiStateNeeds {
					session.set(aiStateWaiting, "idle", session.Detail, session.Since)
				}
			}
		case "Stop":
			if event.Kind == "question" {
				session.set(aiStateNeeds, "question", event.Detail, event.At)
			} else {
				session.set(aiStateWaiting, "done", event.Detail, event.At)
			}
		case "StopFailure":
			session.set(aiStateFailed, "error", event.Detail, event.At)
		case "SessionEnd":
			session.set(aiStateEnded, "", "", event.At)
		}
	}
	return sessions
}

// settleAISession은 hook이 알리지 않는 변화를 반영한다. 승인 뒤에는 다음 Stop까지 이벤트가 없으므로 대화 기록이 늘었는지 보고,
// 에이전트가 hook 없이 끝났으면 process로 판정한다.
func settleAISession(session *aiSession, now time.Time, alive func(int) bool, modified func(string) (time.Time, bool)) {
	if session.State == aiStateEnded {
		return
	}
	if session.State == aiStateNeeds && session.Reason != "question" && session.Transcript != "" {
		if at, ok := modified(session.Transcript); ok && at.After(session.Since.Add(aiApprovedSlack)) {
			session.set(aiStateWorking, "", "", at)
		}
	}
	switch {
	case session.PID > 0 && !alive(session.PID):
		session.set(aiStateEnded, "exited", "", session.Last)
	case session.PID == 0 && now.Sub(session.Last) > aiSessionKeep:
		session.set(aiStateEnded, "stale", "", session.Last)
	}
}

func aiProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func aiFileModified(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// aiEventTail은 이벤트 파일에서 새로 붙은 줄만 읽는다. 실시간 화면은 2초마다 읽으므로 8 MiB 파일을 매번 다시 읽지 않는다.
type aiEventTail struct {
	path   string
	offset int64
	events []aiHookEvent
}

// read는 마지막으로 읽은 곳부터 끝난 줄만 더한다. hook이 쓰는 중인 마지막 줄은 다음에 읽는다.
// 파일이 읽은 곳보다 짧아지면 hook이 .1로 넘긴 것이므로 두 파일을 처음부터 다시 읽는다.
func (tail *aiEventTail) read() error {
	info, err := os.Stat(tail.path)
	if errors.Is(err, fs.ErrNotExist) {
		tail.offset, tail.events = 0, nil
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() < tail.offset || tail.offset == 0 && len(tail.events) == 0 {
		tail.offset, tail.events = 0, nil
		if _, err := readAIHookLines(tail.path+".1", 0, &tail.events); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	next, err := readAIHookLines(tail.path, tail.offset, &tail.events)
	if err != nil {
		return err
	}
	tail.offset = next
	return nil
}

// readAIHookLines는 offset부터 마지막 줄바꿈까지 읽고 다음에 읽을 위치를 돌려준다.
func readAIHookLines(path string, offset int64, events *[]aiHookEvent) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			return offset, nil
		}
		if err != nil {
			return offset, err
		}
		offset += int64(len(line))
		var event aiHookEvent
		// 줄 하나가 깨져도 나머지 세션은 보인다.
		if json.Unmarshal(line, &event) == nil && event.Session != "" {
			*events = append(*events, event)
		}
	}
}

func readAIHookEvents(path string) ([]aiHookEvent, error) {
	tail := aiEventTail{path: path}
	if err := tail.read(); err != nil {
		return nil, err
	}
	sort.SliceStable(tail.events, func(i, j int) bool { return tail.events[i].At.Before(tail.events[j].At) })
	return tail.events, nil
}

func runAISessions(args []string) int {
	set := newFlagSet("ai sessions")
	set.SetOutput(os.Stderr)
	needs := set.Bool("needs", false, T("command.ai.option.needs"))
	all := set.Bool("all", false, T("command.ai.option.all"))
	bell := set.Bool("bell", false, T("command.ai.option.bell"))
	count := set.Int("count", 0, T("command.ai.option.sessions_count"))
	jsonPath := set.String("json", configuredStringFallback(nil, activeConfig.Defaults.Common.JSON, ""), T("option.json"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("observe.ai.sessions.usage"))
		return 2
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, T("observe.ai.count_minimum"))
		return 2
	}
	path, err := aiEventsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *jsonPath == "" && *count == 0 && liveTerminal() {
		return runAISessionsDashboard(path, *needs, *all, *bell)
	}
	var writer io.Writer = os.Stdout
	if *jsonPath != "" && *jsonPath != "-" {
		file, err := os.OpenFile(*jsonPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer file.Close()
		writer = file
	}
	tail := &aiEventTail{path: path}
	activities := aiActivityReader{}
	for sample := range max(*count, 1) {
		if sample > 0 {
			time.Sleep(aiScanInterval)
		}
		if err := tail.read(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		now := time.Now()
		sessions := selectAISessions(foldAIHookEvents(tail.events), now, *needs, *all, aiProcessAlive, aiFileModified)
		activities.annotate(sessions, now)
		switch {
		case *jsonPath != "":
			// 표본이 여럿이면 한 줄에 하나씩 쓴다. 다른 도구가 줄 단위로 읽는다.
			encoder := json.NewEncoder(writer)
			if *count <= 1 {
				encoder.SetIndent("", "  ")
			}
			if err := encoder.Encode(sessions); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		case len(tail.events) == 0:
			fmt.Fprintln(writer, T("observe.ai.sessions.no_events", path))
		default:
			if sample > 0 {
				fmt.Fprintln(writer)
			}
			writeAISessions(writer, sessions, now)
		}
	}
	return 0
}

func selectAISessions(folded map[string]*aiSession, now time.Time, needs, all bool, alive func(int) bool, modified func(string) (time.Time, bool)) []aiSession {
	sessions := []aiSession{}
	for _, session := range folded {
		settleAISession(session, now, alive, modified)
		switch {
		case needs && session.State != aiStateNeeds && session.State != aiStateFailed:
			continue
		case !all && session.State == aiStateEnded:
			continue
		}
		sessions = append(sessions, *session)
	}
	// 같은 상태 안에서는 오래 기다린 세션을 먼저 보인다.
	sort.Slice(sessions, func(i, j int) bool {
		if aiStateOrder[sessions[i].State] != aiStateOrder[sessions[j].State] {
			return aiStateOrder[sessions[i].State] < aiStateOrder[sessions[j].State]
		}
		return sessions[i].Since.Before(sessions[j].Since)
	})
	return sessions
}

// aiStateMarks는 색 없이도 상태를 가르는 기호다.
var aiStateMarks = map[aiSessionState]string{aiStateNeeds: "!", aiStateFailed: "×", aiStateWorking: "▸", aiStateWaiting: "·", aiStateEnded: " "}

func writeAISessions(writer io.Writer, sessions []aiSession, now time.Time) {
	if len(sessions) == 0 {
		fmt.Fprintln(writer, T("observe.ai.sessions.none"))
		return
	}
	for _, session := range sessions {
		fmt.Fprintln(writer, aiSessionLine(session, now))
	}
}

// aiSessionContextWidth는 줄 끝 컨텍스트 칸의 폭이다. "722K 72%"가 들어간다.
const aiSessionContextWidth = 9

// aiSessionLine은 terminal이 아닌 출력의 한 줄이다. 폭을 모르므로 컨텍스트를 설명 앞에 두어 열을 맞춘다.
func aiSessionLine(session aiSession, now time.Time) string {
	context, _ := aiSessionContext(session)
	line := aiSessionHead(session, now) + " " + padLeft(context, aiSessionContextWidth) + " " + aiSessionDetail(session, now)
	return strings.TrimRight(line, " ")
}

// aiSessionFit은 화면 폭에 맞춘 한 줄이다. 설명을 줄여 컨텍스트를 줄 끝에 고정한다. 줄마다 같은 자리에 오므로 훑어보기 쉽다.
// wide이면 막대와 크기, 비율을 모두 보이고, 아니면 비율만 보인다. 비율을 모르면 크기를 보인다.
func aiSessionFit(head, detail string, session aiSession, width int, wide bool) string {
	context, percent := aiSessionContext(session)
	right := aiContextBar(percent) + " " + padLeft(context, aiSessionContextWidth)
	if !wide {
		if percent >= 0 {
			context = fmt.Sprintf("%d%%", percent)
		}
		right = padLeft(context, 5)
	}
	room := max(0, width-liveWidth(head)-liveWidth(right)-2)
	left := aiPaintSession(session.State, head+" "+padRight(ansi.Truncate(detail, room, "…"), room))
	return left + " " + aiPaintContext(right, percent)
}

func aiSessionHead(session aiSession, now time.Time) string {
	project := filepath.Base(session.Cwd)
	if session.Cwd == "" {
		project = "-"
	}
	where := session.TTY
	if where == "" {
		where = "-"
	}
	return strings.Join([]string{aiStateMarks[session.State], padRight(aiSessionStateText(session), 18), padRight(session.Tool, 6),
		padRight(truncateName(project, 18), 18), padLeft(formatTopEventDuration(now.Sub(session.Since)), 6),
		padLeft(aiSessionTokens(session), 7), padRight(where, 8)}, " ")
}

// aiSessionTokens는 세션이 쓴 token이다. edc ai의 total과 같이 cache에서 읽은 token은 넣지 않는다.
// ↑는 최근 5분 동안 세션 평균보다 빠르게 쓰고 있다는 뜻이다.
func aiSessionTokens(session aiSession) string {
	if session.Info == nil || session.Info.Tokens.empty() {
		return "-"
	}
	text := aiCompact(session.Info.Tokens.total())
	if session.Info.Fast {
		text += "↑"
	}
	return text
}

// aiSessionContext는 마지막 요청의 컨텍스트와 창 대비 비율이다. 비율을 모르면 -1이다.
func aiSessionContext(session aiSession) (string, int) {
	info := session.Info
	if info == nil || info.Context <= 0 {
		return "-", -1
	}
	window, _ := aiSessionWindow(session)
	if window <= 0 {
		return aiCompact(info.Context), -1
	}
	percent := int(info.Context * 100 / window)
	return fmt.Sprintf("%s %d%%", aiCompact(info.Context), percent), percent
}

// aiSessionWindow는 컨텍스트 창 크기다. Claude는 기록에 창 크기가 없어, 200K를 넘은 세션만 1M 창으로 추정한다.
func aiSessionWindow(session aiSession) (int64, bool) {
	info := session.Info
	switch {
	case info == nil:
		return 0, false
	case info.ContextWindow > 0:
		return info.ContextWindow, false
	case session.Tool == "claude" && info.Context > aiClaudeSmallWindow:
		return aiClaudeLargeWindow, true
	}
	return 0, false
}

// aiContextBar는 5칸 막대다. 색이 없어도 칸 수로 남은 양을 가른다.
func aiContextBar(percent int) string {
	if percent < 0 {
		return "     "
	}
	filled := min(5, (percent+10)/20)
	return strings.Repeat("▰", filled) + strings.Repeat("▱", 5-filled)
}

// aiPaintContext는 곧 압축될 세션을 칠한다. 80% 이상은 노랑, 90% 이상은 빨강이다.
func aiPaintContext(text string, percent int) string {
	switch {
	case percent >= 90:
		return richPaint(text, richRed, true)
	case percent >= 80:
		return richPaint(text, richYellow, false)
	}
	return richDim(text)
}

// aiSessionDetail은 작업 중인 세션에는 지금 하는 일을, 활동을 모르면 받은 작업을 보인다. 이번 턴에 실패한 도구가 있으면 ✗와 그 수를 앞에 둔다.
func aiSessionDetail(session aiSession, now time.Time) string {
	if session.State != aiStateWorking {
		return session.Detail
	}
	text := aiActivityText(session.Activity, now)
	if text == "" {
		text = session.Task
	}
	if session.Info != nil && session.Info.TurnFailed > 0 {
		text = fmt.Sprintf("✗%d %s", session.Info.TurnFailed, text)
	}
	return text
}

func aiSessionStateText(session aiSession) string {
	state := T("observe.ai.sessions.state." + string(session.State))
	if session.Reason != "" {
		state += " · " + T("observe.ai.sessions.reason."+session.Reason)
	}
	return state
}
