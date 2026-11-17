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
	"sort"
	"strings"
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
)

// aiTaskMarks는 색 없이도 종류를 가르는 기호다.
var aiTaskMarks = map[string]string{"session": "●", "agent": "◆", "shell": "$"}

// aiTask는 지금 도는 Claude Code 작업 하나다. Kind는 session(본 대화), agent(subagent), shell(run_in_background로 띄운 Bash)이다.
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
		parser.stopped = false
		return nil
	case "user", "assistant":
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
	stopped  bool
	cwd      string
	started  time.Time
	step     *aiAgentStep
	meta     aiAgentMeta
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
	aiNotificationTag = regexp.MustCompile(`(?s)<(task-id|status|summary)>(.*?)</(?:task-id|status|summary)>`)
)

// index는 지난번 뒤에 끝난 줄에서 백그라운드 작업의 시작과 완료 알림을 찾는다. 대부분의 줄은 문자열 검사만으로 건너뛴다.
func (transcript *aiTranscript) index(path string) {
	transcript.offset, _ = readAITranscriptLines(path, transcript.offset, func(line []byte) {
		// api-request, api-request-blob, api-request-shape 줄은 API에 보낸 요청의 사본이다. 대화의 표시 문구를 다시 담고 있어도
		// 활동이 아니다. 처음 읽을 때 바이트의 3분의 1 가까이를 검색 없이 건너뛴다.
		if bytes.HasPrefix(line, []byte(`{"type":"api-request`)) {
			return
		}
		switch {
		case bytes.Contains(line, []byte("ackground")):
			// run_in_background와 backgroundTaskId는 이 글자를 함께 가진다. 한 번 찾아 대부분의 줄을 거른다.
			// agent 시작 결과도 "working in the background"라는 안내를 담아 이 분기로 온다.
			if bytes.Contains(line, []byte(`"run_in_background":true`)) {
				transcript.indexLaunch(line)
			} else if bytes.Contains(line, []byte(`"backgroundTaskId"`)) || bytes.Contains(line, []byte(`"async_launched"`)) {
				transcript.indexStarted(line)
			}
			if bytes.Contains(line, []byte("<task-notification>")) {
				transcript.indexNotification(line)
			}
		case bytes.Contains(line, []byte(`"async_launched"`)):
			transcript.indexStarted(line)
		case bytes.Contains(line, []byte("<task-notification>")):
			transcript.indexNotification(line)
		}
	})
}

func (transcript *aiTranscript) indexLaunch(line []byte) {
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
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
		if part.Type == "tool_use" && part.Input.Background {
			description := part.Input.Description
			if description == "" {
				description = part.Input.Command
			}
			transcript.launches[part.ID] = aiHookClip(description, false)
		}
	}
}

// indexStarted는 백그라운드 작업이 시작됐다는 도구 결과다. Bash는 toolUseResult.backgroundTaskId, agent는 status async_launched를 남긴다.
func (transcript *aiTranscript) indexStarted(line []byte) {
	var entry struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Result    struct {
			BackgroundTaskID string `json:"backgroundTaskId"`
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
		return
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
	case entry.Result.Status == "async_launched" && entry.Result.AgentID != "":
		task.kind, task.id, task.description, task.output = "agent", entry.Result.AgentID, entry.Result.Description, entry.Result.OutputFile
	default:
		return
	}
	if transcript.tasks[task.id] == nil {
		transcript.order = append(transcript.order, task.id)
	}
	transcript.tasks[task.id] = task
}

// indexNotification은 완료 알림을 이 기록이 띄운 작업에 붙인다. 알림은 queue-operation 줄과 user 줄에 함께 오고,
// user 줄은 안내 문구 뒤에 붙어 온다. 대화에 인용한 알림이 섞여도 이 기록이 띄운 작업 ID에만 붙인다.
func (transcript *aiTranscript) indexNotification(line []byte) {
	var entry struct {
		Type      string          `json:"type"`
		Timestamp time.Time       `json:"timestamp"`
		Content   string          `json:"content"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return
	}
	text := entry.Content
	if entry.Type == "user" {
		var message struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(entry.Message, &message) != nil {
			return
		}
		text = message.Content
	} else if entry.Type != "queue-operation" {
		return
	}
	for _, block := range aiTaskNotification.FindAllString(text, -1) {
		fields := map[string]string{}
		for _, match := range aiNotificationTag.FindAllStringSubmatch(block, -1) {
			fields[match[1]] = strings.TrimSpace(match[2])
		}
		if task := transcript.tasks[fields["task-id"]]; task != nil && fields["status"] != "" {
			task.status, task.ended, task.summary = fields["status"], entry.Timestamp, fields["summary"]
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
	var data []byte
	for window := aiActivityTail; ; window *= 4 {
		start := transcript.size - window
		if start < 0 {
			start = 0
		}
		data = make([]byte, transcript.size-start)
		if _, err := file.ReadAt(data, start); err != nil && err != io.EOF {
			return
		}
		if start > 0 {
			// 중간에서 시작한 첫 줄은 잘린 JSON이다.
			if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
				data = data[cut+1:]
			}
		}
		// 창 안에 끝난 줄이 하나도 없으면 마지막 줄이 창보다 길다. 넓혀 읽어야 그 줄의 stop_reason을 본다.
		if start == 0 || bytes.IndexByte(data, '\n') >= 0 || window >= aiTranscriptTailLimit {
			break
		}
	}
	parser := newAIAgentParser()
	var last *aiAgentStep
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		// 줄바꿈이 없는 마지막 줄은 아직 쓰는 중이다.
		if !bytes.HasSuffix(line, []byte{'\n'}) {
			break
		}
		for _, step := range parser.line(line) {
			last = &step
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
	transcript := reader.transcripts[path]
	if transcript == nil || stat.Size() < transcript.offset {
		transcript = &aiTranscript{launches: map[string]string{}, tasks: map[string]*aiBackground{}}
		if data, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json"); err == nil {
			_ = json.Unmarshal(data, &transcript.meta)
		}
		reader.transcripts[path] = transcript
	}
	if transcript.size != stat.Size() || !transcript.modified.Equal(stat.ModTime()) {
		transcript.size, transcript.modified = stat.Size(), stat.ModTime()
		transcript.index(path)
		transcript.tail(path)
	}
	return transcript
}

// endedSessions는 hook 이벤트로 끝났다고 아는 Claude 세션이다. hook이 없으면 비어 있다.
func (reader *aiTaskReader) endedSessions(now time.Time) map[string]bool {
	ended := map[string]bool{}
	if reader.hooks == nil || reader.hooks.read() != nil {
		return ended
	}
	for _, session := range foldAIHookEvents(reader.hooks.events) {
		if session.Tool != "claude" {
			continue
		}
		settleAISession(session, now, aiProcessAlive, aiFileModified)
		if session.State == aiStateEnded {
			ended[session.Session] = true
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
			if !sweep && known && !active {
				continue
			}
			// ReadDir과 stat 사이에 Claude Code가 세션을 지웠다.
			stat, err := os.Stat(path)
			active = err == nil && now.Sub(stat.ModTime()) <= aiTaskWindow
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

// agentFiles는 sessions의 subagents 디렉터리에서 aiTaskWindow 안에 기록이 늘어난 agent 기록이다.
// agent가 도는 동안 그 세션도 시작과 완료를 기록하므로, 창 밖의 세션 아래는 보지 않는다.
func agentFiles(sessions []aiTaskFile, now time.Time) []aiTaskFile {
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
			if stat, err := os.Stat(path); err == nil && now.Sub(stat.ModTime()) <= aiTaskWindow {
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
	agentFiles := agentFiles(sessionFiles, now)
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
	for _, file := range agentFiles {
		transcript := agents[file.path]
		if transcript == nil {
			continue
		}
		agent := aiAgentAt(file.path)
		shells := runningAIShells(transcript, file.path, agent.ID, agent.Session, 2, transcript.cwd)
		running := !transcript.stopped || len(shells) > 0
		// 강제로 멈춘 agent는 마지막 응답이 도구 호출이어도 killed 알림이 온다. 알림 뒤에 기록이 늘었으면 다시 시작한 것이다.
		if task := launched[agent.ID]; task != nil && (task.status == "killed" || task.status == "failed") && !transcript.modified.After(task.ended) && len(shells) == 0 {
			running = false
		}
		// 부모 세션의 본 대화처럼 턴 중간에 Claude Code가 끝나면 알림이 없다.
		if running && len(shells) == 0 && now.Sub(transcript.modified) > aiAgentStale && launched[agent.ID] == nil {
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

// runningAIShells는 기록이 띄운 shell 가운데 완료 알림이 아직 없는 것이다. 마지막 출력 줄을 활동으로 붙인다.
// path는 shell을 띄운 기록이다. shell의 Transcript에 넣어, 따라갈 때 그 기록 하나에서 완료 알림을 찾는다.
func runningAIShells(transcript *aiTranscript, path, parent, session string, depth int, cwd string) []aiTask {
	var shells []aiTask
	for _, id := range transcript.order {
		task := transcript.tasks[id]
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
	for _, file := range append(aiTaskFiles(filepath.Join(claudeDir, "projects", "*", "*.jsonl"), now),
		aiTaskFiles(filepath.Join(claudeDir, "projects", "*", "*", "subagents", "agent-*.jsonl"), now)...) {
		transcript := reader.read(file.path, file.stat)
		session := strings.TrimSuffix(filepath.Base(file.path), ".jsonl")
		parent := ""
		if filepath.Base(filepath.Dir(file.path)) == "subagents" {
			agent := aiAgentAt(file.path)
			session, parent = agent.Session, agent.ID
		}
		for _, id := range transcript.order {
			task := transcript.tasks[id]
			if task.kind != "shell" {
				continue
			}
			state := aiTaskRunning
			if ended, ok := aiShellEnd(task.output); ok {
				state = ended
			} else if task.status != "" {
				state = aiTaskState(task.status)
			}
			candidates = append(candidates, aiTask{Kind: "shell", ID: task.id, Session: session, Parent: parent, Cwd: transcript.cwd,
				Description: task.description, State: state, Started: task.started, Transcript: file.path, Output: task.output, Summary: task.summary})
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
	emit := func(step aiAgentStep) {
		if jsonOutput {
			_ = encoder.Encode(step)
			return
		}
		fmt.Fprintln(writer, aiAgentStepLine(step))
	}
	if !jsonOutput {
		fmt.Fprintln(writer, aiTaskTitle(task))
	}
	log := newAITaskLog(task)
	for {
		steps, err := log.read(reader, claudeDir, time.Now())
		for _, step := range steps {
			emit(step)
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
	detail := task.Description
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
	if task.Kind == "shell" {
		kind = "shell"
	}
	if kind == "" {
		kind = "-"
	}
	indent := strings.Repeat("  ", task.Depth)
	head := indent + aiTaskMarks[task.Kind] + " " + padRight(truncateName(kind, 16-len(indent)), 16-len(indent)) + " " + padRight(task.ID[:min(len(task.ID), aiAgentIDWidth)], aiAgentIDWidth)
	return head + " " + padLeft(formatTopEventDuration(now.Sub(task.Started)), 6)
}

// aiTaskDetail은 작업 설명과 마지막 활동이다.
func aiTaskDetail(task aiTask) string {
	var parts []string
	if task.Description != "" {
		parts = append(parts, task.Description)
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
