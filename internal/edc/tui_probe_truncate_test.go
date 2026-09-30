package edc

import (
	"strings"
	"testing"
	"time"
)

// slowTruncateLine은 이분 탐색 전의 truncateLine이다. 새 구현이 같은 결과를 내는지 비교한다.
func slowTruncateLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	trimmed := strings.TrimRight(line, "\n")
	if liveWidth(trimmed) <= width {
		return line
	}
	runes := []rune(trimmed)
	for len(runes) > 0 && liveWidth(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…\n"
}

func TestTruncateLineMatchesTheOneCharacterCut(t *testing.T) {
	spinner := "\x1b[36m⠋\x1b[0m"
	for _, line := range []string{
		"",
		"short",
		strings.Repeat("a", 40) + "\n",
		strings.Repeat("x", 900),
		strings.Repeat("한글", 60),
		spinner + " dns lookup  " + strings.Repeat("output ", 80),
		"\x1b[31m" + strings.Repeat("red", 100) + "\x1b[0m tail",
		"é" + strings.Repeat("é", 50),
		strings.Repeat("👨‍👩‍👧 ", 30),
		strings.Repeat("🇰🇷", 30),
	} {
		for _, width := range []int{0, 1, 2, 10, 40, 80} {
			if got, want := truncateLine(line, width), slowTruncateLine(line, width); got != want {
				t.Fatalf("truncateLine(%q..., %d) = %q, want %q", line[:min(len(line), 20)], width, got, want)
			}
		}
	}
	// 색 escape가 있는 4KiB 줄도 빨리 자른다. 예전에는 줄 하나에 200ms가 넘게 걸렸다. 느린 CI를 생각해 상한을 넉넉히 둔다.
	line := spinner + " http check  " + strings.Repeat(`{"sku": "A-1"}, `, 256)
	started := time.Now()
	for range 100 {
		truncateLine(line, 110)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("100 lines took %s", elapsed)
	}
}
