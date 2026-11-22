package edc

import (
	"sort"
	"time"
)

type eventsTimelineEntry struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject,omitempty"`
	Summary string    `json:"summary"`
}

func eventsTimelineMetrics(from, to time.Time, signals []eventsSignal) map[string]interface{} {
	ordered := append([]eventsSignal(nil), signals...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Time.Before(ordered[j].Time) })
	dropped := max(0, len(ordered)-eventsTimelineLines)
	entries := make([]eventsTimelineEntry, 0, len(ordered)-dropped)
	for _, signal := range ordered[dropped:] {
		entries = append(entries, eventsTimelineEntry{Time: signal.Time, Kind: signal.Kind, Subject: signal.Subject, Summary: signal.Brief})
	}
	return map[string]interface{}{"window_from": from, "window_to": to, "timeline": entries, "timeline_truncated": dropped}
}
