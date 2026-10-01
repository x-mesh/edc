package edc

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestIOTraceScreenFitsWidths(t *testing.T) {
	events := []ioEvent{{Process: "postgres-supervisor", Device: "259:123456", Operation: "write", Bytes: 1 << 20, Event: "io_complete", CgroupID: 123456, Attribution: "insert"}}
	for _, width := range []int{40, 80, 120} {
		model := newIOTraceScreenModel(ioTraceOptions{}, nil, nil, nil)
		model.width, model.height, model.events = width, 24, events
		for _, line := range strings.Split(model.View().Content, "\n") {
			if liveWidth(line) > width {
				t.Fatalf("width %d: %d %q", width, liveWidth(line), line)
			}
		}
	}
}

func TestIOTraceScreenViewsFilterAndDetail(t *testing.T) {
	model := newIOTraceScreenModel(ioTraceOptions{}, nil, nil, nil)
	model.events = []ioEvent{{Process: "postgres", Device: "8:0", Event: "io_complete", Bytes: 4096, CgroupID: 7}, {Process: "nginx", Device: "8:1", Event: "io_complete", Bytes: 512, CgroupID: 8}}
	for _, key := range []string{"d", "p", "c", "e", "g"} {
		next, _ := model.Update(tea.KeyPressMsg{Text: key})
		model = next.(ioTraceScreenModel)
	}
	next, _ := model.Update(tea.KeyPressMsg{Text: "/"})
	model = next.(ioTraceScreenModel)
	for _, rune := range "postgres" {
		next, _ = model.Update(tea.KeyPressMsg{Text: string(rune)})
		model = next.(ioTraceScreenModel)
	}
	next, _ = model.Update(tea.KeyPressMsg{Text: "enter"})
	model = next.(ioTraceScreenModel)
	if !strings.Contains(model.View().Content, "postgres") {
		t.Fatal("filter did not retain matching event")
	}
}
