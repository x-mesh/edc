package edc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// aiActivityTail은 활동을 찾으려고 읽는 대화 기록의 끝부분이다. 한 턴의 마지막 몇 단계가 들어가고,
// 파일 크기와 관계없이 2초마다 읽어도 부담이 작다.
const aiActivityTail int64 = 256 << 10

// aiActivity는 작업 중인 세션이 마지막으로 한 일이다. Running이면 결과가 아직 없는 도구 호출이다.
type aiActivity struct {
	Kind    string    `json:"kind"`
	Text    string    `json:"text,omitempty"`
	At      time.Time `json:"at"`
	Running bool      `json:"running,omitempty"`
}

// aiCodexCommand는 Codex exec 도구가 넘기는 JS 안의 셸 명령이다. Codex 0.165는 명령을 tools.exec_command 호출로 감싼다.
var aiCodexCommand = regexp.MustCompile(`exec_command\(\{\s*cmd:\s*"((?:[^"\\]|\\.)*)"`)

// aiCodexPatch는 apply_patch가 고치는 첫 파일이다.
var aiCodexPatch = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: ([^\\\n"]+)`)

// aiSessionInfo는 대화 기록 끝부분에 함께 있는 세션 정보다. 활동을 찾으려고 읽는 범위에서 얻는다.
// Claude의 누적 token만 파일 전체가 필요하므로 처음 한 번 전체를 읽고, 그 뒤에는 새로 붙은 줄만 읽는다.
type aiSessionInfo struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Mode는 Claude의 permissionMode, Codex의 approval_policy다.
	Mode    string `json:"mode,omitempty"`
	Sandbox string `json:"sandbox,omitempty"`
	// Context는 마지막 요청이 보낸 입력 token이다. cache에서 읽은 token도 들어간다.
	Context       int64   `json:"context,omitempty"`
	ContextWindow int64   `json:"context_window,omitempty"`
	Tokens        aiUsage `json:"tokens"`
	// Last는 마지막 요청의 token이다. Input은 cache에서 읽지 않은 입력이다.
	Last aiUsage `json:"last"`
	// TurnTools와 TurnFailed는 이번 턴의 도구 호출 수와 실패 수다. 턴 시작이 읽은 범위 밖이면 TurnPartial이고 값은 하한이다.
	TurnTools   int    `json:"turn_tools,omitempty"`
	TurnFailed  int    `json:"turn_failed,omitempty"`
	TurnPartial bool   `json:"turn_partial,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// Rate는 최근 aiRateWindow 동안의 분당 token이다. Fast는 세션 평균보다 확실히 빠르다는 뜻이다.
	Rate   int64 `json:"tokens_per_minute,omitempty"`
	Fast   bool  `json:"fast,omitempty"`
	points []aiTokenPoint
}

// aiTokenPoint는 그 시각까지의 누적 token이다.
type aiTokenPoint struct {
	at    time.Time
	total int64
}

const (
	aiRateWindow = 5 * time.Minute
	// aiFastRatio배를 넘고 aiFastMinimum 이상이어야 빠르다고 본다. 막 시작한 세션은 평균이 작아 쉽게 넘는다.
	aiFastRatio   = 1.5
	aiFastMinimum = 1000
	// aiClaudeSmallWindow를 넘은 Claude 세션은 1M 창으로 본다. Claude 기록에는 창 크기가 없다.
	aiClaudeSmallWindow = 200_000
	aiClaudeLargeWindow = 1_000_000
)

// aiCodexExitCode는 Codex exec_command 결과 JSON의 종료 코드다.
var aiCodexExitCode = regexp.MustCompile(`exit_code\\?"\s*:\s*(-?\d+)`)

// aiCodexOutput은 결과 JSON 안의 명령 출력이다. 결과는 JSON 문자열 안의 JSON이라 따옴표가 \"로 끝난다.
var aiCodexOutput = regexp.MustCompile(`"output\\?"\s*:\s*\\?"((?:[^"\\]|\\[^"])*)`)

type aiActivityCache struct {
	size     int64
	modified time.Time
	activity aiActivity
	ok       bool
	info     aiSessionInfo
	// offset과 seen, tokens는 Claude 누적 token을 이어서 세는 상태다.
	offset int64
	seen   map[string]aiSeenMessage
	tokens aiUsage
	points []aiTokenPoint
}

// aiActivityReader는 대화 기록이 바뀌었을 때만 다시 읽는다.
type aiActivityReader map[string]*aiActivityCache

func (reader aiActivityReader) read(tool, path string) (aiActivity, bool, aiSessionInfo) {
	stat, err := os.Stat(path)
	if err != nil {
		return aiActivity{}, false, aiSessionInfo{}
	}
	cached := reader[path]
	if cached == nil || stat.Size() < cached.offset {
		cached = &aiActivityCache{seen: map[string]aiSeenMessage{}}
		reader[path] = cached
	} else if cached.size == stat.Size() && cached.modified.Equal(stat.ModTime()) {
		return cached.activity, cached.ok, cached.info
	}
	cached.size, cached.modified = stat.Size(), stat.ModTime()
	previous := cached.info
	cached.activity, cached.ok, cached.info = readAITranscriptActivity(tool, path, stat.Size())
	keepAISessionInfo(&cached.info, previous)
	if tool == "claude" {
		readAIClaudeTokens(path, cached)
		cached.info.Tokens, cached.info.points = cached.tokens, cached.points
	}
	return cached.activity, cached.ok, cached.info
}

// keepAISessionInfo는 끝부분에 없던 값을 앞서 읽은 값으로 채운다. 큰 도구 출력이 끝부분을 채우면 마지막 응답이 범위 밖으로 밀린다.
func keepAISessionInfo(info *aiSessionInfo, previous aiSessionInfo) {
	if info.Model == "" {
		info.Model, info.Context = previous.Model, previous.Context
	}
	if info.ContextWindow == 0 {
		info.ContextWindow = previous.ContextWindow
	}
	for _, pair := range []struct{ value, old *string }{{&info.Branch, &previous.Branch}, {&info.Mode, &previous.Mode}, {&info.Effort, &previous.Effort}, {&info.Sandbox, &previous.Sandbox}} {
		if *pair.value == "" {
			*pair.value = *pair.old
		}
	}
	if info.Last.empty() {
		info.Last = previous.Last
	}
}

// readAIClaudeTokens는 읽은 위치부터 끝난 줄의 token을 더한다. 처음에는 파일 전체를 읽는다.
func readAIClaudeTokens(path string, cached *aiActivityCache) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	if _, err := file.Seek(cached.offset, io.SeekStart); err != nil {
		return
	}
	reader := bufio.NewReaderSize(file, 256<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		cached.offset += int64(len(line))
		if at, usage, ok := aiClaudeUsageDelta(line, cached.seen); ok {
			cached.tokens.add(usage)
			cached.points = append(cached.points, aiTokenPoint{at: at, total: cached.tokens.total()})
		}
	}
}

// aiTokenRate는 최근 aiRateWindow의 분당 token과, 그 값이 세션 평균보다 빠른지 돌려준다.
// 창 앞의 마지막 점을 기준으로 삼는다. 그런 점이 없으면 세션이 창 안에서 시작했으므로 0에서 센다.
func aiTokenRate(points []aiTokenPoint, now time.Time) (int64, bool) {
	if len(points) == 0 {
		return 0, false
	}
	last := points[len(points)-1]
	from := now.Add(-aiRateWindow)
	var base int64
	for _, point := range points {
		if point.at.After(from) {
			break
		}
		base = point.total
	}
	rate := (last.total - base) * int64(time.Minute) / int64(aiRateWindow)
	span := last.at.Sub(points[0].at)
	if span < aiRateWindow {
		return rate, false
	}
	average := float64(last.total) / span.Minutes()
	return rate, rate >= aiFastMinimum && float64(rate) > average*aiFastRatio
}

// annotate는 세션 정보를 붙이고, 작업 중인 세션에만 활동을 붙인다. 다른 상태는 hook이 남긴 설명이 더 정확하다.
func (reader aiActivityReader) annotate(sessions []aiSession, now time.Time) {
	for index := range sessions {
		session := &sessions[index]
		if session.State == aiStateEnded || session.Transcript == "" {
			continue
		}
		activity, ok, info := reader.read(session.Tool, session.Transcript)
		info.Rate, info.Fast = aiTokenRate(info.points, now)
		session.Info = &info
		if ok && session.State == aiStateWorking {
			session.Activity = &activity
		}
	}
}

func readAITranscriptActivity(tool, path string, size int64) (aiActivity, bool, aiSessionInfo) {
	var info aiSessionInfo
	file, err := os.Open(path)
	if err != nil {
		return aiActivity{}, false, info
	}
	defer file.Close()
	start := size - aiActivityTail
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return aiActivity{}, false, info
	}
	data, err := io.ReadAll(io.LimitReader(file, aiActivityTail))
	if err != nil {
		return aiActivity{}, false, info
	}
	if start > 0 {
		// 중간에서 시작한 첫 줄은 잘린 JSON이다.
		if cut := bytes.IndexByte(data, '\n'); cut >= 0 {
			data = data[cut+1:]
		}
	}
	var activity aiActivity
	var ok bool
	if tool == "codex" {
		activity, ok = parseAICodexActivity(data, &info)
	} else {
		activity, ok = parseAIClaudeActivity(data, &info)
	}
	return activity, ok, info
}

// aiClaudeResultText는 도구 결과의 글자만 꺼낸다. 결과는 문자열이거나 text 조각의 배열이다.
func aiClaudeResultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(part.Text + " ")
	}
	return builder.String()
}

// parseAIClaudeActivity는 마지막 활동을 고른다. 결과가 온 도구 호출은 끝난 것으로 표시한다.
func parseAIClaudeActivity(data []byte, info *aiSessionInfo) (aiActivity, bool) {
	// 턴 시작을 만나기 전까지 센 값은 읽은 범위 안의 하한이다.
	info.TurnPartial = true
	var last aiActivity
	found := false
	pending := map[string]bool{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry struct {
			Type           string    `json:"type"`
			Timestamp      time.Time `json:"timestamp"`
			GitBranch      string    `json:"gitBranch"`
			PermissionMode string    `json:"permissionMode"`
			IsMeta         bool      `json:"isMeta"`
			Message        struct {
				Model   string          `json:"model"`
				Content json.RawMessage `json:"content"`
				Usage   *struct {
					Input         int64 `json:"input_tokens"`
					CacheCreation int64 `json:"cache_creation_input_tokens"`
					CacheRead     int64 `json:"cache_read_input_tokens"`
					Output        int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" && entry.Type != "user" {
			continue
		}
		if entry.GitBranch != "" {
			info.Branch = entry.GitBranch
		}
		if entry.PermissionMode != "" {
			info.Mode = entry.PermissionMode
		}
		// <synthetic>은 Claude Code가 API 없이 만든 응답이다. 모델과 컨텍스트가 없다.
		if entry.Message.Model != "" && entry.Message.Model != "<synthetic>" {
			info.Model = entry.Message.Model
			if usage := entry.Message.Usage; usage != nil {
				info.Context = usage.Input + usage.CacheCreation + usage.CacheRead
				info.Last = aiUsage{Requests: 1, Input: usage.Input + usage.CacheCreation, Output: usage.Output, Cache: usage.CacheRead}
			}
		}
		// 문자열 content는 사용자가 친 프롬프트다. 도구 결과는 배열로 온다.
		var prompt string
		if entry.Type == "user" && !entry.IsMeta && json.Unmarshal(entry.Message.Content, &prompt) == nil {
			info.TurnTools, info.TurnFailed, info.LastError, info.TurnPartial = 0, 0, "", false
			continue
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
			continue
		}
		for _, part := range parts {
			switch {
			case part.Type == "tool_use":
				pending[part.ID] = true
				info.TurnTools++
				last, found = aiActivity{Kind: "tool", Text: aiHookToolDetail(part.Name, part.Input), At: entry.Timestamp, Running: true}, true
			case part.Type == "tool_result":
				delete(pending, part.ToolUseID)
				if part.IsError {
					info.TurnFailed++
					info.LastError = aiHookClip(aiClaudeResultText(part.Content), false)
				}
			case part.Type == "text" && entry.Type == "assistant" && strings.TrimSpace(part.Text) != "":
				last, found = aiActivity{Kind: "text", Text: aiHookClip(part.Text, true), At: entry.Timestamp}, true
			case part.Type == "thinking":
				last, found = aiActivity{Kind: "thinking", At: entry.Timestamp}, true
			}
		}
	}
	// 병렬 호출 가운데 하나라도 결과가 없으면 아직 실행 중이다.
	last.Running = last.Kind == "tool" && len(pending) > 0
	return last, found
}

func parseAICodexActivity(data []byte, info *aiSessionInfo) (aiActivity, bool) {
	info.TurnPartial = true
	var last aiActivity
	found := false
	pending := map[string]bool{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Payload   struct {
				Type           string `json:"type"`
				Model          string `json:"model"`
				Effort         string `json:"effort"`
				ApprovalPolicy string `json:"approval_policy"`
				SandboxPolicy  struct {
					Type string `json:"type"`
				} `json:"sandbox_policy"`
				Info *struct {
					Total  aiCodexTotals `json:"total_token_usage"`
					Last   aiCodexTotals `json:"last_token_usage"`
					Window int64         `json:"model_context_window"`
				} `json:"info"`
				Window    int64           `json:"model_context_window"`
				Output    json.RawMessage `json:"output"`
				Role      string          `json:"role"`
				Name      string          `json:"name"`
				CallID    string          `json:"call_id"`
				Arguments string          `json:"arguments"`
				Input     string          `json:"input"`
				Content   []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		payload := entry.Payload
		switch {
		case entry.Type == "turn_context":
			info.Model, info.Effort, info.Mode, info.Sandbox = payload.Model, payload.Effort, payload.ApprovalPolicy, payload.SandboxPolicy.Type
			continue
		case entry.Type == "event_msg" && payload.Type == "task_started":
			info.TurnTools, info.TurnFailed, info.LastError, info.TurnPartial = 0, 0, "", false
			if payload.Window > 0 {
				info.ContextWindow = payload.Window
			}
			continue
		case entry.Type == "event_msg" && payload.Type == "token_count" && payload.Info != nil:
			// 누적값이므로 마지막 이벤트가 세션 전체다. cached_input_tokens는 input_tokens에 들어 있다.
			total, latest := payload.Info.Total, payload.Info.Last
			info.Tokens = aiUsage{Input: aiGrowth(total.Cached, total.Input), Output: total.Output, Cache: total.Cached}
			info.Last = aiUsage{Requests: 1, Input: aiGrowth(latest.Cached, latest.Input), Output: latest.Output, Cache: latest.Cached}
			info.Context, info.ContextWindow = latest.Input, payload.Info.Window
			info.points = append(info.points, aiTokenPoint{at: entry.Timestamp, total: info.Tokens.total()})
			continue
		case entry.Type != "response_item":
			continue
		}
		switch payload.Type {
		case "function_call", "custom_tool_call":
			pending[payload.CallID] = true
			info.TurnTools++
			last, found = aiActivity{Kind: "tool", Text: aiCodexToolDetail(payload.Name, payload.Arguments+payload.Input), At: entry.Timestamp, Running: true}, true
		case "function_call_output", "custom_tool_call_output":
			delete(pending, payload.CallID)
			if text, failed := aiCodexFailure(payload.Output); failed {
				info.TurnFailed++
				info.LastError = text
			}
		case "reasoning":
			last, found = aiActivity{Kind: "thinking", At: entry.Timestamp}, true
		case "message":
			if payload.Role != "assistant" {
				continue
			}
			var text strings.Builder
			for _, content := range payload.Content {
				text.WriteString(content.Text)
			}
			if strings.TrimSpace(text.String()) != "" {
				last, found = aiActivity{Kind: "text", Text: aiHookClip(text.String(), true), At: entry.Timestamp}, true
			}
		}
	}
	last.Running = last.Kind == "tool" && len(pending) > 0
	return last, found
}

// aiCodexFailure는 exec_command 결과의 종료 코드가 0이 아니면 그 코드와 출력 앞부분을 돌려준다.
func aiCodexFailure(raw json.RawMessage) (string, bool) {
	text := string(raw)
	match := aiCodexExitCode.FindStringSubmatch(text)
	if match == nil || match[1] == "0" {
		return "", false
	}
	detail := "exit " + match[1]
	if output := aiCodexOutput.FindStringSubmatch(text); output != nil {
		unescaped := strings.NewReplacer(`\\n`, " ", `\n`, " ", `\\"`, `"`, `\"`, `"`).Replace(output[1])
		detail += " · " + unescaped
	}
	return aiHookClip(detail, false), true
}

// aiCodexToolDetail은 Codex 도구 호출을 Claude와 같은 "도구: 대상" 형태로 줄인다.
func aiCodexToolDetail(name, input string) string {
	if match := aiCodexCommand.FindStringSubmatch(input); match != nil {
		command := match[1]
		if unquoted, err := strconv.Unquote(`"` + command + `"`); err == nil {
			command = unquoted
		}
		return aiHookClip("exec_command: "+command, false)
	}
	if match := aiCodexPatch.FindStringSubmatch(input); match != nil {
		return aiHookClip("apply_patch: "+match[1], false)
	}
	var arguments map[string]any
	if json.Unmarshal([]byte(input), &arguments) == nil {
		raw, _ := json.Marshal(arguments)
		return aiHookToolDetail(name, raw)
	}
	return aiHookClip(name, false)
}

// aiActivityText는 목록 한 줄에 넣을 활동이다. 실행 중인 도구는 걸린 시간을 앞에 둔다. 좁은 칸에서 설명 끝이 잘려도 시간은 남는다.
func aiActivityText(activity *aiActivity, now time.Time) string {
	if activity == nil {
		return ""
	}
	switch {
	case activity.Kind == "tool" && activity.Running:
		return formatTopEventDuration(now.Sub(activity.At)) + " · " + activity.Text
	case activity.Kind == "tool":
		return "thinking · after " + activity.Text
	case activity.Kind == "thinking":
		return "thinking"
	}
	return activity.Text
}
