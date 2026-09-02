package edc

import (
	"strings"
	"testing"
)

func TestFormatBannerShape(t *testing.T) {
	plain := formatBanner("0.1.0", false)
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(lines) != len(bannerRows) {
		t.Fatalf("banner = %d lines, want %d", len(lines), len(bannerRows))
	}
	// 각 줄은 bannerRows가 정한 글자 모양으로 시작한다.
	for index, row := range bannerRows {
		expected := make([]string, 0, len(row))
		for _, letter := range row {
			expected = append(expected, letter.stem+letter.body)
		}
		if !strings.HasPrefix(lines[index], strings.Join(expected, bannerGap)) {
			t.Fatalf("line %q does not start with %q", lines[index], strings.Join(expected, bannerGap))
		}
	}
	// 오른쪽 설명은 두 줄에서 같은 열에 선다.
	want := bannerWidth + liveWidth(bannerSideGap)
	for index, side := range map[int]string{1: T("observe.banner.tag"), 2: "0.1.0"} {
		line := lines[index]
		at := strings.Index(line, side)
		if at < 0 {
			t.Fatalf("line %q does not carry %q", line, side)
		}
		if got := liveWidth(line[:at]); got != want {
			t.Fatalf("line %q starts %q at column %d, want %d", line, side, got, want)
		}
	}
	if !strings.Contains(plain, T("observe.banner.tag")) || !strings.Contains(plain, "0.1.0") {
		t.Fatalf("banner must carry the tagline and the version: %q", plain)
	}
	if strings.Contains(plain, "\033[") {
		t.Fatalf("색을 끄면 escape가 없어야 한다: %q", plain)
	}
}

func TestFormatBannerColorsStemAndBody(t *testing.T) {
	colored := formatBanner("0.1.0", true)
	if !strings.Contains(colored, bannerAccent+"█") {
		t.Fatalf("왼쪽 기둥은 accent 색이어야 한다: %q", colored)
	}
	if !strings.Contains(colored, bannerBody+"▀▀") {
		t.Fatalf("몸통은 body 색이어야 한다: %q", colored)
	}
	// 마지막 줄은 기둥이 없어 accent가 붙지 않는다.
	last := strings.Split(strings.TrimRight(colored, "\n"), "\n")[len(bannerRows)-1]
	if strings.Contains(last, bannerAccent) {
		t.Fatalf("바닥 줄에는 기둥이 없다: %q", last)
	}
}

func TestPrintVersionStaysMachineReadableOffTerminal(t *testing.T) {
	var output strings.Builder
	printVersion(&output, "0.1.0")
	if output.String() != "edc 0.1.0 (schema 1.0)\n" {
		t.Fatalf("파이프 출력 = %q", output.String())
	}
}
