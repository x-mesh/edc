package edc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// grepBloomBytes는 블록 하나의 bloom filter 크기다. 1 MiB 블록에 8 KiB면 파일의 0.8%다. 실제 syslog에서 비트 밀도가
	// 6.3%였고, trigram 셋 이상인 낱말은 거짓 양성이 드물었다.
	grepBloomBytes = 8 << 10
	// grepIndexMinSize보다 작은 파일은 index를 만들지 않는다. 그 크기는 훑는 데 1초가 걸리지 않는다.
	grepIndexMinSize = 64 << 20
	// grepIndexCacheCap은 캐시 디렉터리의 index 전체 상한이다. 넘으면 오래 쓰지 않은 것부터 지운다.
	grepIndexCacheCap = 1 << 30
	// grepIndexSeenWindow 안에 같은 파일을 다시 검색하면 그때 index를 만든다. 첫 검색은 grep과 같은 비용으로 끝낸다.
	grepIndexSeenWindow = 24 * time.Hour
	grepIndexVersion    = 1
)

var grepIndexMagic = [8]byte{'E', 'D', 'C', 'G', 'R', 'I', 'D', 'X'}

var grepCRC = crc32.MakeTable(crc32.Castagnoli)

// grepBlockRegion은 블록 하나에서 시작하는 줄 전체다. 마지막 줄이 블록 끝을 넘으면 그 끝까지 담으므로, 블록 경계에 걸친
// 문자열도 그 줄이 시작한 블록의 sketch에 들어간다. raw는 블록 자리의 원문 바이트로 checksum에 쓴다.
// complete는 블록이 가득 찼고 마지막 줄이 줄바꿈으로 끝나, 뒤에 무엇이 쌓여도 이 블록에서 시작하는 줄이 바뀌지 않는다는 뜻이다.
// 그런 블록만 index에 넣는다. data와 raw는 visit 동안만 유효하다.
type grepBlockRegion struct {
	index    int
	start    int64
	data     []byte
	raw      []byte
	lines    int64
	complete bool
}

// grepReadBlocks는 firstBlock부터 size까지 블록을 차례로 읽어 visit에 넘긴다. lines는 firstBlock 앞의 줄바꿈 수다.
// 블록보다 긴 줄이 와도 버퍼는 그 줄이 끝날 때까지만 자란다.
func grepReadBlocks(file io.ReaderAt, size int64, blockSize, lineLimit, firstBlock int, lines int64, visit func(grepBlockRegion) error) error {
	block := int64(blockSize)
	bufferStart := int64(firstBlock) * block
	if bufferStart >= size {
		return nil
	}
	lineStart := true
	if bufferStart > 0 {
		previous := make([]byte, 1)
		if _, err := file.ReadAt(previous, bufferStart-1); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		lineStart = previous[0] == '\n'
	}
	buffer := make([]byte, 0, 3*blockSize)
	chunk := make([]byte, blockSize)
	readAt := bufferStart
	fill := func(until int64) error {
		for readAt < until && readAt < size {
			want := int64(len(chunk))
			if readAt+want > size {
				want = size - readAt
			}
			count, err := file.ReadAt(chunk[:want], readAt)
			buffer = append(buffer, chunk[:count]...)
			readAt += int64(count)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if count == 0 || errors.Is(err, io.EOF) {
				size = readAt
				return nil
			}
		}
		return nil
	}
	for index := firstBlock; ; index++ {
		blockStart := int64(index) * block
		if blockStart >= size {
			return nil
		}
		blockEnd := blockStart + block
		if err := fill(blockEnd); err != nil {
			return err
		}
		rel := func(offset int64) int { return int(offset - bufferStart) }
		full := blockEnd <= size
		// 블록이 줄 중간에서 시작하면 첫 줄바꿈 뒤가 이 블록의 첫 줄이다. 블록 안에 줄바꿈이 없으면 이 블록에서 시작하는 줄은 없다.
		start := blockStart
		if !lineStart {
			limit := blockEnd
			if limit > size {
				limit = size
			}
			newline := bytes.IndexByte(buffer[rel(blockStart):rel(limit)], '\n')
			if newline < 0 {
				start = limit
			} else {
				start = blockStart + int64(newline) + 1
			}
		}
		// 마지막 줄의 끝은 blockEnd-1 이후의 첫 줄바꿈이다. 그 줄바꿈이 blockEnd-1에 있으면 줄이 블록 끝에서 끝난 것이다.
		end, complete := int64(0), false
		if start >= blockEnd {
			end = start
			complete = full
		} else {
			search := blockEnd - 1
			for {
				if err := fill(search + 1); err != nil {
					return err
				}
				if search >= size {
					end, complete = size, false
					break
				}
				newline := bytes.IndexByte(buffer[rel(search):], '\n')
				if newline >= 0 {
					end, complete = search+int64(newline)+1, full
					break
				}
				search = bufferStart + int64(len(buffer))
				if search >= size {
					// 파일이 이 줄 중간에서 끝났다. 더 쌓이면 바뀌는 줄이라 index에 넣지 않는다.
					end, complete = size, false
					break
				}
				if search-blockEnd > int64(lineLimit) {
					// 상한을 넘은 줄은 뒤가 어떻든 비교하지 않는다. 어디서 끝나는지 모르므로 index에는 넣지 않고,
					// 나머지는 다음 블록들이 줄바꿈까지 건너뛴다.
					end, complete = blockEnd+int64(lineLimit), false
					break
				}
			}
		}
		rawEnd := blockEnd
		if rawEnd > size {
			rawEnd = size
		}
		region := grepBlockRegion{index: index, start: start, data: buffer[rel(start):rel(end)], raw: buffer[rel(blockStart):rel(rawEnd)], lines: lines, complete: complete}
		if err := visit(region); err != nil {
			return err
		}
		lines += int64(bytes.Count(region.raw, []byte{'\n'}))
		if !full {
			return nil
		}
		lineStart = buffer[rel(blockEnd-1)] == '\n'
		keep := blockEnd
		if end > keep {
			keep = end
			// 긴 줄이 다음 블록들을 다 차지했으면 그 블록들에서 시작하는 줄은 없다.
			for next := index + 1; int64(next+1)*block <= end; next++ {
				index++
				nextStart := int64(next) * block
				nextEnd := nextStart + block
				empty := grepBlockRegion{index: next, start: nextEnd, lines: lines, complete: nextEnd <= size, raw: buffer[rel(nextStart):rel(nextEnd)]}
				if err := visit(empty); err != nil {
					return err
				}
				lines += int64(bytes.Count(empty.raw, []byte{'\n'}))
			}
			lineStart = buffer[rel(end-1)] == '\n'
			keep = int64(index+1) * block
			if keep < end {
				// 줄이 다음 블록 중간에서 끝났다. 그 블록은 줄 중간에서 시작한다.
				lineStart = false
				keep = int64(index+1) * block
			}
		}
		buffer = append(buffer[:0], buffer[rel(keep):]...)
		bufferStart = keep
	}
}

// grepSketch는 블록 하나의 요약이다. bloom은 글자가 든 trigram이고, 시각 범위와 level·status flags는 그 블록에서 시작한
// 줄 전체의 것이다. 이어지는 줄은 앞 블록의 머리 줄에서 level과 시각을 물려받으므로, 그 블록의 마지막 머리 줄(lastLevel,
// lastEvent)과 이 블록이 이어지는 줄로 시작하는지(flag)를 함께 두어 검색할 때 이어 붙인다.
type grepSketch struct {
	MinTime   int64
	MaxTime   int64
	LastEvent int64
	Lines     int64
	Flags     uint32
	CRC       uint32
	LastLevel int8
	Bloom     [grepBloomBytes]byte
}

const (
	grepFlagStatusShift = 8
	grepFlagHasHeader   = 1 << 20
	grepFlagLeadingCont = 1 << 21
)

func grepFoldByte(char byte) byte {
	if char >= 'A' && char <= 'Z' {
		return char + 32
	}
	return char
}

// grepTrigrams는 data에서 글자가 든 trigram마다 visit을 부른다. 숫자와 구두점만으로 된 trigram은 IP와 숫자 때문에
// 어느 블록에나 있어 거르는 힘이 없다. 대소문자는 접어서 -i에도 쓴다.
func grepTrigrams(data []byte, visit func(uint32)) {
	for position := 0; position+2 < len(data); position++ {
		a, b, c := grepFoldByte(data[position]), grepFoldByte(data[position+1]), grepFoldByte(data[position+2])
		if !grepLetter(a) && !grepLetter(b) && !grepLetter(c) {
			continue
		}
		visit(uint32(a)<<16 | uint32(b)<<8 | uint32(c))
	}
}

func grepLetter(char byte) bool { return char >= 'a' && char <= 'z' }

// grepBloomBits는 trigram 하나가 켜는 비트 세 개다.
func grepBloomBits(trigram uint32) [3]uint32 {
	return [3]uint32{
		(trigram * 0x9E3779B1) >> 16,
		(trigram * 0x85EBCA77) >> 16,
		(trigram * 0xC2B2AE3D) >> 16,
	}
}

func grepBloomHas(bloom *[grepBloomBytes]byte, bits [3]uint32) bool {
	for _, bit := range bits {
		if bloom[bit>>3]&(1<<(bit&7)) == 0 {
			return false
		}
	}
	return true
}

// grepQueryBits는 needle의 trigram이 켜야 하는 비트다. 글자가 없으면 비어 있어 내용으로는 거르지 않는다.
// 비ASCII 바이트가 든 -i 패턴은 접은 결과를 알 수 없으므로 빈 값이다.
func grepQueryBits(needle []byte, ignoreCase bool) [][3]uint32 {
	if ignoreCase {
		for _, char := range needle {
			if char >= 0x80 {
				return nil
			}
		}
	}
	var bits [][3]uint32
	grepTrigrams(needle, func(trigram uint32) { bits = append(bits, grepBloomBits(trigram)) })
	return bits
}

// grepBuildSketch는 블록 하나의 sketch다. worker가 블록마다 독립적으로 부르므로 앞 블록의 머리 줄은 모른다.
func grepBuildSketch(region grepBlockRegion, format viewFormat, now time.Time) grepSketch {
	sketch := grepSketch{Lines: region.lines, LastLevel: -1, CRC: crc32.Checksum(region.raw, grepCRC)}
	filter := newGrepFilter(format, nil, now)
	inherits := format == viewFormatLevel || format == viewFormatJSON
	data := region.data
	for len(data) != 0 {
		end := bytes.IndexByte(data, '\n')
		next := end + 1
		if end < 0 {
			end, next = len(data), len(data)
		}
		line := data[:end]
		if len(line) > grepLineLimit {
			line = line[:grepLineLimit]
		}
		grepTrigrams(line, func(trigram uint32) {
			for _, bit := range grepBloomBits(trigram) {
				sketch.Bloom[bit>>3] |= 1 << (bit & 7)
			}
		})
		fields := filter.track(line)
		header := inherits && fields.event == nil && (fields.level != "" || len(fields.timeText) != 0)
		if fields.event != nil && sketch.Flags&grepFlagHasHeader == 0 {
			sketch.Flags |= grepFlagLeadingCont
		}
		if rank := grepLevelRank(fields.level); rank >= 0 {
			sketch.Flags |= 1 << rank
		}
		if fields.status >= 100 {
			sketch.Flags |= 1 << (grepFlagStatusShift + fields.status/100)
		}
		at, ok := filter.timeOf(fields)
		if ok {
			unix := at.UnixNano()
			if sketch.MinTime == 0 || unix < sketch.MinTime {
				sketch.MinTime = unix
			}
			if unix > sketch.MaxTime {
				sketch.MaxTime = unix
			}
		}
		if header {
			sketch.Flags |= grepFlagHasHeader
			sketch.LastLevel = int8(grepLevelRank(fields.level))
			sketch.LastEvent = 0
			if ok {
				sketch.LastEvent = at.UnixNano()
			}
		}
		data = data[next:]
	}
	return sketch
}

// grepIndexHeader는 index 파일의 머리다. 파일이 바뀌었는지 보기 위해 만들 때의 크기와 첫 블록·마지막 블록의 checksum을 둔다.
type grepIndexHeader struct {
	Magic    [8]byte
	Version  uint32
	Block    uint32
	Format   [8]byte
	FileSize int64
	Indexed  int64
	Blocks   uint32
	FirstCRC uint32
	LastCRC  uint32
	Reserved uint32
	Lines    int64
}

var grepSketchSize = binary.Size(grepSketch{})

// encode와 decode는 binary.Write, binary.Read와 같은 배치(little endian, 필드 순서)지만 reflection을 쓰지 않는다.
// 3,814개 record를 reflection으로 읽으면 0.4초가 들어 작은 검색의 대부분을 차지했다.
func (sketch *grepSketch) encode(out []byte) {
	binary.LittleEndian.PutUint64(out[0:], uint64(sketch.MinTime))
	binary.LittleEndian.PutUint64(out[8:], uint64(sketch.MaxTime))
	binary.LittleEndian.PutUint64(out[16:], uint64(sketch.LastEvent))
	binary.LittleEndian.PutUint64(out[24:], uint64(sketch.Lines))
	binary.LittleEndian.PutUint32(out[32:], sketch.Flags)
	binary.LittleEndian.PutUint32(out[36:], sketch.CRC)
	out[40] = byte(sketch.LastLevel)
	copy(out[41:], sketch.Bloom[:])
}

func (sketch *grepSketch) decode(in []byte) {
	sketch.MinTime = int64(binary.LittleEndian.Uint64(in[0:]))
	sketch.MaxTime = int64(binary.LittleEndian.Uint64(in[8:]))
	sketch.LastEvent = int64(binary.LittleEndian.Uint64(in[16:]))
	sketch.Lines = int64(binary.LittleEndian.Uint64(in[24:]))
	sketch.Flags = binary.LittleEndian.Uint32(in[32:])
	sketch.CRC = binary.LittleEndian.Uint32(in[36:])
	sketch.LastLevel = int8(in[40])
	copy(sketch.Bloom[:], in[41:])
}

var grepIndexHeaderSize = binary.Size(grepIndexHeader{})

func grepFormatBytes(format viewFormat) [8]byte {
	var out [8]byte
	copy(out[:], format)
	return out
}

// grepWriteIndex는 header, record, 그리고 전체 checksum을 임시 파일에 쓰고 rename한다. 동시에 두 검색이 써도 온전한 쪽만 남는다.
// records는 앞 index 파일의 record 바이트를 그대로 복사하는 reader와 새 sketch로 나눠 받아, 긴 파일의 index를 메모리에
// 다 올리지 않는다.
func grepWriteIndex(path string, header grepIndexHeader, existing io.Reader, existingCount int, fresh []grepSketch) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	sum := crc32.New(grepCRC)
	writer := bufio.NewWriterSize(io.MultiWriter(temp, sum), 1<<20)
	header.Magic, header.Version, header.Blocks = grepIndexMagic, grepIndexVersion, uint32(existingCount+len(fresh))
	if err := binary.Write(writer, binary.LittleEndian, header); err != nil {
		return err
	}
	if existing != nil {
		if _, err := io.CopyN(writer, existing, int64(existingCount)*int64(grepSketchSize)); err != nil {
			return err
		}
	}
	record := make([]byte, grepSketchSize)
	for index := range fresh {
		fresh[index].encode(record)
		if _, err := writer.Write(record); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := binary.Write(temp, binary.LittleEndian, sum.Sum32()); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

var errGrepIndexInvalid = errors.New("grep index invalid")

// grepIndexFile은 열어서 검증한 index다. record는 순서대로 streaming으로 읽는다.
type grepIndexFile struct {
	path   string
	header grepIndexHeader
	file   *os.File
	reader *bufio.Reader
	size   int64
	record []byte
}

// grepOpenIndex는 index를 열고 전체 checksum, 버전, 블록 크기, 형식을 확인한다. 로그 파일과의 대조는 grepIndexMatches가 한다.
func grepOpenIndex(path string, blockSize int, format viewFormat) (*grepIndexFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	sum := crc32.New(grepCRC)
	body := info.Size() - 4
	if body < int64(grepIndexHeaderSize) {
		file.Close()
		return nil, fmt.Errorf("%w: short file", errGrepIndexInvalid)
	}
	if _, err := io.Copy(sum, io.NewSectionReader(file, 0, body)); err != nil {
		file.Close()
		return nil, err
	}
	var stored uint32
	if err := binary.Read(io.NewSectionReader(file, body, 4), binary.LittleEndian, &stored); err != nil || stored != sum.Sum32() {
		file.Close()
		return nil, fmt.Errorf("%w: checksum", errGrepIndexInvalid)
	}
	var header grepIndexHeader
	if err := binary.Read(io.NewSectionReader(file, 0, int64(grepIndexHeaderSize)), binary.LittleEndian, &header); err != nil {
		file.Close()
		return nil, err
	}
	switch {
	case header.Magic != grepIndexMagic || header.Version != grepIndexVersion:
		file.Close()
		return nil, fmt.Errorf("%w: version", errGrepIndexInvalid)
	case int(header.Block) != blockSize || header.Format != grepFormatBytes(format):
		file.Close()
		return nil, fmt.Errorf("%w: layout", errGrepIndexInvalid)
	case int64(grepIndexHeaderSize)+int64(header.Blocks)*int64(grepSketchSize) != body:
		file.Close()
		return nil, fmt.Errorf("%w: record count", errGrepIndexInvalid)
	}
	index := &grepIndexFile{path: path, header: header, file: file, size: info.Size()}
	index.rewind()
	return index, nil
}

func (index *grepIndexFile) rewind() {
	index.reader = bufio.NewReaderSize(io.NewSectionReader(index.file, int64(grepIndexHeaderSize), int64(index.header.Blocks)*int64(grepSketchSize)), 1<<20)
}

func (index *grepIndexFile) next(sketch *grepSketch) error {
	if index.record == nil {
		index.record = make([]byte, grepSketchSize)
	}
	if _, err := io.ReadFull(index.reader, index.record); err != nil {
		return err
	}
	sketch.decode(index.record)
	return nil
}

// records는 record 바이트를 처음부터 읽는 reader다. index를 늘려 다시 쓸 때 복사한다.
func (index *grepIndexFile) records() io.Reader {
	return io.NewSectionReader(index.file, int64(grepIndexHeaderSize), int64(index.header.Blocks)*int64(grepSketchSize))
}

func (index *grepIndexFile) close() error { return index.file.Close() }

// grepBlockCRC는 로그 파일의 한 블록 자리 원문 checksum이다.
func grepBlockCRC(file io.ReaderAt, offset, length int64) (uint32, error) {
	buffer := make([]byte, length)
	count, err := file.ReadAt(buffer, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return crc32.Checksum(buffer[:count], grepCRC), nil
}

// grepIndexMatches는 index가 지금의 로그 파일에 맞는지 본다. 크기가 줄었거나 첫 블록이나 마지막으로 index한 블록의 내용이
// 바뀌었으면 다른 파일이거나 다시 쓰인 파일이다. 뒤에 쌓인 부분은 index에 없을 뿐 맞는 것이다.
func grepIndexMatches(index *grepIndexFile, file io.ReaderAt, size int64) error {
	header := index.header
	if header.FileSize > size || header.Indexed > size {
		return fmt.Errorf("%w: file shrank", errGrepIndexInvalid)
	}
	block := int64(header.Block)
	first := block
	if first > size {
		first = size
	}
	if sum, err := grepBlockCRC(file, 0, first); err != nil {
		return err
	} else if sum != header.FirstCRC {
		return fmt.Errorf("%w: first block changed", errGrepIndexInvalid)
	}
	if header.Indexed >= block {
		if sum, err := grepBlockCRC(file, header.Indexed-block, block); err != nil {
			return err
		} else if sum != header.LastCRC {
			return fmt.Errorf("%w: last indexed block changed", errGrepIndexInvalid)
		}
	}
	return nil
}

// grepIndexQuery는 블록을 건너뛸지 정하는 조건이다. 패턴의 trigram, 시각 범위, level과 status의 flags다.
type grepIndexQuery struct {
	bits        [][3]uint32
	since       time.Time
	until       time.Time
	minLevel    int
	statusMask  uint32
	needsTime   bool
	needsLevel  bool
	needsStatus bool
}

func newGrepIndexQuery(matcher *grepMatcher, ignoreCase bool, filter *grepFilter) grepIndexQuery {
	query := grepIndexQuery{bits: grepQueryBits(matcher.indexNeedle, ignoreCase), minLevel: -1}
	if filter == nil {
		return query
	}
	query.since, query.until = filter.since, filter.until
	query.needsTime = !filter.since.IsZero() || !filter.until.IsZero()
	query.minLevel, query.needsLevel = filter.minLevel, filter.minLevel >= 0
	if filter.statuses != nil {
		query.needsStatus = true
		classes := filter.statuses.classes
		for status := range filter.statuses.exact {
			classes[status/100] = true
		}
		for _, span := range filter.statuses.ranges {
			for class := span[0] / 100; class <= span[1]/100; class++ {
				classes[class] = true
			}
		}
		for class, on := range classes {
			if on {
				query.statusMask |= 1 << (grepFlagStatusShift + class)
			}
		}
	}
	return query
}

// grepCarried는 앞 블록들의 마지막 머리 줄이다. 이어지는 줄로 시작하는 블록은 이 값을 물려받는다.
type grepCarried struct {
	level int8
	at    int64
	known bool
}

func (carried *grepCarried) update(sketch *grepSketch) {
	if sketch.Flags&grepFlagHasHeader != 0 {
		carried.level, carried.at, carried.known = sketch.LastLevel, sketch.LastEvent, true
	}
}

// pass는 블록에 조건에 맞는 줄이 있을 수 있는지다. 거짓 양성은 블록을 읽어 보는 시간 손실일 뿐이고, 거짓 음성이 없어야 한다.
func (query grepIndexQuery) pass(sketch *grepSketch, carried grepCarried) bool {
	for _, bits := range query.bits {
		if !grepBloomHas(&sketch.Bloom, bits) {
			return false
		}
	}
	inherits := sketch.Flags&grepFlagLeadingCont != 0 && carried.known
	if query.needsTime {
		minTime, maxTime := sketch.MinTime, sketch.MaxTime
		if inherits && carried.at != 0 {
			if minTime == 0 || carried.at < minTime {
				minTime = carried.at
			}
			if carried.at > maxTime {
				maxTime = carried.at
			}
		}
		if maxTime == 0 {
			return false
		}
		if !query.since.IsZero() && maxTime < query.since.UnixNano() {
			return false
		}
		if !query.until.IsZero() && minTime > query.until.UnixNano() {
			return false
		}
	}
	flags := sketch.Flags
	if inherits && carried.level >= 0 {
		flags |= 1 << uint(carried.level)
	}
	if query.needsLevel {
		if flags>>uint(query.minLevel)&((1<<(len(viewLevels)-query.minLevel))-1) == 0 {
			return false
		}
	}
	if query.needsStatus && flags&query.statusMask == 0 {
		return false
	}
	return true
}

// grepIndexRoot가 비어 있지 않으면 index를 그 아래에 둔다. 테스트가 캐시 위치를 바꿀 때 쓴다.
var grepIndexRoot string

// grepIndexDir은 index를 두는 곳이다. 설정은 os.UserConfigDir에 두는 것과 나란히 캐시는 os.UserCacheDir에 둔다.
func grepIndexDir() (string, error) {
	base := grepIndexRoot
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "edc", "grep"), nil
}

// grepFileKey는 장치와 inode로 만든 파일의 이름이다. 경로가 바뀌어도(logrotate) 같은 파일을 가리킨다.
func grepFileKey(info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%x-%x", uint64(stat.Dev), uint64(stat.Ino)), true
}

// grepIndexTrim은 캐시가 상한을 넘으면 오래 쓰지 않은 index부터 지운다. 쓴 index는 mtime을 갱신하므로 mtime이 쓴 순서다.
func grepIndexTrim(dir string, limit int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type candidate struct {
		path string
		size int64
		used time.Time
	}
	var candidates []candidate
	var total int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".idx") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{filepath.Join(dir, entry.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].used.Before(candidates[right].used) })
	for _, entry := range candidates {
		if total <= limit {
			break
		}
		if err := os.Remove(entry.path); err == nil {
			total -= entry.size
		}
	}
	return nil
}

// grepSketchWorkers는 블록 sketch를 만드는 goroutine 수다. 코어의 절반, 최대 4개라서 장애 중인 서버의 코어를 다 쓰지 않는다.
func grepSketchWorkers() int {
	workers := runtime.NumCPU() / 2
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// grepSketchPool은 블록 region을 받아 sketch를 병렬로 만든다. 결과는 블록 순서대로 모인다.
type grepSketchPool struct {
	format  viewFormat
	now     time.Time
	jobs    chan grepBlockRegion
	results map[int]grepSketch
	first   int
	mutex   sync.Mutex
	group   sync.WaitGroup
}

func newGrepSketchPool(format viewFormat, now time.Time, workers, firstBlock int) *grepSketchPool {
	pool := &grepSketchPool{format: format, now: now, jobs: make(chan grepBlockRegion, workers*2), results: map[int]grepSketch{}, first: firstBlock}
	for range workers {
		pool.group.Add(1)
		go func() {
			defer pool.group.Done()
			for region := range pool.jobs {
				sketch := grepBuildSketch(region, format, now)
				pool.mutex.Lock()
				pool.results[region.index] = sketch
				pool.mutex.Unlock()
			}
		}()
	}
	return pool
}

// submit은 region을 복사해 worker에 넘긴다. 호출한 쪽의 버퍼는 곧 재사용되기 때문이다.
func (pool *grepSketchPool) submit(region grepBlockRegion) {
	region.raw = bytes.Clone(region.raw)
	region.data = bytes.Clone(region.data)
	pool.jobs <- region
}

// finish는 worker를 끝내고 첫 블록부터 빈틈없이 이어진 sketch를 순서대로 돌려준다.
func (pool *grepSketchPool) finish() []grepSketch {
	close(pool.jobs)
	pool.group.Wait()
	var sketches []grepSketch
	for index := pool.first; ; index++ {
		sketch, ok := pool.results[index]
		if !ok {
			return sketches
		}
		sketches = append(sketches, sketch)
	}
}

var errGrepIndexStale = errors.New("grep index stale")

// grepScanBlocks는 firstBlock부터 끝까지 블록 reader로 읽으며 줄을 훑고, pool이 있으면 complete한 블록을 sketch로 만든다.
// index를 처음 만들 때와 index 뒤에 쌓인 부분을 이어 만들 때 쓴다. index는 앞에서부터 빈틈없이 이어진 블록만 담으므로
// complete하지 않은 블록이 나오면 그 뒤는 sketch로 만들지 않는다. 돌려주는 값은 읽은 바이트, sketch로 만든 블록 수,
// 그 블록들 끝까지의 줄바꿈 수다.
func grepScanBlocks(file io.ReaderAt, size int64, scanner grepScanner, filter *grepFilter, firstBlock int, lines int64, numbers bool, pool *grepSketchPool, emit func(grepLineRef) bool) (int64, int, int64, error) {
	var read int64
	indexed, linesAt := 0, lines
	contiguous := pool != nil
	stopped := false
	err := grepReadBlocks(file, size, scanner.block, scanner.lineLimit, firstBlock, lines, func(region grepBlockRegion) error {
		read += int64(len(region.raw))
		if contiguous && region.complete {
			pool.submit(region)
			indexed++
			linesAt = region.lines + int64(bytes.Count(region.raw, []byte{'\n'}))
		} else {
			contiguous = false
		}
		if stopped || len(region.data) == 0 {
			return nil
		}
		number := int64(-1)
		if numbers {
			number = region.lines + int64(bytes.Count(region.raw[:region.start-int64(region.index)*int64(scanner.block)], []byte{'\n'}))
		}
		blockEnd := int64(region.index+1) * int64(scanner.block)
		err := scanner.scanRegion(region.data, region.start, blockEnd, &number, func(ref grepLineRef) bool {
			if !emit(ref) {
				stopped = true
				return false
			}
			return true
		})
		if errors.Is(err, errGrepStopped) {
			return nil
		}
		return err
	})
	return read, indexed, linesAt, err
}

// grepIndexedSearch는 index로 블록을 가려 가며 검색한다. index가 덮지 않는 뒤쪽은 그대로 훑고, pool이 있으면 그 부분의
// sketch를 만들어 index를 늘릴 수 있게 한다.
type grepIndexedSearch struct {
	index   *grepIndexFile
	file    io.ReaderAt
	size    int64
	scanner grepScanner
	filter  *grepFilter
	query   grepIndexQuery
	numbers bool
	// 통계는 hint에 쓴다. sequential은 통과하는 블록이 많아 index를 쓰지 않고 순서대로 읽었다는 뜻이다.
	blocksRead int
	bytesRead  int64
	stopped    bool
	sequential bool
}

// grepIndexSequentialPercent보다 많은 블록이 통과하면 index로 건너뛰지 않고 순서대로 읽는다.
const grepIndexSequentialPercent = 20

// run은 index의 블록을 차례로 보며 조건에 맞을 수 있는 블록만 읽는다. 이어진 블록들은 한 구간으로 묶어 한 번에 훑는다.
// 블록마다 따로 읽으면 마지막 줄의 나머지를 읽는 비용과 버퍼가 블록 수만큼 든다. 읽은 블록의 checksum이 index와 다르면
// errGrepIndexStale이다. 그 뒤 index 밖의 꼬리를 훑는다. 돌려주는 값은 꼬리에서 만든 sketch와 그 끝까지의 줄바꿈 수다.
func (search *grepIndexedSearch) run(pool *grepSketchPool, emit func(grepLineRef) bool) ([]grepSketch, int64, error) {
	header := search.index.header
	block := int64(header.Block)
	if int64(search.scanner.block) != block {
		return nil, 0, fmt.Errorf("%w: block size", errGrepIndexInvalid)
	}
	search.index.rewind()
	carried := grepCarried{level: -1}
	wrapped := func(ref grepLineRef) bool {
		if !emit(ref) {
			search.stopped = true
			return false
		}
		return true
	}
	// 먼저 record를 모두 보며 어느 블록이 통과하는지 정한다. 블록마다 checksum과 줄 수만 남기므로 메모리는 블록당 12바이트다.
	blocks := int(header.Blocks)
	passes := make([]bool, blocks)
	crcs := make([]uint32, blocks)
	lines := make([]int64, blocks)
	passing := 0
	var sketch grepSketch
	for number := 0; number < blocks; number++ {
		if err := search.index.next(&sketch); err != nil {
			return nil, 0, fmt.Errorf("%w: %v", errGrepIndexInvalid, err)
		}
		passes[number] = search.query.pass(&sketch, carried)
		crcs[number], lines[number] = sketch.CRC, sketch.Lines
		carried.update(&sketch)
		if passes[number] {
			passing++
		}
	}
	// 통과하는 블록이 많으면 건너뛰어 얻는 것보다 블록을 따로 읽는 비용이 크다. HDD에서는 1 MiB 블록을 흩어 읽는 쪽이
	// 순서대로 읽는 쪽보다 느렸다. 그때는 index를 믿지 않고 처음부터 끝까지 읽으므로 checksum 확인도 필요 없다.
	if passing*100 > blocks*grepIndexSequentialPercent && blocks > 0 {
		search.sequential = true
		for number := range passes {
			passes[number] = true
		}
		crcs = nil
	}
	runStart := -1
	flush := func(runEnd int) error {
		if runStart < 0 {
			return nil
		}
		from, to := int64(runStart)*block, int64(runEnd)*block
		if search.filter != nil {
			if err := search.filter.prime(search.file, from); err != nil {
				return err
			}
		}
		scanner := search.scanner
		if crcs != nil {
			scanner.verify = func(offset int64, chunk []byte) error {
				if offset < from || offset >= to || (offset-from)%block != 0 || int64(len(chunk)) != block {
					return nil
				}
				if crc32.Checksum(chunk, grepCRC) != crcs[runStart+int((offset-from)/block)] {
					return errGrepIndexStale
				}
				return nil
			}
		}
		lineBase := int64(-1)
		if search.numbers {
			lineBase = lines[runStart]
		}
		read, err := scanner.scanRange(search.file, from, to, lineBase, wrapped)
		search.blocksRead += runEnd - runStart
		search.bytesRead += read
		runStart = -1
		return err
	}
	for number := 0; number < blocks; number++ {
		if passes[number] && !search.stopped {
			if runStart < 0 {
				runStart = number
			}
			continue
		}
		if err := flush(number); err != nil {
			return nil, 0, err
		}
	}
	if err := flush(blocks); err != nil {
		return nil, 0, err
	}
	if search.stopped || header.Indexed >= search.size {
		return nil, header.Lines, nil
	}
	if search.filter != nil {
		if err := search.filter.prime(search.file, header.Indexed); err != nil {
			return nil, 0, err
		}
	}
	if pool == nil {
		lineBase := int64(-1)
		if search.numbers {
			lineBase = header.Lines
		}
		read, err := search.scanner.scanRange(search.file, header.Indexed, search.size, lineBase, wrapped)
		search.bytesRead += read
		return nil, header.Lines, err
	}
	read, _, linesAt, err := grepScanBlocks(search.file, search.size, search.scanner, search.filter, int(header.Blocks), header.Lines, search.numbers, pool, wrapped)
	search.bytesRead += read
	if err != nil {
		return nil, 0, err
	}
	return pool.finish(), linesAt, nil
}

// grepIndexHeaderFor는 sketch들로 header를 채운다. 첫 블록과 마지막 블록의 checksum은 sketch가 이미 갖고 있다.
func grepIndexHeaderFor(format viewFormat, blockSize int, size int64, first, last *grepSketch, blocks int, lines int64) grepIndexHeader {
	header := grepIndexHeader{Block: uint32(blockSize), Format: grepFormatBytes(format), FileSize: size, Indexed: int64(blocks) * int64(blockSize), Lines: lines}
	if first != nil {
		header.FirstCRC = first.CRC
	}
	if last != nil {
		header.LastCRC = last.CRC
	}
	return header
}
