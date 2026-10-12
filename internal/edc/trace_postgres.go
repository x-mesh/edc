package edc

import (
	"bytes"
)

const (
	tracePostgresDefaultPort = 5432
	postgresEventPrefix      = "postgres_"
	postgresEventConnect     = "postgres_connect"
	postgresEventTLS         = "postgres_tls"
	postgresEventQuery       = "postgres_query"
	postgresEventResult      = "postgres_result"
	postgresEventError       = "postgres_error"
	postgresCommandConnect   = "connect"
	postgresCommandQuery     = "query"
	postgresCommandExecute   = "execute"
	postgresCommandPrepare   = "prepare"
	postgresCommandFunction  = "function"
	// postgresSQLLimit은 event와 저장한 prepared statement의 SQL 상한이다. mysqlSQLLimit과 같다.
	postgresSQLLimit = 1024
	// postgresMessageLimit은 ErrorResponse의 message와 detail 상한이다.
	postgresMessageLimit = 1024
	// 아래 세 상한은 넘으면 모두 비운다. 비운 뒤 이어지는 응답은 짝 없이 사라지고, execute는 SQL 없이 보인다.
	postgresSocketLimit    = 65536
	postgresStatementLimit = 65536
	postgresPendingLimit   = 1024
)

// tracePostgresEvent는 PostgreSQL event에만 붙는 값이다. 응답 event에는 답한 요청의 command와 SQL을 함께 붙인다.
type tracePostgresEvent struct {
	Command       string  `json:"command,omitempty"`
	SQL           string  `json:"sql,omitempty"`
	SQLTruncated  bool    `json:"sql_truncated,omitempty"`
	Statement     string  `json:"statement,omitempty"`
	Executes      int     `json:"executes,omitempty"`
	User          string  `json:"user,omitempty"`
	Database      string  `json:"database,omitempty"`
	ServerVersion string  `json:"server_version,omitempty"`
	Encryption    string  `json:"encryption,omitempty"`
	Tag           string  `json:"tag,omitempty"`
	Rows          *uint64 `json:"rows,omitempty"`
	Severity      string  `json:"severity,omitempty"`
	SQLState      string  `json:"sql_state,omitempty"`
	Message       string  `json:"message,omitempty"`
	Detail        string  `json:"detail,omitempty"`
}

// postgresEvent는 captureEvent에 PostgreSQL 값을 붙인 것이다. 이 topic은 기존 파일을 고치지 않으므로 captureEvent에 아직
// Postgres 필드가 없다. trace에 연결하는 topic이 Postgres를 captureEvent로 옮기면 이 type은 captureEvent로 바뀐다.
type postgresEvent struct {
	captureEvent
	Postgres *tracePostgresEvent `json:"postgres,omitempty"`
}

// postgresTracker는 socket마다 두 방향의 message를 따라가며 요청 단위와 ReadyForQuery를 짝짓는다. side는 tracker가 볼
// 쪽이고 비어 있으면 두 쪽을 모두 본다.
type postgresTracker struct {
	side        string
	showSecrets bool
}

func newPostgresTracker(side string, showSecrets bool) *postgresTracker {
	return &postgresTracker{side: side, showSecrets: showSecrets}
}

// sqlText는 event에 붙일 SQL이다. 문자열 literal은 --show-secrets가 아니면 가린다. standardStrings는 연결의
// standard_conforming_strings이고, 꺼져 있으면 '...' 안의 backslash가 escape다.
func (tracker *postgresTracker) sqlText(raw []byte, truncated, standardStrings bool) (string, bool) {
	if !tracker.showSecrets {
		raw = postgresMaskSQL(raw, standardStrings)
	}
	text := string(raw)
	if len(text) > postgresSQLLimit {
		text, truncated = traceTrimText(text, postgresSQLLimit), true
	}
	return traceEscapeText([]byte(text)), truncated
}

// postgresIdentifierByte는 PostgreSQL identifier를 잇는 byte다. $는 첫 byte가 아닐 때만 identifier에 들어간다.
func postgresIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= 0x80 || (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func postgresTagStart(value byte) bool {
	return value == '_' || value >= 0x80 || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

// postgresLiteralEnd는 i의 따옴표에서 시작하는 문자열의 끝과 닫혔는지를 돌려준다. 두 번 쓴 따옴표는 문자열 안의
// 따옴표다. backslash는 E'...'와 standard_conforming_strings=off일 때만 escape다.
func postgresLiteralEnd(sql []byte, i int, backslash bool) (int, bool) {
	for i++; i < len(sql); i++ {
		switch {
		case backslash && sql[i] == '\\':
			i++
		case sql[i] == '\'' && i+1 < len(sql) && sql[i+1] == '\'':
			i++
		case sql[i] == '\'':
			return i + 1, true
		}
	}
	return len(sql), false
}

// postgresQuotedEnd는 "..." identifier의 끝이다. 두 번 쓴 따옴표는 identifier 안의 따옴표다.
func postgresQuotedEnd(sql []byte, i int) int {
	for i++; i < len(sql); i++ {
		if sql[i] == '"' {
			if i+1 < len(sql) && sql[i+1] == '"' {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(sql)
}

// postgresCommentEnd는 i에서 주석이 시작하면 그 끝을, 아니면 i를 돌려준다. PostgreSQL의 /* */는 중첩된다.
func postgresCommentEnd(sql []byte, i int) int {
	switch {
	case sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-':
		if end := bytes.IndexByte(sql[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(sql)
	case sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*':
		depth := 0
		for j := i; j+1 < len(sql); {
			switch {
			case sql[j] == '/' && sql[j+1] == '*':
				depth, j = depth+1, j+2
			case sql[j] == '*' && sql[j+1] == '/':
				depth, j = depth-1, j+2
				if depth == 0 {
					return j
				}
			default:
				j++
			}
		}
		return len(sql)
	}
	return i
}

// postgresDollarTag는 i의 $에서 시작하는 dollar quote 여는 tag($$나 $tag$)의 끝을 돌려준다. $1 같은 parameter와
// identifier 안의 $는 tag가 아니다.
func postgresDollarTag(sql []byte, i int) (int, bool) {
	if i > 0 && postgresIdentifierByte(sql[i-1]) {
		return 0, false
	}
	j := i + 1
	if j < len(sql) && sql[j] == '$' {
		return j + 1, true
	}
	if j >= len(sql) || !postgresTagStart(sql[j]) {
		return 0, false
	}
	for j++; j < len(sql) && sql[j] != '$'; j++ {
		if !postgresIdentifierByte(sql[j]) {
			return 0, false
		}
	}
	if j >= len(sql) {
		return 0, false
	}
	return j + 1, true
}

// postgresMaskSQL은 문자열 상수와 dollar quote 본문을 ?로 바꾼다. 닫히지 않은 것은 끝까지 가린다. "..." identifier,
// 주석, 숫자, $1 parameter는 그대로 둔다. E'...', U&'...', B'...', X'...', N'...'은 앞 글자를 남기고 본문만 가린다.
// MySQL과 달리 standard_conforming_strings=on이면 '...' 안의 backslash는 escape가 아니다.
func postgresMaskSQL(sql []byte, standardStrings bool) []byte {
	out := make([]byte, 0, len(sql))
	literal := func(i int, backslash bool) int {
		end, closed := postgresLiteralEnd(sql, i, backslash)
		out = append(out, '\'', '?')
		if closed {
			out = append(out, '\'')
		}
		return end
	}
	for i := 0; i < len(sql); {
		value := sql[i]
		tokenStart := i == 0 || !postgresIdentifierByte(sql[i-1])
		next := func(n int) byte {
			if i+n < len(sql) {
				return sql[i+n]
			}
			return 0
		}
		switch {
		case value == '\'':
			i = literal(i, !standardStrings)
		case tokenStart && (value == 'E' || value == 'e') && next(1) == '\'':
			out = append(out, value)
			i = literal(i+1, true)
		case tokenStart && (value == 'U' || value == 'u') && next(1) == '&' && next(2) == '\'':
			out = append(out, value, '&')
			i = literal(i+2, false)
		case tokenStart && (value == 'B' || value == 'b' || value == 'X' || value == 'x') && next(1) == '\'':
			out = append(out, value)
			i = literal(i+1, false)
		case tokenStart && (value == 'N' || value == 'n') && next(1) == '\'':
			out = append(out, value)
			i = literal(i+1, !standardStrings)
		case value == '"':
			end := postgresQuotedEnd(sql, i)
			out = append(out, sql[i:end]...)
			i = end
		case value == '$':
			tagEnd, ok := postgresDollarTag(sql, i)
			if !ok {
				out = append(out, value)
				i++
				continue
			}
			tag := sql[i:tagEnd]
			out = append(out, tag...)
			out = append(out, '?')
			if end := bytes.Index(sql[tagEnd:], tag); end >= 0 {
				out = append(out, tag...)
				i = tagEnd + end + len(tag)
			} else {
				i = len(sql)
			}
		case tokenStart && postgresIdentifierByte(value):
			// identifier와 숫자는 통째로 넘긴다. 그래야 abcE'..'의 E를 접두어로 읽지 않는다.
			end := i + 1
			for end < len(sql) && postgresIdentifierByte(sql[end]) {
				end++
			}
			out = append(out, sql[i:end]...)
			i = end
		default:
			if end := postgresCommentEnd(sql, i); end > i {
				out = append(out, sql[i:end]...)
				i = end
				continue
			}
			out = append(out, value)
			i++
		}
	}
	return out
}
