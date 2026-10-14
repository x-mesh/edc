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

// 이 객체는 trace exec만 불러온다. exec, exit, signal 세 tracepoint로 process의 시작과 끝, 그리고 누가 신호를 보냈는지 본다.
// 세 tracepoint 모두 task_struct를 직접 주므로 map 없이 기록 하나씩 낸다. oom/mark_victim은 OOM killer가 고른 victim을 본다.

#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_RINGBUF 27
#define TASK_COMM_LEN 16
#define EXEC_ARGS_MAX 256
#define PF_KTHREAD 0x00200000
#define EXEC_KIND_EXEC 0
#define EXEC_KIND_EXIT 1
#define EXEC_KIND_SIGNAL 2
#define EXEC_KIND_OOM 3
#define EXEC_FLAG_ARGS_TRUNCATED 1
#define EXEC_FLAG_CORE_DUMPED 2
#define EXEC_FLAG_FROM_KERNEL 4
#define EXEC_FLAG_OOM_KILLED 8
#define EXEC_FLAG_OOM_DETAIL 16

struct kernfs_node { unsigned long long id; } __attribute__((preserve_access_index));
struct cgroup { struct kernfs_node *kn; } __attribute__((preserve_access_index));
struct css_set { struct cgroup *dfl_cgrp; } __attribute__((preserve_access_index));
struct mm_struct { unsigned long arg_start; unsigned long arg_end; } __attribute__((preserve_access_index));
struct signal_struct { struct mm_struct *oom_mm; } __attribute__((preserve_access_index));
struct task_struct {
	int pid;
	int tgid;
	unsigned int flags;
	struct task_struct *real_parent;
	unsigned long long start_boottime;
	int exit_code;
	char comm[TASK_COMM_LEN];
	struct mm_struct *mm;
	struct css_set *cgroups;
	struct signal_struct *signal;
} __attribute__((preserve_access_index));
struct linux_binprm { const char *filename; } __attribute__((preserve_access_index));
struct kernel_siginfo { struct { int si_signo; int si_errno; int si_code; }; } __attribute__((preserve_access_index));
// 5.15의 mark_victim은 pid만 준다. comm부터 oom_score_adj까지는 6.x에서 들어왔으므로 CO-RE로 있는지 보고 읽는다.
struct trace_event_raw_mark_victim {
	int pid;
	__u32 __data_loc_comm;
	unsigned long total_vm;
	unsigned long anon_rss;
	unsigned long file_rss;
	unsigned long shmem_rss;
	unsigned int uid;
	short oom_score_adj;
} __attribute__((preserve_access_index));

// exec_record는 event 하나다. kind가 signal이면 parent_comm은 보낸 process의 이름이고 code는 신호 번호, signal은 전달 결과다.
// kind가 exit이면 code는 종료 코드, signal은 끝낸 신호, duration_ns는 process가 산 시간이다. args는 exec의 argv다.
// kind가 oom이면 pid와 comm은 victim이고, sender_pid와 parent_comm은 메모리를 할당하다 OOM을 일으킨 task, code는 oom_score_adj다.
struct exec_record {
	__u64 timestamp_ns;
	__u64 cgroup_id;
	__u64 duration_ns;
	__u32 pid;
	__u32 ppid;
	__u32 sender_pid;
	__u32 uid;
	__s32 code;
	__s32 signal;
	char comm[TASK_COMM_LEN];
	char parent_comm[TASK_COMM_LEN];
	__u8 kind;
	__u8 flags;
	__u16 args_len;
	char args[EXEC_ARGS_MAX];
	__u64 total_vm_kb;
	__u64 anon_rss_kb;
	__u64 file_rss_kb;
	__u64 shmem_rss_kb;
};
struct exec_counters { __u64 lost_events; __u64 filtered_signals; __u64 args_truncated; };
const struct exec_record *unused_exec_record __attribute__((unused));
const struct exec_counters *unused_exec_counters __attribute__((unused));

struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 22); } events SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct exec_counters); } counters SEC(".maps");
// signal_mask의 bit n은 신호 n을 기록한다는 뜻이다. Go가 --signal로 정한다.
volatile const __u64 signal_mask = 0;

static __always_inline struct exec_counters *counter(void) { __u32 key = 0; return bpf_map_lookup_elem(&counters, &key); }

static __always_inline __u64 task_cgroup(struct task_struct *task) {
	struct css_set *set = BPF_CORE_READ(task, cgroups);
	if (!set) return 0;
	struct cgroup *group = BPF_CORE_READ(set, dfl_cgrp);
	if (!group) return 0;
	struct kernfs_node *node = BPF_CORE_READ(group, kn);
	if (!node) return 0;
	return BPF_CORE_READ(node, id);
}

static __always_inline struct exec_record *reserve(struct task_struct *task, __u8 kind) {
	struct exec_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		struct exec_counters *c = counter();
		if (c) __sync_fetch_and_add(&c->lost_events, 1);
		return 0;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->cgroup_id = task_cgroup(task);
	record->duration_ns = 0;
	record->pid = BPF_CORE_READ(task, tgid);
	record->sender_pid = 0;
	record->uid = (__u32)bpf_get_current_uid_gid();
	record->code = 0;
	record->signal = 0;
	bpf_core_read(record->comm, TASK_COMM_LEN, &task->comm);
	struct task_struct *parent = BPF_CORE_READ(task, real_parent);
	record->ppid = 0;
	record->parent_comm[0] = 0;
	if (parent) {
		record->ppid = BPF_CORE_READ(parent, tgid);
		bpf_core_read(record->parent_comm, TASK_COMM_LEN, &parent->comm);
	}
	record->kind = kind;
	record->flags = 0;
	record->args_len = 0;
	record->total_vm_kb = 0;
	record->anon_rss_kb = 0;
	record->file_rss_kb = 0;
	record->shmem_rss_kb = 0;
	return record;
}

SEC("tp_btf/sched_process_exec") int exec_process_exec(__u64 *ctx) {
	struct task_struct *task = (struct task_struct *)ctx[0];
	struct linux_binprm *bprm = (struct linux_binprm *)ctx[2];
	struct exec_record *record = reserve(task, EXEC_KIND_EXEC);
	if (!record) return 0;
	// 이 시점에는 새 mm이 들어와 있고 argv가 arg_start부터 arg_end까지 NUL로 나뉘어 있다.
	struct mm_struct *mm = BPF_CORE_READ(task, mm);
	unsigned long start = 0, end = 0;
	if (mm) {
		start = BPF_CORE_READ(mm, arg_start);
		end = BPF_CORE_READ(mm, arg_end);
	}
	__u32 length = 0;
	if (end > start) {
		unsigned long span = end - start;
		if (span > EXEC_ARGS_MAX) {
			length = EXEC_ARGS_MAX;
			record->flags |= EXEC_FLAG_ARGS_TRUNCATED;
		} else {
			length = (__u32)span;
		}
	}
	if (length > 0 && length <= EXEC_ARGS_MAX && bpf_probe_read_user(record->args, length, (const void *)start) == 0) {
		record->args_len = (__u16)length;
	} else {
		// argv를 읽지 못하면 실행 파일 경로라도 남긴다.
		const char *filename = BPF_CORE_READ(bprm, filename);
		long n = bpf_probe_read_kernel_str(record->args, EXEC_ARGS_MAX, filename);
		record->args_len = n > 1 ? (__u16)(n - 1) : 0;
	}
	if (record->flags & EXEC_FLAG_ARGS_TRUNCATED) {
		struct exec_counters *c = counter();
		if (c) __sync_fetch_and_add(&c->args_truncated, 1);
	}
	bpf_ringbuf_submit(record, 0);
	return 0;
}

SEC("tp_btf/sched_process_exit") int exec_process_exit(__u64 *ctx) {
	struct task_struct *task = (struct task_struct *)ctx[0];
	if (BPF_CORE_READ(task, flags) & PF_KTHREAD) return 0;
	// 스레드의 종료는 process의 종료가 아니다. 그룹의 대표 스레드만 본다.
	if (BPF_CORE_READ(task, pid) != BPF_CORE_READ(task, tgid)) return 0;
	struct exec_record *record = reserve(task, EXEC_KIND_EXIT);
	if (!record) return 0;
	int code = BPF_CORE_READ(task, exit_code);
	record->code = (code >> 8) & 0xff;
	record->signal = code & 0x7f;
	if (code & 0x80) record->flags |= EXEC_FLAG_CORE_DUMPED;
	// mark_oom_victim이 thread group이 함께 쓰는 signal_struct에 oom_mm을 남긴다. 그래서 victim이 어느 thread였든 대표 스레드의 종료에서 보인다.
	if (BPF_CORE_READ(task, signal, oom_mm)) record->flags |= EXEC_FLAG_OOM_KILLED;
	// start_boottime은 trace가 시작되기 전에 뜬 process에도 있다. 그래서 exec 기록 없이도 산 시간이 나온다.
	__u64 started = BPF_CORE_READ(task, start_boottime), now = bpf_ktime_get_boot_ns();
	record->duration_ns = now > started ? now - started : 0;
	bpf_ringbuf_submit(record, 0);
	return 0;
}

SEC("tp_btf/signal_generate") int exec_signal_generate(__u64 *ctx) {
	int sig = (int)ctx[0];
	struct kernel_siginfo *info = (struct kernel_siginfo *)ctx[1];
	struct task_struct *task = (struct task_struct *)ctx[2];
	int result = (int)ctx[4];
	if (sig <= 0 || sig >= 64) return 0;
	if (!((signal_mask >> sig) & 1)) {
		struct exec_counters *c = counter();
		if (c) __sync_fetch_and_add(&c->filtered_signals, 1);
		return 0;
	}
	struct exec_record *record = reserve(task, EXEC_KIND_SIGNAL);
	if (!record) return 0;
	// 보낸 쪽은 지금 실행 중인 task다. parent 칸을 보낸 process로 쓴다.
	record->sender_pid = (__u32)(bpf_get_current_pid_tgid() >> 32);
	bpf_get_current_comm(record->parent_comm, TASK_COMM_LEN);
	record->ppid = 0;
	record->code = sig;
	record->signal = result;
	// SEND_SIG_NOINFO(0)와 SEND_SIG_PRIV(1)는 포인터가 아니라 읽기가 실패한다. 그 둘과 양수 si_code는 커널이 만든 신호이고,
	// SI_USER(0)와 음수(SI_TKILL, SI_QUEUE)는 사용자 process가 보낸 신호다.
	int si_code = 0;
	if (bpf_core_read(&si_code, sizeof(si_code), &info->si_code) != 0 || si_code > 0) record->flags |= EXEC_FLAG_FROM_KERNEL;
	bpf_ringbuf_submit(record, 0);
	return 0;
}

SEC("tracepoint/oom/mark_victim") int exec_oom_mark_victim(struct trace_event_raw_mark_victim *ctx) {
	// 이 tracepoint는 OOM을 일으킨 task의 문맥에서 불린다. cgroup과 보낸 쪽 칸은 그 task로 채우고, victim은 ctx에서 읽는다.
	struct task_struct *current = (struct task_struct *)bpf_get_current_task();
	struct exec_record *record = reserve(current, EXEC_KIND_OOM);
	if (!record) return 0;
	record->sender_pid = (__u32)(bpf_get_current_pid_tgid() >> 32);
	bpf_get_current_comm(record->parent_comm, TASK_COMM_LEN);
	record->ppid = 0;
	record->pid = (__u32)ctx->pid;
	record->comm[0] = 0;
	record->uid = 0;
	if (bpf_core_field_exists(ctx->anon_rss)) {
		record->flags |= EXEC_FLAG_OOM_DETAIL;
		__u32 loc = ctx->__data_loc_comm;
		bpf_probe_read_kernel_str(record->comm, TASK_COMM_LEN, (void *)ctx + (loc & 0xffff));
		record->total_vm_kb = ctx->total_vm;
		record->anon_rss_kb = ctx->anon_rss;
		record->file_rss_kb = ctx->file_rss;
		record->shmem_rss_kb = ctx->shmem_rss;
		record->uid = ctx->uid;
		record->code = ctx->oom_score_adj;
	}
	bpf_ringbuf_submit(record, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
