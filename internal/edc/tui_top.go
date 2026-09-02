package edc

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// runTopDashboard는 alt screen 대시보드를 실행한다. 종료하면 화면이 원래대로 돌아온다.
func runTopDashboard(interval time.Duration) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintf(os.Stderr, "host 정보를 읽지 못했습니다: %v\n", err)
		return 1
	}
	first, err := collectResourceSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resource를 읽지 못했습니다: %v\n", err)
		return 1
	}
	model := newTopModel(details, first, interval, collectResourceSnapshot)
	final, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if dashboard, ok := final.(topModel); ok && dashboard.err != nil {
		fmt.Fprintf(os.Stderr, "resource를 읽지 못했습니다: %v\n", dashboard.err)
		return 1
	}
	return 0
}

type topModel struct {
	details  hostDetails
	limits   topLimits
	interval time.Duration
	paused   bool
	previous resourceSnapshot
	rows     []string
	view     viewport.Model
	sample   func() (resourceSnapshot, error)
	seq      int
	err      error
}

// topSampleMsg는 tick마다 수집한 snapshot이다. seq가 다르면 interval이 바뀐 뒤의 낡은 tick이다.
type topSampleMsg struct {
	seq      int
	snapshot resourceSnapshot
	err      error
}

func newTopModel(details hostDetails, first resourceSnapshot, interval time.Duration, sample func() (resourceSnapshot, error)) topModel {
	return topModel{
		details: details, limits: newTopLimits(details.Cores, true), interval: interval,
		previous: first, view: viewport.New(), sample: sample,
	}
}

func (model topModel) Init() tea.Cmd { return model.tick() }

func (model topModel) tick() tea.Cmd {
	seq := model.seq
	sample := model.sample
	return tea.Tick(model.interval, func(time.Time) tea.Msg {
		snapshot, err := sample()
		return topSampleMsg{seq: seq, snapshot: snapshot, err: err}
	})
}

func (model topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case topSampleMsg:
		if value.seq != model.seq {
			return model, nil
		}
		if value.err != nil {
			model.err = value.err
			return model, tea.Quit
		}
		model.rows = appendTopRow(model.rows, formatTopRow(value.snapshot.TakenAt, calculateRate(model.previous, value.snapshot), model.limits))
		model.previous = value.snapshot
		model.view.SetContent(strings.Join(model.rows, "\n"))
		model.view.GotoBottom()
		if model.paused {
			return model, nil
		}
		return model, model.tick()
	case tea.WindowSizeMsg:
		model.view.SetWidth(value.Width)
		model.view.SetHeight(max(1, value.Height-topDashboardFixedLines))
		model.view.GotoBottom()
		return model, nil
	case tea.KeyPressMsg:
		return model.updateKey(value)
	}
	var cmd tea.Cmd
	model.view, cmd = model.view.Update(msg)
	return model, cmd
}

func (model topModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "p":
		model.paused = !model.paused
		model.seq++
		if model.paused {
			return model, nil
		}
		return model, model.tick()
	case "+", "=":
		return model.withInterval(nextTopInterval(model.interval, 1))
	case "-", "_":
		return model.withInterval(nextTopInterval(model.interval, -1))
	}
	var cmd tea.Cmd
	model.view, cmd = model.view.Update(key)
	return model, cmd
}

func (model topModel) withInterval(interval time.Duration) (tea.Model, tea.Cmd) {
	if interval == model.interval {
		return model, nil
	}
	model.interval = interval
	// 낡은 tick이 새 간격을 덮어쓰지 않게 순번을 올린다.
	model.seq++
	if model.paused {
		return model, nil
	}
	return model, model.tick()
}

// nextTopInterval은 ladder에서 한 칸 옮긴다. CLI로 받은 값이 ladder에 없으면 가장 가까운 자리에 끼워 넣는다.
func nextTopInterval(current time.Duration, step int) time.Duration {
	ladder := append([]time.Duration{}, topIntervalLadder...)
	index := sort.Search(len(ladder), func(i int) bool { return ladder[i] >= current })
	if index == len(ladder) || ladder[index] != current {
		ladder = append(ladder, current)
		sort.Slice(ladder, func(i, j int) bool { return ladder[i] < ladder[j] })
		index = sort.Search(len(ladder), func(i int) bool { return ladder[i] >= current })
	}
	next := index + step
	if next < 0 {
		next = 0
	}
	if next >= len(ladder) {
		next = len(ladder) - 1
	}
	return ladder[next]
}

func appendTopRow(rows []string, row string) []string {
	rows = append(rows, row)
	if len(rows) > topDashboardHistory {
		rows = rows[len(rows)-topDashboardHistory:]
	}
	return rows
}

func (model topModel) View() tea.View {
	view := tea.NewView(formatTopHeader(model.details) + model.view.View() + "\n" + model.statusLine())
	view.AltScreen = true
	return view
}

func (model topModel) statusLine() string {
	state := "interval " + model.interval.String()
	if model.paused {
		state = "일시정지  ·  " + state
	}
	return fmt.Sprintf("%s  ·  q 종료  p 일시정지  +/- interval", state)
}
