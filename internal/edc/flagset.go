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
// 원래 오류는 감싸서 남긴다.
var errFlagReported = errors.New("flag error already reported")

func flagReported(err error) error {
	return fmt.Errorf("%w: %w", errFlagReported, err)
}

// helpRequested는 args에 -h, --h, -help, --help가 있는지 본다. 대시 개수는 reorderFSWatchArgs처럼 따지지 않는다.
// -- 뒤는 감싼 명령의 인자라서 보지 않는다.
func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			switch strings.TrimLeft(arg, "-") {
			case "h", "help":
				return true
			}
		}
	}
	return false
}

// parseReordered는 `edc svc nginx -v`처럼 위치 인자 뒤에 온 flag도 받는다. flag package는 첫 위치 인자에서 멈춘다.
// help 요청이면 flag.ErrHelp를, 그 밖의 오류면 이미 출력한 오류를 돌려준다.
func parseReordered(set *flag.FlagSet, args []string) error {
	ordered, err := reorderFSWatchArgs(set, args)
	if errors.Is(err, flag.ErrHelp) {
		set.Usage()
		return err
	}
	if err != nil {
		fmt.Fprintln(set.Output(), err)
		set.Usage()
		return flagReported(err)
	}
	if err := set.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return flagReported(err)
	}
	return nil
}
