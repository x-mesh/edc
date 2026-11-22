package edc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEventsTimelineMetricsPreservesOffsetsAndEmptyWindow(t *testing.T) {
	from := time.Date(2026, 11, 23, 10, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	to := from.Add(time.Hour)
	signals := []eventsSignal{{Time: from.Add(time.Minute), Kind: "oom", Subject: "worker", Brief: "worker killed"}}
	for _, result := range []Result{
		eventsWindow(eventsFacts{Start: from, Now: to, Since: time.Hour, Signals: signals}),
		evaluateMacEvents(macEventsFacts{Start: from, Now: to, Since: time.Hour, Signals: signals})[0],
	} {
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var saved struct {
			Metrics struct {
				From    time.Time             `json:"window_from"`
				To      time.Time             `json:"window_to"`
				Entries []eventsTimelineEntry `json:"timeline"`
			} `json:"metrics"`
		}
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if !saved.Metrics.From.Equal(from.UTC()) || !saved.Metrics.To.Equal(to.UTC()) || len(saved.Metrics.Entries) != 1 || !saved.Metrics.Entries[0].Time.Equal(signals[0].Time.UTC()) || saved.Metrics.Entries[0].Kind != "oom" {
			t.Fatalf("lost timeline instant: %s", data)
		}
		if !strings.Contains(string(data), "+09:00") || len(result.Evidence) == 0 {
			t.Fatalf("lost offset or existing evidence: %s", data)
		}
	}
	empty := eventsWindow(eventsFacts{Start: from, Now: to, Since: time.Hour})
	data, _ := json.Marshal(empty)
	if !strings.Contains(string(data), `"timeline":[]`) {
		t.Fatalf("observed empty timeline must not be null: %s", data)
	}
	unreadable := errors.New("unavailable")
	for _, result := range []Result{eventsWindow(eventsFacts{KernelErr: unreadable, JournalErr: unreadable}), evaluateMacEvents(macEventsFacts{Err: unreadable})[0]} {
		if result.Status != StatusSkip || result.Metrics != nil {
			t.Fatalf("unavailable collection became empty observed: %+v", result)
		}
	}
}

func TestEventsTimelineMetricsBoundsEntries(t *testing.T) {
	from := time.Unix(1000, 0).UTC()
	signals := make([]eventsSignal, eventsTimelineLines+3)
	for i := range signals {
		signals[i] = eventsSignal{Time: from.Add(time.Duration(len(signals)-i) * time.Second), Kind: "link", Brief: "changed"}
	}
	original := signals[0].Time
	metrics := eventsTimelineMetrics(from, from.Add(time.Hour), signals)
	entries := metrics["timeline"].([]eventsTimelineEntry)
	if len(entries) != eventsTimelineLines || metrics["timeline_truncated"] != 3 || !entries[0].Time.Equal(from.Add(4*time.Second)) || !signals[0].Time.Equal(original) {
		t.Fatalf("timeline bound or input mutation: %+v", metrics)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatal("timeline order is not chronological")
		}
	}
}
