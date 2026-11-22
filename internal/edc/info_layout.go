package edc

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type infoStyle struct{ textStyle }

func newInfoStyle(color bool) infoStyle {
	style := textStyle{color: color}
	if color || isTerminal(os.Stdout) {
		style.width = terminalWidth()
	}
	return infoStyle{style}
}

func (style infoStyle) line(writer io.Writer, text string) {
	style.write(writer, "", "    ", text)
}

func (style infoStyle) tree(writer io.Writer, branch, text string) {
	indent := "│   "
	if branch == "└──" {
		indent = "    "
	}
	style.write(writer, branch+" ", indent, text)
}

func (style infoStyle) write(writer io.Writer, prefix, indent, text string) {
	width := style.width
	if width <= 0 {
		fmt.Fprintln(writer, style.dim(strings.TrimSuffix(prefix, " "))+strings.Repeat(" ", len(prefix)-len(strings.TrimSuffix(prefix, " ")))+text)
		return
	}
	available := max(1, width-max(liveWidth(prefix), liveWidth(indent)))
	parts := strings.Split(text, style.dim(" · "))
	var rows []string
	current := ""
	for _, part := range parts {
		if current == "" {
			current = part
			continue
		}
		next := current + style.dim(" · ") + part
		if liveWidth(next) <= available {
			current = next
		} else {
			rows = append(rows, current)
			current = part
		}
	}
	rows = append(rows, current)
	first := true
	for _, row := range rows {
		wrapped := ansi.Hardwrap(ansi.Wrap(row, available, ""), available, true)
		for _, line := range strings.Split(wrapped, "\n") {
			lead := indent
			if first {
				lead = prefix
				first = false
			}
			fmt.Fprintln(writer, style.dim(strings.TrimSuffix(lead, " "))+strings.Repeat(" ", len(lead)-len(strings.TrimSuffix(lead, " ")))+line)
		}
	}
}

func infoMountLabel(path string, width int) string {
	if width <= 0 || liveWidth(path) <= width {
		return path
	}
	tail := filepath.Base(path)
	if liveWidth("/…/"+tail) <= width {
		return ansi.Truncate(path, width-liveWidth(tail)-2, "…") + "/" + tail
	}
	return ansi.Truncate(path, width, "…")
}

func infoInterfaceCIDR(iface interfaceDetails) string {
	if mask := net.ParseIP(iface.Mask).To4(); mask != nil {
		if ones, bits := net.IPMask(mask).Size(); bits == 32 {
			return fmt.Sprintf("%s/%d", iface.Address, ones)
		}
	}
	return iface.Address + " / " + iface.Mask
}
