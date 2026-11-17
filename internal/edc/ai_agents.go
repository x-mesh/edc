package edc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const (
	// aiTaskWindow는 실행 중인 작업을 찾으려고 읽는 대화 기록의 범위다. 이보다 오래 기록이 늘지 않은 세션과 agent는 보지 않는다.
	// 백그라운드 작업을 기다리는 세션은 그동안 기록을 쓰지 않으므로 짧게 잡으면 도는 작업을 놓친다.
	aiTaskWindow = 24 * time.Hour
	// aiAgentStale은 세션 본 대화가 턴 중간인데 기록이 늘지 않아도 작업 중으로 보는 시간이다. Bash 한 번의 제한 시간인 10분보다 길게 둔다.
	// 넘으면 Claude Code가 강제로 끝나 턴이 끊긴 것으로 본다.
	aiAgentStale          = 15 * time.Minute
	aiAgentFollowInterval = time.Second
	// aiAgentIDWidth는 목록에 보이는 ID의 앞부분이다. 인자는 ID의 어느 부분이든 받는다.
	aiAgentIDWidth = 9
	// aiAgentCandidates는 인자가 여러 항목에 맞을 때 보이는 후보 수다.
	aiAgentCandidates = 10
	// aiTaskOutputTail은 백그라운드 shell 출력에서 한 번에 읽는 상한이다. 출력 파일은 수 GB까지 커질 수 있다.
	// 따라가는 동안 한 번에 이보다 많이 늘면 앞부분을 건너뛰고, 줄바꿈 없이 이보다 많이 쌓이면 한 줄로 내보낸다.
	aiTaskOutputTail int64 = 64 << 10
	// aiShellLineLimit는 shell 출력 한 줄로 남기는 글자 수다. 활동 화면은 aiAgentsLogLimit줄을 들고 있다.
	aiShellLineLimit = 1000
	// aiTranscriptTailLimit는 끝부분에서 완전한 줄을 찾으려고 넓혀 읽는 상한이다. 마지막 응답이나 도구 결과 한 줄이
	// aiActivityTail보다 길면 그 창 안에는 완전한 줄이 없어 멈춤 여부를 알 수 없다.
	aiTranscriptTailLimit int64 = 16 << 20
)

type aiTaskState string

const (
	aiTaskRunning aiTaskState = "running"
	// aiTaskIdle은 본 대화는 쉬고 있지만 띄운 agent나 shell이 도는 세션이다.
	aiTaskIdle    aiTaskState = "idle"
	aiTaskStopped aiTaskState = "stopped"
	aiTaskKilled  aiTaskState = "killed"
	aiTaskFailed  aiTaskState = "failed"
	// aiTaskExpired는 monitor가 timeoutMs가 지나 스스로 끝난 상태다.
	aiTaskExpired aiTaskState = "expired"
)

// aiTaskMarks는 색 없이도 종류를 가르는 기호다.
var aiTaskMarks = map[string]string{"session": "●", "agent": "◆", "shell": "$", "monitor": "◷"}

// aiTask는 지금 도는 Claude Code 작업 하나다. Kind는 session(본 대화), agent(subagent), shell(run_in_background로 띄운 Bash),
// monitor(Monitor 도구로 건 감시)다. Expires는 monitor가 스스로 끝나는 시각이다.
// Parent는 shell을 띄운 agent의 ID다. 세션이 직접 띄웠으면 비어 있다.
type aiTask struct {
	Kind        string       `json:"kind"`
	ID          string       `json:"id"`
	Session     string       `json:"session"`
	Parent      string       `json:"parent,omitempty"`
	Depth       int          `json:"depth"`
	Cwd         string       `json:"cwd,omitempty"`
	Type        string       `json:"type,omitempty"`
	Description string       `json:"description,omitempty"`
	Model       string       `json:"model,omitempty"`
	State       aiTaskState  `json:"state"`
	Started     time.Time    `json:"started"`
	Last        time.Time    `json:"last"`
	Transcript  string       `json:"transcript,omitempty"`
	Output      string       `json:"output,omitempty"`
	Expires     time.Time    `json:"expires,omitzero"`
	Summary     string       `json:"summary,omitempty"`
	Step        *aiAgentStep `json:"step,omitempty"`
}

// aiAgentStep은 활동 한 줄이다. Kind는 prompt, text, tool, failed, output, stopped다.
// 도구 입력과 결과 전체는 파일 내용을 담을 수 있어 aiHookClip으로 줄인 것만 남긴다. thinking 내용은 남기지 않는다.
type aiAgentStep struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Tool string    `json:"tool,omitempty"`
	Text string    `json:"text,omitempty"`
}

// aiAgentParser는 대화 기록을 줄 순서대로 읽어 활동과 멈춤 여부를 얻는다.
type aiAgentParser struct {
	// tools는 결과가 올 때 도구 이름을 붙이려고 남기는 tool_use ID별 이름이다. 병렬 호출의 결과는 순서가 섞인다.
	tools   map[string]string
	stopped bool
	// decided는 멈춤 여부를 정하는 줄(user, assistant, api-request)을 하나라도 읽었는지다.
	decided bool
	cwd     string
	started time.Time
	last    time.Time
}

func newAIAgentParser() *aiAgentParser { return &aiAgentParser{tools: map[string]string{}} }

// line은 한 줄의 활동을 돌려준다. 마지막 응답이 도구 호출 없이 끝났으면 stopped가 된다. 응답은 블록마다 한 줄씩 기록되고
// 줄마다 응답의 최종 stop_reason이 있다. 다음 요청이 시작되면 다시 실행 중이다.
func (parser *aiAgentParser) line(data []byte) []aiAgentStep {
	var entry struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Cwd       string    `json:"cwd"`
		IsMeta    bool      `json:"isMeta"`
		Message   struct {
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &entry) != nil {
		return nil
	}
	// api-request-blob도 message를 담지만 API에 보낸 대화의 사본이다. 활동은 user와 assistant 줄에서만 얻는다.
	switch entry.Type {
	case "api-request":
		parser.stopped, parser.decided = false, true
		return nil
	case "user", "assistant":
		parser.decided = true
	default:
		return nil
	}
	if parser.cwd == "" {
		parser.cwd = entry.Cwd
	}
	if parser.started.IsZero() {
		parser.started = entry.Timestamp
	}
	parser.last = entry.Timestamp
	parser.stopped = entry.Type == "assistant" && entry.Message.StopReason != "" && entry.Message.StopReason != "tool_use"
	// 문자열 content는 agent에게 준 작업이나 SendMessage로 이어 준 지시다. 도구 결과는 배열로 온다.
	var prompt string
	if entry.Type == "user" && json.Unmarshal(entry.Message.Content, &prompt) == nil {
		if entry.IsMeta || strings.TrimSpace(prompt) == "" {
			return nil
		}
		return []aiAgentStep{{At: entry.Timestamp, Kind: "prompt", Text: aiHookClip(aiPromptText(prompt), false)}}
	}
	var parts []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		ToolUseID string          `json:"tool_use_id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Text      string          `json:"text"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(entry.Message.Content, &parts) != nil {
		return nil
	}
	var steps []aiAgentStep
	for _, part := range parts {
		switch {
		case part.Type == "tool_use":
			parser.tools[part.ID] = part.Name
			steps = append(steps, aiAgentStep{At: entry.Timestamp, Kind: "tool", Tool: part.Name, Text: aiAgentToolText(part.Name, part.Input)})
		case part.Type == "tool_result" && part.IsError:
			steps = append(steps, aiAgentStep{At: entry.Timestamp, Kind: "failed", Tool: parser.tools[part.ToolUseID], Text: aiHookClip(aiClaudeResultText(part.Content), false)})
		case part.Type == "text" && entry.Type == "assistant" && strings.TrimSpace(part.Text) != "":
			steps = append(steps, aiAgentStep{At: entry.Timestamp, Kind: "text", Text: aiHookClip(part.Text, false)})
		}
	}
	return steps
}

// aiNotificationField는 알림의 태그 값이다. 160자로 잘린 이벤트에서도 읽으려고 닫는 태그를 요구하지 않는다.
var aiNotificationField = regexp.MustCompile(`<(task-id|status|summary)>([^<]*)`)

// aiPromptText는 프롬프트를 목록 한 줄에 맞게 바꾼다. Claude Code가 백그라운드 작업의 완료를 알리려고 넣은 프롬프트는
// 태그와 출력 경로가 앞에 와서, 160자로 자르면 무엇이 끝났는지가 잘린다. 그런 프롬프트는 요약 한 줄로 바꾼다.
func aiPromptText(prompt string) string {
	start := strings.Index(prompt, "<task-notification>")
	if start < 0 {
		return prompt
	}
	fields := map[string]string{}
	for _, match := range aiNotificationField.FindAllStringSubmatch(prompt[start:], -1) {
		if fields[match[1]] == "" {
			fields[match[1]] = html.UnescapeString(strings.TrimSuffix(strings.TrimSpace(match[2]), "…"))
		}
	}
	if summary := fields["summary"]; summary != "" {
		return "↩ " + summary
	}
	return strings.TrimSpace("↩ task " + fields["task-id"] + " " + fields["status"])
}

// aiAgentToolText는 도구 호출을 한 줄로 줄인다. Bash는 명령 앞에 설명을 둔다. 명령은 cd나 변수 대입으로 시작해 앞부분만으로는 하는 일을 알기 어렵다.
func aiAgentToolText(name string, raw json.RawMessage) string {
	var input map[string]any
	_ = json.Unmarshal(raw, &input)
	value := func(key string) string {
		text, _ := input[key].(string)
		return strings.TrimSpace(text)
	}
	if description := value("description"); name == "Bash" && description != "" {
		return aiHookClip(description+" · "+value("command"), false)
	}
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "skill", "query", "description"} {
		if text := value(key); text != "" {
			return aiHookClip(text, false)
		}
	}
	return ""
}

// readAIAgentLines는 offset부터 끝난 줄만 parser에 넘기고 다음에 읽을 위치를 돌려준다. 쓰는 중인 마지막 줄은 다음에 읽는다.
// api-request-blob 줄은 수 MB일 수 있어 bufio.Scanner의 줄 상한을 쓰지 않는다.
func readAIAgentLines(path string, offset int64, parser *aiAgentParser, emit func(aiAgentStep)) (int64, error) {
	return readAITranscriptLines(path, offset, func(line []byte) {
		for _, step := range parser.line(line) {
			emit(step)
		}
	})
}

func readAITranscriptLines(path string, offset int64, each func([]byte)) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	// 줄마다 새 slice를 만들지 않는다. 24시간 기록을 처음 읽을 때 수백 MB를 할당했다. each에 넘긴 줄은 다음 줄을 읽으면 바뀌므로
	// each는 줄을 붙잡지 말고 해석한 값만 남긴다. 버퍼보다 긴 줄(api-request-blob은 수 MB)은 long에 모은다.
	reader := bufio.NewReaderSize(file, 256<<10)
	var long []byte
	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, line...)
			continue
		}
		if err == io.EOF {
			return offset, nil
		}
		if err != nil {
			return offset, err
		}
		joined := len(long) > 0
		if joined {
			long = append(long, line...)
			line = long
		}
		offset += int64(len(line))
		each(line)
		if joined {
			long = long[:0]
		}
	}
}

// aiLineStart는 offset 뒤의 첫 줄이 시작하는 곳이다. 큰 세션 기록을 끝부분부터 읽을 때 잘린 첫 줄을 건너뛴다.
func aiLineStart(path string, offset int64) int64 {
	if offset <= 0 {
		return 0
	}
	file, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer file.Close()
	if _, err := file.Seek(offset-1, io.SeekStart); err != nil {
		return offset
	}
	skipped, err := bufio.NewReaderSize(file, 64<<10).ReadBytes('\n')
	if err != nil {
		return offset
	}
	return offset - 1 + int64(len(skipped))
}

// aiBackground는 대화 기록이 띄운 백그라운드 작업이다. Bash의 run_in_background 호출이나 비동기 agent다.
// Status는 완료 알림의 상태다. 비어 있으면 아직 알림이 없다.
type aiBackground struct {
	kind        string
	id          string
	description string
	output      string
	started     time.Time
	ended       time.Time
	status      string
	summary     string
	// deadline은 monitor가 timeoutMs 뒤에 스스로 끝나는 시각이다. persistent monitor는 비어 있다.
	deadline time.Time
	// event는 monitor의 마지막 이벤트다. 상태 없는 알림으로 온다.
	event   string
	eventAt time.Time
}

// aiTranscript는 대화 기록 하나를 읽은 상태다. 백그라운드 작업의 시작과 완료는 처음부터 이어 읽고, 멈춤 여부와 마지막 활동은 끝부분에서 얻는다.
type aiTranscript struct {
	size     int64
	modified time.Time
	offset   int64
	// launches는 결과를 기다리는 run_in_background 호출의 tool_use ID별 설명이다.
	launches map[string]string
	tasks    map[string]*aiBackground
	order    []string
	// halted는 이 기록의 TaskStop 호출이 멈춘 작업 ID다. 다른 기록이 띄운 작업을 멈출 수도 있어 목록에서 모아 쓴다.
	halted  map[string]time.Time
	stopped bool
	cwd     string
	started time.Time
	step    *aiAgentStep
	meta    aiAgentMeta
}

type aiAgentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	Model       string `json:"model"`
}

var (
	// aiBackgroundOutput은 Bash 시작 결과에 적힌 출력 파일이다. 구조화된 필드에는 경로가 없다.
	aiBackgroundOutput = regexp.MustCompile(`Output is being written to: (\S+?)\.?(?:\s|$)`)
	aiTaskNotification = regexp.MustCompile(`(?s)<task-notification>.*?</task-notification>`)
	// aiShellEndLine은 shell이 끝나면 Claude Code가 출력 끝에 붙이는 줄이다. 띄운 agent가 먼저 멈췄거나 강제로 멈춘 shell은
	// 완료 알림이 기록에 남지 않으므로 이 줄을 먼저 본다.
	aiShellEndLine    = regexp.MustCompile(`^\[(?:exited with code (-?\d+)|(killed))\]$`)
	aiNotificationTag = regexp.MustCompile(`(?s)<(task-id|status|summary|event)>(.*?)</(?:task-id|status|summary|event)>`)
)

// index는 지난번 뒤에 끝난 줄에서 백그라운드 작업의 시작과 완료 알림을 찾는다. 대부분의 줄은 문자열 검사만으로 건너뛴다.
// aiLineKind는 색인이 줄마다 할 검색을 고르려고 짐작한 줄 종류다. 줄 전체를 해석하지 않고 앞뒤의 짧은 구간만 본다.
type aiLineKind int

const (
	// aiLineUnknown은 종류를 가르지 못한 줄이다. 모든 표시 문구를 찾는다. Claude Code가 키 순서를 바꾸면 느려질 뿐 놓치지 않는다.
	aiLineUnknown aiLineKind = iota
	aiLineSkip
	aiLineUser
	aiLineAssistant
	aiLineQueue
)

const (
	// aiLineHead는 user와 attachment 줄의 최상위 키를 보는 앞부분이다. 두 줄은 parentUuid, isSidechain 다음에 그 키가 온다.
	aiLineHead = 256
	// aiLineTail은 assistant 줄의 type 키를 보는 끝부분이다. assistant 줄은 message가 먼저 오고 type은 끝에서 2 KiB 안에 있었다.
	aiLineTail = 4 << 10
)

// classifyAILine은 줄 종류를 짐작한다. 검색 비용의 대부분은 줄마다 문구를 찾는 데서 나와서, 필요한 검색만 하면 처음 읽기가 줄어든다.
// api-request 계열은 API에 보낸 요청의 사본이고 attachment는 hook 결과나 알림의 사본이라 건너뛴다.
func classifyAILine(line []byte) aiLineKind {
	switch {
	case bytes.HasPrefix(line, []byte(`{"type":"api-request`)):
		return aiLineSkip
	case bytes.HasPrefix(line, []byte(`{"type":"queue-operation"`)):
		return aiLineQueue
	}
	head := line[:min(len(line), aiLineHead)]
	// message 뒤의 내용에 같은 글자가 인용돼도 attachment로 보지 않도록 message 키보다 앞에 있을 때만 쓴다.
	if attachment := bytes.Index(head, []byte(`"attachment":{`)); attachment >= 0 {
		if message := bytes.Index(head, []byte(`"message":`)); message < 0 || attachment < message {
			return aiLineSkip
		}
	}
	if bytes.Contains(head, []byte(`"type":"user"`)) {
		return aiLineUser
	}
	if bytes.Contains(line[max(0, len(line)-aiLineTail):], []byte(`"type":"assistant"`)) {
		return aiLineAssistant
	}
	return aiLineUnknown
}

// index는 지난번 뒤에 끝난 줄에서 백그라운드 작업의 시작과 완료 알림을 찾는다. 시작 호출은 assistant 줄에, 시작 결과와
// TaskStop은 user 줄의 toolUseResult에, 알림은 queue-operation 줄과 user 줄에 있다.
func (transcript *aiTranscript) index(path string) {
	transcript.offset, _ = readAITranscriptLines(path, transcript.offset, func(line []byte) {
		kind := classifyAILine(line)
		if kind == aiLineSkip {
			return
		}
		if kind == aiLineUser || kind == aiLineUnknown {
			// 처음 나온 자리부터 줄 끝까지 보면 실제 toolUseResult 키가 늘 들어 있다. bytes.LastIndex는 bytes.Index보다 훨씬 느려
			// 처음 읽기를 두 배로 늘렸다. 결과 문구에 "background" 같은 글자가 섞여도 결과 부분만 보므로 다른 분기로 새지 않는다.
			// 찾는 문구는 흔하지 않은 글자로 시작하게 자른다. JSON에 흔한 "로 시작하면 bytes.Contains가 3배 느렸다.
			if index := bytes.Index(line, []byte(`UseResult":`)); index >= 0 {
				switch result := line[index:]; {
				case bytes.Contains(result, []byte(`"backgroundTaskId"`)), bytes.Contains(result, []byte(`"async_launched"`)), bytes.Contains(result, []byte(`"timeoutMs"`)):
					transcript.indexStarted(line)
				case bytes.Contains(result, []byte(`"task_type"`)):
					transcript.indexStopped(line)
				}
			}
		}
		if kind == aiLineAssistant || kind == aiLineUnknown {
			// Monitor 호출은 tool_use 블록에서 name 바로 뒤에 input이 왔다. 도구 정의 목록의 "name":"Monitor","description"은
			// 거른다. 순서가 바뀌어 놓쳐도 monitor 설명만 빠지고, 작업은 시작 결과로 찾는다.
			if bytes.Contains(line, []byte(`_in_background":true`)) || bytes.Contains(line, []byte(`Monitor","input"`)) {
				transcript.indexLaunch(line)
			}
		}
		if kind != aiLineAssistant && bytes.Contains(line, []byte("<task-notification>")) {
			transcript.indexNotification(line)
		}
	})
}

// indexStopped는 TaskStop 호출의 결과다. 멈춘 shell은 출력 끝에 [killed]가 붙지만 monitor는 그런 줄이 없다.
func (transcript *aiTranscript) indexStopped(line []byte) bool {
	var entry struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Result    struct {
			Message string `json:"message"`
			TaskID  string `json:"task_id"`
		} `json:"toolUseResult"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "user" || entry.Result.TaskID == "" ||
		!strings.HasPrefix(entry.Result.Message, "Successfully stopped task") {
		return false
	}
	if transcript.halted == nil {
		transcript.halted = map[string]time.Time{}
	}
	keepEarliestStop(transcript.halted, entry.Result.TaskID, entry.Timestamp)
	if task := transcript.tasks[entry.Result.TaskID]; task != nil && task.status == "" {
		task.status, task.ended = "killed", entry.Timestamp
	}
	return true
}

func (transcript *aiTranscript) indexLaunch(line []byte) {
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input struct {
					Description string `json:"description"`
					Command     string `json:"command"`
					Background  bool   `json:"run_in_background"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
		return
	}
	for _, part := range entry.Message.Content {
		if part.Type == "tool_use" && (part.Input.Background || part.Name == "Monitor") {
			description := part.Input.Description
			if description == "" {
				description = part.Input.Command
			}
			transcript.launches[part.ID] = aiHookClip(description, false)
		}
	}
}

// indexStarted는 백그라운드 작업이 시작됐다는 도구 결과다. Bash는 toolUseResult.backgroundTaskId, agent는 status async_launched를 남긴다.
func (transcript *aiTranscript) indexStarted(line []byte) bool {
	var entry struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Result    struct {
			BackgroundTaskID string `json:"backgroundTaskId"`
			TaskID           string `json:"taskId"`
			TimeoutMs        int64  `json:"timeoutMs"`
			Persistent       bool   `json:"persistent"`
			Status           string `json:"status"`
			AgentID          string `json:"agentId"`
			Description      string `json:"description"`
			OutputFile       string `json:"outputFile"`
		} `json:"toolUseResult"`
		Message struct {
			Content []struct {
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "user" {
		return false
	}
	task := &aiBackground{started: entry.Timestamp}
	switch {
	case entry.Result.BackgroundTaskID != "":
		task.kind, task.id = "shell", entry.Result.BackgroundTaskID
		for _, part := range entry.Message.Content {
			task.description = transcript.launches[part.ToolUseID]
			delete(transcript.launches, part.ToolUseID)
			if match := aiBackgroundOutput.FindStringSubmatch(aiClaudeResultText(part.Content)); match != nil {
				task.output = match[1]
			}
		}
	case entry.Result.TaskID != "" && (entry.Result.TimeoutMs > 0 || entry.Result.Persistent):
		// Monitor 도구의 결과다. 시작 결과에는 출력 경로가 없다.
		task.kind, task.id = "monitor", entry.Result.TaskID
		for _, part := range entry.Message.Content {
			task.description = transcript.launches[part.ToolUseID]
			delete(transcript.launches, part.ToolUseID)
		}
		if !entry.Result.Persistent {
			task.deadline = entry.Timestamp.Add(time.Duration(entry.Result.TimeoutMs) * time.Millisecond)
		}
	case entry.Result.Status == "async_launched" && entry.Result.AgentID != "":
		task.kind, task.id, task.description, task.output = "agent", entry.Result.AgentID, entry.Result.Description, entry.Result.OutputFile
	default:
		return false
	}
	if transcript.tasks[task.id] == nil {
		transcript.order = append(transcript.order, task.id)
	}
	transcript.tasks[task.id] = task
	return true
}

// aiNotification은 알림 한 블록이다.
// source는 알림이 온 줄의 type이다. 세션 기록은 같은 알림을 queue-operation 줄과 user 줄에 겹쳐 받고, agent 기록은 user 줄로만 받는다.
type aiNotification struct {
	at     time.Time
	source string
	fields map[string]string
}

// aiNotifications는 한 줄에 든 알림이다. 알림은 queue-operation 줄과 user 줄에 함께 오고, user 줄은 안내 문구 뒤에 붙어 온다.
func aiNotifications(line []byte) []aiNotification {
	var entry struct {
		Type      string          `json:"type"`
		Timestamp time.Time       `json:"timestamp"`
		Content   string          `json:"content"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return nil
	}
	text := entry.Content
	if entry.Type == "user" {
		var message struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(entry.Message, &message) != nil {
			return nil
		}
		text = message.Content
	} else if entry.Type != "queue-operation" {
		return nil
	}
	var notifications []aiNotification
	for _, block := range aiTaskNotification.FindAllString(text, -1) {
		fields := map[string]string{}
		for _, match := range aiNotificationTag.FindAllStringSubmatch(block, -1) {
			fields[match[1]] = html.UnescapeString(strings.TrimSpace(match[2]))
		}
		notifications = append(notifications, aiNotification{at: entry.Timestamp, source: entry.Type, fields: fields})
	}
	return notifications
}

// aiMonitorExpired는 monitor가 만료됐다는 이벤트다. 만료 알림에는 상태가 없다.
func aiMonitorExpired(event string) bool { return strings.HasPrefix(event, "[Monitor expired") }

// indexNotification은 알림을 이 기록이 띄운 작업에 붙인다. 대화에 인용한 알림이 섞여도 이 기록이 띄운 작업 ID에만 붙인다.
// 상태가 있으면 끝났다는 뜻이다. monitor는 이벤트마다 상태 없는 알림을 받는다.
func (transcript *aiTranscript) indexNotification(line []byte) {
	for _, notification := range aiNotifications(line) {
		fields := notification.fields
		task := transcript.tasks[fields["task-id"]]
		switch {
		case task == nil:
		case fields["status"] != "" && task.status == fields["status"] && !task.ended.IsZero() && notification.at.After(task.ended):
			// 같은 알림이 queue-operation 줄과 user 줄에 몇 초 간격으로 겹쳐 온다. 늦은 사본이 끝난 시각을 늦추면 만료 전에 끝난
			// monitor가 만료로 보인다.
		case fields["status"] != "":
			task.status, task.ended, task.summary = fields["status"], notification.at, fields["summary"]
		case fields["event"] != "":
			task.event, task.eventAt = aiHookClip(fields["event"], false), notification.at
			if aiMonitorExpired(fields["event"]) && task.status == "" {
				task.status, task.ended, task.summary = string(aiTaskExpired), notification.at, fields["summary"]
			}
		}
	}
}

// tail은 끝부분에서 멈춤 여부와 마지막 활동을 다시 읽는다. 첫 줄의 시작 시각과 디렉터리는 한 번만 읽는다.
func (transcript *aiTranscript) tail(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	// 세션 기록은 대화가 아닌 줄로 시작하기도 한다. 앞부분에서 시각과 디렉터리가 있는 첫 줄을 쓴다.
	if transcript.started.IsZero() {
		head := bufio.NewReaderSize(file, 64<<10)
		for range 50 {
			line, err := head.ReadBytes('\n')
			if err != nil {
				break
			}
			var entry struct {
				Timestamp time.Time `json:"timestamp"`
				Cwd       string    `json:"cwd"`
			}
			if json.Unmarshal(line, &entry) != nil {
				continue
			}
			if transcript.started.IsZero() {
				transcript.started = entry.Timestamp
			}
			if transcript.cwd == "" {
				transcript.cwd = entry.Cwd
			}
			if !transcript.started.IsZero() && transcript.cwd != "" {
				break
			}
		}
	}
	var parser *aiAgentParser
	var last *aiAgentStep
	for window := aiActivityTail; ; window *= 4 {
		start := transcript.size - window
		if start < 0 {
			start = 0
		}
		data := make([]byte, transcript.size-start)
		if _, err := file.ReadAt(data, start); err != nil && err != io.EOF {
			return
		}
		if start > 0 {
			// 중간에서 시작한 첫 줄은 잘린 JSON이다.
			if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
				data = data[cut+1:]
			}
		}
		parser, last = newAIAgentParser(), nil
		for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
			// 줄바꿈이 없는 마지막 줄은 아직 쓰는 중이다.
			if !bytes.HasSuffix(line, []byte{'\n'}) {
				break
			}
			for _, step := range parser.line(line) {
				last = &step
			}
		}
		// 창 안에 대화 줄이 하나도 없으면 마지막 응답이나 도구 결과 한 줄이 창보다 길다. 그 뒤에 attachment 같은 짧은 줄이
		// 붙어 있어도 같다. 넓혀 읽어야 그 줄의 stop_reason을 본다.
		if start == 0 || parser.decided || window >= aiTranscriptTailLimit {
			break
		}
	}
	if transcript.cwd == "" {
		transcript.cwd = parser.cwd
	}
	transcript.stopped, transcript.step = parser.stopped, last
}

// aiTaskReader는 바뀐 대화 기록만 다시 읽는다. 실시간 화면은 1초마다 모든 기록을 본다.
type aiTaskReader struct {
	transcripts map[string]*aiTranscript
	// hooks는 edc ai hooks가 남긴 이벤트다. 있으면 세션 process가 끝났는지 보고, 알림 없이 끊긴 작업을 뺀다.
	hooks *aiEventTail
	// projects는 프로젝트 디렉터리별 세션 기록 목록이다. 디렉터리의 수정 시각이 바뀌었을 때만 다시 읽는다.
	// 1초마다 projects/*/*/subagents를 glob하면 세션 디렉터리 수천 개를 확인해 읽기 한 번의 대부분을 쓴다.
	projects map[string]*aiProjectDir
	// recent는 지난번 stat에서 aiTaskWindow 안이었던 세션 기록이다. 그 밖의 기록은 aiTaskSweep마다 한 번만 stat한다.
	recent map[string]bool
	swept  time.Time
	// pinned는 aiTaskWindow 밖이어도 읽는 세션 기록이다. 지난 읽기에서 도는 agent나 shell이 있었던 기록과, hook이 아는
	// 살아 있는 세션의 기록이다. 며칠 도는 개발 서버 같은 shell은 그동안 세션 기록이 바뀌지 않는다.
	pinned map[string]bool
	alive  map[string]bool
}

// aiTaskSweep은 오래된 세션 기록까지 모두 stat하는 간격이다. 기록 파일에 덧붙여도 디렉터리 수정 시각은 바뀌지 않으므로,
// 오래 쉬던 세션을 이어 쓰기 시작하면 이 간격만큼 늦게 보일 수 있다.
const aiTaskSweep = 10 * time.Second

type aiProjectDir struct {
	modified time.Time
	sessions []string
}

func newAITaskReader() *aiTaskReader {
	reader := &aiTaskReader{transcripts: map[string]*aiTranscript{}}
	if path, err := aiEventsPath(); err == nil {
		reader.hooks = &aiEventTail{path: path}
	}
	return reader
}

func (reader *aiTaskReader) read(path string, stat os.FileInfo) *aiTranscript {
	transcript, stale := reader.entry(path, stat)
	if stale {
		transcript.refresh(path)
	}
	return transcript
}

// entry는 path의 상태와, 기록이 바뀌어 다시 읽어야 하는지다. map은 여기서만 고친다.
func (reader *aiTaskReader) entry(path string, stat os.FileInfo) (*aiTranscript, bool) {
	transcript := reader.transcripts[path]
	if transcript == nil || stat.Size() < transcript.offset {
		transcript = &aiTranscript{launches: map[string]string{}, tasks: map[string]*aiBackground{}}
		if data, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json"); err == nil {
			_ = json.Unmarshal(data, &transcript.meta)
		}
		reader.transcripts[path] = transcript
	}
	if transcript.size == stat.Size() && transcript.modified.Equal(stat.ModTime()) {
		return transcript, false
	}
	transcript.size, transcript.modified = stat.Size(), stat.ModTime()
	return transcript, true
}

func (transcript *aiTranscript) refresh(path string) {
	transcript.index(path)
	transcript.tail(path)
}

// aiTaskIndexWorkers는 기록을 동시에 색인하는 수의 상한이다. 처음 읽을 때 기록 수십 개를 한 번에 읽는다.
const aiTaskIndexWorkers = 8

// prepare는 files 가운데 바뀐 기록을 동시에 다시 읽는다. 기록마다 상태가 따로라 goroutine 하나가 하나씩만 고친다.
// 그 뒤의 read는 바뀐 것이 없어 바로 돌아온다.
func (reader *aiTaskReader) prepare(files []aiTaskFile) {
	type work struct {
		path       string
		transcript *aiTranscript
	}
	var jobs []work
	for _, file := range files {
		if transcript, stale := reader.entry(file.path, file.stat); stale {
			jobs = append(jobs, work{file.path, transcript})
		}
	}
	if len(jobs) < 2 {
		for _, job := range jobs {
			job.transcript.refresh(job.path)
		}
		return
	}
	queue := make(chan work)
	var wait sync.WaitGroup
	for range min(len(jobs), runtime.GOMAXPROCS(0), aiTaskIndexWorkers) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for job := range queue {
				job.transcript.refresh(job.path)
			}
		}()
	}
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	wait.Wait()
}

// endedSessions는 hook 이벤트로 끝났다고 아는 Claude 세션이다. 살아 있다고 아는 세션의 기록은 reader.alive에 남긴다.
// hook이 없으면 둘 다 비어 있다.
func (reader *aiTaskReader) endedSessions(now time.Time) map[string]bool {
	ended := map[string]bool{}
	reader.alive = map[string]bool{}
	if reader.hooks == nil || reader.hooks.read() != nil {
		return ended
	}
	for _, session := range foldAIHookEvents(reader.hooks.events) {
		if session.Tool != "claude" {
			continue
		}
		settleAISession(session, now, aiProcessAlive, aiFileModified)
		switch {
		case session.State == aiStateEnded:
			ended[session.Session] = true
		case session.PID > 0 && session.Transcript != "":
			reader.alive[session.Transcript] = true
		}
	}
	return ended
}

type aiTaskFile struct {
	path string
	stat os.FileInfo
}

// sessionFiles는 aiTaskWindow 안에 기록이 늘어난 세션 기록이다. 새 기록은 프로젝트 디렉터리의 수정 시각으로 알아챈다.
func (reader *aiTaskReader) sessionFiles(claudeDir string, now time.Time) []aiTaskFile {
	if reader.projects == nil {
		reader.projects, reader.recent = map[string]*aiProjectDir{}, map[string]bool{}
	}
	root := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	sweep := now.Sub(reader.swept) >= aiTaskSweep
	if sweep {
		reader.swept = now
	}
	seen := map[string]bool{}
	var files []aiTaskFile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		seen[dir] = true
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		project := reader.projects[dir]
		if project == nil || !project.modified.Equal(info.ModTime()) {
			project = &aiProjectDir{modified: info.ModTime()}
			children, _ := os.ReadDir(dir)
			for _, child := range children {
				if !child.IsDir() && strings.HasSuffix(child.Name(), ".jsonl") {
					project.sessions = append(project.sessions, filepath.Join(dir, child.Name()))
				}
			}
			reader.projects[dir] = project
		}
		for _, path := range project.sessions {
			active, known := reader.recent[path]
			pinned := reader.pinned[path] || reader.alive[path]
			if !sweep && known && !active && !pinned {
				continue
			}
			// ReadDir과 stat 사이에 Claude Code가 세션을 지웠다.
			stat, err := os.Stat(path)
			active = err == nil && (pinned || now.Sub(stat.ModTime()) <= aiTaskWindow)
			reader.recent[path] = active
			if active {
				files = append(files, aiTaskFile{path, stat})
			}
		}
	}
	for dir, project := range reader.projects {
		if !seen[dir] {
			for _, path := range project.sessions {
				delete(reader.recent, path)
			}
			delete(reader.projects, dir)
		}
	}
	return files
}

// agentFiles는 sessions의 subagents 디렉터리에 있는 agent 기록이다. agent가 도는 동안 그 세션도 시작과 완료를 기록하므로,
// 창 밖의 세션 아래는 보지 않는다. 창 안의 세션 아래에서는 오래된 기록도 읽는다. agent는 끝났어도 그 agent가 띄운 shell이나
// monitor는 하루 넘게 돌 수 있다.
func agentFiles(sessions []aiTaskFile) []aiTaskFile {
	var files []aiTaskFile
	for _, session := range sessions {
		dir := filepath.Join(strings.TrimSuffix(session.path, ".jsonl"), "subagents")
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if stat, err := os.Stat(path); err == nil {
				files = append(files, aiTaskFile{path, stat})
			}
		}
	}
	return files
}

func aiTaskFiles(pattern string, now time.Time) []aiTaskFile {
	paths, _ := filepath.Glob(pattern)
	var files []aiTaskFile
	for _, path := range paths {
		// glob과 stat 사이에 Claude Code가 세션을 지웠다.
		stat, err := os.Stat(path)
		if err != nil || now.Sub(stat.ModTime()) > aiTaskWindow {
			continue
		}
		files = append(files, aiTaskFile{path, stat})
	}
	return files
}

// aiTaskGroup은 세션 하나와 그 세션에서 도는 agent와 shell이다.
type aiTaskGroup struct {
	session aiTask
	working bool
	agents  []aiTask
	shells  []aiTask
	// children은 agent ID별로 그 agent가 띄운 실행 중인 shell이다.
	children map[string][]aiTask
}

// list는 지금 도는 작업이다. 본 대화가 턴 중간이거나 agent나 shell이 도는 세션만 고르고, 세션마다 그 아래 agent와 shell을 둔다.
// shell은 시작 기록 뒤에 완료 알림이 없으면 돈다. agent는 마지막 응답이 도구 호출로 끝났거나 띄운 shell이 돌면 돈다.
// 완료 알림만으로는 agent를 판정하지 않는다. agent는 SendMessage로 다시 시작하고, 띄운 shell을 기다리며 멈춰도 알림이 온다.
func (reader *aiTaskReader) list(claudeDir string, now time.Time) ([]aiTask, error) {
	if _, err := os.Stat(filepath.Join(claudeDir, "projects")); err != nil {
		return nil, err
	}
	ended := reader.endedSessions(now)
	groups := map[string]*aiTaskGroup{}
	group := func(session string) *aiTaskGroup {
		if groups[session] == nil {
			groups[session] = &aiTaskGroup{session: aiTask{Kind: "session", ID: session, Session: session, State: aiTaskIdle}, children: map[string][]aiTask{}}
		}
		return groups[session]
	}
	// launched는 세션이나 agent 기록이 띄운 비동기 agent다. agent의 알림은 띄운 쪽 기록에 온다.
	launched := map[string]*aiBackground{}
	sessions := map[string]*aiTranscript{}
	sessionFiles := reader.sessionFiles(claudeDir, now)
	reader.prepare(sessionFiles)
	for _, file := range sessionFiles {
		id := strings.TrimSuffix(filepath.Base(file.path), ".jsonl")
		if ended[id] {
			continue
		}
		transcript := reader.read(file.path, file.stat)
		sessions[id] = transcript
		for _, task := range transcript.tasks {
			if task.kind == "agent" {
				launched[task.id] = task
			}
		}
		entry := group(id)
		entry.session.Cwd, entry.session.Started, entry.session.Last = transcript.cwd, transcript.started, transcript.modified
		entry.session.Transcript, entry.session.Step = file.path, transcript.step
		// AskUserQuestion의 답을 기다리는 본 대화는 일하는 중이 아니다. 사용자를 기다리는 세션은 edc ai sessions가 보인다.
		asking := transcript.step != nil && transcript.step.Kind == "tool" && transcript.step.Tool == "AskUserQuestion"
		entry.working = !transcript.stopped && !asking && now.Sub(transcript.modified) <= aiAgentStale
		for _, task := range runningAIShells(transcript, file.path, "", id, 1, entry.session.Cwd) {
			entry.shells = append(entry.shells, task)
		}
	}
	agentFiles := agentFiles(sessionFiles)
	reader.prepare(agentFiles)
	agents := map[string]*aiTranscript{}
	for _, file := range agentFiles {
		agent := aiAgentAt(file.path)
		if ended[agent.Session] {
			continue
		}
		transcript := reader.read(file.path, file.stat)
		agents[file.path] = transcript
		for _, task := range transcript.tasks {
			if task.kind == "agent" {
				launched[task.id] = task
			}
		}
	}
	// TaskStop은 다른 기록이 띄운 작업도 멈출 수 있다. 이번에 읽은 모든 기록의 결과를 모은다.
	halted := map[string]bool{}
	for _, transcript := range sessions {
		for id := range transcript.halted {
			halted[id] = true
		}
	}
	for _, transcript := range agents {
		for id := range transcript.halted {
			halted[id] = true
		}
	}
	for id, entry := range groups {
		kept := entry.shells[:0]
		for _, task := range entry.shells {
			if !halted[task.ID] {
				kept = append(kept, task)
			}
		}
		groups[id].shells = kept
	}
	for _, file := range agentFiles {
		transcript := agents[file.path]
		if transcript == nil {
			continue
		}
		agent := aiAgentAt(file.path)
		var shells []aiTask
		for _, task := range runningAIShells(transcript, file.path, agent.ID, agent.Session, 2, transcript.cwd) {
			if !halted[task.ID] {
				shells = append(shells, task)
			}
		}
		running := !transcript.stopped || len(shells) > 0
		// 강제로 멈춘 agent는 마지막 응답이 도구 호출이어도 killed 알림이 온다. 알림 뒤에 기록이 늘었으면 다시 시작한 것이다.
		if task := launched[agent.ID]; task != nil && (task.status == "killed" || task.status == "failed") && !transcript.modified.After(task.ended) && len(shells) == 0 {
			running = false
		}
		// 부모 세션의 본 대화처럼 턴 중간에 Claude Code가 끝나면 알림이 없다.
		if running && len(shells) == 0 && now.Sub(transcript.modified) > aiAgentStale && launched[agent.ID] == nil {
			running = false
		}
		// 창보다 오래 기록이 없는 agent는 띄운 작업이 돌 때만 보인다. 도구 호출에서 멈춘 채 남은 기록을 도는 것으로 보지 않는다.
		if running && len(shells) == 0 && now.Sub(transcript.modified) > aiTaskWindow {
			running = false
		}
		if !running {
			continue
		}
		agent.Kind, agent.Depth, agent.State = "agent", 1, aiTaskRunning
		agent.Cwd, agent.Started, agent.Last, agent.Step = transcript.cwd, transcript.started, transcript.modified, transcript.step
		agent.Type, agent.Description, agent.Model = transcript.meta.AgentType, transcript.meta.Description, transcript.meta.Model
		entry := group(agent.Session)
		if session := sessions[agent.Session]; session == nil && entry.session.Cwd == "" {
			entry.session.Cwd = agent.Cwd
		}
		entry.agents = append(entry.agents, agent)
		entry.children[agent.ID] = shells
	}
	// 도는 작업이 있는 세션 기록은 창 밖으로 밀려도 다음 읽기에서 계속 본다.
	pinned := map[string]bool{}
	for _, file := range sessionFiles {
		id := strings.TrimSuffix(filepath.Base(file.path), ".jsonl")
		if entry := groups[id]; entry != nil && (len(entry.agents) > 0 || len(entry.shells) > 0) {
			pinned[file.path] = true
		}
	}
	reader.pinned = pinned
	var ordered []*aiTaskGroup
	for _, entry := range groups {
		if entry.working || len(entry.agents) > 0 || len(entry.shells) > 0 {
			if entry.working {
				entry.session.State = aiTaskRunning
			}
			ordered = append(ordered, entry)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].session.Last.After(ordered[j].session.Last) })
	tasks := []aiTask{}
	for _, entry := range ordered {
		tasks = append(tasks, entry.session)
		sortAITasks(entry.agents)
		for _, agent := range entry.agents {
			tasks = append(tasks, agent)
			tasks = append(tasks, entry.children[agent.ID]...)
		}
		sortAITasks(entry.shells)
		tasks = append(tasks, entry.shells...)
	}
	return tasks, nil
}

// runningAIShells는 기록이 띄운 shell과 monitor 가운데 끝나지 않은 것이다. shell은 마지막 출력 줄을, monitor는 마지막 이벤트를
// 활동으로 붙인다. path는 띄운 기록이다. Transcript에 넣어, 따라갈 때 그 기록 하나에서 완료 알림을 찾는다.
// monitor는 완료 알림이나 만료 이벤트가 없어도 deadline이 지나면 끝났다고 본다.
func runningAIShells(transcript *aiTranscript, path, parent, session string, depth int, cwd string) []aiTask {
	var shells []aiTask
	for _, id := range transcript.order {
		task := transcript.tasks[id]
		if task.kind == "monitor" && task.status == "" && (task.deadline.IsZero() || time.Now().Before(task.deadline)) {
			monitor := aiTask{Kind: "monitor", ID: task.id, Session: session, Parent: parent, Depth: depth, Cwd: cwd, Description: task.description,
				State: aiTaskRunning, Started: task.started, Last: task.started, Transcript: path, Expires: task.deadline}
			if task.event != "" {
				monitor.Last, monitor.Step = task.eventAt, &aiAgentStep{At: task.eventAt, Kind: "event", Text: task.event}
			}
			shells = append(shells, monitor)
			continue
		}
		if task.kind != "shell" || task.status != "" {
			continue
		}
		if _, ended := aiShellEnd(task.output); ended {
			continue
		}
		shell := aiTask{Kind: "shell", ID: task.id, Session: session, Parent: parent, Depth: depth, Cwd: cwd, Description: task.description,
			State: aiTaskRunning, Started: task.started, Last: task.started, Transcript: path, Output: task.output}
		if stat, err := os.Stat(task.output); err == nil {
			shell.Last = stat.ModTime()
			if text := aiShellLastLine(task.output, stat.Size()); text != "" {
				shell.Step = &aiAgentStep{At: stat.ModTime(), Kind: "output", Text: text}
			}
		}
		shells = append(shells, shell)
	}
	return shells
}

func sortAITasks(tasks []aiTask) {
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Started.Before(tasks[j].Started) })
}

// aiShellLines는 shell 출력을 화면에 쓸 줄로 나눈다. 진행 막대가 \r로 덮어쓴 줄은 마지막 내용만 남기고, escape와 제어 문자는 지운다.
func aiShellLines(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if cut := strings.LastIndexByte(strings.TrimRight(line, "\r"), '\r'); cut >= 0 {
			line = line[cut+1:]
		}
		line = viewSanitize(strings.TrimRight(line, "\r"))
		if runes := []rune(line); len(runes) > aiShellLineLimit {
			line = string(runes[:aiShellLineLimit-1]) + "…"
		}
		lines = append(lines, line)
	}
	return lines
}

// aiShellEnd는 출력 끝의 종료 줄에서 얻은 상태다. 종료 코드가 0이면 completed, 아니면 failed다.
func aiShellEnd(path string) (aiTaskState, bool) {
	stat, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	data, _, err := aiShellTail(path, stat.Size(), 256)
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(string(data))
	match := aiShellEndLine.FindStringSubmatch(text[strings.LastIndexByte(text, '\n')+1:])
	switch {
	case match == nil:
		return "", false
	case match[2] != "":
		return aiTaskKilled, true
	case match[1] != "0":
		return aiTaskFailed, true
	}
	return "completed", true
}

// aiShellLastLine은 출력의 마지막 내용 있는 줄이다. 끝부분만 읽는다.
func aiShellLastLine(path string, size int64) string {
	data, _, err := aiShellTail(path, size, 4<<10)
	if err != nil {
		return ""
	}
	lines := aiShellLines(data)
	for index := len(lines) - 1; index >= 0; index-- {
		if text := strings.TrimSpace(lines[index]); text != "" {
			return aiHookClip(text, false)
		}
	}
	return ""
}

// aiShellTail은 출력의 끝 limit 바이트와 그 시작 위치다. 중간에서 시작하면 잘린 첫 줄을 버린다.
func aiShellTail(path string, size, limit int64) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	start := size - limit
	if start < 0 {
		start = 0
	}
	data := make([]byte, size-start)
	read, err := file.ReadAt(data, start)
	if err != nil && err != io.EOF {
		return nil, 0, err
	}
	data = data[:read]
	if start > 0 {
		if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
			data, start = data[cut+1:], start+int64(cut+1)
		}
	}
	return data, start, nil
}

// aiAgentAt은 경로에서 바로 알 수 있는 ID와 세션만 채운 agent다. 인자를 맞출 때 모든 기록을 읽지 않으려고 쓴다.
func aiAgentAt(path string) aiTask {
	return aiTask{
		Kind:       "agent",
		ID:         strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl"),
		Session:    filepath.Base(filepath.Dir(filepath.Dir(path))),
		Transcript: path,
	}
}

// findAITasks는 인자에 맞는 작업이다. 끝난 agent와 shell도 찾는다. Claude Code의 tasks/<ID>.output 경로, ID의 일부, 세션 ID의 앞부분을 받는다.
// agent는 경로만으로 맞춘다. shell은 aiTaskWindow 안의 세션과 agent 기록에서 시작 기록을 찾는다.
func findAITasks(reader *aiTaskReader, claudeDir, query string, now time.Time) []aiTask {
	var candidates []aiTask
	sessionPaths, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", "*.jsonl"))
	for _, path := range sessionPaths {
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		candidates = append(candidates, aiTask{Kind: "session", ID: id, Session: id, Transcript: path})
	}
	agentPaths, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", "*", "subagents", "agent-*.jsonl"))
	for _, path := range agentPaths {
		candidates = append(candidates, aiAgentAt(path))
	}
	// agent 기록은 목록처럼 창 안의 세션 아래 것을 모두 읽는다. 그래야 목록에 나온 오래된 agent의 shell도 찾는다.
	sessions := aiTaskFiles(filepath.Join(claudeDir, "projects", "*", "*.jsonl"), now)
	files := append([]aiTaskFile{}, sessions...)
	seen := map[string]bool{}
	for _, file := range append(agentFiles(sessions), aiTaskFiles(filepath.Join(claudeDir, "projects", "*", "*", "subagents", "agent-*.jsonl"), now)...) {
		if !seen[file.path] {
			seen[file.path] = true
			files = append(files, file)
		}
	}
	reader.prepare(files)
	// TaskStop은 다른 기록이 띄운 작업도 멈출 수 있다. 목록과 같이 모든 기록의 결과를 모은다.
	halted := map[string]time.Time{}
	for _, file := range files {
		for id, at := range reader.read(file.path, file.stat).halted {
			keepEarliestStop(halted, id, at)
		}
	}
	for _, file := range files {
		transcript := reader.read(file.path, file.stat)
		session := strings.TrimSuffix(filepath.Base(file.path), ".jsonl")
		parent := ""
		if filepath.Base(filepath.Dir(file.path)) == "subagents" {
			agent := aiAgentAt(file.path)
			session, parent = agent.Session, agent.ID
		}
		for _, id := range transcript.order {
			task := transcript.tasks[id]
			if task.kind != "shell" && task.kind != "monitor" {
				continue
			}
			state := aiTaskRunning
			end := aiTaskEnd{}
			if task.kind == "monitor" {
				end.deadline = task.deadline
			}
			if task.status != "" {
				end.offer(task.ended, task.status)
			}
			if at, ok := halted[task.id]; ok {
				end.offer(at, string(aiTaskKilled))
			}
			if !end.deadline.IsZero() && !now.Before(end.deadline) {
				end.offer(end.deadline, string(aiTaskExpired))
			}
			if ended, ok := aiShellEnd(task.output); task.kind == "shell" && ok {
				state = ended
			} else if !end.at.IsZero() {
				state = aiTaskState(end.state)
			}
			candidates = append(candidates, aiTask{Kind: task.kind, ID: task.id, Session: session, Parent: parent, Cwd: transcript.cwd,
				Description: task.description, State: state, Started: task.started, Transcript: file.path, Output: task.output,
				Expires: task.deadline, Summary: task.summary})
		}
	}
	return matchAITasks(candidates, query)
}

// matchAITasks는 인자에 맞는 후보다. ID나 경로가 정확히 맞으면 그것만, 아니면 ID의 일부나 세션 ID의 앞부분이 맞는 것을 모두 돌려준다.
func matchAITasks(candidates []aiTask, query string) []aiTask {
	if resolved, err := filepath.EvalSymlinks(query); err == nil {
		for _, task := range candidates {
			// shell의 Transcript는 띄운 기록이라 세션 경로를 shell로 맞추지 않는다.
			if task.Kind != "shell" && task.Transcript == resolved || task.Output == resolved || task.Output == query {
				return []aiTask{task}
			}
		}
	}
	key := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(query), ".output"), ".jsonl")
	key = strings.TrimPrefix(key, "agent-")
	if key == "" {
		return nil
	}
	var matches []aiTask
	for _, task := range candidates {
		if task.ID == key {
			return []aiTask{task}
		}
		if strings.Contains(task.ID, key) || task.Kind != "session" && strings.HasPrefix(task.Session, key) {
			matches = append(matches, task)
		}
	}
	return matches
}

func runAIAgents(args []string) int {
	set := newFlagSet("ai agents")
	set.SetOutput(os.Stderr)
	var follow bool
	set.BoolVar(&follow, "follow", false, T("command.ai.option.follow"))
	set.BoolVar(&follow, "f", false, T("command.ai.option.follow"))
	count := set.Int("count", 0, T("command.ai.option.sessions_count"))
	jsonPath := set.String("json", configuredStringFallback(nil, activeConfig.Defaults.Common.JSON, ""), T("option.json"))
	positional, err := parseInterspersed(set, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintln(os.Stderr, T("observe.ai.agents.usage"))
		return 2
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, T("observe.ai.count_minimum"))
		return 2
	}
	if follow && len(positional) == 0 {
		fmt.Fprintln(os.Stderr, T("observe.ai.agents.follow_needs_id"))
		return 2
	}
	home, _ := os.UserHomeDir()
	claudeDir := aiClaudeDir(home)
	if len(positional) == 0 && *jsonPath == "" && *count == 0 && liveTerminal() {
		return runAIAgentsDashboard(claudeDir)
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
	if len(positional) == 1 {
		return runAIAgentLog(writer, claudeDir, positional[0], follow, *jsonPath != "")
	}
	reader := newAITaskReader()
	for sample := range max(*count, 1) {
		if sample > 0 {
			time.Sleep(aiScanInterval)
		}
		now := time.Now()
		tasks, err := reader.list(claudeDir, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if *jsonPath != "" {
			// 표본이 여럿이면 한 줄에 하나씩 쓴다. 다른 도구가 줄 단위로 읽는다.
			encoder := json.NewEncoder(writer)
			if *count <= 1 {
				encoder.SetIndent("", "  ")
			}
			if err := encoder.Encode(tasks); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			continue
		}
		if sample > 0 {
			fmt.Fprintln(writer)
		}
		writeAITasks(writer, tasks, now)
	}
	return 0
}

// aiTaskLog는 작업 하나의 활동을 이어 읽는다. 세션과 agent는 대화 기록을, shell은 출력 파일을 읽는다.
// 세션 기록은 수십 MB라 끝부분부터 읽는다.
type aiTaskLog struct {
	task   aiTask
	parser *aiAgentParser
	offset int64
	// pending은 shell 출력에서 아직 줄바꿈이 오지 않은 끝부분이다.
	pending []byte
	// queued는 monitor 활동에서 queue-operation 사본을 본 적이 있는지다. 그 뒤로는 user 사본을 건너뛴다.
	queued  bool
	stopped bool
	done    bool
}

func newAITaskLog(task aiTask) *aiTaskLog {
	log := &aiTaskLog{task: task, parser: newAIAgentParser()}
	switch task.Kind {
	case "session":
		if stat, err := os.Stat(task.Transcript); err == nil {
			log.offset = aiLineStart(task.Transcript, stat.Size()-aiActivityTail)
		}
	case "shell":
		if stat, err := os.Stat(task.Output); err == nil {
			if _, start, err := aiShellTail(task.Output, stat.Size(), aiTaskOutputTail); err == nil {
				log.offset = start
			}
		}
	}
	return log
}

// read는 새 활동이다. shell은 완료 알림을 보면 마지막에 상태 줄을 붙이고 더 읽지 않는다.
func (log *aiTaskLog) read(reader *aiTaskReader, claudeDir string, now time.Time) ([]aiAgentStep, error) {
	var steps []aiAgentStep
	if log.done {
		return nil, nil
	}
	if log.task.Kind == "monitor" {
		return log.readMonitor(reader, claudeDir, now)
	}
	if log.task.Kind != "shell" {
		stopped := log.stopped
		offset, err := readAIAgentLines(log.task.Transcript, log.offset, log.parser, func(step aiAgentStep) { steps = append(steps, step) })
		log.offset, log.stopped = offset, log.parser.stopped
		if err == nil && log.parser.stopped && (!stopped || len(steps) > 0) {
			steps = append(steps, aiAgentStep{At: log.parser.last, Kind: "stopped"})
		}
		return steps, err
	}
	steps = append(steps, log.readOutput(now)...)
	// 끝난 shell은 출력 끝에 종료 줄이 붙고, 완료 알림은 shell을 띄운 기록에 온다. 그 뒤에는 출력이 더 늘지 않는다.
	if state, summary, ended := log.shellEnd(reader, claudeDir, now); ended {
		if len(log.pending) > 0 {
			steps = append(steps, aiAgentStep{At: now, Kind: "output", Text: aiShellLines(log.pending)[0]})
			log.pending = nil
		}
		log.done = true
		steps = append(steps, aiAgentStep{At: now, Kind: "stopped", Text: strings.TrimSpace(string(state) + " " + summary)})
	}
	return steps, nil
}

// haltedTasks는 aiTaskWindow 안의 세션과 agent 기록에서 TaskStop으로 멈춘 작업 ID와 그 시각이다. 실시간 화면은 목록을 읽을 때
// 이미 색인한 기록을 그대로 쓰므로 거의 비용이 없다.
func (reader *aiTaskReader) haltedTasks(claudeDir string, now time.Time) map[string]time.Time {
	sessions := reader.sessionFiles(claudeDir, now)
	files := append(sessions, agentFiles(sessions)...)
	reader.prepare(files)
	halted := map[string]time.Time{}
	for _, file := range files {
		for id, at := range reader.read(file.path, file.stat).halted {
			keepEarliestStop(halted, id, at)
		}
	}
	return halted
}

// keepEarliestStop은 id의 TaskStop 시각을 halted에 남긴다. 같은 작업을 여러 번 멈췄으면 가장 이른 시각이 끝난 때다.
// 기록을 읽는 순서는 파일 이름 순이라, 나중에 읽은 늦은 TaskStop이 앞선 TaskStop을 덮으면 만료로 잘못 판정한다.
func keepEarliestStop(halted map[string]time.Time, id string, at time.Time) {
	if previous, ok := halted[id]; !ok || at.Before(previous) {
		halted[id] = at
	}
}

// readMonitor는 monitor를 띄운 기록에서 그 monitor의 이벤트를 읽는다. 같은 알림이 queue-operation 줄과 user 줄에 겹쳐 오므로
// queue-operation 사본을 본 기록에서는 user 사본을 건너뛴다. 두 사본의 시각은 몇 초씩 달라 시각으로는 가를 수 없다.
// 끝 알림, 만료 이벤트, TaskStop, deadline 가운데 먼저 오는 것에서 멈춘다. 다른 기록의 TaskStop도 본다.
func (log *aiTaskLog) readMonitor(reader *aiTaskReader, claudeDir string, now time.Time) ([]aiAgentStep, error) {
	var steps []aiAgentStep
	if log.done {
		return nil, nil
	}
	end := aiTaskEnd{deadline: log.task.Expires}
	id := []byte(log.task.ID)
	offset, err := readAITranscriptLines(log.task.Transcript, log.offset, func(line []byte) {
		if !bytes.Contains(line, id) {
			return
		}
		if bytes.Contains(line, []byte(`"task_type"`)) {
			stopped := &aiTranscript{tasks: map[string]*aiBackground{}}
			stopped.indexStopped(line)
			if at, ok := stopped.halted[log.task.ID]; ok {
				end.offer(at, string(aiTaskKilled))
			}
			return
		}
		for _, notification := range aiNotifications(line) {
			fields := notification.fields
			if fields["task-id"] != log.task.ID {
				continue
			}
			if notification.source == "queue-operation" {
				log.queued = true
			} else if log.queued {
				continue
			}
			switch {
			case fields["status"] != "":
				end.offer(notification.at, strings.TrimSpace(fields["status"]+" "+fields["summary"]))
			case fields["event"] != "":
				steps = append(steps, aiAgentStep{At: notification.at, Kind: "event", Text: aiHookClip(fields["event"], false)})
				if aiMonitorExpired(fields["event"]) {
					end.offer(notification.at, string(aiTaskExpired))
				}
			}
		}
	})
	log.offset = offset
	if reader != nil {
		if at, ok := reader.haltedTasks(claudeDir, now)[log.task.ID]; ok {
			end.offer(at, string(aiTaskKilled))
		}
	}
	// 정확히 만료 시각부터 끝난 것으로 본다. 목록도 그 시각부터 monitor를 뺀다.
	if !log.task.Expires.IsZero() && !now.Before(log.task.Expires) {
		end.offer(log.task.Expires, string(aiTaskExpired))
	}
	if !end.at.IsZero() {
		// 끝 시각보다 늦게 기록된 이벤트는 끝난 뒤의 것이라 쓰지 않는다.
		kept := steps[:0]
		for _, step := range steps {
			if !step.At.After(end.at) {
				kept = append(kept, step)
			}
		}
		log.done = true
		steps = append(kept, aiAgentStep{At: end.at, Kind: "stopped", Text: end.state})
	}
	return steps, err
}

// aiTaskEnd는 작업이 끝났다는 후보 가운데 가장 이른 것이다. 끝 알림, 만료 이벤트, 여러 기록의 TaskStop, monitor의 만료 시각이
// 서로 다른 순서로 기록되므로, 읽은 순서가 아니라 시각으로 고른다. 만료 시각보다 늦은 끝은 만료 시각의 만료로 바꾼다.
type aiTaskEnd struct {
	deadline time.Time
	at       time.Time
	state    string
}

func (end *aiTaskEnd) offer(at time.Time, state string) {
	if !end.deadline.IsZero() && at.After(end.deadline) {
		at, state = end.deadline, string(aiTaskExpired)
	}
	if end.at.IsZero() || at.Before(end.at) {
		end.at, end.state = at, state
	}
}

// readOutput은 shell 출력에서 새로 끝난 줄이다. 한 번에 aiTaskOutputTail까지만 읽는다. 그보다 많이 늘었으면 앞부분을 건너뛰고
// 건너뛴 크기를 한 줄로 알린다. 줄바꿈 없이 aiTaskOutputTail을 넘게 쌓인 출력은 한 줄로 내보내 pending이 커지지 않게 한다.
func (log *aiTaskLog) readOutput(now time.Time) []aiAgentStep {
	stat, err := os.Stat(log.task.Output)
	if err != nil {
		return nil
	}
	size := stat.Size()
	// shell이 출력 파일을 다시 썼다.
	if size < log.offset {
		log.offset, log.pending = 0, nil
	}
	var steps []aiAgentStep
	if grown := size - log.offset; grown > aiTaskOutputTail {
		start := aiLineStart(log.task.Output, size-aiTaskOutputTail)
		skipped := start - log.offset + int64(len(log.pending))
		steps = append(steps, aiAgentStep{At: now, Kind: "output", Text: fmt.Sprintf("… %d bytes skipped", skipped)})
		log.offset, log.pending = start, nil
	}
	file, err := os.Open(log.task.Output)
	if err != nil {
		return steps
	}
	data := make([]byte, size-log.offset)
	read, _ := file.ReadAt(data, log.offset)
	file.Close()
	log.offset += int64(read)
	data = append(log.pending, data[:read]...)
	cut := bytes.LastIndexByte(data, '\n')
	log.pending = append([]byte(nil), data[cut+1:]...)
	if cut >= 0 {
		for _, line := range aiShellLines(data[:cut]) {
			steps = append(steps, aiAgentStep{At: now, Kind: "output", Text: line})
		}
	}
	if int64(len(log.pending)) > aiTaskOutputTail {
		steps = append(steps, aiAgentStep{At: now, Kind: "output", Text: aiShellLines(log.pending)[0]})
		log.pending = nil
	}
	return steps
}

// shellEnd는 따라가는 shell이 끝났는지다. 출력 끝의 종료 줄과, shell을 띄운 기록 하나의 완료 알림을 본다.
// 띄운 기록을 모를 때만 ID로 찾는다. 그 검색은 모든 기록을 glob해 1초마다 하기에는 무겁다.
func (log *aiTaskLog) shellEnd(reader *aiTaskReader, claudeDir string, now time.Time) (aiTaskState, string, bool) {
	ended, ok := aiShellEnd(log.task.Output)
	if log.task.Transcript == "" {
		for _, task := range findAITasks(reader, claudeDir, log.task.ID, now) {
			if task.Kind == "shell" && task.ID == log.task.ID && task.State != aiTaskRunning {
				return task.State, task.Summary, true
			}
		}
		return ended, "", ok
	}
	var task *aiBackground
	if stat, err := os.Stat(log.task.Transcript); err == nil {
		task = reader.read(log.task.Transcript, stat).tasks[log.task.ID]
	}
	switch {
	case ok:
		summary := ""
		if task != nil {
			summary = task.summary
		}
		return ended, summary, true
	case task != nil && task.status != "":
		return aiTaskState(task.status), task.summary, true
	}
	return "", "", false
}

// runAIAgentLog는 작업 하나의 활동을 쓴다. follow이면 tail -f처럼 새 줄을 기다린다. shell은 끝나면 상태 줄을 쓰고 마친다.
// agent와 세션은 멈췄다 다시 시작할 수 있어 끝내지 않는다. JSON이면 활동 하나를 한 줄로 쓴다.
func runAIAgentLog(writer io.Writer, claudeDir, query string, follow, jsonOutput bool) int {
	now := time.Now()
	reader := newAITaskReader()
	matches := findAITasks(reader, claudeDir, query, now)
	switch len(matches) {
	case 0:
		fmt.Fprintln(os.Stderr, T("observe.ai.agents.not_found", query, filepath.Join(claudeDir, "projects")))
		return 1
	case 1:
	default:
		fmt.Fprintln(os.Stderr, T("observe.ai.agents.ambiguous", query, len(matches)))
		for _, task := range matches[:min(len(matches), aiAgentCandidates)] {
			fmt.Fprintln(os.Stderr, "  "+aiTaskMarks[task.Kind]+" "+task.ID+"  "+task.Description)
		}
		if len(matches) > aiAgentCandidates {
			fmt.Fprintln(os.Stderr, "…")
		}
		return 2
	}
	task := matches[0]
	if task.Kind == "agent" {
		if stat, err := os.Stat(task.Transcript); err == nil {
			transcript := reader.read(task.Transcript, stat)
			task.Type, task.Description, task.Cwd = transcript.meta.AgentType, transcript.meta.Description, transcript.cwd
		}
	}
	encoder := json.NewEncoder(writer)
	emit := func(step aiAgentStep) error {
		if jsonOutput {
			return encoder.Encode(step)
		}
		fmt.Fprintln(writer, aiAgentStepLine(step))
		return nil
	}
	if !jsonOutput {
		fmt.Fprintln(writer, aiTaskTitle(task))
	}
	log := newAITaskLog(task)
	for {
		steps, err := log.read(reader, claudeDir, time.Now())
		for _, step := range steps {
			if err := emit(step); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !follow || log.done {
			return 0
		}
		time.Sleep(aiAgentFollowInterval)
	}
}

func aiTaskStateText(state aiTaskState) string {
	return T("observe.ai.agents.state." + string(state))
}

func writeAITasks(writer io.Writer, tasks []aiTask, now time.Time) {
	if len(tasks) == 0 {
		fmt.Fprintln(writer, T("observe.ai.agents.none"))
		return
	}
	for _, task := range tasks {
		fmt.Fprintln(writer, aiTaskLine(task, now))
	}
}

// attachAITasks는 Claude 세션마다 그 세션에서 도는 agent와 shell을 붙인다. agent가 띄운 shell은 그 agent 바로 뒤에 온다.
func attachAITasks(sessions []aiSession, tasks []aiTask) {
	children := map[string][]aiTask{}
	for _, task := range tasks {
		if task.Kind != "session" {
			children[task.Session] = append(children[task.Session], task)
		}
	}
	changed := false
	for index := range sessions {
		session := &sessions[index]
		if session.Tool != "claude" {
			continue
		}
		session.Tasks = children[session.Session]
		// 본 대화가 쉬어도 띄운 작업이 돌면 세션은 일하는 중이다. 입력 필요와 실패는 사용자가 봐야 하므로 그대로 둔다.
		if len(session.Tasks) > 0 && session.State == aiStateWaiting {
			session.State, session.Reason, changed = aiStateWorking, "background", true
		}
	}
	if changed {
		sortAISessions(sessions)
	}
}

// aiChildRow는 세션 줄 아래에 들여 쓴 agent나 shell 한 줄이다. width가 0이면 자르지 않는다.
// 시간은 시작한 뒤 지난 시간이고, 설명 뒤에 마지막 활동이나 마지막 출력 줄이 온다.
func aiChildRow(task aiTask, now time.Time, width int, color bool) string {
	tree := "   " + strings.Repeat("   ", max(0, task.Depth-1)) + "└ "
	kind := task.Type
	if task.Kind == "shell" || kind == "" {
		kind = task.Kind
	}
	head := aiTaskMarks[task.Kind] + " " + padRight(truncateName(kind, 14), 14) + " " + padRight(task.ID[:min(len(task.ID), aiAgentIDWidth)], aiAgentIDWidth) +
		" " + padLeft(formatTopEventDuration(now.Sub(task.Started)), 6)
	// 설명 칸은 목록과 같다. monitor는 남은 시간이 붙는다. 활동은 칠하려고 따로 붙인다.
	plain := task
	plain.Step = nil
	detail := aiTaskDetail(plain)
	step := ""
	if task.Step != nil {
		step = aiAgentStepText(*task.Step)
	}
	if !color {
		return strings.TrimRight(tree+head+" "+strings.TrimSpace(detail+"  "+step), " ")
	}
	text := strings.TrimSpace(detail + "  " + step)
	if width > 0 {
		text = ansi.Truncate(text, max(0, width-liveWidth(tree+head)-1), "…")
	}
	// 자른 뒤에 칠한다. 칠한 글자를 자르면 활동의 색이 잘린 자리에서 끝나지 않는다.
	if task.Step != nil && strings.HasSuffix(strings.TrimSuffix(text, "…"), step) && step != "" {
		text = strings.TrimSuffix(text, step) + aiPaintStep(*task.Step)
	}
	return richDim(tree) + richPaint(head, richCyan, false) + " " + text
}

// aiTaskHead는 목록 한 줄의 앞부분이다. 세션 줄은 프로젝트와 본 대화 상태를, 그 아래 줄은 들여 써서 종류와 ID를 보인다.
// 시간은 agent와 shell이 시작한 뒤, 세션은 마지막으로 기록이 늘어난 뒤 지난 시간이다.
func aiTaskHead(task aiTask, now time.Time) string {
	if task.Kind == "session" {
		project := filepath.Base(task.Cwd)
		if task.Cwd == "" {
			project = "-"
		}
		return strings.Join([]string{aiTaskMarks[task.Kind], padRight(truncateName(project, 18), 18), padRight(task.ID[:min(len(task.ID), 8)], 8),
			padRight(aiTaskStateText(task.State), 9), padLeft(formatTopEventDuration(now.Sub(task.Last)), 6)}, " ")
	}
	kind := task.Type
	if task.Kind == "shell" || task.Kind == "monitor" {
		kind = task.Kind
	}
	if kind == "" {
		kind = "-"
	}
	indent := strings.Repeat("  ", task.Depth)
	head := indent + aiTaskMarks[task.Kind] + " " + padRight(truncateName(kind, 16-len(indent)), 16-len(indent)) + " " + padRight(task.ID[:min(len(task.ID), aiAgentIDWidth)], aiAgentIDWidth)
	return head + " " + padLeft(formatTopEventDuration(now.Sub(task.Started)), 6)
}

// aiTaskDetail은 작업 설명과 마지막 활동이다. monitor는 스스로 끝날 때까지 남은 시간을 붙인다.
func aiTaskDetail(task aiTask) string {
	var parts []string
	if task.Description != "" {
		parts = append(parts, task.Description)
	}
	if task.Kind == "monitor" && !task.Expires.IsZero() {
		left := time.Until(task.Expires)
		if left < 0 {
			left = 0
		}
		parts = append(parts, formatTopEventDuration(left)+" left")
	}
	if task.Step != nil {
		parts = append(parts, aiAgentStepText(*task.Step))
	}
	// 활동 앞의 기호가 구분자 역할을 한다. " · "로 이으면 text 활동의 "·"와 겹친다.
	return strings.Join(parts, "  ")
}

func aiTaskLine(task aiTask, now time.Time) string {
	return strings.TrimRight(aiTaskHead(task, now)+" "+aiTaskDetail(task), " ")
}

func aiTaskTitle(task aiTask) string {
	parts := []string{task.Kind + " " + task.ID}
	for _, value := range []string{task.Type, task.Description, task.Cwd} {
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

// aiAgentStepText는 시각을 뺀 활동 한 줄이다. 기호로 종류를 가른다.
func aiAgentStepText(step aiAgentStep) string {
	switch step.Kind {
	case "prompt":
		return "▶ " + step.Text
	case "text":
		return "· " + step.Text
	case "failed":
		return strings.TrimRight("✗ "+step.Tool+" "+step.Text, " ")
	case "output":
		return "│ " + step.Text
	case "event":
		return "◷ " + step.Text
	case "stopped":
		if step.Text != "" {
			return "■ " + step.Text
		}
		return "■ " + aiTaskStateText(aiTaskStopped)
	}
	return strings.TrimRight(step.Tool+" "+step.Text, " ")
}

func aiAgentStepLine(step aiAgentStep) string {
	return step.At.Local().Format("15:04:05") + "  " + aiAgentStepText(step)
}
