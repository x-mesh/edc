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
#define BPF_MAP_TYPE_RINGBUF 27
#define NSEC_PER_MSEC 1000000ULL
#define IO_PENDING_MAX 65536
#define IO_COUNTER_MAX 5
#define BPF_NOEXIST 1

enum io_counter { IO_MAP_FULL, IO_UNMATCHED_COMPLETE, IO_INCOMPLETE, IO_REQUEUE, IO_RING_LOST };
enum io_stage { IO_INSERT = 1, IO_ISSUE = 2 };

struct block_device { __u32 bd_dev; } __attribute__((preserve_access_index));
struct request { __u64 q; __u64 mq_ctx; __u64 mq_hctx; __u32 cmd_flags; __u32 rq_flags; int tag; int internal_tag; __u32 timeout; __u32 __data_len; __u64 __sector; __u64 bio; __u64 biotail; __u64 queuelist; struct block_device *part; } __attribute__((preserve_access_index));

struct io_pending {
	__u64 insert_ns; __u64 issue_ns; __u64 cgroup_id; __u64 bytes; __u32 dev; __u32 pid; char comm[16]; char rwbs[8]; __u8 stage;
};
struct io_record {
	__u64 timestamp_ns; __u64 cgroup_id; __u64 bytes; __u64 queue_ns; __u64 service_ns; __u64 total_ns; __u32 dev; __u32 pid; char comm[16]; char rwbs[8]; __u8 attribution;
};
const struct io_record *unused_io_record __attribute__((unused));
struct io_counters { __u64 values[IO_COUNTER_MAX]; };
const struct io_counters *unused_io_counters __attribute__((unused));

struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, IO_PENDING_MAX); __type(key, void *); __type(value, struct io_pending); } pending SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 22); } events SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct io_counters); } counters SEC(".maps");
volatile const __u64 minimum_latency_ns = NSEC_PER_MSEC;

static __always_inline void count(__u32 index) {
	__u32 key = 0; struct io_counters *counts = bpf_map_lookup_elem(&counters, &key); if (counts) __sync_fetch_and_add(&counts->values[index], 1);
}
static __always_inline void remember(struct request *rq, __u8 stage) {
	struct io_pending value = {}; struct block_device *part = BPF_CORE_READ(rq, part); if (stage == IO_INSERT) value.insert_ns = bpf_ktime_get_ns(); else value.issue_ns = bpf_ktime_get_ns(); value.dev = BPF_CORE_READ(part, bd_dev); value.bytes = BPF_CORE_READ(rq, __data_len); value.pid = bpf_get_current_pid_tgid() >> 32; value.cgroup_id = bpf_get_current_cgroup_id(); value.stage = stage; bpf_get_current_comm(&value.comm, sizeof(value.comm)); if (BPF_CORE_READ(rq, cmd_flags) & 1) value.rwbs[0] = 'W'; else value.rwbs[0] = 'R';
	if (bpf_map_update_elem(&pending, &rq, &value, BPF_NOEXIST)) count(IO_MAP_FULL);
}
SEC("tp_btf/block_rq_insert") int insert(unsigned long long *ctx) { struct request *rq = (void *)ctx[0]; remember(rq, IO_INSERT); return 0; }
SEC("tp_btf/block_rq_issue") int issue(unsigned long long *ctx) { struct request *rq = (void *)ctx[0];
	struct io_pending *value = bpf_map_lookup_elem(&pending, &rq); if (!value) { remember(rq, IO_ISSUE); return 0; }
	value->issue_ns = bpf_ktime_get_ns(); value->stage = IO_ISSUE; return 0;
}
SEC("tp_btf/block_rq_requeue") int requeue(unsigned long long *ctx) { struct request *rq = (void *)ctx[0]; count(IO_REQUEUE); bpf_map_delete_elem(&pending, &rq); return 0; }
SEC("tp_btf/block_rq_complete") int complete(unsigned long long *ctx) { struct request *rq = (void *)ctx[0];
	struct io_pending *value = bpf_map_lookup_elem(&pending, &rq); if (!value) { count(IO_UNMATCHED_COMPLETE); return 0; }
	__u64 now = bpf_ktime_get_ns(); if (value->stage != IO_ISSUE || !value->insert_ns || !value->issue_ns) { count(IO_INCOMPLETE); bpf_map_delete_elem(&pending, &rq); return 0; }
	__u64 total = now - value->insert_ns; if (total < minimum_latency_ns) { bpf_map_delete_elem(&pending, &rq); return 0; }
	struct io_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0); if (!record) { count(IO_RING_LOST); bpf_map_delete_elem(&pending, &rq); return 0; }
	record->timestamp_ns = now; record->cgroup_id = value->cgroup_id; record->bytes = value->bytes; record->queue_ns = value->issue_ns - value->insert_ns; record->service_ns = now - value->issue_ns; record->total_ns = total; record->dev = value->dev; record->pid = value->pid; __builtin_memcpy(record->comm, value->comm, sizeof(record->comm)); __builtin_memcpy(record->rwbs, value->rwbs, sizeof(record->rwbs)); record->attribution = IO_INSERT; bpf_ringbuf_submit(record, 0); bpf_map_delete_elem(&pending, &rq); return 0;
}
char LICENSE[] SEC("license") = "Dual BSD/GPL";
