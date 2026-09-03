package edc

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

type logOptions struct {
	stream         string
	output         string
	commandDisplay string
	command        []string
}

type logStreams struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type logCopyResult struct {
	wrote    bool
	last     byte
	writeErr error
}

func runLog(args []string) int {
	return runLogWithStreams(args, logStreams{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr})
}

func runLogWithStreams(args []string, streams logStreams) int {
	options, ok := parseLogOptions(args, streams.stderr)
	if !ok {
		return 2
	}

	logFile, err := openLogFile(options.output)
	if err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.open_failed", options.output, err))
		return 2
	}
	defer logFile.Close()

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	interrupted, err := lockLogFile(logFile, signals)
	if err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.lock_failed", options.output, err))
		return 2
	}
	if interrupted != nil {
		return signalExitCode(interrupted)
	}
	defer unlockLogFile(logFile)

	started := time.Now()
	if err := writeLogStart(logFile, started, options); err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, err))
		return 2
	}
	if err := logFile.Sync(); err != nil {
		_ = writeLogEnd(logFile, started, "status=log_error exit=2", logCopyResult{})
		fmt.Fprintln(streams.stderr, T("cli.log.sync_failed", options.output, err))
		return 2
	}

	process := exec.Command(options.command[0], options.command[1:]...)
	process.Stdin = streams.stdin
	configureLogProcess(process)

	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return finishLogStartError(logFile, streams.stderr, options.output, started, err)
	}
	if options.stream == "stdout" {
		process.Stdout = writePipe
		process.Stderr = streams.stderr
	} else {
		process.Stdout = streams.stdout
		process.Stderr = writePipe
	}

	if err := process.Start(); err != nil {
		readPipe.Close()
		writePipe.Close()
		return finishLogStartError(logFile, streams.stderr, options.output, started, err)
	}
	// The child owns its duplicate. Keeping this descriptor open would prevent EOF.
	writePipe.Close()

	copyDone := make(chan logCopyResult, 1)
	go func() {
		result := copyLogStream(logFile, readPipe)
		readPipe.Close()
		copyDone <- result
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- process.Wait() }()

	var waitErr error
waitLoop:
	for {
		select {
		case received := <-signals:
			if err := signalLogProcess(process, received); err != nil {
				fmt.Fprintln(streams.stderr, T("cli.log.signal_failed", err))
			}
		case waitErr = <-waitDone:
			break waitLoop
		}
	}
	copyResult := <-copyDone

	status, exitCode, childErr := logProcessStatus(process, waitErr)
	if copyResult.writeErr != nil {
		status = "status=log_error exit=2"
		exitCode = 2
		fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, copyResult.writeErr))
	} else if childErr != nil {
		status = "status=wait_error exit=2"
		exitCode = 2
		fmt.Fprintln(streams.stderr, T("cli.log.wait_failed", childErr))
	}

	if err := writeLogEnd(logFile, started, status, copyResult); err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, err))
		return 2
	}
	if err := logFile.Sync(); err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.sync_failed", options.output, err))
		return 2
	}
	return exitCode
}

func parseLogOptions(args []string, stderr io.Writer) (logOptions, bool) {
	var options logOptions
	separator := -1
	for index, argument := range args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(stderr, T("cli.log.command_required"))
		return logOptions{}, false
	}
	set := flag.NewFlagSet("log", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.stream, "stream", "", T("command.log.option.stream"))
	set.StringVar(&options.output, "output", "", T("command.log.option.output"))
	set.StringVar(&options.commandDisplay, "command-display", "full", T("command.log.option.command_display"))
	if err := set.Parse(args[:separator]); err != nil {
		return logOptions{}, false
	}
	if set.NArg() != 0 {
		fmt.Fprintln(stderr, T("cli.log.command_required"))
		return logOptions{}, false
	}
	options.command = args[separator+1:]
	if options.stream != "stdout" && options.stream != "stderr" {
		fmt.Fprintln(stderr, T("cli.log.stream_value"))
		return logOptions{}, false
	}
	if options.output == "" {
		fmt.Fprintln(stderr, T("cli.log.output_required"))
		return logOptions{}, false
	}
	if options.output == "-" {
		fmt.Fprintln(stderr, T("cli.log.output_file_required"))
		return logOptions{}, false
	}
	if options.commandDisplay != "full" && options.commandDisplay != "name" && options.commandDisplay != "none" {
		fmt.Fprintln(stderr, T("cli.log.command_display_value"))
		return logOptions{}, false
	}
	if len(options.command) == 0 {
		fmt.Fprintln(stderr, T("cli.log.command_required"))
		return logOptions{}, false
	}
	return options, true
}

func openLogFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if chmodErr := file.Chmod(0o600); chmodErr != nil {
			file.Close()
			return nil, chmodErr
		}
		return file, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
}

func writeLogStart(writer io.Writer, started time.Time, options logOptions) error {
	command := options.command
	if options.commandDisplay == "name" {
		command = []string{filepath.Base(command[0])}
	}
	field := ""
	if options.commandDisplay != "none" {
		field = " command=" + asciiJSON(command)
	}
	_, err := fmt.Fprintf(writer, "=== edc log start time=%s stream=%s%s ===\n", started.Format(time.RFC3339Nano), options.stream, field)
	return err
}

func finishLogStartError(file *os.File, stderr io.Writer, path string, started time.Time, cause error) int {
	fmt.Fprintln(stderr, T("cli.log.start_failed", cause))
	if err := writeLogEnd(file, started, "status=start_error exit=2", logCopyResult{}); err != nil {
		fmt.Fprintln(stderr, T("cli.log.write_failed", path, err))
		return 2
	}
	if err := file.Sync(); err != nil {
		fmt.Fprintln(stderr, T("cli.log.sync_failed", path, err))
	}
	return 2
}

func writeLogEnd(writer io.Writer, started time.Time, status string, copied logCopyResult) error {
	if copied.wrote && copied.last != '\n' {
		if _, err := io.WriteString(writer, "\n"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(writer, "=== edc log end time=%s %s duration=%s ===\n", time.Now().Format(time.RFC3339Nano), status, time.Since(started).Round(time.Microsecond))
	return err
}

func copyLogStream(writer io.Writer, reader io.Reader) logCopyResult {
	buffer := make([]byte, 32*1024)
	var result logCopyResult
	for {
		read, readErr := reader.Read(buffer)
		if read > 0 && result.writeErr == nil {
			written, writeErr := writer.Write(buffer[:read])
			if written > 0 {
				result.wrote = true
				result.last = buffer[written-1]
			}
			if writeErr != nil {
				result.writeErr = writeErr
			} else if written != read {
				result.writeErr = io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr != io.EOF && result.writeErr == nil {
				result.writeErr = readErr
			}
			return result
		}
	}
}

func logProcessStatus(process *exec.Cmd, waitErr error) (string, int, error) {
	if waitErr == nil {
		return "status=exit exit=0", 0, nil
	}
	var exitError *exec.ExitError
	if !errors.As(waitErr, &exitError) {
		return "", 2, waitErr
	}
	waitStatus, ok := process.ProcessState.Sys().(syscall.WaitStatus)
	if ok && waitStatus.Signaled() {
		received := waitStatus.Signal()
		exitCode := 128 + int(received)
		return fmt.Sprintf("status=signal signal=%s exit=%d", unixSignalName(received), exitCode), exitCode, nil
	}
	exitCode := process.ProcessState.ExitCode()
	return fmt.Sprintf("status=exit exit=%d", exitCode), exitCode, nil
}

func signalExitCode(received os.Signal) int {
	if unixSignal, ok := received.(syscall.Signal); ok {
		return 128 + int(unixSignal)
	}
	return 2
}

func asciiJSON(value any) string {
	encoded, _ := json.Marshal(value)
	var builder strings.Builder
	for len(encoded) > 0 {
		r, size := utf8.DecodeRune(encoded)
		encoded = encoded[size:]
		if r <= utf8.RuneSelf {
			builder.WriteRune(r)
			continue
		}
		if r <= 0xffff {
			fmt.Fprintf(&builder, "\\u%04x", r)
			continue
		}
		high, low := utf16.EncodeRune(r)
		fmt.Fprintf(&builder, "\\u%04x\\u%04x", high, low)
	}
	return builder.String()
}
