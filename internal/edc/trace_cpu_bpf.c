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

// 이 객체는 trace cpu만 불러온다. CPU마다 연 cpu-clock perf event가 일정한 간격으로 cpu_sample을 부르고,
// program은 그때 돌던 task의 kernel stack과 사용자 stack을 stack map에 넣어 (process, 두 stack)별로 센다.
// --off-cpu에서는 cpu_sample 대신 offcpu_switch가 sched_switch에서 잠드는 task의 stack을 잡고, 깨어나 다시 돌 때 잠든
// 시간을 그 stack에 더한다. 심볼 이름은 사용자 공간이 끝날 때 붙인다.

#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
#define BPF_MAP_TYPE_STACK_TRACE 7
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_MAP_TYPE_TASK_STORAGE 29
#define BPF_F_NO_PREALLOC 1
#define BPF_LOCAL_STORAGE_GET_F_CREATE 1
#define TASK_RUNNING 0
#define BPF_NOEXIST 1
#define BPF_F_USER_STACK (1ULL << 8)
#define TASK_COMM_LEN 16
#define PF_KTHREAD 0x00200000
#define EEXIST 17
#define ENOMEM 12
// kernel.perf_event_max_stack의 기본값과 같다. 이보다 깊은 stack은 위쪽이 잘린다.
#define CPU_STACK_DEPTH 127

enum cpu_counter {
	CPU_SAMPLES,
	CPU_IDLE,
	CPU_SELF,
	CPU_FILTERED,
	CPU_COUNTS_FULL,
	CPU_USER_MISSING,
	CPU_USER_COLLISION,
	CPU_KERNEL_EMPTY,
	CPU_KERNEL_COLLISION,
	CPU_STACKS_FULL,
	CPU_FRESH_LOST,
	CPU_OFFCPU_STARTS_FULL,
	CPU_COUNTERS,
};

struct task_struct { unsigned int flags; int pid; int tgid; unsigned int __state; struct task_struct *group_leader; char comm[TASK_COMM_LEN]; } __attribute__((preserve_access_index));
// 5.14에서 state가 __state로 바뀌었다. CO-RE가 있는 쪽을 읽는다.
struct task_struct___pre514 { long state; } __attribute__((preserve_access_index));

// stack id가 음수이면 그 stack이 없다. kernel thread에는 사용자 stack이 없고, 사용자 공간에서 샘플된 task에는 kernel
// stack이 없다. cgroup_id는 늘 채운다. 같은 tgid는 같은 cgroup이라 항목 수를 늘리지 않는다.
struct cpu_key { __u64 cgroup_id; __u32 tgid; __s32 user_stack; __s32 kernel_stack; __u32 pad; char comm[TASK_COMM_LEN]; };
struct cpu_comm { char comm[TASK_COMM_LEN]; };
// cpu_fresh는 처음 샘플된 (tgid, comm)이다. 짧은 process는 끝나면 /proc에서 사라지므로, 사용자 공간이 이 알림을 받자마자
// mapping을 읽는다. exec는 mapping과 comm을 함께 바꾸므로 comm까지 key로 쓴다.
struct cpu_fresh { __u32 tgid; char comm[TASK_COMM_LEN]; };
struct cpu_counters { __u64 values[CPU_COUNTERS]; };
// cpu_value의 count는 on-CPU에서 샘플 수, off-CPU에서 잠든 횟수다. off_ns는 off-CPU에서만 쌓는 잠든 시간이다.
struct cpu_value { __u64 count; __u64 off_ns; };
// offcpu_start는 잠든 thread 하나다. start_ns가 0이면 그 thread는 잠들어 있지 않다.
struct offcpu_start { __u64 start_ns; struct cpu_key key; };
const struct cpu_key *unused_cpu_key __attribute__((unused));
const struct cpu_comm *unused_cpu_comm __attribute__((unused));
const struct cpu_counters *unused_cpu_counters __attribute__((unused));
const struct cpu_fresh *unused_cpu_fresh __attribute__((unused));
const struct cpu_value *unused_cpu_value __attribute__((unused));

// 두 map의 크기는 Go가 정한다. stacks의 값은 주소 CPU_STACK_DEPTH개다.
struct { __uint(type, BPF_MAP_TYPE_STACK_TRACE); __uint(max_entries, 16384); __uint(key_size, sizeof(__u32)); __uint(value_size, CPU_STACK_DEPTH * sizeof(__u64)); } stacks SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 16384); __type(key, struct cpu_key); __type(value, struct cpu_value); } counts SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_TASK_STORAGE); __uint(map_flags, BPF_F_NO_PREALLOC); __type(key, int); __type(value, struct offcpu_start); } offcpu_starts SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 1024); __type(key, __u32); __type(value, __u8); } allowed_pids SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 64); __type(key, struct cpu_comm); __type(value, __u8); } allowed_comms SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u8); } allowed_cgroups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct cpu_counters); } counters SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 16384); __type(key, struct cpu_fresh); __type(value, __u8); } seen SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 18); } fresh SEC(".maps");

// self_tgid는 edc 자신이다. edc가 init pid namespace 밖에 있으면 BPF가 보는 tgid를 알 수 없어 0으로 두고 제외하지 않는다.
volatile const __u32 self_tgid = 0;
volatile const __u8 filter_process = 0;
volatile const __u8 filter_cgroup = 0;

static __always_inline void count(__u32 index) {
	__u32 key = 0;
	struct cpu_counters *c = bpf_map_lookup_elem(&counters, &key);
	if (c && index < CPU_COUNTERS) c->values[index]++;
}

// 5.15 kernel은 comm을 strlcpy로 바꿔 NUL 뒤에 이전 이름이 남을 수 있다. map key로 쓰려면 NUL 뒤를 지운다.
static __always_inline void leader_comm(struct task_struct *task, char *comm) {
	struct task_struct *leader = BPF_CORE_READ(task, group_leader);
	bpf_core_read(comm, TASK_COMM_LEN, &leader->comm);
	int ended = 0;
#pragma unroll
	for (int index = 0; index < TASK_COMM_LEN; index++) {
		if (ended) comm[index] = 0;
		else if (!comm[index]) ended = 1;
	}
}

// stack_id는 bpf_get_stackid의 실패를 이유별로 센다. EEXIST는 다른 stack이 같은 hash 칸을 쓰고 있다는 뜻이다.
// BPF_F_REUSE_STACKID로 덮어쓰면 그 칸을 가리키던 이전 count가 다른 stack을 가리키게 되므로 덮어쓰지 않는다.
static __always_inline __s32 stack_id(void *ctx, __u64 flags, __u32 missing, __u32 collision) {
	long id = bpf_get_stackid(ctx, &stacks, flags);
	if (id >= 0) return (__s32)id;
	if (id == -EEXIST) count(collision);
	else if (id == -ENOMEM) count(CPU_STACKS_FULL);
	else count(missing);
	return -1;
}

// selected는 필터를 통과한 task인지 보고 key의 comm을 채운다. cgroup은 지금 돌고 있는 task의 것이므로, task는 늘 current다.
static __always_inline int selected(struct task_struct *task, __u32 tgid, struct cpu_key *key) {
	if (self_tgid && tgid == self_tgid) {
		count(CPU_SELF);
		return 0;
	}
	if (filter_cgroup) {
		__u64 cgroup_id = bpf_get_current_cgroup_id();
		if (!bpf_map_lookup_elem(&allowed_cgroups, &cgroup_id)) {
			count(CPU_FILTERED);
			return 0;
		}
	}
	leader_comm(task, key->comm);
	if (filter_process && !bpf_map_lookup_elem(&allowed_pids, &tgid)) {
		struct cpu_comm name = {};
		__builtin_memcpy(name.comm, key->comm, TASK_COMM_LEN);
		if (!bpf_map_lookup_elem(&allowed_comms, &name)) {
			count(CPU_FILTERED);
			return 0;
		}
	}
	return 1;
}

// fill_key는 지금 돌고 있는 task의 cgroup과 두 stack을 key에 넣는다. 처음 본 (tgid, comm)이면 사용자 공간에 알린다.
static __always_inline void fill_key(void *ctx, struct task_struct *task, __u32 tgid, struct cpu_key *key) {
	key->tgid = tgid;
	struct cpu_fresh first = { .tgid = tgid };
	__builtin_memcpy(first.comm, key->comm, TASK_COMM_LEN);
	__u8 yes = 1;
	if (bpf_map_update_elem(&seen, &first, &yes, BPF_NOEXIST) == 0 && bpf_ringbuf_output(&fresh, &first, sizeof(first), 0) != 0) count(CPU_FRESH_LOST);
	key->cgroup_id = bpf_get_current_cgroup_id();
	key->kernel_stack = stack_id(ctx, 0, CPU_KERNEL_EMPTY, CPU_KERNEL_COLLISION);
	key->user_stack = -1;
	if (!(BPF_CORE_READ(task, flags) & PF_KTHREAD)) key->user_stack = stack_id(ctx, BPF_F_USER_STACK, CPU_USER_MISSING, CPU_USER_COLLISION);
}

static __always_inline void add(struct cpu_key *key, __u64 off_ns) {
	struct cpu_value *value = bpf_map_lookup_elem(&counts, key);
	if (!value) {
		struct cpu_value zero = {};
		bpf_map_update_elem(&counts, key, &zero, BPF_NOEXIST);
		value = bpf_map_lookup_elem(&counts, key);
		if (!value) {
			count(CPU_COUNTS_FULL);
			return;
		}
	}
	__sync_fetch_and_add(&value->count, 1);
	if (off_ns) __sync_fetch_and_add(&value->off_ns, off_ns);
}

SEC("perf_event") int cpu_sample(void *ctx) {
	count(CPU_SAMPLES);
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;
	// pid 0은 CPU마다 하나씩 있는 idle task다. 이 수로 CPU가 쉰 비율을 낸다.
	if ((__u32)id == 0) {
		count(CPU_IDLE);
		return 0;
	}
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	struct cpu_key key = {};
	if (!selected(task, tgid, &key)) return 0;
	fill_key(ctx, task, tgid, &key);
	add(&key, 0);
	return 0;
}

static __always_inline long task_state(struct task_struct *task) {
	if (bpf_core_field_exists(task->__state)) return BPF_CORE_READ(task, __state);
	return BPF_CORE_READ((struct task_struct___pre514 *)task, state);
}

// offcpu_switch는 5.15처럼 sched_switch에 prev_state 인자가 없는 kernel에서도 돈다. 그 kernel의 tracepoint 규칙대로,
// 선점이면 runnable이고 아니면 그 순간 prev의 상태를 읽는다. runnable task는 CPU를 기다리는 것이라 잠든 시간이 아니다.
// tracepoint는 prev의 문맥에서 불리므로 bpf_get_stackid가 prev의 stack을 읽고, current의 cgroup이 prev의 cgroup이다.
SEC("tp_btf/sched_switch") int offcpu_switch(__u64 *ctx) {
	struct task_struct *prev = (struct task_struct *)ctx[1];
	struct task_struct *next = (struct task_struct *)ctx[2];
	__u64 now = bpf_ktime_get_ns();
	struct offcpu_start *woke = bpf_task_storage_get(&offcpu_starts, next, 0, 0);
	if (woke && woke->start_ns) {
		__u64 slept = now > woke->start_ns ? now - woke->start_ns : 0;
		woke->start_ns = 0;
		add(&woke->key, slept ? slept : 1);
	}
	if ((__u8)ctx[0] || task_state(prev) == TASK_RUNNING) return 0;
	__u32 tgid = BPF_CORE_READ(prev, tgid);
	if (!BPF_CORE_READ(prev, pid) || (BPF_CORE_READ(prev, flags) & PF_KTHREAD)) return 0;
	struct cpu_key key = {};
	if (!selected(prev, tgid, &key)) return 0;
	struct offcpu_start *start = bpf_task_storage_get(&offcpu_starts, prev, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (!start) {
		count(CPU_OFFCPU_STARTS_FULL);
		return 0;
	}
	fill_key(ctx, prev, tgid, &key);
	start->key = key;
	start->start_ns = now;
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
