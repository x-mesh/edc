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

// 이 객체는 trace priv만 불러온다. capability 검사의 결과를 (process, capability, 결과)별로 kernel 안에서 세고, 거부된
// 검사, uid와 gid를 바꾼 자격 변경, kernel module 적재는 event로 하나씩 보낸다.

#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_PERCPU_HASH 5
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_NOEXIST 1
#define TASK_COMM_LEN 16
#define MODULE_NAME_LEN 56
#define NSEC_PER_SEC 1000000000ULL
// include/linux/security.h의 CAP_OPT_NOAUDIT다. 결과를 기록하지 말라는 검사로, 거부되어도 동작을 고르는 데만 쓴다.
#define CAP_OPT_NOAUDIT 2

enum priv_kind { PRIV_CAP_DENIED, PRIV_CRED, PRIV_MODULE };
enum priv_counter { PRIV_STATS_FULL, PRIV_RING_LOST, PRIV_COUNTERS };

typedef struct { __u32 val; } kuid_t;
typedef struct { __u32 val; } kgid_t;
struct cred { kuid_t uid; kgid_t gid; kuid_t euid; kgid_t egid; } __attribute__((preserve_access_index));
struct task_struct { const struct cred *cred; struct task_struct *group_leader; char comm[TASK_COMM_LEN]; } __attribute__((preserve_access_index));
struct module { char name[MODULE_NAME_LEN]; } __attribute__((preserve_access_index));

struct priv_key { __u64 cgroup_id; char comm[TASK_COMM_LEN]; __u32 cap; __u8 denied; __u8 noaudit; __u8 pad[2]; };
struct priv_comm { char comm[TASK_COMM_LEN]; };
struct priv_counters { __u64 values[PRIV_COUNTERS]; };
struct priv_window { __u64 start_ns; __u64 sent; __u64 suppressed; };
// ids는 바꾸기 전과 뒤의 uid, euid, gid, egid다. kind가 module이면 name이 module 이름이다.
struct priv_event { __u64 timestamp_ns; __u64 cgroup_id; __u32 kind; __u32 tgid; __u32 tid; __s32 cap; __u32 ids[8]; char comm[TASK_COMM_LEN]; char name[MODULE_NAME_LEN]; };
const struct priv_key *unused_priv_key __attribute__((unused));
const struct priv_comm *unused_priv_comm __attribute__((unused));
const struct priv_counters *unused_priv_counters __attribute__((unused));
const struct priv_window *unused_priv_window __attribute__((unused));
const struct priv_event *unused_priv_event __attribute__((unused));

struct { __uint(type, BPF_MAP_TYPE_PERCPU_HASH); __uint(max_entries, 4096); __type(key, struct priv_key); __type(value, __u64); } checks SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 1024); __type(key, __u32); __type(value, __u8); } allowed_pids SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 64); __type(key, struct priv_comm); __type(value, __u8); } allowed_comms SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u8); } allowed_cgroups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct priv_counters); } counters SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 20); } events SEC(".maps");
// event_windows는 CPU마다 지금 1초 창에서 보낸 거부 event 수다. 거부가 쏟아지면 event만 줄이고 합계는 계속 센다.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct priv_window); } event_windows SEC(".maps");

// self_tgid는 edc 자신이다. edc가 init pid namespace 밖에 있으면 BPF가 보는 tgid를 알 수 없어 0으로 두고 제외하지 않는다.
volatile const __u32 self_tgid = 0;
volatile const __u8 filter_process = 0;
volatile const __u8 filter_cgroup = 0;
volatile const __u32 event_limit = 1000;

static __always_inline void count(__u32 index) {
	__u32 key = 0;
	struct priv_counters *c = bpf_map_lookup_elem(&counters, &key);
	if (c && index < PRIV_COUNTERS) __sync_fetch_and_add(&c->values[index], 1);
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

static __always_inline int selected(struct task_struct *task, __u64 *cgroup_id, char *comm) {
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	if (self_tgid && tgid == self_tgid) return 0;
	*cgroup_id = bpf_get_current_cgroup_id();
	if (filter_cgroup && !bpf_map_lookup_elem(&allowed_cgroups, cgroup_id)) return 0;
	leader_comm(task, comm);
	if (filter_process && !bpf_map_lookup_elem(&allowed_pids, &tgid)) {
		struct priv_comm name = {};
		__builtin_memcpy(name.comm, comm, TASK_COMM_LEN);
		if (!bpf_map_lookup_elem(&allowed_comms, &name)) return 0;
	}
	return 1;
}

static __always_inline struct priv_event *reserve(__u32 kind, __u64 cgroup_id, char *comm) {
	struct priv_event *event = bpf_ringbuf_reserve(&events, sizeof(*event), 0);
	if (!event) {
		count(PRIV_RING_LOST);
		return 0;
	}
	__u64 id = bpf_get_current_pid_tgid();
	event->timestamp_ns = bpf_ktime_get_ns();
	event->cgroup_id = cgroup_id;
	event->kind = kind;
	event->tgid = id >> 32;
	event->tid = (__u32)id;
	event->cap = -1;
#pragma unroll
	for (int index = 0; index < 8; index++) event->ids[index] = 0;
	__builtin_memcpy(event->comm, comm, TASK_COMM_LEN);
	event->name[0] = 0;
	return event;
}

// cap_capable은 capable()과 ns_capable()이 LSM을 거쳐 부르는 commoncap 검사다. 0이면 허용, 음수면 거부다.
SEC("fexit/cap_capable") int priv_cap_capable(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	struct priv_key key = {};
	if (!selected(task, &key.cgroup_id, key.comm)) return 0;
	key.cap = (__u32)ctx[2];
	key.noaudit = ((__u32)ctx[3] & CAP_OPT_NOAUDIT) != 0;
	key.denied = (int)ctx[4] != 0;
	__u64 *value = bpf_map_lookup_elem(&checks, &key);
	if (!value) {
		__u64 zero = 0;
		bpf_map_update_elem(&checks, &key, &zero, BPF_NOEXIST);
		value = bpf_map_lookup_elem(&checks, &key);
	}
	if (value) (*value)++;
	else count(PRIV_STATS_FULL);
	if (!key.denied || key.noaudit) return 0;
	__u32 zero = 0;
	struct priv_window *window = bpf_map_lookup_elem(&event_windows, &zero);
	if (!window) return 0;
	__u64 now = bpf_ktime_get_ns();
	if (now - window->start_ns >= NSEC_PER_SEC) {
		window->start_ns = now;
		window->sent = 0;
	}
	if (window->sent >= event_limit) {
		window->suppressed++;
		return 0;
	}
	struct priv_event *event = reserve(PRIV_CAP_DENIED, key.cgroup_id, key.comm);
	if (!event) return 0;
	window->sent++;
	event->cap = (__s32)key.cap;
	bpf_ringbuf_submit(event, 0);
	return 0;
}

// commit_creds는 자격을 바꾸는 모든 길이 지나는 곳이다. setuid 계열 syscall과 setuid program의 exec가 여기서 바뀐다.
// uid, euid, gid, egid 중 하나라도 바뀔 때만 보낸다.
SEC("fentry/commit_creds") int priv_commit_creds(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	const struct cred *old = BPF_CORE_READ(task, cred);
	const struct cred *new = (const struct cred *)ctx[0];
	__u32 ids[8] = {
		BPF_CORE_READ(old, uid.val), BPF_CORE_READ(new, uid.val), BPF_CORE_READ(old, euid.val), BPF_CORE_READ(new, euid.val),
		BPF_CORE_READ(old, gid.val), BPF_CORE_READ(new, gid.val), BPF_CORE_READ(old, egid.val), BPF_CORE_READ(new, egid.val),
	};
	if (ids[0] == ids[1] && ids[2] == ids[3] && ids[4] == ids[5] && ids[6] == ids[7]) return 0;
	__u64 cgroup_id = 0;
	char comm[TASK_COMM_LEN] = {};
	if (!selected(task, &cgroup_id, comm)) return 0;
	struct priv_event *event = reserve(PRIV_CRED, cgroup_id, comm);
	if (!event) return 0;
#pragma unroll
	for (int index = 0; index < 8; index++) event->ids[index] = ids[index];
	bpf_ringbuf_submit(event, 0);
	return 0;
}

SEC("tp_btf/module_load") int priv_module_load(__u64 *ctx) {
	struct task_struct *task = bpf_get_current_task_btf();
	__u64 cgroup_id = 0;
	char comm[TASK_COMM_LEN] = {};
	if (!selected(task, &cgroup_id, comm)) return 0;
	struct priv_event *event = reserve(PRIV_MODULE, cgroup_id, comm);
	if (!event) return 0;
	struct module *mod = (struct module *)ctx[0];
	bpf_probe_read_kernel_str(event->name, MODULE_NAME_LEN, &mod->name);
	bpf_ringbuf_submit(event, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
