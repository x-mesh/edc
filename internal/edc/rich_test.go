package edc

import (
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
	if !strings.Contains(richBadge("P1", richMagenta), "\033[1;30;45m") {
		t.Errorf("badge = %q", richBadge("P1", richMagenta))
	}
	if !strings.Contains(richBadge("SKIP", richGray), ";100m") {
		t.Errorf("a gray badge uses the bright black background: %q", richBadge("SKIP", richGray))
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
