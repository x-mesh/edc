package edc

import (
	"strings"
	"testing"
	"time"
)

func parseViewLines(raw []string, format viewFormat) []viewLine {
	lines := make([]viewLine, 0, len(raw))
	var previous *viewLine
	for _, text := range raw {
		lines = append(lines, parseViewLine(text, format, previous))
		previous = &lines[len(lines)-1]
	}
	return lines
}

func TestViewFilterMatchesFieldsTextAndExclusions(t *testing.T) {
	lines := parseViewLines(readViewFixture(t, "access.log"), viewFormatAccess)
	rpc := parseViewLine(`127.0.0.1 127.0.0.1  [28/Aug/2026:10:54:19 +0900] POST /api/v3 HTTP/1.1 400 106  "curl/7.81.0"  {"method":"icx_sendTransaction"} HTTP/1.1 0.005`, viewFormatAccess, nil)
	lines = append(lines, rpc)
	for _, test := range []struct {
		filter string
		want   int
	}{
		{"", 7},
		{"users", 3},
		{"USERS", 3},
		{"path:/api/users", 3},
		// 필드는 전체 일치라 /api/orders?id=7은 path:/api/orders에 걸리지 않는다.
		{"path:/api/orders", 0},
		{"path:/api/*", 6},
		{"path:*.js", 1},
		{"path:/API/Users", 3},
		{`path:"/api/v3 icx_sendTransaction"`, 1},
		{"rpc:icx_sendtransaction", 1},
		{"status:5xx", 1},
		{"status:4xx", 2},
		{"status:404", 1},
		{"client:198.51.100.4", 2},
		{"client:192.0.*", 2},
		{"method:post", 2},
		{"path:/api/* -users", 3},
		{"-path:/api/users", 4},
		{"-status:2xx -status:3xx", 3},
		{"users curl", 2},
		{`"10:00:01"`, 1},
		// 모르는 필드 이름은 글자로 찾는다. 시각의 콜론이 필드로 읽히지 않는다.
		{"10:00:01", 1},
		{`"-users"`, 0},
		{"-", 6},
	} {
		filter, err := parseViewFilter(test.filter)
		if err != nil {
			t.Fatalf("%q: %v", test.filter, err)
		}
		count := 0
		for _, line := range lines {
			if filter.matches(line) {
				count++
			}
		}
		if count != test.want {
			t.Errorf("%q matched %d lines, want %d", test.filter, count, test.want)
		}
	}
}

func TestViewFilterRejectsBadInput(t *testing.T) {
	for _, test := range []struct{ filter, want string }{
		{`path:"/api`, "quote is not closed"},
		{"status:abc", "status:abc"},
		{"status:6xx", "status:6xx"},
		{"path:", "needs a value"},
	} {
		if _, err := parseViewFilter(test.filter); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: error %v, want %q", test.filter, err, test.want)
		}
	}
}

func TestViewFilterMatchesLevelAndProgram(t *testing.T) {
	lines := parseViewLines(readViewFixture(t, "auth.log"), viewFormatSyslog)
	for _, test := range []struct {
		filter string
		want   int
	}{
		{"level:error", 2},
		{"program:sshd-session", 5},
		{"program:sshd* -level:error", 3},
	} {
		filter, err := parseViewFilter(test.filter)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, line := range lines {
			if filter.matches(line) {
				count++
			}
		}
		if count != test.want {
			t.Errorf("%q matched %d lines, want %d", test.filter, count, test.want)
		}
	}
}

// 잘못된 filter는 Enter로 적용하지 않고, 입력 칸 옆에 오류를 보인다. 글자를 고치면 오류가 사라진다.
func TestViewScreenKeepsBadFilterOpen(t *testing.T) {
	model := viewScreenWith(t, "access.log", 140, 30)
	model = viewPress(model, "/", "s", "t", "a", "t", "u", "s", ":", "x", "enter")
	if !model.filtering || model.filter != "" || !strings.Contains(strings.Join(model.header(false), "\n"), "use a class such as 5xx") {
		t.Fatalf("bad filter: filtering %v filter %q err %q", model.filtering, model.filter, model.filterErr)
	}
	model = viewPress(model, "x")
	if model.filterErr != "" {
		t.Fatal("typing clears the error")
	}
	model = viewPress(model, "esc", "/")
	if model.filterErr != "" || model.input.Value() != "" {
		t.Fatalf("esc drops the bad filter: %q %q", model.filterErr, model.input.Value())
	}
}

func TestViewFilterWithReplacesTheSameField(t *testing.T) {
	for _, test := range []struct{ filter, field, value, want string }{
		{"", "path", "/api/users", "path:/api/users"},
		{"score", "path", "/api/users", "score path:/api/users"},
		{"score path:/api/users -path:/health", "path", "/api/orders", "score -path:/health path:/api/orders"},
		{`path:"/a b" status:5xx`, "client", "192.0.2.1", `path:"/a b" status:5xx client:192.0.2.1`},
		{"", "path", "/api/v3 icx_sendTransaction", `path:"/api/v3 icx_sendTransaction"`},
		{"", "path", `/q?x="1"`, "path:/q?x=*1*"},
	} {
		got := viewFilterWith(test.filter, test.field, test.value)
		if got != test.want {
			t.Errorf("%q + %s:%s = %q, want %q", test.filter, test.field, test.value, got, test.want)
		}
		if _, err := parseViewFilter(got); err != nil {
			t.Errorf("%q does not parse: %v", got, err)
		}
	}
}

// tab으로 통계 패널에 들어가 항목을 Enter로 filter에 더한다. 패널에 남아 이어서 좁힐 수 있다.
func TestViewScreenPicksAPanelEntryAsFilter(t *testing.T) {
	model := viewScreenWith(t, "access.log", 140, 40)
	picks := model.panelPicks()
	// STATUS 2xx 3xx 4xx 5xx 다음에 TOP PATHS가 온다. 가장 많은 path가 /api/users다.
	if len(picks) < 5 || picks[0] != (viewPanelPick{"status", "2xx"}) || picks[4] != (viewPanelPick{"path", "/api/users"}) {
		t.Fatalf("picks %v", picks)
	}
	model = viewPress(model, "tab")
	if !model.picking || !strings.Contains(strings.Join(model.screen(false), "\n"), "▸2xx") {
		t.Fatalf("tab picks in the panel:\n%s", strings.Join(model.screen(false), "\n"))
	}
	model = viewPress(model, "down", "down", "down", "down", "enter")
	if model.filter != "path:/api/users" || !model.picking || model.panelStats().lines != 3 {
		t.Fatalf("enter adds the path: filter %q picking %v lines %d", model.filter, model.picking, model.panelStats().lines)
	}
	// 이제 STATUS는 2xx와 4xx뿐이고, 그 뒤에 path 하나, 그 뒤에 client가 온다.
	model.pick = model.panelPicks()[0]
	model = viewPress(model, "down", "down", "down", "enter")
	if model.filter != "path:/api/users client:192.0.2.77" || model.panelStats().lines != 1 {
		t.Fatalf("filter %q lines %d picks %v", model.filter, model.panelStats().lines, model.panelPicks())
	}
	model = viewPress(model, "esc")
	if model.picking || model.filter == "" {
		t.Fatalf("esc leaves the panel and keeps the filter: picking %v filter %q", model.picking, model.filter)
	}
	narrow := viewPress(viewScreenWith(t, "access.log", 90, 30), "tab", "down")
	if joined := strings.Join(narrow.screen(false), "\n"); !strings.Contains(joined, "▸3xx 2 33%") {
		t.Fatalf("narrow panel marks the pick:\n%s", joined)
	}
	if empty := viewPress(newViewScreenModel("plain", 10), "tab"); empty.picking {
		t.Fatal("a panel with nothing to pick stays in the list")
	}
}

// 패널이 화면보다 길거나 한 줄이 폭보다 길어도, 고른 항목은 늘 화면에 보인다. Enter가 보이지 않는 항목을 filter에 넣지 않는다.
func TestViewScreenKeepsThePickOnScreen(t *testing.T) {
	long := `192.0.2.9 - - [26/Oct/2026:10:00:09 +0900] "GET /api/` + strings.Repeat("x", 50) + ` HTTP/1.1" 500 1 "-" "x"`
	for _, size := range []struct{ width, height int }{{140, 24}, {140, 12}, {90, 30}, {90, 14}} {
		model := viewScreenWith(t, "access.log", size.width, size.height)
		model.ingest([]string{long, long}, time.Now(), false)
		model = viewPress(model, "tab")
		picks := model.panelPicks()
		for index := range picks {
			if got := model.pickIndex(picks); got != index {
				t.Fatalf("%dx%d: pick %d, want %d", size.width, size.height, got, index)
			}
			screen := strings.Join(model.screen(false), "\n")
			if !strings.Contains(screen, "▸") {
				t.Fatalf("%dx%d: pick %d %v is off screen:\n%s", size.width, size.height, index, picks[index], screen)
			}
			model = viewPress(model, "down")
		}
	}
}

// 새 줄이 들어와 TOP 순서가 바뀌어도 고른 항목은 그대로다.
func TestViewScreenPickFollowsTheEntry(t *testing.T) {
	model := viewPress(viewScreenWith(t, "access.log", 140, 40), "tab", "down", "down", "down", "down", "down")
	if model.pick != (viewPanelPick{"path", "/api/login"}) {
		t.Fatalf("pick %v", model.pick)
	}
	for range 5 {
		model.ingest([]string{`192.0.2.1 - - [26/Oct/2026:10:00:06 +0900] "POST /api/login HTTP/1.1" 200 1 "-" "x"`}, time.Now(), false)
	}
	if picks := model.panelPicks(); picks[model.pickIndex(picks)] != (viewPanelPick{"path", "/api/login"}) {
		t.Fatalf("the pick moved: %v", picks)
	}
	model = viewPress(model, "enter")
	if model.filter != "path:/api/login" {
		t.Fatalf("filter %q", model.filter)
	}
}

func TestViewFilterQuotedPhrase(t *testing.T) {
	line := parseViewLine("2026-10-26 10:00:00 ERROR connection refused by peer", viewFormatLevel, nil)
	other := parseViewLine("2026-10-26 10:00:01 ERROR refused connection", viewFormatLevel, nil)
	for _, test := range []struct {
		filter      string
		line, other bool
	}{{`"connection refused"`, true, false}, {"connection refused", true, true}} {
		filter, err := parseViewFilter(test.filter)
		if err != nil {
			t.Fatal(err)
		}
		if filter.matches(line) != test.line || filter.matches(other) != test.other {
			t.Errorf("%q: %v %v", test.filter, filter.matches(line), filter.matches(other))
		}
	}
}
