package edc

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

// 잘못된 flag는 오류 한 줄과 edc help 안내만 낸다. Go 기본 usage의 "-json string" 같은 목록은 나오지 않는다.
func TestNewFlagSetPointsAtEdcHelp(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	var output bytes.Buffer
	set := newFlagSet("tcp check")
	set.SetOutput(&output)
	set.String("json", "", "JSON output path")
	if err := set.Parse([]string{"--nope"}); err == nil {
		t.Fatal("an unknown flag must fail")
	}
	want := "flag provided but not defined: -nope\nrun edc help tcp to see the options\n"
	if output.String() != want {
		t.Errorf("output = %q, want %q", output.String(), want)
	}
}

func TestHelpRequestedStopsAtTheCommandSeparator(t *testing.T) {
	for args, want := range map[string]bool{
		"lookup --help":         true,
		"lookup example.com -h": true,
		"-help":                 true,
		"--h":                   true,
		"---help":               true,
		"-":                     false,
		"-- ls --help":          false,
		"history --failed":      false,
		"":                      false,
	} {
		if got := helpRequested(strings.Fields(args)); got != want {
			t.Errorf("helpRequested(%q) = %t, want %t", args, got, want)
		}
	}
}

// log history는 flag package가 이미 출력한 오류를 다시 출력하지 않는다.
func TestLogHistoryPrintsAFlagErrorOnce(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	var stdout, stderr strings.Builder
	code := runLogWithStreams([]string{"history", "--nope"}, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 2 || strings.Count(stderr.String(), "flag provided but not defined") != 1 || !strings.Contains(stderr.String(), "edc help log") {
		t.Errorf("code = %d, stderr = %q", code, stderr.String())
	}
}

// 위치 인자 뒤의 flag를 받는 명령도 다른 명령과 같은 문장으로 오류를 한 번만 낸다. 원래 오류는 감싸서 남는다.
func TestParseReorderedReportsOnceLikeTheFlagPackage(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"nginx", "--bogus"}, "flag provided but not defined: -bogus\nrun edc help svc to see the options\n"},
		{[]string{"nginx", "--since"}, "flag needs an argument: -since\nrun edc help svc to see the options\n"},
	} {
		var output bytes.Buffer
		set := newFlagSet("svc")
		set.SetOutput(&output)
		set.String("since", "", "since")
		err := parseReordered(set, test.args)
		if !errors.Is(err, errFlagReported) || !strings.Contains(err.Error(), strings.SplitN(test.want, "\n", 2)[0]) {
			t.Errorf("%v: err = %v", test.args, err)
		}
		if output.String() != test.want {
			t.Errorf("%v: output = %q, want %q", test.args, output.String(), test.want)
		}
	}
	set := newFlagSet("svc")
	set.SetOutput(&bytes.Buffer{})
	if err := parseReordered(set, []string{"nginx", "--h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--h: err = %v, want flag.ErrHelp", err)
	}
}
