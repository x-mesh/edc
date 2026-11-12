package edc

import (
	"fmt"
	"strconv"
	"strings"
)

// viewFilter는 edc view의 filter 식이다. 공백으로 나눈 조건을 모두 만족하는 줄만 남긴다.
type viewFilter struct {
	terms []viewFilterTerm
}

// viewFilterTerm은 조건 하나다. field가 비어 있으면 줄 전체에서 value를 찾는다. field가 있으면 그 필드의 값이 pattern과
// 같아야 하고, pattern의 조각 사이(입력의 *)에는 아무 글자나 올 수 있다. TOP 목록의 항목으로 건 filter가 패널에 보인 수와
// 같은 줄을 고르도록, 필드는 부분 일치가 아니라 전체 일치로 비교한다.
type viewFilterTerm struct {
	field   string
	value   string
	pattern []string
	negate  bool
	// class는 status:5xx의 5다. status는 status:404의 404다.
	class  int
	status int
}

var viewFilterFields = []string{"path", "status", "client", "level", "program", "method", "rpc"}

func parseViewFilter(text string) (viewFilter, error) {
	tokens, err := viewFilterTokens(text)
	if err != nil {
		return viewFilter{}, err
	}
	var filter viewFilter
	for _, token := range tokens {
		term, err := parseViewFilterTerm(token)
		if err != nil {
			return viewFilter{}, err
		}
		filter.terms = append(filter.terms, term)
	}
	return filter, nil
}

// viewFilterToken은 따옴표를 벗긴 조건이다. quotedAt은 따옴표로 감싼 부분이 시작한 자리다. 따옴표 안의 -와 :는 글자다.
// raw는 입력한 그대로의 조건이라, 조건을 바꿔 filter를 다시 쓸 때 쓴다. text에는 \ escape가 그대로 남는다. *가 글자인지
// 와일드카드인지는 조건을 읽을 때 가린다.
type viewFilterToken struct {
	text     string
	quotedAt int
	raw      string
}

func viewFilterTokens(text string) ([]viewFilterToken, error) {
	var tokens []viewFilterToken
	var current strings.Builder
	quotedAt, quoted, started, start, escaped := -1, false, false, 0, false
	flush := func(end int) {
		if started {
			tokens = append(tokens, viewFilterToken{current.String(), quotedAt, text[start:end]})
		}
		current.Reset()
		quotedAt, started = -1, false
	}
	for at, r := range text {
		if !started && !quoted {
			start = at
		}
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			current.WriteRune(r)
			started, escaped = true, true
		case r == '"':
			if !quoted && quotedAt < 0 {
				quotedAt = current.Len()
			}
			quoted, started = !quoted, true
		case !quoted && (r == ' ' || r == '\t'):
			flush(at)
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if quoted {
		return nil, fmt.Errorf("a quote is not closed")
	}
	flush(len(text))
	return tokens, nil
}

func parseViewFilterTerm(token viewFilterToken) (viewFilterTerm, error) {
	text, quotedAt := token.text, token.quotedAt
	var term viewFilterTerm
	if strings.HasPrefix(text, "-") && len(text) > 1 && quotedAt != 0 {
		term.negate = true
		text = text[1:]
		if quotedAt > 0 {
			quotedAt--
		}
	}
	if colon := strings.IndexByte(text, ':'); colon > 0 && (quotedAt < 0 || colon < quotedAt) {
		field := strings.ToLower(text[:colon])
		for _, known := range viewFilterFields {
			if field == known {
				term.field = field
				break
			}
		}
		if term.field != "" {
			raw := strings.ToLower(text[colon+1:])
			if raw == "" {
				return viewFilterTerm{}, fmt.Errorf("%s: needs a value", field)
			}
			term.pattern = viewFilterPattern(raw)
			term.value = strings.Join(term.pattern, "*")
			if field == "status" {
				return parseViewFilterStatus(term)
			}
			return term, nil
		}
	}
	// 글자 조건에는 와일드카드가 없으므로 *는 글자다.
	term.value = strings.Join(viewFilterPattern(strings.ToLower(text)), "*")
	return term, nil
}

// viewFilterEscapable은 \ 뒤에서 글자 그대로 읽는 글자다. 그 밖의 \는 백슬래시 자체라, C:\temp 같은 값은 escape 없이 쓴다.
const viewFilterEscapable = "*\"\\ \t"

// viewFilterPattern은 escape를 풀면서 값을 와일드카드 *로 나눈 조각이다. \*는 나누지 않고 글자 *로 둔다.
func viewFilterPattern(raw string) []string {
	var parts []string
	var current strings.Builder
	runes := []rune(raw)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch {
		case r == '\\' && index+1 < len(runes) && strings.ContainsRune(viewFilterEscapable, runes[index+1]):
			index++
			current.WriteRune(runes[index])
		case r == '*':
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	return append(parts, current.String())
}

func parseViewFilterStatus(term viewFilterTerm) (viewFilterTerm, error) {
	value := term.value
	if len(value) == 3 && value[0] >= '1' && value[0] <= '5' && value[1:] == "xx" {
		term.class = int(value[0] - '0')
		return term, nil
	}
	if status, err := strconv.Atoi(value); err == nil && status >= 100 && status <= 999 {
		term.status = status
		return term, nil
	}
	return viewFilterTerm{}, fmt.Errorf("status:%s: use a class such as 5xx or a code such as 404", value)
}

func (filter viewFilter) matches(line viewLine) bool {
	if line.marker {
		return true
	}
	lower := ""
	for _, term := range filter.terms {
		var found bool
		switch term.field {
		case "":
			if lower == "" {
				lower = strings.ToLower(line.text)
			}
			found = strings.Contains(lower, term.value)
		case "status":
			found = line.status != 0 && (term.class == min(line.status/100, 5) || term.status == line.status)
		default:
			found = viewFilterWildcard(term.pattern, strings.ToLower(viewFilterFieldValue(line, term.field)))
		}
		if found == term.negate {
			return false
		}
	}
	return true
}

func viewFilterFieldValue(line viewLine, field string) string {
	switch field {
	case "path":
		return line.path
	case "client":
		return line.client
	case "level":
		return line.level
	case "program":
		return line.program
	case "method":
		return line.method
	case "rpc":
		return line.rpc
	}
	return ""
}

// viewFilterWildcard는 value 전체가 parts를 차례로 이은 것과 같은지 본다. 조각 사이에는 빈 글자를 포함해 아무 글자나 온다.
func viewFilterWildcard(parts []string, value string) bool {
	if value == "" {
		return false
	}
	if len(parts) == 1 {
		return parts[0] == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		at := strings.Index(value, part)
		if at < 0 {
			return false
		}
		value = value[at+len(part):]
	}
	return strings.HasSuffix(value, last)
}

// viewFilterWith는 filter에 field:value 조건을 더한 식이다. 같은 field를 고르는 조건이 이미 있으면 그 조건을 바꾼다.
// 다른 값을 고를 때마다 두 값을 함께 요구해 아무 줄도 남지 않는 일을 막는다. 제외 조건은 그대로 둔다.
func viewFilterWith(filter, field, value string) string {
	tokens, _ := viewFilterTokens(filter)
	kept := make([]string, 0, len(tokens)+1)
	for _, token := range tokens {
		if term, err := parseViewFilterTerm(token); err == nil && term.field == field && !term.negate {
			continue
		}
		kept = append(kept, token.raw)
	}
	return strings.Join(append(kept, field+":"+viewFilterQuote(value)), " ")
}

// viewFilterQuote는 값을 그 값만 고르는 filter 글자로 쓴다. \, *, "를 escape하고, 공백이 있으면 따옴표로 감싼다.
func viewFilterQuote(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `"`, `\"`).Replace(value)
	if strings.ContainsAny(value, " \t") {
		return `"` + value + `"`
	}
	return value
}
