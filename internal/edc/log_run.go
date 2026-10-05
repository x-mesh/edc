package edc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type logAttemptResult struct {
	code                           int
	started, stopped, wrapperError bool
}

func runLogAttempt(options logOptions, streams logStreams, file *rotatingLogFile, signals <-chan os.Signal, attempt int) logAttemptResult {
	started := time.Now()
	result := logAttemptResult{code: 2}
	writeFailure := func(err error) logAttemptResult {
		fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, err))
		result.wrapperError = true
		return result
	}
	if err := writeLogStart(file, started, options); err != nil {
		return writeFailure(err)
	}
	if err := file.Sync(); err != nil {
		return writeFailure(err)
	}
	process := exec.Command(options.command[0], options.command[1:]...)
	process.Stdin = streams.stdin
	process.WaitDelay = options.killAfter
	configureLogProcess(process)
	readPipe, writePipe, startErr := os.Pipe()
	if startErr == nil {
		switch options.stream {
		case "stdout":
			process.Stdout, process.Stderr = writePipe, streams.stderr
		case "stderr":
			process.Stdout, process.Stderr = streams.stdout, writePipe
		case "both":
			process.Stdout, process.Stderr = writePipe, writePipe
		}
		startErr = process.Start()
		writePipe.Close()
	}
	if startErr != nil {
		if readPipe != nil {
			readPipe.Close()
		}
		fmt.Fprintln(streams.stderr, T("cli.log.start_failed", startErr))
		if _, err := fmt.Fprintf(file, "=== edc log start_error cause=%s ===\n", asciiJSON(startErr.Error())); err != nil {
			return writeFailure(err)
		}
		if err := writeLogEnd(file, started, "status=start_error exit=2", logCopyResult{}); err != nil {
			return writeFailure(err)
		}
		if err := file.Sync(); err != nil {
			return writeFailure(err)
		}
		return result
	}
	result.started = true
	if _, err := fmt.Fprintf(file, "=== edc log process attempt=%d pid=%d executable=%s ===\n", attempt, process.Process.Pid, asciiJSON(process.Path)); err != nil {
		_ = signalLogProcess(process, syscall.SIGKILL)
		readPipe.Close()
		_ = process.Wait()
		return writeFailure(err)
	}
	copyDone := make(chan logCopyResult, 1)
	go func() { copied := copyLogStream(file, readPipe); readPipe.Close(); copyDone <- copied }()
	waitDone := make(chan error, 1)
	go func() { waitDone <- process.Wait() }()
	var timeout <-chan time.Time
	if options.timeout > 0 {
		timer := time.NewTimer(options.timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	var kill <-chan time.Time
	var killTimer *time.Timer
	var signalErr error
	defer func() {
		if killTimer != nil {
			killTimer.Stop()
		}
	}()
	stop := func(received os.Signal) {
		if err := signalLogProcess(process, received); err != nil {
			signalErr = err
			fmt.Fprintln(streams.stderr, T("cli.log.signal_failed", err))
		}
		if killTimer == nil {
			killTimer = time.NewTimer(options.killAfter)
			kill = killTimer.C
		}
	}
	var waitErr, logErr error
	var copied logCopyResult
	var interrupted os.Signal
	timedOut := false
	forcedKill := false
	failures := file.failures
	for {
		if waitDone == nil && copyDone == nil {
			if kill == nil {
				break
			}
			// Group members can outlive the child without retaining its output pipe.
			err := syscall.Kill(-process.Process.Pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if err != nil {
				signalErr = err
				fmt.Fprintln(streams.stderr, T("cli.log.signal_failed", err))
				break
			}
		}
		select {
		case received := <-signals:
			if interrupted == nil {
				interrupted = received
			}
			result.stopped = true
			timeout = nil
			stop(received)
		case <-timeout:
			timedOut, timeout = true, nil
			stop(syscall.SIGTERM)
		case <-kill:
			forcedKill = true
			stop(syscall.SIGKILL)
			kill = nil
			// Descendants outside the group can retain the pipe after group termination.
			readPipe.Close()
		case logErr = <-failures:
			failures = nil
			stop(syscall.SIGTERM)
		case waitErr = <-waitDone:
			waitDone = nil
			if options.restart != "never" && (options.restart == "always" || waitErr != nil) && killTimer == nil {
				stop(syscall.SIGTERM)
			}
		case copied = <-copyDone:
			copyDone = nil
		}
	}
	status, code, childErr := logProcessStatus(process, waitErr)
	if timedOut {
		status, code = "status=timeout exit=124", 124
	}
	if interrupted != nil {
		status, code = fmt.Sprintf("status=signal signal=%s exit=%d", unixSignalName(interrupted.(syscall.Signal)), signalExitCode(interrupted)), signalExitCode(interrupted)
	}
	if signalErr != nil {
		status, code, result.wrapperError = "status=signal_error exit=2", 2, true
	}
	if copied.writeErr != nil {
		logErr = copied.writeErr
	} else if copied.readErr != nil && !(forcedKill && errors.Is(copied.readErr, os.ErrClosed)) {
		logErr = copied.readErr
	}
	if logErr != nil {
		status, code, result.wrapperError = "status=log_error exit=2", 2, true
		fmt.Fprintln(streams.stderr, T("cli.log.write_failed", options.output, logErr))
	} else if childErr != nil && !timedOut && interrupted == nil {
		status, code, result.wrapperError = "status=wait_error exit=2", 2, true
		fmt.Fprintln(streams.stderr, T("cli.log.wait_failed", childErr))
	}
	if forcedKill {
		status += " forced_kill=true"
	}
	result.code = code
	if err := writeLogEnd(file, started, status, copied); err != nil {
		result.code = 2
		return writeFailure(err)
	}
	if err := file.Sync(); err != nil {
		result.code = 2
		return writeFailure(err)
	}
	return result
}
