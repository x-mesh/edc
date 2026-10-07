package edc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// aiClaudeUsageURL은 Claude Code의 /usage가 부르는 API다. 공개 문서에 없으므로 응답이 바뀌면 오류로 드러낸다.
	aiClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	// aiClaudeBeta는 Claude Code가 OAuth token으로 API를 부를 때 보내는 beta header 값이다.
	aiClaudeBeta     = "oauth-2025-04-20"
	aiRequestTimeout = 10 * time.Second
	// aiBucket은 기록을 모으는 가장 작은 구간이다. 대시보드의 가장 짧은 행과 같다.
	aiBucket = 10 * time.Second
	// aiRetention은 기록을 보관하는 기간이다. 1시간 행으로 하루를 되돌아본다.
	aiRetention = 24 * time.Hour
	// aiResetTolerance보다 리셋 시각이 늦어져야 새 창으로 본다. Claude는 같은 창에서도 소수점 아래 초가 달라진다.
	aiResetTolerance = time.Minute
)

var errAIAuthExpired = errors.New("auth expired · open Claude Code to refresh")

// aiWindow는 한도 창 하나다. 두 도구를 같은 상자로 그리도록 이름, 사용률, 리셋 시각만 남긴다.
// 리셋 시각을 받지 못하면 ResetsAt은 0이고, --json에 0001-01-01을 시각처럼 내보내지 않게 뺀다.
type aiWindow struct {
	Name     string    `json:"name"`
	Used     float64   `json:"used_percent"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

type aiProvider struct {
	Name      string     `json:"name"`
	Plan      string     `json:"plan,omitempty"`
	Windows   []aiWindow `json:"windows"`
	FetchedAt time.Time  `json:"fetched_at,omitzero"`
	Err       string     `json:"error,omitempty"`
	// rateLimited는 429 응답이다. 수집기가 다음 조회를 늦춘다.
	rateLimited bool
}

func (provider aiProvider) window(name string) (aiWindow, bool) {
	for _, window := range provider.Windows {
		if window.Name == name {
			return window, true
		}
	}
	return aiWindow{}, false
}

type aiClaudeCredentials struct {
	ClaudeAiOauth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

func aiClaudeDir(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}

func aiCodexDir(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".codex")
}

// readAIClaudeToken은 조회할 때마다 파일을 다시 읽는다. Claude Code가 token을 갱신하면 이 파일만 바뀐다.
// 만료된 token은 갱신하지 않는다. refresh token을 여기서 쓰면 Claude Code의 login이 끊길 수 있다.
func readAIClaudeToken(dir string, now time.Time) (token, plan string, err error) {
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return "", "", errors.New("no Claude credentials · log in with Claude Code")
	}
	var credentials aiClaudeCredentials
	// 파싱 오류 문구에 파일 내용이 섞이지 않게 고정 문구만 돌려준다.
	if json.Unmarshal(data, &credentials) != nil {
		return "", "", errors.New("unreadable Claude credentials")
	}
	oauth := credentials.ClaudeAiOauth
	if oauth.AccessToken == "" {
		return "", oauth.SubscriptionType, errors.New("no Claude OAuth token · log in with Claude Code")
	}
	if oauth.ExpiresAt > 0 && !now.Before(time.UnixMilli(oauth.ExpiresAt)) {
		return "", oauth.SubscriptionType, errAIAuthExpired
	}
	return oauth.AccessToken, oauth.SubscriptionType, nil
}

func fetchAIClaude(ctx context.Context, client *http.Client, dir string, now time.Time) aiProvider {
	provider := aiProvider{Name: "claude", FetchedAt: now, Windows: []aiWindow{}}
	token, plan, err := readAIClaudeToken(dir, now)
	provider.Plan = plan
	if err != nil {
		provider.Err = err.Error()
		return provider
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, aiClaudeUsageURL, nil)
	if err != nil {
		provider.Err = err.Error()
		return provider
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", aiClaudeBeta)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		provider.Err = err.Error()
		return provider
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		provider.Err = err.Error()
		return provider
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		provider.Err = errAIAuthExpired.Error()
		return provider
	case response.StatusCode == http.StatusTooManyRequests:
		provider.Err, provider.rateLimited = "rate limited", true
		return provider
	case response.StatusCode != http.StatusOK:
		provider.Err = fmt.Sprintf("usage API answered HTTP %d", response.StatusCode)
		return provider
	}
	provider.Windows, err = parseAIClaudeUsage(body)
	if err != nil {
		provider.Err = "unexpected usage API response"
	}
	return provider
}

// aiClaudeWindows는 그릴 창과 순서다. 응답의 다른 키는 요금제 내부 항목이라 사용률의 뜻이 정해져 있지 않다.
var aiClaudeWindows = []struct{ key, name string }{
	{"five_hour", "5h"},
	{"seven_day", "7d"},
	{"seven_day_opus", "7d opus"},
	{"seven_day_sonnet", "7d sonnet"},
}

func parseAIClaudeUsage(data []byte) ([]aiWindow, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	windows := []aiWindow{}
	for _, known := range aiClaudeWindows {
		if len(raw[known.key]) == 0 {
			continue
		}
		var value *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if err := json.Unmarshal(raw[known.key], &value); err != nil {
			return nil, err
		}
		if value == nil || value.Utilization == nil {
			continue
		}
		window := aiWindow{Name: known.name, Used: *value.Utilization}
		if value.ResetsAt != nil {
			at, err := time.Parse(time.RFC3339Nano, *value.ResetsAt)
			if err != nil {
				return nil, err
			}
			// 응답은 08:59:59.84처럼 정각 직전을 준다. 초 단위로 맞춰야 카운트다운이 정각에 끝난다.
			window.ResetsAt = at.Round(time.Second)
		}
		windows = append(windows, window)
	}
	return windows, nil
}

type aiCodexWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

type aiCodexSnapshot struct {
	LimitID   string         `json:"limitId"`
	LimitName *string        `json:"limitName"`
	PlanType  string         `json:"planType"`
	Primary   *aiCodexWindow `json:"primary"`
	Secondary *aiCodexWindow `json:"secondary"`
}

// parseAICodexRateLimits는 account/rateLimits/read 결과를 창 목록으로 바꾼다.
// 기본 한도를 먼저 두고, 모델별 한도는 이름을 붙여 뒤에 둔다.
func parseAICodexRateLimits(data json.RawMessage) (string, []aiWindow, error) {
	var response struct {
		RateLimits          aiCodexSnapshot            `json:"rateLimits"`
		RateLimitsByLimitID map[string]aiCodexSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", nil, err
	}
	snapshots := []aiCodexSnapshot{response.RateLimits}
	ids := make([]string, 0, len(response.RateLimitsByLimitID))
	for id := range response.RateLimitsByLimitID {
		if id != response.RateLimits.LimitID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		snapshots = append(snapshots, response.RateLimitsByLimitID[id])
	}
	windows := []aiWindow{}
	for _, snapshot := range snapshots {
		label := ""
		if snapshot.LimitID != response.RateLimits.LimitID {
			label = " " + snapshot.LimitID
			if snapshot.LimitName != nil && *snapshot.LimitName != "" {
				label = " " + *snapshot.LimitName
			}
		}
		for _, value := range []*aiCodexWindow{snapshot.Primary, snapshot.Secondary} {
			if value == nil {
				continue
			}
			window := aiWindow{Name: aiWindowName(value.WindowDurationMins) + label, Used: value.UsedPercent}
			if value.ResetsAt > 0 {
				window.ResetsAt = time.Unix(value.ResetsAt, 0)
			}
			windows = append(windows, window)
		}
	}
	return response.RateLimits.PlanType, windows, nil
}

func aiWindowName(minutes int64) string {
	switch {
	case minutes > 0 && minutes%(24*60) == 0:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes > 0 && minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

type aiRPCMessage struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// aiCodexClient는 codex app-server와 stdio로 JSON-RPC를 주고받는다.
// 조회마다 process를 띄우지 않도록 edc ai가 도는 동안 하나를 유지한다.
type aiCodexClient struct {
	command  *exec.Cmd
	input    io.WriteCloser
	messages chan aiRPCMessage
	nextID   int
}

func startAICodexClient(path string) (*aiCodexClient, error) {
	command := exec.Command(path, "app-server")
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	client := &aiCodexClient{command: command, input: input, messages: make(chan aiRPCMessage, 16)}
	go client.read(output)
	if _, err := client.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "edc", "version": "ai"}}); err != nil {
		client.close()
		return nil, err
	}
	if err := client.write(map[string]any{"method": "initialized"}); err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func (client *aiCodexClient) read(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		var message aiRPCMessage
		// server가 보내는 요청과 알림은 method가 있다. 응답만 넘긴다.
		if json.Unmarshal(scanner.Bytes(), &message) != nil || message.ID == nil || message.Method != "" {
			continue
		}
		// 기다리는 호출이 없는 늦은 응답이 쌓여 채널이 차면 버린다. 막히면 process를 닫은 뒤에도 이 goroutine이 남는다.
		select {
		case client.messages <- message:
		default:
		}
	}
	close(client.messages)
}

func (client *aiCodexClient) write(message map[string]any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = client.input.Write(append(data, '\n'))
	return err
}

// call은 응답 id가 맞을 때까지 읽는다. 앞선 호출이 시간을 넘겨 늦게 온 응답은 건너뛴다.
func (client *aiCodexClient) call(method string, params any) (json.RawMessage, error) {
	client.nextID++
	id := client.nextID
	request := map[string]any{"method": method, "id": id}
	if params != nil {
		request["params"] = params
	}
	if err := client.write(request); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(aiRequestTimeout)
	defer deadline.Stop()
	for {
		select {
		case message, ok := <-client.messages:
			if !ok {
				return nil, errors.New("codex app-server exited")
			}
			if *message.ID != id {
				continue
			}
			if message.Error != nil {
				return nil, errors.New(message.Error.Message)
			}
			return message.Result, nil
		case <-deadline.C:
			return nil, fmt.Errorf("codex app-server did not answer %s", method)
		}
	}
}

func (client *aiCodexClient) close() {
	client.input.Close()
	if client.command.Process != nil {
		client.command.Process.Kill()
	}
	client.command.Wait()
}

type aiCodexSource struct {
	client *aiCodexClient
}

// fetch는 account/rateLimits/read만 부른다. 같은 protocol의 account/rateLimitResetCredit/consume은
// 한도 초기화 크레딧을 써 버리므로 edc ai에서 부르지 않는다.
func (source *aiCodexSource) fetch(now time.Time) aiProvider {
	provider := aiProvider{Name: "codex", FetchedAt: now, Windows: []aiWindow{}}
	if source.client == nil {
		path, err := exec.LookPath("codex")
		if err != nil {
			provider.Err = "codex not found in PATH"
			return provider
		}
		client, err := startAICodexClient(path)
		if err != nil {
			provider.Err = err.Error()
			return provider
		}
		source.client = client
	}
	result, err := source.client.call("account/rateLimits/read", nil)
	if err != nil {
		// app-server가 끝났거나 멈췄다. 다음 조회에서 새로 띄운다.
		source.close()
		provider.Err = err.Error()
		return provider
	}
	provider.Plan, provider.Windows, err = parseAICodexRateLimits(result)
	if err != nil {
		provider.Err = "unexpected codex rate limit response"
	}
	return provider
}

func (source *aiCodexSource) close() {
	if source.client != nil {
		source.client.close()
		source.client = nil
	}
}

// aiUsage는 한 구간에 쓴 token이다. in은 새로 보낸 입력, cache는 cache에서 읽은 입력이다.
type aiUsage struct {
	Requests int64 `json:"requests"`
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cache    int64 `json:"cache"`
}

func (usage aiUsage) total() int64 { return usage.Input + usage.Output + usage.Cache }

func (usage *aiUsage) add(other aiUsage) {
	usage.Requests += other.Requests
	usage.Input += other.Input
	usage.Output += other.Output
	usage.Cache += other.Cache
}

func aiGrowth(previous, next int64) int64 {
	if next > previous {
		return next - previous
	}
	return 0
}

type aiCodexTotals struct {
	Input  int64 `json:"input_tokens"`
	Cached int64 `json:"cached_input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}

type aiScanFile struct {
	offset int64
	// codex는 이 session 파일에서 마지막으로 읽은 누적 token이다.
	codex aiCodexTotals
}

type aiSeenMessage struct {
	at    time.Time
	usage aiUsage
}

// aiTokenScanner는 이 host의 대화 기록에서 token을 aiBucket 구간으로 모은다.
// 파일마다 읽은 위치를 기억해 새로 붙은 줄만 읽으므로 짧은 간격으로 불러도 부담이 작다.
type aiTokenScanner struct {
	claudeDir, codexDir string
	files               map[string]*aiScanFile
	buckets             map[string]map[time.Time]aiUsage
	// seen은 Claude 응답 하나가 여러 줄에 기록되는 것을 한 번만 센다.
	seen map[string]aiSeenMessage
}

func newAITokenScanner(claudeDir, codexDir string) *aiTokenScanner {
	return &aiTokenScanner{
		claudeDir: claudeDir, codexDir: codexDir,
		files:   map[string]*aiScanFile{},
		buckets: map[string]map[time.Time]aiUsage{"claude": {}, "codex": {}},
		seen:    map[string]aiSeenMessage{},
	}
}

// aiUsageSnapshot은 scan 한 번의 결과다. 대시보드가 행 구간을 바꿔도 다시 읽지 않도록 aiBucket 구간을 그대로 넘긴다.
type aiUsageSnapshot struct {
	At      time.Time
	Buckets map[string]map[time.Time]aiUsage
	Err     string
}

// aiTotalWindows는 Σ 행의 기간이다. 24h는 보관 기간 전체다.
var aiTotalWindows = []struct {
	name string
	span time.Duration
}{{"10m", 10 * time.Minute}, {"1h", time.Hour}, {"24h", aiRetention}}

type aiTotal struct {
	Window string  `json:"window"`
	Claude aiUsage `json:"claude"`
	Codex  aiUsage `json:"codex"`
}

// totals는 진행 중인 구간까지 넣어 기간마다 더한다. 시작을 aiBucket 경계에 맞춰 10초 행을 더한 값과 같게 한다.
func (snapshot aiUsageSnapshot) totals(now time.Time) []aiTotal {
	end := now.Truncate(aiBucket).Add(aiBucket)
	totals := make([]aiTotal, len(aiTotalWindows))
	for index, window := range aiTotalWindows {
		totals[index].Window = window.name
		from := end.Add(-window.span)
		for provider, buckets := range snapshot.Buckets {
			for bucket, usage := range buckets {
				if bucket.Before(from) || !bucket.Before(end) {
					continue
				}
				if provider == "claude" {
					totals[index].Claude.add(usage)
				} else {
					totals[index].Codex.add(usage)
				}
			}
		}
	}
	return totals
}

type aiRow struct {
	Start  time.Time `json:"start"`
	Claude aiUsage   `json:"claude"`
	Codex  aiUsage   `json:"codex"`
}

// aiRowStart는 at이 든 행의 시작이다. 1시간 행은 현지 시각의 정각에 맞춘다.
func aiRowStart(at time.Time, size time.Duration) time.Time {
	at = at.Local()
	if size >= time.Hour {
		return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, at.Location())
	}
	return at.Truncate(size)
}

// rows는 size 구간 count개를 오래된 것부터 돌려준다. 마지막 행은 end가 든 구간이다.
func (snapshot aiUsageSnapshot) rows(end time.Time, size time.Duration, count int) []aiRow {
	last := aiRowStart(end, size)
	rows := make([]aiRow, count)
	index := make(map[time.Time]int, count)
	for position := range rows {
		rows[position].Start = last.Add(-time.Duration(count-1-position) * size)
		index[rows[position].Start] = position
	}
	for provider, buckets := range snapshot.Buckets {
		for bucket, usage := range buckets {
			position, ok := index[aiRowStart(bucket, size)]
			if !ok {
				continue
			}
			if provider == "claude" {
				rows[position].Claude.add(usage)
			} else {
				rows[position].Codex.add(usage)
			}
		}
	}
	return rows
}

func (scanner *aiTokenScanner) scan(now time.Time) aiUsageSnapshot {
	oldest := now.Add(-aiRetention)
	var errs []string
	for _, source := range []struct {
		provider, dir string
		line          func(*aiScanFile, []byte, time.Time)
	}{
		{"claude", filepath.Join(scanner.claudeDir, "projects"), scanner.claudeLine},
		{"codex", filepath.Join(scanner.codexDir, "sessions"), scanner.codexLine},
	} {
		if err := scanner.walk(source.dir, oldest, source.line); err != nil {
			errs = append(errs, source.provider+": "+err.Error())
		}
	}
	snapshot := aiUsageSnapshot{At: now, Buckets: map[string]map[time.Time]aiUsage{}, Err: strings.Join(errs, " · ")}
	for provider, buckets := range scanner.buckets {
		copied := make(map[time.Time]aiUsage, len(buckets))
		for bucket, usage := range buckets {
			if bucket.Before(oldest.Truncate(aiBucket)) {
				delete(buckets, bucket)
				continue
			}
			copied[bucket] = usage
		}
		snapshot.Buckets[provider] = copied
	}
	for key, message := range scanner.seen {
		if message.at.Before(oldest) {
			delete(scanner.seen, key)
		}
	}
	return snapshot
}

// walk는 디렉터리가 없으면 그 도구를 쓰지 않은 host로 보고 0으로 둔다.
func (scanner *aiTokenScanner) walk(root string, oldest time.Time, line func(*aiScanFile, []byte, time.Time)) error {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		file, tracked := scanner.files[path]
		if !tracked {
			info, err := entry.Info()
			// 기록이 멈춘 파일은 보관 구간에 더할 줄이 없다.
			if err != nil || info.ModTime().Before(oldest) {
				return nil
			}
			file = &aiScanFile{}
			scanner.files[path] = file
		}
		return scanner.readFile(path, file, oldest, line)
	})
}

// readFile은 줄바꿈으로 끝난 줄만 읽는다. 기록 중인 마지막 줄은 다음 scan에서 온전히 읽는다.
func (scanner *aiTokenScanner) readFile(path string, file *aiScanFile, oldest time.Time, line func(*aiScanFile, []byte, time.Time)) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return err
	}
	if info.Size() == file.offset {
		return nil
	}
	if info.Size() < file.offset {
		// 파일이 잘렸거나 새로 쓰였다. 처음부터 다시 읽는다.
		*file = aiScanFile{}
	}
	if _, err := handle.Seek(file.offset, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(handle)
	if err != nil {
		return err
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil
	}
	for _, text := range bytes.Split(data[:end], []byte{'\n'}) {
		line(file, text, oldest)
	}
	file.offset += int64(end + 1)
	return nil
}

func (scanner *aiTokenScanner) add(provider string, at, oldest time.Time, usage aiUsage) {
	if usage.total() <= 0 || at.Before(oldest) {
		return
	}
	bucket := at.Truncate(aiBucket)
	value := scanner.buckets[provider][bucket]
	value.add(usage)
	scanner.buckets[provider][bucket] = value
}

// claudeLine은 cache 생성을 새 입력으로, cache 읽기를 cache로 센다.
func (scanner *aiTokenScanner) claudeLine(_ *aiScanFile, line []byte, oldest time.Time) {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	var record struct {
		Timestamp time.Time `json:"timestamp"`
		RequestID string    `json:"requestId"`
		Message   struct {
			ID    string `json:"id"`
			Usage *struct {
				Input         int64 `json:"input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				Output        int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &record) != nil || record.Message.Usage == nil || record.Timestamp.IsZero() {
		return
	}
	raw := record.Message.Usage
	usage := aiUsage{Requests: 1, Input: raw.Input + raw.CacheCreation, Output: raw.Output, Cache: raw.CacheRead}
	if record.Message.ID == "" {
		scanner.add("claude", record.Timestamp, oldest, usage)
		return
	}
	// 같은 응답의 앞선 줄은 output이 덜 찬 값이다. 늘어난 만큼만 처음 기록한 구간에 더한다.
	key := record.Message.ID + ":" + record.RequestID
	seen, ok := scanner.seen[key]
	if !ok {
		scanner.seen[key] = aiSeenMessage{at: record.Timestamp, usage: usage}
		scanner.add("claude", record.Timestamp, oldest, usage)
		return
	}
	growth := aiUsage{
		Input:  aiGrowth(seen.usage.Input, usage.Input),
		Output: aiGrowth(seen.usage.Output, usage.Output),
		Cache:  aiGrowth(seen.usage.Cache, usage.Cache),
	}
	if growth.total() > 0 {
		scanner.add("claude", seen.at, oldest, growth)
		seen.usage.Input, seen.usage.Output, seen.usage.Cache = seen.usage.Input+growth.Input, seen.usage.Output+growth.Output, seen.usage.Cache+growth.Cache
		scanner.seen[key] = seen
	}
}

// codexLine은 누적값의 증가분을 더한다. token_count 이벤트는 같은 누적값을 여러 번 기록한다.
// total_tokens가 input과 output의 합이므로 cached_input_tokens는 input_tokens에 들어 있다고 보고 뺀다.
func (scanner *aiTokenScanner) codexLine(file *aiScanFile, line []byte, oldest time.Time) {
	if !bytes.Contains(line, []byte(`"token_count"`)) {
		return
	}
	var record struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Type string `json:"type"`
			Info *struct {
				Total aiCodexTotals `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil || record.Payload.Type != "token_count" || record.Payload.Info == nil {
		return
	}
	total, last := record.Payload.Info.Total, file.codex
	if total.Total > last.Total {
		cached := aiGrowth(last.Cached, total.Cached)
		scanner.add("codex", record.Timestamp, oldest, aiUsage{
			Requests: 1,
			Input:    aiGrowth(cached, aiGrowth(last.Input, total.Input)),
			Output:   aiGrowth(last.Output, total.Output),
			Cache:    cached,
		})
	}
	file.codex = total
}

type aiResetEvent struct {
	At         time.Time `json:"at"`
	Provider   string    `json:"provider"`
	Window     string    `json:"window"`
	ResetAt    time.Time `json:"reset_at"`
	NextReset  time.Time `json:"next_reset"`
	UsedBefore float64   `json:"used_before"`
	UsedAfter  float64   `json:"used_after"`
}

// detectAIResets는 리셋된 창을 찾는다. 리셋 시각이 뒤로 밀린 것만으로는 부족하다.
// 쓰지 않은 Codex 창은 조회할 때마다 "지금 + 창 길이"를 돌려주므로, 앞선 리셋 시각이 지났거나
// 크레딧으로 일찍 리셋되어 사용률이 내려간 경우만 리셋으로 본다.
func detectAIResets(previous, next aiProvider, now time.Time) []aiResetEvent {
	var events []aiResetEvent
	for _, window := range next.Windows {
		old, ok := previous.window(window.Name)
		if !ok || old.ResetsAt.IsZero() || window.ResetsAt.IsZero() {
			continue
		}
		due := !now.Before(old.ResetsAt.Add(-aiResetTolerance))
		if window.ResetsAt.Sub(old.ResetsAt) > aiResetTolerance && (due || window.Used < old.Used) {
			events = append(events, aiResetEvent{
				At: now, Provider: next.Name, Window: window.Name,
				ResetAt: old.ResetsAt, NextReset: window.ResetsAt,
				UsedBefore: old.Used, UsedAfter: window.Used,
			})
		}
	}
	return events
}

func appendAIResets(path string, events []aiResetEvent) error {
	if len(events) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			file.Close()
			return err
		}
	}
	return file.Close()
}

// aiClaudeState는 ai-claude.json의 내용이다. 마지막으로 성공한 조회와 함께 다음 조회 시각과 429 백오프를 남긴다.
// 거절된 호출도 제한을 늘리므로, 다시 실행해도 백오프 중이면 그 시각 전에는 부르지 않는다. --poll 하한은 남기지 않는다.
type aiClaudeState struct {
	aiProvider
	NextTry time.Time     `json:"next_try,omitzero"`
	Backoff time.Duration `json:"backoff_ns,omitzero"`
}

// saveAIClaudeState는 Claude 조회 상태를 쓴다. 임시 파일을 바꿔 넣어 다른 edc ai가 반쯤 쓴 파일을 읽지 않는다.
func saveAIClaudeState(path string, state aiClaudeState) error {
	state.Name, state.Err = "claude", ""
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return err
	}
	return os.Rename(temp.Name(), path)
}

// loadAIClaudeState는 파일이 없거나 읽을 수 없으면 false를 돌려준다. 저장한 값은 다시 받을 수 있는 cache라서
// 그때는 처음 실행처럼 바로 조회한다.
// 한 번도 성공하지 못했어도 429로 미룬 시각은 남는다. 이때 FetchedAt은 0이다.
func loadAIClaudeState(path string) (aiClaudeState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return aiClaudeState{}, false
	}
	var state aiClaudeState
	if json.Unmarshal(data, &state) != nil || state.Name != "claude" || (state.FetchedAt.IsZero() && state.NextTry.IsZero()) {
		return aiClaudeState{}, false
	}
	if state.Windows == nil {
		state.Windows = []aiWindow{}
	}
	return state, true
}

// loadAILastResets는 도구마다 가장 최근 리셋을 읽는다. 상자는 edc ai를 다시 열어도 마지막 리셋을 보인다.
func loadAILastResets(path string) map[string]aiResetEvent {
	last := map[string]aiResetEvent{}
	data, err := os.ReadFile(path)
	if err != nil {
		return last
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var event aiResetEvent
		if json.Unmarshal(line, &event) != nil || event.Provider == "" {
			continue
		}
		if event.ResetAt.After(last[event.Provider].ResetAt) {
			last[event.Provider] = event
		}
	}
	return last
}
