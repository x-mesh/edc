package edc

import "time"

const topProcessDeltaWindow = 10 * time.Minute

type topProcessDelta struct {
	Before, After uint64
	Window        time.Duration
	Samples       int
	Unavailable   string
}

func topObservedProcessDelta(rows []topDashboardRow, selected int, process topProcess, filter string) topProcessDelta {
	delta := topProcessDelta{}
	if process.PID <= 0 || process.Started.IsZero() {
		delta.Unavailable = "process identity unknown"
		return delta
	}
	if selected < 0 || selected >= len(rows) {
		delta.Unavailable = "no process observation"
		return delta
	}
	end := rows[selected].processesAt
	if end.IsZero() {
		delta.Unavailable = "process observation time unknown"
		return delta
	}
	cutoff := end.Add(-topProcessDeltaWindow)
	first, last := time.Time{}, time.Time{}
	for index := selected; index >= 0; index-- {
		row := rows[index]
		if row.filter != filter {
			delta.Unavailable = "filter changed"
			break
		}
		if !row.processesValid {
			delta.Unavailable = "process collection unavailable"
			break
		}
		var observed *topProcess
		for i := range row.processes {
			if row.processes[i].PID == process.PID {
				observed = &row.processes[i]
				break
			}
		}
		if observed == nil {
			delta.Unavailable = "process was not observed"
			break
		}
		if observed.Started.IsZero() || !observed.Started.Equal(process.Started) {
			delta.Unavailable = "process identity changed"
			break
		}
		at := row.processesAt
		if at.IsZero() {
			delta.Unavailable = "process observation time unknown"
			break
		}
		if !last.IsZero() && at.After(first) {
			delta.Unavailable = "process observation time moved backwards"
			break
		}
		if at.Before(cutoff) {
			break
		}
		if !first.IsZero() && at.Equal(first) {
			continue
		}
		if delta.Samples == 0 {
			delta.After = observed.RSS
			last = at
		}
		delta.Before = observed.RSS
		first = at
		delta.Samples++
	}
	if delta.Samples >= 2 {
		delta.Window = last.Sub(first)
		delta.Unavailable = ""
	} else if delta.Unavailable == "" {
		delta.Unavailable = "need two distinct process observations"
	}
	return delta
}

func (delta topProcessDelta) changeText() string {
	switch {
	case delta.After > delta.Before:
		return "+" + formatSizeShort(delta.After-delta.Before)
	case delta.After < delta.Before:
		return "-" + formatSizeShort(delta.Before-delta.After)
	default:
		return formatSizeShort(0)
	}
}

func (delta topProcessDelta) windowText() string {
	if delta.Window < time.Millisecond {
		return delta.Window.String()
	}
	precision := time.Second
	if delta.Window < time.Second {
		precision = time.Millisecond
	}
	return delta.Window.Round(precision).String()
}
