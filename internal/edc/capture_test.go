package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestCapturePlanDetailListsEveryCondition(t *testing.T) {
	// wide 문자가 섞이는 언어까지 돌려야 열 정렬을 실제로 잰다.
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		t.Run(language, func(t *testing.T) { assertCapturePlanDetail(t) })
	}
}

func assertCapturePlanDetail(t *testing.T) {
	t.Helper()
	plan := capturePlan{interfaceName: "en0", duration: 15 * time.Second, count: 500, outputPath: "/tmp/incident.pcap"}
	detail := plan.detail()
	if !strings.HasPrefix(detail, T("cli.capture.plan_title")+"\n") || !strings.Contains(detail, capturePayloadWarning()) {
		t.Fatalf("detail = %q", detail)
	}
	// 한글 label이 섞여도 값 열이 같은 자리에서 시작해야 한다.
	rows := map[string]string{"interface": "en0", "duration": "15s", "packet limit": "500", "filter": "(none)", "output": "/tmp/incident.pcap", T("cli.capture.label.privilege"): T("cli.capture.privilege_sudo")}
	column := -1
	for _, line := range strings.Split(detail, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		label, value := "", ""
		for candidate, expected := range rows {
			if strings.HasPrefix(line, "  "+candidate) {
				label, value = candidate, expected
				break
			}
		}
		if label == "" {
			t.Fatalf("unexpected row %q", line)
		}
		if !strings.HasSuffix(line, value) {
			t.Fatalf("row %q does not end with %q", line, value)
		}
		start := liveWidth(strings.TrimSuffix(line, value))
		if column >= 0 && start != column {
			t.Fatalf("row %q starts its value at column %d, want %d", line, start, column)
		}
		column = start
		delete(rows, label)
	}
	if len(rows) != 0 {
		t.Fatalf("missing rows: %#v", rows)
	}
	filtered := capturePlan{interfaceName: "en0", filter: "host 203.0.113.10", privileged: true}
	if !strings.Contains(filtered.detail(), "host 203.0.113.10") {
		t.Fatalf("filter row = %q", filtered.detail())
	}
	if !strings.Contains(filtered.detail(), T("cli.capture.privilege_root")) {
		t.Fatalf("privilege row = %q", filtered.detail())
	}
}

// 확인 화면은 답을 고른 뒤에도 계획을 남겨야 tcpdump 출력 위에 조건이 보인다.
func TestCaptureConfirmKeepsPlanAfterAnswer(t *testing.T) {
	plan := capturePlan{interfaceName: "en0", duration: time.Second, count: 10, outputPath: "/tmp/a.pcap"}
	model := newDetailedConfirmModel(plan.detail(), T("cli.capture.confirm"), false)
	if !strings.Contains(model.View().Content, "en0") {
		t.Fatalf("view = %q", model.View().Content)
	}
	answered, _ := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	answeredModel := answered.(confirmModel)
	final := answeredModel.View().Content
	if !strings.Contains(final, "interface") || !strings.Contains(final, "en0") || !strings.Contains(final, T("cli.capture.confirm")+" "+confirmYesLabel()+"\n") {
		t.Fatalf("final view = %q", final)
	}
	// 확인 전후 화면 높이가 같아야 이전 줄이 남지 않는다.
	if liveLineCount(final) != liveLineCount(model.View().Content) {
		t.Fatalf("frame height changed: %d → %d", liveLineCount(model.View().Content), liveLineCount(final))
	}
}

func TestCaptureConfirmTextFallback(t *testing.T) {
	// terminal이 아니면 계획을 그대로 출력하고 y/N을 읽는다.
	if !strings.Contains(capturePlan{interfaceName: "en0"}.detail(), capturePayloadWarning()) {
		t.Fatal("plain fallback must keep the payload warning")
	}
}

func TestCaptureSelectItemsNamesTheDefaultRoute(t *testing.T) {
	interfaces := []interfaceDetails{
		{Name: "bridge100", Address: "192.168.139.3"},
		{Name: "en0", Address: "192.168.1.92", Gateway: "192.168.1.1"},
		// 주소가 둘인 interface는 목록에 두 번 나온다.
		{Name: "en0", Address: "10.0.0.5"},
	}
	items := captureSelectItems(interfaces, "en0")
	if len(items) != 2 {
		t.Fatalf("주소가 여럿인 interface는 한 줄이어야 한다: %#v", items)
	}
	if items[0].value != "bridge100" || items[1].value != "en0" {
		t.Fatalf("고르는 값은 interface 이름이어야 한다: %#v", items)
	}
	if !strings.Contains(items[1].label, "192.168.1.92") {
		t.Fatalf("이름만으로는 어느 것인지 알 수 없다: %q", items[1].label)
	}
	if !strings.Contains(items[1].label, T("cli.capture.default_route")) {
		t.Fatalf("기본 경로 interface에 표시가 없다: %q", items[1].label)
	}
	if strings.Contains(items[0].label, T("cli.capture.default_route")) {
		t.Fatalf("기본 경로가 아닌 interface에 표시가 붙었다: %q", items[0].label)
	}
	// 이름 열 폭을 맞춰야 주소를 세로로 훑을 수 있다.
	first, second := strings.Index(items[0].label, "192.168.139.3"), strings.Index(items[1].label, "192.168.1.92")
	if first != second {
		t.Fatalf("주소가 같은 열에서 시작하지 않는다: %q / %q", items[0].label, items[1].label)
	}
}
