package edc

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// commandSuggestLimit은 한 번에 권하는 명령의 수다. 더 많으면 오타가 아니라 다른 낱말이다.
const commandSuggestLimit = 3

// printUnknownCommand는 없는 명령의 오류다. 첫 줄은 그대로 둔다. check --group과 find --group이 예전 edc의
// "unknown command: check"를 읽어 그 host를 가려내기 때문이다.
func printUnknownCommand(writer io.Writer, name string, hint bool) {
	fmt.Fprintln(writer, T("cli.error.unknown_command", name))
	if similar := suggestCommands(name); len(similar) > 0 {
		fmt.Fprintln(writer, T("cli.error.did_you_mean", strings.Join(similar, ", ")))
	}
	if hint {
		fmt.Fprintln(writer, T("cli.error.help_hint"))
	}
}

// suggestCommands는 잘못 친 이름과 가장 가까운 명령을 이름 순서로 돌려준다. 글자 하나를 빼거나 더하거나 바꾸거나,
// 이웃한 두 글자를 맞바꾼 것을 한 번의 오타로 센다. 일곱 글자부터는 오타 두 번까지 본다. 그보다 짧은 이름에 두 번을
// 허용하면 docker에 doctor를 권하는 것처럼 다른 낱말을 오타로 읽는다.
func suggestCommands(name string) []string {
	typed := []rune(strings.ToLower(name))
	if len(typed) < 3 {
		return nil
	}
	limit := 1
	if len(typed) >= 7 {
		limit = 2
	}
	best := limit + 1
	var similar []string
	for _, candidate := range append(commandNames(), "help") {
		distance := typoDistance(typed, []rune(candidate))
		if distance == 0 || distance > limit || distance > best {
			continue
		}
		if distance < best {
			best, similar = distance, nil
		}
		similar = append(similar, candidate)
	}
	if len(similar) > commandSuggestLimit {
		return nil
	}
	sort.Strings(similar)
	return similar
}

// typoDistance는 a를 b로 바꾸는 오타의 수다. 이웃한 두 글자를 맞바꾼 것도 하나로 센다.
func typoDistance(a, b []rune) int {
	previous2 := make([]int, len(b)+1)
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				current[j] = min(current[j], previous2[j-2]+1)
			}
		}
		previous2, previous, current = previous, current, previous2
	}
	return previous[len(b)]
}
