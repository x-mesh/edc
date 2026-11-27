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

func BenchmarkTraceScreenView(b *testing.B) {
	base := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	events := make([]captureEvent, traceScreenEventLimit)
	for index := range events {
		events[index] = captureEvent{
			Protocol:    "http",
			Event:       traceHTTPRequestEvent,
			Process:     "service",
			Source:      "10.0.0.1:54321",
			Target:      "api.example",
			Method:      "GET",
			Path:        fmt.Sprintf("/resources/%d", index),
			TimestampNS: uint64(index + 1),
		}
	}
	next, _ := base.Update(traceEventMsg{events: events})
	base = next.(traceScreenModel)
	base.width, base.height = 120, 30

	for _, benchmark := range []struct {
		name  string
		width int
	}{
		{name: "wide", width: 120},
		{name: "narrow", width: 80},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			model := base
			model.width = benchmark.width
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				_ = model.View()
			}
		})
	}
}

func BenchmarkTraceScreenPairedDetailSnapshot(b *testing.B) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	events := make([]captureEvent, 0, traceScreenEventLimit)
	for index := 0; index < traceScreenEventLimit/2; index++ {
		events = append(events,
			captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, requestID: uint64(index + 1), Method: "GET", Target: "api.example", Path: fmt.Sprintf("/resources/%d", index)},
			captureEvent{Protocol: "http", Event: "http_response", requestID: uint64(index + 1), Target: "api.example", Path: fmt.Sprintf("/resources/%d", index)},
		)
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = next.(traceScreenModel)
	number := model.first + len(model.events) - 2
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = model.detailSnapshot(number, 0)
	}
}

func BenchmarkTraceScreenGroupReport(b *testing.B) {
	model := newTraceScreenModel("http", tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	events := make([]captureEvent, traceScreenEventLimit)
	for index := range events {
		events[index] = captureEvent{
			Protocol: "http", Event: traceHTTPRequestEvent, Process: fmt.Sprintf("service-%d", index%10),
			Source: "10.0.0.1:54321", Target: fmt.Sprintf("api-%d.example", index%100),
			Method: "GET", Path: fmt.Sprintf("/resources/%d", index),
		}
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = next.(traceScreenModel)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = model.groupReport()
	}
}
