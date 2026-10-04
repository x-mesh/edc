//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type process_stats topProcessEvents top_process_bpf.c -- -I./bpf

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// topBPFWatchedMax는 top_process_bpf.c의 WATCHED_MAX다. 이보다 많이 맞아도 CPU 상위 이 개수까지만 감시한다.
const topBPFWatchedMax = 4096

var topBPFTracepoints = []string{"sched_wakeup", "sched_wakeup_new", "sched_switch", "sched_process_exit", "block_rq_issue", "block_rq_requeue", "block_rq_complete"}

// topProcessBPFPrerequisites는 kernel이 이 program을 지원하는지 먼저 보고, 그다음 권한을 본다.
func topProcessBPFPrerequisites() error {
	return traceBPFPrerequisites("top --detail", topProcessKernelSupported)
}

func topProcessKernelSupported(spec *btf.Spec) error {
	if err := traceTracepointsAvailable(spec, topBPFTracepoints); err != nil {
		return err
	}
	// tp_btf는 인자 수만큼만 읽을 수 있다. 이 program은 sched_switch의 prev와 next, block_rq_complete의 nr_bytes를 읽는다.
	// sched_switch의 prev_state는 5.15에 없으므로, 없으면 top_switch_legacy가 task의 상태 field를 대신 읽는다.
	for _, required := range []struct {
		tracepoint string
		params     int
	}{{"sched_switch", 4}, {"block_rq_complete", 4}} {
		count, err := tracepointParams(spec, required.tracepoint)
		if err != nil {
			return err
		}
		if count < required.params {
			return fmt.Errorf("kernel tracepoint %s has %d arguments, need %d", required.tracepoint, count-1, required.params-1)
		}
	}
	for _, required := range []struct{ name, member string }{{"task_struct", "signal"}, {"signal_struct", "pids"}, {"pid", "numbers"}, {"pid_namespace", "ns"}, {"request", "__data_len"}} {
		if err := btfStructHasMember(spec, required.name, required.member); err != nil {
			return err
		}
	}
	if legacy, err := topSchedSwitchLegacy(spec); err != nil || !legacy {
		return err
	}
	if btfStructHasMember(spec, "task_struct", "__state") != nil && btfStructHasMember(spec, "task_struct", "state") != nil {
		return errors.New("kernel BTF has no field task_struct.__state")
	}
	return nil
}

// topSchedSwitchLegacy는 sched_switch가 prev_state를 넘기지 않는 kernel인지다. 첫 인자는 tp_btf가 쓰는 void *이고,
// preempt, prev, next 뒤에 prev_state가 오면 인자는 다섯이다.
func topSchedSwitchLegacy(spec *btf.Spec) (bool, error) {
	count, err := tracepointParams(spec, "sched_switch")
	if err != nil {
		return false, err
	}
	return count < 5, nil
}

// btfStructHasMember는 이름이 같은 struct가 둘 이상인 kernel에서 하나라도 member를 가지면 통과한다.
func btfStructHasMember(spec *btf.Spec, name, member string) error {
	types, err := spec.AnyTypesByName(name)
	if err != nil {
		return fmt.Errorf("kernel BTF has no type %s", name)
	}
	for _, typ := range types {
		if value, ok := typ.(*btf.Struct); ok && btfHasMember(value, member) {
			return nil
		}
	}
	return fmt.Errorf("kernel BTF has no field %s.%s", name, member)
}

// tracepointParams는 btf_trace_<name>이 가리키는 함수 원형의 인자 수다. 첫 인자는 tp_btf가 쓰는 void *이다.
func tracepointParams(spec *btf.Spec, name string) (int, error) {
	var typedef *btf.Typedef
	if err := spec.TypeByName("btf_trace_"+name, &typedef); err != nil {
		return 0, fmt.Errorf("kernel BTF has no tracepoint %s", name)
	}
	pointer, ok := typedef.Type.(*btf.Pointer)
	if !ok {
		return 0, fmt.Errorf("kernel BTF tracepoint %s has an unexpected type", name)
	}
	proto, ok := pointer.Target.(*btf.FuncProto)
	if !ok {
		return 0, fmt.Errorf("kernel BTF tracepoint %s has an unexpected type", name)
	}
	return len(proto.Params), nil
}

// pidNamespaceInode는 edc가 속한 PID namespace의 inode다. /proc의 pid는 이 namespace의 값이고, BPF가 보는 pid는 init namespace의 값이라
// 컨테이너에서는 서로 다르다. BPF가 이 inode로 pid를 바꿔 비교한다.
func pidNamespaceInode() (uint32, error) {
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return 0, fmt.Errorf("read PID namespace: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("read PID namespace: no inode")
	}
	return uint32(stat.Ino), nil
}

type topProcessBPF struct {
	objects  topProcessEventsObjects
	links    []link.Link
	watched  map[int]struct{}
	previous map[int]topBPFStats
	at       time.Time
}

// startTopProcessBPF는 감시 pid가 없는 채로 program을 붙인다. 돌려주는 observer로 pid를 정하고, close로 모두 뗀다.
func startTopProcessBPF() (topBPFObserver, func(), error) {
	if err := topProcessBPFPrerequisites(); err != nil {
		return nil, nil, err
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, nil, fmt.Errorf("remove memlock limit: %w", err)
	}
	namespace, err := pidNamespaceInode()
	if err != nil {
		return nil, nil, err
	}
	spec, err := loadTopProcessEvents()
	if err != nil {
		return nil, nil, fmt.Errorf("load eBPF objects: %w", err)
	}
	var variables topProcessEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return nil, nil, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := variables.TargetNsInum.Set(namespace); err != nil {
		return nil, nil, fmt.Errorf("set PID namespace: %w", err)
	}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return nil, nil, fmt.Errorf("read kernel BTF: %w", err)
	}
	legacy, err := topSchedSwitchLegacy(kernel)
	if err != nil {
		return nil, nil, err
	}
	// 고르지 않은 program도 불러오면 verifier가 없는 인자를 읽는다고 거부하므로, 고른 program을 두 이름에 모두 넣는다.
	selected := "top_switch"
	if legacy {
		selected = "top_switch_legacy"
	}
	switchSpec := spec.Programs[selected]
	if switchSpec == nil {
		return nil, nil, fmt.Errorf("missing eBPF program %s", selected)
	}
	for _, name := range []string{"top_switch", "top_switch_legacy"} {
		spec.Programs[name] = switchSpec.Copy()
		spec.Programs[name].Name = name
	}
	tracer := &topProcessBPF{watched: map[int]struct{}{}, previous: map[int]topBPFStats{}}
	if err := spec.LoadAndAssign(&tracer.objects, nil); err != nil {
		return nil, nil, fmt.Errorf("load eBPF objects: %w", err)
	}
	for _, attachment := range []struct {
		name    string
		program *ebpf.Program
	}{
		{"sched_wakeup", tracer.objects.TopWakeup}, {"sched_wakeup_new", tracer.objects.TopWakeupNew}, {"sched_switch", tracer.objects.TopSwitch},
		{"sched_process_exit", tracer.objects.TopExit}, {"block_rq_issue", tracer.objects.TopIssue}, {"block_rq_requeue", tracer.objects.TopRequeue},
		{"block_rq_complete", tracer.objects.TopComplete},
	} {
		attached, err := link.AttachTracing(link.TracingOptions{Program: attachment.program, AttachType: ebpf.AttachTraceRawTp})
		if err != nil {
			tracer.close()
			return nil, nil, fmt.Errorf("attach tp_btf/%s: %w", attachment.name, err)
		}
		tracer.links = append(tracer.links, attached)
	}
	return tracer.observe, tracer.close, nil
}

func (tracer *topProcessBPF) close() {
	closeCaptureLinks(tracer.links)
	tracer.links = nil
	_ = tracer.objects.Close()
}

// observe는 pids만 감시하게 맞추고 pid마다 직전 관측 이후 센 값을 돌려준다. 이번에 새로 감시한 pid는 기준이 없어 다음 관측부터 나온다.
func (tracer *topProcessBPF) observe(pids []int) map[int]topBPFStats {
	now := time.Now()
	window := now.Sub(tracer.at)
	want := make(map[int]struct{}, min(len(pids), topBPFWatchedMax))
	for _, pid := range pids[:min(len(pids), topBPFWatchedMax)] {
		want[pid] = struct{}{}
	}
	observed := make(map[int]topBPFStats, len(tracer.watched))
	for pid := range tracer.watched {
		if _, keep := want[pid]; !keep {
			key := uint32(pid)
			_ = tracer.objects.Watched.Delete(key)
			_ = tracer.objects.Stats.Delete(key)
			delete(tracer.watched, pid)
			delete(tracer.previous, pid)
			continue
		}
		cumulative, err := tracer.read(pid)
		if err != nil {
			continue
		}
		delta := cumulative.sub(tracer.previous[pid])
		delta.Window = window
		tracer.previous[pid] = cumulative
		observed[pid] = delta
	}
	for pid := range want {
		if _, ok := tracer.watched[pid]; ok {
			continue
		}
		if err := tracer.objects.Watched.Put(uint32(pid), uint8(1)); err != nil {
			continue
		}
		tracer.watched[pid] = struct{}{}
		tracer.previous[pid] = topBPFStats{}
	}
	tracer.at = now
	return observed
}

// read는 pid의 누적값을 모든 CPU에 걸쳐 더한다. 아직 이벤트가 없으면 0이다.
func (tracer *topProcessBPF) read(pid int) (topBPFStats, error) {
	var perCPU []topProcessEventsProcessStats
	if err := tracer.objects.Stats.Lookup(uint32(pid), &perCPU); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return topBPFStats{}, nil
		}
		return topBPFStats{}, err
	}
	var total topBPFStats
	for _, cpu := range perCPU {
		total.RunqCount += cpu.RunqCount
		total.RunqSumNS += cpu.RunqSumNs
		total.IOCount += cpu.IoCount
		total.IOBytes += cpu.IoBytes
		total.IOSumNS += cpu.IoSumNs
		for index := range total.RunqHist {
			total.RunqHist[index] += cpu.RunqHist[index]
			total.IOHist[index] += cpu.IoHist[index]
		}
	}
	return total, nil
}
