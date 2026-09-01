package edc

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type remoteDisplay struct {
	mu      sync.Mutex
	output  io.Writer
	verbose bool
	spinner bool
	color   bool
	label   string
	stop    chan struct{}
	done    chan struct{}
}

func newRemoteDisplay(output io.Writer, verbose, spinner bool) *remoteDisplay {
	display := &remoteDisplay{output: output, verbose: verbose, spinner: spinner && !verbose, color: spinner && os.Getenv("NO_COLOR") == ""}
	if display.spinner {
		display.stop = make(chan struct{})
		display.done = make(chan struct{})
		go display.spin()
	}
	return display
}

func (display *remoteDisplay) Start(host, step, phase string) {
	display.mu.Lock()
	defer display.mu.Unlock()
	display.label = fmt.Sprintf("%s / %s / %s", host, step, phase)
	if display.verbose {
		fmt.Fprintf(display.output, "\n%s\n", display.phaseLabel(host, step, phase))
	}
}

func (display *remoteDisplay) WriteLine(prefix, line string) {
	display.mu.Lock()
	defer display.mu.Unlock()
	if display.color {
		fmt.Fprintf(display.output, "\033[90m  %-30s │\033[0m %s\n", prefix, line)
		return
	}
	fmt.Fprintf(display.output, "  %-30s | %s\n", prefix, line)
}

func (display *remoteDisplay) phaseLabel(host, step, phase string) string {
	label := fmt.Sprintf("› %s  %s  %s", host, step, phase)
	if display.color {
		return "\033[90m" + label + "\033[0m"
	}
	return label
}

func (display *remoteDisplay) Close() {
	if !display.spinner {
		return
	}
	close(display.stop)
	<-display.done
	display.mu.Lock()
	fmt.Fprint(display.output, "\r\033[2K")
	display.mu.Unlock()
}

func (display *remoteDisplay) spin() {
	defer close(display.done)
	frames := []string{"|", "/", "-", "\\"}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	index := 0
	for {
		select {
		case <-display.stop:
			return
		case <-ticker.C:
			display.mu.Lock()
			fmt.Fprintf(display.output, "\r\033[2K%s %s", frames[index%len(frames)], display.label)
			display.mu.Unlock()
			index++
		}
	}
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
