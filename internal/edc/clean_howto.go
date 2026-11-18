package edc

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// cleanHowTo는 보고서 끝의 정리 방법 한 줄 묶음이다. 명령이 없는 항목은 note만 있다.
type cleanHowTo struct {
	size    string
	bytes   uint64
	name    string
	command string
	note    string
}

// cleanRemoveCommand는 잰 경로를 지우는 명령이다. 묶음 후보의 roots는 부모 폴더가 아니라 잰 하위 경로라서
// /tmp나 ~/Library/Caches 전체를 가리키는 명령이 나오지 않는다.
func cleanRemoveCommand(roots []string, homes ...string) string {
	args := make([]string, 0, len(roots))
	for _, root := range roots {
		args = append(args, cleanShellPath(root, homes))
	}
	return "rm -rf " + strings.Join(args, " ")
}

// cleanShellPath는 홈 아래 경로를 ~/로 줄인다. 따옴표는 ~/ 뒤에만 붙여야 셸이 ~를 펼친다.
func cleanShellPath(path string, homes []string) string {
	for _, home := range homes {
		home = strings.TrimSuffix(home, "/")
		if home == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(path, home+"/"); ok {
			return "~/" + cleanShellQuote(rest)
		}
	}
	return cleanShellQuote(path)
}

func cleanHowToEntries(items []cleanItem, docker cleanDocker, worktrees cleanWorktrees) []cleanHowTo {
	var entries []cleanHowTo
	shown, _ := cleanShownItems(items, false)
	for _, item := range shown {
		if !item.counted() || *item.UsageBytes == 0 {
			continue
		}
		entries = append(entries, cleanHowTo{size: cleanItemSize(item), bytes: *item.UsageBytes,
			name: T("clean.candidate." + item.ID), command: item.Cleanup, note: T("clean.howto.note." + item.ID)})
	}
	for _, result := range docker.Types {
		if result.Status != cleanMeasured || result.Cleanup == "" || result.ReclaimableBytes == nil || *result.ReclaimableBytes <= 0 {
			continue
		}
		bytes := uint64(*result.ReclaimableBytes)
		entries = append(entries, cleanHowTo{size: formatSizeShort(bytes), bytes: bytes, name: T("clean.howto.docker." + result.Type),
			command: result.Cleanup, note: T("clean.howto.note.docker_" + result.Type)})
	}
	if worktrees.counted() && worktrees.Cleanup != "" {
		entry := cleanHowTo{size: cleanNoSize, name: T("clean.worktree.panel"), command: worktrees.Cleanup, note: T("clean.howto.note.worktrees")}
		// git-kit worktree cleanup이 기본으로 지우는 대상은 병합됐고 변경이 없는 worktree다.
		var merged uint64
		var known bool
		for _, item := range worktrees.Items {
			if item.Verdict == cleanVerdictMergedClean && item.UsageBytes != nil {
				merged += *item.UsageBytes
				known = true
			}
		}
		if known {
			entry.size, entry.bytes = formatSizeShort(merged), merged
		}
		entries = append(entries, entry)
	}
	return entries
}

// formatCleanHowTo는 보고서 끝에 정리 명령과 그 영향을 모은다. 명령 줄은 자르거나 접지 않는다.
// 잘린 명령은 실행할 수 없고, 접힌 명령을 복사하면 줄마다 따로 실행된다.
func formatCleanHowTo(entries []cleanHowTo, width int, style cleanStyle) string {
	if len(entries) == 0 {
		return ""
	}
	var sizeWidth, nameWidth int
	for _, entry := range entries {
		sizeWidth = max(sizeWidth, liveWidth(entry.size))
		nameWidth = max(nameWidth, liveWidth(entry.name))
	}
	// 줄 맨 앞 두 칸은 다른 섹션처럼 들여 쓰고, 명령과 설명은 이름 칸에서 시작한다.
	indent := strings.Repeat(" ", 2+sizeWidth+2)
	lead := 2 + sizeWidth + 2 + nameWidth + 2
	raw := func(text string) string {
		if style {
			return richPanelLine(text[1:], "") + "\n"
		}
		return text + "\n"
	}
	var builder strings.Builder
	builder.WriteString(style.open(T("clean.howto.title"), T("clean.howto.panel"), T("clean.howto.note_title"), width))
	for _, entry := range entries {
		head := "  " + style.size(cleanPadLeft(entry.size, sizeWidth), entry.bytes, true) + "  "
		command := style.text().paint(entry.command, richCyan)
		if entry.command != "" && (width <= 0 || lead+liveWidth(entry.command) <= width) {
			builder.WriteString(raw(head + cleanPad(entry.name, nameWidth) + "  " + command))
		} else {
			builder.WriteString(style.row("  ", strings.TrimPrefix(head, "  ")+entry.name, width))
			if entry.command != "" {
				builder.WriteString(raw(indent + command))
			}
		}
		room := width - len(indent) - 1
		note := entry.note
		if width > 0 && room >= 20 {
			note = ansi.Wrap(note, room, "")
		}
		for _, line := range strings.Split(note, "\n") {
			builder.WriteString(style.row(indent, style.dim(line), width))
		}
	}
	builder.WriteString(style.close())
	return builder.String()
}
