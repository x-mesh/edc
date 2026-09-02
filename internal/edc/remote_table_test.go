package edc

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tableFixture(width int) (remoteTable, []remoteHost, remoteRecipe) {
	hosts := []remoteHost{
		{Name: "jw-server", Tags: []string{"linux"}},
		{Name: "jinwoos-macbook-pro", Tags: []string{"mac"}},
	}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{
		{Name: "git-kit", Command: "git-kit update", Verify: "git-kit --version", Timeout: time.Minute},
		{Name: "x-mesh", Command: "xm update", Verify: "xm version", Timeout: time.Minute},
		{Name: "brew update", Command: "brew update && brew upgrade -f", Timeout: time.Minute, Tags: []string{"mac"}},
	}}
	return newRemoteTable(hosts, recipe, width), hosts, recipe
}

func TestRemoteTableUsesWordsWhenTheyFit(t *testing.T) {
	table, _, _ := tableFixture(100)
	if table.symbols {
		t.Fatal("a wide terminal must keep the status words")
	}
	header := table.header()
	if !strings.HasPrefix(header, remoteHostHeader) || !strings.Contains(header, "brew update") {
		t.Fatalf("header = %q", header)
	}
	row := table.row("jw-server", []string{terminalStatus(StatusPass, false), terminalStatus(StatusFail, false), remoteGlyphAbsent})
	if !strings.Contains(row, "PASS") || !strings.Contains(row, remoteGlyphAbsent) {
		t.Fatalf("row = %q", row)
	}
	// 열 이름과 값이 같은 자리에서 시작해야 한다.
	if column(header, "git-kit") != column(row, "PASS") {
		t.Fatalf("header %q and row %q do not share the column", header, row)
	}
}

func column(line, value string) int {
	index := strings.Index(line, value)
	if index < 0 {
		return -1
	}
	return liveWidth(line[:index])
}

func TestRemoteTableFallsBackToSymbols(t *testing.T) {
	table, _, _ := tableFixture(36)
	if !table.symbols {
		t.Fatal("a narrow terminal must use symbols")
	}
	if liveWidth(table.header()) > 36 {
		t.Fatalf("header %q is wider than the terminal", table.header())
	}
	if got := table.statusCell(StatusPass, false); got != remoteGlyphPass {
		t.Fatalf("pass cell = %q", got)
	}
	if got := table.statusCell(StatusSkip, false); got != remoteGlyphSkip {
		t.Fatalf("skip cell = %q", got)
	}
}

func TestRemoteTableTruncatesStepNamesWhenVeryNarrow(t *testing.T) {
	table, _, _ := tableFixture(30)
	if liveWidth(table.header()) > 30 {
		t.Fatalf("header %q is wider than the terminal", table.header())
	}
	if !strings.Contains(table.header(), "…") {
		t.Fatalf("long step names must be shortened: %q", table.header())
	}
}

func TestRemoteTableRecomputesOnResize(t *testing.T) {
	table, _, _ := tableFixture(100)
	narrow := table.withWidth(36)
	if !narrow.symbols || table.symbols {
		t.Fatalf("resize must not change the original table: %v %v", table.symbols, narrow.symbols)
	}
	if wide := narrow.withWidth(120); wide.symbols {
		t.Fatal("a wider terminal must go back to words")
	}
}

func TestRemoteRunHeaderCountsPlannedSteps(t *testing.T) {
	_, hosts, recipe := tableFixture(100)
	cwd := t.TempDir()
	header := remoteRunHeader("daily", filepath.Join(cwd, "inventory.yaml"), "/etc/edc/recipe.yaml", cwd, hosts, recipe)
	// host 2개 × step 3개 중 brew는 mac 1대만 대상이라 5개다.
	if !strings.Contains(header, "host 2  ·  step 3  ·  실행 5") {
		t.Fatalf("header = %q", header)
	}
	if !strings.Contains(header, "inventory  ./inventory.yaml") {
		t.Fatalf("local path must be short: %q", header)
	}
	if !strings.Contains(header, "recipe  /etc/edc/recipe.yaml") {
		t.Fatalf("outside path must stay absolute: %q", header)
	}
}

func TestRemoteStepLegendShowsCommandsAndTags(t *testing.T) {
	_, hosts, recipe := tableFixture(100)
	lines := remoteStepLegend(recipe, hosts)
	if len(lines) != 3 {
		t.Fatalf("legend = %#v", lines)
	}
	if !strings.Contains(lines[0], "git-kit update  →  git-kit --version") {
		t.Fatalf("legend[0] = %q", lines[0])
	}
	if !strings.Contains(lines[2], "tags mac") || strings.Contains(lines[2], "대상 없음") {
		t.Fatalf("legend[2] = %q", lines[2])
	}
	// 어느 host와도 맞지 않는 tag는 그 사실을 밝힌다.
	orphan := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "apt", Command: "apt-get update", Tags: []string{"bsd"}}}}
	if got := remoteStepLegend(orphan, hosts); !strings.Contains(got[0], "tags bsd (대상 없음)") {
		t.Fatalf("orphan legend = %q", got[0])
	}
}

func TestShortPathKeepsOutsideFilesAbsolute(t *testing.T) {
	cwd := t.TempDir()
	if got := shortPath(cwd, filepath.Join(cwd, "a", "b.yaml")); got != "./a/b.yaml" {
		t.Fatalf("inside path = %q", got)
	}
	outside := filepath.Join(t.TempDir(), "b.yaml")
	if got := shortPath(cwd, outside); got != outside {
		t.Fatalf("outside path = %q", got)
	}
	if got := shortPath("", "/tmp/a.yaml"); got != "/tmp/a.yaml" {
		t.Fatalf("unknown cwd = %q", got)
	}
}

func TestTruncateName(t *testing.T) {
	if got := truncateName("brew update", 6); got != "brew …" {
		t.Fatalf("truncated = %q", got)
	}
	if got := truncateName("brew", 6); got != "brew" {
		t.Fatalf("short name must stay: %q", got)
	}
}

// 폭이 모자라면 열 이름을 먼저 줄이고, 단어가 들어갈 수 없을 때만 기호로 바꾼다.
func TestRemoteTablePrefersWordsOverSymbols(t *testing.T) {
	table, _, _ := tableFixture(45)
	if table.symbols {
		t.Fatalf("이름을 줄여 단어가 들어가면 기호로 바꾸지 않는다: %q", table.header())
	}
	if !strings.Contains(table.header(), "…") {
		t.Fatalf("긴 열 이름은 줄여야 한다: %q", table.header())
	}
	if liveWidth(table.header()) > 45 {
		t.Fatalf("header %q is wider than the terminal", table.header())
	}
}

func TestShortPathResolvesRelativeInput(t *testing.T) {
	cwd := t.TempDir()
	if got := shortPath(cwd, "slow.yaml"); got != "./slow.yaml" {
		t.Fatalf("relative input = %q", got)
	}
}
