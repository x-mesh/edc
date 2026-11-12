package edc

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// viewExceptionMessageLimit는 예외 열쇠에 남기는 메시지의 글자 수다. 메시지 전체를 열쇠로 쓰면 긴 JSON이 든 예외가 패널을
// 가득 채운다.
const viewExceptionMessageLimit = 80

// viewExceptionSuffixes는 예외 클래스 이름의 끝이다. Java와 C#의 Exception, Error, Throwable, Python의 Error와 Exception,
// JavaScript의 Error를 함께 읽는다.
var viewExceptionSuffixes = []string{"Exception", "Error", "Throwable"}

func viewIsExceptionHead(text string) bool {
	_, ok := viewExceptionHead(text)
	return ok
}

// viewExceptionHead는 stack trace에서 예외를 적은 줄이면 "단순 클래스 이름: 메시지" 꼴의 열쇠를 돌려준다.
//
//	java.lang.IllegalArgumentException: Invalid URI path
//	Caused by: java.net.SocketTimeoutException: Read timed out
//	Exception in thread "main" java.lang.NullPointerException
//	ValueError: invalid literal for int() with base 10: 'x'
//
// 메시지에서 숫자가 든 낱말은 N으로 바꾼다. 요청마다 다른 ID나 주소가 든 메시지를 한 열쇠로 모으기 위해서다.
func viewExceptionHead(text string) (string, bool) {
	rest := strings.TrimPrefix(text, "Caused by: ")
	if strings.HasPrefix(rest, `Exception in thread "`) {
		end := strings.Index(rest[len(`Exception in thread "`):], `" `)
		if end < 0 {
			return "", false
		}
		rest = rest[len(`Exception in thread "`)+end+2:]
	}
	name, message, _ := strings.Cut(rest, ": ")
	name = strings.TrimSuffix(name, ":")
	if !viewExceptionName(name) {
		return "", false
	}
	key := name[strings.LastIndexByte(name, '.')+1:]
	if message = viewNormalizeMessage(message); message != "" {
		key += ": " + message
	}
	return key, true
}

// viewExceptionName은 점으로 이은 식별자이고 마지막 이름이 대문자로 시작해 viewExceptionSuffixes로 끝나는지 본다. 공백이 든
// 낱말은 예외 이름이 아니다.
func viewExceptionName(name string) bool {
	if name == "" {
		return false
	}
	parts := strings.Split(name, ".")
	for _, part := range parts {
		if !viewIdentifier(part) {
			return false
		}
	}
	last := parts[len(parts)-1]
	if first, _ := utf8.DecodeRuneInString(last); !unicode.IsUpper(first) {
		return false
	}
	for _, suffix := range viewExceptionSuffixes {
		if strings.HasSuffix(last, suffix) {
			return true
		}
	}
	return false
}

func viewIdentifier(part string) bool {
	if part == "" {
		return false
	}
	for index, r := range part {
		if !(r == '_' || r == '$' || unicode.IsLetter(r) || index > 0 && unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// viewNormalizeMessage는 예외 메시지의 숫자가 든 낱말을 N으로 바꾸고, 공백을 하나로 줄이고, viewExceptionMessageLimit에서
// 자른다. 낱말은 글자, 숫자, _가 이어진 것이다.
func viewNormalizeMessage(message string) string {
	var builder strings.Builder
	runes := []rune(strings.Join(strings.Fields(message), " "))
	for index := 0; index < len(runes); {
		if !viewWordRune(runes[index]) {
			builder.WriteRune(runes[index])
			index++
			continue
		}
		end, digit := index, false
		for end < len(runes) && viewWordRune(runes[end]) {
			digit = digit || unicode.IsDigit(runes[end])
			end++
		}
		if digit {
			builder.WriteString("N")
		} else {
			builder.WriteString(string(runes[index:end]))
		}
		index = end
	}
	normalized := []rune(builder.String())
	if len(normalized) > viewExceptionMessageLimit {
		return string(normalized[:viewExceptionMessageLimit-1]) + "…"
	}
	return string(normalized)
}

func viewWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
