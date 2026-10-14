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
#define BPF_MAP_TYPE_TASK_STORAGE 29
#define BPF_F_NO_PREALLOC 1
#define BPF_LOCAL_STORAGE_GET_F_CREATE 1
#define BPF_NOEXIST 1
#define WATCHED_MAX 4096
#define PENDING_MAX 65536
#define STATS_MAX 4096
#define HIST_BUCKETS 24
#define TASK_RUNNING 0
#define WAITS_MAX 4096
// nr은 64비트 번호이고, 32비트 호환 호출은 WAIT_SYSCALL_COMPAT bit로 표시한다. syscall 밖이면 WAIT_NO_SYSCALL이다.
#define WAIT_NO_SYSCALL 0xFFFF
#define WAIT_SYSCALL_COMPAT 0x8000
#define TS_COMPAT 0x0002
// 사용자 모드로 들어온 적이 없는 스레드는 register 영역이 비어 있어 번호 0이 syscall처럼 보인다.
#define PF_IO_WORKER 0x00000010
#define PF_USER_WORKER 0x00004000
#define PF_KTHREAD 0x00200000

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
struct pt_regs___x86 { unsigned long orig_ax; } __attribute__((preserve_access_index));
struct pt_regs___arm64 { int syscallno; } __attribute__((preserve_access_index));
struct thread_info___x86 { __u32 status; } __attribute__((preserve_access_index));
struct task_struct___syscall { unsigned int flags; struct thread_info___x86 thread_info; } __attribute__((preserve_access_index));

// bucket i는 [2^i, 2^(i+1)) 마이크로초다. 누적 count의 차이로 구간별 분포를 구한다.
struct process_stats {
	__u64 runq_count; __u64 runq_sum_ns; __u64 runq_hist[HIST_BUCKETS];
	__u64 io_count; __u64 io_bytes; __u64 io_sum_ns; __u64 io_hist[HIST_BUCKETS];
};
const struct process_stats *unused_process_stats __attribute__((unused));
struct io_start { __u64 start_ns; __u64 bytes; __u32 tgid; __u32 pad; };
// top_task는 감시 중인 스레드의 대기 상태다. watched와 tgid는 깨어날 때(enqueue) 이미 계산한 값을 남겨, 잠들 때마다
// PID namespace를 따라가며 tgid를 다시 계산하지 않게 한다. wait_start_ns는 잠들며 CPU를 떠난 시각이고, 0이면 대기 중이
// 아니다. 대기는 다시 CPU를 받을 때 센다.
struct top_task { __u64 wait_start_ns; __u32 tgid; __u32 nr; __u8 watched; };
struct wait_key { __u32 tgid; __u32 nr; };
struct wait_stats { __u64 count; __u64 sum_ns; };
const struct wait_key *unused_wait_key __attribute__((unused));
const struct wait_stats *unused_wait_stats __attribute__((unused));

// edc가 속한 PID namespace의 inode다. 감시하는 pid는 /proc에서 읽은 그 namespace의 값이므로 BPF가 보는 pid를 이 값으로 바꿔 비교한다.
volatile const __u32 target_ns_inum = 0;

struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, WATCHED_MAX); __type(key, __u32); __type(value, __u8); } watched SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_LRU_HASH); __uint(max_entries, PENDING_MAX); __type(key, __u32); __type(value, __u64); } runq_start SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_LRU_HASH); __uint(max_entries, PENDING_MAX); __type(key, void *); __type(value, struct io_start); } io_pending SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, STATS_MAX); __type(key, __u32); __type(value, struct process_stats); } stats SEC(".maps");
// 모든 sched_switch에서 보므로 hash map보다 빠른 task local storage에 둔다. 스레드가 끝나면 kernel이 지운다.
struct { __uint(type, BPF_MAP_TYPE_TASK_STORAGE); __uint(map_flags, BPF_F_NO_PREALLOC); __type(key, int); __type(value, struct top_task); } top_tasks SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, WAITS_MAX); __type(key, struct wait_key); __type(value, struct wait_stats); } waits SEC(".maps");

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

// current_syscall은 스레드가 CPU를 떠날 때 들어가 있던 syscall이다. kernel은 syscall로 들어올 때 번호를 register 영역에
// 남기고, page fault나 interrupt로 들어올 때는 -1을 둔다. x86은 orig_ax, arm64는 syscallno에 둔다.
static __always_inline __u32 current_syscall(struct task_struct *task) {
	struct task_struct___syscall *flavor = (void *)task;
	if (bpf_core_field_exists(flavor->flags) && (flavor->flags & (PF_KTHREAD | PF_IO_WORKER | PF_USER_WORKER))) return WAIT_NO_SYSCALL;
	void *regs = (void *)bpf_task_pt_regs((void *)task);
	if (!regs) return WAIT_NO_SYSCALL;
	long nr = -1;
	struct pt_regs___x86 *x86 = regs;
	struct pt_regs___arm64 *arm64 = regs;
	if (bpf_core_field_exists(x86->orig_ax)) nr = (long)x86->orig_ax;
	else if (bpf_core_field_exists(arm64->syscallno)) nr = arm64->syscallno;
	if (nr < 0 || nr >= WAIT_SYSCALL_COMPAT) return WAIT_NO_SYSCALL;
	if (bpf_core_field_exists(flavor->thread_info.status) && (flavor->thread_info.status & TS_COMPAT)) nr |= WAIT_SYSCALL_COMPAT;
	return (__u32)nr;
}

// wait_out은 감시 중인 스레드가 잠들며 CPU를 떠날 때 부른다. 감시를 멈춘 process의 표시는 여기서 지운다.
static __always_inline void wait_out(struct task_struct *task) {
	struct top_task *state = bpf_task_storage_get(&top_tasks, task, 0, 0);
	if (!state || !state->watched) return;
	if (!is_watched(state->tgid)) {
		state->watched = 0;
		return;
	}
	state->wait_start_ns = bpf_ktime_get_ns();
	state->nr = current_syscall(task);
}

static __always_inline void wait_in(struct task_struct *task) {
	struct top_task *state = bpf_task_storage_get(&top_tasks, task, 0, 0);
	if (!state || !state->wait_start_ns) return;
	__u64 latency = bpf_ktime_get_ns() - state->wait_start_ns;
	struct wait_key key = {.tgid = state->tgid, .nr = state->nr};
	state->wait_start_ns = 0;
	struct wait_stats *value = bpf_map_lookup_elem(&waits, &key);
	if (!value) {
		struct wait_stats zero = {};
		bpf_map_update_elem(&waits, &key, &zero, BPF_NOEXIST);
		value = bpf_map_lookup_elem(&waits, &key);
	}
	if (value) { value->count++; value->sum_ns += latency; }
}

static __always_inline void enqueue(struct task_struct *task) {
	__u32 tgid = ns_tgid(task), pid = BPF_CORE_READ(task, pid);
	if (!tgid || !is_watched(tgid)) return;
	struct top_task *state = bpf_task_storage_get(&top_tasks, task, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (state) { state->tgid = tgid; state->watched = 1; }
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
	wait_in(next);
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
	else wait_out((struct task_struct *)ctx[1]);
	return 0;
}
// 5.15처럼 sched_switch에 prev_state가 없는 kernel용이다. tp_btf는 없는 인자를 읽을 수 없으므로, 그 kernel의 tracepoint가
// prev_state를 정하던 규칙을 따른다. 선점이면 runnable이고, 아니면 그 순간 prev의 상태를 읽는다.
SEC("tp_btf/sched_switch") int top_switch_legacy(__u64 *ctx) {
	struct task_struct *prev = (struct task_struct *)ctx[1];
	switch_in((struct task_struct *)ctx[2]);
	if ((__u8)ctx[0] || task_state(prev) == TASK_RUNNING) enqueue(prev);
	else wait_out(prev);
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
