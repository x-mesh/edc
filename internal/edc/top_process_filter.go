package edc

import (
	"errors"
	"strconv"
	"strings"
)

// topFilteredProcessLimit은 필터를 건 sampler가 남기는 process 수다. 필터가 없을 때의 topProcessLimit보다 크게 둬서
// 같은 이름의 worker가 여럿이어도 JSON에 모두 담는다.
const topFilteredProcessLimit = 50

var errTopProcessFilterEmpty = errors.New("empty process filter term")

// topProcessFilter는 쉼표로 나눈 항목 중 하나라도 맞는 process만 남긴다. 숫자는 PID와 정확히 같아야 하고,
// 그 밖의 항목은 대소문자를 가리지 않고 command에 들어 있어야 한다. 값이 없으면 모든 process가 맞는다.
type topProcessFilter struct {
	text  string
	pids  map[int]struct{}
	names []string
}

func parseTopProcessFilter(value string) (topProcessFilter, error) {
	if strings.TrimSpace(value) == "" {
		return topProcessFilter{}, nil
	}
	filter := topProcessFilter{pids: map[int]struct{}{}}
	terms := []string{}
	for _, term := range strings.Split(value, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return topProcessFilter{}, errTopProcessFilterEmpty
		}
		terms = append(terms, term)
		if pid, err := strconv.Atoi(term); err == nil && pid > 0 {
			filter.pids[pid] = struct{}{}
			continue
		}
		filter.names = append(filter.names, strings.ToLower(term))
	}
	filter.text = strings.Join(terms, ",")
	return filter, nil
}

func (filter topProcessFilter) active() bool { return filter.text != "" }

func (filter topProcessFilter) String() string { return filter.text }

func (filter topProcessFilter) match(process topProcess) bool {
	if !filter.active() {
		return true
	}
	if _, ok := filter.pids[process.PID]; ok {
		return true
	}
	command := strings.ToLower(process.Command)
	for _, name := range filter.names {
		if strings.Contains(command, name) {
			return true
		}
	}
	return false
}

func (filter topProcessFilter) apply(processes []topProcess) []topProcess {
	if !filter.active() {
		return processes
	}
	kept := make([]topProcess, 0, len(processes))
	for _, process := range processes {
		if filter.match(process) {
			kept = append(kept, process)
		}
	}
	return kept
}

func (filter topProcessFilter) limit() int {
	if filter.active() {
		return topFilteredProcessLimit
	}
	return topProcessLimit
}
