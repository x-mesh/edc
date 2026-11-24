package edc

import (
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// cat_code.go는 소스 코드를 chroma로 나눈다. 설정 형식은 cat_syntax.go가 직접 읽고, 그 밖의 파일만 여기로 온다.

// catCodeLimit은 소스 코드에 색을 입히는 크기 상한이다. chroma는 내용 전체를 한 번에 받아 나누고, 2026-11에 Go,
// Python, minified JavaScript로 쟀을 때 1 MB에 1초쯤 걸렸다. 이보다 큰 파일은 줄 단위로 색 없이 낸다.
const catCodeLimit = 512 << 10

// catSyntax는 입력 하나를 나누는 방법이다. format은 edc가 직접 읽는 설정 형식이고, code는 소스 코드의 chroma lexer다.
// 둘 다 없으면 평문이다.
type catSyntax struct {
	format catFormat
	code   chroma.Lexer
}

func (syntax catSyntax) name() string {
	if syntax.code != nil {
		config := syntax.code.Config()
		if len(config.Aliases) > 0 {
			return config.Aliases[0]
		}
		return strings.ToLower(config.Name)
	}
	if syntax.format != catFormatPlain {
		return syntax.format.name()
	}
	return ""
}

// maskable은 --mask가 값을 가릴 수 있는 형식인지다. edc가 직접 읽는 설정 형식만 해당한다. 소스 코드는 문자열과 숫자를
// 가려도 주석과 따옴표 없는 값이 남는다.
func (syntax catSyntax) maskable() bool {
	return syntax.code == nil && syntax.format != catFormatPlain
}

// catCodeLexer는 색을 입힐 것이 없는 plaintext lexer를 버린다.
func catCodeLexer(lexer chroma.Lexer) chroma.Lexer {
	if lexer == nil || lexer.Config().Name == "plaintext" {
		return nil
	}
	return lexer
}

// catSyntaxOf는 파일 이름으로 형식을 정한다.
func catSyntaxOf(path string) catSyntax {
	base := filepath.Base(path)
	format := detectCatFormat(path)
	// nginx.conf나 apache2.conf는 key=value 형식이 아니다. chroma가 아는 이름의 .conf는 그 lexer로 읽는다.
	if format == catFormatPlain || strings.EqualFold(filepath.Ext(base), ".conf") {
		if lexer := catCodeLexer(lexers.Match(base)); lexer != nil {
			return catSyntax{code: lexer}
		}
	}
	return catSyntax{format: format}
}

// catSyntaxNamed는 --as로 받은 이름의 형식이다. 모르는 이름이면 false다.
func catSyntaxNamed(name string) (catSyntax, bool) {
	switch strings.ToLower(name) {
	case "dotenv", "env":
		return catSyntax{format: catFormatEnv}, true
	case "json":
		return catSyntax{format: catFormatJSON}, true
	case "yaml", "yml":
		return catSyntax{format: catFormatYAML}, true
	case "toml":
		return catSyntax{format: catFormatTOML}, true
	case "ini":
		return catSyntax{format: catFormatINI}, true
	case "text":
		return catSyntax{}, true
	}
	lexer := lexers.Get(name)
	if lexer == nil {
		return catSyntax{}, false
	}
	return catSyntax{code: catCodeLexer(lexer)}, true
}

func catCodeClass(token chroma.TokenType) catClass {
	switch {
	case token == chroma.KeywordConstant:
		return catLiteral
	case token.InCategory(chroma.Keyword), token == chroma.NameTag:
		return catKeyword
	case token == chroma.NameAttribute:
		return catKey
	case token.InCategory(chroma.Comment):
		return catComment
	case token.InSubCategory(chroma.LiteralString), token == chroma.GenericInserted:
		return catString
	case token.InSubCategory(chroma.LiteralNumber):
		return catLiteral
	case token == chroma.GenericDeleted:
		return catDeleted
	case token == chroma.GenericHeading, token == chroma.GenericSubheading:
		return catSection
	}
	return catPlain
}

// catCodeLines는 소스 코드를 줄마다 색 조각으로 나눈다. lines는 escape를 지운 줄이다.
func catCodeLines(lexer chroma.Lexer, lines []string) [][]catSpan {
	result := make([][]catSpan, len(lines))
	if tokens, err := chroma.Tokenise(chroma.Coalesce(lexer), nil, strings.Join(lines, "\n")); err == nil {
		index := 0
		for _, token := range tokens {
			class := catCodeClass(token.Type)
			for position, part := range strings.Split(token.Value, "\n") {
				if position > 0 {
					index++
				}
				if index < len(lines) {
					result[index] = catAppend(result[index], part, class)
				}
			}
		}
	}
	// cat은 파일의 내용을 그대로 보여야 한다. lexer가 실패했거나 글자를 바꾸고 빠뜨린 줄은 색 없이 원문으로 낸다.
	for index, line := range lines {
		if catSpanText(result[index]) != line {
			result[index] = catAppend(nil, line, catPlain)
		}
	}
	return result
}

func catSpanText(spans []catSpan) string {
	var builder strings.Builder
	for _, span := range spans {
		builder.WriteString(span.text)
	}
	return builder.String()
}
