//go:build ignore

typedef unsigned char __u8;
typedef unsigned short __u16;
typedef unsigned int __u32;
typedef unsigned long long __u64;
typedef signed int __s32;
typedef signed long long __s64;
typedef __u16 __be16;
typedef __u32 __be32;
typedef __u32 __wsum;

#include "bpf_helpers.h"
#include "bpf_core_read.h"

#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_PERCPU_HASH 5
#define BPF_MAP_TYPE_LRU_HASH 9
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_NOEXIST 1
#define WATCHED_MAX 4096
#define PENDING_MAX 65536
#define STATS_MAX 4096
#define HIST_BUCKETS 24
#define TASK_RUNNING 0

#define PIDTYPE_TGID 1
#define PID_LEVELS 8

struct ns_common { __u32 inum; } __attribute__((preserve_access_index));
struct pid_namespace { struct ns_common ns; } __attribute__((preserve_access_index));
struct upid { int nr; struct pid_namespace *ns; } __attribute__((preserve_access_index));
struct pid { unsigned int level; struct upid numbers[]; } __attribute__((preserve_access_index));
struct signal_struct { struct pid *pids[4]; } __attribute__((preserve_access_index));
struct task_struct { int pid; struct signal_struct *signal; unsigned int __state; } __attribute__((preserve_access_index));
// 5.14 전 kernel은 같은 값을 state라는 이름으로 둔다.
struct task_struct___pre514 { long state; } __attribute__((preserve_access_index));
struct request { __u32 __data_len; } __attribute__((preserve_access_index));

// bucket i는 [2^i, 2^(i+1)) 마이크로초다. 누적 count의 차이로 구간별 분포를 구한다.
struct process_stats {
	__u64 runq_count; __u64 runq_sum_ns; __u64 runq_hist[HIST_BUCKETS];
	__u64 io_count; __u64 io_bytes; __u64 io_sum_ns; __u64 io_hist[HIST_BUCKETS];
};
const struct process_stats *unused_process_stats __attribute__((unused));
struct io_start { __u64 start_ns; __u64 bytes; __u32 tgid; __u32 pad; };

// edc가 속한 PID namespace의 inode다. 감시하는 pid는 /proc에서 읽은 그 namespace의 값이므로 BPF가 보는 pid를 이 값으로 바꿔 비교한다.
volatile const __u32 target_ns_inum = 0;

struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, WATCHED_MAX); __type(key, __u32); __type(value, __u8); } watched SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_LRU_HASH); __uint(max_entries, PENDING_MAX); __type(key, __u32); __type(value, __u64); } runq_start SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_LRU_HASH); __uint(max_entries, PENDING_MAX); __type(key, void *); __type(value, struct io_start); } io_pending SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, STATS_MAX); __type(key, __u32); __type(value, struct process_stats); } stats SEC(".maps");

static __always_inline __u32 log2_bucket(__u64 microseconds) {
	__u64 value = microseconds + 1;
	__u32 result = (value > 0xFFFFFFFFULL) << 5; value >>= result;
	__u32 shift = (value > 0xFFFF) << 4; value >>= shift; result |= shift;
	shift = (value > 0xFF) << 3; value >>= shift; result |= shift;
	shift = (value > 0xF) << 2; value >>= shift; result |= shift;
	shift = (value > 0x3) << 1; value >>= shift; result |= shift;
	result |= (__u32)(value >> 1);
	return result < HIST_BUCKETS ? result : HIST_BUCKETS - 1;
}

// hist_add는 microseconds가 든 bucket을 하나 올린다. 5.15 verifier는 log2_bucket의 상한을 배열 주소 계산까지 따라가지 못하고
// 범위 밖 접근으로 거부한다. 컴파일러가 확인 전 값으로 주소를 만들지 못하게 막고, 확인한 값으로 index를 만든다.
static __always_inline void hist_add(__u64 *hist, __u64 microseconds) {
	__u32 bucket = log2_bucket(microseconds);
	asm volatile("" : "+r"(bucket));
	if (bucket < HIST_BUCKETS) hist[bucket]++;
}

// task의 tgid를 target_ns_inum namespace에서 본 값으로 바꾼다. 그 namespace에 보이지 않는 task는 0이다.
static __always_inline __u32 ns_tgid(struct task_struct *task) {
	struct pid *pid = BPF_CORE_READ(task, signal, pids[PIDTYPE_TGID]);
	if (!pid) return 0;
	__u32 level = BPF_CORE_READ(pid, level);
#pragma unroll
	for (int index = 0; index < PID_LEVELS; index++) {
		if (index > level) break;
		struct pid_namespace *space = BPF_CORE_READ(pid, numbers[index].ns);
		if (space && BPF_CORE_READ(space, ns.inum) == target_ns_inum) return BPF_CORE_READ(pid, numbers[index].nr);
	}
	return 0;
}

static __always_inline int is_watched(__u32 tgid) { return bpf_map_lookup_elem(&watched, &tgid) != 0; }

static __always_inline struct process_stats *stats_for(__u32 tgid) {
	struct process_stats *value = bpf_map_lookup_elem(&stats, &tgid);
	if (value) return value;
	struct process_stats zero = {};
	bpf_map_update_elem(&stats, &tgid, &zero, BPF_NOEXIST);
	return bpf_map_lookup_elem(&stats, &tgid);
}

static __always_inline void enqueue(struct task_struct *task) {
	__u32 tgid = ns_tgid(task), pid = BPF_CORE_READ(task, pid);
	if (!tgid || !is_watched(tgid)) return;
	__u64 now = bpf_ktime_get_ns();
	bpf_map_update_elem(&runq_start, &pid, &now, 0);
}

SEC("tp_btf/sched_wakeup") int top_wakeup(__u64 *ctx) { enqueue((struct task_struct *)ctx[0]); return 0; }
SEC("tp_btf/sched_wakeup_new") int top_wakeup_new(__u64 *ctx) { enqueue((struct task_struct *)ctx[0]); return 0; }
static __always_inline long task_state(struct task_struct *task) {
	if (bpf_core_field_exists(task->__state)) return BPF_CORE_READ(task, __state);
	return BPF_CORE_READ((struct task_struct___pre514 *)task, state);
}

static __always_inline void switch_in(struct task_struct *next) {
	__u32 next_pid = BPF_CORE_READ(next, pid);
	__u64 *start = bpf_map_lookup_elem(&runq_start, &next_pid);
	if (start) {
		__u64 latency = bpf_ktime_get_ns() - *start;
		bpf_map_delete_elem(&runq_start, &next_pid);
		__u32 next_tgid = ns_tgid(next);
		struct process_stats *value = next_tgid && is_watched(next_tgid) ? stats_for(next_tgid) : 0;
		if (value) { value->runq_count++; value->runq_sum_ns += latency; hist_add(value->runq_hist, latency / 1000); }
	}
}

// 선점당한 task는 깨어나지 않고 곧바로 runqueue로 돌아가므로 prev_state가 TASK_RUNNING일 때 다시 시작 시각을 잡는다.
SEC("tp_btf/sched_switch") int top_switch(__u64 *ctx) {
	switch_in((struct task_struct *)ctx[2]);
	if ((__u32)ctx[3] == TASK_RUNNING) enqueue((struct task_struct *)ctx[1]);
	return 0;
}
// 5.15처럼 sched_switch에 prev_state가 없는 kernel용이다. tp_btf는 없는 인자를 읽을 수 없으므로, 그 kernel의 tracepoint가
// prev_state를 정하던 규칙을 따른다. 선점이면 runnable이고, 아니면 그 순간 prev의 상태를 읽는다.
SEC("tp_btf/sched_switch") int top_switch_legacy(__u64 *ctx) {
	struct task_struct *prev = (struct task_struct *)ctx[1];
	switch_in((struct task_struct *)ctx[2]);
	if ((__u8)ctx[0] || task_state(prev) == TASK_RUNNING) enqueue(prev);
	return 0;
}
SEC("tp_btf/sched_process_exit") int top_exit(__u64 *ctx) {
	struct task_struct *task = (struct task_struct *)ctx[0];
	__u32 pid = BPF_CORE_READ(task, pid);
	bpf_map_delete_elem(&runq_start, &pid);
	return 0;
}

// block layer의 pid는 요청을 낸 task다. 동기 읽기와 direct I/O, fsync는 그 process로 잡히지만 writeback은 kworker로 잡힌다.
SEC("tp_btf/block_rq_issue") int top_issue(__u64 *ctx) {
	struct request *rq = (struct request *)ctx[0];
	__u32 tgid = ns_tgid((struct task_struct *)bpf_get_current_task());
	if (!tgid || !is_watched(tgid)) return 0;
	struct io_start value = {.start_ns = bpf_ktime_get_ns(), .bytes = BPF_CORE_READ(rq, __data_len), .tgid = tgid};
	bpf_map_update_elem(&io_pending, &rq, &value, 0);
	return 0;
}
SEC("tp_btf/block_rq_requeue") int top_requeue(__u64 *ctx) { struct request *rq = (struct request *)ctx[0]; bpf_map_delete_elem(&io_pending, &rq); return 0; }
// 부분 완료마다 이 tracepoint가 발생하고 __data_len은 그 뒤에 줄어든다. 남은 byte를 모두 끝내는 호출이 요청의 완료다.
SEC("tp_btf/block_rq_complete") int top_complete(__u64 *ctx) {
	struct request *rq = (struct request *)ctx[0];
	if ((__u32)ctx[2] < BPF_CORE_READ(rq, __data_len)) return 0;
	struct io_start *pending = bpf_map_lookup_elem(&io_pending, &rq);
	if (!pending) return 0;
	__u64 latency = bpf_ktime_get_ns() - pending->start_ns;
	__u32 tgid = pending->tgid;
	__u64 bytes = pending->bytes;
	bpf_map_delete_elem(&io_pending, &rq);
	struct process_stats *value = stats_for(tgid);
	if (value) { value->io_count++; value->io_bytes += bytes; value->io_sum_ns += latency; hist_add(value->io_hist, latency / 1000); }
	return 0;
}
char LICENSE[] SEC("license") = "Dual BSD/GPL";
