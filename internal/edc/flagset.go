package edc

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// newFlagSet은 edc의 flag 집합이다. 잘못된 flag를 주면 flag package가 오류 한 줄을 먼저 쓰고 Usage를 부른다.
// 기본 Usage는 "-json string"처럼 대시 하나짜리 flag 목록을 쏟아 내서, edc help 안내 한 줄로 바꾼다.
func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	command, _, _ := strings.Cut(name, " ")
	set.Usage = func() { fmt.Fprintln(set.Output(), T("cli.error.flag_help", "edc help "+command)) }
	return set
}

// errFlagReported는 flag package가 이미 오류를 출력한 parse 실패다. 받은 쪽은 다시 출력하지 않는다.
var errFlagReported = errors.New("flag error already reported")

// helpRequested는 args에 -h, -help, --help가 있는지 본다. -- 뒤는 감싼 명령의 인자라서 보지 않는다.
func helpRequested(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}
