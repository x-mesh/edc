package edc

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// listenFilterFixture는 서로 다른 프로세스·범위·컨테이너를 가진 소켓 넷이 있는 브라우저다.
func listenFilterFixture(t *testing.T) listenBrowser {
	t.Helper()
	model := newListenBrowser(context.Background(), time.Second, false, false)
	model.load = func(context.Context) Result {
		return Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": []listenSocket{
			{Proto: "tcp", Port: 8080, Address: "0.0.0.0", Process: "api-server", PID: "100"},
			{Proto: "tcp", Port: 5432, Address: "127.0.0.1", Process: "postgres", PID: "200"},
			{Proto: "udp", Port: 5353, Address: "192.0.2.7", Process: "mDNSResponder", PID: "300"},
			{Proto: "tcp", Port: 443, Address: "0.0.0.0", Process: "nginx", PID: "4100"},
		}}}
	}
	next, _ := model.Update(model.Init()())
	return next.(listenBrowser)
}

// listenFilterType는 키를 하나씩 누른 것처럼 글자를 보낸다.
func listenFilterType(model listenBrowser, text string) listenBrowser {
	for _, character := range text {
		model, _ = listenBrowserKey(model, string(character))
	}
	return model
}

func listenFilterProcesses(model listenBrowser) []string {
	var names []string
	for _, row := range model.items {
		names = append(names, row.first().Process)
	}
	return names
}

func TestListenFilterNarrowsRowsWhileTyping(t *testing.T) {
	model := listenFilterFixture(t)
	if len(model.items) != 4 {
		t.Fatalf("fixture has %d rows", len(model.items))
	}
	model, _ = listenBrowserKey(model, "/")
	if !model.filtering {
		t.Fatal("/ did not start the filter")
	}
	for text, want := range map[string][]string{
		"ngi":         {"nginx"},
		"NGIN":        {"nginx"},    // 대소문자를 가리지 않는다
		"5432":        {"postgres"}, // 포트
		"udp":         {"mDNSResponder"},
		"192.0.2":     {"mDNSResponder"},       // 주소
		"4100":        {"nginx"},               // PID
		"tcp 0.0.0.0": {"api-server", "nginx"}, // 낱말은 모두 맞아야 한다
		"loopback":    {"postgres"},            // 범위
		"nothing":     nil,
	} {
		model.filter = ""
		model.applyFilter()
		model = listenFilterType(model, text)
		if got := listenFilterProcesses(model); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("filter %q: got %v, want %v", text, got, want)
		}
	}
}

func TestListenFilterMatchesTheContainer(t *testing.T) {
	model := listenFilterFixture(t)
	model.sockets[0].Container = &listenContainer{Name: "web-shop", Image: "nginx:1"}
	model.rebuild()
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "web-shop")
	if got := listenFilterProcesses(model); len(got) != 1 || got[0] != "api-server" {
		t.Fatalf("container filter: %v", got)
	}
}

func TestListenFilterKeysWhileTyping(t *testing.T) {
	model := listenFilterFixture(t)
	model, _ = listenBrowserKey(model, "/")
	// q는 글자이지 끝내는 키가 아니다. Ctrl-C만 끝낸다.
	model, quit := listenBrowserKey(model, "q")
	if quit != nil || model.filter != "q" {
		t.Fatalf("q while typing: filter %q, quit %v", model.filter, quit != nil)
	}
	if _, quit = listenBrowserKey(model, "ctrl+c"); quit == nil {
		t.Fatal("ctrl+c did not quit while typing")
	}
	model, _ = listenBrowserKey(model, "backspace")
	if model.filter != "" || len(model.items) != 4 {
		t.Fatalf("backspace: filter %q, %d rows", model.filter, len(model.items))
	}
	// 위아래는 입력 중에도 고른 줄을 옮긴다.
	model, _ = listenBrowserKey(model, "down")
	model, _ = listenBrowserKey(model, "down")
	if model.cursor != 2 || !model.filtering {
		t.Fatalf("down while typing: cursor %d, filtering %v", model.cursor, model.filtering)
	}
	// Enter는 필터를 두고 입력만 끝낸다. 그 뒤의 q는 끝내는 키다.
	model = listenFilterType(model, "tcp")
	model, _ = listenBrowserKey(model, "enter")
	if model.filtering || model.filter != "tcp" || len(model.items) != 3 {
		t.Fatalf("enter: filtering %v, filter %q, %d rows", model.filtering, model.filter, len(model.items))
	}
	if _, quit = listenBrowserKey(model, "q"); quit == nil {
		t.Fatal("q did not quit after the filter was accepted")
	}
}

func TestListenFilterEscClears(t *testing.T) {
	model := listenFilterFixture(t)
	// 입력 중 Esc는 필터를 지우고 입력도 끝낸다.
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "nginx")
	model, _ = listenBrowserKey(model, "esc")
	if model.filtering || model.filter != "" || len(model.items) != 4 {
		t.Fatalf("esc while typing: filtering %v, filter %q, %d rows", model.filtering, model.filter, len(model.items))
	}
	// 입력을 끝낸 뒤의 Esc도 필터를 지운다. 필터가 없을 때 Esc는 아무것도 하지 않는다.
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "nginx")
	model, _ = listenBrowserKey(model, "enter")
	model, _ = listenBrowserKey(model, "esc")
	if model.filter != "" || len(model.items) != 4 {
		t.Fatalf("esc after enter: filter %q, %d rows", model.filter, len(model.items))
	}
	if next, _ := listenBrowserKey(model, "esc"); next.filter != "" || len(next.items) != 4 || next.detail != nil {
		t.Fatal("esc without a filter changed the list")
	}
}

// 필터는 새로고침과 loopback 숨김과 함께 걸리고, 고른 줄이 남으면 그 줄에 머문다.
func TestListenFilterSurvivesRefreshAndKeepsTheSelection(t *testing.T) {
	model := listenFilterFixture(t)
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "tcp")
	model, _ = listenBrowserKey(model, "enter")
	model, _ = listenBrowserKey(model, "down")
	model, _ = listenBrowserKey(model, "down")
	if selected, _ := model.selected(); selected.Process != "nginx" {
		t.Fatalf("selected %q", selected.Process)
	}
	// 더 좁혀도 고른 줄이 맞으면 그 줄에 머문다.
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, " 443")
	if selected, ok := model.selected(); !ok || selected.Process != "nginx" || model.cursor != 0 {
		t.Fatalf("selection after narrowing: %q cursor %d", selected.Process, model.cursor)
	}
	model, _ = listenBrowserKey(model, "enter")
	model, reload := listenBrowserKey(model, "r")
	next, _ := model.Update(reload())
	model = next.(listenBrowser)
	if model.filter != "tcp 443" || len(model.items) != 1 {
		t.Fatalf("refresh dropped the filter: %q, %d rows", model.filter, len(model.items))
	}
	// loopback 숨김과 필터는 함께 걸린다.
	model.filter = "tcp"
	model.applyFilter()
	model, _ = listenBrowserKey(model, "l")
	if got := listenFilterProcesses(model); strings.Join(got, ",") != "api-server,nginx" {
		t.Fatalf("filter with loopback hidden: %v", got)
	}
	// 고른 줄이 걸러지면 맨 위로 돌아간다.
	model, _ = listenBrowserKey(model, "end")
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, " api")
	if selected, ok := model.selected(); !ok || selected.Process != "api-server" || model.cursor != 0 {
		t.Fatalf("selection after the row was filtered out: %q cursor %d", selected.Process, model.cursor)
	}
}

func TestListenFilterIsIgnoredInDetails(t *testing.T) {
	model := listenFilterFixture(t)
	model.inspect = func(context.Context, listenRow) []string { return []string{"detail"} }
	model, cmd := listenBrowserKey(model, "enter")
	next, _ := model.Update(cmd())
	model = next.(listenBrowser)
	model, _ = listenBrowserKey(model, "/")
	if model.filtering || model.detail == nil {
		t.Fatalf("/ in details: filtering %v, detail %v", model.filtering, model.detail != nil)
	}
}

// --redact는 화면에 보이는 글자로 맞춘다. 가린 주소를 필터로 짚어 낼 수 없다.
func TestListenFilterMatchesWhatTheScreenShows(t *testing.T) {
	model := listenFilterFixture(t)
	model.redact = true
	model.rebuild()
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "192.0.2.7")
	if len(model.items) != 0 {
		t.Fatalf("redacted address was matched: %v", listenFilterProcesses(model))
	}
	model.filter = ""
	model.applyFilter()
	model = listenFilterType(model, "mdns")
	if len(model.items) != 1 {
		t.Fatalf("process was not matched under --redact: %v", listenFilterProcesses(model))
	}
}

func TestListenFilterScreen(t *testing.T) {
	model := listenFilterFixture(t)
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "nginx")
	typing := model.View().Content
	for _, want := range []string{"filter: nginx█", "Enter done", "Esc clear", "filter nginx", "1 rows"} {
		if !strings.Contains(typing, want) {
			t.Errorf("typing screen lacks %q:\n%s", want, typing)
		}
	}
	if strings.Contains(typing, "api-server") {
		t.Errorf("typing screen shows a filtered-out row:\n%s", typing)
	}
	model, _ = listenBrowserKey(model, "enter")
	accepted := model.View().Content
	for _, want := range []string{"/ filter", "Esc clear", "filter nginx", "4 sockets", "1 rows"} {
		if !strings.Contains(accepted, want) {
			t.Errorf("accepted screen lacks %q:\n%s", want, accepted)
		}
	}
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "zzz")
	empty := model.View().Content
	if !strings.Contains(empty, "No sockets match the filter") || strings.Contains(empty, "No matching sockets. Press r") {
		t.Errorf("empty filtered screen:\n%s", empty)
	}
	// 필터가 없을 때의 빈 목록은 원래 문구를 쓴다.
	empty = func() string {
		blank := listenFilterFixture(t)
		blank.sockets, blank.items = nil, nil
		return blank.View().Content
	}()
	if strings.Contains(empty, "No sockets match the filter") {
		t.Errorf("empty list without a filter blames the filter:\n%s", empty)
	}
}

// 필터가 켜진 키 안내 줄도 80열에서 q가 잘리지 않는다. 모든 언어에서 확인한다.
func TestListenFilterKeyLineKeepsQuitVisibleAt80Columns(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		model := listenFilterFixture(t)
		next, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		model = next.(listenBrowser)
		model, _ = listenBrowserKey(model, "/")
		model = listenFilterType(model, "tcp")
		model, _ = listenBrowserKey(model, "enter")
		line := strings.Split(model.View().Content, "\n")[1]
		var quit, clear string
		for _, group := range liveHintSeparator.Split(T("listenbrowse.keys_filtered"), -1) {
			switch {
			case strings.HasPrefix(group, "q "):
				quit = group
			case strings.HasPrefix(group, "Esc "):
				clear = group
			}
		}
		if quit == "" || clear == "" || !strings.Contains(line, quit) || !strings.Contains(line, clear) {
			t.Errorf("%s: %q or %q is cut at 80 columns: %q", language, quit, clear, line)
		}
	}
}

// 색이 있을 때 켜진 필터는 loopback 숨김처럼 켜진 키로 칠한다. 글자는 색이 없어도 같다.
func TestListenFilterHintIsHighlightedWhenOn(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	model := listenFilterFixture(t)
	model.color = true
	off := model.View().Content
	model, _ = listenBrowserKey(model, "/")
	model = listenFilterType(model, "nginx")
	model, _ = listenBrowserKey(model, "enter")
	on := model.View().Content
	if !strings.Contains(on, "\x1b[") || off == on {
		t.Fatal("accepted filter does not change the colored screen")
	}
	if strip := ansi.Strip(on); !strings.Contains(strip, "/ filter") {
		t.Fatalf("colored screen lost the key hint: %q", strip)
	}
}

// 키가 눌린 글자가 없는 키(방향키가 아닌 기능키)는 필터에 들어가지 않는다.
func TestListenFilterIgnoresKeysWithoutText(t *testing.T) {
	model := listenFilterFixture(t)
	model, _ = listenBrowserKey(model, "/")
	next, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyF5})
	if got := next.(listenBrowser).filter; got != "" {
		t.Fatalf("a key with no text changed the filter to %q", got)
	}
}
