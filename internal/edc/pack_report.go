package edc

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// packProgressInterval은 -v의 진행 줄을 다시 그리는 간격이다.
const packProgressInterval = 250 * time.Millisecond

// packReporter는 -v의 출력이다. 판단 줄과 항목 줄은 stderr에 한 줄씩 쓰고, stderr가 터미널이면 진행 줄을 한 줄로 고쳐 쓴다.
// 항목 줄을 쓸 때는 진행 줄을 지웠다가 다시 그려 두 출력이 섞이지 않게 한다. -v가 없으면 아무것도 쓰지 않는다.
type packReporter struct {
	mu      sync.Mutex
	out     io.Writer
	verbose bool
	live    bool
	color   bool
	status  string
}

func (reporter *packReporter) enabled() bool { return reporter != nil && reporter.verbose }

// line은 한 줄을 쓴다. 로그와 항목 이름은 호출하는 쪽이 viewSanitize를 거친다.
func (reporter *packReporter) line(text string) {
	if !reporter.enabled() {
		return
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	reporter.erase()
	if reporter.color {
		text = richDim(text)
	}
	fmt.Fprintln(reporter.out, text)
	reporter.draw()
}

func (reporter *packReporter) setStatus(text string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	reporter.erase()
	reporter.status = text
	reporter.draw()
}

func (reporter *packReporter) erase() {
	if reporter.live && reporter.status != "" {
		fmt.Fprint(reporter.out, "\r\033[K")
	}
}

func (reporter *packReporter) draw() {
	if reporter.live && reporter.status != "" {
		text := reporter.status
		if reporter.color {
			text = richDim(text)
		}
		fmt.Fprint(reporter.out, text)
	}
}

// track은 name을 처리하는 동안 진행 줄을 그린다. read는 지금까지 읽은 입력 바이트이고, total이 0보다 크면 비율도 보인다.
// files가 nil이 아니면 묶은 파일 수를 보인다. 돌려준 함수를 부르면 진행 줄을 지우고 멈춘다. 터미널이 아니면 아무것도 하지 않는다.
func (reporter *packReporter) track(name string, total int64, read *atomic.Int64, files *atomic.Int64) func() {
	if !reporter.enabled() || !reporter.live {
		return func() {}
	}
	started := time.Now()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(packProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				reporter.setStatus(packProgressText(name, read.Load(), total, files, time.Since(started)))
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		reporter.setStatus("")
	}
}

func packProgressText(name string, read, total int64, files *atomic.Int64, elapsed time.Duration) string {
	rate := "-"
	if seconds := elapsed.Seconds(); seconds > 0 {
		rate = formatSizeShort(uint64(float64(read)/seconds)) + "/s"
	}
	if files != nil {
		return T("pack.verbose.progress_files", name, files.Load(), formatSizeShort(uint64(read)), rate)
	}
	if total > 0 {
		return T("pack.verbose.progress", name, formatSizeShort(uint64(read)), formatSizeShort(uint64(total)), min(read*100/total, 100), rate)
	}
	return T("pack.verbose.progress_bytes", name, formatSizeShort(uint64(read)), rate)
}

// packCountingReader는 진행 줄에 쓸 바이트 수를 센다. 진행 줄을 그리는 goroutine이 읽으므로 원자적으로 센다.
type packCountingReader struct {
	reader io.Reader
	count  *atomic.Int64
}

func (reader packCountingReader) Read(p []byte) (int, error) {
	n, err := reader.reader.Read(p)
	reader.count.Add(int64(n))
	return n, err
}

// packKindText는 판단 줄에 쓰는 입력의 종류다.
func packKindText(kind packKind) string {
	switch {
	case kind.format != "" && kind.tar:
		return T("pack.verbose.kind_compressed_tar", kind.format)
	case kind.format != "":
		return T("pack.verbose.kind_compressed", kind.format)
	case kind.tar:
		return T("pack.verbose.kind_tar")
	}
	return T("pack.verbose.kind_plain")
}
