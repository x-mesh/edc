//go:build linux

package edc

import (
	"encoding/binary"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	return traceTargetFromArguments(commandLineArguments(data))
}

func commandLineArguments(data []byte) []string {
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
}

// ownerRecordType은 BPF가 socket 주인의 명령줄을 알리는 레코드다. 사용자에게 보일 event가 아니다.
// 앞 12바이트는 struct event와 같아서 event_type으로 구분한다.
const (
	ownerRecordType       = 8
	ownerRecordArgsOffset = 24
)

type ownerAnnouncement struct {
	pid      uint32
	target   string
	readable bool
}

func parseOwnerAnnouncement(sample []byte) (ownerAnnouncement, bool) {
	if len(sample) < ownerRecordArgsOffset || binary.LittleEndian.Uint32(sample[8:12]) != ownerRecordType {
		return ownerAnnouncement{}, false
	}
	owner := ownerAnnouncement{pid: binary.LittleEndian.Uint32(sample[12:16])}
	args := sample[ownerRecordArgsOffset:]
	if size := int(binary.LittleEndian.Uint32(sample[16:20])); size < len(args) {
		args = args[:size]
	}
	// 명령줄을 읽지 못한 레코드는 target을 모른다는 뜻이라, /proc 조회를 막지 않도록 구분한다.
	if len(args) == 0 {
		return owner, true
	}
	owner.readable = true
	owner.target = traceTargetFromArguments(commandLineArguments(args))
	return owner, true
}

// pidTargetLimit은 알림이 계속 들어와도 메모리를 제한한다. BPF의 socket 주인 map과 같은 크기다.
const pidTargetLimit = 65536

// pidTargetCache는 BPF가 알린 명령줄로 만든 target이다. /proc 조회와 달리 프로세스가 끝난 뒤에도 쓸 수 있다.
type pidTargetCache struct {
	entries map[uint32]string
}

func newPIDTargetCache() *pidTargetCache {
	return &pidTargetCache{entries: map[uint32]string{}}
}

func (cache *pidTargetCache) remember(pid uint32, target string) {
	if _, ok := cache.entries[pid]; !ok && len(cache.entries) >= pidTargetLimit {
		clear(cache.entries)
	}
	cache.entries[pid] = target
}

func (cache *pidTargetCache) target(pid uint32) (string, bool) {
	target, ok := cache.entries[pid]
	return target, ok
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

// traceTargetFromArguments는 명령줄에서 연결 대상으로 보이는 호스트를 고른다. URL이 가장 강한 근거라서
// 먼저 찾는다. 옵션 값(jq 필터, 헤더, 파일 이름)이 대상으로 잡히지 않도록 호스트 모양을 검사한다.
func traceTargetFromArguments(arguments []string) string {
	for index := 1; index < len(arguments); index++ {
		if _, rest, ok := strings.Cut(arguments[index], "://"); ok {
			if host := traceURLHost(rest); host != "" {
				return host
			}
		}
	}
	for index := 1; index < len(arguments); index++ {
		argument := arguments[index]
		if strings.HasPrefix(argument, "-") || argument == "" || strings.Contains(argument, "://") {
			continue
		}
		if host := traceBareHost(strings.SplitN(argument, "/", 2)[0]); host != "" {
			return host
		}
	}
	return ""
}

func traceURLHost(rest string) string {
	authority := rest
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	// 사용자 정보는 대상이 아니고 비밀번호가 들어 있을 수 있다.
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	host := authority
	if name, port, err := net.SplitHostPort(authority); err == nil {
		if !traceNumericPort(port) {
			return ""
		}
		host = name
	} else if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		host = authority[1 : len(authority)-1]
	}
	if net.ParseIP(host) != nil {
		return host
	}
	if traceHostname(host, false) {
		return strings.TrimSuffix(host, ".")
	}
	return ""
}

func traceBareHost(argument string) string {
	if net.ParseIP(argument) != nil {
		return argument
	}
	if host, port, err := net.SplitHostPort(argument); err == nil {
		if traceNumericPort(port) && (net.ParseIP(host) != nil || traceHostname(host, false)) {
			return strings.TrimSuffix(host, ".")
		}
		return ""
	}
	if user, host, ok := strings.Cut(argument, "@"); ok {
		if traceUserName(user) && (net.ParseIP(host) != nil || traceHostname(host, true)) {
			return user + "@" + strings.TrimSuffix(host, ".")
		}
		return ""
	}
	// URL도 포트도 없는 이름은 점이 있어야 대상으로 본다. "hosts", "GET" 같은 낱말을 거른다.
	if traceHostname(argument, true) {
		return strings.TrimSuffix(argument, ".")
	}
	return ""
}

// traceHostname은 호스트 이름 모양만 본다. 국제화 도메인을 그대로 받으려고 유니코드 글자도 허용한다.
func traceHostname(name string, needDot bool) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 || (needDot && !strings.Contains(name, ".")) {
		return false
	}
	numeric := true
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
				return false
			}
			if !unicode.IsDigit(r) {
				numeric = false
			}
		}
	}
	// 숫자로만 된 이름은 호출자가 IP로 먼저 본다. 여기까지 오면 1.2.3 같은 잘못된 주소다.
	return !numeric
}

func traceNumericPort(port string) bool {
	value, err := strconv.Atoi(port)
	return err == nil && value > 0 && value <= 65535 && !strings.HasPrefix(port, "+")
}

func traceUserName(user string) bool {
	if user == "" {
		return false
	}
	for _, r := range user {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
