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

// 이 객체는 trace lock만 불러온다. kernel lock은 lock:contention_begin과 contention_end로, 사용자 lock은 futex_wait의
// fentry와 fexit로 기다린 시간을 잰다. 둘 다 (lock 주소, stack, process)별로 kernel 안에서 모으고, 이름은 사용자 공간이
// 끝날 때 붙인다.

#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_PERCPU_HASH 5
#define BPF_MAP_TYPE_STACK_TRACE 7
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_MAP_TYPE_TASK_STORAGE 29
#define BPF_F_NO_PREALLOC 1
#define BPF_LOCAL_STORAGE_GET_F_CREATE 1
#define BPF_NOEXIST 1
#define BPF_F_USER_STACK (1ULL << 8)
#define TASK_COMM_LEN 16
#define HIST_BUCKETS 24
// 한 task 안에서 contention이 겹치는 깊이다. mutex의 느린 길은 그 안의 spinlock에서 다시 기다릴 수 있다.
#define LOCK_DEPTH 4
#define LOCK_STACK_DEPTH 64

enum lock_kind { LOCK_KERNEL, LOCK_FUTEX };
enum lock_counter { LOCK_STATS_FULL, LOCK_DEPTH_FULL, LOCK_UNMATCHED, LOCK_STACK_LOST, LOCK_FRESH_LOST, LOCK_COUNTERS };

struct task_struct { struct task_struct *group_leader; char comm[TASK_COMM_LEN]; } __attribute__((preserve_access_index));

// tgid는 futex에만 채운다. 사용자 주소는 process마다 뜻이 다르고, kernel lock은 process 이름으로만 묶어 짧은 process가
// 항목을 늘리지 않게 한다.
struct lock_key { __u64 lock; __u64 cgroup_id; __u32 tgid; __s32 stack; __u32 flags; __u32 kind; char comm[TASK_COMM_LEN]; };
// bucket i는 [2^i, 2^(i+1)) 마이크로초다.
struct lock_stats { __u64 count; __u64 sum_ns; __u64 max_ns; __u64 hist[HIST_BUCKETS]; };
struct lock_wait { __u64 lock; __u64 start_ns; __s32 stack; __u32 flags; };
struct lock_task { __u64 futex_start_ns; __u64 futex_addr; __s32 futex_stack; __u32 depth; struct lock_wait kernel[LOCK_DEPTH]; };
struct lock_comm { char comm[TASK_COMM_LEN]; };
struct lock_counters { __u64 values[LOCK_COUNTERS]; };
struct lock_fresh { __u32 tgid; char comm[TASK_COMM_LEN]; };
const struct lock_key *unused_lock_key __attribute__((unused));
const struct lock_stats *unused_lock_stats __attribute__((unused));
const struct lock_comm *unused_lock_comm __attribute__((unused));
const struct lock_counters *unused_lock_counters __attribute__((unused));
const struct lock_fresh *unused_lock_fresh __attribute__((unused));

struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, 8192); __type(key, struct lock_key); __type(value, struct lock_stats); } stats SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_STACK_TRACE); __uint(max_entries, 16384); __uint(key_size, sizeof(__u32)); __uint(value_size, LOCK_STACK_DEPTH * sizeof(__u64)); } stacks SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_TASK_STORAGE); __uint(map_flags, BPF_F_NO_PREALLOC); __type(key, int); __type(value, struct lock_task); } tasks SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 1024); __type(key, __u32); __type(value, __u8); } allowed_pids SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 64); __type(key, struct lock_comm); __type(value, __u8); } allowed_comms SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u8); } allowed_cgroups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct lock_counters); } counters SEC(".maps");
// fresh는 futex를 처음 기다린 (tgid, comm)이다. 사용자 공간이 받자마자 mapping을 읽어 futex 주소에 변수 이름을 붙인다.
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 16384); __type(key, struct lock_fresh); __type(value, __u8); } seen SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 18); } fresh SEC(".maps");

// self_tgid는 edc 자신이다. edc가 init pid namespace 밖에 있으면 BPF가 보는 tgid를 알 수 없어 0으로 두고 제외하지 않는다.
volatile const __u32 self_tgid = 0;
volatile const __u8 filter_process = 0;
volatile const __u8 filter_cgroup = 0;

static __always_inline void count(__u32 index) {
	__u32 key = 0;
	struct lock_counters *c = bpf_map_lookup_elem(&counters, &key);
	if (c && index < LOCK_COUNTERS) __sync_fetch_and_add(&c->values[index], 1);
}

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

// 5.15 verifier는 log2_bucket의 상한을 배열 주소 계산까지 따라가지 못하고 범위 밖 접근으로 거부한다.
// 컴파일러가 확인 전 값으로 주소를 만들지 못하게 막고, 확인한 값으로 index를 만든다.
static __always_inline void hist_add(__u64 *hist, __u64 microseconds) {
	__u32 bucket = log2_bucket(microseconds);
	asm volatile("" : "+r"(bucket));
	if (bucket < HIST_BUCKETS) hist[bucket]++;
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

// selected는 필터를 통과한 task인지 보고 key의 cgroup과 comm을 채운다. 기다리기 시작할 때 한 번만 본다.
static __always_inline int selected(struct task_struct *task, struct lock_key *key) {
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	if (self_tgid && tgid == self_tgid) return 0;
	key->cgroup_id = bpf_get_current_cgroup_id();
	if (filter_cgroup && !bpf_map_lookup_elem(&allowed_cgroups, &key->cgroup_id)) return 0;
	leader_comm(task, key->comm);
	if (filter_process && !bpf_map_lookup_elem(&allowed_pids, &tgid)) {
		struct lock_comm name = {};
		__builtin_memcpy(name.comm, key->comm, TASK_COMM_LEN);
		if (!bpf_map_lookup_elem(&allowed_comms, &name)) return 0;
	}
	return 1;
}

static __always_inline __s32 stack_id(void *ctx, __u64 flags) {
	long id = bpf_get_stackid(ctx, &stacks, flags);
	if (id < 0) {
		count(LOCK_STACK_LOST);
		return -1;
	}
	return (__s32)id;
}

static __always_inline void add(struct lock_key *key, __u64 waited) {
	struct lock_stats *value = bpf_map_lookup_elem(&stats, key);
	if (!value) {
		struct lock_stats zero = {};
		bpf_map_update_elem(&stats, key, &zero, BPF_NOEXIST);
		value = bpf_map_lookup_elem(&stats, key);
		if (!value) {
			count(LOCK_STATS_FULL);
			return;
		}
	}
	value->count++;
	value->sum_ns += waited;
	if (waited > value->max_ns) value->max_ns = waited;
	hist_add(value->hist, waited / 1000);
}

SEC("tp_btf/contention_begin") int lock_contention_begin(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	struct lock_key key = {};
	if (!selected(task, &key)) return 0;
	struct lock_task *state = bpf_task_storage_get(&tasks, task, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (!state) return 0;
	__u32 depth = state->depth;
	// mutex의 느린 길은 spin하기 전과 잠들기 전에 contention_begin을 두 번 부르고 contention_end는 한 번 부른다. 맨 위가
	// 같은 lock이면 같은 기다림이 이어지는 것이라 새로 쌓지 않는다. 쌓으면 끝이 하나만 꺼내 남은 칸이 thread가 끝날 때까지
	// 깊이를 차지한다. 기다린 시간은 첫 시작부터 잰다.
	if (depth > 0 && state->kernel[(depth - 1) & (LOCK_DEPTH - 1)].lock == ctx[0]) return 0;
	if (depth >= LOCK_DEPTH) {
		count(LOCK_DEPTH_FULL);
		return 0;
	}
	struct lock_wait *wait = &state->kernel[depth & (LOCK_DEPTH - 1)];
	wait->lock = ctx[0];
	wait->flags = (__u32)ctx[1];
	wait->stack = stack_id(ctx, 0);
	wait->start_ns = bpf_ktime_get_ns();
	state->depth = depth + 1;
	return 0;
}

// 기다림은 시작한 순서의 반대로 끝난다. 그래도 맨 위가 다른 lock이면 아래에서 같은 lock을 찾아 그것까지 꺼낸다.
SEC("tp_btf/contention_end") int lock_contention_end(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	struct lock_task *state = bpf_task_storage_get(&tasks, task, 0, 0);
	if (!state || !state->depth) return 0;
	__u64 lock = ctx[0], now = bpf_ktime_get_ns();
	__u32 depth = state->depth;
#pragma unroll
	for (int step = 0; step < LOCK_DEPTH; step++) {
		if (depth == 0) break;
		depth--;
		struct lock_wait *wait = &state->kernel[depth & (LOCK_DEPTH - 1)];
		if (wait->lock != lock) continue;
		struct lock_key key = { .lock = lock, .stack = wait->stack, .flags = wait->flags, .kind = LOCK_KERNEL };
		key.cgroup_id = bpf_get_current_cgroup_id();
		leader_comm(task, key.comm);
		add(&key, now > wait->start_ns ? now - wait->start_ns : 0);
		state->depth = depth;
		return 0;
	}
	count(LOCK_UNMATCHED);
	return 0;
}

SEC("fentry/futex_wait") int lock_futex_wait(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	struct lock_key key = {};
	if (!selected(task, &key)) return 0;
	struct lock_task *state = bpf_task_storage_get(&tasks, task, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (!state) return 0;
	struct lock_fresh first = { .tgid = bpf_get_current_pid_tgid() >> 32 };
	__builtin_memcpy(first.comm, key.comm, TASK_COMM_LEN);
	__u8 yes = 1;
	if (bpf_map_update_elem(&seen, &first, &yes, BPF_NOEXIST) == 0 && bpf_ringbuf_output(&fresh, &first, sizeof(first), 0) != 0) count(LOCK_FRESH_LOST);
	// 5.15 verifier는 struct가 아닌 pointer 인자(u32 *uaddr)를 ctx에서 바로 읽지 못하게 한다. trampoline이 저장한 값을 복사한다.
	__u64 uaddr = 0;
	bpf_probe_read_kernel(&uaddr, sizeof(uaddr), ctx);
	state->futex_addr = uaddr;
	state->futex_stack = stack_id(ctx, BPF_F_USER_STACK);
	state->futex_start_ns = bpf_ktime_get_ns();
	return 0;
}

SEC("fexit/futex_wait") int lock_futex_wait_exit(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	struct lock_task *state = bpf_task_storage_get(&tasks, task, 0, 0);
	if (!state || !state->futex_start_ns) return 0;
	__u64 now = bpf_ktime_get_ns(), waited = now > state->futex_start_ns ? now - state->futex_start_ns : 0;
	state->futex_start_ns = 0;
	struct lock_key key = { .lock = state->futex_addr, .stack = state->futex_stack, .kind = LOCK_FUTEX, .tgid = bpf_get_current_pid_tgid() >> 32 };
	key.cgroup_id = bpf_get_current_cgroup_id();
	leader_comm(task, key.comm);
	add(&key, waited);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
