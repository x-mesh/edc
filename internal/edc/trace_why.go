package edc

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// trace why는 같은 host에서 함께 기록한 trace 파일들을 읽어, 서버가 받은 HTTP 요청의 지연을 그 요청이 처리되는 동안 같은
// process가 한 하위 호출(DNS 질의, 나가는 HTTP 요청, DB 명령)로 나눈다. kernel 기록에는 요청과 호출을 잇는 표시가 없어서
// PID와 시간으로 잇는다. 한 process가 요청 여러 개를 동시에 처리하면 가릴 수 없는 호출이 생기고, 그런 호출은 어느 요청에도
// 더하지 않는다.

const (
	// traceWhyCallLimit은 경로 하나가 두는 하위 호출 묶음 수의 상한이다. 나가는 요청의 path에 ID가 들어 있으면 호출마다
	// 묶음이 생긴다. 넘은 뒤 처음 보는 이름은 traceGroupOther에 더한다.
	traceWhyCallLimit = 1000
	// traceWhyCallRows는 경로 하나 아래에 쓰는 하위 호출 행 수다. 나머지는 한 행으로 합친다.
	traceWhyCallRows = 5
)

var errTraceWhyNoSharedRange = errors.New("the recordings share no time range")

type traceWhyOptions struct {
	// port가 0이 아니면 로컬 port가 이 값인 서버가 받은 요청만 나눈다.
	port int
}

type traceWhyCall struct {
	Kind  string  `json:"kind"`
	Name  string  `json:"name"`
	Calls uint64  `json:"calls"`
	MS    float64 `json:"ms"`
}

// traceWhyPath는 process와 요청 경로 하나의 결과다. Calls는 이 경로의 요청 하나에만 들어가는 호출이고 시간이 큰 순서다.
// AmbiguousCalls는 이 경로의 요청을 포함해 둘 이상의 요청에 들어가서 어느 요청에도 더하지 않은 호출 수다. AmbiguousMS는
// 요청 하나의 시간 중 그런 호출만 덮는 부분의 평균이고, SelfMS는 어떤 호출도 덮지 않는 부분의 평균이다. 그래서 겹치지 않는
// 호출의 시간, AmbiguousMS, SelfMS를 더하면 LatencyMS다.
type traceWhyPath struct {
	Process        string         `json:"process"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	Requests       uint64         `json:"requests"`
	LatencyMS      float64        `json:"latency_ms"`
	LatencyMaxMS   float64        `json:"latency_max_ms"`
	SelfMS         float64        `json:"self_ms"`
	Calls          []traceWhyCall `json:"calls"`
	AmbiguousCalls uint64         `json:"ambiguous_calls"`
	AmbiguousMS    float64        `json:"ambiguous_ms"`
	// Parallel은 한 요청 안에서 호출이 겹쳐, 호출 시간의 합이 요청 지연을 넘을 수 있다는 표시다.
	Parallel bool `json:"parallel"`
}

type traceWhyLost struct {
	Trace string `json:"trace"`
	Lost  uint64 `json:"lost_events"`
}

type traceWhyReport struct {
	AnalyzedMS     float64        `json:"analyzed_ms"`
	RecordedMS     float64        `json:"recorded_ms"`
	Requests       uint64         `json:"requests"`
	Calls          uint64         `json:"calls"`
	CallMS         float64        `json:"call_ms"`
	AmbiguousCalls uint64         `json:"ambiguous_calls"`
	AmbiguousMS    float64        `json:"ambiguous_ms"`
	OutsideCalls   uint64         `json:"outside_calls"`
	OutsideMS      float64        `json:"outside_ms"`
	Lost           []traceWhyLost `json:"lost,omitempty"`
	Paths          []traceWhyPath `json:"paths"`
}

type traceWhyGroupKey struct{ process, method, path string }

type traceWhyCallKey struct{ kind, name string }

type traceWhyCallSum struct {
	calls uint64
	ns    uint64
}

type traceWhyGroup struct {
	key            traceWhyGroupKey
	requests       uint64
	latencyNS      uint64
	latencyMaxNS   uint64
	coveredNS      uint64
	touchedNS      uint64
	calls          map[traceWhyCallKey]*traceWhyCallSum
	ambiguousCalls uint64
	parallel       bool
}

// traceWhyParent는 서버가 받은 요청 하나다. covered는 이 요청에 붙은 호출 구간의 합집합이고, touched는 거기에 가릴 수 없는
// 호출의 구간까지 더한 합집합이다. 호출을 시작 순서로 넣으므로 어디까지 셌는지만 두면 된다.
type traceWhyParent struct {
	start, end uint64
	group      *traceWhyGroup
	covered    traceWhyUnion
	touched    traceWhyUnion
}

type traceWhyUnion struct {
	until uint64
	ns    uint64
}

// add는 구간을 더하고, 앞서 더한 구간과 겹쳤는지 돌려준다. 이미 센 앞부분은 다시 세지 않는다.
func (union *traceWhyUnion) add(start, end uint64) bool {
	overlapped := start < union.until
	if overlapped {
		start = union.until
	}
	if end > start {
		union.ns += end - start
		union.until = end
	}
	return overlapped
}

type traceWhyChild struct {
	start, end uint64
	key        traceWhyCallKey
}

// traceWhyInterval은 응답 event에서 요청이나 호출의 구간을 되살린다. boot_time_ns는 응답을 본 때이고 latency_ms는 그때까지
// 걸린 시간이다.
func traceWhyInterval(event captureEvent) (start, end uint64, ok bool) {
	if event.LatencyMS == nil || event.BootTimeNS == 0 || *event.LatencyMS < 0 {
		return 0, 0, false
	}
	latency := uint64(*event.LatencyMS * float64(time.Millisecond))
	return event.BootTimeNS - min(event.BootTimeNS, latency), event.BootTimeNS, true
}

// traceWhyCallOf는 event가 하위 호출의 응답이면 그 이름을 돌려준다. DNS는 client 쪽 응답에 side가 없다. HTTP와 DB는 같은
// host의 서버가 본 쪽을 빼야 호출을 두 번 세지 않는다.
func traceWhyCallOf(event captureEvent) (traceWhyCallKey, bool) {
	switch {
	case event.Protocol == "dns" && event.Side == "" && event.Event != traceDNSQueryEvent:
		return traceWhyCallKey{"dns", strings.TrimSpace(event.Target + " " + event.QueryType)}, true
	case event.Side != traceClientSide:
		return traceWhyCallKey{}, false
	case event.Protocol == "http" && event.Status >= 200:
		return traceWhyCallKey{"http", strings.TrimSpace(event.Method + " " + event.Target + event.Path)}, true
	case event.Protocol == "mysql" && event.MySQL != nil:
		return traceWhyCallKey{"mysql", mysqlTraceShape(event.MySQL)}, true
	case event.Protocol == "postgres" && event.Postgres != nil:
		return traceWhyCallKey{"postgres", postgresTraceShape(event.Postgres)}, true
	case event.Protocol == "redis" && event.Redis != nil:
		return traceWhyCallKey{"redis", event.Redis.Command}, true
	}
	return traceWhyCallKey{}, false
}

func traceWhyLocalPort(address string) int {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0
	}
	number, _ := strconv.Atoi(port)
	return number
}

// analyzeTraceWhy는 함께 기록한 파일들로 경로별 결과를 만든다. 호출 수와 요청 밖 호출은 요청을 받은 process의 것만 센다. 파일마다 기록을 시작한 때가 달라서, 모든 파일이 event를
// 받던 구간 안에서 시작하고 끝난 요청과 호출만 센다. 그 구간을 모르는 파일이 있으면 다른 시각으로 대신 맞추지 않고 거부한다.
func analyzeTraceWhy(files []*traceShowFile, options traceWhyOptions) (traceWhyReport, error) {
	var report traceWhyReport
	var from, until, first, last uint64
	hasHTTP := false
	for _, file := range files {
		summary := file.summary
		if summary.StartBootTimeNS == 0 || summary.EndBootTimeNS == 0 {
			return report, fmt.Errorf("the %s recording has no recorded range (trace why needs files that the same edc version recorded on Linux)", file.protocol)
		}
		if from == 0 || summary.StartBootTimeNS > from {
			from = summary.StartBootTimeNS
		}
		if until == 0 || summary.EndBootTimeNS < until {
			until = summary.EndBootTimeNS
		}
		if first == 0 || summary.StartBootTimeNS < first {
			first = summary.StartBootTimeNS
		}
		if summary.EndBootTimeNS > last {
			last = summary.EndBootTimeNS
		}
		if summary.LostEvents != 0 {
			report.Lost = append(report.Lost, traceWhyLost{file.protocol, summary.LostEvents})
		}
		hasHTTP = hasHTTP || file.protocol == "http"
	}
	if !hasHTTP {
		return report, errors.New("no http recording to take the requests from")
	}
	report.RecordedMS = float64(last-first) / float64(time.Millisecond)
	if from >= until {
		return report, errTraceWhyNoSharedRange
	}
	report.AnalyzedMS = float64(until-from) / float64(time.Millisecond)

	groups := map[traceWhyGroupKey]*traceWhyGroup{}
	parents := map[uint32][]*traceWhyParent{}
	children := map[uint32][]traceWhyChild{}
	for _, file := range files {
		for _, event := range file.events {
			start, end, ok := traceWhyInterval(event)
			if !ok || start < from || end > until {
				continue
			}
			if event.Protocol == "http" && event.Side == traceServerSide && event.Status >= 200 {
				if options.port != 0 && traceWhyLocalPort(event.Source) != options.port {
					continue
				}
				key := traceWhyGroupKey{event.Process, event.Method, event.Path}
				group := groups[key]
				if group == nil && len(groups) >= traceGroupLimit {
					key = traceWhyGroupKey{path: traceGroupOther}
					group = groups[key]
				}
				if group == nil {
					group = &traceWhyGroup{key: key, calls: map[traceWhyCallKey]*traceWhyCallSum{}}
					groups[key] = group
				}
				group.requests++
				group.latencyNS += end - start
				if end-start > group.latencyMaxNS {
					group.latencyMaxNS = end - start
				}
				parents[event.PID] = append(parents[event.PID], &traceWhyParent{start: start, end: end, group: group})
				report.Requests++
				continue
			}
			if key, ok := traceWhyCallOf(event); ok {
				children[event.PID] = append(children[event.PID], traceWhyChild{start, end, key})
			}
		}
	}

	var callNS, ambiguousNS, outsideNS uint64
	for pid, calls := range children {
		requests := parents[pid]
		// 고른 요청을 하나도 받지 않은 process의 호출은 세지 않는다. 요청을 보낸 client나 host의 다른 program이 한 호출이라서,
		// 요청 밖 호출로 세면 서버가 요청과 무관하게 한 일처럼 보인다.
		if len(requests) == 0 {
			continue
		}
		slices.SortFunc(calls, func(a, b traceWhyChild) int { return cmp.Compare(a.start, b.start) })
		slices.SortFunc(requests, func(a, b *traceWhyParent) int { return cmp.Compare(a.start, b.start) })
		// active는 지금 호출보다 먼저 시작해 아직 끝나지 않은 요청이다. 호출을 시작 순서로 보므로 한 번 빠진 요청은 다시 볼 일이 없다.
		var active []*traceWhyParent
		next := 0
		for _, call := range calls {
			for next < len(requests) && requests[next].start <= call.start {
				active = append(active, requests[next])
				next++
			}
			active = slices.DeleteFunc(active, func(request *traceWhyParent) bool { return request.end < call.start })
			var owners []*traceWhyParent
			for _, request := range active {
				if request.end >= call.end {
					owners = append(owners, request)
				}
			}
			duration := call.end - call.start
			report.Calls++
			callNS += duration
			switch len(owners) {
			case 0:
				report.OutsideCalls++
				outsideNS += duration
			case 1:
				owner := owners[0]
				group := owner.group
				key := call.key
				sum := group.calls[key]
				if sum == nil && len(group.calls) >= traceWhyCallLimit {
					key = traceWhyCallKey{name: traceGroupOther}
					sum = group.calls[key]
				}
				if sum == nil {
					sum = &traceWhyCallSum{}
					group.calls[key] = sum
				}
				sum.calls++
				sum.ns += duration
				if owner.covered.add(call.start, call.end) {
					group.parallel = true
				}
				owner.touched.add(call.start, call.end)
			default:
				report.AmbiguousCalls++
				ambiguousNS += duration
				var seen []*traceWhyGroup
				for _, owner := range owners {
					owner.touched.add(call.start, call.end)
					if !slices.Contains(seen, owner.group) {
						seen = append(seen, owner.group)
						owner.group.ambiguousCalls++
					}
				}
			}
		}
	}
	for _, requests := range parents {
		for _, request := range requests {
			request.group.coveredNS += request.covered.ns
			request.group.touchedNS += request.touched.ns
		}
	}

	ms := func(ns uint64) float64 { return float64(ns) / float64(time.Millisecond) }
	report.CallMS, report.AmbiguousMS, report.OutsideMS = ms(callNS), ms(ambiguousNS), ms(outsideNS)
	slices.SortFunc(report.Lost, func(a, b traceWhyLost) int { return cmp.Compare(a.Trace, b.Trace) })
	for _, group := range groups {
		path := traceWhyPath{
			Process: group.key.process, Method: group.key.method, Path: group.key.path, Requests: group.requests,
			LatencyMS: ms(group.latencyNS) / float64(group.requests), LatencyMaxMS: ms(group.latencyMaxNS),
			SelfMS:         ms(group.latencyNS-min(group.latencyNS, group.touchedNS)) / float64(group.requests),
			AmbiguousCalls: group.ambiguousCalls, AmbiguousMS: ms(group.touchedNS-group.coveredNS) / float64(group.requests), Parallel: group.parallel,
		}
		for key, sum := range group.calls {
			path.Calls = append(path.Calls, traceWhyCall{key.kind, key.name, sum.calls, ms(sum.ns)})
		}
		slices.SortFunc(path.Calls, func(a, b traceWhyCall) int {
			return cmp.Or(cmp.Compare(b.MS, a.MS), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
		})
		report.Paths = append(report.Paths, path)
	}
	slices.SortFunc(report.Paths, func(a, b traceWhyPath) int {
		return cmp.Or(cmp.Compare(b.LatencyMS, a.LatencyMS), cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Process, b.Process), cmp.Compare(a.Method, b.Method), cmp.Compare(a.Path, b.Path))
	})
	return report, nil
}

func traceWhyShare(part, whole float64) string {
	if whole <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", part/whole*100)
}

// print는 경로마다 한 행을 쓰고 그 아래에 하위 호출을 들여 쓴다. 하위 호출의 AVG는 요청 하나당 그 호출에 쓴 시간이고,
// SHARE는 요청 지연에서 차지하는 비중이다.
func (report traceWhyReport) print(out io.Writer) {
	style := newTraceSummaryStyle(out)
	fmt.Fprintln(out, style.title("HTTP request breakdown", fmt.Sprintf("%.1fs of %.1fs recorded", report.AnalyzedMS/1000, report.RecordedMS/1000)))
	fmt.Fprintln(out)
	pairs := []string{"Requests", fmt.Sprint(report.Requests), "Calls", fmt.Sprint(report.Calls)}
	if report.AmbiguousCalls != 0 {
		pairs = append(pairs, "Ambiguous calls", fmt.Sprintf("%d (%s of call time)", report.AmbiguousCalls, traceWhyShare(report.AmbiguousMS, report.CallMS)))
	}
	if report.OutsideCalls != 0 {
		pairs = append(pairs, "Calls outside requests", fmt.Sprintf("%d (%s of call time)", report.OutsideCalls, traceWhyShare(report.OutsideMS, report.CallMS)))
	}
	for _, lost := range report.Lost {
		pairs = append(pairs, "Lost events", fmt.Sprintf("%d in %s", lost.Lost, lost.Trace))
	}
	style.fields(out, "", pairs...)
	fmt.Fprintln(out)
	if len(report.Paths) == 0 {
		fmt.Fprintln(out, "No HTTP request started and ended in the analyzed range.")
		return
	}
	table := traceTable{titles: []string{"PROCESS", "REQUEST / CALL", "COUNT", "AVG", "MAX", "SHARE"}}
	parallel := false
	for _, path := range report.Paths {
		total := path.LatencyMS * float64(path.Requests)
		table.add(emptyAs(path.Process, "-"), strings.TrimSpace(path.Method+" "+emptyAs(path.Path, "-")), fmt.Sprint(path.Requests),
			fmt.Sprintf("%.1fms", path.LatencyMS), fmt.Sprintf("%.1fms", path.LatencyMaxMS), "")
		call := func(label string, count uint64, sum float64) {
			table.add("", "  "+label, fmt.Sprint(count), fmt.Sprintf("%.1fms", sum/float64(path.Requests)), "", traceWhyShare(sum, total))
		}
		shown := path.Calls[:min(len(path.Calls), traceWhyCallRows)]
		for _, item := range shown {
			call(strings.TrimSpace(item.Kind+" "+item.Name), item.Calls, item.MS)
		}
		if rest := path.Calls[len(shown):]; len(rest) != 0 {
			var count uint64
			var sum float64
			for _, item := range rest {
				count += item.Calls
				sum += item.MS
			}
			call(fmt.Sprintf("%d more calls", len(rest)), count, sum)
		}
		if path.AmbiguousCalls != 0 {
			table.add("", "  (ambiguous)", fmt.Sprint(path.AmbiguousCalls), fmt.Sprintf("%.1fms", path.AmbiguousMS), "", traceWhyShare(path.AmbiguousMS, path.LatencyMS))
		}
		self := "self"
		if path.Parallel {
			self, parallel = "self *", true
		}
		table.add("", "  "+self, "", fmt.Sprintf("%.1fms", path.SelfMS), "", traceWhyShare(path.SelfMS, path.LatencyMS))
	}
	style.writeTable(out, table)
	if report.AmbiguousCalls != 0 {
		fmt.Fprintln(out, style.hint("ambiguous: request time that only calls of an unknown request cover. The process handled several requests at that time, so edc cannot tell which request made those calls."))
	}
	if parallel {
		fmt.Fprintln(out, style.hint("*: calls of one request ran at the same time, so the shares of the calls can add up to more than 100%."))
	}
}
