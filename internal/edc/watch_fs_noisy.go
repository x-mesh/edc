package edc

import (
	"path"
	"path/filepath"
	"strings"
	"time"
)

// 시끄러운 디렉터리를 알리는 기준이다. 처음 정한 값이고 실제 사용에서 조정한다.
const (
	// fsWatchNoisyWindow 동안 한 디렉터리에서 fsWatchNoisyEvents건 이상, 그리고 그 동안 전체의 fsWatchNoisyShare 이상이 나오면 알린다.
	fsWatchNoisyWindow = 10 * time.Second
	fsWatchNoisyEvents = 30
	fsWatchNoisyShare  = 0.5
	// fsWatchNoisyLimit은 한 번 실행에서 알리는 디렉터리의 수다. 같은 디렉터리는 한 번만 알린다.
	fsWatchNoisyLimit = 3
	// fsWatchNoisyQueueMax는 창 안에서 기억하는 이벤트의 수다. 초당 수만 건이 쏟아져도 메모리가 늘지 않게 오래된 것부터 버린다.
	fsWatchNoisyQueueMax = 100000
)

type fsWatchNoisyEvent struct {
	directory string
	at        time.Time
}

// fsWatchNoisy는 이벤트가 한 디렉터리에 몰리는지 본다. 몰리면 그 디렉터리를 --exclude로 빼는 방법을 알린다. 한 goroutine에서만 쓴다.
type fsWatchNoisy struct {
	queue  []fsWatchNoisyEvent
	counts map[string]int
	hinted map[string]bool
}

func newFSWatchNoisy() *fsWatchNoisy {
	return &fsWatchNoisy{counts: map[string]int{}, hinted: map[string]bool{}}
}

// fsWatchNoisyHint는 알릴 디렉터리와 그것을 빼는 --exclude 패턴이다.
type fsWatchNoisyHint struct {
	Directory string
	Events    int
	Pattern   string
}

// add는 감시 루트 기준 경로의 이벤트 하나를 센다. 알릴 때가 되면 그 내용을, 아니면 nil을 돌려준다.
func (noisy *fsWatchNoisy) add(eventPath string, now time.Time) *fsWatchNoisyHint {
	directory := path.Dir(filepath.ToSlash(eventPath))
	noisy.queue = append(noisy.queue, fsWatchNoisyEvent{directory: directory, at: now})
	noisy.counts[directory]++
	for len(noisy.queue) > 0 && (len(noisy.queue) > fsWatchNoisyQueueMax || now.Sub(noisy.queue[0].at) >= fsWatchNoisyWindow) {
		oldest := noisy.queue[0]
		noisy.queue = noisy.queue[1:]
		if noisy.counts[oldest.directory]--; noisy.counts[oldest.directory] <= 0 {
			delete(noisy.counts, oldest.directory)
		}
	}
	count := noisy.counts[directory]
	if count < fsWatchNoisyEvents || float64(count) < fsWatchNoisyShare*float64(len(noisy.queue)) {
		return nil
	}
	if noisy.hinted[directory] || len(noisy.hinted) >= fsWatchNoisyLimit || !fsWatchNoisyExcludable(directory) {
		return nil
	}
	noisy.hinted[directory] = true
	return &fsWatchNoisyHint{Directory: directory, Events: count, Pattern: directory + "/**"}
}

// fsWatchNoisyExcludable은 --exclude로 안전하게 뺄 수 있는 디렉터리인지 본다. 감시 루트 자체는 빼면 아무것도 보이지 않고, glob
// 문자가 든 이름은 패턴이 다른 경로에 맞거나 맞지 않을 수 있어서 알리지 않는다.
func fsWatchNoisyExcludable(directory string) bool {
	return directory != "." && directory != "" && !strings.ContainsAny(directory, `*?[\`)
}

// fsWatchShellQuote는 셸에 그대로 붙여 넣을 수 있게 작은따옴표로 감싼다.
func fsWatchShellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}
