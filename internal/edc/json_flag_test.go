package edc

import (
	"io"
	"slices"
	"testing"
	"time"
)

// 값 없이 쓴 --json은 맨 끝이거나 다른 옵션 앞일 때만 stdout이다. 경로를 띄어 쓴 형식과 -- 뒤의 인자, grep의 불리언 --json은 그대로다.
func TestNormalizeJSONFlag(t *testing.T) {
	for _, test := range []struct{ in, want []string }{
		{[]string{"audit", "--json"}, []string{"audit", "--json", "-"}},
		{[]string{"audit", "--json", "-v"}, []string{"audit", "--json", "-", "-v"}},
		{[]string{"audit", "-json"}, []string{"audit", "-json", "-"}},
		{[]string{"audit", "--json", "--"}, []string{"audit", "--json", "-", "--"}},
		{[]string{"audit", "--json", "-"}, []string{"audit", "--json", "-"}},
		{[]string{"audit", "--json", "report.json"}, []string{"audit", "--json", "report.json"}},
		// -로 시작해도 .이나 /가 있으면 파일 이름이다. 옵션 이름은 영문자, 숫자, -만 쓴다.
		{[]string{"audit", "--json", "-report.json"}, []string{"audit", "--json", "-report.json"}},
		{[]string{"audit", "--json", "-out/report"}, []string{"audit", "--json", "-out/report"}},
		{[]string{"audit", "--json", "--since=1h"}, []string{"audit", "--json", "-", "--since=1h"}},
		{[]string{"audit", "--json=report.json", "-v"}, []string{"audit", "--json=report.json", "-v"}},
		{[]string{"tcp", "check", "--json", "out.json", "host:443"}, []string{"tcp", "check", "--json", "out.json", "host:443"}},
		{[]string{"tcp", "check", "host:443", "--json"}, []string{"tcp", "check", "host:443", "--json", "-"}},
		{[]string{"log", "--", "jq", "--json"}, []string{"log", "--", "jq", "--json"}},
		{[]string{"log", "--json", "--", "jq", "--json"}, []string{"log", "--json", "-", "--", "jq", "--json"}},
		{[]string{"grep", "--json", "-n", "pattern"}, []string{"grep", "--json", "-n", "pattern"}},
		{nil, nil},
	} {
		if got := normalizeJSONFlag(test.in); !slices.Equal(got, test.want) {
			t.Errorf("%q = %q, want %q", test.in, got, test.want)
		}
	}
}

// 값 없는 --json을 고친 결과가 공통 옵션의 해석에서 stdout(-)이 된다.
func TestBareJSONFlagParsesAsStdout(t *testing.T) {
	options := configuredCommon(time.Second)
	set := newFlagSet("audit")
	set.SetOutput(io.Discard)
	bindCommon(set, &options)
	if err := set.Parse(normalizeJSONFlag([]string{"audit", "--json", "-v"})[1:]); err != nil || options.jsonPath != "-" || !options.verbose || set.NArg() != 0 {
		t.Fatalf("json = %q, verbose = %v, args = %q, err = %v", options.jsonPath, options.verbose, set.Args(), err)
	}
}
