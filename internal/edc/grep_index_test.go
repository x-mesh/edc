package edc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const grepTestBlock = 4096

func grepTestScanner(t *testing.T, pattern string, regex, ignoreCase bool, format viewFormat, configure func(*grepFilter), data []byte) (grepScanner, *grepMatcher, *grepFilter) {
	t.Helper()
	matcher, err := newGrepMatcher(pattern, regex, ignoreCase, data[:min(len(data), grepSampleSize)])
	if err != nil {
		t.Fatal(err)
	}
	filter := newGrepFilter(format, nil, time.Now())
	if configure != nil {
		configure(filter)
	}
	scanner := newGrepScanner(matcher, filter)
	scanner.block = grepTestBlock
	return scanner, matcher, filter
}

// grepBuildTestIndex는 data 전체를 훑으며 index를 만들어 파일로 쓴다.
func grepBuildTestIndex(t *testing.T, path string, data []byte, format viewFormat) {
	t.Helper()
	scanner, _, _ := grepTestScanner(t, "", false, false, format, nil, data)
	pool := newGrepSketchPool(format, time.Now(), 3, 0)
	_, indexed, linesAt, err := grepScanBlocks(bytes.NewReader(data), int64(len(data)), scanner, nil, 0, 0, false, pool, func(grepLineRef) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	sketches := pool.finish()
	if len(sketches) != indexed || indexed == 0 {
		t.Fatalf("sketches %d, indexed %d", len(sketches), indexed)
	}
	header := grepIndexHeaderFor(format, grepTestBlock, int64(len(data)), &sketches[0], &sketches[len(sketches)-1], len(sketches), linesAt)
	if err := grepWriteIndex(path, header, nil, 0, sketches); err != nil {
		t.Fatal(err)
	}
}

func grepFullScan(t *testing.T, data []byte, pattern string, regex, ignoreCase bool, format viewFormat, configure func(*grepFilter)) []grepLineRef {
	t.Helper()
	scanner, _, _ := grepTestScanner(t, pattern, regex, ignoreCase, format, configure, data)
	var refs []grepLineRef
	if _, err := scanner.scanRange(bytes.NewReader(data), 0, int64(len(data)), 0, func(ref grepLineRef) bool {
		refs = append(refs, grepLineRef{ref.offset, ref.number, bytes.Clone(ref.text)})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return refs
}

func grepIndexedScan(t *testing.T, indexPath string, data []byte, pattern string, regex, ignoreCase bool, format viewFormat, configure func(*grepFilter), pool *grepSketchPool) ([]grepLineRef, *grepIndexedSearch, []grepSketch, int64, error) {
	t.Helper()
	index, err := grepOpenIndex(indexPath, grepTestBlock, format)
	if err != nil {
		t.Fatal(err)
	}
	defer index.close()
	reader := bytes.NewReader(data)
	if err := grepIndexMatches(index, reader, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	scanner, matcher, filter := grepTestScanner(t, pattern, regex, ignoreCase, format, configure, data)
	search := &grepIndexedSearch{index: index, file: reader, size: int64(len(data)), scanner: scanner, filter: filter, query: newGrepIndexQuery(matcher, ignoreCase, filter), numbers: true}
	if !filter.active() {
		search.filter = nil
	}
	var refs []grepLineRef
	fresh, linesAt, err := search.run(pool, func(ref grepLineRef) bool {
		refs = append(refs, grepLineRef{ref.offset, ref.number, bytes.Clone(ref.text)})
		return true
	})
	return refs, search, fresh, linesAt, err
}

func TestGrepReadBlocksCoversEveryLineOnce(t *testing.T) {
	data := grepFixture(1000)
	for _, block := range []int{7, 64, 777, 4096} {
		var joined []byte
		var regions []grepBlockRegion
		err := grepReadBlocks(bytes.NewReader(data), int64(len(data)), block, 1<<20, 0, 0, func(region grepBlockRegion) error {
			joined = append(joined, region.data...)
			regions = append(regions, grepBlockRegion{index: region.index, start: region.start, lines: region.lines, complete: region.complete, data: bytes.Clone(region.data), raw: bytes.Clone(region.raw)})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(joined, data) {
			t.Fatalf("block %d: regions join to %d bytes, want %d", block, len(joined), len(data))
		}
		for position, region := range regions {
			if region.index != position {
				t.Fatalf("block %d: region %d has index %d", block, position, region.index)
			}
			blockStart := int64(position) * int64(block)
			if want := int64(bytes.Count(data[:blockStart], []byte{'\n'})); region.lines != want {
				t.Fatalf("block %d: region %d counts %d lines before it, want %d", block, position, region.lines, want)
			}
			if region.complete && (len(region.data) != 0 && region.data[len(region.data)-1] != '\n' || blockStart+int64(block) > int64(len(data))) {
				t.Fatalf("block %d: region %d is complete but %q / full %v", block, position, region.data[max(0, len(region.data)-10):], blockStart+int64(block) <= int64(len(data)))
			}
			if want := data[blockStart:min(blockStart+int64(block), int64(len(data)))]; !bytes.Equal(region.raw, want) {
				t.Fatalf("block %d: region %d raw mismatch", block, position)
			}
		}
		// 줄바꿈 없는 마지막 줄이 든 블록은 complete가 아니다.
		if last := regions[len(regions)-1]; last.complete {
			t.Fatalf("block %d: the last region must not be complete", block)
		}
	}
}

func TestGrepIndexSearchEqualsFullScan(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	level := grepLevelLog(12000, start, 5)
	access := grepTimedAccessLog(20000, start)
	dir := t.TempDir()
	levelIndex, accessIndex := filepath.Join(dir, "level.idx"), filepath.Join(dir, "access.idx")
	grepBuildTestIndex(t, levelIndex, level, viewFormatLevel)
	grepBuildTestIndex(t, accessIndex, access, viewFormatAccess)
	// 로그는 줄마다 0~3초씩 늘어 약 5시간 분량이다. 가운데 시각으로 앞뒤를 가른다.
	levelMid := start.Add(150 * time.Minute)
	for _, scenario := range []struct {
		name       string
		index      string
		data       []byte
		format     viewFormat
		pattern    string
		regex      bool
		ignoreCase bool
		configure  func(*grepFilter)
		// prunes는 읽은 블록 수의 기대다. 0은 전부, 1은 전부보다 적게, 2는 절반 미만이다.
		prunes int
	}{
		{"rare literal", levelIndex, level, viewFormatLevel, "refunded", false, false, nil, 2},
		{"digits only read every block", levelIndex, level, viewFormatLevel, "88", false, false, nil, 0},
		{"ignore case", levelIndex, level, viewFormatLevel, "REFUNDED", false, true, nil, 2},
		{"regex with prefix", levelIndex, level, viewFormatLevel, `c\.e\.Api - (payment|slow) `, true, false, nil, 0},
		{"level error keeps traces across blocks", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }, 0},
		{"level error with trace pattern", levelIndex, level, viewFormatLevel, "Db.java:7", false, false, func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }, 0},
		{"level warn", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) { filter.minLevel = grepLevelRank("warn") }, 0},
		{"since", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) { filter.since = levelMid }, 0},
		{"until", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) { filter.until = levelMid }, 0},
		{"since and until with pattern", levelIndex, level, viewFormatLevel, "request", false, false, func(filter *grepFilter) {
			filter.since, filter.until = levelMid, levelMid.Add(30*time.Minute)
		}, 2},
		{"level error since", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) {
			filter.minLevel, filter.since = grepLevelRank("error"), levelMid
		}, 0},
		// 끝이나 처음 가까이만 남으면 통과하는 블록이 20% 아래라 index로 건너뛴다.
		{"since near the end", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) { filter.since = start.Add(4*time.Hour + 45*time.Minute) }, 2},
		{"until near the start", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) { filter.until = start.Add(15 * time.Minute) }, 2},
		{"level error since near the end", levelIndex, level, viewFormatLevel, "", false, false, func(filter *grepFilter) {
			filter.minLevel, filter.since = grepLevelRank("error"), start.Add(4*time.Hour+45*time.Minute)
		}, 2},
		{"access status", accessIndex, access, viewFormatAccess, "", false, false, func(filter *grepFilter) { set, _ := parseGrepStatuses("5xx"); filter.setStatuses(set) }, 0},
		{"access pattern since", accessIndex, access, viewFormatAccess, "503", false, false, func(filter *grepFilter) { filter.since = start.Add(5 * time.Hour) }, 2},
		{"access status not present", accessIndex, access, viewFormatAccess, "", false, false, func(filter *grepFilter) { set, _ := parseGrepStatuses("4xx"); filter.setStatuses(set) }, 2},
	} {
		want := grepFullScan(t, scenario.data, scenario.pattern, scenario.regex, scenario.ignoreCase, scenario.format, scenario.configure)
		got, search, _, _, err := grepIndexedScan(t, scenario.index, scenario.data, scenario.pattern, scenario.regex, scenario.ignoreCase, scenario.format, scenario.configure, nil)
		if err != nil {
			t.Fatalf("%s: %v", scenario.name, err)
		}
		grepRefsEqual(t, scenario.name, got, want, true)
		total := int(search.index.header.Blocks)
		switch scenario.prunes {
		case 0:
			if search.blocksRead != total {
				t.Errorf("%s: read %d of %d blocks, expected every block", scenario.name, search.blocksRead, total)
			}
		case 1:
			if search.blocksRead >= total {
				t.Errorf("%s: read every one of %d blocks", scenario.name, total)
			}
		case 2:
			if search.blocksRead*2 > total {
				t.Errorf("%s: read %d of %d blocks", scenario.name, search.blocksRead, total)
			}
		}
		if scenario.name == "rare literal" && len(want) == 0 {
			t.Fatal("fixture has no rare literal")
		}
	}
}

// 파일이 늘어나면 index 밖의 꼬리를 훑고, 그 부분의 sketch로 index를 늘린다. 늘린 index도 전체 훑기와 같다.
func TestGrepIndexExtendsAfterAppend(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	first := grepLevelLog(4000, start, 9)
	path := filepath.Join(t.TempDir(), "level.idx")
	grepBuildTestIndex(t, path, first, viewFormatLevel)
	grown := append(bytes.Clone(first), grepLevelLog(4000, start.Add(6*time.Hour), 10)...)
	configure := func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }
	want := grepFullScan(t, grown, "", false, false, viewFormatLevel, configure)
	pool := newGrepSketchPool(viewFormatLevel, time.Now(), 2, 0)
	index, _ := grepOpenIndex(path, grepTestBlock, viewFormatLevel)
	pool.first = int(index.header.Blocks)
	before := index.header
	index.close()
	got, search, fresh, linesAt, err := grepIndexedScan(t, path, grown, "", false, false, viewFormatLevel, configure, pool)
	if err != nil {
		t.Fatal(err)
	}
	grepRefsEqual(t, "grown file", got, want, true)
	if len(fresh) == 0 || search.blocksRead > int(before.Blocks) {
		t.Fatalf("fresh %d, read %d of %d indexed blocks", len(fresh), search.blocksRead, before.Blocks)
	}
	index, _ = grepOpenIndex(path, grepTestBlock, viewFormatLevel)
	header := grepIndexHeaderFor(viewFormatLevel, grepTestBlock, int64(len(grown)), nil, &fresh[len(fresh)-1], int(before.Blocks)+len(fresh), linesAt)
	header.FirstCRC = before.FirstCRC
	if err := grepWriteIndex(path, header, index.records(), int(before.Blocks), fresh); err != nil {
		t.Fatal(err)
	}
	index.close()
	again, search, more, _, err := grepIndexedScan(t, path, grown, "refunded", false, false, viewFormatLevel, nil, nil)
	if err != nil || len(more) != 0 {
		t.Fatal(err, more)
	}
	grepRefsEqual(t, "extended index", again, grepFullScan(t, grown, "refunded", false, false, viewFormatLevel, nil), true)
	if search.index.header.Blocks != before.Blocks+uint32(len(fresh)) || search.index.header.Indexed <= before.Indexed {
		t.Fatalf("header %+v, before %+v", search.index.header, before)
	}
}

func TestGrepIndexRejectsAChangedFile(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	data := grepLevelLog(4000, start, 21)
	path := filepath.Join(t.TempDir(), "level.idx")
	grepBuildTestIndex(t, path, data, viewFormatLevel)
	open := func() *grepIndexFile {
		index, err := grepOpenIndex(path, grepTestBlock, viewFormatLevel)
		if err != nil {
			t.Fatal(err)
		}
		return index
	}
	index := open()
	defer index.close()
	shrunk := data[:len(data)/2]
	if err := grepIndexMatches(index, bytes.NewReader(shrunk), int64(len(shrunk))); err == nil {
		t.Fatal("a shrunk file must not match")
	}
	rewritten := bytes.Clone(data)
	copy(rewritten[10:], "XXXX")
	if err := grepIndexMatches(index, bytes.NewReader(rewritten), int64(len(rewritten))); err == nil {
		t.Fatal("a changed first block must not match")
	}
	lastIndexed := bytes.Clone(data)
	copy(lastIndexed[index.header.Indexed-100:], "XXXX")
	if err := grepIndexMatches(index, bytes.NewReader(lastIndexed), int64(len(lastIndexed))); err == nil {
		t.Fatal("a changed last indexed block must not match")
	}
	// 가운데 블록만 바뀐 파일은 열 때는 모르지만, 그 블록을 읽을 때 checksum으로 알아챈다. 패턴은 블록의 20% 미만에서만
	// 맞아야 index로 건너뛰는 경로를 탄다.
	middle := bytes.Clone(data)
	at := len(data)/2 + bytes.Index(data[len(data)/2:], []byte("refunded"))
	copy(middle[at+len("refunded")+1:], "XXXX")
	if err := grepIndexMatches(index, bytes.NewReader(middle), int64(len(middle))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := grepIndexedScan(t, path, middle, "refunded", false, false, viewFormatLevel, nil, nil); err == nil || err.Error() != errGrepIndexStale.Error() {
		t.Fatalf("a changed middle block must stop the search: %v", err)
	}
}

func TestGrepIndexFileRejectsCorruption(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	data := grepLevelLog(2000, start, 2)
	path := filepath.Join(t.TempDir(), "level.idx")
	grepBuildTestIndex(t, path, data, viewFormatLevel)
	if _, err := grepOpenIndex(path, grepTestBlock*2, viewFormatLevel); err == nil {
		t.Fatal("another block size must not open")
	}
	if _, err := grepOpenIndex(path, grepTestBlock, viewFormatAccess); err == nil {
		t.Fatal("another format must not open")
	}
	raw, _ := os.ReadFile(path)
	raw[grepIndexHeaderSize+5000] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := grepOpenIndex(path, grepTestBlock, viewFormatLevel); err == nil {
		t.Fatal("a flipped byte must fail the checksum")
	}
}

// 이어지는 줄로 시작하는 블록은 앞 블록의 마지막 머리 줄에서 level과 시각을 물려받아 판단한다.
func TestGrepIndexQueryInheritsTheCarriedEvent(t *testing.T) {
	at := time.Date(2026, 10, 26, 10, 0, 0, 0, time.Local)
	trace := grepSketch{Flags: grepFlagLeadingCont, LastLevel: -1}
	errorQuery := grepIndexQuery{minLevel: grepLevelRank("error"), needsLevel: true}
	if errorQuery.pass(&trace, grepCarried{level: -1}) {
		t.Fatal("no carried event: the block cannot hold an error")
	}
	carried := grepCarried{level: int8(grepLevelRank("error")), at: at.UnixNano(), known: true}
	if !errorQuery.pass(&trace, carried) {
		t.Fatal("the carried error must make the block pass")
	}
	timeQuery := grepIndexQuery{since: at.Add(-time.Minute), until: at.Add(time.Minute), needsTime: true}
	if !timeQuery.pass(&trace, carried) || timeQuery.pass(&trace, grepCarried{level: -1}) {
		t.Fatal("the carried time decides a block without its own time")
	}
	header := grepSketch{Flags: grepFlagHasHeader | 1<<grepLevelRank("info"), LastLevel: int8(grepLevelRank("info")), MinTime: at.UnixNano(), MaxTime: at.UnixNano()}
	if errorQuery.pass(&header, carried) {
		t.Fatal("a block with only info lines and no leading continuation must not pass for error")
	}
	var updated grepCarried
	updated.update(&header)
	if !updated.known || updated.level != int8(grepLevelRank("info")) {
		t.Fatalf("carried %+v", updated)
	}
}

func TestGrepIndexTrimRemovesTheOldest(t *testing.T) {
	dir := t.TempDir()
	for index, name := range []string{"a.idx", "b.idx", "c.idx", "keep.seen"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-time.Duration(10-index) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := grepIndexTrim(dir, 150); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, " ") != "c.idx keep.seen" {
		t.Fatalf("left %v", names)
	}
}

// 블록 reader로 훑어도 scanRange와 같은 줄을 낸다. 상한보다 긴 줄은 둘 다 앞부분만 비교한다.
func TestGrepScanBlocksEqualsScanRange(t *testing.T) {
	const lineLimit = 500
	data := grepFixture(lineLimit)
	for _, block := range []int{64, 4096} {
		for _, pattern := range []string{"cancel", "", " 200"} {
			matcher, _ := newGrepMatcher(pattern, false, false, data[:grepSampleSize/8])
			scanner := newGrepScanner(matcher, nil)
			scanner.block, scanner.lineLimit = block, lineLimit
			want := grepNaiveLines(data, lineLimit, matcher.match)
			var got []grepLineRef
			if _, _, _, err := grepScanBlocks(bytes.NewReader(data), int64(len(data)), scanner, nil, 0, 0, true, nil, func(ref grepLineRef) bool {
				got = append(got, grepLineRef{ref.offset, ref.number, bytes.Clone(ref.text)})
				return true
			}); err != nil {
				t.Fatal(err)
			}
			grepRefsEqual(t, fmt.Sprintf("block %d pattern %q", block, pattern), got, want, true)
		}
	}
}

// 검색이 일찍 끝나도(-l처럼 첫 일치에서 멈춤) sketch worker는 남지 않는다.
func TestGrepIndexedSearchFinishesThePoolOnEarlyStop(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	data := grepLevelLog(3000, start, 41)
	path := filepath.Join(t.TempDir(), "level.idx")
	grepBuildTestIndex(t, path, data, viewFormatLevel)
	index, err := grepOpenIndex(path, grepTestBlock, viewFormatLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer index.close()
	scanner, matcher, _ := grepTestScanner(t, "refunded", false, false, viewFormatLevel, nil, data)
	search := &grepIndexedSearch{index: index, file: bytes.NewReader(data), size: int64(len(data)), scanner: scanner, query: newGrepIndexQuery(matcher, false, nil)}
	pool := newGrepSketchPool(viewFormatLevel, time.Now(), 2, int(index.header.Blocks))
	if _, _, err := search.run(pool, func(grepLineRef) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, open := <-pool.jobs; open {
		t.Fatal("the pool must be closed after the search stops")
	}
	// 다시 불러도 닫힌 채로 같은 결과다.
	if sketches := pool.finish(); len(sketches) != 0 {
		t.Fatalf("finish after an early stop returned %d sketches", len(sketches))
	}
}
