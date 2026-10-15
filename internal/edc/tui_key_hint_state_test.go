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
	got := strings.Join(model.helpOn(), ",")
	if got != "u path,i split,m secrets" {
		t.Fatalf("http help on = %q", got)
	}
}

func TestBrowserHintsMarkAllFailedAndFilter(t *testing.T) {
	svc := svcBrowser{all: true, failed: true, filter: "nginx"}
	if got := strings.Join(svc.hintOn(), ","); got != "a all,failed only,filter nginx" {
		t.Fatalf("svc on = %q", got)
	}
	svc.filtering = true
	if got := strings.Join(svc.hintOn(), ","); got != "a all,failed only" {
		t.Fatalf("svc on while typing = %q", got)
	}
	if got := strings.Join((ctrBrowser{all: true}).hintOn(), ","); got != "a all" {
		t.Fatalf("ctr on = %q", got)
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
