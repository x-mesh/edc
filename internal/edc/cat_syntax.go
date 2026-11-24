package edc

import (
	"path/filepath"
	"strconv"
	"strings"
)

// cat_syntax.go는 설정 파일 한 줄을 키, 값, 주석으로 나눈다. 색을 입히는 데와 --mask로 값을 가리는 데에 같은 결과를 쓴다.
// 파일을 끝까지 읽지 않고 줄마다 나누므로, 여러 줄에 걸친 문자열은 catLexer가 열린 상태를 들고 다음 줄로 넘긴다.

type catFormat int

const (
	catFormatPlain catFormat = iota
	catFormatEnv
	catFormatJSON
	catFormatYAML
	catFormatTOML
	catFormatINI
)

func (format catFormat) name() string {
	switch format {
	case catFormatEnv:
		return "dotenv"
	case catFormatJSON:
		return "json"
	case catFormatYAML:
		return "yaml"
	case catFormatTOML:
		return "toml"
	case catFormatINI:
		return "ini"
	}
	return "text"
}

// detectCatFormat은 파일 이름으로 형식을 정한다. 내용은 보지 않는다. 앞 몇 줄로 추측하면 --mask가 형식을 잘못 짚어
// 값을 키로 보고 내보낼 수 있다.
func detectCatFormat(path string) catFormat {
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return catFormatEnv
	}
	switch filepath.Ext(base) {
	case ".json", ".jsonl", ".ndjson":
		return catFormatJSON
	case ".yaml", ".yml":
		return catFormatYAML
	case ".toml":
		return catFormatTOML
	// systemd unit도 [Section]과 key=value로 쓴다.
	case ".ini", ".cfg", ".cnf", ".conf", ".properties", ".service", ".socket", ".timer", ".mount", ".target":
		return catFormatINI
	}
	return catFormatPlain
}

type catClass int

const (
	catPlain catClass = iota
	catKey
	// catString, catLiteral, catValue는 값이다. --mask는 이 셋을 가린다.
	catString
	catLiteral
	catValue
	catComment
	catSection
	catMasked
	// catKeyword와 catDeleted는 소스 코드에서만 나온다(cat_code.go).
	catKeyword
	catDeleted
)

type catSpan struct {
	text  string
	class catClass
}

// catMaskText는 가린 값 자리에 쓴다. 값의 길이를 드러내지 않도록 길이를 고정한다.
const catMaskText = "••••••••"

// catLexer는 한 파일을 위에서부터 읽으며 줄을 나눈다. 필드는 앞 줄에서 열려 아직 닫히지 않은 값이다.
type catLexer struct {
	format catFormat
	// mask면 값 뒤의 주석을 따로 떼지 않고 값에 넣는다. 따옴표 없는 값에 든 #을 주석으로 잘못 보면 그 뒤가 그대로 나간다.
	mask bool
	// quote는 env와 yaml에서 닫히지 않은 따옴표다.
	quote byte
	// tomlString은 닫히지 않은 """나 '''이고, tomlDepth는 닫히지 않은 배열과 inline table의 깊이다.
	tomlString string
	tomlDepth  int
	// yamlBlock이면 | 나 > 로 연 block scalar 안이다. yamlBlockIndent보다 깊이 들여 쓴 줄까지가 그 값이다.
	yamlBlock       bool
	yamlBlockIndent int
	// iniContinued면 앞 줄이 \로 끝나 이 줄까지 값이 이어진다.
	iniContinued bool
}

func (lexer *catLexer) line(text string) []catSpan {
	var spans []catSpan
	switch lexer.format {
	case catFormatEnv:
		spans = lexer.env(text)
	case catFormatJSON:
		spans = lexer.json(text)
	case catFormatYAML:
		spans = lexer.yaml(text)
	case catFormatTOML:
		spans = lexer.toml(text)
	case catFormatINI:
		spans = lexer.ini(text)
	default:
		return catAppend(nil, text, catPlain)
	}
	if lexer.mask {
		return catMaskSpans(spans)
	}
	return spans
}

// catAppend는 빈 조각을 버리고, 앞 조각과 종류가 같으면 잇는다.
func catAppend(spans []catSpan, text string, class catClass) []catSpan {
	if text == "" {
		return spans
	}
	if last := len(spans) - 1; last >= 0 && spans[last].class == class {
		spans[last].text += text
		return spans
	}
	return append(spans, catSpan{text, class})
}

// catMaskSpans는 값을 catMaskText로 바꾼다. 잇닿았거나 공백으로만 떨어진 값은 하나로 합쳐 낱말 수도 드러내지 않는다.
func catMaskSpans(spans []catSpan) []catSpan {
	masked := make([]catSpan, 0, len(spans))
	for _, span := range spans {
		switch span.class {
		case catString, catLiteral, catValue:
			last := len(masked) - 1
			if last >= 1 && masked[last].class == catPlain && strings.TrimSpace(masked[last].text) == "" && masked[last-1].class == catMasked {
				masked = masked[:last]
				continue
			}
			if last >= 0 && masked[last].class == catMasked {
				continue
			}
			masked = append(masked, catSpan{catMaskText, catMasked})
		default:
			masked = append(masked, span)
		}
	}
	return masked
}

func catRender(spans []catSpan, color bool) string {
	var builder strings.Builder
	for _, span := range spans {
		if !color {
			builder.WriteString(span.text)
			continue
		}
		switch span.class {
		case catKey:
			builder.WriteString(richPaint(span.text, richCyan, false))
		case catString:
			builder.WriteString(richPaint(span.text, richGreen, false))
		case catLiteral:
			builder.WriteString(richPaint(span.text, richYellow, false))
		case catComment, catMasked:
			builder.WriteString(richDim(span.text))
		case catSection:
			builder.WriteString(richPaint(span.text, richMagenta, true))
		case catKeyword:
			builder.WriteString(richPaint(span.text, richBlue, false))
		case catDeleted:
			builder.WriteString(richPaint(span.text, richRed, false))
		default:
			builder.WriteString(span.text)
		}
	}
	return builder.String()
}

func catIndent(text string) int {
	return len(text) - len(strings.TrimLeft(text, " \t"))
}

// catSimpleKey는 env와 ini의 키로 볼 이름인지 본다. .conf에는 nginx처럼 key=value가 아닌 파일도 있다. 그런 줄에서
// 첫 = 앞을 키로 보면 "proxy_pass http://user:pass@host/?a=b"의 앞부분이 --mask에서 그대로 나간다.
func catSimpleKey(key string) bool {
	return key != "" && strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-") == ""
}

// catValueClass는 따옴표 없는 값이 숫자나 true, false, null인지 본다.
func catValueClass(text string) catClass {
	switch strings.ToLower(text) {
	case "true", "false", "null":
		return catLiteral
	}
	if text == "" || !strings.ContainsRune("+-.0123456789", rune(text[0])) {
		return catValue
	}
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return catLiteral
	}
	if _, err := strconv.ParseInt(text, 0, 64); err == nil {
		return catLiteral
	}
	return catValue
}

// catQuoteEnd는 start부터 찾은 닫는 따옴표 바로 뒤의 위치다. 없으면 -1이다. 큰따옴표 안의 \는 다음 글자를 건너뛴다.
// doubled면 작은따옴표 안에서 잇따른 작은따옴표 두 개를 따옴표 글자로 읽는다(YAML).
func catQuoteEnd(text string, start int, quote byte, doubled bool) int {
	for index := start; index < len(text); index++ {
		switch {
		case quote == '"' && text[index] == '\\':
			index++
		case text[index] == quote:
			if doubled && quote == '\'' && index+1 < len(text) && text[index+1] == '\'' {
				index++
				continue
			}
			return index + 1
		}
	}
	return -1
}

// tail은 닫는 따옴표나 table 머리 뒤에 남은 부분이다. 주석이 올 자리지만, mask에서는 값으로 본다.
func (lexer *catLexer) tail(spans []catSpan, rest string) []catSpan {
	indent := catIndent(rest)
	body := rest[indent:]
	switch {
	case body == "":
		return catAppend(spans, rest, catPlain)
	case lexer.mask:
		return catAppend(spans, rest, catValue)
	case body[0] == '#':
		return catAppend(catAppend(spans, rest[:indent], catPlain), body, catComment)
	}
	return catAppend(spans, rest, catPlain)
}

// plainValue는 따옴표 없는 값과 그 뒤의 주석이다.
func (lexer *catLexer) plainValue(spans []catSpan, body string) []catSpan {
	if !lexer.mask {
		if comment := strings.Index(body, " #"); comment >= 0 {
			text := strings.TrimRight(body[:comment], " \t")
			spans = catAppend(spans, text, catValueClass(text))
			spans = catAppend(spans, body[len(text):comment+1], catPlain)
			return catAppend(spans, body[comment+1:], catComment)
		}
	}
	text := strings.TrimRight(body, " \t")
	spans = catAppend(spans, text, catValueClass(text))
	return catAppend(spans, body[len(text):], catPlain)
}

func (lexer *catLexer) env(text string) []catSpan {
	var spans []catSpan
	if lexer.quote != 0 {
		end := catQuoteEnd(text, 0, lexer.quote, false)
		if end < 0 {
			return catAppend(spans, text, catString)
		}
		lexer.quote = 0
		return lexer.tail(catAppend(spans, text[:end], catString), text[end:])
	}
	indent := catIndent(text)
	rest := text[indent:]
	spans = catAppend(spans, text[:indent], catPlain)
	if rest == "" {
		return spans
	}
	if rest[0] == '#' {
		return catAppend(spans, rest, catComment)
	}
	if after, ok := strings.CutPrefix(rest, "export "); ok {
		after = strings.TrimLeft(after, " \t")
		spans = catAppend(spans, rest[:len(rest)-len(after)], catPlain)
		rest = after
	}
	separator := strings.IndexByte(rest, '=')
	if separator <= 0 {
		return catAppend(spans, rest, catValue)
	}
	key := strings.TrimRight(rest[:separator], " \t")
	if !catSimpleKey(key) {
		return catAppend(spans, rest, catValue)
	}
	spans = catAppend(spans, key, catKey)
	spans = catAppend(spans, rest[len(key):separator+1], catPlain)
	value := rest[separator+1:]
	indent = catIndent(value)
	spans = catAppend(spans, value[:indent], catPlain)
	value = value[indent:]
	if value == "" {
		return spans
	}
	if value[0] == '"' || value[0] == '\'' {
		end := catQuoteEnd(value, 1, value[0], false)
		if end < 0 {
			lexer.quote = value[0]
			return catAppend(spans, value, catString)
		}
		return lexer.tail(catAppend(spans, value[:end], catString), value[end:])
	}
	return lexer.plainValue(spans, value)
}

func (lexer *catLexer) ini(text string) []catSpan {
	indent := catIndent(text)
	rest := text[indent:]
	spans := catAppend(nil, text[:indent], catPlain)
	if rest == "" {
		lexer.iniContinued = false
		return spans
	}
	// \로 끝난 줄의 다음 줄은 key=value처럼 보여도 앞 값의 나머지다(.properties, systemd unit).
	continued := lexer.iniContinued
	lexer.iniContinued = strings.HasSuffix(strings.TrimRight(rest, " \t"), "\\")
	if continued {
		return catAppend(spans, rest, catValue)
	}
	if rest[0] == '#' || rest[0] == ';' {
		lexer.iniContinued = false
		return catAppend(spans, rest, catComment)
	}
	trimmed := strings.TrimRight(rest, " \t")
	if rest[0] == '[' && strings.HasSuffix(trimmed, "]") {
		return catAppend(catAppend(spans, trimmed, catSection), rest[len(trimmed):], catPlain)
	}
	separator := strings.IndexByte(rest, '=')
	if separator <= 0 {
		return catAppend(spans, rest, catValue)
	}
	key := strings.TrimRight(rest[:separator], " \t")
	if !catSimpleKey(key) {
		return catAppend(spans, rest, catValue)
	}
	value := rest[separator+1:]
	// ini에는 여러 줄 값의 끝을 알리는 표시가 없다. 들여 쓴 줄은 앞 값이 이어진 줄일 수 있고, = 뒤가 비었거나 =뿐인 줄은
	// base64 덩어리의 마지막 줄일 수 있다. mask에서는 이런 줄의 앞부분을 키로 믿지 않고 줄 전체를 가린다.
	if lexer.mask && (indent > 0 || strings.Trim(value, "= \t") == "") {
		return catAppend(spans, rest, catValue)
	}
	spans = catAppend(spans, key, catKey)
	spans = catAppend(spans, rest[len(key):separator+1], catPlain)
	indent = catIndent(value)
	spans = catAppend(spans, value[:indent], catPlain)
	body := strings.TrimRight(value[indent:], " \t")
	class := catValueClass(body)
	if len(body) >= 2 && (body[0] == '"' || body[0] == '\'') && body[len(body)-1] == body[0] {
		class = catString
	}
	spans = catAppend(spans, body, class)
	return catAppend(spans, value[indent+len(body):], catPlain)
}

func (lexer *catLexer) json(text string) []catSpan {
	var spans []catSpan
	for index := 0; index < len(text); {
		char := text[index]
		end := index + 1
		switch {
		case char == ' ' || char == '\t':
			spans = catAppend(spans, text[index:end], catPlain)
		case char == '"':
			if end = catQuoteEnd(text, index+1, '"', false); end < 0 {
				end = len(text)
			}
			class := catString
			if strings.HasPrefix(strings.TrimLeft(text[end:], " \t"), ":") {
				class = catKey
			}
			spans = catAppend(spans, text[index:end], class)
		case strings.IndexByte("{}[],:", char) >= 0:
			spans = catAppend(spans, text[index:end], catPlain)
		default:
			for end < len(text) && strings.IndexByte(" \t{}[],:\"", text[end]) < 0 {
				end++
			}
			spans = catAppend(spans, text[index:end], catValueClass(text[index:end]))
		}
		index = end
	}
	return spans
}

func (lexer *catLexer) toml(text string) []catSpan {
	var spans []catSpan
	if lexer.tomlString != "" {
		end := catTOMLStringEnd(text, 0, lexer.tomlString)
		if end < 0 {
			return catAppend(spans, text, catString)
		}
		lexer.tomlString = ""
		return lexer.tomlValues(catAppend(spans, text[:end], catString), text[end:])
	}
	if lexer.tomlDepth > 0 {
		return lexer.tomlValues(spans, text)
	}
	indent := catIndent(text)
	rest := text[indent:]
	spans = catAppend(spans, text[:indent], catPlain)
	if rest == "" {
		return spans
	}
	if rest[0] == '#' {
		return catAppend(spans, rest, catComment)
	}
	if rest[0] == '[' {
		head := rest
		if comment := strings.IndexByte(rest, '#'); comment >= 0 {
			head = rest[:comment]
		}
		end := strings.LastIndexByte(head, ']')
		if end < 0 {
			return catAppend(spans, rest, catValue)
		}
		return lexer.tail(catAppend(spans, rest[:end+1], catSection), rest[end+1:])
	}
	separator := catTOMLSeparator(rest)
	if separator <= 0 {
		return catAppend(spans, rest, catValue)
	}
	key := strings.TrimRight(rest[:separator], " \t")
	spans = catAppend(spans, key, catKey)
	spans = catAppend(spans, rest[len(key):separator+1], catPlain)
	return lexer.tomlValues(spans, rest[separator+1:])
}

// catTOMLSeparator는 키와 값을 나누는 =의 위치다. 따옴표로 감싼 키 안의 =는 건너뛴다.
func catTOMLSeparator(rest string) int {
	for index := 0; index < len(rest); index++ {
		switch rest[index] {
		case '"', '\'':
			end := catQuoteEnd(rest, index+1, rest[index], false)
			if end < 0 {
				return -1
			}
			index = end - 1
		case '=':
			return index
		case '#':
			return -1
		}
	}
	return -1
}

// catTOMLStringEnd는 여러 줄 문자열을 닫는 delimiter 바로 뒤의 위치다. 없으면 -1이다.
func catTOMLStringEnd(text string, start int, delimiter string) int {
	for index := start; index < len(text); index++ {
		if delimiter == `"""` && text[index] == '\\' {
			index++
			continue
		}
		if strings.HasPrefix(text[index:], delimiter) {
			return index + len(delimiter)
		}
	}
	return -1
}

// tomlValues는 = 뒤의 값이다. 배열과 inline table이 다음 줄로 이어지면 tomlDepth에 남긴다.
func (lexer *catLexer) tomlValues(spans []catSpan, text string) []catSpan {
	for index := 0; index < len(text); {
		char := text[index]
		end := index + 1
		switch {
		case char == ' ' || char == '\t':
			spans = catAppend(spans, text[index:end], catPlain)
		case char == '#':
			end = len(text)
			class := catComment
			if lexer.mask {
				class = catValue
			}
			spans = catAppend(spans, text[index:], class)
		case strings.HasPrefix(text[index:], `"""`) || strings.HasPrefix(text[index:], `'''`):
			delimiter := text[index : index+3]
			if end = catTOMLStringEnd(text, index+3, delimiter); end < 0 {
				lexer.tomlString = delimiter
				end = len(text)
			}
			spans = catAppend(spans, text[index:end], catString)
		case char == '"' || char == '\'':
			if end = catQuoteEnd(text, index+1, char, false); end < 0 {
				end = len(text)
			}
			spans = catAppend(spans, text[index:end], catString)
		case char == '[' || char == '{':
			lexer.tomlDepth++
			spans = catAppend(spans, text[index:end], catPlain)
		case char == ']' || char == '}':
			if lexer.tomlDepth > 0 {
				lexer.tomlDepth--
			}
			spans = catAppend(spans, text[index:end], catPlain)
		case char == ',' || char == '=':
			spans = catAppend(spans, text[index:end], catPlain)
		default:
			for end < len(text) && strings.IndexByte(" \t,]}=#", text[end]) < 0 {
				end++
			}
			class := catValueClass(text[index:end])
			// inline table 안의 키다.
			if strings.HasPrefix(strings.TrimLeft(text[end:], " \t"), "=") {
				class = catKey
			}
			spans = catAppend(spans, text[index:end], class)
		}
		index = end
	}
	return spans
}

func (lexer *catLexer) yaml(text string) []catSpan {
	var spans []catSpan
	if lexer.quote != 0 {
		end := catQuoteEnd(text, 0, lexer.quote, true)
		if end < 0 {
			return catAppend(spans, text, catString)
		}
		lexer.quote = 0
		return lexer.tail(catAppend(spans, text[:end], catString), text[end:])
	}
	indent := catIndent(text)
	rest := text[indent:]
	spans = catAppend(spans, text[:indent], catPlain)
	if rest == "" {
		return spans
	}
	if lexer.yamlBlock {
		if indent > lexer.yamlBlockIndent {
			return catAppend(spans, rest, catString)
		}
		lexer.yamlBlock = false
	}
	if rest[0] == '#' {
		return catAppend(spans, rest, catComment)
	}
	if rest == "---" || rest == "..." {
		return catAppend(spans, rest, catSection)
	}
	column := indent
	// parent는 이 줄의 값이 block scalar를 열 때 그 값이 끝나는 들여쓰기다. 키가 있으면 키의 열, 없으면 마지막 - 의 열이다.
	parent := indent - 1
	if after, ok := strings.CutPrefix(rest, "--- "); ok {
		after = strings.TrimLeft(after, " ")
		spans = catAppend(spans, "---", catSection)
		spans = catAppend(spans, rest[3:len(rest)-len(after)], catPlain)
		column += len(rest) - len(after)
		parent = -1
		rest = after
	}
	for rest == "-" || strings.HasPrefix(rest, "- ") {
		after := strings.TrimLeft(rest[1:], " ")
		spans = catAppend(spans, rest[:len(rest)-len(after)], catPlain)
		parent = column
		column += len(rest) - len(after)
		rest = after
	}
	if rest == "" {
		return spans
	}
	if keyEnd, valueStart := catYAMLKey(rest); keyEnd >= 0 {
		spans = catAppend(spans, rest[:keyEnd], catKey)
		spans = catAppend(spans, rest[keyEnd:valueStart], catPlain)
		return lexer.yamlValue(spans, rest[valueStart:], column)
	}
	return lexer.yamlValue(spans, rest, parent)
}

// catYAMLKey는 rest가 "key: value"나 "key:"일 때 키의 끝과 값의 시작이다. 키가 아니면 -1이다.
func catYAMLKey(rest string) (int, int) {
	separates := func(index int) bool {
		return index < len(rest) && rest[index] == ':' && (index+1 == len(rest) || rest[index+1] == ' ' || rest[index+1] == '\t')
	}
	if rest[0] == '"' || rest[0] == '\'' {
		end := catQuoteEnd(rest, 1, rest[0], true)
		if end < 0 {
			return -1, -1
		}
		if colon := end + catIndent(rest[end:]); separates(colon) {
			return end, colon + 1
		}
		return -1, -1
	}
	// flow collection, anchor, alias, tag, block scalar로 시작하면 키로 읽지 않는다.
	if strings.IndexByte("[{&*!|>", rest[0]) >= 0 {
		return -1, -1
	}
	for index := 0; index < len(rest); index++ {
		if rest[index] == '#' && index > 0 && rest[index-1] == ' ' {
			return -1, -1
		}
		if separates(index) {
			return index, index + 1
		}
	}
	return -1, -1
}

// catYAMLBlockHeader는 body가 block scalar를 여는 |, >, |-, >2 같은 표시인지 본다. 앞에 anchor나 tag가 올 수 있다.
func catYAMLBlockHeader(body string) bool {
	if comment := strings.Index(body, " #"); comment >= 0 {
		body = body[:comment]
	}
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false
	}
	last := fields[len(fields)-1]
	if (last[0] != '|' && last[0] != '>') || len(last) > 3 || strings.Trim(last[1:], "+-0123456789") != "" {
		return false
	}
	for _, field := range fields[:len(fields)-1] {
		if field[0] != '&' && field[0] != '!' {
			return false
		}
	}
	return true
}

func (lexer *catLexer) yamlValue(spans []catSpan, value string, parent int) []catSpan {
	indent := catIndent(value)
	spans = catAppend(spans, value[:indent], catPlain)
	body := value[indent:]
	if body == "" {
		return spans
	}
	if body[0] == '#' {
		return catAppend(spans, body, catComment)
	}
	if catYAMLBlockHeader(body) {
		lexer.yamlBlock = true
		lexer.yamlBlockIndent = parent
		return catAppend(spans, body, catPlain)
	}
	if body[0] == '"' || body[0] == '\'' {
		end := catQuoteEnd(body, 1, body[0], true)
		if end < 0 {
			lexer.quote = body[0]
			return catAppend(spans, body, catString)
		}
		return lexer.tail(catAppend(spans, body[:end], catString), body[end:])
	}
	return lexer.plainValue(spans, body)
}
