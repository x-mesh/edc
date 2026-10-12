package edc

import (
	"strings"
	"testing"
)

func TestPostgresMaskSQLTable(t *testing.T) {
	for _, tc := range []struct {
		sql, want string
		standard  bool
	}{
		{`SELECT 'a''b', 1`, `SELECT '?', 1`, true},
		{`SELECT 'C:\' , 'x'`, `SELECT '?' , '?'`, true},
		{`SELECT E'a\'b', 'c'`, `SELECT E'?', '?'`, true},
		{`SELECT e'a\\', 'c'`, `SELECT e'?', '?'`, true},
		{`SELECT U&'d\0061t', u&'x'`, `SELECT U&'?', u&'?'`, true},
		{`SELECT B'1010', b'1', X'1F', x'2', N'nat', n'b'`, `SELECT B'?', b'?', X'?', x'?', N'?', n'?'`, true},
		{`SELECT $$it's$$, 'x'`, `SELECT $$?$$, '?'`, true},
		{`SELECT $tag$a $$ b$tag$, $_t9$x$_t9$`, `SELECT $tag$?$tag$, $_t9$?$_t9$`, true},
		{`SELECT $é$secret$é$`, `SELECT $é$?$é$`, true},
		{`SELECT $1, $2::text FROM t WHERE a = $10`, `SELECT $1, $2::text FROM t WHERE a = $10`, true},
		{`SELECT a$b$c, x$$y FROM t`, `SELECT a$b$c, x$$y FROM t`, true},
		{`SELECT $1abc$ 'x'`, `SELECT $1abc$ '?'`, true},
		{`SELECT 'open literal AND more`, `SELECT '?`, true},
		{`SELECT E'open \' more`, `SELECT E'?`, true},
		{`SELECT $t$open body`, `SELECT $t$?`, true},
		{`SELECT "it's", "a""b" FROM "T" WHERE c = 'd'`, `SELECT "it's", "a""b" FROM "T" WHERE c = '?'`, true},
		{"SELECT 1 -- it's 'a comment'\nFROM t WHERE x='y'", "SELECT 1 -- it's 'a comment'\nFROM t WHERE x='?'", true},
		{`SELECT 1 --'x'`, `SELECT 1 --'x'`, true},
		{`SELECT /* a /* it's */ 'x' */ 'y'`, `SELECT /* a /* it's */ 'x' */ '?'`, true},
		{`SELECT /* unclosed /* 'x' */`, `SELECT /* unclosed /* 'x' */`, true},
		{`SELECT 42, 1.5e10, 0x1F, 'v'`, `SELECT 42, 1.5e10, 0x1F, '?'`, true},
		{`SELECT typeE'x', name'y'`, `SELECT typeE'?', name'?'`, true},
		{`SELECT '비밀 値', 'ok'`, `SELECT '?', '?'`, true},
		{`SELECT 'a$$b', $$c$$`, `SELECT '?', $$?$$`, true},
		{`SELECT 'a\'b', 'c'`, `SELECT '?', '?'`, false},
		{`SELECT N'a\'b', 'c'`, `SELECT N'?', '?'`, false},
		{`SELECT U&'a\' , 'c'`, `SELECT U&'?' , '?'`, false},
		{`SELECT E'a\'b'`, `SELECT E'?'`, false},
		{``, ``, true},
		{`'`, `'?`, true},
		{`$`, `$`, true},
		{`$$`, `$$?`, true},
		{`E'`, `E'?`, true},
	} {
		if got := string(postgresMaskSQL([]byte(tc.sql), tc.standard)); got != tc.want {
			t.Errorf("mask(%q, standard=%v) = %q, want %q", tc.sql, tc.standard, got, tc.want)
		}
	}
}

// PostgreSQL 기본값(standard_conforming_strings=on)에서는 '...' 안의 backslash가 escape가 아니다. MySQL 규칙으로 가리면
// 뒤 literal이 드러난다.
func TestPostgresMaskSQLDoesNotReuseMySQLSemantics(t *testing.T) {
	sql := []byte(`SELECT 'C:\' , 'edc-literal-secret'`)
	if got := string(mysqlMaskSQL(sql)); !strings.Contains(got, "edc-literal-secret") {
		t.Fatalf("mysqlMaskSQL = %q, the premise of this test changed", got)
	}
	if got := string(postgresMaskSQL(sql, true)); strings.Contains(got, "edc-literal-secret") || got != `SELECT '?' , '?'` {
		t.Fatalf("postgresMaskSQL = %q", got)
	}
}

func TestPostgresSQLTextMasksCapsAndEscapes(t *testing.T) {
	masked := newPostgresTracker("", false)
	shown := newPostgresTracker("", true)
	sql := []byte("SELECT 'edc-literal-secret'\x01\x1b[31m")
	if got, truncated := masked.sqlText(sql, false, true); got != `SELECT '?'\x01\x1b[31m` || truncated {
		t.Fatalf("masked = %q, %v", got, truncated)
	}
	if got, _ := shown.sqlText(sql, false, true); got != `SELECT 'edc-literal-secret'\x01\x1b[31m` {
		t.Fatalf("shown = %q", got)
	}
	long := []byte("SELECT " + strings.Repeat("é", postgresSQLLimit))
	got, truncated := shown.sqlText(long, false, true)
	if !truncated || len(got) > postgresSQLLimit || !strings.HasPrefix(got, "SELECT é") || strings.HasSuffix(got, "\\x") {
		t.Fatalf("long = %d bytes, truncated %v, tail %q", len(got), truncated, got[len(got)-8:])
	}
	if _, truncated := masked.sqlText([]byte("SELECT 1"), true, true); !truncated {
		t.Fatal("a cut message must stay truncated")
	}
}
