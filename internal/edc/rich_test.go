package edc

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRichBarKeepsItsWidth(t *testing.T) {
	for _, test := range []struct {
		value, largest float64
		want           string
	}{
		{0, 10, "    "},
		{10, 10, "████"},
		{5, 10, "██  "},
		{0.01, 10, "▏   "},
		{3, 0, "    "},
	} {
		if got := richBar(test.value, test.largest, 4); got != test.want {
			t.Errorf("richBar(%v, %v) = %q, want %q", test.value, test.largest, got, test.want)
		}
	}
}

func TestRichSegmentsGiveEveryNonzeroPartACell(t *testing.T) {
	got := ansi.Strip(richSegments([]richSegment{{1, richBlue}, {0, richCyan}, {100, richGreen}}, 10))
	if got != strings.Repeat("█", 10) {
		t.Errorf("segments = %q", got)
	}
	if richSegments(nil, 10) != "" || richSegments([]richSegment{{0, richRed}}, 10) != "" {
		t.Error("no value draws nothing")
	}
	if got := ansi.Strip(richSegments([]richSegment{{1, richBlue}, {1, richCyan}, {1, richGreen}}, 2)); got != "███" {
		t.Errorf("three parts need three cells: %q", got)
	}
}

func TestRichStatusBadgesShareOneWidth(t *testing.T) {
	for _, status := range []Status{StatusPass, StatusWarn, StatusFail, StatusSkip} {
		if width := liveWidth(richStatusBadge(status)); width != richStatusBadgeWidth {
			t.Errorf("%s badge is %d cells", status, width)
		}
	}
	if !strings.Contains(richBadge("P1", richMagenta), richExpect("\033[1;30;45m")) {
		t.Errorf("badge = %q", richBadge("P1", richMagenta))
	}
	if !strings.Contains(richBadge("SKIP", richGray), richFixedColor("100", richTruecolor())+"m") {
		t.Errorf("a gray badge uses the gray background: %q", richBadge("SKIP", richGray))
	}
}

func TestRichEvidenceNeedsColorAndARenderer(t *testing.T) {
	result := Result{Probe: "rich.test", Evidence: []Evidence{{Label: "x", Value: "y"}}}
	if richEvidenceLines(true, result) != nil {
		t.Fatal("a probe without a renderer keeps its evidence")
	}
	richDetailRenderers["rich.test"] = func(Result, int) []string { return []string{"drawn"} }
	defer delete(richDetailRenderers, "rich.test")
	if richEvidenceLines(false, result) != nil {
		t.Error("plain output never uses the renderer")
	}
	if got := richEvidenceLines(true, result); len(got) != 1 || got[0] != "drawn" {
		t.Errorf("lines = %q", got)
	}
}

var richExpectSGR = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// richExpect는 테스트가 ANSI 16색 번호로 적은 기대값을 실제로 내보내는 고정 색 표기로 바꾼다.
// 테스트는 색의 역할(빨강, 회색)을 확인하고, 정확한 RGB는 richSGR 한 곳이 정한다.
func richExpect(text string) string {
	return richExpectSGR.ReplaceAllStringFunc(text, func(sequence string) string {
		params := richExpectSGR.FindStringSubmatch(sequence)[1]
		if params == "" || params == "0" {
			return sequence
		}
		if params == "100;97" {
			return strings.TrimSuffix(richChip("x"), " x "+richReset)
		}
		return strings.TrimSuffix(richSGR("x", strings.Split(params, ";")...), "x"+richReset)
	})
}

// 터미널 테마가 아니라 시안의 색을 쓴다. truecolor를 알리지 않는 터미널은 가장 가까운 256색을 받는다.
func TestRichSGRUsesTheFixedPalette(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	if got := richPaint("ok", richGreen, true); got != "\x1b[1;38;2;111;207;142mok\x1b[0m" {
		t.Errorf("truecolor green = %q", got)
	}
	if got := richBadge("FAIL", richRed); got != "\x1b[1;38;2;14;17;22;48;2;240;114;123m FAIL \x1b[0m" {
		t.Errorf("truecolor badge = %q", got)
	}
	t.Setenv("COLORTERM", "")
	if got := richPaint("ok", richGreen, false); got != "\x1b[38;5;78mok\x1b[0m" {
		t.Errorf("256 color green = %q", got)
	}
	if got := rich256(richRGB{0x7c, 0x85, 0x91}); got < 16 {
		t.Errorf("gray must not fall back to a theme color: %d", got)
	}
}
