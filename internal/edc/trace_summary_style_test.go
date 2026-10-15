package edc

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// setTraceSummaryTerminal은 요약이 width칸 색 터미널에 쓰는 것처럼 그리게 한다. color가 false면 평문이다.
func setTraceSummaryTerminal(t *testing.T, color bool, width int) {
	t.Helper()
	previous := traceSummaryTerminal
	traceSummaryTerminal = func(io.Writer) (bool, int) { return color, width }
	t.Cleanup(func() { traceSummaryTerminal = previous })
}

func traceSummaryOutput(t *testing.T, fixture traceSummaryFixture, color bool, width int) string {
	t.Helper()
	setTraceSummaryTerminal(t, color, width)
	return traceCaptureOutput(t, &os.Stdout, fixture.print)
}

// 색을 지우면 평문과 같은 글자다. tab으로 나눈 표만 색 터미널에서 칸을 공백으로 맞춘다.
func TestTraceSummariesKeepThePlainTextWithColor(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	for _, fixture := range traceSummaryFixtures() {
		plain := traceSummaryOutput(t, fixture, false, 0)
		colored := traceSummaryOutput(t, fixture, true, 400)
		if strings.Contains(plain, "\x1b") {
			t.Fatalf("%s: plain output has escapes:\n%q", fixture.name, plain)
		}
		if !strings.Contains(colored, "\x1b[") {
			t.Fatalf("%s: colored output has no color", fixture.name)
		}
		plainLines, coloredLines := strings.Split(plain, "\n"), strings.Split(ansi.Strip(colored), "\n")
		if len(plainLines) != len(coloredLines) {
			t.Fatalf("%s: %d plain lines, %d colored lines:\n%s\n---\n%s", fixture.name, len(plainLines), len(coloredLines), plain, ansi.Strip(colored))
		}
		for index, line := range plainLines {
			got := coloredLines[index]
			if strings.Contains(line, "\t") {
				if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(line), " ") || strings.Contains(got, "\t") {
					t.Errorf("%s: table line %q became %q", fixture.name, line, got)
				}
				continue
			}
			if got != line {
				t.Errorf("%s: line %q became %q", fixture.name, line, got)
			}
		}
	}
}

// 색을 쓴 줄은 터미널보다 넓지 않다. 넓은 표는 가장 넓은 글자 열부터 줄여 오른쪽의 수를 남긴다.
func TestTraceSummariesFitTheTerminal(t *testing.T) {
	const width = 80
	for _, fixture := range traceSummaryFixtures() {
		for _, line := range strings.Split(traceSummaryOutput(t, fixture, true, width), "\n") {
			if liveWidth(line) > width {
				t.Errorf("%s: line is %d cells wide: %q", fixture.name, liveWidth(line), ansi.Strip(line))
			}
		}
	}
	var tcp traceSummaryFixture
	for _, fixture := range traceSummaryFixtures() {
		if fixture.name == "tcp detail" {
			tcp = fixture
		}
	}
	text := ansi.Strip(traceSummaryOutput(t, tcp, true, width))
	if !strings.Contains(text, "RETRANS  RESET") || !strings.Contains(text, "10.0.0.5:443 (api.…  established") {
		t.Fatalf("the wide column was not shortened first:\n%s", text)
	}
}

func TestTraceSummaryColorsProblemsOnlyAboveZero(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	style := traceSummaryStyle{textStyle{color: true}}
	for _, test := range []struct{ got, want string }{
		{style.title("TCP trace", "2s"), "\x1b[1;35mTCP trace\x1b[0m\x1b[90m:\x1b[0m 2\x1b[90ms\x1b[0m"},
		{style.field("Resets", "2"), "\x1b[90mResets:\x1b[0m \x1b[31m2\x1b[0m"},
		{style.field("Resets", "0"), "\x1b[90mResets:\x1b[0m 0"},
		{style.field("Retransmissions", "-"), "\x1b[90mRetransmissions:\x1b[0m -"},
		{style.field("Lost events", "3 (buffer overflows)"), "\x1b[90mLost events:\x1b[0m \x1b[33m3\x1b[0m \x1b[90m(buffer overflows)\x1b[0m"},
		{style.field("Latency avg", "12.5ms"), "\x1b[90mLatency avg:\x1b[0m 12.5\x1b[90mms\x1b[0m"},
		{style.field("Traffic rate", "1.0 B/s · 8.0 bps"), "\x1b[90mTraffic rate:\x1b[0m 1.0 \x1b[90mB/s\x1b[0m\x1b[90m · \x1b[0m8.0 \x1b[90mbps\x1b[0m"},
		{style.headline("SYSCALL TRACE", traceStat{"calls", "5"}, traceStat{"errors", "1"}), "\x1b[1;35mSYSCALL TRACE\x1b[0m  \x1b[90mcalls\x1b[0m 5  \x1b[90merrors\x1b[0m \x1b[31m1\x1b[0m"},
		{style.stats(traceStat{"ring_lost", "0"}, traceStat{"map_full", "2"}), "\x1b[90mring_lost\x1b[0m 0  \x1b[90mmap_full\x1b[0m \x1b[33m2\x1b[0m"},
		{style.httpStatuses("200 5, 404 1, 503 2"), "\x1b[32m200\x1b[0m 5\x1b[90m, \x1b[0m\x1b[33m404\x1b[0m 1\x1b[90m, \x1b[0m\x1b[31m503\x1b[0m 2"},
		{style.units("7zip"), "7zip"},
	} {
		if test.got != richExpect(test.want) {
			t.Errorf("got  %q\nwant %q", test.got, richExpect(test.want))
		}
	}
}

func TestTraceSummaryTablesColorTheProblemColumns(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	output := map[string]string{}
	for _, fixture := range traceSummaryFixtures() {
		output[fixture.name] = traceSummaryOutput(t, fixture, true, 400)
	}
	for _, test := range []struct{ fixture, want string }{
		// 재전송은 노랑, reset은 빨강이고 0은 기본색이다.
		{"tcp detail", "\x1b[33m4\x1b[0m        \x1b[31mtrue\x1b[0m"},
		{"tcp detail", "  0        false"},
		// group 표는 live 화면과 같은 열의 색과 LAST event의 색이다.
		{"group tcp", "\x1b[33m3\x1b[0m        \x1b[31m1\x1b[0m      \x1b[31mtcp_reset\x1b[0m"},
		{"group arp", "\x1b[31m1\x1b[0m            0         \x1b[31marp_mac_change\x1b[0m"},
		{"http", "\x1b[32m200\x1b[0m 1\x1b[90m, \x1b[0m\x1b[33m404\x1b[0m 1\x1b[90m, \x1b[0m\x1b[31m503\x1b[0m 1"},
		{"mysql", "\x1b[31m1\x1b[0m       0"},
		{"drop", "NO_SOCKET  \x1b[31m3\x1b[0m"},
		// 고정 폭 표는 칸을 맞춘 뒤 칠해 평문과 같은 자리에 있다.
		{"syscall", "  4  \x1b[31m3 ENOENT\x1b[0m     4.0\x1b[90mus\x1b[0m"},
		{"syscall", "  2  0          10.00\x1b[90mms\x1b[0m"},
		{"priv", "\x1b[31mdenied          \x1b[0m"},
		{"priv", "\x1b[1;31mDENIED\x1b[0m"},
		{"exec", "\x1b[33m       1\x1b[0m"},
		{"cpu", "copy_to_user \x1b[90m[k]\x1b[0m"},
	} {
		if !strings.Contains(output[test.fixture], richExpect(test.want)) {
			t.Errorf("%s misses %q:\n%q", test.fixture, richExpect(test.want), output[test.fixture])
		}
	}
	// 표 제목은 칸마다 회색이고, 수와 이름은 기본색이다.
	if !strings.Contains(output["redis"], richExpect("\x1b[90mCOMMAND\x1b[0m  \x1b[90mCOUNT\x1b[0m  \x1b[90mERRORS\x1b[0m  \x1b[90mUNANSWERED\x1b[0m  \x1b[90mAVG\x1b[0m    \x1b[90mMAX\x1b[0m    \x1b[90mPROCESS\x1b[0m\nGET      3      0       0           0.2\x1b[90mms\x1b[0m")) {
		t.Errorf("redis table:\n%q", output["redis"])
	}
}
