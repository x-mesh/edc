package edc

import (
	"bytes"
	"errors"
	"io"
	"regexp"
)

const (
	// grepBlockSize는 파일을 읽는 단위이고 index의 블록 크기다.
	grepBlockSize = 1 << 20
	// grepLineLimit는 한 줄로 다루는 최대 바이트다. 이보다 긴 줄은 앞부분만 비교한다. view와 같은 상한이다.
	grepLineLimit = 64 << 20
	// grepSampleSize는 pivot 바이트를 고르기 위해 파일에서 읽는 표본 크기다.
	grepSampleSize = 64 << 10
	// grepTailRead는 구간 끝을 지난 뒤 마지막 줄의 나머지를 읽는 단위다. 블록 단위로 읽으면 구간마다 1 MiB를 더 읽는다.
	grepTailRead = 64 << 10
	// grepPivotShare는 pivot으로 쓸 수 있는 바이트의 최대 비율이다. 가장 드문 바이트가 이보다 흔하면 IndexByte로 찍는
	// 후보가 너무 많아 bytes.Index가 빠르다. 측정에서 1.49%인 '4'까지는 pivot이 빨랐다.
	grepPivotShare = 0.015
)

// grepMatcher는 한 검색의 패턴이다. needle은 모든 일치에 반드시 들어가는 바이트라서, 줄을 하나씩 보지 않고 needle이 있는
// 자리로 바로 건너뛴다. 정규식은 LiteralPrefix만 needle로 쓴다. (?i)와 선택(a|b)에서는 빈 문자열이 나오므로 놓치는 일이 없다.
type grepMatcher struct {
	literal []byte
	regex   *regexp.Regexp
	needle  []byte
	// indexNeedle은 index의 trigram에 대는 바이트다. bloom은 대소문자를 접어 두므로 -i 문자열 패턴도 패턴 전체를 쓴다.
	indexNeedle []byte
	// pivot은 needle에서 IndexByte로 찍는 바이트의 위치다. -1이면 bytes.Index를 쓴다. second는 전체 비교 전에 확인하는
	// 두 번째 바이트다.
	pivot, second int
}

// newGrepMatcher는 pattern을 컴파일하고 sample(파일의 일부)의 바이트 빈도로 pivot을 고른다.
func newGrepMatcher(pattern string, useRegex, ignoreCase bool, sample []byte) (*grepMatcher, error) {
	matcher := &grepMatcher{pivot: -1, second: -1}
	switch {
	case !useRegex && !ignoreCase:
		matcher.literal = []byte(pattern)
		matcher.needle = matcher.literal
		matcher.indexNeedle = matcher.literal
	default:
		expr := pattern
		if !useRegex {
			expr = regexp.QuoteMeta(pattern)
		}
		if ignoreCase {
			expr = "(?i)" + expr
		}
		regex, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		matcher.regex = regex
		prefix, _ := regex.LiteralPrefix()
		matcher.needle = []byte(prefix)
		matcher.indexNeedle = matcher.needle
		if ignoreCase && !useRegex {
			// 대소문자를 무시해도 글자가 아닌 바이트는 그대로다. 그중 가장 긴 구간이 모든 일치에 들어간다.
			matcher.needle = grepLongestNonLetterRun(pattern)
			matcher.indexNeedle = []byte(pattern)
		}
	}
	matcher.pivot, matcher.second = grepChoosePivot(matcher.needle, sample)
	return matcher, nil
}

func grepLongestNonLetterRun(pattern string) []byte {
	var best []byte
	start := -1
	for index := 0; index <= len(pattern); index++ {
		if index < len(pattern) && !viewWordByte(pattern[index]) {
			if start < 0 {
				start = index
			}
			continue
		}
		if start >= 0 && index-start > len(best) {
			best = []byte(pattern[start:index])
		}
		start = -1
	}
	return best
}

// grepChoosePivot은 sample에서 가장 드문 needle 바이트의 위치와 두 번째로 드문 위치를 고른다. 가장 드문 바이트도
// grepPivotShare보다 흔하면 -1을 돌려주어 bytes.Index를 쓰게 한다. 결과는 pivot과 무관하게 전체 비교로 정해지므로
// 표본이 파일을 대표하지 않아도 느려질 뿐 틀리지 않는다.
func grepChoosePivot(needle, sample []byte) (int, int) {
	if len(needle) < 2 || len(sample) == 0 {
		return -1, -1
	}
	var histogram [256]int
	for _, char := range sample {
		histogram[char]++
	}
	first := 0
	for index := range needle {
		if histogram[needle[index]] < histogram[needle[first]] {
			first = index
		}
	}
	if float64(histogram[needle[first]])/float64(len(sample)) > grepPivotShare {
		return -1, -1
	}
	second := -1
	for index := range needle {
		if index != first && (second < 0 || histogram[needle[index]] < histogram[needle[second]]) {
			second = index
		}
	}
	return first, second
}

// find는 data에서 needle이 시작하는 첫 위치다.
func (matcher *grepMatcher) find(data []byte) int {
	if matcher.pivot < 0 {
		return bytes.Index(data, matcher.needle)
	}
	needle := matcher.needle
	position := 0
	for {
		index := bytes.IndexByte(data[position:], needle[matcher.pivot])
		if index < 0 {
			return -1
		}
		at := position + index - matcher.pivot
		if at >= 0 && at+len(needle) <= len(data) && data[at+matcher.second] == needle[matcher.second] && bytes.Equal(data[at:at+len(needle)], needle) {
			return at
		}
		position += index + 1
	}
}

// match는 line이 패턴에 맞는지 확정한다.
func (matcher *grepMatcher) match(line []byte) bool {
	if matcher.regex != nil {
		return matcher.regex.Match(line)
	}
	return bytes.Contains(line, matcher.literal)
}

// grepLineRef는 scanner가 찾은 줄이다. number는 1부터 세며 0이면 모른다. text는 emit 동안만 유효하다.
type grepLineRef struct {
	offset int64
	number int64
	text   []byte
}

// grepScanner는 파일의 한 구간을 훑는다. find는 후보 자리로 건너뛰고(nil이면 줄마다 본다), accept가 줄을 확정한다.
// 블록 크기를 바꿀 수 있어 테스트는 작은 fixture로 블록 경계를 지난다.
type grepScanner struct {
	block     int
	lineLimit int
	find      func([]byte) int
	accept    func([]byte) bool
	// buffers는 scanRange 호출 사이에 다시 쓰는 읽기 버퍼다. --last는 블록마다 scanRange를 부르므로 매번 만들면
	// 블록 수만큼 메모리를 할당한다.
	buffers *grepBuffers
	// prepare는 scanLast가 블록을 읽기 전에 부른다. 앞 줄의 level과 시각을 물려받는 필터가 그 블록 앞의 머리 줄을 찾는 데 쓴다.
	prepare func(file io.ReaderAt, from int64) error
	// verify는 읽은 조각마다 부른다. index 검색은 블록 원문의 checksum을 index와 맞춰 본다.
	verify func(offset int64, chunk []byte) error
}

// newGrepScanner는 패턴과 조건으로 scanner를 만든다. 조건이 머리 줄의 level과 시각을 물려받는 형식이면 줄을 건너뛰지 않는다.
type grepBuffers struct {
	buffer []byte
}

func newGrepScanner(matcher *grepMatcher, filter *grepFilter) grepScanner {
	scanner := grepScanner{block: grepBlockSize, lineLimit: grepLineLimit, buffers: &grepBuffers{}}
	if filter == nil || !filter.active() {
		scanner.accept = matcher.match
		if len(matcher.needle) != 0 {
			scanner.find = matcher.find
		}
		return scanner
	}
	filter.matcher = matcher
	scanner.accept = func(line []byte) bool { return filter.prefilter(line) && filter.accept(line) }
	scanner.prepare = filter.prime
	if len(matcher.needle) != 0 && !filter.needsEveryLine() {
		scanner.find = matcher.find
	}
	return scanner
}

var errGrepStopped = errors.New("grep scan stopped")

// scanRange는 [from, to)에서 시작하는 줄을 훑어 맞는 줄마다 emit을 부른다. 마지막 줄을 끝까지 읽기 위해 to 뒤도 읽고,
// from이 줄 중간이면 첫 줄바꿈까지는 앞 구간에서 시작한 줄이라 건너뛴다. 그래서 줄은 시작한 구간에 한 번만 속한다.
// lineBase는 from 앞의 줄바꿈 수다. 0 이상이면 줄 번호를 세고, -1이면 세지 않는다. 읽은 바이트 수를 돌려준다.
func (scanner grepScanner) scanRange(file io.ReaderAt, from, to, lineBase int64, emit func(grepLineRef) bool) (int64, error) {
	if scanner.buffers == nil {
		scanner.buffers = &grepBuffers{}
	}
	if cap(scanner.buffers.buffer) < scanner.block*2 {
		scanner.buffers.buffer = make([]byte, 0, scanner.block*2)
	}
	buffer := scanner.buffers.buffer[:0]
	defer func() { scanner.buffers.buffer = buffer[:0] }()
	bufferStart, readAt := from, from
	var read int64
	number := lineBase
	// discard는 다음 줄바꿈까지 버린다. 앞 구간에서 시작한 줄의 나머지와 상한을 넘은 줄의 나머지가 그 대상이다.
	// from 바로 앞이 줄바꿈이면 from에서 시작하는 줄은 이 구간의 것이다.
	discard := false
	if from > 0 {
		previous := make([]byte, 1)
		if _, err := file.ReadAt(previous, from-1); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		discard = previous[0] != '\n'
	}
	eof := false
	for {
		if !eof {
			want := scanner.block
			if readAt >= to && want > grepTailRead {
				want = grepTailRead
			}
			// 버퍼 뒤에 바로 읽어 복사를 없앤다.
			if cap(buffer)-len(buffer) < want {
				grown := make([]byte, len(buffer), 2*cap(buffer)+want)
				copy(grown, buffer)
				buffer = grown
			}
			count, err := file.ReadAt(buffer[len(buffer):len(buffer)+want], readAt)
			if err != nil && !errors.Is(err, io.EOF) {
				return read, err
			}
			if scanner.verify != nil && count > 0 {
				if err := scanner.verify(readAt, buffer[len(buffer):len(buffer)+count]); err != nil {
					return read, err
				}
			}
			buffer = buffer[:len(buffer)+count]
			readAt += int64(count)
			read += int64(count)
			eof = errors.Is(err, io.EOF) || count == 0
		}
		if discard {
			newline := bytes.IndexByte(buffer, '\n')
			if newline < 0 {
				if eof {
					return read, nil
				}
				buffer = buffer[:0]
				bufferStart = readAt
				continue
			}
			buffer = buffer[newline+1:]
			bufferStart += int64(newline + 1)
			if number >= 0 {
				number++
			}
			discard = false
			if bufferStart >= to {
				return read, nil
			}
		}
		last := bytes.LastIndexByte(buffer, '\n')
		region := buffer[:last+1]
		overlong := len(buffer)-len(region) > scanner.lineLimit
		if eof || overlong {
			// 줄바꿈 없는 마지막 줄, 또는 상한을 넘은 줄의 앞부분도 한 줄이다.
			region = buffer
		}
		if len(region) != 0 {
			if err := scanner.scanRegion(region, bufferStart, to, &number, emit); err != nil {
				if errors.Is(err, errGrepStopped) {
					return read, nil
				}
				return read, err
			}
		}
		next := bufferStart + int64(len(region))
		if eof || next >= to {
			return read, nil
		}
		discard = overlong
		buffer = append(buffer[:0], buffer[len(region):]...)
		bufferStart = next
	}
}

// scanRegion은 완성된 줄(또는 마지막 조각)만 든 region을 훑는다. 후보를 찾으면 그 줄의 경계를 잡아 accept로 확정한다.
// to 이후에 시작한 줄이 나오면 멈춘다.
func (scanner grepScanner) scanRegion(region []byte, regionStart, to int64, number *int64, emit func(grepLineRef) bool) error {
	position, counted := 0, 0
	for position < len(region) {
		lineStart, searchFrom := position, position
		if scanner.find != nil {
			candidate := scanner.find(region[position:])
			if candidate < 0 {
				break
			}
			candidate += position
			lineStart = bytes.LastIndexByte(region[:candidate], '\n') + 1
			searchFrom = candidate
		}
		lineEnd := len(region)
		if newline := bytes.IndexByte(region[searchFrom:], '\n'); newline >= 0 {
			lineEnd = searchFrom + newline
		}
		offset := regionStart + int64(lineStart)
		if offset >= to {
			return errGrepStopped
		}
		if *number >= 0 {
			*number += int64(bytes.Count(region[counted:lineStart], []byte{'\n'}))
			counted = lineStart
		}
		line := region[lineStart:lineEnd]
		if len(line) > scanner.lineLimit {
			line = line[:scanner.lineLimit]
		}
		if scanner.accept(line) {
			ref := grepLineRef{offset: offset, text: line}
			if *number >= 0 {
				ref.number = *number + 1
			}
			if !emit(ref) {
				return errGrepStopped
			}
		}
		position = lineEnd + 1
	}
	if *number >= 0 {
		*number += int64(bytes.Count(region[counted:], []byte{'\n'}))
	}
	return nil
}

// grepLine은 scanLast가 복사해 둔 줄이다.
type grepLine struct {
	offset int64
	text   []byte
}

// scanLast는 파일 끝에서 블록 단위로 거꾸로 읽어 가장 최근의 맞는 줄 count개를 찾는다. 결과는 파일 순서다.
// 블록은 index와 같은 경계에 맞춰, 뒤 블록을 다 본 뒤에야 앞 블록으로 간다.
func (scanner grepScanner) scanLast(file io.ReaderAt, size int64, count int) ([]grepLine, int64, error) {
	var found []grepLine
	var read int64
	end := size
	for end > 0 && len(found) < count {
		start := (end - 1) / int64(scanner.block) * int64(scanner.block)
		if scanner.prepare != nil {
			if err := scanner.prepare(file, start); err != nil {
				return nil, read, err
			}
		}
		var block []grepLine
		n, err := scanner.scanRange(file, start, end, -1, func(ref grepLineRef) bool {
			block = append(block, grepLine{ref.offset, bytes.Clone(ref.text)})
			return true
		})
		read += n
		if err != nil {
			return nil, read, err
		}
		if need := count - len(found); len(block) > need {
			block = block[len(block)-need:]
		}
		found = append(block, found...)
		end = start
	}
	return found, read, nil
}
