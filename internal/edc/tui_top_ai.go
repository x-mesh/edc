package edc

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// topAISource는 AI 패널이 쓰는 한도 조회다. *aiCollector가 맞고, 시험은 정해 둔 값을 준다.
type topAISource interface {
	poll(ctx context.Context, poll time.Duration) aiPollResult
	close()
}

// topAIStartMsg는 AI 패널을 켠 채로 시작할 때 Init이 보낸다. Init은 모델을 바꿀 수 없으므로 수집기는 Update에서 연다.
type topAIStartMsg struct{}

type topAIPollMsg aiPollResult

// toggleAI는 a 키다. 수집기는 처음 켤 때 열어, 끈 채로 쓰는 실행은 ~/.claude를 읽거나 한도 API를 부르지 않는다.
func (model topModel) toggleAI() (topModel, tea.Cmd) {
	model.aiOn = !model.aiOn
	if !model.aiOn {
		return model, nil
	}
	return model.startAI()
}

// startAI는 앞선 조회가 끝나지 않았으면 새로 부르지 않는다. 수집기 상태를 두 goroutine이 함께 쓰지 않는다.
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
	if model.aiPolling {
		return model, nil
	}
	model.aiPolling = true
	return model, model.aiPollCmd(0)
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
