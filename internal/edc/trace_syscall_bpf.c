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
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_PERCPU_HASH 5
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_MAP_TYPE_TASK_STORAGE 29
#define BPF_F_NO_PREALLOC 1
#define BPF_LOCAL_STORAGE_GET_F_CREATE 1
#define BPF_NOEXIST 1
#define TASK_COMM_LEN 16
#define HIST_BUCKETS 24
#define ERRNOS_MAX 4096
#define MAX_ERRNO 4095
#define NSEC_PER_SEC 1000000000ULL
// rules는 64비트 번호를 그대로, 32비트 호환 번호는 SYSCALL_NR_MAX를 더한 자리에 둔다. 두 표 모두 번호가 500 아래다.
#define SYSCALL_NR_MAX 1024
#define SYSCALL_PATH_MAX 256
// x86 thread_info.status의 TS_COMPAT이다. 32비트 진입점이 syscall마다 켜고 사용자 공간으로 돌아가기 전에 끈다.
#define TS_COMPAT 0x0002
#define SYSCALL_COMPAT 0x80000000U

enum syscall_counter { SYSCALL_STARTS_FULL, SYSCALL_STATS_FULL, SYSCALL_ERRNOS_FULL, SYSCALL_UNMATCHED, SYSCALL_RING_LOST, SYSCALL_COUNTERS };
enum syscall_events { SYSCALL_EVENTS_OFF, SYSCALL_EVENTS_ALL_ERRORS, SYSCALL_EVENTS_LISTED_ERRORS };

struct thread_info___x86 { __u32 status; } __attribute__((preserve_access_index));
struct task_struct___x86 { struct thread_info___x86 thread_info; } __attribute__((preserve_access_index));
struct task_struct { struct task_struct *group_leader; char comm[TASK_COMM_LEN]; } __attribute__((preserve_access_index));
// sys_exit tracepoint가 주는 사용자 register다. 하나의 object가 두 아키텍처에서 돌므로 field가 있는 쪽을 쓴다.
// arm64는 x0을 반환값으로 덮으므로 첫 인자는 orig_x0에 남는다.
struct pt_regs___x86 { unsigned long di; unsigned long si; unsigned long dx; unsigned long r10; unsigned long r8; unsigned long r9; } __attribute__((preserve_access_index));
struct pt_regs___arm64 { __u64 regs[31]; __u64 orig_x0; } __attribute__((preserve_access_index));
struct seq_file;
struct bpf_iter_meta { struct seq_file *seq; } __attribute__((preserve_access_index));
struct bpf_iter__task { struct bpf_iter_meta *meta; struct task_struct *task; } __attribute__((preserve_access_index));

// 짧게 사는 process가 많은 host에서는 tgid마다 항목을 만들면 map이 금방 찬다. 같은 이름은 한 항목에 모은다.
// cgroup_id는 --group-by cgroup일 때만 채운다. ssh login마다 생기는 session scope처럼 잠깐 있는 cgroup도 항목을 늘린다.
// nr의 맨 위 bit는 32비트 호환 호출이다. amd64에서 같은 번호가 64비트와 32비트에서 다른 syscall이다.
struct syscall_key { __u64 cgroup_id; char comm[TASK_COMM_LEN]; __u32 nr; __u32 pad; };
// calls는 모든 호출이고 timed는 돌아온 호출이다. exit와 exit_group은 돌아오지 않아 calls에만 들어간다.
// bucket i는 [2^i, 2^(i+1)) 마이크로초다.
struct syscall_stats { __u64 calls; __u64 timed; __u64 errors; __u64 sum_ns; __u64 max_ns; __u64 hist[HIST_BUCKETS]; };
// start_ns가 0이면 그 thread는 syscall 안에 있지 않다.
struct syscall_start { __u64 start_ns; __u32 tgid; __u32 nr; };
struct syscall_in_flight { __u64 start_ns; __u32 tgid; __u32 nr; char comm[TASK_COMM_LEN]; };
struct syscall_errno_key { struct syscall_key owner; __u32 err; __u32 pad; };
struct syscall_comm { char comm[TASK_COMM_LEN]; };
struct syscall_counters { __u64 values[SYSCALL_COUNTERS]; };
// path_arg는 경로 인자의 위치에 1을 더한 값이고, 0이면 경로가 없다. path_gone은 성공하면 경로를 읽을 수 없는 호출이다.
// execve가 성공하면 register와 memory가 새 program의 것으로 바뀐다.
struct syscall_rule { __u8 allowed; __u8 path_arg; __u8 path_gone; __u8 pad; };
struct syscall_window { __u64 start_ns; __u64 sent; __u64 suppressed; };
// path_state는 0이면 경로가 없는 호출, 1이면 읽은 경로, 2이면 읽지 못한 경로, 3이면 잘린 경로다.
struct syscall_event { __u64 timestamp_ns; __u64 latency_ns; __s64 ret; __u32 tgid; __u32 tid; __u32 nr; __u8 path_state; __u8 pad[3]; char comm[TASK_COMM_LEN]; char path[SYSCALL_PATH_MAX]; };
const struct syscall_key *unused_syscall_key __attribute__((unused));
const struct syscall_stats *unused_syscall_stats __attribute__((unused));
const struct syscall_start *unused_syscall_start __attribute__((unused));
const struct syscall_in_flight *unused_syscall_in_flight __attribute__((unused));
const struct syscall_errno_key *unused_syscall_errno_key __attribute__((unused));
const struct syscall_comm *unused_syscall_comm __attribute__((unused));
const struct syscall_counters *unused_syscall_counters __attribute__((unused));
const struct syscall_rule *unused_syscall_rule __attribute__((unused));
const struct syscall_window *unused_syscall_window __attribute__((unused));
const struct syscall_event *unused_syscall_event __attribute__((unused));

// stats의 max_entries는 사용자 공간이 CPU 수와 메모리 상한으로 다시 정한다. per-CPU 값이라 CPU마다 항목 하나를 미리 잡는다.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, 4096); __type(key, struct syscall_key); __type(value, struct syscall_stats); } stats SEC(".maps");
// 시작 시각은 thread마다 하나다. tid를 key로 한 hash map보다 task local storage가 진입과 복귀 모두 빠르고,
// thread가 끝나면 kernel이 값을 지운다. 대신 사용자 공간이 순회할 수 없어, 진행 중인 호출은 task iterator로 읽는다.
struct { __uint(type, BPF_MAP_TYPE_TASK_STORAGE); __uint(map_flags, BPF_F_NO_PREALLOC); __type(key, int); __type(value, struct syscall_start); } starts SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, ERRNOS_MAX); __type(key, struct syscall_errno_key); __type(value, __u64); } errnos SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 1024); __type(key, __u32); __type(value, __u8); } allowed_pids SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 64); __type(key, struct syscall_comm); __type(value, __u8); } allowed_comms SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u8); } allowed_cgroups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct syscall_counters); } counters SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 2 * SYSCALL_NR_MAX); __type(key, __u32); __type(value, struct syscall_rule); } rules SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, MAX_ERRNO + 1); __type(key, __u32); __type(value, __u8); } event_errnos SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 22); } events SEC(".maps");
// event_windows는 CPU마다 지금 1초 창에서 보낸 event 수다. 오류가 쏟아지면 event만 줄이고 합계는 계속 센다.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct syscall_window); } event_windows SEC(".maps");

// self_tgid는 edc 자신이다. edc가 init pid namespace 밖에 있으면 BPF가 보는 tgid를 알 수 없어 0으로 두고 제외하지 않는다.
volatile const __u32 self_tgid = 0;
volatile const __u8 filter_process = 0;
volatile const __u8 filter_cgroup = 0;
volatile const __u8 key_cgroup = 0;
// 돌아오지 않는 syscall의 번호다. 아키텍처마다 달라 사용자 공간이 정하고, 없는 값은 어떤 번호와도 같지 않게 둔다.
volatile const __u32 exit_nr = 0xFFFFFFFFU;
volatile const __u32 exit_group_nr = 0xFFFFFFFFU;
volatile const __u32 compat_exit_nr = 0xFFFFFFFFU;
volatile const __u32 compat_exit_group_nr = 0xFFFFFFFFU;
// filter_syscall이면 rules의 allowed가 있는 syscall만 센다.
volatile const __u8 filter_syscall = 0;
// slow_ns가 0이 아니면 그 시간 이상 걸린 호출을, event_errors가 켜져 있으면 실패한 호출을 event로 낸다.
volatile const __u64 slow_ns = 0;
volatile const __u8 event_errors = SYSCALL_EVENTS_OFF;
volatile const __u32 event_limit = 1000;

static __always_inline void count(__u32 index) {
	__u32 key = 0;
	struct syscall_counters *counts = bpf_map_lookup_elem(&counters, &key);
	if (counts && index < SYSCALL_COUNTERS) __sync_fetch_and_add(&counts->values[index], 1);
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

// thread 이름은 process마다 다를 수 있다. Java의 worker thread도 process 이름으로 묶으려고 대표 thread의 comm을 읽는다.
// helper 대신 BTF pointer로 직접 읽는다. 모든 syscall에서 돌기 때문에 helper 호출 두 번의 비용이 크다.
// 5.15 kernel은 comm을 strlcpy로 바꿔 NUL 뒤에 이전 이름이 남을 수 있다. map key로 쓰려면 NUL 뒤를 지운다.
static __always_inline void leader_comm(struct task_struct *task, char *comm) {
	struct task_struct *leader = task->group_leader;
	__builtin_memcpy(comm, leader->comm, TASK_COMM_LEN);
	int ended = 0;
#pragma unroll
	for (int index = 0; index < TASK_COMM_LEN; index++) {
		if (ended) comm[index] = 0;
		else if (!comm[index]) ended = 1;
	}
}

// selected는 필터를 통과한 process인지 본다. 필터가 없으면 map을 보지 않아, 대상이 아닌 호출의 비용을 줄인다.
static __always_inline int selected(struct task_struct *task, __u32 tgid) {
	if (self_tgid && tgid == self_tgid) return 0;
	if (filter_cgroup) {
		__u64 cgroup_id = bpf_get_current_cgroup_id();
		if (!bpf_map_lookup_elem(&allowed_cgroups, &cgroup_id)) return 0;
	}
	if (filter_process && !bpf_map_lookup_elem(&allowed_pids, &tgid)) {
		struct syscall_comm key = {};
		leader_comm(task, key.comm);
		if (!bpf_map_lookup_elem(&allowed_comms, &key)) return 0;
	}
	return 1;
}

// x86에서만 thread_info에 status가 있다. arm64 kernel에는 이 field가 없어 늘 64비트 호출로 센다.
static __always_inline __u32 syscall_nr(struct task_struct *task, long id) {
	__u32 nr = (__u32)id;
	struct task_struct___x86 *x86 = (void *)task;
	if (bpf_core_field_exists(x86->thread_info.status) && (x86->thread_info.status & TS_COMPAT)) nr |= SYSCALL_COMPAT;
	return nr;
}

static __always_inline struct syscall_stats *stats_for(struct task_struct *task, __u32 nr, struct syscall_key *key) {
	if (key_cgroup) key->cgroup_id = bpf_get_current_cgroup_id();
	leader_comm(task, key->comm);
	key->nr = nr;
	struct syscall_stats *value = bpf_map_lookup_elem(&stats, key);
	if (value) return value;
	struct syscall_stats zero = {};
	bpf_map_update_elem(&stats, key, &zero, BPF_NOEXIST);
	value = bpf_map_lookup_elem(&stats, key);
	if (!value) count(SYSCALL_STATS_FULL);
	return value;
}

static __always_inline void count_errno(struct syscall_key *owner, __u32 err) {
	struct syscall_errno_key key = {.owner = *owner, .err = err};
	__u64 *value = bpf_map_lookup_elem(&errnos, &key);
	if (value) {
		__sync_fetch_and_add(value, 1);
		return;
	}
	__u64 one = 1;
	if (!bpf_map_update_elem(&errnos, &key, &one, BPF_NOEXIST)) return;
	// 다른 CPU가 먼저 같은 key를 넣었으면 그 값에 더한다. 그래도 없으면 map이 찬 것이다.
	value = bpf_map_lookup_elem(&errnos, &key);
	if (value) __sync_fetch_and_add(value, 1);
	else count(SYSCALL_ERRNOS_FULL);
}

static __always_inline struct syscall_rule *rule_for(__u32 nr) {
	__u32 index = nr & ~SYSCALL_COMPAT;
	if (index >= SYSCALL_NR_MAX) return 0;
	if (nr & SYSCALL_COMPAT) index += SYSCALL_NR_MAX;
	return bpf_map_lookup_elem(&rules, &index);
}

// syscall_arg는 복귀 시점의 register에서 인자 하나를 읽는다. BTF pointer는 상수 위치로만 읽을 수 있어 위치마다 나눈다.
static __always_inline __u64 syscall_arg(void *regs, __u8 index) {
	struct pt_regs___x86 *x86 = regs;
	if (bpf_core_field_exists(x86->di)) {
		switch (index) {
		case 0: return x86->di;
		case 1: return x86->si;
		case 2: return x86->dx;
		case 3: return x86->r10;
		case 4: return x86->r8;
		default: return x86->r9;
		}
	}
	struct pt_regs___arm64 *arm64 = regs;
	if (bpf_core_field_exists(arm64->orig_x0)) {
		switch (index) {
		case 0: return arm64->orig_x0;
		case 1: return arm64->regs[1];
		case 2: return arm64->regs[2];
		case 3: return arm64->regs[3];
		case 4: return arm64->regs[4];
		default: return arm64->regs[5];
		}
	}
	return 0;
}

static __always_inline int event_wanted(__u64 latency, long ret) {
	if (slow_ns && latency >= slow_ns) return 1;
	if (event_errors == SYSCALL_EVENTS_OFF || ret >= 0 || ret < -MAX_ERRNO) return 0;
	if (event_errors == SYSCALL_EVENTS_ALL_ERRORS) return 1;
	__u32 err = (__u32)-ret;
	__u8 *allowed = bpf_map_lookup_elem(&event_errnos, &err);
	return allowed && *allowed;
}

// emit은 호출 하나를 event로 낸다. 경로는 낼 때만 사용자 memory에서 읽는다. 복귀 시점에는 kernel이 방금 그 page를 읽었다.
static __always_inline void emit(__u64 *ctx, struct task_struct *task, __u32 tgid, __u32 nr, __u64 latency, long ret) {
	__u32 zero = 0;
	__u64 now = bpf_ktime_get_ns();
	struct syscall_window *window = bpf_map_lookup_elem(&event_windows, &zero);
	if (!window) return;
	if (now - window->start_ns >= NSEC_PER_SEC) {
		window->start_ns = now;
		window->sent = 0;
	}
	if (window->sent >= event_limit) {
		window->suppressed++;
		return;
	}
	struct syscall_event *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		count(SYSCALL_RING_LOST);
		return;
	}
	window->sent++;
	record->timestamp_ns = now;
	record->latency_ns = latency;
	record->ret = ret;
	record->tgid = tgid;
	record->tid = (__u32)bpf_get_current_pid_tgid();
	record->nr = nr;
	record->path_state = 0;
	leader_comm(task, record->comm);
	record->path[0] = 0;
	struct syscall_rule *rule = rule_for(nr);
	// 32비트 호환 호출은 인자가 다른 register에 있어 경로를 읽지 않는다.
	if (rule && rule->path_arg && !(nr & SYSCALL_COMPAT) && !(rule->path_gone && ret == 0)) {
		const void *path = (const void *)syscall_arg((void *)ctx[0], rule->path_arg - 1);
		long length = bpf_probe_read_user_str(record->path, sizeof(record->path), path);
		if (length < 0) record->path_state = 2;
		else if (length == sizeof(record->path)) {
			// helper가 돌려주는 길이는 NUL을 포함하므로, 꼭 맞는 255 byte 경로와 잘린 긴 경로가 모두 256이다. 사용자
			// memory의 다음 byte가 NUL이면 꼭 맞은 것이다. 그 byte를 읽지 못하면 잘린 것으로 둔다.
			char next = 1;
			bpf_probe_read_user(&next, 1, path + sizeof(record->path) - 1);
			record->path_state = next ? 3 : 1;
		} else record->path_state = 1;
	}
	bpf_ringbuf_submit(record, 0);
}

SEC("tp_btf/sys_enter") int syscall_enter(__u64 *ctx) {
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	struct task_struct *task = bpf_get_current_task_btf();
	if (!selected(task, tgid)) return 0;
	__u32 nr = syscall_nr(task, (long)ctx[1]);
	if (filter_syscall) {
		struct syscall_rule *rule = rule_for(nr);
		if (!rule || !rule->allowed) return 0;
	}
	if (nr == exit_nr || nr == exit_group_nr || nr == compat_exit_nr || nr == compat_exit_group_nr) {
		struct syscall_key key = {};
		struct syscall_stats *value = stats_for(task, nr, &key);
		if (value) value->calls++;
		return 0;
	}
	struct syscall_start *start = bpf_task_storage_get(&starts, task, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (!start) {
		count(SYSCALL_STARTS_FULL);
		return 0;
	}
	start->start_ns = bpf_ktime_get_ns();
	start->tgid = tgid;
	start->nr = nr;
	return 0;
}

SEC("tp_btf/sys_exit") int syscall_exit(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	// 시작 시각은 진입에서 필터를 통과한 호출에만 있으므로, 복귀에서는 필터를 다시 보지 않는다.
	struct syscall_start *start = bpf_task_storage_get(&starts, task, 0, 0);
	if (!start || !start->start_ns) {
		// trace를 붙이기 전에 시작한 호출, 그리고 fork와 clone으로 생긴 thread가 처음 돌아오는 복귀다. 필터가 있으면
		// 대상이 아닌 호출과 구분하려고 모든 복귀에서 필터를 다시 봐야 하므로 세지 않는다. --syscall은 복귀에서 번호를
		// 알 수 없어 구분하지 못한다.
		if (!filter_process && !filter_cgroup && !filter_syscall && selected(task, bpf_get_current_pid_tgid() >> 32)) count(SYSCALL_UNMATCHED);
		return 0;
	}
	__u64 latency = bpf_ktime_get_ns() - start->start_ns;
	__u32 nr = start->nr, tgid = start->tgid;
	start->start_ns = 0;
	struct syscall_key key = {};
	struct syscall_stats *value = stats_for(task, nr, &key);
	if (!value) return 0;
	long ret = (long)ctx[1];
	value->calls++;
	value->timed++;
	value->sum_ns += latency;
	if (latency > value->max_ns) value->max_ns = latency;
	hist_add(value->hist, latency / 1000);
	if (ret < 0 && ret >= -MAX_ERRNO) {
		value->errors++;
		count_errno(&key, (__u32)-ret);
	}
	if (event_wanted(latency, ret)) emit(ctx, task, tgid, nr, latency, ret);
	return 0;
}

// trace를 뗀 뒤 모든 thread를 돌며, 시작 시각이 남은 호출을 진행 중으로 낸다. 그 thread는 살아 있으므로 이름도 함께 읽는다.
SEC("iter/task") int syscall_iter_in_flight(struct bpf_iter__task *ctx) {
	struct task_struct *task = ctx->task;
	if (!task) return 0;
	struct syscall_start *start = bpf_task_storage_get(&starts, task, 0, 0);
	if (!start || !start->start_ns) return 0;
	struct syscall_in_flight record = {.start_ns = start->start_ns, .tgid = start->tgid, .nr = start->nr};
	leader_comm(task, record.comm);
	bpf_seq_write(ctx->meta->seq, &record, sizeof(record));
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
