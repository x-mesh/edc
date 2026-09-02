package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestCapturePlanDetailListsEveryCondition(t *testing.T) {
	plan := capturePlan{interfaceName: "en0", duration: 15 * time.Second, count: 500, outputPath: "/tmp/incident.pcap"}
	detail := plan.detail()
	if !strings.HasPrefix(detail, "capture 계획\n") || !strings.Contains(detail, capturePayloadWarning) {
		t.Fatalf("detail = %q", detail)
	}
	// 한글 label이 섞여도 값 열이 같은 자리에서 시작해야 한다.
	rows := map[string]string{"interface": "en0", "duration": "15s", "packet limit": "500", "filter": "(none)", "output": "/tmp/incident.pcap", "권한": "sudo로 tcpdump를 실행합니다"}
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
	if !strings.Contains(filtered.detail(), "root로 실행합니다") {
		t.Fatalf("privilege row = %q", filtered.detail())
	}
}

// 확인 화면은 답을 고른 뒤에도 계획을 남겨야 tcpdump 출력 위에 조건이 보인다.
func TestCaptureConfirmKeepsPlanAfterAnswer(t *testing.T) {
	plan := capturePlan{interfaceName: "en0", duration: time.Second, count: 10, outputPath: "/tmp/a.pcap"}
	model := newDetailedConfirmModel(plan.detail(), "capture를 실행할까요?", false)
	if !strings.Contains(model.View().Content, "en0") {
		t.Fatalf("view = %q", model.View().Content)
	}
	answered, _ := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	answeredModel := answered.(confirmModel)
	final := answeredModel.View().Content
	if !strings.Contains(final, "interface") || !strings.Contains(final, "en0") || !strings.Contains(final, "capture를 실행할까요? 예\n") {
		t.Fatalf("final view = %q", final)
	}
	// 확인 전후 화면 높이가 같아야 이전 줄이 남지 않는다.
	if liveLineCount(final) != liveLineCount(model.View().Content) {
		t.Fatalf("frame height changed: %d → %d", liveLineCount(model.View().Content), liveLineCount(final))
	}
}

func TestCaptureConfirmTextFallback(t *testing.T) {
	// terminal이 아니면 계획을 그대로 출력하고 y/N을 읽는다.
	if !strings.Contains(capturePlan{interfaceName: "en0"}.detail(), capturePayloadWarning) {
		t.Fatal("plain fallback must keep the payload warning")
	}
}
