package edc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestLoadFindPreviewDoesNotOpenSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan findPreview, 1)
	go func() { done <- loadFindPreview(fifo, time.Now()) }()
	select {
	case preview := <-done:
		// FIFO를 열면 쓰는 쪽이 올 때까지 막힌다.
		if !strings.Contains(preview.note, "FIFO") || preview.lines != nil {
			t.Errorf("preview = %+v", preview)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the preview opened a FIFO")
	}
}

func TestLoadFindPreviewReadsTheStartOfAFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	var builder strings.Builder
	for line := 0; builder.Len() < 2*findPreviewBytes; line++ {
		fmt.Fprintf(&builder, "line %d\tok\n", line)
	}
	if err := os.WriteFile(big, []byte(builder.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	preview := loadFindPreview(big, time.Now())
	if preview.lines[0] != "line 0  ok" || preview.note != T("find.preview.truncated", formatSizeShort(findPreviewBytes)) {
		t.Errorf("first = %q, note = %q", preview.lines[0], preview.note)
	}
	for _, line := range preview.lines {
		if !strings.HasPrefix(line, "line ") || !strings.HasSuffix(line, "ok") {
			t.Fatalf("a cut line must not be shown: %q", line)
		}
	}
	// 앞 64 KiB 안의 완전한 줄만 보인다.
	if want := strings.Count(builder.String()[:findPreviewBytes], "\n"); len(preview.lines) != want {
		t.Errorf("lines %d, want %d", len(preview.lines), want)
	}
	binary := filepath.Join(dir, "tool")
	if err := os.WriteFile(binary, []byte("ELF\x00\x01\x02"), 0o755); err != nil {
		t.Fatal(err)
	}
	if preview := loadFindPreview(binary, time.Now()); preview.lines != nil || !strings.Contains(preview.note, "6 B") {
		t.Errorf("binary = %+v", preview)
	}
	control := filepath.Join(dir, "esc.txt")
	if err := os.WriteFile(control, []byte("a\x1b[31mred\x07\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if preview := loadFindPreview(control, time.Now()); strings.ContainsAny(preview.lines[0], "\x1b\x07") {
		t.Errorf("control = %q", preview.lines)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if preview := loadFindPreview(empty, time.Now()); preview.note != T("find.preview.empty") {
		t.Errorf("empty = %+v", preview)
	}
}

func TestLoadFindPreviewListsADirectoryAndALink(t *testing.T) {
	dir := t.TempDir()
	for index := range findPreviewEntries + 5 {
		writeFindTree(t, dir, fmt.Sprintf("many/f%03d", index))
	}
	writeFindTree(t, dir, "many/sub/")
	preview := loadFindPreview(filepath.Join(dir, "many"), time.Now())
	if len(preview.lines) != findPreviewEntries || preview.note != T("find.preview.more", 6) || preview.lines[0] != "f000" {
		t.Errorf("lines %d, note %q", len(preview.lines), preview.note)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if preview := loadFindPreview(filepath.Join(dir, "dangling"), time.Now()); preview.note != T("find.preview.link_missing", "/nonexistent/target") {
		t.Errorf("link = %+v", preview)
	}
	if preview := loadFindPreview(filepath.Join(dir, "missing"), time.Now()); !strings.Contains(preview.note, "no such file") {
		t.Errorf("missing = %+v", preview)
	}
}

func TestFindModelPreviewFollowsTheSelection(t *testing.T) {
	dir := t.TempDir()
	writeFindTree(t, dir, "a.txt", "b.txt")
	model := testFindModel(false)
	model, _ = findUpdate(t, model, tea.WindowSizeMsg{Width: 130, Height: 10})
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry(filepath.Join(dir, "a.txt"), findKindFile, 0), findTestEntry(filepath.Join(dir, "b.txt"), findKindFile, time.Hour)}})
	model, load := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	if !model.previewOn || load == nil {
		t.Fatal("tab must turn on the preview and read the selection")
	}
	first := findRunBatch(load)
	model, move := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyDown})
	second := findRunBatch(move)
	// 늦게 끝난 앞 선택의 미리보기는 버린다.
	model, _ = findUpdate(t, model, second)
	model, _ = findUpdate(t, model, first)
	if model.preview.path != filepath.Join(dir, "b.txt") || model.preview.lines[0] != "b.txt" {
		t.Errorf("preview = %+v", model.preview)
	}
	for _, line := range strings.Split(model.View().Content, "\n") {
		if width := ansi.StringWidth(line); width > 130 {
			t.Errorf("width %d: %q", width, line)
		}
	}
	// 좁은 화면에서는 목록 아래에 둔다.
	model, _ = findUpdate(t, model, tea.WindowSizeMsg{Width: 70, Height: 20})
	lines := strings.Split(ansi.Strip(model.View().Content), "\n")
	if len(lines) != 20 || !strings.Contains(strings.Join(lines, "\n"), "────") || model.rows() >= 17 {
		t.Errorf("rows %d, view:\n%s", model.rows(), strings.Join(lines, "\n"))
	}
	model, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	if model.previewOn || cmd != nil || model.rows() != 17 {
		t.Errorf("tab must turn the preview off: rows %d", model.rows())
	}
}

// findRunBatch는 tea.Batch에서 미리보기 메시지를 꺼낸다.
func findRunBatch(cmd tea.Cmd) tea.Msg {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, inner := range batch {
			if inner == nil {
				continue
			}
			if preview, ok := inner().(findPreviewMsg); ok {
				return preview
			}
		}
	}
	return msg
}
