package edc

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func BenchmarkTraceScreenMySQLRows(b *testing.B) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	events := make([]captureEvent, 0, traceScreenEventLimit)
	for index := 0; index < traceScreenEventLimit/2; index++ {
		sql := fmt.Sprintf("SELECT %d", index)
		events = append(events,
			captureEvent{SocketID: uint64(index), Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}},
			captureEvent{SocketID: uint64(index), Protocol: "mysql", Event: mysqlEventResult, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}},
		)
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = next.(traceScreenModel)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = model.displayRows()
	}
}

func BenchmarkTraceScreenMySQLAppendEvict(b *testing.B) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	for index := 0; index < traceScreenEventLimit; index++ {
		event := captureEvent{SocketID: uint64(index), Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: fmt.Sprintf("SELECT %d", index)}}
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		event := captureEvent{SocketID: uint64(index), Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: fmt.Sprintf("SELECT %d", index)}}
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
	}
}

func BenchmarkTraceScreenDetailSchedule(b *testing.B) {
	body := strings.Repeat("界", (1<<20)/3)
	makeModel := func() traceScreenModel {
		model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height = 80, 24
		event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "POST", Target: "api.example", Path: "/fixture", Payload: "POST /fixture HTTP/1.1\r\n\r\n" + body}
		model.events = []captureEvent{cloneTraceEvent(event)}
		model.payloadRevisions[0] = 1
		model.payloads.keep(0, event.Payload)
		model.selected = 0
		return model
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		b.StopTimer()
		model := makeModel()
		b.StartTimer()
		next, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil || next.(traceScreenModel).detail == nil {
			b.Fatal("detail schedule did not return its first-frame shell and worker command")
		}
	}
}

func BenchmarkTraceScreenPreviewCachedNavigation(b *testing.B) {
	body := strings.Repeat("x", 1<<20)
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height, model.split = 80, 24, true
	model.events = []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/fixture", Payload: "GET /fixture HTTP/1.1\r\n\r\n" + body}}
	model.payloadRevisions[0] = 1
	model.payloads.keep(0, model.events[0].Payload)
	cmd := model.requestPreviewBuild()
	result := cmd().(traceDetailBuiltMsg)
	next, _ := model.Update(result)
	model = next.(traceScreenModel)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		next, _ := model.Update(tea.KeyPressMsg{Code: 'K', Text: "K"})
		model = next.(traceScreenModel)
	}
}
