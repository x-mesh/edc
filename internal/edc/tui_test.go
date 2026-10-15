package edc

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type stubModel struct {
	messages []string
	quitOn   string
}

type stubMsg struct{ name string }

func (model stubModel) Init() tea.Cmd { return nil }

func (model stubModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	value, ok := msg.(stubMsg)
	if !ok {
		return model, nil
	}
	model.messages = append(model.messages, value.name)
	if value.name == model.quitOn {
		return model, tea.Quit
	}
	return model, nil
}

func (model stubModel) View() tea.View { return tea.NewView(strings.Join(model.messages, ",") + "\n") }

func headlessOptions() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithoutRenderer(), tea.WithInput(nil), tea.WithoutSignalHandler()}
}

func TestStartLiveProgramSendsAndFinishes(t *testing.T) {
	exited := make(chan struct{})
	live, err := startLiveProgram(stubModel{quitOn: "done"}, func() { close(exited) }, headlessOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	live.send(stubMsg{name: "first"})
	live.send(stubMsg{name: "second"})
	model, err := live.finish(stubMsg{name: "done"})
	if err != nil {
		t.Fatal(err)
	}
	final, ok := model.(stubModel)
	if !ok {
		t.Fatalf("model = %#v", model)
	}
	if strings.Join(final.messages, ",") != "first,second,done" {
		t.Fatalf("messages = %#v", final.messages)
	}
	<-exited
	// finish 뒤의 send는 event loop가 없으므로 block하지 않아야 한다.
	live.send(stubMsg{name: "late"})
}

func TestStartLiveProgramReportsStartFailure(t *testing.T) {
	if _, err := startLiveProgram(nil, nil, headlessOptions()...); err == nil {
		t.Fatal("nil model must fail to start")
	} else if !strings.Contains(err.Error(), "InitialModel") {
		t.Fatalf("error = %v", err)
	}
}

func TestNilLiveProgramIsNoop(t *testing.T) {
	var live *liveProgram
	live.send(stubMsg{name: "ignored"})
	if model, err := live.finish(stubMsg{name: "ignored"}); model != nil || err != nil {
		t.Fatalf("nil program returned %#v, %v", model, err)
	}
}

func TestLiveTerminalRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if liveTerminal() {
		t.Fatal("NO_COLOR must disable the live screen")
	}
}

func TestLiveCellKeepsColorAndWidth(t *testing.T) {
	colored := terminalStatus(StatusPass, true)
	cell := liveCell(colored, 10)
	if !strings.Contains(cell, richExpect("\033[32m")) {
		t.Fatalf("padding dropped the color: %q", cell)
	}
	if liveWidth(cell) != 10 {
		t.Fatalf("cell width = %d, want 10", liveWidth(cell))
	}
	if liveWidth(colored) != 4 {
		t.Fatalf("colored status width = %d, want 4", liveWidth(colored))
	}
}

// liveChipBase는 liveSelected가 줄 앞과 reset 뒤마다 거는 굵은 글씨와 chip 바탕이다.
func liveChipBase() string {
	return "\033[1;" + richRGBCode(richChipBackground, true, richTruecolor()) + "m"
}

func TestLiveSelectedKeepsTheChipAcrossCells(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "")
	row := liveSelectedBar + " " + richDim("10:00") + " web"
	got := liveSelected(row, true)
	want := liveChipBase() + "\033[" + richFixedColor(richMagenta, true) + "m" + liveSelectedBar + "\033[39m" +
		" " + strings.TrimSuffix(richDim("10:00"), richReset) + richReset + liveChipBase() + " web" + richReset
	if got != want {
		t.Fatalf("selected = %q, want %q", got, want)
	}
	if liveWidth(got) != liveWidth(row) {
		t.Fatalf("width = %d, want %d", liveWidth(got), liveWidth(row))
	}
	if liveSelected(row, false) != row {
		t.Fatal("plain selection must not change")
	}
}

func TestLiveMutedRepaintsAfterInnerResets(t *testing.T) {
	t.Setenv("COLORTERM", "")
	t.Setenv("NO_COLOR", "")
	if got := liveMuted("q quit", true); got != richDim("q quit") {
		t.Fatalf("muted = %q", got)
	}
	gray := strings.TrimSuffix(richDim("x"), "x"+richReset)
	if got := liveMuted("a"+richBold("b")+"c", true); got != gray+"a"+strings.TrimSuffix(richBold("b"), richReset)+richReset+gray+"c"+richReset {
		t.Fatalf("muted = %q", got)
	}
	if liveMuted("q quit", false) != "q quit" {
		t.Fatal("plain hint must not change")
	}
}

func TestLiveKeyHintsPaintsKeysOnly(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "")
	key := func(text string) string { return richPaint(text, richCyan, true) }
	for text, want := range map[string]string{
		"←/→ move   Enter select": key("←/→") + richDim(" move") + richDim("   ") + key("Enter") + richDim(" select"),
		"↑↓ select · 3 shown":     key("↑↓") + richDim(" select") + richDim(" · ") + richDim("3 shown"),
		"  ·  f filter, q quit":   richDim("  ·  ") + key("f") + richDim(" filter") + richDim(", ") + key("q") + richDim(" quit"),
	} {
		got := liveKeyHints(text, true)
		if got != want {
			t.Fatalf("hints(%q) = %q, want %q", text, got, want)
		}
		if ansi.Strip(got) != text {
			t.Fatalf("hints(%q) changed the text: %q", text, ansi.Strip(got))
		}
	}
	if liveKeyHints("q quit", false) != "q quit" {
		t.Fatal("plain hint must not change")
	}
	title := "Select the group   ↑/↓ move"
	if got := liveTitleHints(title, true); got != richBold("Select the group")+liveKeyHints("   ↑/↓ move", true) {
		t.Fatalf("title = %q", got)
	}
}

// 선택기와 확인 화면은 NO_COLOR에서도 color를 켠 채 뜬다. 그때는 예전처럼 반전과 흐림만 쓰고 팔레트 색을 내지 않는다.
func TestLiveHelpersKeepTheMonochromeOutputUnderNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	row := liveSelectedBar + " daily "
	if got := liveSelected(row, true); got != liveReverse+row+liveReset {
		t.Fatalf("selected = %q", got)
	}
	if got := liveMuted("q quit", true); got != liveDim+"q quit"+liveReset {
		t.Fatalf("muted = %q", got)
	}
	if got := liveKeyHints("q quit", true); got != liveDim+"q quit"+liveReset {
		t.Fatalf("hints = %q", got)
	}
	if got := liveTitleHints("Select   q cancel", true); got != "Select   q cancel" {
		t.Fatalf("title = %q", got)
	}
	if got := confirmPrompt("Run?", false, true); strings.Contains(got, "\033[38") || strings.Contains(got, "\033[48") {
		t.Fatalf("confirm prompt carries palette colors: %q", got)
	}
}

// 언어팩의 키 안내는 묶음마다 키로 시작해야 키가 청록으로 칠해진다. 새 키 이름이 생기면 liveHintKeys에 더한다.
func TestLocaleKeyHintsStartWithKnownKeys(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	hints := []string{"ui.confirm.help", "cli.report.pick_help", "cli.report.viewer_help", "cli.log_history.list_help", "cli.log_history.detail_help", "cli.log_history.expanded_help"}
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		texts := []string{}
		for _, key := range hints {
			texts = append(texts, T(key))
		}
		for _, key := range []string{selectGroupTitle, selectInventoryTitle, selectRecipeTitle} {
			_, rest, _ := strings.Cut(T(key), "   ")
			texts = append(texts, rest)
		}
		for _, text := range texts {
			for _, group := range liveHintSeparator.Split(text, -1) {
				if group = strings.TrimSpace(group); group == "" {
					continue
				}
				if key, _, _ := strings.Cut(group, " "); !liveHintKey(key) {
					t.Errorf("%s: %q starts with %q, not a key", language, text, key)
				}
			}
		}
	}
}
