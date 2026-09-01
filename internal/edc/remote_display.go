package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type remoteDisplayOptions struct {
	verbose bool
	spinner bool
	results bool
	redact  bool
}

type remoteDisplay struct {
	mu      sync.Mutex
	output  io.Writer
	verbose bool
	spinner bool
	status  bool
	color   bool
	results bool
	redact  bool
	label   string
	stop    chan struct{}
	done    chan struct{}
}

func newRemoteDisplay(output io.Writer, options remoteDisplayOptions) *remoteDisplay {
	terminal := options.spinner && os.Getenv("NO_COLOR") == ""
	display := &remoteDisplay{
		output: output, verbose: options.verbose, spinner: terminal && !options.verbose,
		status: terminal, color: terminal, results: options.results, redact: options.redact,
	}
	if display.status {
		display.stop = make(chan struct{})
		display.done = make(chan struct{})
		go display.spin()
	}
	return display
}

// Result는 step이 끝날 때마다 PASS/FAIL 줄을 바로 출력한다. 최종 출력은 실패 상세와 요약만 남는다.
func (display *remoteDisplay) Result(result Result) {
	if display == nil || !display.results {
		return
	}
	line := formatResultLine(result, display.color)
	if display.redact {
		line = redactIPAddresses(line)
	}
	display.mu.Lock()
	defer display.mu.Unlock()
	display.clearStatusLocked()
	fmt.Fprint(display.output, line)
	display.renderStatusLocked(0)
}

func (display *remoteDisplay) Start(host, step, phase string) {
	display.mu.Lock()
	defer display.mu.Unlock()
	display.label = fmt.Sprintf("%s / %s / %s", host, step, phase)
	if display.verbose {
		display.clearStatusLocked()
		fmt.Fprintf(display.output, "\n%s\n", display.phaseLabel(host, step, phase))
		display.renderStatusLocked(0)
	}
}

func (display *remoteDisplay) WriteLine(prefix, line string) {
	display.mu.Lock()
	defer display.mu.Unlock()
	display.clearStatusLocked()
	if display.color {
		fmt.Fprintf(display.output, "\033[90m  %-30s │\033[0m %s\n", prefix, line)
	} else {
		fmt.Fprintf(display.output, "  %-30s | %s\n", prefix, line)
	}
	display.renderStatusLocked(0)
}

func (display *remoteDisplay) phaseLabel(host, step, phase string) string {
	label := fmt.Sprintf("› %s  %s  %s", host, step, phase)
	if display.color {
		return "\033[90m" + label + "\033[0m"
	}
	return label
}

func (display *remoteDisplay) Close() {
	if !display.status {
		return
	}
	close(display.stop)
	<-display.done
	display.mu.Lock()
	display.clearStatusLocked()
	display.mu.Unlock()
}

func (display *remoteDisplay) spin() {
	defer close(display.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	index := 0
	for {
		select {
		case <-display.stop:
			return
		case <-ticker.C:
			display.mu.Lock()
			display.renderStatusLocked(index)
			display.mu.Unlock()
			index++
		}
	}
}

func (display *remoteDisplay) clearStatusLocked() {
	if display.status {
		fmt.Fprint(display.output, "\r\033[2K")
	}
}

func (display *remoteDisplay) renderStatusLocked(frame int) {
	if !display.status || display.label == "" {
		return
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	fmt.Fprintf(display.output, "\r\033[2K\033[97m%s  running  %s\033[0m", frames[frame%len(frames)], display.label)
}

type remoteStreamWriter struct {
	mu      sync.Mutex
	display *remoteDisplay
	prefix  string
	pending string
}

func (writer *remoteStreamWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.pending += string(data)
	for {
		index := strings.IndexByte(writer.pending, '\n')
		if index < 0 {
			break
		}
		writer.display.WriteLine(writer.prefix, writer.pending[:index])
		writer.pending = writer.pending[index+1:]
	}
	return len(data), nil
}

func (writer *remoteStreamWriter) Flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.pending != "" {
		writer.display.WriteLine(writer.prefix, writer.pending)
		writer.pending = ""
	}
}
