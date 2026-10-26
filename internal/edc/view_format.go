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
		case viewFindLevel(line) != "":
			counts[viewFormatLevel]++
		}
	}
	if seen == 0 {
		return viewFormatPlain
	}
	best, bestCount := viewFormatPlain, 0
	for _, format := range []viewFormat{viewFormatJSON, viewFormatAccess, viewFormatLevel} {
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
	if !strings.HasPrefix(rest, "\"") {
		return false
	}
	end := strings.IndexByte(rest[1:], '"')
	if end < 0 {
		return false
	}
	request := rest[1 : end+1]
	rest = strings.TrimLeft(rest[end+2:], " ")
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
	quoted := viewQuoted(rest)
	for index, name := range []string{"referrer", "user agent"} {
		if index < len(quoted) {
			line.fields = append(line.fields, viewField{name, quoted[index]})
		}
	}
	return true
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
	if quote := strings.IndexByte(text[closing+1:], '"'); quote >= 0 {
		if end := strings.IndexByte(text[closing+quote+2:], '"'); end >= 0 {
			requestEnd = closing + quote + 2 + end + 1
		}
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
	return []traceText{
		{text: text[:open]},
		{text: text[open : closing+1], color: richGray},
		{text: text[closing+1 : statusAt]},
		{text: statusText, color: richHTTPCodeColor(line.status), bold: line.status >= 500},
		{text: text[statusEnd:], color: richGray},
	}
}

// viewLinePlain은 색 없이 그리는 한 줄이다. 칠한 줄과 글자가 같다.
func viewLinePlain(line viewLine, format viewFormat) string {
	var builder strings.Builder
	for _, span := range viewLineSpans(line, format) {
		builder.WriteString(span.text)
	}
	return builder.String()
}
