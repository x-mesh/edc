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

// /proc/<pid>/cmdline 읽기는 ring buffer의 다음 read를 막으므로, 같은 process의 연속 event는
// 짧은 TTL 동안 결과를 재사용한다. TTL이 exec나 PID 재사용 뒤에 남는 오래된 값의 수명을 제한한다.
const commandTargetTTL = time.Second

const commandTargetSweepSize = 1024

type commandTargetEntry struct {
	target  string
	expires time.Time
}

type commandTargetCache struct {
	lookup  func(uint32) string
	entries map[uint32]commandTargetEntry
}

func newCommandTargetCache(lookup func(uint32) string) *commandTargetCache {
	return &commandTargetCache{lookup: lookup, entries: map[uint32]commandTargetEntry{}}
}

func (cache *commandTargetCache) target(pid uint32, now time.Time) string {
	if entry, ok := cache.entries[pid]; ok && now.Before(entry.expires) {
		return entry.target
	}
	if len(cache.entries) >= commandTargetSweepSize {
		for key, entry := range cache.entries {
			if !now.Before(entry.expires) {
				delete(cache.entries, key)
			}
		}
	}
	target := cache.lookup(pid)
	cache.entries[pid] = commandTargetEntry{target: target, expires: now.Add(commandTargetTTL)}
	return target
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
