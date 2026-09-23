package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

type listenWatchEvent struct {
	Type   string       `json:"type"`
	Time   time.Time    `json:"time"`
	Socket listenSocket `json:"socket"`
}

type listenWatchSummary struct {
	Type       string `json:"type"`
	Opened     int    `json:"opened"`
	Closed     int    `json:"closed"`
	Errors     int    `json:"errors"`
	DurationMS int64  `json:"duration_ms"`
}

func runListenWatch(options commonOptions, families listenFamilies, intervalText string, duration time.Duration) int {
	interval, err := parseObserveInterval(intervalText)
	if err != nil || duration < 0 {
		if err == nil {
			err = fmt.Errorf("%s", T("observe.watch.options_invalid"))
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	writer, closeOutput, err := openObserveStream(options.jsonPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer closeOutput()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	highlight := options.jsonPath == "" && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	return streamListenWatch(ctx, writer, interval, options.timeout, options.jsonPath != "", options.redact, highlight, func(ctx context.Context) Result {
		return probeListen(ctx, families)
	})
}

func streamListenWatch(ctx context.Context, writer io.Writer, interval, timeout time.Duration, jsonOutput, redact, highlight bool, probe func(context.Context) Result) int {
	started := time.Now()
	initialCtx, cancel := context.WithTimeout(ctx, timeout)
	initial := probe(initialCtx)
	cancel()
	if initial.Status != StatusPass {
		fmt.Fprintln(writer, listenWatchError(initial))
		return 1
	}
	previous := listenSocketsOf(initial)
	if jsonOutput {
		if err := writeListenWatchJSON(writer, map[string]interface{}{"type": "snapshot", "time": time.Now().UTC(), "sockets": previous}, redact); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		fmt.Fprintf(writer, "%s  %s\n", time.Now().Format("15:04:05"), initial.Summary)
		if table := listenTableOf(initial, false); table != "" {
			if redact {
				table = redactIPAddresses(table)
			}
			fmt.Fprintln(writer, table)
		}
	}
	summary := listenWatchSummary{Type: "summary"}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			summary.DurationMS = time.Since(started).Milliseconds()
			if jsonOutput {
				if err := writeListenWatchJSON(writer, summary, redact); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 2
				}
			} else {
				fmt.Fprintln(writer, T("observe.listen.watch_summary", summary.Opened, summary.Closed, summary.Errors, time.Duration(summary.DurationMS)*time.Millisecond))
			}
			if summary.Errors > 0 {
				return 1
			}
			return 0
		case <-ticker.C:
		}
		sampleCtx, stopSample := context.WithTimeout(ctx, timeout)
		result := probe(sampleCtx)
		stopSample()
		if ctx.Err() != nil {
			continue
		}
		if result.Status != StatusPass {
			summary.Errors++
			message := listenWatchError(result)
			if redact {
				message = redactIPAddresses(message)
			}
			if jsonOutput {
				if err := writeListenWatchJSON(writer, map[string]interface{}{"type": "error", "time": time.Now().UTC(), "message": message}, false); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 2
				}
			} else {
				fmt.Fprintf(writer, "%s  ERROR  %s\n", time.Now().Format("15:04:05"), message)
			}
			continue
		}
		current := listenSocketsOf(result)
		opened, closed := diffListenSockets(previous, current)
		previous = current
		summary.Opened += len(opened)
		summary.Closed += len(closed)
		for _, change := range []struct {
			kind    string
			sockets []listenSocket
		}{{"open", opened}, {"close", closed}} {
			for _, socket := range change.sockets {
				event := listenWatchEvent{Type: change.kind, Time: time.Now().UTC(), Socket: socket}
				if jsonOutput {
					if err := writeListenWatchJSON(writer, event, redact); err != nil {
						fmt.Fprintln(os.Stderr, err)
						return 2
					}
				} else {
					fmt.Fprintln(writer, formatListenWatchEvent(event, redact, highlight))
				}
			}
		}
	}
}

func formatListenWatchEvent(event listenWatchEvent, redact, highlight bool) string {
	socket := event.Socket
	line := fmt.Sprintf("%s  %-5s  %-4s  %-25s  %s (%s)", event.Time.Local().Format("15:04:05"), strings.ToUpper(event.Type), socket.Proto, socket.Address, socket.Process, socket.PID)
	if redact {
		line = redactIPAddresses(line)
	}
	if highlight {
		return liveReverse + line + liveReset
	}
	return line
}

func listenWatchError(result Result) string {
	if len(result.Warnings) > 0 {
		return firstLine(strings.Join(result.Warnings, ", "))
	}
	return firstLine(result.Summary)
}

func listenSocketsOf(result Result) []listenSocket {
	sockets, _ := result.Metrics["sockets"].([]listenSocket)
	return sockets
}

func diffListenSockets(before, after []listenSocket) (opened, closed []listenSocket) {
	old := make(map[listenSocketKey]listenSocket, len(before))
	current := make(map[listenSocketKey]listenSocket, len(after))
	for _, socket := range before {
		old[listenKeyOf(socket)] = socket
	}
	for _, socket := range after {
		current[listenKeyOf(socket)] = socket
		if _, exists := old[listenKeyOf(socket)]; !exists {
			opened = append(opened, socket)
		}
	}
	for key, socket := range old {
		if _, exists := current[key]; !exists {
			closed = append(closed, socket)
		}
	}
	order := func(values []listenSocket) {
		sort.Slice(values, func(i, j int) bool {
			return fmt.Sprintf("%s:%05d:%s:%s", values[i].Proto, values[i].Port, values[i].Address, values[i].PID) < fmt.Sprintf("%s:%05d:%s:%s", values[j].Proto, values[j].Port, values[j].Address, values[j].PID)
		})
	}
	order(opened)
	order(closed)
	return opened, closed
}

func writeListenWatchJSON(writer io.Writer, value interface{}, redact bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if redact {
		data = []byte(redactIPAddresses(string(data)))
	}
	_, err = fmt.Fprintln(writer, string(data))
	return err
}
