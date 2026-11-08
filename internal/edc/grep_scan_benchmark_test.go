package edc

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// grepBenchPatterns에서 session과 kernel에는 유니코드 대소문자 접기에서 ASCII 밖 글자와도 같은 s, k가 들어 있다.
var grepBenchPatterns = []string{"timeout", "error", "panic", "session", "kernel"}

// grepBenchLog는 시각, 수준, worker와 요청·오류 메시지가 섞인 애플리케이션 로그다. timeout은 소문자 줄과 대문자 줄이
// 모두 있고, panic과 kernel은 어느 줄에도 없다.
func grepBenchLog(size int) []byte {
	random := rand.New(rand.NewSource(11))
	levels := []string{"INFO", "INFO", "INFO", "INFO", "INFO", "INFO", "INFO", "DEBUG", "DEBUG", "WARN", "ERROR"}
	methods := []string{"GET", "GET", "GET", "POST", "PUT", "DELETE"}
	resources := []string{"users", "orders", "items", "carts", "payments"}
	var buffer bytes.Buffer
	buffer.Grow(size + 512)
	for buffer.Len() < size {
		fmt.Fprintf(&buffer, "2026-11-05T%02d:%02d:%02d.%03dZ %-5s [worker-%d] ", random.Intn(24), random.Intn(60), random.Intn(60), random.Intn(1000), levels[random.Intn(len(levels))], random.Intn(16))
		switch random.Intn(40) {
		case 0, 1:
			fmt.Fprintf(&buffer, "upstream timeout after %dms host=10.0.%d.%d\n", random.Intn(30000), random.Intn(256), random.Intn(256))
		case 2:
			fmt.Fprintf(&buffer, "Request TIMEOUT for /api/orders/%d\n", random.Intn(100000))
		case 3, 4:
			fmt.Fprintf(&buffer, "error reading body: unexpected EOF after %d bytes\n", random.Intn(1<<20))
		case 5:
			fmt.Fprintf(&buffer, "session %08x expired for user %d\n", random.Uint32(), random.Intn(100000))
		default:
			fmt.Fprintf(&buffer, "%s /api/%s/%d %d %dms bytes=%d agent=\"Mozilla/5.0 (X11; Linux x86_64)\"\n", methods[random.Intn(len(methods))], resources[random.Intn(len(resources))], random.Intn(100000), 200+random.Intn(4)*100, random.Intn(900), random.Intn(1<<16))
		}
	}
	return buffer.Bytes()
}

// BenchmarkGrepFind는 후보 자리를 찾는 find만 잰다. 검색은 1 MiB 블록을 버퍼에 읽은 뒤 훑으므로 캐시에 올라온 블록
// 하나를 쓴다.
func BenchmarkGrepFind(b *testing.B) {
	data := grepBenchLog(grepBlockSize)
	for _, pattern := range grepBenchPatterns {
		for _, mode := range []struct {
			name       string
			ignoreCase bool
		}{{"case", false}, {"i", true}} {
			matcher, err := newGrepMatcher(pattern, false, mode.ignoreCase, data[:grepSampleSize])
			if err != nil {
				b.Fatal(err)
			}
			if mode.ignoreCase && !matcher.fold {
				continue
			}
			b.Run(pattern+"/"+mode.name, func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				candidates := 0
				for b.Loop() {
					candidates = 0
					for position := 0; position < len(data); {
						at := matcher.find(data[position:])
						if at < 0 {
							break
						}
						candidates++
						position += at + 1
					}
				}
				b.ReportMetric(float64(candidates), "hits")
			})
		}
	}
}

// BenchmarkGrepScan은 페이지 캐시에 있는 파일을 찾는 경우처럼 메모리에서 블록을 읽어 줄을 확정하기까지 잰다. i-regex는
// ASCII 접기를 쓰지 못하는 -i 패턴이 지나는 정규식 경로다.
func BenchmarkGrepScan(b *testing.B) {
	data := grepBenchLog(32 << 20)
	reader := bytes.NewReader(data)
	for _, pattern := range grepBenchPatterns {
		for _, mode := range []struct {
			name              string
			regex, ignoreCase bool
		}{{"case", false, false}, {"i", false, true}, {"i-regex", true, true}} {
			matcher, err := newGrepMatcher(pattern, mode.regex, mode.ignoreCase, data[:grepSampleSize])
			if err != nil {
				b.Fatal(err)
			}
			scanner := newGrepScanner(matcher, nil)
			b.Run(pattern+"/"+mode.name, func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				lines := 0
				for b.Loop() {
					lines = 0
					if _, err := scanner.scanRange(reader, 0, int64(len(data)), -1, func(grepLineRef) bool {
						lines++
						return true
					}); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(lines), "lines")
			})
		}
	}
}
