package edc

import (
	"encoding/json"
	"errors"
	"github.com/charmbracelet/x/ansi"
	"os"
	"strings"
	"testing"
)

func TestCollectionIsIndependentOfHealthAndPreservesLegacyJSON(t *testing.T) {
	result := Result{Probe: "jobs.cron", Status: StatusFail, Summary: "job failed"}
	legacy, _ := json.Marshal(result)
	if strings.Contains(string(legacy), "collection") {
		t.Fatal("legacy JSON changed")
	}
	observed := withCollection(result, collectionObserved, intPointer(0), "")
	data, _ := json.Marshal(observed)
	if observed.Status != StatusFail || exitCode([]Result{observed}) != 1 || !strings.Contains(string(data), `"observed":0`) {
		t.Fatalf("collection changed health or lost zero: %s", data)
	}
	unreadable := collectionReadError(result, os.ErrPermission)
	data, _ = json.Marshal(unreadable)
	if unreadable.Collection.State != collectionPermissionDenied || strings.Contains(string(data), `"observed"`) {
		t.Fatalf("permission became zero: %s", data)
	}
	if got := collectionReadError(result, errors.New("read failed")); got.Collection.State != collectionUnavailable {
		t.Fatalf("state %s", got.Collection.State)
	}
}

func TestCollectionRenderingDoesNotInferCoverageFromStatusOrWarnings(t *testing.T) {
	legacy := []Result{{Probe: "svc.state", Status: StatusFail, Warnings: []string{"permission denied"}}}
	var output strings.Builder
	printCollectionSummary(&output, legacy, false)
	printCollectionDetail(&output, legacy[0], true, false)
	if output.Len() != 0 {
		t.Fatalf("legacy result reclassified: %s", output.String())
	}
	results := []Result{withCollection(legacy[0], collectionObserved, intPointer(0), ""), withCollection(Result{}, collectionPartial, intPointer(2), "some files hidden"), withCollection(Result{}, collectionPermissionDenied, nil, "needs root")}
	printCollectionSummary(&output, results, false)
	text := output.String()
	for _, want := range []string{"1 observed", "1 partial", "1 permission denied"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	output.Reset()
	printCollectionDetail(&output, results[0], false, false)
	if output.Len() != 0 {
		t.Fatal("ordinary observed detail should stay compact")
	}
	printCollectionDetail(&output, results[0], true, false)
	if !strings.Contains(output.String(), "0 observed items") {
		t.Fatalf("zero missing: %s", output.String())
	}
}

func TestCollectionReasonUsesExistingReportRedaction(t *testing.T) {
	result := withCollection(Result{Status: StatusSkip}, collectionUnavailable, nil, "peer 192.0.2.1")
	report := buildReport("test", result.StartedAt, nil, []Result{result}, true)
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "192.0.2.1") || report.Results[0].Collection == nil {
		t.Fatalf("redaction: %s", data)
	}
}

func TestCollectionSummaryPreservesStatesAtTerminalWidths(t *testing.T) {
	language := currentLanguage()
	defer setLanguage(language)
	states := []collectionState{collectionObserved, collectionPartial, collectionPermissionDenied, collectionUnsupported, collectionUnavailable}
	var results []Result
	for _, state := range states {
		results = append(results, withCollection(Result{}, state, nil, ""))
	}
	for _, lang := range supportedLanguages {
		setLanguage(lang)
		for _, width := range []int{48, 80, 140} {
			for _, color := range []bool{false, true} {
				var output strings.Builder
				printCollectionSummaryWithStyle(&output, results, infoStyle{textStyle{color: color, width: width}})
				text := ansi.Strip(output.String())
				for _, state := range states {
					if !strings.Contains(text, collectionStateText(state)) {
						t.Fatalf("lost %s: %s", state, text)
					}
				}
				for _, line := range strings.Split(text, "\n") {
					if liveWidth(line) > width {
						t.Fatalf("%s width %d exceeded: %q", lang, width, line)
					}
				}
				if !color && strings.Contains(output.String(), "\x1b") {
					t.Fatal("plain output gained escapes")
				}
			}
		}
	}
}
