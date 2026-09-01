package edc

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

func selectRemoteGroup(input *os.File, output io.Writer, groups []string) (string, error) {
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return "", err
	}
	defer term.Restore(int(input.Fd()), state)
	selected := 0
	fmt.Fprintln(output, "group(대상)을 선택하세요. ↑/↓ 이동, Enter 선택")
	renderRemoteGroups(output, groups, selected, false, false)
	buffer := make([]byte, 3)
	for {
		count, err := input.Read(buffer[:1])
		if err != nil {
			return "", err
		}
		if count == 0 {
			continue
		}
		switch buffer[0] {
		case '\r', '\n':
			renderRemoteGroups(output, groups, selected, true, true)
			return groups[selected], nil
		case 3:
			return "", errRemoteCancelled
		case 27:
			if _, err := io.ReadFull(input, buffer[1:3]); err != nil {
				return "", err
			}
			if buffer[1] != '[' {
				continue
			}
			switch buffer[2] {
			case 'A':
				selected = (selected - 1 + len(groups)) % len(groups)
			case 'B':
				selected = (selected + 1) % len(groups)
			default:
				continue
			}
			renderRemoteGroups(output, groups, selected, false, true)
		}
	}
}

func renderRemoteGroups(output io.Writer, groups []string, selected int, final, moveUp bool) {
	if final {
		if moveUp && len(groups) > 0 {
			fmt.Fprintf(output, "\033[%dA", len(groups))
		}
		fmt.Fprintf(output, "\r\033[2Kgroup(대상): %s\r\n", groups[selected])
		for index := 1; index < len(groups); index++ {
			fmt.Fprint(output, "\r\033[2K\r\n")
		}
		return
	}
	if moveUp && len(groups) > 0 {
		fmt.Fprintf(output, "\033[%dA", len(groups))
	}
	for index, group := range groups {
		marker := "  "
		if index == selected {
			marker = "› "
		}
		fmt.Fprintf(output, "\r\033[2K%s%s\r\n", marker, group)
	}
}
