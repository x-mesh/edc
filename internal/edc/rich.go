package edc

import (
	"fmt"
	"os"
	"strings"
)

// rich.go는 색을 쓰는 터미널에서만 쓰는 배지, 패널, 막대다. 파이프, NO_COLOR, --json은 이 함수를 거치지 않고
// 지금의 평문을 그대로 낸다. 색은 시안에서 고른 고정 색이다. ANSI 16색 번호를 그대로 내면 터미널 테마가 색을 정해,
// 형광 연두나 연보라처럼 시안과 전혀 다른 배지가 나왔다. 코드는 ANSI 번호로 색을 부르고 richSGR이 고정 색으로 바꾼다.

// 색 이름 대신 쓰는 ANSI 전경색 번호다. 배경색은 richBackground가 고른다.
const (
	richRed     = "31"
	richGreen   = "32"
	richYellow  = "33"
	richBlue    = "34"
	richMagenta = "35"
	richCyan    = "36"
	richGray    = "90"
)

const (
	richReset = "\033[0m"
	// richBadgeText는 배지 글자색이다. 진한 바탕 위에서 터미널의 검정을 쓴다.
	richBadgeText = "30"
)

// richEnabled는 이 파일에 색을 써도 되는지다. 다른 command가 써 온 규칙과 같다.
func richEnabled(file *os.File) bool {
	return isTerminal(file) && os.Getenv("NO_COLOR") == ""
}

func richSGR(text string, codes ...string) string {
	if text == "" || len(codes) == 0 {
		return text
	}
	truecolor := richTruecolor()
	params := make([]string, 0, len(codes))
	for _, code := range codes {
		params = append(params, richFixedColor(code, truecolor))
	}
	return "\033[" + strings.Join(params, ";") + "m" + text + richReset
}

// richRGB는 시안의 어두운 터미널 색이다. 강조색 여섯 개는 시안보다 채도를 높였다. 시안 그대로는 흐려 보였다.
type richRGB struct{ r, g, b int }

// richPalette는 ANSI 번호를 시안의 고정 색으로 바꾼다. 3x는 글자색, 4x와 100은 배경색이다.
// 30은 배지 글자(터미널 바탕에 가까운 검정), 97은 chip 글자다.
var richPalette = map[string]struct {
	rgb        richRGB
	background bool
}{
	"30": {richRGB{0x0e, 0x11, 0x16}, false},
	"31": {richRGB{0xff, 0x54, 0x60}, false},
	"32": {richRGB{0x4c, 0xe2, 0x7d}, false},
	"33": {richRGB{0xff, 0xbb, 0x3a}, false},
	"34": {richRGB{0x51, 0x9e, 0xff}, false},
	"35": {richRGB{0xb9, 0x83, 0xff}, false},
	"36": {richRGB{0x33, 0xe5, 0xef}, false},
	"90": {richRGB{0x7c, 0x85, 0x91}, false},
	"97": {richRGB{0xd6, 0xda, 0xe0}, false},
	"41": {richRGB{0xff, 0x54, 0x60}, true},
	"42": {richRGB{0x4c, 0xe2, 0x7d}, true},
	"43": {richRGB{0xff, 0xbb, 0x3a}, true},
	"44": {richRGB{0x51, 0x9e, 0xff}, true},
	"45": {richRGB{0xb9, 0x83, 0xff}, true},
	"46": {richRGB{0x33, 0xe5, 0xef}, true},
	// 100은 두 곳에 쓴다. 회색 배지 바탕과 chip 바탕이다. chip은 글자(97)와 함께 오므로 richChip이 따로 고른다.
	"100": {richRGB{0x7c, 0x85, 0x91}, true},
}

// richChipBackground는 chip의 바탕이다. 시안의 터미널 바탕보다 조금 밝은 회색이다.
var richChipBackground = richRGB{0x26, 0x2b, 0x33}

// richTruecolor는 24비트 색을 써도 되는지다. 터미널은 COLORTERM으로 알린다. 모르면 256색으로 줄인다.
func richTruecolor() bool {
	value := strings.ToLower(os.Getenv("COLORTERM"))
	return value == "truecolor" || value == "24bit"
}

func richFixedColor(code string, truecolor bool) string {
	entry, ok := richPalette[code]
	if !ok {
		return code
	}
	return richRGBCode(entry.rgb, entry.background, truecolor)
}

func richRGBCode(rgb richRGB, background, truecolor bool) string {
	lead := "38"
	if background {
		lead = "48"
	}
	if truecolor {
		return fmt.Sprintf("%s;2;%d;%d;%d", lead, rgb.r, rgb.g, rgb.b)
	}
	return fmt.Sprintf("%s;5;%d", lead, rich256(rgb))
}

// rich256은 xterm 256색에서 가장 가까운 색이다. 6단계 색 정육면체와 24단계 회색 중 거리가 짧은 쪽을 고른다.
func rich256(rgb richRGB) int {
	levels := []int{0, 95, 135, 175, 215, 255}
	nearest := func(value int) int {
		best := 0
		for index, level := range levels {
			if abs(level-value) < abs(levels[best]-value) {
				best = index
			}
		}
		return best
	}
	r, g, b := nearest(rgb.r), nearest(rgb.g), nearest(rgb.b)
	cube := 16 + 36*r + 6*g + b
	cubeDistance := richDistance(rgb, richRGB{levels[r], levels[g], levels[b]})
	average := (rgb.r + rgb.g + rgb.b) / 3
	step := min(max((average-8+5)/10, 0), 23)
	gray := 8 + step*10
	if richDistance(rgb, richRGB{gray, gray, gray}) < cubeDistance {
		return 232 + step
	}
	return cube
}

func richDistance(a, b richRGB) int {
	return (a.r-b.r)*(a.r-b.r) + (a.g-b.g)*(a.g-b.g) + (a.b-b.b)*(a.b-b.b)
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// richPaint는 글자에 색을 입힌다. bold면 굵게도 한다.
func richPaint(text, color string, bold bool) string {
	if bold {
		return richSGR(text, "1", color)
	}
	return richSGR(text, color)
}

func richBold(text string) string { return richSGR(text, "1") }

// richDim은 경로와 보조 정보다. SGR 2(faint)는 터미널마다 다르게 그려서 회색(90)을 쓴다.
func richDim(text string) string { return richSGR(text, richGray) }

// richBackground는 전경색 번호를 같은 색의 배경색 번호로 바꾼다. 회색 90은 밝은 검정 배경 100이 된다.
func richBackground(color string) string {
	if color == richGray {
		return "100"
	}
	return "4" + strings.TrimPrefix(color, "3")
}

// richBadge는 진한 색 바탕에 검은 글자로 쓴 " text "다. 상태, 우선순위, HTTP code에 쓴다.
func richBadge(text, color string) string {
	return richSGR(" "+text+" ", "1", richBadgeText, richBackground(color))
}

// richStatusColor는 상태의 색이다.
func richStatusColor(status Status) string {
	switch status {
	case StatusPass:
		return richGreen
	case StatusWarn:
		return richYellow
	case StatusFail:
		return richRed
	}
	return richGray
}

// richStatusBadge는 결과 줄 맨 앞의 상태 배지다. 네 상태가 같은 폭(6칸)이 되도록 SKIP도 대문자 네 글자를 쓴다.
func richStatusBadge(status Status) string {
	return richBadge(strings.ToUpper(string(status)), richStatusColor(status))
}

// richStatusBadgeWidth는 richStatusBadge의 표시 폭이다.
const richStatusBadgeWidth = 6

// richChip은 flag나 짧은 값을 회색 바탕에 담는다.
func richChip(text string) string {
	if text == "" {
		return text
	}
	return "\033[" + richRGBCode(richChipBackground, true, richTruecolor()) + ";" + richFixedColor("97", richTruecolor()) + "m " + text + " " + richReset
}

// richHTTPCodeColor는 HTTP 상태 code의 색이다. 2xx 초록, 3xx 청록, 4xx 노랑, 5xx 빨강.
func richHTTPCodeColor(code int) string {
	switch code / 100 {
	case 2:
		return richGreen
	case 3:
		return richCyan
	case 4:
		return richYellow
	case 5:
		return richRed
	}
	return richGray
}

// richSizeColor는 크기의 색이다. 10 GB 이상 빨강, 1 GB 이상 노랑, 그 아래 초록, 0은 회색.
func richSizeColor(bytes int64) string {
	switch {
	case bytes <= 0:
		return richGray
	case bytes >= 10_000_000_000:
		return richRed
	case bytes >= 1_000_000_000:
		return richYellow
	}
	return richGreen
}

// 패널은 오른쪽 테두리가 없는 열린 상자다. 긴 줄이 접혀도 지금의 ERROR 상자처럼 깨지지 않는다.
// color가 빈 문자열이면 테두리는 회색이다.

// richPanelTop은 "╭─ title  detail ───"이다. 제목은 color로 굵게, detail은 회색이다.
func richPanelTop(title, detail, color string) string {
	border := richPanelBorderColor(color)
	titleColor := color
	if titleColor == "" {
		titleColor = richMagenta
	}
	line := richSGR("╭─ ", border) + richPaint(title, titleColor, true)
	if detail != "" {
		line += "  " + richDim(detail)
	}
	return line + richSGR(" ───", border)
}

// richPanelLine은 패널 안의 한 줄이다.
func richPanelLine(text, color string) string {
	if text == "" {
		return richSGR("│", richPanelBorderColor(color))
	}
	return richSGR("│ ", richPanelBorderColor(color)) + text
}

func richPanelBottom(color string) string {
	return richSGR("╰─", richPanelBorderColor(color))
}

func richPanelBorderColor(color string) string {
	if color == "" {
		return richGray
	}
	return color
}

// richBlocks는 1/8칸 단위 막대 조각이다.
var richBlocks = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// richBar는 value가 largest에 대해 차지하는 비율을 width칸 막대로 그린다. 색은 caller가 입힌다.
// 0보다 크면 가장 얇은 조각이라도 남겨, 작은 값과 0을 구분한다. 결과의 표시 폭은 항상 width다.
func richBar(value, largest float64, width int) string {
	if width <= 0 {
		return ""
	}
	if largest <= 0 || value <= 0 {
		return strings.Repeat(" ", width)
	}
	eighths := int(value/largest*float64(width*8) + 0.5)
	eighths = min(max(eighths, 1), width*8)
	bar := strings.Repeat("█", eighths/8) + richBlocks[eighths%8]
	return bar + strings.Repeat(" ", width-liveWidth(bar))
}

// richSegment는 여러 색으로 나눈 막대의 한 구간이다.
type richSegment struct {
	value float64
	color string
}

// richSegments는 구간들을 합계에 비례해 cells칸에 나눠 그린다. 0보다 큰 구간은 적어도 한 칸을 받는다.
// 개수 막대와 http 구간 막대가 쓴다.
func richSegments(segments []richSegment, cells int) string {
	total := 0.0
	nonzero := 0
	for _, segment := range segments {
		if segment.value > 0 {
			total += segment.value
			nonzero++
		}
	}
	if total <= 0 || cells <= 0 {
		return ""
	}
	cells = max(cells, nonzero)
	var builder strings.Builder
	used, seen := 0, 0.0
	for _, segment := range segments {
		if segment.value <= 0 {
			continue
		}
		seen += segment.value
		end := max(int(seen/total*float64(cells)+0.5), used+1)
		builder.WriteString(richSGR(strings.Repeat("█", end-used), segment.color))
		used = end
	}
	return builder.String()
}

// richDetailRenderers는 색을 쓰는 터미널에서 probe의 evidence 블록 대신 그릴 줄이다. 키는 probe ID다.
// --redact를 거친 Result를 받으므로 가린 값만 그린다. 그래서 evidence 문자열이 아니라 Result.Metrics에서 그린다.
// Metrics의 숫자는 --redact가 JSON을 한 번 거치면 float64가 되므로 richNumber로 읽는다.
var richDetailRenderers = map[string]func(result Result, width int) []string{}

// richEvidence는 그 probe의 그림이 있으면 그 줄을, 없으면 nil을 돌려준다.
func richEvidence(result Result, width int) []string {
	render, ok := richDetailRenderers[result.Probe]
	if !ok {
		return nil
	}
	return render(result, width)
}

// richNumber는 Metrics의 숫자를 float64로 읽는다. int, int64, float64 어느 쪽이든 받는다.
func richNumber(value interface{}) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, true
	}
	return 0, false
}
