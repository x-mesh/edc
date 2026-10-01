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
#define TASK_COMM_LEN 16
#define MAX_PENDING_NS 30000000000ULL

struct kernfs_node { unsigned long long pad[8]; unsigned long long id; } __attribute__((preserve_access_index));
struct cgroup { unsigned long long pad[8]; struct kernfs_node *kn; } __attribute__((preserve_access_index));
struct css_set { unsigned long long pad[2]; struct cgroup *dfl_cgrp; } __attribute__((preserve_access_index));
struct task_struct { unsigned long long pad[40]; int pid; int tgid; unsigned long long pad2[14]; unsigned long long start_boottime; char pad3[128]; char comm[TASK_COMM_LEN]; unsigned long long pad4[64]; struct css_set *cgroups; } __attribute__((preserve_access_index));
struct pending { unsigned long long timestamp_ns; unsigned long long start_boottime; unsigned long long cgroup_id; char comm[TASK_COMM_LEN]; };
struct offcpu { unsigned long long timestamp_ns; unsigned long long start_boottime; unsigned long long cgroup_id; char comm[TASK_COMM_LEN]; unsigned char klass; };
struct task_key { unsigned int pid; unsigned long long start_boottime; };
struct sched_record { unsigned long long timestamp_ns; unsigned long long latency_ns; unsigned long long cgroup_id; unsigned long long start_boottime; unsigned int pid; char comm[TASK_COMM_LEN]; unsigned char kind; unsigned char offcpu_class; unsigned short reserved; };
struct sched_counters { unsigned long long lost_events; unsigned long long map_full; unsigned long long unmatched; unsigned long long repeated_wakeups; unsigned long long below_threshold; };
const struct sched_record *unused_sched_record __attribute__((unused));
const struct sched_counters *unused_sched_counters __attribute__((unused));
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 20); } events SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 16384); __type(key, struct task_key); __type(value, struct pending); } wakeups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 16384); __type(key, struct task_key); __type(value, struct offcpu); } offcpus SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 1); __type(key, unsigned int); __type(value, struct sched_counters); } counters SEC(".maps");
volatile const unsigned long long minimum_latency_ns = 1000000ULL;
static __always_inline struct sched_counters *counter(void) { unsigned int key = 0; return bpf_map_lookup_elem(&counters, &key); }
static __always_inline int task_key(struct task_struct *task, struct task_key *key, unsigned long long *cgroup_id, char *comm) { if (!bpf_core_field_exists(task->start_boottime) || !bpf_core_field_exists(task->cgroups) || !bpf_core_field_exists(((struct css_set *)0)->dfl_cgrp) || !bpf_core_field_exists(((struct cgroup *)0)->kn) || !bpf_core_field_exists(((struct kernfs_node *)0)->id)) return -1; key->pid=BPF_CORE_READ(task,pid); key->start_boottime=BPF_CORE_READ(task,start_boottime); struct css_set *set=BPF_CORE_READ(task,cgroups); if (!set) return -1; struct cgroup *group=BPF_CORE_READ(set,dfl_cgrp); if (!group) return -1; struct kernfs_node *node=BPF_CORE_READ(group,kn); if (!node) return -1; *cgroup_id=BPF_CORE_READ(node,id); bpf_core_read(comm, TASK_COMM_LEN, &task->comm); return 0; }
static __always_inline void emit(struct task_key *key, unsigned long long cgroup_id, char *comm, unsigned char kind, unsigned char klass, unsigned long long start) { unsigned long long now = bpf_ktime_get_ns(), latency = now - start; if (latency < minimum_latency_ns) { struct sched_counters *c = counter(); if (c) __sync_fetch_and_add(&c->below_threshold, 1); return; } struct sched_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0); if (!record) { struct sched_counters *c = counter(); if (c) __sync_fetch_and_add(&c->lost_events, 1); return; } record->timestamp_ns=now; record->latency_ns=latency; record->cgroup_id=cgroup_id; record->start_boottime=key->start_boottime; record->pid=key->pid; __builtin_memcpy(record->comm, comm, TASK_COMM_LEN); record->kind=kind; record->offcpu_class=klass; record->reserved=0; bpf_ringbuf_submit(record, 0); }
static __always_inline int wakeup(struct task_struct *task) { struct task_key key={}; unsigned long long cgroup_id=0,now=bpf_ktime_get_ns(); char comm[TASK_COMM_LEN]={}; if(task_key(task,&key,&cgroup_id,comm)) return 0; struct pending *old=bpf_map_lookup_elem(&wakeups,&key); if (old && now-old->timestamp_ns<MAX_PENDING_NS) { struct sched_counters *c=counter(); if(c) __sync_fetch_and_add(&c->repeated_wakeups,1); return 0; } if (old) { struct sched_counters *c=counter(); if(c) __sync_fetch_and_add(&c->unmatched,1); } struct pending value={.timestamp_ns=now,.start_boottime=key.start_boottime,.cgroup_id=cgroup_id}; __builtin_memcpy(value.comm,comm,TASK_COMM_LEN); if (bpf_map_update_elem(&wakeups,&key,&value,0)) { struct sched_counters*c=counter(); if(c)__sync_fetch_and_add(&c->map_full,1); } return 0; }
SEC("tp_btf/sched_wakeup") int sched_wakeup(__u64 *ctx) { return wakeup((struct task_struct *)ctx[0]); }
SEC("tp_btf/sched_wakeup_new") int sched_wakeup_new(__u64 *ctx) { return wakeup((struct task_struct *)ctx[0]); }
SEC("tp_btf/sched_switch") int sched_switch(__u64 *ctx) { _Bool preempt=(_Bool)ctx[0]; struct task_struct *prev=(struct task_struct *)ctx[1], *next=(struct task_struct *)ctx[2]; struct task_key next_key={},prev_key={}; unsigned long long next_cgroup=0,prev_cgroup=0; char next_comm[TASK_COMM_LEN]={},prev_comm[TASK_COMM_LEN]={}; if(!task_key(next,&next_key,&next_cgroup,next_comm)) { struct pending *wake=bpf_map_lookup_elem(&wakeups,&next_key); if(wake) { emit(&next_key,wake->cgroup_id,wake->comm,0,0,wake->timestamp_ns); bpf_map_delete_elem(&wakeups,&next_key); } struct offcpu *off=bpf_map_lookup_elem(&offcpus,&next_key); if(off) { emit(&next_key,off->cgroup_id,off->comm,2,off->klass,off->timestamp_ns); if(off->klass) emit(&next_key,off->cgroup_id,off->comm,1,0,off->timestamp_ns); bpf_map_delete_elem(&offcpus,&next_key); } } if(!task_key(prev,&prev_key,&prev_cgroup,prev_comm) && prev_key.pid>0) { struct offcpu value={.timestamp_ns=bpf_ktime_get_ns(),.start_boottime=prev_key.start_boottime,.cgroup_id=prev_cgroup,.klass=preempt}; __builtin_memcpy(value.comm,prev_comm,TASK_COMM_LEN); if(bpf_map_update_elem(&offcpus,&prev_key,&value,0)) { struct sched_counters*c=counter(); if(c)__sync_fetch_and_add(&c->map_full,1); } } return 0; }
SEC("tp_btf/sched_process_exit") int sched_process_exit(__u64 *ctx) { struct task_struct *task=(struct task_struct *)ctx[0]; struct task_key key={}; unsigned long long cgroup_id; char comm[TASK_COMM_LEN]; if(!task_key(task,&key,&cgroup_id,comm)) { bpf_map_delete_elem(&wakeups,&key); bpf_map_delete_elem(&offcpus,&key); } return 0; }
char LICENSE[] SEC("license") = "Dual BSD/GPL";
