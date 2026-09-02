package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// bannerBody와 bannerAccent는 글자 몸통과 왼쪽 기둥의 색이다.
	bannerBody    = "\033[38;5;63m"
	bannerAccent  = "\033[38;5;87m"
	bannerGap     = " "
	bannerSideGap = "   "
	bannerTag     = "SE/SRE 진단 툴킷"
	// bannerWidth는 글자 세 개와 그 사이 여백의 폭이다.
	bannerWidth = 3*3 + 2
)

// bannerLetter는 한 글자의 한 줄이다. stem은 accent 색, body는 몸통 색으로 그린다.
type bannerLetter struct {
	stem string
	body string
}

// bannerRows는 edc 세 글자를 3줄 블록으로 그린다.
// 왼쪽 기둥만 색을 달리해 글자가 작아도 형태가 살아 있게 한다.
var bannerRows = [][]bannerLetter{
	{{stem: "█", body: "▀▀"}, {stem: "█", body: "▀▄"}, {stem: "█", body: "▀▀"}},
	{{stem: "█", body: "▀▀"}, {stem: "█", body: " █"}, {stem: "█", body: "  "}},
	{{body: "▀▀▀"}, {body: "▀▀▀"}, {body: "▀▀▀"}},
}

// formatBanner는 3줄 banner를 만든다. 오른쪽에는 설명과 버전을 둔다.
func formatBanner(version string, color bool) string {
	side := []string{"", bannerTag, version}
	var builder strings.Builder
	for index, row := range bannerRows {
		letters := make([]string, 0, len(row))
		for _, letter := range row {
			letters = append(letters, paintBanner(letter.stem, bannerAccent, color)+paintBanner(letter.body, bannerBody, color))
		}
		// 글자 블록 폭을 고정해 오른쪽 설명이 세 줄에서 같은 열에 선다.
		line := liveCell(strings.Join(letters, bannerGap), bannerWidth)
		if side[index] == "" {
			builder.WriteString(strings.TrimRight(line, " ") + "\n")
			continue
		}
		builder.WriteString(line + bannerSideGap + side[index] + "\n")
	}
	return builder.String()
}

func paintBanner(text, code string, color bool) string {
	if text == "" {
		return ""
	}
	if !color {
		return text
	}
	return code + text + liveReset
}

// printVersion은 terminal에서는 banner를, 그 외에는 기계가 읽는 한 줄만 출력한다.
func printVersion(writer io.Writer, version string) {
	if isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "" {
		fmt.Fprint(writer, formatBanner(version, true))
		return
	}
	fmt.Fprintf(writer, "edc %s (schema 1.0)\n", version)
}
