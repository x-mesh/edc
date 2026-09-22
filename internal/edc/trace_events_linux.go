//go:build linux

package edc

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func collectTraceEvents(duration time.Duration) ([]captureEvent, captureSummary, error) {
	return collectCaptureEvents(duration, nil)
}

func collectTraceEventsLive(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) ([]captureEvent, captureSummary, error) {
	return collectCaptureEventsUntil(duration, onEvent, stop)
}

func commandTarget(pid uint32) string {
	data, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/cmdline")
	if err != nil {
		return ""
	}
	arguments := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return traceTargetFromArguments(arguments)
}

func traceTargetFromArguments(arguments []string) string {
	for index := 1; index < len(arguments); index++ {
		argument := arguments[index]
		if strings.HasPrefix(argument, "-") || argument == "" {
			continue
		}
		if strings.Contains(argument, "://") {
			argument = strings.SplitN(argument, "://", 2)[1]
		}
		argument = strings.SplitN(argument, "/", 2)[0]
		if host, _, err := net.SplitHostPort(argument); err == nil {
			return host
		}
		if strings.Contains(argument, ".") && !strings.Contains(argument, "/") {
			return strings.TrimSuffix(argument, ".")
		}
	}
	return ""
}
