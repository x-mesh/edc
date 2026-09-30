package edc

import (
	"fmt"
	"testing"
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
