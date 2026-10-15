package edc

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// promptMissingArgs는 빠진 필수 위치 인자를 terminal에서 받아 온다. 지금까지는 인자가 빠지면
// usage만 내고 끝나서 명령을 처음부터 다시 쳐야 했다. 묻는 자리는 그 오류 자리뿐이라,
// 인자를 제대로 준 실행의 흐름은 건드리지 않는다.
//
// terminal이 아니면 아무것도 묻지 않고 false를 돌려준다. caller가 지금과 같은 usage 오류와
// exit code를 유지해야 script의 계약이 깨지지 않는다. option과 flag는 묻지 않는다.
func promptMissingArgs(command string, labels ...string) ([]string, bool) {
	file, ok := terminalInput(os.Stdin)
	if !ok {
		return nil, false
	}
	return promptArgs(bufio.NewReader(file), os.Stdout, command, labels)
}

// promptArgs는 실제 질문이다. terminal 판정과 떼어 두어 검사할 수 있다.
func promptArgs(reader *bufio.Reader, output io.Writer, command string, labels []string) ([]string, bool) {
	return promptArgsWithColor(reader, output, command, labels, promptColor(output))
}

func promptArgsWithColor(reader *bufio.Reader, output io.Writer, command string, labels []string, color bool) ([]string, bool) {
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		// label은 묻는 줄에만 쓴다. 빈 값의 오류 문장은 여기서 버리므로 색이 섞이지 않는다.
		if color {
			label = richBold(label)
		}
		value, err := promptRemoteText(reader, output, label, "")
		// 빈 값과 EOF는 취소다. caller가 usage를 내고 지금과 같은 exit code로 끝낸다.
		if err != nil {
			return nil, false
		}
		values = append(values, value)
	}
	writePromptEcho(output, command+" "+strings.Join(values, " "), color)
	return values, true
}

// promptMissingChoice는 하위 command가 빠졌을 때 terminal에서 고르게 한다. 하위 command도
// 명령에 꼭 필요한 값이라, 값을 제대로 준 실행에는 나타나지 않는다.
func promptMissingChoice(command string, choices []string) (string, bool) {
	file, ok := terminalInput(os.Stdin)
	// 선택 화면은 stdout에 그린다. stdout이 파이프나 파일이면 화면이 그쪽으로 가고 입력만 기다리게 된다.
	if !ok || !isTerminal(os.Stdout) {
		return "", false
	}
	// 고를 것이 하나뿐이면 고르는 화면은 의식일 뿐이다. 무엇이 실행되는지는 echo가 알려 준다.
	if len(choices) == 1 {
		promptEcho(os.Stdout, command+" "+choices[0])
		return choices[0], true
	}
	model := newSelectModel("cli.prompt.subcommand_title", "cli.prompt.subcommand_label", subcommandSelectItems(strings.TrimPrefix(command, "edc "), choices))
	value, err := runSelect(file, os.Stdout, model)
	if err != nil {
		return "", false
	}
	promptEcho(os.Stdout, command+" "+value)
	return value, true
}

// promptStatePath는 --state가 빠졌을 때 남아 있는 상태 파일에서 고르게 한다. run id가 박힌
// 경로를 옮겨 적는 대신 고른다. 되돌려야 하는 순간은 대개 급하다.
func promptStatePath(command string, paths []string) (string, bool) {
	if len(paths) == 0 {
		return "", false
	}
	file, ok := terminalInput(os.Stdin)
	if !ok {
		return "", false
	}
	model := newSelectModel("cli.prompt.state_title", "cli.prompt.state_label", selectItemsFromValues(paths))
	value, err := runSelect(file, os.Stdout, model)
	if err != nil {
		return "", false
	}
	promptEcho(os.Stdout, command+" --state "+value)
	return value, true
}

// promptEcho는 받은 값을 채운 명령을 되돌려 준다. 다음부터는 묻지 않고 바로 칠 수 있고,
// 그대로 script에 옮길 수도 있다.
func promptEcho(output io.Writer, command string) {
	writePromptEcho(output, command, promptColor(output))
}

// writePromptEcho는 색을 쓰면 화살표를 회색으로, 그대로 칠 수 있는 명령을 굵은 청록으로 쓴다.
func writePromptEcho(output io.Writer, command string, color bool) {
	if color {
		fmt.Fprintf(output, "%s %s\n\n", richDim("→"), richPaint(command, richCyan, true))
		return
	}
	fmt.Fprintf(output, "→ %s\n\n", command)
}

// promptColor는 묻는 줄과 echo에 색을 써도 되는지다. 색을 쓰는 터미널 파일일 때만 칠한다.
func promptColor(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && richEnabled(file)
}
