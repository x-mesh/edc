package edc

import (
	"strings"
	"testing"
)

// 켜진 묶음은 전체 글자나 키로 고르고, 고른 항목과 같은 바탕으로 칠한다. 나머지 묶음은 키가 청록, 설명이 회색이다.
func TestLiveKeyHintsMarkTheGroupsThatAreOn(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	got := liveKeyHints("↑↓ select · a all · f failed · q quit", true, "a all", "f")
	for _, want := range []string{liveSelected("a all", true), liveSelected("f failed", true), richPaint("q", richCyan, true) + richDim(" quit")} {
		if !strings.Contains(got, want) {
			t.Fatalf("hints lack %q: %q", want, got)
		}
	}
	if plain := liveKeyHints("a all · q quit", false, "a"); plain != "a all · q quit" {
		t.Fatalf("without color the text stays: %q", plain)
	}
	// 번역된 안내도 키로 고른다.
	if got := liveKeyHints("  ·  f 필터, e 상세, q 종료", true, "e"); !strings.Contains(got, liveSelected("e 상세", true)) {
		t.Fatalf("translated hints = %q", got)
	}
}

func TestTraceKeySpansMarkTheCurrentView(t *testing.T) {
	const keys = "/ filter  tab view  p process  a parent  e event  c cgroup  g scroll  ↑↓ select"
	for groupBy, want := range map[string]string{"": "g scroll", "parent": "a parent", "cgroup": "c cgroup", "missing": ""} {
		if got := traceViewOn(keys, groupBy); got != want {
			t.Errorf("view %q = %q, want %q", groupBy, got, want)
		}
	}
	spans := traceKeySpans(keys, "a parent")
	marked := 0
	for _, span := range spans {
		if span.on {
			marked++
			if span.text != "a parent" {
				t.Fatalf("marked span = %q", span.text)
			}
		}
	}
	if marked != 1 || traceTexts(spans, 0, false) != keys {
		t.Fatalf("marked %d, plain %q", marked, traceTexts(spans, 0, false))
	}
}

func TestTraceScreenHelpMarksViewAndToggles(t *testing.T) {
	model := traceScreenModel{protocol: "http", groupBy: traceGroupByPath, split: true, secrets: true}
	// 미리 보기는 event 목록에서만 그리므로, path 보기에서는 split을 칠하지 않는다.
	if got := strings.Join(model.helpOn(), ","); got != "u path,m secrets" {
		t.Fatalf("http help on = %q", got)
	}
	model.groupBy = ""
	if got := strings.Join(model.helpOn(), ","); got != "g scroll,i split,m secrets" {
		t.Fatalf("event list help on = %q", got)
	}
}

func TestBrowserHintsMarkAllFailedAndFilter(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if got := svcBrowserHints("", false, false, true, 0, true); !strings.Contains(got, liveSelected("a all", true)) {
		t.Fatalf("a all is not marked: %q", got)
	}
	// 필터 글자에 안내의 구분자나 끝 공백이 있어도 조건 전체를 칠한다.
	for _, filter := range []string{"nginx", "web, api", "a · b", "nginx "} {
		got := svcBrowserHints(filter, false, true, false, 3, true)
		for _, want := range []string{liveSelected("filter "+filter, true), liveSelected("failed only", true)} {
			if !strings.Contains(got, want) {
				t.Errorf("filter %q: hints lack %q: %q", filter, want, got)
			}
		}
		if plain := svcBrowserHints(filter, false, true, false, 3, false); plain != "filter "+filter+", failed only · 3 shown · Esc clear · Enter open · q quit" {
			t.Errorf("filter %q: plain = %q", filter, plain)
		}
	}
	if got := svcBrowserHints("nginx", true, false, false, 0, true); strings.Contains(got, liveSelected("filter nginx", true)) {
		t.Fatalf("the filter is not on while it is typed: %q", got)
	}
}

// 한국어 안내는 키에 조사를 붙인다. 조사를 뺀 키만 청록으로 칠한다.
func TestLiveKeyHintsPaintAKeyBeforeAParticle(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for text, key := range map[string]string{"q를 누르면 멈추고 읽은 만큼 보여 줍니다": "q", "Ctrl-C로 중단하고 읽은 만큼 출력합니다": "Ctrl-C", "q で止めて読んだ分を表示します": "q"} {
		got := liveKeyHints(text, true)
		if want := richPaint(key, richCyan, true); !strings.HasPrefix(got, want) {
			t.Errorf("%q = %q, want the key %q first", text, got, key)
		}
		if plain := liveKeyHints(text, false); plain != text {
			t.Errorf("%q: plain = %q", text, plain)
		}
	}
	if got := liveKeyHints("PID로 고릅니다", true); got != richDim("PID로 고릅니다") {
		t.Fatalf("a word that is not a key stays gray: %q", got)
	}
}

func TestTopInterfacePickerMarksThePreset(t *testing.T) {
	picker := topInterfacePicker{presets: []topInterfacePreset{{label: "all"}, {label: "physical"}}, preset: 1}
	if got := strings.Join(picker.footerOn(), ","); got != "p" {
		t.Fatalf("physical preset on = %q", got)
	}
	picker.preset = 0
	if got := strings.Join(picker.footerOn(), ","); got != "a" {
		t.Fatalf("all preset on = %q", got)
	}
	picker.preset = -1
	if picker.footerOn() != nil {
		t.Fatal("checked names are not a preset")
	}
}
