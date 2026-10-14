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

// 이 객체는 trace reclaim만 불러온다. 메모리를 할당하던 task가 회수와 throttle 때문에 멈춘 시간을 잰다.
// 종류마다 시작과 끝을 task local storage로 짝짓고, (cgroup, process 이름, 종류)별로 kernel 안에서 모은다.
// kswapd와 kcompactd 같은 kernel thread의 회수는 할당하던 task를 멈추지 않으므로 세지 않는다.

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
#define NSEC_PER_SEC 1000000000ULL
#define PF_KTHREAD 0x00200000

// direct는 zone이 모자라 할당하던 task가 직접 하는 회수다. memcg는 cgroup의 memory.max나 memory.high에 걸려 하는
// 회수다. throttle은 memory.high를 넘은 task가 사용자 공간으로 돌아가기 전에 회수하고 잠드는 시간 전체다. 그 안의 회수는
// memcg에도 들어간다. compact는 큰 page를 얻으려고 할당하던 task가 직접 하는 compaction이다.
enum reclaim_kind { RECLAIM_DIRECT, RECLAIM_MEMCG, RECLAIM_THROTTLE, RECLAIM_COMPACT, RECLAIM_KINDS };
enum reclaim_counter { RECLAIM_STATS_FULL, RECLAIM_RING_LOST, RECLAIM_COUNTERS };

struct task_struct { unsigned int flags; int pid; int tgid; struct task_struct *group_leader; char comm[TASK_COMM_LEN]; unsigned int memcg_nr_pages_over_high; } __attribute__((preserve_access_index));

struct reclaim_key { __u64 cgroup_id; char comm[TASK_COMM_LEN]; __u32 kind; __u32 pad; };
// bucket i는 [2^i, 2^(i+1)) 마이크로초다. reclaimed는 회수한 page 수이고, throttle과 compact에는 없다.
struct reclaim_stats { __u64 count; __u64 sum_ns; __u64 max_ns; __u64 reclaimed; __u64 hist[HIST_BUCKETS]; };
struct reclaim_start { __u64 start_ns[RECLAIM_KINDS]; };
struct reclaim_comm { char comm[TASK_COMM_LEN]; };
struct reclaim_counters { __u64 values[RECLAIM_COUNTERS]; };
struct reclaim_window { __u64 start_ns; __u64 sent; __u64 suppressed; };
struct reclaim_event { __u64 timestamp_ns; __u64 latency_ns; __u64 reclaimed; __u64 cgroup_id; __u32 tgid; __u32 tid; __u32 kind; __u32 pad; char comm[TASK_COMM_LEN]; };
const struct reclaim_key *unused_reclaim_key __attribute__((unused));
const struct reclaim_stats *unused_reclaim_stats __attribute__((unused));
const struct reclaim_comm *unused_reclaim_comm __attribute__((unused));
const struct reclaim_counters *unused_reclaim_counters __attribute__((unused));
const struct reclaim_window *unused_reclaim_window __attribute__((unused));
const struct reclaim_event *unused_reclaim_event __attribute__((unused));

struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, 4096); __type(key, struct reclaim_key); __type(value, struct reclaim_stats); } stats SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_TASK_STORAGE); __uint(map_flags, BPF_F_NO_PREALLOC); __type(key, int); __type(value, struct reclaim_start); } starts SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 1024); __type(key, __u32); __type(value, __u8); } allowed_pids SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 64); __type(key, struct reclaim_comm); __type(value, __u8); } allowed_comms SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u8); } allowed_cgroups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct reclaim_counters); } counters SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 20); } events SEC(".maps");
// event_windows는 CPU마다 지금 1초 창에서 보낸 event 수다. 회수가 쏟아지면 event만 줄이고 합계는 계속 센다.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct reclaim_window); } event_windows SEC(".maps");

// self_tgid는 edc 자신이다. edc가 init pid namespace 밖에 있으면 BPF가 보는 tgid를 알 수 없어 0으로 두고 제외하지 않는다.
volatile const __u32 self_tgid = 0;
volatile const __u8 filter_process = 0;
volatile const __u8 filter_cgroup = 0;
// slow_ns가 0이 아니면 그 시간 이상 멈춘 것을 event로 낸다.
volatile const __u64 slow_ns = 0;
volatile const __u32 event_limit = 1000;

static __always_inline void count(__u32 index) {
	__u32 key = 0;
	struct reclaim_counters *c = bpf_map_lookup_elem(&counters, &key);
	if (c && index < RECLAIM_COUNTERS) __sync_fetch_and_add(&c->values[index], 1);
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

static __always_inline void begin(__u32 kind) {
	struct task_struct *task = bpf_get_current_task_btf();
	if (task->flags & PF_KTHREAD) return;
	struct reclaim_start *start = bpf_task_storage_get(&starts, task, 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
	if (start && kind < RECLAIM_KINDS) start->start_ns[kind] = bpf_ktime_get_ns();
}

static __always_inline void emit(struct reclaim_key *key, __u32 tgid, __u32 tid, __u64 now, __u64 latency, __u64 reclaimed) {
	__u32 zero = 0;
	struct reclaim_window *window = bpf_map_lookup_elem(&event_windows, &zero);
	if (!window) return;
	if (now - window->start_ns >= NSEC_PER_SEC) {
		window->start_ns = now;
		window->sent = 0;
	}
	if (window->sent >= event_limit) {
		window->suppressed++;
		return;
	}
	struct reclaim_event *event = bpf_ringbuf_reserve(&events, sizeof(*event), 0);
	if (!event) {
		count(RECLAIM_RING_LOST);
		return;
	}
	window->sent++;
	event->timestamp_ns = now;
	event->latency_ns = latency;
	event->reclaimed = reclaimed;
	event->cgroup_id = key->cgroup_id;
	event->tgid = tgid;
	event->tid = tid;
	event->kind = key->kind;
	event->pad = 0;
	__builtin_memcpy(event->comm, key->comm, TASK_COMM_LEN);
	bpf_ringbuf_submit(event, 0);
}

static __always_inline void finish(__u32 kind, __u64 reclaimed) {
	struct task_struct *task = bpf_get_current_task_btf();
	struct reclaim_start *start = bpf_task_storage_get(&starts, task, 0, 0);
	if (!start || kind >= RECLAIM_KINDS || !start->start_ns[kind]) return;
	__u64 now = bpf_ktime_get_ns(), latency = now - start->start_ns[kind];
	start->start_ns[kind] = 0;
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;
	if (self_tgid && tgid == self_tgid) return;
	struct reclaim_key key = { .kind = kind };
	key.cgroup_id = bpf_get_current_cgroup_id();
	if (filter_cgroup && !bpf_map_lookup_elem(&allowed_cgroups, &key.cgroup_id)) return;
	leader_comm(task, key.comm);
	if (filter_process && !bpf_map_lookup_elem(&allowed_pids, &tgid)) {
		struct reclaim_comm name = {};
		__builtin_memcpy(name.comm, key.comm, TASK_COMM_LEN);
		if (!bpf_map_lookup_elem(&allowed_comms, &name)) return;
	}
	struct reclaim_stats *value = bpf_map_lookup_elem(&stats, &key);
	if (!value) {
		struct reclaim_stats zero = {};
		bpf_map_update_elem(&stats, &key, &zero, BPF_NOEXIST);
		value = bpf_map_lookup_elem(&stats, &key);
	}
	if (!value) {
		count(RECLAIM_STATS_FULL);
	} else {
		value->count++;
		value->sum_ns += latency;
		if (latency > value->max_ns) value->max_ns = latency;
		value->reclaimed += reclaimed;
		hist_add(value->hist, latency / 1000);
	}
	if (slow_ns && latency >= slow_ns) emit(&key, tgid, (__u32)id, now, latency, reclaimed);
}

SEC("tp_btf/mm_vmscan_direct_reclaim_begin") int reclaim_direct_begin(__u64 *ctx) { begin(RECLAIM_DIRECT); return 0; }
SEC("tp_btf/mm_vmscan_direct_reclaim_end") int reclaim_direct_end(__u64 *ctx) { finish(RECLAIM_DIRECT, ctx[0]); return 0; }
SEC("tp_btf/mm_vmscan_memcg_reclaim_begin") int reclaim_memcg_begin(__u64 *ctx) { begin(RECLAIM_MEMCG); return 0; }
SEC("tp_btf/mm_vmscan_memcg_reclaim_end") int reclaim_memcg_end(__u64 *ctx) { finish(RECLAIM_MEMCG, ctx[0]); return 0; }
// 이 함수의 인자는 5.15에 없고 6.x에서 gfp_t 하나가 생겼다. 인자를 읽지 않으므로 두 kernel에 함께 붙는다.
// kernel은 사용자 공간으로 돌아갈 때마다 이 함수를 부르고, memory.high를 넘은 page가 없으면 바로 돌아온다. 그 호출은
// throttle이 아니므로 넘은 page가 있을 때만 시작 시각을 잡는다.
SEC("fentry/mem_cgroup_handle_over_high") int reclaim_throttle_begin(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	if (bpf_core_field_exists(task->memcg_nr_pages_over_high) && !task->memcg_nr_pages_over_high) return 0;
	begin(RECLAIM_THROTTLE);
	return 0;
}
SEC("fexit/mem_cgroup_handle_over_high") int reclaim_throttle_end(__u64 *ctx) { finish(RECLAIM_THROTTLE, 0); return 0; }
// compaction의 두 tracepoint는 kernel마다 인자가 달라 읽지 않는다. kcompactd는 begin에서 걸러진다.
SEC("tp_btf/mm_compaction_begin") int reclaim_compact_begin(__u64 *ctx) { begin(RECLAIM_COMPACT); return 0; }
SEC("tp_btf/mm_compaction_end") int reclaim_compact_end(__u64 *ctx) { finish(RECLAIM_COMPACT, 0); return 0; }

char LICENSE[] SEC("license") = "Dual BSD/GPL";
