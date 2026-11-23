package edc

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// listenRowFixture는 이 호스트에서 한 포트가 여러 줄로 보이던 모양이다. 3001/tcp는 tailscaled 두 소켓 사이에 reviewer가 끼어 있다.
var listenRowFixture = []listenSocket{
	{Proto: "tcp", Address: "0.0.0.0", Port: 22, Process: "sshd", PID: "10"},
	{Proto: "tcp", Address: "::", Port: 22, Process: "sshd", PID: "10"},
	{Proto: "tcp", Address: "127.0.0.53", Port: 53, Process: "systemd-resolve", PID: "20"},
	{Proto: "tcp", Address: "127.0.0.54", Port: 53, Process: "systemd-resolve", PID: "20"},
	{Proto: "tcp", Address: "100.64.0.1", Port: 3001, Process: "tailscaled", PID: "30"},
	{Proto: "tcp", Address: "127.0.0.1", Port: 3001, Process: "reviewer", PID: "40"},
	{Proto: "tcp", Address: "fd7a:115c:a1e0::1", Port: 3001, Process: "tailscaled", PID: "30"},
	{Proto: "tcp", Address: "0.0.0.0", Port: 3737, Process: "docker-proxy", PID: "50"},
	{Proto: "tcp", Address: "::", Port: 3737, Process: "docker-proxy", PID: "51"},
	{Proto: "tcp", Address: "127.0.0.1", Port: 8080, Process: "api", PID: "60"},
	{Proto: "tcp", Address: "0.0.0.0", Port: 8080, Process: "api", PID: "60"},
	{Proto: "unix", Address: "/run/app.sock", Process: "app", PID: "70"},
	{Proto: "unix", Address: "/run/app.sock", Process: "app", PID: "71"},
}

func TestGroupListenSocketsMergesAddressesOfOneScope(t *testing.T) {
	rows := groupListenSockets(listenRowFixture)
	var got []string
	for _, row := range rows {
		got = append(got, row.first().Process+" "+strings.Join(row.addresses(), ",")+" "+strings.Join(row.pids(), ","))
	}
	want := []string{
		"sshd 0.0.0.0,:: 10",
		"systemd-resolve 127.0.0.53,127.0.0.54 20",
		"tailscaled 100.64.0.1,fd7a:115c:a1e0::1 30",
		"reviewer 127.0.0.1 40",
		"docker-proxy 0.0.0.0,:: 50,51",
		// 같은 포트와 process라도 loopback과 모든 주소는 범위가 달라 한 줄로 모으지 않는다.
		"api 127.0.0.1 60",
		"api 0.0.0.0 60",
		// 유닉스 소켓은 모으지 않는다.
		"app /run/app.sock 70",
		"app /run/app.sock 71",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows:\n%s", strings.Join(got, "\n"))
	}
}

func TestGroupListenSocketsKeepsContainersApart(t *testing.T) {
	first := listenSocket{Proto: "tcp", Address: "0.0.0.0", Port: 80, Process: "docker-proxy", PID: "1", Container: &listenContainer{Name: "web"}}
	second := first
	second.Address, second.PID, second.Container = "::", "2", &listenContainer{Name: "web"}
	other := second
	other.Container = &listenContainer{Name: "other"}
	if rows := groupListenSockets([]listenSocket{first, second}); len(rows) != 1 {
		t.Fatalf("same container split into %d rows", len(rows))
	}
	if rows := groupListenSockets([]listenSocket{first, other}); len(rows) != 2 {
		t.Fatalf("different containers merged into %d rows", len(rows))
	}
}

func TestFormatListenTableMergesRowsAndKeepsDetailRowCount(t *testing.T) {
	plain := formatListenTable(listenRowFixture, false, textStyle{}, nil)
	detail := formatListenTable(listenRowFixture, true, textStyle{}, nil)
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	rows := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, " ") {
			rows++
		}
	}
	if rows != 10 {
		t.Fatalf("rows = %d:\n%s", rows, plain)
	}
	if len(strings.Split(strings.TrimRight(detail, "\n"), "\n")) != len(lines) {
		t.Fatalf("-v changed the row count:\n%s", detail)
	}
	// ADDRESS 칸은 가장 긴 주소 하나(fd7a:115c:a1e0::1)의 폭이다. 넘치는 주소는 같은 칸의 아랫줄로 간다.
	addressAt := strings.Index(lines[0], T("observe.listen.column.address"))
	for _, text := range []string{"100.64.0.1,", "fd7a:115c:a1e0::1", "127.0.0.53,", "127.0.0.54"} {
		found := false
		for _, line := range lines {
			if strings.Index(line, text) == addressAt && strings.TrimSpace(line[addressAt+len(text):]) == "" || strings.Contains(line, "  "+text+" ") {
				found = true
			}
		}
		if !found {
			t.Fatalf("address %q is not alone in the ADDRESS column:\n%s", text, plain)
		}
	}
	for _, text := range []string{"0.0.0.0, ::", "docker-proxy (50, 51)"} {
		if !strings.Contains(plain, text) {
			t.Fatalf("missing %q:\n%s", text, plain)
		}
	}
	// 색이 없을 때도 SCOPE 칸이 범위를 글자로 남긴다.
	if !strings.Contains(plain, T("observe.listen.scope.loopback")) || strings.Contains(plain, "\x1b") {
		t.Fatalf("plain table:\n%s", plain)
	}
}

func TestFormatListenTablePaintsScopeAndRedactsAfterPainting(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	style := textStyle{color: true}
	table := formatListenTable(listenRowFixture, false, style, nil)
	if !strings.Contains(table, style.paint("0.0.0.0, ::", richYellow)) || !strings.Contains(table, style.paint("127.0.0.1", richGray)) {
		t.Fatalf("scope colors missing: %q", table)
	}
	if strings.Contains(ansi.Strip(table), "\x1b") {
		t.Fatal("strip left escapes")
	}
	redacted := redactIPAddresses(table)
	for _, address := range []string{"0.0.0.0", "127.0.0.53", "100.64.0.1", "fd7a:115c:a1e0::1"} {
		if strings.Contains(redacted, address) {
			t.Fatalf("redaction missed %s next to color codes: %q", address, redacted)
		}
	}
}

func TestPrintInfoListenCountsSocketsAndListsRows(t *testing.T) {
	var output bytes.Buffer
	printInfoListen(&output, Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": listenRowFixture[:11]}}, false, false, false)
	text := output.String()
	// 개수는 소켓 단위다. 노출된 소켓 7개 가운데 줄은 4개다.
	if !strings.Contains(text, "TCP 11") || !strings.Contains(text, "Loopback 4") {
		t.Fatalf("socket counts:\n%s", text)
	}
	for _, line := range []string{"0.0.0.0, ::", "sshd (10)", "docker-proxy (50, 51)", "100.64.0.1 +1"} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q:\n%s", line, text)
		}
	}
	if strings.Contains(text, "more") {
		t.Fatalf("four rows fit under the row limit:\n%s", text)
	}
}

func listenRowBrowser(t *testing.T) listenBrowser {
	t.Helper()
	model := newListenBrowser(context.Background(), time.Second, false, false)
	model.load = func(context.Context) Result {
		return Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": listenRowFixture[:11]}}
	}
	next, _ := model.Update(model.Init()())
	next, _ = next.(listenBrowser).Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	return next.(listenBrowser)
}

func TestListenBrowserShowsMergedAddressesAndPIDs(t *testing.T) {
	model := listenRowBrowser(t)
	text := model.View().Content
	for _, want := range []string{"ADDRESS", "0.0.0.0, ::", "50 +1", "11 sockets", "7 rows"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "BINDING") || strings.Contains(text, T("observe.listen.scope.all")) {
		t.Fatalf("scope words remain without color:\n%s", text)
	}
}

func TestListenBrowserLoopbackToggleKeepsSelection(t *testing.T) {
	model := listenRowBrowser(t)
	for model.items[model.cursor].first().Process != "docker-proxy" {
		model, _ = listenBrowserKey(model, "down")
	}
	model, cmd := listenBrowserKey(model, "l")
	if cmd != nil || !model.hideLoopback {
		t.Fatal("l must toggle loopback locally")
	}
	for _, row := range model.items {
		if row.scope() == "loopback" {
			t.Fatalf("loopback row shown: %v", row.addresses())
		}
	}
	if len(model.items) != 4 || model.items[model.cursor].first().Process != "docker-proxy" {
		t.Fatalf("selection lost: %d rows, cursor %d", len(model.items), model.cursor)
	}
	if text := model.View().Content; !strings.Contains(text, T("listenbrowse.loopback_hidden")) {
		t.Fatalf("hidden state missing:\n%s", text)
	}
	// 새로고침도 숨김을 유지한다.
	next, _ := model.Update(listenBrowserListMsg{seq: model.seq, result: model.load(context.Background()), at: time.Now()})
	model = next.(listenBrowser)
	if len(model.items) != 4 || model.items[model.cursor].first().Process != "docker-proxy" {
		t.Fatal("refresh dropped the loopback filter or the selection")
	}
	model, _ = listenBrowserKey(model, "l")
	if len(model.items) != 7 || model.items[model.cursor].first().Process != "docker-proxy" {
		t.Fatal("showing loopback again lost the selection")
	}
}

func TestListenBrowserRedactsColoredAddressesAtNarrowWidth(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	model := listenRowBrowser(t)
	model.redact, model.color = true, true
	for _, width := range []int{48, 60, 100} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		model = next.(listenBrowser)
		text := ansi.Strip(model.View().Content)
		for _, address := range []string{"0.0.0.0", "127.0.0.53", "100.64.0.1", "fd7a", "::"} {
			if strings.Contains(text, address) {
				t.Fatalf("width %d leaked %s:\n%s", width, address, text)
			}
		}
		for _, line := range strings.Split(model.View().Content, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d exceeded: %q", width, line)
			}
		}
	}
}

func TestListenBrowserDetailListsEverySocket(t *testing.T) {
	row := groupListenSockets(listenRowFixture[7:9])[0]
	// 끝난 context로 읽으면 실제 PID를 읽지 않는다. 상세의 소켓 줄만 본다.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	text := strings.Join(inspectListenSocket(ctx, row, false), "\n")
	for _, want := range []string{"0.0.0.0, ::", "50, 51", T("listenbrowse.socket", "::", "51", "-")} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

func TestListenBrowserColorsLegendPortsAndContainers(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	sockets := append([]listenSocket{}, listenRowFixture...)
	sockets[7].Container = &listenContainer{Name: "web"}
	sockets[8].Container = &listenContainer{Name: "web"}
	model := newListenBrowser(context.Background(), time.Second, true, false)
	model.load = func(context.Context) Result {
		return Result{Status: StatusPass, Warnings: []string{"engine unreachable"}, Metrics: map[string]interface{}{"sockets": sockets}}
	}
	next, _ := model.Update(model.Init()())
	next, _ = next.(listenBrowser).Update(tea.WindowSizeMsg{Width: 88, Height: 20})
	model = next.(listenBrowser)
	text := model.View().Content
	// 경고가 안내 줄을 차지해도 범례는 색을 가진 채 남는다.
	for _, want := range []string{richPaint("●", richYellow, false), richPaint("●", richCyan, false), "engine unreachable", richPaint(" → web", richCyan, false), richPaint("0.0.0.0, ::", richYellow, false), "127.0.0.53, 127.0.0.54"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	// 색이 없는 범위(unix)에는 빈 색 코드를 넣지 않는다.
	if strings.Contains(text, "\x1b[m") || strings.Contains(text, "\x1b[38;5;m") {
		t.Fatalf("empty color code:\n%q", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if ansi.StringWidth(line) > 88 {
			t.Fatalf("width exceeded: %q", line)
		}
	}
}
