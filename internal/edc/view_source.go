package edc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// viewBlockSize는 파일 끝에서 거꾸로 읽는 단위다.
const viewBlockSize = 64 << 10

// viewPollInterval은 fsnotify 알림이 오지 않을 때 파일을 다시 확인하는 간격이다. NFS처럼 알림이 없는 파일 시스템과,
// 감시를 걸지 못한 경우에도 따라가게 한다.
const viewPollInterval = time.Second

// viewTailOffset은 file의 마지막 lines줄이 시작하는 offset이다. 끝에서 블록 단위로 거꾸로 읽어서, 파일 크기와 상관없이
// 읽는 양이 마지막 lines줄 정도다. 마지막 줄이 줄바꿈으로 끝나지 않아도 한 줄로 센다.
func viewTailOffset(file io.ReaderAt, size int64, lines int) (int64, error) {
	if lines <= 0 || size == 0 {
		return size, nil
	}
	block := make([]byte, viewBlockSize)
	end := size
	// 마지막 바이트가 줄바꿈이면 그 줄바꿈은 마지막 줄의 끝이라 세지 않는다.
	last := make([]byte, 1)
	if _, err := file.ReadAt(last, size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		end--
	}
	found := 0
	for end > 0 {
		start := end - viewBlockSize
		if start < 0 {
			start = 0
		}
		chunk := block[:end-start]
		count, err := file.ReadAt(chunk, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		chunk = chunk[:count]
		for index := len(chunk) - 1; index >= 0; index-- {
			if chunk[index] != '\n' {
				continue
			}
			found++
			if found == lines {
				return start + int64(index) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

// viewTailLineLimit는 pipe의 -n이 한 줄에서 보관하는 최대 바이트다. 줄바꿈 없는 입력이 끝없이 와도 메모리가 이 안에서
// 멈춘다. 넘친 줄은 앞부분만 남는다. 변수인 것은 테스트가 작은 값으로 바꾸기 위해서다.
var viewTailLineLimit = viewScreenReadLimit

// viewTailReader는 stdin처럼 되감을 수 없는 입력의 마지막 lines줄이다. 줄마다 보관하지 않고 원문 바이트를 그대로 둔다.
// viewTailLineLimit보다 긴 줄은 앞부분만 남기고, 그런 줄의 수를 함께 돌려준다.
func viewTailReader(reader io.Reader, lines int) ([][]byte, int, error) {
	if lines <= 0 {
		_, err := io.Copy(io.Discard, reader)
		return nil, 0, err
	}
	// 줄마다 ring을 당기면 줄 수 × lines만큼 복사해서, 다 차면 가장 오래된 자리에 덮어쓴다.
	ring := make([][]byte, 0, min(lines, 1024))
	next, truncated := 0, 0
	buffered := bufio.NewReaderSize(reader, viewBlockSize)
	for {
		line, cut, err := viewReadLine(buffered, viewTailLineLimit)
		if cut {
			truncated++
		}
		if len(line) != 0 {
			if len(ring) < lines {
				ring = append(ring, line)
			} else {
				ring[next] = line
				next = (next + 1) % lines
			}
		}
		if err != nil {
			ring = append(ring[next:], ring[:next]...)
			if errors.Is(err, io.EOF) {
				return ring, truncated, nil
			}
			return ring, truncated, err
		}
	}
}

// viewReadLine은 줄바꿈까지 한 줄을 읽는다. limit를 넘는 바이트는 버리고 cut을 true로 돌려주되, 줄바꿈까지는 계속 읽어
// 다음 줄의 시작을 잃지 않는다.
func viewReadLine(reader *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	cut := false
	for {
		chunk, err := reader.ReadSlice('\n')
		switch {
		case len(line)+len(chunk) <= limit:
			line = append(line, chunk...)
		case len(line) < limit:
			line = append(line, chunk[:limit-len(line)]...)
			cut = true
		default:
			cut = true
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if cut && err == nil {
			// 잘린 줄도 줄바꿈으로 끝나야 다음 줄과 붙지 않는다.
			line = append(line, '\n')
		}
		return line, cut, err
	}
}

// viewFollowEvent는 따라가는 동안 생긴 일이다. data는 새로 쓰인 바이트이고, reset은 파일이 바뀐 까닭이다.
type viewFollowEvent struct {
	data  []byte
	reset string
	err   error
}

const (
	viewResetReplaced  = "replaced"
	viewResetTruncated = "truncated"
)

// followViewFile은 file의 offset부터 새로 쓰인 바이트를 emit에 넘긴다. logrotate처럼 이름이 다른 파일로 바뀌면
// 옛 파일의 나머지를 다 읽은 뒤 새 파일을 처음부터 읽고, 크기가 줄면(truncate) 처음부터 다시 읽는다. tail -F와 같다.
// emit이 false를 돌려주거나 ctx가 끝나면 멈춘다. file은 여기서 닫는다.
func followViewFile(ctx context.Context, path string, file *os.File, offset int64, emit func(viewFollowEvent) bool) {
	defer func() { file.Close() }()
	wake := make(chan struct{}, 1)
	if watcher, err := fsnotify.NewWatcher(); err == nil {
		defer watcher.Close()
		// 파일 이름이 바뀌어도 알림을 받도록 파일이 아니라 디렉터리를 감시한다.
		if watcher.Add(filepath.Dir(path)) == nil {
			go func() {
				for {
					select {
					case event, ok := <-watcher.Events:
						if !ok {
							return
						}
						if filepath.Clean(event.Name) == filepath.Clean(path) {
							select {
							case wake <- struct{}{}:
							default:
							}
						}
					case _, ok := <-watcher.Errors:
						if !ok {
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}()
		}
	}
	ticker := time.NewTicker(viewPollInterval)
	defer ticker.Stop()
	buffer := make([]byte, viewBlockSize)
	// tail은 offset 바로 앞의 마지막 바이트다. copytruncate 뒤 파일이 옛 offset보다 크게 다시 쓰이면 크기로는 알 수 없으므로,
	// 읽기 전에 그 자리가 그대로인지 비교해 다시 쓰인 파일을 알아챈다.
	tail := viewReadTail(file, offset)
	for {
		if offset > 0 && len(tail) != 0 && !viewTailIntact(file, offset, tail) {
			offset, tail = 0, nil
			if !emit(viewFollowEvent{reset: viewResetTruncated}) {
				return
			}
		}
		for {
			count, err := file.ReadAt(buffer, offset)
			if count > 0 {
				offset += int64(count)
				tail = viewKeepTail(tail, buffer[:count])
				if !emit(viewFollowEvent{data: bytes.Clone(buffer[:count])}) {
					return
				}
			}
			if err != nil && !errors.Is(err, io.EOF) {
				emit(viewFollowEvent{err: err})
				return
			}
			if count < len(buffer) {
				break
			}
		}
		current, err := os.Stat(path)
		opened, openedErr := file.Stat()
		switch {
		case err == nil && openedErr == nil && !os.SameFile(current, opened):
			next, openErr := os.Open(path)
			if openErr != nil {
				break
			}
			file.Close()
			file, offset, tail = next, 0, nil
			if !emit(viewFollowEvent{reset: viewResetReplaced}) {
				return
			}
			continue
		case openedErr == nil && opened.Size() < offset:
			offset, tail = 0, nil
			if !emit(viewFollowEvent{reset: viewResetTruncated}) {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// viewTailCheck는 다시 쓰인 파일을 알아채기 위해 offset 앞에서 기억해 두는 바이트 수다.
const viewTailCheck = 4096

// viewReadTail은 offset 앞의 마지막 viewTailCheck 바이트다. 따라가기를 시작할 때 한 번 읽는다.
func viewReadTail(file io.ReaderAt, offset int64) []byte {
	if offset <= 0 {
		return nil
	}
	start := offset - viewTailCheck
	if start < 0 {
		start = 0
	}
	tail := make([]byte, offset-start)
	count, err := file.ReadAt(tail, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return tail[:count]
}

// viewKeepTail은 새로 읽은 data를 붙인 뒤 마지막 viewTailCheck 바이트만 남긴다.
func viewKeepTail(tail, data []byte) []byte {
	tail = append(tail, data...)
	if len(tail) > viewTailCheck {
		tail = append([]byte(nil), tail[len(tail)-viewTailCheck:]...)
	}
	return tail
}

// viewTailIntact는 offset 앞의 바이트가 기억해 둔 tail과 같은지다. 다르면 같은 inode가 다시 쓰인 것이다.
func viewTailIntact(file io.ReaderAt, offset int64, tail []byte) bool {
	check := make([]byte, len(tail))
	count, err := file.ReadAt(check, offset-int64(len(tail)))
	if err != nil && !errors.Is(err, io.EOF) {
		return true
	}
	return count == len(tail) && bytes.Equal(check, tail)
}

// viewSplitLines는 따라가며 받은 바이트를 줄로 나눈다. 줄바꿈으로 끝나지 않은 나머지는 pending으로 돌려주어 다음 바이트와 잇는다.
func viewSplitLines(pending, data []byte) ([]string, []byte) {
	data = append(pending, data...)
	var lines []string
	for {
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			break
		}
		lines = append(lines, string(bytes.TrimSuffix(data[:index], []byte("\r"))))
		data = data[index+1:]
	}
	return lines, bytes.Clone(data)
}

const (
	// viewScreenReadLimit는 화면을 열 때 파일 끝에서 읽는 최대 바이트다. 줄바꿈이 거의 없는 큰 파일(minified JSON, binary)에서
	// 마지막 줄들의 시작이 파일 처음까지 가도 파일 전체를 메모리에 올리지 않는다.
	viewScreenReadLimit = 64 << 20
	// viewStoreLimit는 화면이 한 줄에서 보관하는 최대 바이트다. 이보다 긴 줄은 앞부분만 보관한다.
	viewStoreLimit = 64 << 10
	// viewPendingLimit는 줄바꿈을 기다리며 모아 두는 최대 바이트다. 넘으면 그때까지를 한 줄로 넣는다.
	viewPendingLimit = 1 << 20
)

// viewReadInitial은 화면을 열 때 보여 줄 마지막 lines줄이다. 파일 끝의 limit바이트 안에서만 찾고, 그 창이 줄 중간에서
// 시작하면 잘린 첫 조각을 버린다.
func viewReadInitial(file io.ReaderAt, size int64, lines int, limit int64) ([]byte, error) {
	if lines <= 0 || size == 0 {
		return nil, nil
	}
	base := size - limit
	if base < 0 {
		base = 0
	}
	offset, err := viewTailOffset(io.NewSectionReader(file, base, size-base), size-base, lines)
	// 크기를 잰 뒤 파일이 잘렸다. 빈 화면에서 시작하면 따라가면서 truncate를 알아채고 처음부터 읽는다.
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := make([]byte, size-base-offset)
	count, err := file.ReadAt(data, base+offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	data = data[:count]
	if base > 0 && offset == 0 && !viewLineStartsAt(file, base) {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 && newline < len(data)-1 {
			data = data[newline+1:]
		}
	}
	return data, nil
}

func viewLineStartsAt(file io.ReaderAt, offset int64) bool {
	previous := make([]byte, 1)
	_, err := file.ReadAt(previous, offset-1)
	return err == nil && previous[0] == '\n'
}
