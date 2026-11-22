package edc

import (
	"strings"
	"testing"
	"time"
)

func topDeltaRows(values ...uint64) []topDashboardRow {
	start := time.Unix(1_800_000_000, 0)
	rows := make([]topDashboardRow, 0, len(values))
	for i, value := range values {
		at := start.Add(time.Duration(i) * time.Minute)
		rows = append(rows, topDashboardRow{at: at, processesAt: at, processesValid: true, filter: "42", processTotal: topProcessTotal{Count: 1}, processes: []topProcess{{PID: 42, Started: start.Add(-time.Hour), RSS: value, Command: "worker"}}})
	}
	return rows
}

func TestTopProcessDeltaSignedEndpointsAndObservedCount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []uint64
		change string
	}{
		{"increase", []uint64{100 << 20, 120 << 20, 150 << 20}, "+50.0 MB"},
		{"decrease", []uint64{150 << 20, 120 << 20, 100 << 20}, "-50.0 MB"},
		{"unchanged", []uint64{100 << 20, 100 << 20, 100 << 20}, "0 B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := topDeltaRows(tc.values...)
			delta := topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
			if delta.Unavailable != "" || delta.Samples != 3 || delta.Window != 2*time.Minute || delta.changeText() != tc.change {
				t.Fatalf("delta: %+v %s", delta, delta.changeText())
			}
		})
	}
	maximum := ^uint64(0)
	for _, values := range [][]uint64{{0, maximum}, {maximum, 0}} {
		rows := topDeltaRows(values...)
		delta := topObservedProcessDelta(rows, 1, rows[1].processes[0], "42")
		sign := "+"
		if values[0] > values[1] {
			sign = "-"
		}
		if delta.Before != values[0] || delta.After != values[1] || !strings.HasPrefix(delta.changeText(), sign) {
			t.Fatalf("unsigned endpoints overflowed: %+v", delta)
		}
	}
}

func TestTopProcessDeltaDeduplicatesCachedObservationTimes(t *testing.T) {
	rows := topDeltaRows(100, 100, 150)
	rows[1].processesAt = rows[0].processesAt
	delta := topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
	if delta.Samples != 2 || delta.Window != 2*time.Minute || delta.Before != 100 || delta.After != 150 {
		t.Fatalf("cached sample counted twice: %+v", delta)
	}
	for i := range rows {
		rows[i].processesAt = rows[0].processesAt
	}
	delta = topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
	if delta.Samples != 1 || delta.Unavailable == "" {
		t.Fatalf("one cached observation became a zero delta: %+v", delta)
	}
}

func TestTopProcessDeltaStopsAtMissingInvalidAndDifferentFilterRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]topDashboardRow)
	}{
		{"missing process", func(rows []topDashboardRow) { rows[1].processes = nil }},
		{"invalid collection", func(rows []topDashboardRow) { rows[1].processesValid = false }},
		{"different filter", func(rows []topDashboardRow) { rows[1].filter = "other" }},
		{"unknown observation time", func(rows []topDashboardRow) { rows[1].processesAt = time.Time{} }},
		{"reused PID", func(rows []topDashboardRow) {
			rows[1].processes[0].Started = rows[1].processes[0].Started.Add(time.Hour)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := topDeltaRows(100, 120, 150)
			tc.edit(rows)
			delta := topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
			if delta.Unavailable == "" || delta.Samples != 1 {
				t.Fatalf("comparison bridged a missing segment: %+v", delta)
			}
		})
	}
	rows := topDeltaRows(100, 120)
	rows[1].processes[0].Started = time.Time{}
	if delta := topObservedProcessDelta(rows, 1, rows[1].processes[0], "42"); delta.Unavailable == "" {
		t.Fatal("unknown lifetime compared")
	}
	rows[1].processes[0].PID = 0
	if delta := topObservedProcessDelta(rows, 1, rows[1].processes[0], "42"); delta.Unavailable == "" {
		t.Fatal("unknown PID compared")
	}
}

func TestTopProcessDeltaUsesOnlySelectedLifetimeAndPastWindow(t *testing.T) {
	rows := topDeltaRows(100, 500, 550)
	rows[0].processes[0].Started = rows[0].processes[0].Started.Add(-time.Hour)
	delta := topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
	if delta.Unavailable != "" || delta.Samples != 2 || delta.Before != 500 || delta.After != 550 || delta.Window != time.Minute {
		t.Fatalf("lifetimes mixed: %+v", delta)
	}
	rows = topDeltaRows(100, 150, 1000)
	delta = topObservedProcessDelta(rows, 1, rows[1].processes[0], "42")
	if delta.Samples != 2 || delta.After != 150 || delta.Window != time.Minute {
		t.Fatalf("past row used future samples: %+v", delta)
	}
	rows = topDeltaRows(0, 50, 100, 150)
	for i := range rows {
		at := rows[0].at.Add(time.Duration(i) * 5 * time.Minute)
		rows[i].at, rows[i].processesAt = at, at
	}
	delta = topObservedProcessDelta(rows, 3, rows[3].processes[0], "42")
	if delta.Samples != 3 || delta.Window != 10*time.Minute || delta.Before != 50 || delta.After != 150 {
		t.Fatalf("window cap not respected: %+v", delta)
	}
}

func TestTopProcessDeltaReportsActualIrregularSpanWithoutExtrapolation(t *testing.T) {
	rows := topDeltaRows(100, 120, 150)
	rows[1].processesAt = rows[0].at.Add(30 * time.Second)
	rows[2].processesAt = rows[0].at.Add(4 * time.Minute)
	delta := topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
	if delta.Samples != 3 || delta.Window != 4*time.Minute || delta.After-delta.Before != 50 {
		t.Fatalf("gap changed measured endpoints: %+v", delta)
	}
	rows[0].processesAt = rows[2].processesAt.Add(time.Minute)
	delta = topObservedProcessDelta(rows, 2, rows[2].processes[0], "42")
	if delta.Samples != 2 || delta.Before != 120 || delta.Window != 3*time.Minute+30*time.Second {
		t.Fatalf("non-monotonic observation included: %+v", delta)
	}
	if text := (topProcessDelta{Window: time.Microsecond}).windowText(); text == "0s" {
		t.Fatal("real nonzero window rounded to zero")
	}
}
