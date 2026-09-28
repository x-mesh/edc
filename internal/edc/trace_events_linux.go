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

// socketTargetLimit은 destroy event를 놓친 socket이 쌓여도 메모리를 제한한다. BPF의 socket 주인 map과 같은 크기다.
const socketTargetLimit = 65536

// socketTargetCache는 TCP socket이 한 번 얻은 target을 그 socket의 뒤 event에 이어 준다. 프로세스가 끝난 뒤
// 도착한 FIN이나 destroy event는 /proc에서 명령줄을 읽을 수 없다.
type socketTargetCache struct {
	entries map[uint64]string
}

func newSocketTargetCache() *socketTargetCache {
	return &socketTargetCache{entries: map[uint64]string{}}
}

func (cache *socketTargetCache) target(event captureEvent) string {
	if event.Protocol != "tcp" || event.SocketID == 0 {
		return event.Target
	}
	target := event.Target
	if target != "" {
		if _, ok := cache.entries[event.SocketID]; !ok && len(cache.entries) >= socketTargetLimit {
			clear(cache.entries)
		}
		cache.entries[event.SocketID] = target
	} else {
		target = cache.entries[event.SocketID]
	}
	// kernel이 해제한 socket 주소를 새 socket에 다시 쓰므로 destroy 뒤에는 남기지 않는다.
	if event.Event == "tcp_destroy" {
		delete(cache.entries, event.SocketID)
	}
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
