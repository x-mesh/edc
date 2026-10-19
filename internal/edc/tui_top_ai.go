package edc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// topAISource는 AI 패널이 쓰는 한도 조회다. *aiCollector가 맞고, 시험은 정해 둔 값을 준다.
type topAISource interface {
	poll(ctx context.Context, poll time.Duration) aiPollResult
	scan() aiUsageSnapshot
	close()
}

// topAIStartMsg는 AI 패널을 켠 채로 시작할 때 Init이 보낸다. Init은 모델을 바꿀 수 없으므로 수집기는 Update에서 연다.
type topAIStartMsg struct{}

type topAIPollMsg aiPollResult

type topAIScanMsg aiUsageSnapshot

// toggleAI는 a 키다. 수집기는 처음 켤 때 열어, 끈 채로 쓰는 실행은 ~/.claude를 읽거나 한도 API를 부르지 않는다.
func (model topModel) toggleAI() (topModel, tea.Cmd) {
	model.aiOn = !model.aiOn
	if !model.aiOn {
		return model, nil
	}
	return model.startAI()
}

// startAI는 AI 패널의 한도 조회와 ai 열의 기록 scan을 시작한다. 앞선 호출이 끝나지 않았으면 새로 부르지 않는다.
// 수집기 상태를 같은 일의 두 goroutine이 함께 쓰지 않는다. poll과 scan은 서로 다른 필드를 쓴다.
func (model topModel) startAI() (topModel, tea.Cmd) {
	if model.aiSource == nil {
		if model.openAI == nil {
			model.aiOn, model.notice = false, "ai panel is unavailable"
			return model, nil
		}
		source, err := model.openAI()
		if err != nil {
			model.aiOn, model.notice = false, "ai panel: "+err.Error()
			return model, nil
		}
		model.aiSource = source
	}
	var cmds []tea.Cmd
	if model.aiOn && !model.aiPolling {
		model.aiPolling = true
		cmds = append(cmds, model.aiPollCmd(0))
	}
	if model.aiTokens && !model.aiScanning {
		model.aiScanning = true
		cmds = append(cmds, model.aiScanCmd(0))
	}
	return model, tea.Batch(cmds...)
}

func (model topModel) updateAIScan(snapshot aiUsageSnapshot) (topModel, tea.Cmd) {
	model.aiSeconds, model.aiFailed, model.aiScanning = snapshot.Seconds, snapshot.Failed, false
	if !model.aiTokens {
		return model, nil
	}
	model.aiScanning = true
	return model, model.aiScanCmd(aiScanInterval)
}

func (model topModel) aiScanCmd(delay time.Duration) tea.Cmd {
	source := model.aiSource
	run := func() tea.Msg { return topAIScanMsg(source.scan()) }
	if delay == 0 {
		return run
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return run() })
}

// topAITokens는 한 행의 ai 열 값이다. claudeOK와 codexOK가 false이면 그 도구의 기록을 아직 읽지 못했거나
// 이번 scan에서 읽지 못한 것이다. 이때 0을 보이면 요청이 없었던 것으로 읽힌다.
type topAITokens struct {
	claudeOK, codexOK bool
	claude, codex     aiUsage
}

// aiTokensBetween은 from 뒤부터 to까지 기록된 token이다. 행마다 직전 행부터 그 행까지 들어온 요청을 세어 다른 열과
// 같은 줄에서 비교할 수 있게 한다. 행 시각은 초 경계에 맞지 않으므로 두 끝이 든 초까지 읽고 ms 시각으로 거른다.
func (model topModel) aiTokensBetween(from, to time.Time) topAITokens {
	if model.aiSeconds == nil {
		return topAITokens{}
	}
	tokens := topAITokens{claudeOK: !model.aiFailed["claude"], codexOK: !model.aiFailed["codex"]}
	sum := func(provider string, usage *aiUsage) {
		for second := from.Unix(); second <= to.Unix(); second++ {
			for _, stamp := range model.aiSeconds[provider][second] {
				if stamp.At > from.UnixMilli() && stamp.At <= to.UnixMilli() {
					usage.add(stamp.Usage)
				}
			}
		}
	}
	sum("claude", &tokens.claude)
	sum("codex", &tokens.codex)
	return tokens
}

// previousRowAt은 at 바로 앞 행의 시각이다. 첫 행은 interval 하나 앞으로 본다.
func (model topModel) previousRowAt(at time.Time) time.Time {
	index := sort.Search(len(model.rows), func(index int) bool { return !model.rows[index].at.Before(at) })
	if index > 0 {
		return model.rows[index-1].at
	}
	return at.Add(-model.interval)
}

func (model topModel) updateAIPoll(result aiPollResult) (topModel, tea.Cmd) {
	model.aiProviders, model.aiPolled, model.aiPolling = result.providers, true, false
	if !model.aiOn {
		return model, nil
	}
	model.aiPolling = true
	return model, model.aiPollCmd(aiDefaultPoll)
}

func (model topModel) aiPollCmd(delay time.Duration) tea.Cmd {
	source := model.aiSource
	run := func() tea.Msg { return topAIPollMsg(source.poll(context.Background(), aiDefaultPoll)) }
	if delay == 0 {
		return run
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return run() })
}

func (model topModel) closeAI() {
	if model.aiSource != nil {
		model.aiSource.close()
	}
}

// aiPanelLines는 도구마다 한도 창 하나를 한 줄에 쓴다. 남은 시간은 마지막 샘플 시각으로 세어 화면이 표본과 함께 바뀐다.
func (model topModel) aiPanelLines() []string {
	lines := []string{"ai limits · a hide"}
	if !model.aiPolled {
		return append(lines, topDim("waiting for the first poll", model.limits.color))
	}
	color := model.limits.color
	now := model.latestAt()
	// 첫 샘플 전에는 행이 없어 남은 시간을 0년부터 세게 된다.
	if now.IsZero() {
		now = time.Now()
	}
	nameWidth := 3
	for _, provider := range model.aiProviders {
		for _, window := range provider.Windows {
			nameWidth = max(nameWidth, len(window.Name))
		}
	}
	for _, provider := range model.aiProviders {
		name := fmt.Sprintf("%-6s", provider.Name)
		for _, window := range provider.Windows {
			percent := fmt.Sprintf("%3.0f%%", window.Used)
			bar := topAIBar(window.Used)
			if color {
				bar, percent = aiUsageBar(window.Used), aiPaintPercent(percent, window.Used)
			}
			lines = append(lines, fmt.Sprintf("%s %-*s %s %s  %s", name, nameWidth, window.Name, bar, percent, aiResetText(window.ResetsAt, now)))
		}
		switch {
		case provider.Err != "":
			lines = append(lines, name+" "+topTint("! "+provider.Err, topLevelDanger, color))
		case len(provider.Windows) == 0:
			lines = append(lines, name+" "+topDim("no limits reported", color))
		}
	}
	return lines
}

// topAIBar는 색 없이 그리는 한도 막대다. 색을 쓸 때는 edc ai와 같은 aiUsageBar를 쓴다.
func topAIBar(used float64) string {
	filled := min(aiUsageBarWidth, max(0, int(used/100*aiUsageBarWidth+0.5)))
	return strings.Repeat("█", filled) + strings.Repeat("░", aiUsageBarWidth-filled)
}
