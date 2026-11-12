package edc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	// findPreviewBytes는 미리보기로 읽는 파일 앞부분의 크기다. 큰 로그도 화면 한 쪽을 채우기에 충분하다.
	findPreviewBytes = 64 << 10
	// findPreviewEntries는 디렉터리 미리보기에 이름을 보이는 항목 수다.
	findPreviewEntries = 200
	// findPreviewSideWidth는 미리보기를 목록 오른쪽에 두는 최소 화면 너비다. 더 좁으면 목록 아래에 둔다.
	findPreviewSideWidth = 120
)

type findPreview struct {
	path   string
	header string
	lines  []string
	note   string
}

type findPreviewMsg struct {
	generation int
	preview    findPreview
}

// loadFindPreview는 path의 미리보기를 만든다. 일반 파일만 연다. FIFO는 열면 쓰는 쪽이 올 때까지 막히고, 장치 파일은 읽는 것만으로
// 상태가 바뀔 수 있다.
func loadFindPreview(path string, now time.Time) findPreview {
	preview := findPreview{path: path}
	info, err := os.Lstat(path)
	if err != nil {
		preview.note = T("find.preview.error", findPreviewError(err))
		return preview
	}
	preview.header = fmt.Sprintf("%s  %s  %s", info.Mode().String(), findPreviewSize(info), findAge(now.Sub(info.ModTime())))
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			preview.note = T("find.preview.error", findPreviewError(err))
			return preview
		}
		if _, err := os.Stat(path); err != nil {
			preview.note = T("find.preview.link_missing", viewSanitize(target))
		} else {
			preview.note = T("find.preview.link", viewSanitize(target))
		}
	case mode.IsDir():
		preview.lines, preview.note = findPreviewDir(path)
	case mode.IsRegular():
		preview.lines, preview.note = findPreviewFile(path, info.Size())
	default:
		preview.note = T("find.preview.special", findPreviewKind(mode))
	}
	return preview
}

func findPreviewSize(info fs.FileInfo) string {
	if !info.Mode().IsRegular() {
		return "-"
	}
	return formatSizeShort(uint64(max(0, int(info.Size()))))
}

func findPreviewError(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

func findPreviewKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeNamedPipe != 0:
		return "FIFO"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	}
	return mode.Type().String()
}

// findPreviewOpen은 미리보기용으로 연다. Lstat 뒤에 경로가 FIFO로 바뀌어도 막히지 않도록 O_NONBLOCK으로 열고, 연 것을 다시 확인한다.
// symlink는 따라가지 않는다.
func findPreviewOpen(path string, directory bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
	if directory {
		flags |= syscall.O_DIRECTORY
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (directory && !info.IsDir() || !directory && !info.Mode().IsRegular()) {
		err = errors.New(T("find.preview.special", findPreviewKind(info.Mode())))
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func findPreviewFile(path string, size int64) ([]string, string) {
	file, err := findPreviewOpen(path, false)
	if err != nil {
		return nil, T("find.preview.error", findPreviewError(err))
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, findPreviewBytes))
	if err != nil {
		return nil, T("find.preview.error", findPreviewError(err))
	}
	if len(data) == 0 {
		return nil, T("find.preview.empty")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, T("find.preview.binary", formatSizeShort(uint64(max(0, int(size)))))
	}
	text := string(data)
	note := ""
	if size > int64(len(data)) {
		note = T("find.preview.truncated", formatSizeShort(findPreviewBytes))
		// 잘린 마지막 줄은 버린다.
		if cut := strings.LastIndexByte(text, '\n'); cut > 0 {
			text = text[:cut]
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for index, line := range lines {
		lines[index] = viewSanitize(line)
	}
	return lines, note
}

// findPreviewDir는 이름순으로 앞 findPreviewEntries개만 남기며 목록을 묶음으로 읽는다. 전부 메모리에 올리면 큰 디렉터리에서 멈춘다.
func findPreviewDir(path string) ([]string, string) {
	file, err := findPreviewOpen(path, true)
	if err != nil {
		return nil, T("find.preview.error", findPreviewError(err))
	}
	defer file.Close()
	var kept []fs.DirEntry
	total := 0
	for {
		batch, err := file.ReadDir(findReadBatch)
		total += len(batch)
		kept = append(kept, batch...)
		slices.SortFunc(kept, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		kept = kept[:min(len(kept), findPreviewEntries)]
		if err != nil {
			if !errors.Is(err, io.EOF) && total == 0 {
				return nil, T("find.preview.error", findPreviewError(err))
			}
			break
		}
	}
	var lines []string
	for _, entry := range kept {
		name := viewSanitize(entry.Name())
		switch {
		case entry.IsDir():
			name += "/"
		case entry.Type()&fs.ModeSymlink != 0:
			name += "@"
		}
		lines = append(lines, name)
	}
	note := ""
	if total == 0 {
		note = T("find.preview.empty_dir")
	} else if extra := total - findPreviewEntries; extra > 0 {
		note = T("find.preview.more", extra)
	}
	return lines, note
}

// previewCmd는 미리보기가 켜져 있고 고른 경로가 바뀌었으면 읽기를 시작한다. 늦게 끝난 읽기는 세대 번호로 버린다.
func (model *findModel) previewCmd() tea.Cmd {
	if !model.previewOn {
		return nil
	}
	hit, ok := model.selected()
	if !ok {
		model.preview = findPreview{}
		return nil
	}
	path := model.paths[hit]
	if path == model.previewPath {
		return nil
	}
	model.previewPath = path
	model.previewGeneration++
	model.preview = findPreview{path: path, note: T("find.preview.loading")}
	generation, now := model.previewGeneration, model.config.now()
	return func() tea.Msg { return findPreviewMsg{generation: generation, preview: loadFindPreview(path, now)} }
}

// previewLayout은 미리보기 위치다. side면 오른쪽 열의 너비, 아니면 아래 줄 수를 돌려준다.
func (model findModel) previewLayout() (side bool, size int) {
	if !model.previewOn {
		return false, 0
	}
	if model.width >= findPreviewSideWidth {
		return true, model.width * 45 / 100
	}
	return false, max(3, (model.height-3)*2/5)
}

// previewLines는 미리보기 칸을 width×height로 채운다.
func (model findModel) previewLines(width, height int) []string {
	preview := model.preview
	var lines []string
	if preview.header != "" {
		lines = append(lines, model.paint(preview.header, richGray, false))
	}
	for _, line := range preview.lines {
		if len(lines) >= height-1 && preview.note != "" || len(lines) >= height {
			break
		}
		lines = append(lines, line)
	}
	if preview.note != "" && len(lines) < height {
		lines = append(lines, model.paint(preview.note, richYellow, false))
	}
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, max(1, width), "…")
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines[:height]
}
