package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	aiDefaultPoll = time.Minute
	// aiMinPoll은 화면 갱신과 Codex 조회의 하한이다. Claude 사용량 API에는 별도 간격을 적용한다.
	aiMinPoll = 30 * time.Second
	// aiClaudeMinInterval은 Claude 사용량 API를 부르는 최소 간격이다. 시험에서 1-4분 간격의 호출은 429를 받았고
	// 7분 쉰 뒤에는 200을 받았다. 10분은 이 관측보다 긴 기본 간격이다.
	aiClaudeMinInterval = 10 * time.Minute
	// aiMaxBackoff는 429를 받아 늘린 Claude 조회 간격의 상한이다.
	aiMaxBackoff = 30 * time.Minute
	// aiScanInterval은 대화 기록을 다시 읽는 간격이다. 새로 붙은 줄만 읽으므로 짧아도 부담이 작다.
	// Claude Code는 응답을 받은 뒤 1초 안에 기록하므로 진행 중인 행이 2초 안에 늘어난다.
	aiScanInterval = 2 * time.Second
	aiResetLogName = "ai-resets.jsonl"
	// aiClaudeStateName은 마지막으로 받은 Claude 사용량이다. token은 담지 않는다.
	aiClaudeStateName = "ai-claude.json"
)

func runAI(args []string, version string) int {
	set := flag.NewFlagSet("ai", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	poll := set.Duration("poll", aiDefaultPoll, T("command.ai.option.poll"))
	count := set.Int("count", 0, T("command.ai.option.count"))
	jsonPath := set.String("json", configuredStringFallback(nil, activeConfig.Defaults.Common.JSON, ""), T("option.json"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("observe.ai.usage"))
		return 2
	}
	if *poll < aiMinPoll {
		fmt.Fprintln(os.Stderr, T("observe.ai.poll_minimum", aiMinPoll))
		return 2
	}
	if *count < 0 {
		fmt.Fprintln(os.Stderr, T("observe.ai.count_minimum"))
		return 2
	}
	historyPath, err := defaultHistoryPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	home, _ := os.UserHomeDir()
	collector := newAICollector(aiClaudeDir(home), aiCodexDir(home), filepath.Dir(historyPath), time.Now())
	defer collector.close()

	var writer io.Writer = os.Stdout
	if *jsonPath != "" && *jsonPath != "-" {
		file, err := os.OpenFile(*jsonPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer file.Close()
		writer = file
	}
	jsonOutput := *jsonPath != ""
	if !jsonOutput && *count == 0 && liveTerminal() {
		return runAIDashboard(collector, *poll, version)
	}
	return runAIStream(writer, collector, *poll, *count, jsonOutput)
}

// aiCollector는 한도 조회, 기록 scan, 리셋 기록을 맡는다.
// 대시보드는 poll과 scan을 각각 앞선 호출이 끝난 뒤에만 다시 부르므로 두 메서드가 각자의 필드를 겹쳐 쓰지 않는다.
type aiCollector struct {
	http      *http.Client
	claudeDir string
	codex     aiCodexSource
	scanner   *aiTokenScanner
	resetLog  string
	// claudeState는 마지막으로 성공한 Claude 조회를 남기는 파일이다. 다시 열 때 429를 기다리지 않고 바로 보인다.
	claudeState string
	last        map[string]aiProvider
	lastResets  map[string]aiResetEvent
	// shown은 도구마다 마지막으로 보인 값이다. Claude를 건너뛴 조회도 이 값을 다시 보인다.
	shown map[string]aiProvider
	// claudeNext 전에는 Claude를 부르지 않는다. claudeBackoff는 429를 받을 때마다 두 배로 늘린 간격이고 성공해도 줄지 않는다.
	// --poll 하한과 따로 둔다. 둘을 한 값에 담으면 다른 --poll로 다시 실행할 때 앞선 하한이 백오프처럼 남는다.
	claudeNext    time.Time
	claudeBackoff time.Duration
}

func newAICollector(claudeDir, codexDir, stateDir string, now time.Time) *aiCollector {
	resetLog := filepath.Join(stateDir, aiResetLogName)
	collector := &aiCollector{
		http:        &http.Client{Timeout: aiRequestTimeout},
		claudeDir:   claudeDir,
		scanner:     newAITokenScanner(claudeDir, codexDir),
		resetLog:    resetLog,
		claudeState: filepath.Join(stateDir, aiClaudeStateName),
		last:        map[string]aiProvider{},
		lastResets:  loadAILastResets(resetLog),
		shown:       map[string]aiProvider{},
	}
	if state, ok := loadAIClaudeState(collector.claudeState); ok {
		if claude := state.aiProvider; !claude.FetchedAt.IsZero() {
			// 저장한 값을 앞선 조회로 삼는다. 꺼져 있는 동안 지난 리셋도 다음 조회에서 찾는다.
			collector.last["claude"], collector.shown["claude"] = claude, claude
			// 직전 실행이 조회한 지 최소 간격이 지나지 않았으면 그때까지 부르지 않는다. 시각이 미래면 믿지 않는다.
			if claude.FetchedAt.Before(now) {
				collector.claudeNext = claude.FetchedAt.Add(aiClaudeMinInterval)
			}
		}
		// 백오프는 직전 실행이 정한 다음 조회 시각에서 한 간격이 더 지날 때까지 이어진다. 그 안에 다시 실행하면
		// --count 1을 되풀이할 때처럼 같은 간격에서 다시 늘리고, 더 오래 쉬었으면 처음 간격으로 돌아간다.
		// 백오프 상한보다 먼 시각은 믿지 않는다.
		if backoff := min(state.Backoff, aiMaxBackoff); backoff > 0 && state.NextTry.Before(now.Add(aiMaxBackoff)) && now.Before(state.NextTry.Add(backoff)) {
			collector.claudeBackoff = backoff
			if state.NextTry.After(collector.claudeNext) {
				collector.claudeNext = state.NextTry
			}
			// 성공한 값 없이 429로 미룬 상태다. 상자가 빈 채로 기다리지 않게 미룬 이유와 다음 조회 시각을 보인다.
			if state.FetchedAt.IsZero() && state.NextTry.After(now) {
				collector.shown["claude"] = aiProvider{Name: "claude", Windows: []aiWindow{}, Err: "rate limited · next try " + state.NextTry.Local().Format("15:04:05")}
			}
		}
	}
	return collector
}

func (collector *aiCollector) close() { collector.codex.close() }

type aiPollResult struct {
	providers      []aiProvider
	lastResets     map[string]aiResetEvent
	claudeInterval time.Duration
	logErr         error
	stateErr       error
}

func (collector *aiCollector) poll(ctx context.Context, poll time.Duration) aiPollResult {
	now := time.Now()
	var codex aiProvider
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		codex = collector.codex.fetch(now)
	}()
	var fetched []aiProvider
	var stateErr error
	// poll 간격과 조회 시간이 겹쳐 예정 시각보다 조금 일찍 깨도 이번 차례로 본다.
	if !now.Add(time.Second).Before(collector.claudeNext) {
		claude := fetchAIClaude(ctx, collector.http, collector.claudeDir, now)
		collector.scheduleClaude(claude.rateLimited, now, poll)
		if claude.rateLimited {
			claude.Err += " · next try " + collector.claudeNext.Local().Format("15:04:05")
			// 다시 실행해도 미룬 시각 전에는 부르지 않도록 남긴다. 마지막으로 성공한 값은 그대로 둔다.
			stateErr = saveAIClaudeState(collector.claudeState, aiClaudeState{aiProvider: collector.last["claude"], NextTry: collector.claudeRetry(now), Backoff: collector.claudeBackoff})
		}
		fetched = append(fetched, claude)
	}
	wait.Wait()
	fetched = append(fetched, codex)

	var events []aiResetEvent
	for _, provider := range fetched {
		previous, known := collector.last[provider.Name]
		if provider.Err == "" {
			if known {
				events = append(events, detectAIResets(previous, provider, now)...)
			}
			collector.last[provider.Name], collector.shown[provider.Name] = provider, provider
			if provider.Name == "claude" {
				// 429 뒤에 늘어난 백오프는 성공해도 줄지 않는다. 다시 실행해도 이어지도록 다음 조회 시각과 함께 남긴다.
				stateErr = saveAIClaudeState(collector.claudeState, aiClaudeState{aiProvider: provider, NextTry: collector.claudeRetry(now), Backoff: collector.claudeBackoff})
			}
			continue
		}
		// 실패한 조회는 마지막으로 성공한 값을 지우지 않는다. 상자는 그 값과 오류를 함께 보인다.
		if known {
			previous.Err = provider.Err
			provider = previous
		} else {
			provider.FetchedAt = time.Time{}
		}
		collector.shown[provider.Name] = provider
	}
	result := aiPollResult{lastResets: map[string]aiResetEvent{}, claudeInterval: collector.claudeEvery(poll), stateErr: stateErr}
	for _, name := range []string{"claude", "codex"} {
		provider, ok := collector.shown[name]
		if !ok {
			provider = aiProvider{Name: name, Windows: []aiWindow{}}
		}
		result.providers = append(result.providers, provider)
	}
	result.logErr = appendAIResets(collector.resetLog, events)
	for _, event := range events {
		collector.lastResets[event.Provider] = event
	}
	for name, event := range collector.lastResets {
		result.lastResets[name] = event
	}
	return result
}

// scheduleClaude는 다음 Claude 조회 시각을 정한다. 사용량 API의 요청 한도는 공개되지 않았고 같은 계정의
// Claude Code도 이 API를 부른다. 429를 받으면 간격을 두 배로 늘리고, 성공해도 한도에 닿았던 간격으로 돌아가지 않는다.
func (collector *aiCollector) scheduleClaude(rateLimited bool, now time.Time, poll time.Duration) {
	if rateLimited {
		collector.claudeBackoff = min(2*collector.claudeEvery(poll), aiMaxBackoff)
	}
	collector.claudeNext = now.Add(collector.claudeEvery(poll))
}

// claudeEvery는 지금 적용되는 Claude 조회 간격이다. 저장한 값으로 시작해 아직 부르지 않았어도 상태 줄이 같은 간격을 보인다.
func (collector *aiCollector) claudeEvery(poll time.Duration) time.Duration {
	every := collector.claudeBackoff
	for _, floor := range []time.Duration{aiClaudeMinInterval, poll} {
		if every < floor {
			every = floor
		}
	}
	return every
}

// claudeRetry는 상태 파일에 남길 다음 조회 시각이다. --poll 하한은 이 실행의 설정이라 다음 실행으로 넘기지 않는다.
// 하한이 섞이면 --poll이 백오프 상한보다 길 때 저장한 시각이 복원 범위를 벗어나 백오프가 사라진다.
func (collector *aiCollector) claudeRetry(now time.Time) time.Time {
	every := collector.claudeBackoff
	if every < aiClaudeMinInterval {
		every = aiClaudeMinInterval
	}
	return now.Add(every)
}

func (collector *aiCollector) scan() aiUsageSnapshot { return collector.scanner.scan(time.Now()) }

type aiSample struct {
	At        time.Time    `json:"at"`
	Providers []aiProvider `json:"providers"`
	Totals    []aiTotal    `json:"totals"`
	Minutes   []aiRow      `json:"minutes"`
	Hours     []aiRow      `json:"hours"`
	ScanError string       `json:"scan_error,omitempty"`
}

// newAISample은 최근 60분을 1분 행으로, 최근 24시간을 1시간 행으로 담는다. 마지막 행은 진행 중인 구간이다.
func newAISample(at time.Time, providers []aiProvider, usage aiUsageSnapshot) aiSample {
	return aiSample{
		At: at, Providers: providers, ScanError: usage.Err,
		Totals:  usage.totals(at),
		Minutes: usage.rows(at, time.Minute, 60),
		Hours:   usage.rows(at, time.Hour, 24),
	}
}

// runAIStream은 표나 JSON을 poll 간격마다 쓴다. count가 0이면 중단할 때까지 쓴다.
func runAIStream(writer io.Writer, collector *aiCollector, poll time.Duration, count int, jsonOutput bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	encoder := json.NewEncoder(writer)
	for printed := 0; count == 0 || printed < count; printed++ {
		if printed > 0 {
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(poll):
			}
		}
		result := collector.poll(ctx, poll)
		if result.logErr != nil {
			fmt.Fprintln(os.Stderr, T("observe.ai.error.reset_log", result.logErr))
		}
		if result.stateErr != nil {
			fmt.Fprintln(os.Stderr, T("observe.ai.error.state", result.stateErr))
		}
		sample := newAISample(time.Now(), result.providers, collector.scan())
		if jsonOutput {
			if err := encoder.Encode(sample); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			continue
		}
		fmt.Fprint(writer, formatAITable(sample))
	}
	return 0
}

func formatAITable(sample aiSample) string {
	var builder strings.Builder
	at := sample.At.Local().Format("15:04:05")
	nameWidth := len("updated")
	for _, provider := range sample.Providers {
		for _, window := range provider.Windows {
			nameWidth = max(nameWidth, len(window.Name))
		}
	}
	for _, provider := range sample.Providers {
		if provider.Err != "" {
			fmt.Fprintf(&builder, "%s  %-6s  %-*s  %s\n", at, provider.Name, nameWidth, "error", provider.Err)
		}
		// 저장한 값이나 실패 전에 받은 값은 지금 값이 아니다. 언제 받은 값인지 같이 쓴다.
		if !provider.FetchedAt.IsZero() && sample.At.Sub(provider.FetchedAt) >= time.Minute {
			fmt.Fprintf(&builder, "%s  %-6s  %-*s  %s ago\n", at, provider.Name, nameWidth, "updated", aiAgo(sample.At.Sub(provider.FetchedAt)))
		}
		for _, window := range provider.Windows {
			fmt.Fprintf(&builder, "%s  %-6s  %-*s  %3.0f%%  %s\n", at, provider.Name, nameWidth, window.Name, window.Used, aiResetText(window.ResetsAt, sample.At))
		}
	}
	for _, total := range sample.Totals {
		fmt.Fprintf(&builder, "%s  tokens  %-*s  claude %s · codex %s · this host\n", at, nameWidth, total.Window, aiCompact(total.Claude.total()), aiCompact(total.Codex.total()))
	}
	if sample.ScanError != "" {
		fmt.Fprintf(&builder, "%s  scan    error  %s\n", at, sample.ScanError)
	}
	return builder.String()
}

func aiResetText(at, now time.Time) string {
	if at.IsZero() {
		return "no reset time"
	}
	if !now.Before(at) {
		return at.Local().Format("01-02 15:04:05") + "  reset due"
	}
	return at.Local().Format("01-02 15:04:05") + "  in " + aiCountdown(at.Sub(now))
}

// aiCountdown은 남은 시간을 초까지 보인다. 하루가 넘으면 앞에 일 수를 붙인다.
func aiCountdown(left time.Duration) string {
	if left < 0 {
		left = 0
	}
	total := int64(left / time.Second)
	days, hours, minutes, seconds := total/86400, total%86400/3600, total%3600/60, total%60
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d:%02d", days, hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}
