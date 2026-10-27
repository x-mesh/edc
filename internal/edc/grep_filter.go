package edc

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	// grepTolerance는 로그가 시간 순서에서 벗어날 수 있는 폭이다. nginx의 $time_local은 요청 시작 시각이라 완료 순서로
	// 쓰이는 줄과 몇 초씩 어긋난다. --since 이진 탐색은 이만큼 앞에서 시작하고, --until은 이만큼 지난 줄을 보고 멈춘다.
	grepTolerance = time.Minute
	// grepSeekStop은 이진 탐색을 멈추는 구간 크기다. 이보다 좁아지면 그 자리부터 읽는다.
	grepSeekStop = 64 << 10
	// grepProbeBytes는 이진 탐색이 한 자리에서 시각을 찾으려고 읽는 양이다. 구간 앞의 마지막 event 줄을 찾을 때도 쓴다.
	grepProbeBytes = 64 << 10
	// grepOrderProbes는 파일이 시간 순서인지 볼 때 찍는 자리 수다. 처음과 끝만 보면 로그 둘을 이어 붙인 파일처럼 중간에서
	// 시각이 되돌아가는 파일을 놓친다.
	grepOrderProbes = 16
)

// grepTimeLayouts는 로그에서 보는 시각 표기다. 시간대가 없는 표기는 local time으로 읽는다. 초 뒤의 소수는 layout에
// 없어도 받는다.
var grepTimeLayouts = []string{
	"02/Jan/2006:15:04:05 -0700",
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006/01/02 15:04:05",
	"02/Jan/2006 15:04:05",
	"Jan _2 15:04:05",
	"2006-01-02",
}

// grepTimeParser는 줄의 시각 글자를 time.Time으로 바꾼다. 한 파일 안에서는 같은 표기가 이어지므로, 처음 맞은 layout과
// 시각이 차지한 낱말 수를 기억해 다음 줄부터는 그것만 시도한다.
type grepTimeParser struct {
	now    time.Time
	layout string
	words  int
	// last는 직전에 해석한 글자와 결과다. access log는 같은 초에 수십 줄이 쌓여 글자가 그대로 반복된다.
	last   []byte
	lastAt time.Time
}

func (parser *grepTimeParser) parse(text []byte) (time.Time, bool) {
	if len(text) != 0 && bytes.Equal(text, parser.last) {
		return parser.lastAt, true
	}
	at, ok := parser.parseFresh(text)
	if ok {
		parser.last, parser.lastAt = append(parser.last[:0], text...), at
	}
	return at, ok
}

func (parser *grepTimeParser) parseFresh(text []byte) (time.Time, bool) {
	trimmed := strings.Trim(string(text), "[]\"' |-:")
	if trimmed == "" {
		return time.Time{}, false
	}
	if parser.layout != "" {
		if at, ok := grepParseLayout(parser.layout, grepLeadingWords(trimmed, parser.words), parser.now); ok {
			return at, true
		}
	}
	if at, ok := grepParseEpoch(trimmed); ok {
		return at, true
	}
	// syslog처럼 시각 뒤에 host와 program이 붙으면 뒤 낱말을 하나씩 떼며 맞춰 본다.
	words := strings.Count(trimmed, " ") + 1
	for count := words; count >= 1 && count >= words-4; count-- {
		candidate := grepLeadingWords(trimmed, count)
		for _, layout := range grepTimeLayouts {
			if at, ok := grepParseLayout(layout, candidate, parser.now); ok {
				parser.layout, parser.words = layout, count
				return at, true
			}
		}
	}
	return time.Time{}, false
}

func grepLeadingWords(text string, count int) string {
	if count <= 0 {
		return text
	}
	index := 0
	for ; count > 0 && index < len(text); count-- {
		next := strings.IndexByte(text[index:], ' ')
		if next < 0 {
			return text
		}
		index += next + 1
	}
	return strings.TrimRight(text[:index], " ")
}

func grepParseLayout(layout, text string, now time.Time) (time.Time, bool) {
	at, err := time.ParseInLocation(layout, text, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	// 연도가 없는 syslog 표기는 올해로 보고, 그래서 미래가 되면 작년이다.
	if at.Year() == 0 {
		at = at.AddDate(now.Year(), 0, 0)
		if at.After(now.Add(24 * time.Hour)) {
			at = at.AddDate(-1, 0, 0)
		}
	}
	return at, true
}

// grepParseEpoch는 숫자만 든 시각이다. 자릿수로 초, 밀리초, 마이크로초, 나노초를 가른다.
func grepParseEpoch(text string) (time.Time, bool) {
	for _, char := range text {
		if char < '0' || char > '9' {
			return time.Time{}, false
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	switch len(text) {
	case 10:
		return time.Unix(value, 0), true
	case 13:
		return time.UnixMilli(value), true
	case 16:
		return time.UnixMicro(value), true
	case 19:
		return time.Unix(0, value), true
	}
	return time.Time{}, false
}

var errGrepBadTime = errors.New("bad time")

// grepParseWhen은 --since와 --until의 값이다. 10m, 2h, 1d, 1w 같은 기간은 지금부터 거꾸로 세고, 그 밖에는 시각이다.
// 시각만 쓰면 오늘이다.
func grepParseWhen(text string, now time.Time) (time.Time, error) {
	if age, ok := grepParseAge(text); ok {
		return now.Add(-age), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02", "15:04:05", "15:04"} {
		at, err := time.ParseInLocation(layout, text, time.Local)
		if err != nil {
			continue
		}
		if strings.HasPrefix(layout, "15") {
			at = time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), at.Second(), 0, time.Local)
		}
		return at, nil
	}
	return time.Time{}, errGrepBadTime
}

func grepParseAge(text string) (time.Duration, bool) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if number, ok := strings.CutSuffix(text, suffix); ok {
			count, err := strconv.ParseFloat(number, 64)
			return time.Duration(count * float64(unit)), err == nil && count > 0
		}
	}
	age, err := time.ParseDuration(text)
	return age, err == nil && age > 0
}

// grepStatusSet은 --status의 값이다. 5xx 같은 범주, 404 같은 하나, 500-503 같은 범위를 쉼표로 잇는다.
type grepStatusSet struct {
	classes [10]bool
	exact   map[int]bool
	ranges  [][2]int
}

var errGrepBadStatus = errors.New("bad status")

func parseGrepStatuses(text string) (*grepStatusSet, error) {
	set := &grepStatusSet{exact: map[int]bool{}}
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		switch {
		case len(part) == 3 && strings.HasSuffix(part, "xx") && part[0] >= '1' && part[0] <= '9':
			set.classes[part[0]-'0'] = true
		case strings.Contains(part, "-"):
			low, high, _ := strings.Cut(part, "-")
			from, err := strconv.Atoi(low)
			to, err2 := strconv.Atoi(high)
			if err != nil || err2 != nil || from > to || from < 100 || to > 999 {
				return nil, errGrepBadStatus
			}
			set.ranges = append(set.ranges, [2]int{from, to})
		default:
			status, err := strconv.Atoi(part)
			if err != nil || status < 100 || status > 999 {
				return nil, errGrepBadStatus
			}
			set.exact[status] = true
		}
	}
	return set, nil
}

func (set *grepStatusSet) match(status int) bool {
	if status < 100 || status > 999 {
		return false
	}
	if set.classes[status/100] || set.exact[status] {
		return true
	}
	for _, span := range set.ranges {
		if status >= span[0] && status <= span[1] {
			return true
		}
	}
	return false
}

// markers는 access 줄에서 맞을 수 있는 status가 있는지 파싱 전에 확인하는 바이트다. 요청 뒤 `" 502 `처럼 나온다.
// 범위는 앉은 범주 전체로 넓혀 놓치지 않게 한다.
func (set *grepStatusSet) markers() [][]byte {
	var markers [][]byte
	classes := set.classes
	for _, span := range set.ranges {
		for class := span[0] / 100; class <= span[1]/100; class++ {
			classes[class] = true
		}
	}
	for class, on := range classes {
		if on {
			markers = append(markers, []byte("\" "+strconv.Itoa(class)))
		}
	}
	for status := range set.exact {
		if !classes[status/100] {
			markers = append(markers, []byte("\" "+strconv.Itoa(status)+" "))
		}
	}
	return markers
}

// grepLevelRank는 level의 순서다. --level warn은 warn 이상(warn, error, fatal)이다. level이 없으면 -1이다.
func grepLevelRank(level string) int {
	for index, name := range viewLevels {
		if name == level {
			return len(viewLevels) - 1 - index
		}
	}
	return -1
}

// grepEvent는 가장 최근의 머리 줄이다. level 형식과 JSON 로그의 stack trace 줄은 이 줄의 level과 시각을 물려받는다.
// 시각은 필터가 실제로 볼 때만 해석한다.
type grepEvent struct {
	level    string
	timeText []byte
	at       time.Time
	parsed   bool
	valid    bool
}

// grepLineFields는 필터가 한 줄에서 보는 값이다.
type grepLineFields struct {
	level    string
	timeText []byte
	status   int
	// event는 시각을 상속한 줄에서 해석 결과를 나눠 쓰기 위한 참조다. nil이면 줄 자체의 timeText를 해석한다.
	event *grepEvent
}

// grepFilter는 패턴 밖의 조건이다. 형식마다 싼 방법으로 level, status, 시각만 꺼내고 parseViewLine은 쓰지 않는다.
// 32M줄을 parseViewLine으로 읽으면 57초였고, 필요한 값만 꺼내면 grep과 같은 4초였다.
type grepFilter struct {
	format   viewFormat
	matcher  *grepMatcher
	minLevel int
	statuses *grepStatusSet
	markers  [][]byte
	since    time.Time
	until    time.Time
	times    grepTimeParser
	event    grepEvent
}

func newGrepFilter(format viewFormat, matcher *grepMatcher, now time.Time) *grepFilter {
	return &grepFilter{format: format, matcher: matcher, minLevel: -1, times: grepTimeParser{now: now}}
}

func (filter *grepFilter) setStatuses(set *grepStatusSet) {
	filter.statuses = set
	if filter.format == viewFormatAccess {
		filter.markers = set.markers()
	}
}

// active는 패턴 밖의 조건이 하나라도 있는지다.
func (filter *grepFilter) active() bool {
	return filter.minLevel >= 0 || filter.statuses != nil || !filter.since.IsZero() || !filter.until.IsZero()
}

// needsEveryLine은 줄을 건너뛰지 않고 차례로 봐야 하는지다. level과 시각을 앞 줄에서 물려받는 형식에서 그 두 조건을 쓰면,
// 머리 줄을 지나쳐서는 이어지는 줄을 판단할 수 없다.
func (filter *grepFilter) needsEveryLine() bool {
	inherits := filter.format == viewFormatLevel || filter.format == viewFormatJSON
	return inherits && (filter.minLevel >= 0 || !filter.since.IsZero() || !filter.until.IsZero())
}

// track은 line의 값을 꺼내고 event 상태를 갱신한다. accept와 prime이 함께 쓴다.
func (filter *grepFilter) track(line []byte) grepLineFields {
	var fields grepLineFields
	switch filter.format {
	case viewFormatAccess:
		fields.timeText, fields.status, _ = grepAccessFields(line)
	case viewFormatJSON:
		level, timeText, status, object := grepJSONFields(line)
		if object {
			fields = grepLineFields{level: level, timeText: timeText, status: status}
			filter.remember(level, timeText)
		} else if viewContinues(string(grepHead(line))) {
			fields = filter.inherited()
		}
	case viewFormatLevel:
		level, timeText, header := grepLevelFields(line)
		switch {
		case header:
			fields = grepLineFields{level: level, timeText: timeText}
			filter.remember(level, timeText)
		case len(bytes.TrimSpace(line)) != 0:
			fields = filter.inherited()
		}
	default:
		fields.level = viewFindLevel(string(grepHead(line)))
	}
	return fields
}

// remember는 머리 줄을 event로 둔다. timeText는 scanner가 재사용하는 버퍼를 가리키므로 복사한다.
func (filter *grepFilter) remember(level string, timeText []byte) {
	filter.event = grepEvent{level: level, timeText: bytes.Clone(timeText)}
}

func (filter *grepFilter) inherited() grepLineFields {
	return grepLineFields{level: filter.event.level, timeText: filter.event.timeText, event: &filter.event}
}

// accept는 scanner가 줄마다 부른다. 상태 갱신이 먼저라서 패턴에 맞지 않는 머리 줄도 뒤 줄에 level을 물려준다.
func (filter *grepFilter) accept(line []byte) bool {
	fields := filter.track(line)
	if filter.matcher != nil && !filter.matcher.match(line) {
		return false
	}
	if filter.minLevel >= 0 && grepLevelRank(fields.level) < filter.minLevel {
		return false
	}
	if filter.statuses != nil && !filter.statuses.match(fields.status) {
		return false
	}
	// --until은 줄을 거르기만 하고 읽기를 멈추지 않는다. 시간 순서가 아닌 파일에서 뒤에 오는 범위 안 줄을 놓치지 않기 위해서다.
	if !filter.since.IsZero() || !filter.until.IsZero() {
		at, ok := filter.timeOf(fields)
		if !ok || (!filter.since.IsZero() && at.Before(filter.since)) || (!filter.until.IsZero() && at.After(filter.until)) {
			return false
		}
	}
	return true
}

func (filter *grepFilter) timeOf(fields grepLineFields) (time.Time, bool) {
	if fields.event != nil {
		if !fields.event.parsed {
			fields.event.at, fields.event.valid = filter.times.parse(fields.event.timeText)
			fields.event.parsed = true
		}
		return fields.event.at, fields.event.valid
	}
	if len(fields.timeText) == 0 {
		return time.Time{}, false
	}
	return filter.times.parse(fields.timeText)
}

// prefilter는 파싱 전에 줄을 걸러 낸다. access 줄에서 --status의 marker가 없으면 맞을 수 없다.
func (filter *grepFilter) prefilter(line []byte) bool {
	if len(filter.markers) == 0 {
		return true
	}
	for _, marker := range filter.markers {
		if bytes.Contains(line, marker) {
			return true
		}
	}
	return false
}

// prime은 from 앞의 마지막 머리 줄을 찾아 event 상태를 맞춘다. 구간이 stack trace 중간에서 시작해도 그 줄들이 level과
// 시각을 물려받는다. 앞 grepProbeBytes 안에 머리 줄이 없으면 상태가 비어 그 줄들은 조건에 맞지 않는다.
func (filter *grepFilter) prime(file io.ReaderAt, from int64) error {
	filter.event = grepEvent{}
	if from <= 0 || !filter.needsEveryLine() {
		return nil
	}
	start := from - grepProbeBytes
	if start < 0 {
		start = 0
	}
	// from이 줄 중간이면 그 줄을 끝까지 읽는다. level 낱말 앞에서 잘린 머리 줄을 이어지는 줄로 잘못 보면 안 된다.
	buffer := make([]byte, from-start+grepProbeBytes)
	count, err := file.ReadAt(buffer, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	buffer = buffer[:count]
	if tail := int(from - start); tail < len(buffer) {
		if newline := bytes.IndexByte(buffer[tail:], '\n'); newline >= 0 {
			buffer = buffer[:tail+newline]
		}
	}
	if start > 0 {
		if newline := bytes.IndexByte(buffer, '\n'); newline >= 0 {
			buffer = buffer[newline+1:]
		} else {
			return nil
		}
	}
	for _, line := range bytes.Split(buffer, []byte{'\n'}) {
		filter.track(line)
	}
	return nil
}

// grepHead는 level과 이어지는 줄을 판단하는 데 보는 줄 앞부분이다.
func grepHead(line []byte) []byte {
	if len(line) > viewLevelWindow {
		return line[:viewLevelWindow]
	}
	return line
}

// grepAccessFields는 common, combined access 줄의 시각과 status다. viewParseAccess와 같은 자리를 보되 필드를 만들지 않는다.
func grepAccessFields(line []byte) ([]byte, int, bool) {
	space := bytes.IndexByte(line, ' ')
	if space <= 0 {
		return nil, 0, false
	}
	open := bytes.IndexByte(line[space:], '[')
	if open < 0 {
		return nil, 0, false
	}
	open += space
	closing := bytes.IndexByte(line[open:], ']')
	if closing < 0 {
		return nil, 0, false
	}
	timeText := line[open+1 : open+closing]
	rest := bytes.TrimLeft(line[open+closing+1:], " ")
	if len(rest) == 0 || rest[0] != '"' {
		return nil, 0, false
	}
	end := bytes.IndexByte(rest[1:], '"')
	if end < 0 {
		return nil, 0, false
	}
	rest = bytes.TrimLeft(rest[end+2:], " ")
	statusText := rest
	if next := bytes.IndexByte(rest, ' '); next >= 0 {
		statusText = rest[:next]
	}
	status, err := strconv.Atoi(string(statusText))
	if err != nil || status < 100 || status > 999 {
		return nil, 0, false
	}
	return timeText, status, true
}

// grepJSONFields는 JSON 줄의 level, 시각, status다. 전체를 풀지 않고 키를 찾아 값만 꺼낸다. 줄이 JSON object가 아니면
// object가 false다.
func grepJSONFields(line []byte) (level string, timeText []byte, status int, object bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return "", nil, 0, false
	}
	timeText, _ = grepJSONValue(trimmed, viewJSONTimeKeys)
	if value, ok := grepJSONValue(trimmed, viewJSONLevelKeys); ok {
		level = viewNormalizeLevel(string(value))
	}
	if value, ok := grepJSONValue(trimmed, viewJSONStatusKeys); ok {
		if parsed, err := strconv.Atoi(string(value)); err == nil && parsed >= 100 && parsed <= 999 {
			status = parsed
		}
	}
	// viewParseJSON처럼 level이 없는 5xx 줄은 오류다.
	if level == "" && status >= 500 {
		level = "error"
	}
	return level, timeText, status, true
}

// grepJSONValue는 keys 중 처음 있는 키의 값이다. 문자열은 따옴표 안을, 숫자는 쉼표나 닫는 괄호 앞까지를 돌려준다.
func grepJSONValue(object []byte, keys []string) ([]byte, bool) {
	for _, key := range keys {
		quoted := []byte("\"" + key + "\"")
		position := 0
		for {
			index := bytes.Index(object[position:], quoted)
			if index < 0 {
				break
			}
			after := position + index + len(quoted)
			for after < len(object) && object[after] == ' ' {
				after++
			}
			if after >= len(object) || object[after] != ':' {
				position = after
				continue
			}
			after++
			for after < len(object) && object[after] == ' ' {
				after++
			}
			if after >= len(object) {
				return nil, false
			}
			if object[after] == '"' {
				end := after + 1
				for end < len(object) && object[end] != '"' {
					if object[end] == '\\' {
						end++
					}
					end++
				}
				if end >= len(object) {
					return nil, false
				}
				return object[after+1 : end], true
			}
			end := after
			for end < len(object) && object[end] != ',' && object[end] != '}' {
				end++
			}
			return bytes.TrimSpace(object[after:end]), true
		}
	}
	return nil, false
}

// grepLevelFields는 "2026-10-26 10:00:02,310 ERROR ..." 같은 줄의 level과 그 앞의 시각이다. level이 없거나 들여 쓴 줄은
// 앞 event에 이어지는 줄이라 header가 false다.
func grepLevelFields(line []byte) (string, []byte, bool) {
	head := grepHead(line)
	text := string(head)
	if viewContinues(text) {
		return "", nil, false
	}
	level, start, _ := viewLocateLevel(text)
	if level == "" {
		return "", nil, false
	}
	return level, bytes.TrimRight(head[:start], " [|-:"), true
}

// bounds는 파일이 시간 순서인지 grepOrderProbes 자리를 찍어 확인하고 처음과 끝의 시각을 돌려준다. 어느 자리에서 시각을
// 읽지 못하거나 앞 자리보다 tolerance 넘게 이르면 순서가 아니라고 보고 ok=false다.
func (filter *grepFilter) bounds(file io.ReaderAt, size int64) (time.Time, time.Time, int, bool) {
	probe := *filter
	probe.event = grepEvent{}
	var first, previous time.Time
	for index := 0; index < grepOrderProbes; index++ {
		var at time.Time
		var ok bool
		if index == grepOrderProbes-1 {
			at, ok = probe.lastTime(file, size)
		} else {
			at, ok = probe.timeAt(file, size, size*int64(index)/int64(grepOrderProbes-1))
		}
		if !ok || (index > 0 && at.Before(previous.Add(-grepTolerance))) {
			return first, previous, index + 1, false
		}
		if index == 0 {
			first = at
		}
		previous = at
	}
	return first, previous, grepOrderProbes, true
}

// lastTime은 파일 끝 grepProbeBytes 안에서 시각이 있는 마지막 줄의 시각이다.
func (filter *grepFilter) lastTime(file io.ReaderAt, size int64) (time.Time, bool) {
	start := size - grepProbeBytes
	if start < 0 {
		start = 0
	}
	buffer := make([]byte, size-start)
	count, err := file.ReadAt(buffer, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, false
	}
	buffer = bytes.TrimSuffix(buffer[:count], []byte{'\n'})
	if start > 0 {
		newline := bytes.IndexByte(buffer, '\n')
		if newline < 0 {
			return time.Time{}, false
		}
		buffer = buffer[newline+1:]
	}
	// 이어지는 줄은 앞 줄의 시각을 물려받으므로 앞에서부터 추적하고 마지막으로 읽힌 시각을 남긴다.
	var last time.Time
	found := false
	for _, line := range bytes.Split(buffer, []byte{'\n'}) {
		fields := filter.track(line)
		if at, ok := filter.timeOf(fields); ok {
			last, found = at, true
		}
	}
	return last, found
}

// endsBefore는 파일의 모든 줄이 target 앞인지다. 시간 순서인 파일의 마지막 시각으로 판단하고, 순서를 확인하지 못하면 false다.
func (filter *grepFilter) endsBefore(file io.ReaderAt, size int64, target time.Time) bool {
	_, last, _, ok := filter.bounds(file, size)
	return ok && last.Before(target)
}

// seek는 target 앞의 자리를 이진 탐색한다. bounds로 시간 순서를 확인한 뒤 가운데를 찍어 그 줄의 시각을 보고, 시각을 읽을 수
// 없으면 포기한다(ok=false). 돌려준 offset은 줄 중간일 수 있다.
func (filter *grepFilter) seek(file io.ReaderAt, size int64, target time.Time) (int64, int, bool) {
	first, _, probes, ok := filter.bounds(file, size)
	if !ok {
		return 0, probes, false
	}
	if !first.Before(target) {
		return 0, probes, true
	}
	probe := *filter
	probe.event = grepEvent{}
	timeAt := func(offset int64) (time.Time, bool) {
		probes++
		return probe.timeAt(file, size, offset)
	}
	low, high := int64(0), size
	for high-low > grepSeekStop {
		middle := (low + high) / 2
		at, ok := timeAt(middle)
		if !ok {
			return 0, probes, false
		}
		if at.Before(target) {
			low = middle
		} else {
			high = middle
		}
	}
	return low, probes, true
}

// timeAt은 offset 뒤 첫 시각이 있는 줄의 시각이다. offset이 줄 중간이면 다음 줄부터 본다.
func (filter *grepFilter) timeAt(file io.ReaderAt, size, offset int64) (time.Time, bool) {
	if offset < 0 {
		offset = 0
	}
	buffer := make([]byte, grepProbeBytes)
	count, err := file.ReadAt(buffer, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, false
	}
	buffer = buffer[:count]
	if offset > 0 {
		newline := bytes.IndexByte(buffer, '\n')
		if newline < 0 {
			return time.Time{}, false
		}
		buffer = buffer[newline+1:]
	}
	for len(buffer) != 0 {
		end := bytes.IndexByte(buffer, '\n')
		if end < 0 {
			// 잘린 마지막 줄의 시각은 믿을 수 없다.
			break
		}
		fields := filter.track(buffer[:end])
		if fields.event == nil && len(fields.timeText) != 0 {
			if at, ok := filter.times.parse(fields.timeText); ok {
				return at, true
			}
		}
		if end == len(buffer) {
			break
		}
		buffer = buffer[end+1:]
	}
	return time.Time{}, false
}

// grepCountNewlines는 [from, to)의 줄바꿈 수다. -n과 건너뛰기를 함께 쓸 때 건너뛴 구간의 줄 수를 센다.
func grepCountNewlines(file io.ReaderAt, from, to int64) (int64, error) {
	buffer := make([]byte, grepBlockSize)
	var count int64
	for from < to {
		want := to - from
		if want > int64(len(buffer)) {
			want = int64(len(buffer))
		}
		n, err := file.ReadAt(buffer[:want], from)
		count += int64(bytes.Count(buffer[:n], []byte{'\n'}))
		from += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return count, err
		}
	}
	return count, nil
}
