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
	stream                            string
	output                            string
	commandDisplay                    string
	command                           []string
	maxSizeMB, keepFiles, maxRestarts int
	restart                           string
	restartDelay, timeout, killAfter  time.Duration
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
	readErr  error
}

func runLog(args []string) int {
	return runLogWithStreams(args, logStreams{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr})
}

func runLogWithStreams(args []string, streams logStreams) int {
	options, ok := parseLogOptions(args, streams.stderr)
	if !ok {
		return 2
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if options.output == "" {
		path, err := createDefaultLogOutput(options.command[0], defaultLogDirectory())
		if err != nil {
			fmt.Fprintln(streams.stderr, T("cli.log.open_failed", "automatic output", err))
			return 2
		}
		options.output = path
		if terminal, ok := streams.stderr.(*os.File); ok && isTerminal(terminal) {
			fmt.Fprintln(streams.stderr, T("cli.log.output_path", path))
		}
	}
	if err := ensureRecommendedLogDirectory(options.output); err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.open_failed", options.output, err))
		return 2
	}
	lock, err := openLogFile(options.output + ".lock")
	if err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.lock_failed", options.output, err))
		return 2
	}
	defer lock.Close()
	interrupted, err := lockLogFile(lock, signals)
	if err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.lock_failed", options.output, err))
		return 2
	}
	if interrupted != nil {
		return signalExitCode(interrupted)
	}
	defer unlockLogFile(lock)
	file, err := openRotatingLog(options.output, int64(options.maxSizeMB)*logMiB, options.keepFiles)
	if err != nil {
		fmt.Fprintln(streams.stderr, T("cli.log.open_failed", options.output, err))
		return 2
	}
	defer file.Close()
	for attempt := 0; ; attempt++ {
		select {
		case received := <-signals:
			return signalExitCode(received)
		default:
		}
		result := runLogAttempt(options, streams, file, signals, attempt+1)
		if result.stopped || !result.started || result.wrapperError || options.restart == "never" || options.restart == "on-failure" && result.code == 0 {
			return result.code
		}
		if attempt >= options.maxRestarts {
			if _, err := fmt.Fprintf(file, "=== edc log stopped reason=restart_limit attempts=%d exit=%d ===\n", attempt+1, result.code); err != nil {
				fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, err))
				return 2
			}
			if err := file.Sync(); err != nil {
				fmt.Fprintln(streams.stderr, T("cli.log.sync_failed", options.output, err))
				return 2
			}
			return result.code
		}
		if _, err := fmt.Fprintf(file, "=== edc log restart next_attempt=%d delay=%s ===\n", attempt+2, options.restartDelay); err != nil {
			fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, err))
			return 2
		}
		if err := file.Sync(); err != nil {
			fmt.Fprintln(streams.stderr, T("cli.log.sync_failed", options.output, err))
			return 2
		}
		timer := time.NewTimer(options.restartDelay)
		select {
		case received := <-signals:
			timer.Stop()
			if _, err := fmt.Fprintf(file, "=== edc log stopped signal=%s exit=%d ===\n", unixSignalName(received.(syscall.Signal)), signalExitCode(received)); err != nil {
				fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, err))
				return 2
			}
			if err := file.Sync(); err != nil {
				fmt.Fprintln(streams.stderr, T("cli.log.sync_failed", options.output, err))
				return 2
			}
			return signalExitCode(received)
		case <-timer.C:
		}
	}
}

func ensureRecommendedLogDirectory(output string) error {
	return ensureRecommendedLogDirectoryFor(output, recommendedLogOutputPath())
}

func ensureRecommendedLogDirectoryFor(output, recommended string) error {
	if recommended == "" || output != recommended {
		return nil
	}
	return os.MkdirAll(filepath.Dir(output), 0o700)
}

func parseLogOptions(args []string, stderr io.Writer) (logOptions, bool) {
	config := activeConfig.Defaults.Log
	options := logOptions{
		stream:         configuredString(config.Stream, "both"),
		output:         configuredString(config.Output, ""),
		commandDisplay: configuredString(config.CommandDisplay, "full"),
		maxSizeMB:      configuredInt(config.MaxSizeMB, defaultLogMaxSizeMB),
		keepFiles:      configuredInt(config.KeepFiles, defaultLogKeepFiles),
		restart:        configuredString(config.Restart, "never"),
		maxRestarts:    configuredInt(config.MaxRestarts, defaultLogMaxRestarts),
		restartDelay:   configuredDuration(config.RestartDelay, defaultLogRestartDelay),
		timeout:        configuredDuration(config.Timeout, 0),
		killAfter:      configuredDuration(config.KillAfter, defaultLogKillAfter),
	}
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
	set.StringVar(&options.stream, "stream", options.stream, T("command.log.option.stream"))
	set.StringVar(&options.output, "output", options.output, T("command.log.option.output"))
	set.StringVar(&options.commandDisplay, "command-display", options.commandDisplay, T("command.log.option.command_display"))
	set.IntVar(&options.maxSizeMB, "max-size", options.maxSizeMB, T("command.log.option.max_size"))
	set.IntVar(&options.keepFiles, "keep-files", options.keepFiles, T("command.log.option.keep_files"))
	set.StringVar(&options.restart, "restart", options.restart, T("command.log.option.restart"))
	set.IntVar(&options.maxRestarts, "max-restarts", options.maxRestarts, T("command.log.option.max_restarts"))
	set.DurationVar(&options.restartDelay, "restart-delay", options.restartDelay, T("command.log.option.restart_delay"))
	set.DurationVar(&options.timeout, "timeout", options.timeout, T("command.log.option.timeout"))
	set.DurationVar(&options.killAfter, "kill-after", options.killAfter, T("command.log.option.kill_after"))
	if err := set.Parse(args[:separator]); err != nil {
		return logOptions{}, false
	}
	if set.NArg() != 0 {
		fmt.Fprintln(stderr, T("cli.log.command_required"))
		return logOptions{}, false
	}
	options.command = args[separator+1:]
	if options.stream != "stdout" && options.stream != "stderr" && options.stream != "both" {
		fmt.Fprintln(stderr, T("cli.log.stream_value"))
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
	if err := validateLogPolicy(options); err != nil {
		fmt.Fprintln(stderr, err)
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
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "=== edc log start time=%s pid=%d cwd=%s stream=%s%s ===\n", started.Format(time.RFC3339Nano), os.Getpid(), asciiJSON(cwd), options.stream, field)
	return err
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
			if readErr != io.EOF {
				result.readErr = readErr
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
