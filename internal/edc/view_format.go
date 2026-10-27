package edc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

type viewFormat string

const (
	viewFormatPlain  viewFormat = "plain"
	viewFormatAccess viewFormat = "access"
	viewFormatJSON   viewFormat = "json"
	viewFormatLevel  viewFormat = "level"
	viewFormatSyslog viewFormat = "syslog"
	viewFormatGoloop viewFormat = "goloop"
)

// viewParseLimit은 필드를 읽는 줄의 최대 길이다. 이보다 긴 줄은 minified JSON이나 dump라서, 읽어도 화면에 한 줄로
// 담기지 않고 화면을 그릴 때마다 시간만 든다. 원문은 그대로 보관하고 강조만 하지 않는다.
const viewParseLimit = 16 << 10

// viewDetectLines는 형식을 정할 때 보는 첫 줄 수다.
const viewDetectLines = 64

var viewLevels = []string{"fatal", "error", "warn", "info", "debug", "trace"}

// viewField는 상세 화면의 "이름: 값" 한 줄이다.
type viewField struct {
	name  string
	value string
}

// viewLine은 읽은 한 줄과 그 줄에서 읽어 낸 필드다. text는 escape와 제어 문자를 지운 표시용 글자다.
type viewLine struct {
	text string
	// marker는 "file replaced"처럼 edc가 끼워 넣은 구분 줄이다. 통계와 filter에서 뺀다.
	marker bool
	// cont는 stack trace처럼 앞 줄에 이어지는 줄이다. level은 앞 줄의 것을 물려받는다.
	cont   bool
	time   string
	level  string
	status int
	client string
	method string
	path   string
	// program은 syslog 줄의 프로그램 이름이다. 통계의 상위 프로그램에 쓴다.
	program string
	// rpc는 access 줄의 JSON-RPC 본문에 든 method다.
	rpc string
	// message는 JSON 줄을 "time level message key=value"로 줄여 보여 줄 때의 본문이다.
	message string
	fields  []viewField
}

// viewSanitize는 로그에 든 escape와 제어 문자를 지운다. 로그 내용이 화면을 움직이거나 터미널 설정을 바꾸지 못하게 하고,
// 글자 폭을 잴 수 있게 한다. tab은 다음 4칸 경계까지 공백으로 바꾼다.
func viewSanitize(raw string) string {
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "�")
	}
	if strings.IndexByte(raw, '\x1b') >= 0 {
		raw = ansi.Strip(raw)
	}
	clean := true
	for index := 0; index < len(raw); index++ {
		if raw[index] < 0x20 || raw[index] == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return raw
	}
	var builder strings.Builder
	column := 0
	for _, char := range raw {
		switch {
		case char == '\t':
			spaces := 4 - column%4
			builder.WriteString(strings.Repeat(" ", spaces))
			column += spaces
		case char == '\r':
		case char < 0x20 || char == 0x7f || (char >= 0x80 && char < 0xa0):
			builder.WriteRune('·')
			column++
		default:
			builder.WriteRune(char)
			column++
		}
	}
	return builder.String()
}

// detectViewFormat은 처음 줄들의 절반 넘게 맞는 형식을 고른다. 맞는 형식이 없으면 plain이다. 줄마다 모든 형식을 대 보지
// 않고, 정한 형식 하나로만 읽어서 큰 로그에서도 빠르다.
func detectViewFormat(lines []string) viewFormat {
	counts := map[viewFormat]int{}
	seen := 0
	for _, line := range lines {
		if seen == viewDetectLines {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" || len(line) > viewParseLimit {
			continue
		}
		seen++
		switch {
		case viewLooksJSON(line):
			counts[viewFormatJSON]++
		case viewParseAccess(line, &viewLine{}):
			counts[viewFormatAccess]++
		case viewParseGoloop(line, &viewLine{}):
			counts[viewFormatGoloop]++
		case viewParseSyslog(line, &viewLine{}):
			counts[viewFormatSyslog]++
		case viewFindLevel(line) != "":
			counts[viewFormatLevel]++
		}
	}
	if seen == 0 {
		return viewFormatPlain
	}
	best, bestCount := viewFormatPlain, 0
	for _, format := range []viewFormat{viewFormatJSON, viewFormatAccess, viewFormatGoloop, viewFormatSyslog, viewFormatLevel} {
		if counts[format] > bestCount {
			best, bestCount = format, counts[format]
		}
	}
	// level 줄 사이에 stack trace가 섞이면 절반을 넘지 못할 수 있어, level 형식은 넷 중 하나만 맞아도 고른다.
	if bestCount*2 > seen || (best == viewFormatLevel && bestCount*4 >= seen) {
		return best
	}
	return viewFormatPlain
}

func viewLooksJSON(line string) bool {
	return strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") && json.Valid([]byte(line))
}

// parseViewLine은 raw 한 줄을 format으로 읽는다. previous는 바로 앞 줄로, 이어지는 줄이 level을 물려받는 데 쓴다.
func parseViewLine(raw string, format viewFormat, previous *viewLine) viewLine {
	if len(raw) > viewStoreLimit {
		raw = raw[:viewStoreLimit]
	}
	line := viewLine{text: viewSanitize(raw)}
	if len(raw) > viewParseLimit || strings.TrimSpace(line.text) == "" {
		return line
	}
	switch format {
	case viewFormatAccess:
		viewParseAccess(line.text, &line)
	case viewFormatJSON:
		if !viewParseJSON(raw, &line) && previous != nil && viewContinues(line.text) {
			line.cont, line.level = true, previous.level
		}
	case viewFormatSyslog:
		if !viewParseSyslog(line.text, &line) && previous != nil && viewContinues(line.text) {
			line.cont, line.level = true, previous.level
		}
	case viewFormatGoloop:
		// goloop 줄은 모두 level로 시작하므로, 그렇지 않은 줄은 panic의 stack trace처럼 앞 줄에 이어지는 줄이다.
		if !viewParseGoloop(line.text, &line) && previous != nil {
			line.cont, line.level = true, previous.level
		}
	case viewFormatLevel:
		// level 형식의 로그에서 level이 없는 줄은 앞 event의 일부다. 예외 이름 줄도 들여 쓰지 않는다.
		if previous != nil && (viewContinues(line.text) || viewFindLevel(line.text) == "") {
			line.cont, line.level = true, previous.level
			return line
		}
		viewParseLevel(line.text, &line)
	default:
		line.level = viewFindLevel(line.text)
	}
	return line
}

// viewContinues는 stack trace처럼 앞 줄에 이어지는 줄인지 본다. 들여 쓴 줄, Java의 "Caused by:"와 "... 12 more",
// Python의 "Traceback"이다.
func viewContinues(text string) bool {
	if text == "" {
		return false
	}
	if text[0] == ' ' || text[0] == '\t' {
		return true
	}
	for _, prefix := range []string{"Caused by:", "Traceback (most recent call last)", "... "} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// viewParseAccess는 nginx와 Apache의 common, combined 형식이다.
// 203.0.113.9 - - [26/Oct/2026:10:00:00 +0900] "GET /api HTTP/1.1" 200 512 "-" "curl/8.0"
// 요청을 따옴표 없이 적고 user agent 뒤에 요청 본문과 처리 시간을 붙이는 nginx 변형도 읽는다. JSON-RPC 노드 앞의 nginx가 이렇게 적는다.
// 127.0.0.1 127.0.0.1  [28/Aug/2026:10:54:19 +0900] POST /api/v3 HTTP/1.1 400 106  "curl/7.81.0"  {"method":"icx_sendTransaction"} HTTP/1.1 0.005
func viewParseAccess(text string, line *viewLine) bool {
	client, rest, found := strings.Cut(text, " ")
	if !found || client == "" {
		return false
	}
	open := strings.IndexByte(rest, '[')
	if open < 0 {
		return false
	}
	closing := strings.IndexByte(rest[open:], ']')
	if closing < 0 {
		return false
	}
	timeText := rest[open+1 : open+closing]
	rest = strings.TrimLeft(rest[open+closing+1:], " ")
	var request string
	quotedRequest := strings.HasPrefix(rest, "\"")
	if quotedRequest {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return false
		}
		request = rest[1 : end+1]
		rest = strings.TrimLeft(rest[end+2:], " ")
	} else if request, rest = viewBareRequest(rest); request == "" {
		return false
	}
	statusText, rest, _ := strings.Cut(rest, " ")
	status, err := strconv.Atoi(statusText)
	if err != nil || status < 100 || status > 999 {
		return false
	}
	method, target, _ := strings.Cut(request, " ")
	path, protocol, _ := strings.Cut(target, " ")
	line.client, line.time, line.status, line.method, line.path = client, timeText, status, method, path
	bytesText, rest, _ := strings.Cut(rest, " ")
	line.fields = []viewField{{"client", client}, {"time", timeText}, {"method", method}, {"path", path}, {"protocol", protocol}, {"status", statusText}, {"bytes", bytesText}}
	// 요청을 따옴표 없이 적는 변형은 referrer 없이 user agent만 따옴표로 적는다.
	names := []string{"referrer", "user agent"}
	if !quotedRequest {
		names = []string{"user agent"}
	}
	quoted := viewQuoted(rest)
	for index, name := range names {
		if index < len(quoted) {
			line.fields = append(line.fields, viewField{name, quoted[index]})
		}
	}
	// 본문은 첫 '{'부터 마지막 '}'까지다. JSON-RPC의 method는 상위 path에서 요청을 가르도록 path 뒤에 붙인다.
	if body := viewAccessBody(rest); body != "" {
		line.fields = append(line.fields, viewField{"body", body})
		if line.rpc = viewJSONRPCMethod(body); line.rpc != "" {
			line.path = path + " " + line.rpc
			line.fields = append(line.fields, viewField{"rpc method", line.rpc})
		}
		rest = rest[strings.LastIndexByte(rest, '}')+1:]
	} else if len(quoted) > 0 {
		rest = rest[strings.LastIndexByte(rest, '"')+1:]
	}
	// 줄 끝의 소수는 nginx의 $request_time이다.
	if tokens := strings.Fields(rest); len(tokens) > 0 {
		if last := tokens[len(tokens)-1]; strings.ContainsRune(last, '.') && viewDigits(strings.ReplaceAll(last, ".", "")) {
			line.fields = append(line.fields, viewField{"request time", last})
		}
	}
	return true
}

// viewBareRequest는 따옴표 없는 "POST /api/v3 HTTP/1.1"이다. method는 대문자 낱말이고 세 번째 token은 HTTP/로 시작해야 한다.
// 요청과 그 뒤를 돌려주고, 아니면 빈 요청이다.
func viewBareRequest(rest string) (string, string) {
	method, after, ok := strings.Cut(rest, " ")
	if !ok || method == "" || method != strings.ToUpper(method) || !viewLettersOnly(method) {
		return "", rest
	}
	target, after, ok := strings.Cut(after, " ")
	if !ok || target == "" {
		return "", rest
	}
	protocol, after, _ := strings.Cut(after, " ")
	if !strings.HasPrefix(protocol, "HTTP/") {
		return "", rest
	}
	return method + " " + target + " " + protocol, strings.TrimLeft(after, " ")
}

func viewLettersOnly(text string) bool {
	for index := 0; index < len(text); index++ {
		if !viewWordByte(text[index]) {
			return false
		}
	}
	return text != ""
}

// viewAccessBody는 rest에 든 JSON 본문이다. 첫 '{'부터 마지막 '}'까지가 JSON이어야 하고, 아니면 빈 문자열이다.
func viewAccessBody(rest string) string {
	start, end := strings.IndexByte(rest, '{'), strings.LastIndexByte(rest, '}')
	if start < 0 || end <= start || !json.Valid([]byte(rest[start:end+1])) {
		return ""
	}
	return rest[start : end+1]
}

// viewJSONRPCMethod는 JSON-RPC 본문의 method다. 없으면 빈 문자열이다.
func viewJSONRPCMethod(body string) string {
	var request struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		return ""
	}
	return request.Method
}

// viewQuoted는 rest에 든 큰따옴표 값을 차례로 꺼낸다.
func viewQuoted(rest string) []string {
	var values []string
	for {
		start := strings.IndexByte(rest, '"')
		if start < 0 {
			return values
		}
		end := strings.IndexByte(rest[start+1:], '"')
		if end < 0 {
			return values
		}
		values = append(values, rest[start+1:start+1+end])
		rest = rest[start+end+2:]
	}
}

var (
	viewJSONTimeKeys    = []string{"time", "ts", "timestamp", "@timestamp", "date", "t"}
	viewJSONLevelKeys   = []string{"level", "lvl", "severity", "log.level", "loglevel", "levelname"}
	viewJSONMessageKeys = []string{"msg", "message", "event", "text"}
	viewJSONStatusKeys  = []string{"status", "status_code", "statusCode", "http.status_code"}
)

func viewParseJSON(raw string, line *viewLine) bool {
	var object map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &object); err != nil {
		return false
	}
	used := map[string]bool{}
	var leading []viewField
	take := func(keys []string) string {
		for _, key := range keys {
			if value, ok := object[key]; ok && !used[key] {
				used[key] = true
				text := viewJSONText(value)
				leading = append(leading, viewField{viewSanitize(key), text})
				return text
			}
		}
		return ""
	}
	line.time = viewSanitize(take(viewJSONTimeKeys))
	line.level = viewNormalizeLevel(take(viewJSONLevelKeys))
	message := viewSanitize(take(viewJSONMessageKeys))
	for _, key := range viewJSONStatusKeys {
		if status, err := strconv.Atoi(viewJSONText(object[key])); err == nil && status >= 100 && status <= 999 {
			line.status = status
			break
		}
	}
	if line.level == "" && line.status >= 500 {
		line.level = "error"
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		if !used[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	line.fields = leading
	extra := make([]string, 0, len(keys))
	for _, key := range keys {
		value := viewJSONText(object[key])
		name := viewSanitize(key)
		line.fields = append(line.fields, viewField{name, value})
		extra = append(extra, name+"="+viewSanitize(value))
	}
	line.message = strings.TrimSpace(message + "  " + strings.Join(extra, " "))
	return true
}

// viewJSONText는 JSON 값을 한 줄 글자로 바꾼다. 객체와 배열은 압축한 JSON이다.
func viewJSONText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return "null"
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

// viewParseLevel은 "2026-10-26 10:00:00 ERROR message" 같은 줄이다. level 앞은 시각으로 본다.
func viewParseLevel(text string, line *viewLine) {
	level, start, end := viewLocateLevel(text)
	if level == "" {
		return
	}
	line.level = level
	line.time = strings.TrimRight(strings.TrimSpace(text[:start]), "[|-:")
	line.message = strings.TrimLeft(text[end:], " ]|:-")
	line.fields = []viewField{{"time", line.time}, {"level", text[start:end]}, {"message", line.message}}
}

func viewFindLevel(text string) string {
	level, _, _ := viewLocateLevel(text)
	return level
}

// viewLevelWindow는 level을 찾는 앞부분의 길이다. 본문에 나온 "error" 같은 낱말을 level로 잘못 읽지 않게 줄 앞에서만 찾는다.
const viewLevelWindow = 80

// viewLocateLevel은 줄 앞부분에서 level 낱말과 그 위치를 찾는다. 대문자 낱말(ERROR, [WARN])이거나 level=error 형태여야 한다.
func viewLocateLevel(text string) (string, int, int) {
	window := text
	if len(window) > viewLevelWindow {
		window = window[:viewLevelWindow]
	}
	for index := 0; index < len(window); index++ {
		if index > 0 && viewWordByte(window[index-1]) {
			continue
		}
		end := index
		for end < len(window) && viewWordByte(window[end]) {
			end++
		}
		if end == index {
			continue
		}
		word := window[index:end]
		if level := viewNormalizeLevel(word); level != "" && (word == strings.ToUpper(word) || viewAfterLevelKey(window[:index])) {
			return level, index, end
		}
		index = end
	}
	return "", 0, 0
}

func viewWordByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func viewAfterLevelKey(before string) bool {
	before = strings.ToLower(strings.TrimRight(before, "\"' "))
	return strings.HasSuffix(before, "level=") || strings.HasSuffix(before, "level:") || strings.HasSuffix(before, "lvl=")
}

// viewNormalizeLevel은 여러 표기를 fatal, error, warn, info, debug, trace 중 하나로 모은다. level이 아니면 빈 문자열이다.
func viewNormalizeLevel(word string) string {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "fatal", "panic", "crit", "critical", "emerg", "emergency", "alert":
		return "fatal"
	case "error", "err":
		return "error"
	case "warn", "warning":
		return "warn"
	case "info", "notice":
		return "info"
	case "debug", "dbg":
		return "debug"
	case "trace", "verbose":
		return "trace"
	}
	return ""
}

// viewLevelColor는 level 글자의 색이다. fatal과 error는 빨강, warn은 노랑이고 나머지는 회색이다.
func viewLevelColor(level string) string {
	switch level {
	case "fatal", "error":
		return richRed
	case "warn":
		return richYellow
	case "info":
		return richGreen
	}
	return richGray
}

// viewLineSpans는 목록 한 줄의 색 조각이다. 의미가 있는 토큰(level, status, 시각)만 칠한다.
func viewLineSpans(line viewLine, format viewFormat) []traceText {
	if line.marker {
		return []traceText{{text: line.text, color: richYellow}}
	}
	if line.cont {
		return []traceText{{text: line.text}}
	}
	switch {
	case format == viewFormatAccess && line.status != 0:
		return viewAccessSpans(line)
	case format == viewFormatSyslog && line.program != "":
		return viewSyslogSpans(line)
	case format == viewFormatGoloop && line.program != "":
		return viewGoloopSpans(line)
	case format == viewFormatJSON && (line.time != "" || line.level != "" || line.message != ""):
		spans := []traceText{}
		if line.time != "" {
			spans = append(spans, traceText{text: viewSanitize(line.time) + " ", color: richGray})
		}
		if line.level != "" {
			spans = append(spans, traceText{text: strings.ToUpper(line.level) + " ", color: viewLevelColor(line.level), bold: line.level == "error" || line.level == "fatal"})
		}
		if line.message == "" {
			line.message = line.text
		}
		return append(spans, traceText{text: line.message})
	case line.level != "":
		_, start, end := viewLocateLevel(line.text)
		if end == 0 {
			return []traceText{{text: line.text}}
		}
		return []traceText{
			{text: line.text[:start], color: richGray},
			{text: line.text[start:end], color: viewLevelColor(line.level), bold: line.level == "error" || line.level == "fatal"},
			{text: line.text[end:]},
		}
	}
	return []traceText{{text: line.text}}
}

// viewAccessSpans는 access log 한 줄이다. 시각과 referrer, user agent는 회색, status는 범주 색이다.
func viewAccessSpans(line viewLine) []traceText {
	text := line.text
	open := strings.IndexByte(text, '[')
	closing := strings.IndexByte(text, ']')
	if open < 0 || closing < open {
		return []traceText{{text: text}}
	}
	statusText := strconv.Itoa(line.status)
	requestEnd := -1
	after := closing + 1
	for after < len(text) && text[after] == ' ' {
		after++
	}
	if after < len(text) && text[after] == '"' {
		if end := strings.IndexByte(text[after+1:], '"'); end >= 0 {
			requestEnd = after + 1 + end + 1
		}
	} else if request, _ := viewBareRequest(text[after:]); request != "" {
		requestEnd = after + len(request)
	}
	statusAt := -1
	if requestEnd >= 0 {
		if found := strings.Index(text[requestEnd:], " "+statusText); found >= 0 {
			statusAt = requestEnd + found + 1
		}
	}
	if statusAt < 0 {
		return []traceText{{text: text}}
	}
	statusEnd := statusAt + len(statusText)
	spans := []traceText{
		{text: text[:open]},
		{text: text[open : closing+1], color: richGray},
		{text: text[closing+1 : statusAt]},
		{text: statusText, color: richHTTPCodeColor(line.status), bold: line.status >= 500},
	}
	// JSON-RPC 본문의 method 이름은 청록이다. 그 줄에서 무엇을 요청했는지는 path가 아니라 이것이 말한다.
	tail := text[statusEnd:]
	if line.rpc != "" {
		if key := strings.Index(tail, "\"method\""); key >= 0 {
			if at := strings.Index(tail[key:], "\""+line.rpc+"\""); at >= 0 {
				start := statusEnd + key + at + 1
				return append(spans,
					traceText{text: text[statusEnd:start], color: richGray},
					traceText{text: line.rpc, color: richCyan},
					traceText{text: text[start+len(line.rpc):], color: richGray},
				)
			}
		}
	}
	return append(spans, traceText{text: tail, color: richGray})
}

// viewLinePlain은 색 없이 그리는 한 줄이다. 칠한 줄과 글자가 같다.
func viewLinePlain(line viewLine, format viewFormat) string {
	var builder strings.Builder
	for _, span := range viewLineSpans(line, format) {
		builder.WriteString(span.text)
	}
	return builder.String()
}

// viewParseSyslog는 rsyslog와 journald가 쓰는 "시각 host 프로그램[pid]: 메시지" 줄이다. auth.log, syslog, journalctl 출력이 이
// 모양이다. 프로그램 token은 ':'로 끝나야 해서 "2026-10-26 10:00:00 ERROR message" 같은 level 줄과 겹치지 않는다.
func viewParseSyslog(text string, line *viewLine) bool {
	layout, ok := viewSyslogParts(text)
	if !ok {
		return false
	}
	message := text[layout.messageStart:]
	line.time, line.program, line.message = text[:layout.timeEnd], text[layout.programStart:layout.programEnd], message
	line.level, _, _ = viewSyslogLevel(message)
	line.client = viewSyslogClient(message)
	line.fields = []viewField{{"time", line.time}, {"host", text[layout.hostStart:layout.hostEnd]}, {"program", line.program}}
	if layout.pidStart < layout.pidEnd {
		line.fields = append(line.fields, viewField{"pid", text[layout.pidStart:layout.pidEnd]})
	}
	line.fields = append(line.fields, viewField{"message", message})
	return true
}

// viewSyslogLayout은 syslog 줄 앞부분의 위치다. 색을 칠할 때도 쓰므로 글자를 새로 만들지 않고 위치만 둔다.
type viewSyslogLayout struct {
	timeEnd, hostStart, hostEnd, programStart, programEnd, pidStart, pidEnd, tagEnd, messageStart int
}

func viewSyslogParts(text string) (viewSyslogLayout, bool) {
	var layout viewSyslogLayout
	timeEnd := viewSyslogTimeEnd(text)
	if timeEnd <= 0 || timeEnd >= len(text) || text[timeEnd] != ' ' {
		return layout, false
	}
	hostStart := timeEnd + 1
	hostEnd := strings.IndexByte(text[hostStart:], ' ')
	if hostEnd <= 0 {
		return layout, false
	}
	hostEnd += hostStart
	tagStart := hostEnd + 1
	colon := strings.IndexByte(text[tagStart:], ':')
	if colon <= 0 {
		return layout, false
	}
	tagEnd := tagStart + colon
	// ':'이 메시지 안에 있으면 tag가 아니다. tag 뒤에는 공백이 오거나 줄이 끝난다.
	tag := text[tagStart:tagEnd]
	if strings.ContainsAny(tag, " \t") || (tagEnd+1 < len(text) && text[tagEnd+1] != ' ') {
		return layout, false
	}
	programEnd, pidStart, pidEnd := tagEnd, tagEnd, tagEnd
	if open := strings.IndexByte(tag, '['); open > 0 && strings.HasSuffix(tag, "]") {
		programEnd, pidStart, pidEnd = tagStart+open, tagStart+open+1, tagEnd-1
		if !viewDigits(text[pidStart:pidEnd]) {
			return layout, false
		}
	}
	if !viewProgramName(text[tagStart:programEnd]) {
		return layout, false
	}
	messageStart := tagEnd + 1
	for messageStart < len(text) && text[messageStart] == ' ' {
		messageStart++
	}
	return viewSyslogLayout{timeEnd: timeEnd, hostStart: hostStart, hostEnd: hostEnd, programStart: tagStart, programEnd: programEnd, pidStart: pidStart, pidEnd: pidEnd, tagEnd: tagEnd, messageStart: messageStart}, true
}

// viewSyslogTimeEnd는 시각이 끝나는 위치다. ISO 8601(2026-10-27T06:12:36.022173+00:00)이거나 BSD(Oct  7 06:12:36)다. 둘 다
// 아니면 0이다. BSD는 한 자리 날짜 앞에 공백이 하나 더 있다.
func viewSyslogTimeEnd(text string) int {
	if len(text) >= 19 && viewDigits(text[0:4]) && text[4] == '-' && viewDigits(text[5:7]) && text[7] == '-' && viewDigits(text[8:10]) && text[10] == 'T' && viewDigits(text[11:13]) && text[13] == ':' && viewDigits(text[14:16]) && text[16] == ':' && viewDigits(text[17:19]) {
		return max(0, strings.IndexByte(text, ' '))
	}
	if len(text) < 15 || text[3] != ' ' {
		return 0
	}
	switch text[:3] {
	case "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec":
	default:
		return 0
	}
	index := 4
	for index < len(text) && text[index] == ' ' {
		index++
	}
	dayStart := index
	for index < len(text) && text[index] >= '0' && text[index] <= '9' {
		index++
	}
	if day := index - dayStart; day == 0 || day > 2 || index >= len(text) || text[index] != ' ' {
		return 0
	}
	index++
	if len(text) < index+8 || !viewDigits(text[index:index+2]) || text[index+2] != ':' || !viewDigits(text[index+3:index+5]) || text[index+5] != ':' || !viewDigits(text[index+6:index+8]) {
		return 0
	}
	return index + 8
}

func viewDigits(text string) bool {
	if text == "" {
		return false
	}
	for index := 0; index < len(text); index++ {
		if text[index] < '0' || text[index] > '9' {
			return false
		}
	}
	return true
}

// viewProgramName은 syslog tag에 올 수 있는 이름인지 본다. sshd-session, systemd-logind, CRON, run-parts(/etc/cron.hourly) 같은 이름이다.
func viewProgramName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		char := name[index]
		if !(viewWordByte(char) || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' || char == '/' || char == '(' || char == ')') {
			return false
		}
	}
	return true
}

// viewSyslogWords는 syslog 메시지에서 level을 짐작하는 낱말이다. syslog는 level을 적지 않으므로 sshd와 pam이 쓰는 낱말로 본다.
// error와 warn은 level이 되고, ok는 level 없이 초록으로만 칠한다.
var viewSyslogWords = map[string]string{
	"error": "error", "errors": "error", "fail": "error", "failed": "error", "failure": "error", "failures": "error",
	"invalid": "error", "refused": "error", "denied": "error", "fatal": "error", "panic": "error", "segfault": "error",
	"warning": "warn", "warn": "warn", "timeout": "warn", "timed": "warn", "deprecated": "warn",
	"accepted": "ok", "success": "ok", "successful": "ok", "successfully": "ok", "succeeded": "ok",
}

// viewSyslogLevel은 메시지의 level과 그것을 정한 낱말의 위치다. 대문자 level이나 level= 값이 있으면 그것을 쓰고, 없으면
// viewSyslogWords로 짐작한다. 오류 낱말이 경고 낱말보다, 경고 낱말이 성공 낱말보다 먼저다. 낱말이 없으면 level이 없고 위치는 0이다.
func viewSyslogLevel(message string) (string, int, int) {
	if level, start, end := viewLocateLevel(message); level != "" {
		return level, start, end
	}
	found := map[string][2]int{}
	for index := 0; index < len(message); {
		if !viewWordByte(message[index]) {
			index++
			continue
		}
		end := index
		for end < len(message) && viewWordByte(message[end]) {
			end++
		}
		if kind := viewSyslogWords[strings.ToLower(message[index:end])]; kind != "" {
			if _, seen := found[kind]; !seen {
				found[kind] = [2]int{index, end}
			}
		}
		index = end
	}
	for _, kind := range []string{"error", "warn", "ok"} {
		if at, ok := found[kind]; ok {
			if kind == "ok" {
				return "", at[0], at[1]
			}
			return kind, at[0], at[1]
		}
	}
	return "", 0, 0
}

// viewSyslogClient는 "from 192.0.2.10 port 56889"의 주소다. sshd가 접속 주소를 이렇게 적는다. 주소가 아니면 빈 문자열이다.
func viewSyslogClient(message string) string {
	rest := message
	for {
		at := strings.Index(rest, " from ")
		if at < 0 {
			return ""
		}
		rest = rest[at+len(" from "):]
		token, _, _ := strings.Cut(rest, " ")
		if viewLooksAddress(token) {
			return token
		}
	}
}

func viewLooksAddress(token string) bool {
	if !strings.ContainsAny(token, ".:") {
		return false
	}
	for index := 0; index < len(token); index++ {
		char := token[index]
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F' || char == '.' || char == ':') {
			return false
		}
	}
	return true
}

// viewSyslogSpans는 syslog 한 줄이다. 시각과 host, pid는 회색, 프로그램은 청록이고, 메시지에서는 level을 정한 낱말만 칠한다.
func viewSyslogSpans(line viewLine) []traceText {
	text := line.text
	layout, ok := viewSyslogParts(text)
	if !ok {
		return []traceText{{text: text}}
	}
	spans := []traceText{
		{text: text[:layout.hostEnd], color: richGray},
		{text: text[layout.hostEnd:layout.programStart]},
		{text: text[layout.programStart:layout.programEnd], color: richCyan},
	}
	if layout.programEnd < layout.tagEnd {
		spans = append(spans, traceText{text: text[layout.programEnd:layout.tagEnd], color: richGray})
	}
	level, start, end := viewSyslogLevel(text[layout.messageStart:])
	if end == 0 {
		return append(spans, traceText{text: text[layout.tagEnd:]})
	}
	color := richGreen
	if level != "" {
		color = viewLevelColor(level)
	}
	start, end = layout.messageStart+start, layout.messageStart+end
	return append(spans,
		traceText{text: text[layout.tagEnd:start]},
		traceText{text: text[start:end], color: color, bold: level == "error" || level == "fatal"},
		traceText{text: text[end:]},
	)
}

// viewParseGoloop은 goloop 노드의 "D|20260713-02:58:06.638211|c248|1|CS|syncer.go:210 message" 줄이다. goloop의
// common/log/formatter.go가 level 글자, 시각, wallet, cid, module을 '|'로 잇고, 있을 때만 eid를 더한 뒤 위치와 메시지를 쓴다.
func viewParseGoloop(text string, line *viewLine) bool {
	layout, ok := viewGoloopParts(text)
	if !ok {
		return false
	}
	line.level = viewGoloopLevel(text[0])
	line.time = text[layout.timeStart:layout.timeEnd]
	line.program = text[layout.moduleStart:layout.moduleEnd]
	line.message = text[layout.messageStart:]
	line.fields = []viewField{{"level", text[:1]}, {"time", line.time}, {"wallet", text[layout.walletStart:layout.walletEnd]}, {"cid", text[layout.cidStart:layout.cidEnd]}, {"module", line.program}}
	if layout.eidStart < layout.eidEnd {
		line.fields = append(line.fields, viewField{"eid", text[layout.eidStart:layout.eidEnd]})
	}
	if layout.locationStart < layout.locationEnd {
		line.fields = append(line.fields, viewField{"location", text[layout.locationStart:layout.locationEnd]})
	}
	line.fields = append(line.fields, viewField{"message", line.message})
	return true
}

// viewGoloopLayout은 goloop 줄 앞부분의 위치다. 색을 칠할 때도 쓴다.
type viewGoloopLayout struct {
	timeStart, timeEnd, walletStart, walletEnd, cidStart, cidEnd, moduleStart, moduleEnd, eidStart, eidEnd, locationStart, locationEnd, messageStart int
}

// viewGoloopLevelNames는 goloop의 level 글자다. P는 panic, F는 fatal이다.
const viewGoloopLevelNames = "PFEWIDT"

func viewGoloopLevel(letter byte) string {
	switch letter {
	case 'P', 'F':
		return "fatal"
	case 'E':
		return "error"
	case 'W':
		return "warn"
	case 'I':
		return "info"
	case 'D':
		return "debug"
	}
	return "trace"
}

func viewGoloopParts(text string) (viewGoloopLayout, bool) {
	var layout viewGoloopLayout
	if len(text) < 2 || text[1] != '|' || !strings.ContainsRune(viewGoloopLevelNames, rune(text[0])) {
		return layout, false
	}
	// 시각은 20060102-15:04:05.000000이다. 소수점 아래는 있으면 읽는다.
	timeStart := 2
	timeEnd := timeStart + strings.IndexByte(text[timeStart:], '|')
	if timeEnd < timeStart+17 || !viewDigits(text[timeStart:timeStart+8]) || text[timeStart+8] != '-' || !viewDigits(text[timeStart+9:timeStart+11]) || text[timeStart+11] != ':' || !viewDigits(text[timeStart+12:timeStart+14]) || text[timeStart+14] != ':' || !viewDigits(text[timeStart+15:timeStart+17]) {
		return layout, false
	}
	if rest := text[timeStart+17 : timeEnd]; rest != "" && (rest[0] != '.' || !viewDigits(rest[1:])) {
		return layout, false
	}
	field := func(start int) (int, int, bool) {
		end := strings.IndexByte(text[start:], '|')
		if end < 0 || strings.ContainsAny(text[start:start+end], " \t") {
			return 0, 0, false
		}
		return start, start + end, true
	}
	walletStart, walletEnd, ok := field(timeEnd + 1)
	if !ok {
		return layout, false
	}
	cidStart, cidEnd, ok := field(walletEnd + 1)
	if !ok {
		return layout, false
	}
	moduleStart, moduleEnd, ok := field(cidEnd + 1)
	if !ok || moduleStart == moduleEnd {
		return layout, false
	}
	rest := moduleEnd + 1
	eidStart, eidEnd := rest, rest
	// eid는 8글자 뒤에 '|'가 온다. 위치와 메시지에는 공백이 먼저 오므로 '|'가 공백보다 앞에 있을 때만 eid다.
	if bar := strings.IndexByte(text[rest:], '|'); bar == 8 && !strings.ContainsAny(text[rest:rest+8], " ") {
		eidStart, eidEnd = rest, rest+8
		rest = eidEnd + 1
	}
	locationStart, locationEnd := rest, rest
	token, _, _ := strings.Cut(text[rest:], " ")
	if colon := strings.LastIndex(token, ".go:"); colon > 0 && viewDigits(token[colon+4:]) {
		locationStart, locationEnd = rest, rest+len(token)
		rest = locationEnd
	}
	messageStart := rest
	for messageStart < len(text) && text[messageStart] == ' ' {
		messageStart++
	}
	return viewGoloopLayout{timeStart: timeStart, timeEnd: timeEnd, walletStart: walletStart, walletEnd: walletEnd, cidStart: cidStart, cidEnd: cidEnd, moduleStart: moduleStart, moduleEnd: moduleEnd, eidStart: eidStart, eidEnd: eidEnd, locationStart: locationStart, locationEnd: locationEnd, messageStart: messageStart}, true
}

// viewGoloopSpans는 goloop 한 줄이다. level 글자는 level 색, module은 청록이고, 시각과 wallet, cid, eid, 위치는 회색이다. 메시지는 칠하지 않는다.
func viewGoloopSpans(line viewLine) []traceText {
	text := line.text
	layout, ok := viewGoloopParts(text)
	if !ok {
		return []traceText{{text: text}}
	}
	level := viewGoloopLevel(text[0])
	return []traceText{
		{text: text[:1], color: viewLevelColor(level), bold: level == "error" || level == "fatal"},
		{text: text[1:layout.moduleStart], color: richGray},
		{text: text[layout.moduleStart:layout.moduleEnd], color: richCyan},
		{text: text[layout.moduleEnd:layout.messageStart], color: richGray},
		{text: text[layout.messageStart:]},
	}
}
