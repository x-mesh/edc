//go:build ignore

// SPDX-License-Identifier: GPL-2.0
typedef unsigned int __u32;
typedef unsigned long long __u64;
typedef unsigned char __u8;
typedef unsigned short __u16;
typedef signed int __s32;
typedef signed long long __s64;
typedef __u16 __be16;
typedef __u32 __be32;
typedef __u32 __wsum;
#include "bpf_helpers.h"
#if defined(__TARGET_ARCH_x86) || defined(__x86_64__)
struct pt_regs { __u64 r15,r14,r13,r12,bp,bx,r11,r10,r9,r8,ax,cx,dx,si,di,orig_ax,ip,cs,flags,sp,ss; };
#define PT_REGS_PARM1(x) ((x)->di)
#define PT_REGS_PARM2(x) ((x)->si)
#define PT_REGS_PARM4(x) ((x)->cx)
#define PT_REGS_RC(x) ((x)->ax)
#elif defined(__TARGET_ARCH_arm64) || defined(__aarch64__)
struct pt_regs { __u64 regs[31], sp, pc, pstate; };
#define PT_REGS_PARM1(x) ((x)->regs[0])
#define PT_REGS_PARM2(x) ((x)->regs[1])
#define PT_REGS_PARM4(x) ((x)->regs[3])
#define PT_REGS_RC(x) ((x)->regs[0])
#endif
#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_ANY 0
#define DATA_MAX 16384
enum kind { KIND_READ, KIND_WRITE, KIND_CLOSE };
struct tls_plaintext_record { __u64 timestamp_ns, ssl; __u32 tgid, tid, kind, data_len, original_len, pad; char comm[16]; char data[DATA_MAX]; };
struct tls_plaintext_record *unused_record __attribute__((unused));
struct args { __u64 ssl, buf, out_len; };
const volatile __u32 target_pid = 0;
const volatile __u8 allow_all = 0;
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u8); } allowed_cgroups SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, struct args); } reads SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, struct args); } writes SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, struct args); } reads_ex SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, struct args); } writes_ex SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 1 << 24); } events SEC(".maps");
static __always_inline int enter(void *map, struct pt_regs *ctx, __u64 out) {
 __u64 id=bpf_get_current_pid_tgid();
 if (target_pid) { if (target_pid != id>>32) return 0; }
 else if (!allow_all) { __u64 cgroup=bpf_get_current_cgroup_id(); if (!bpf_map_lookup_elem(&allowed_cgroups,&cgroup)) return 0; }
 struct args a={PT_REGS_PARM1(ctx),PT_REGS_PARM2(ctx),out}; bpf_map_update_elem(map,&id,&a,BPF_ANY); return 0;
}
static __always_inline int leave(void *map, struct pt_regs *ctx, __u32 kind, int ex) {
 __u64 id=bpf_get_current_pid_tgid(); struct args *a=bpf_map_lookup_elem(map,&id); if (!a) return 0;
 __u64 ssl=a->ssl, buf=a->buf, out=a->out_len, len=0; long rc=PT_REGS_RC(ctx);
 if (ex) { if (rc==1 && out) bpf_probe_read_user(&len,sizeof(len),(void*)out); } else if (rc>0) len=rc;
 bpf_map_delete_elem(map,&id); if (!len) return 0; __u32 n=len>DATA_MAX?DATA_MAX:len;
 struct tls_plaintext_record *r=bpf_ringbuf_reserve(&events,sizeof(*r),0); if (!r) return 0;
 r->timestamp_ns=bpf_ktime_get_ns(); r->ssl=ssl; r->tgid=id>>32; r->tid=id; r->kind=kind; r->data_len=n; r->original_len=len>0xffffffffULL?0xffffffffU:len; r->pad=0; bpf_get_current_comm(r->comm,sizeof(r->comm));
 if (bpf_probe_read_user(r->data,n,(void*)buf)) { bpf_ringbuf_discard(r,0); return 0; } bpf_ringbuf_submit(r,0); return 0;
}
SEC("uprobe/SSL_read") int tls_read_enter(struct pt_regs*c){__u64 id=bpf_get_current_pid_tgid();if(bpf_map_lookup_elem(&reads_ex,&id))return 0;return enter(&reads,c,0);} SEC("uretprobe/SSL_read") int tls_read_exit(struct pt_regs*c){return leave(&reads,c,KIND_READ,0);}
SEC("uprobe/SSL_write") int tls_write_enter(struct pt_regs*c){__u64 id=bpf_get_current_pid_tgid();if(bpf_map_lookup_elem(&writes_ex,&id))return 0;return enter(&writes,c,0);} SEC("uretprobe/SSL_write") int tls_write_exit(struct pt_regs*c){return leave(&writes,c,KIND_WRITE,0);}
SEC("uprobe/SSL_read_ex") int tls_read_ex_enter(struct pt_regs*c){return enter(&reads_ex,c,PT_REGS_PARM4(c));} SEC("uretprobe/SSL_read_ex") int tls_read_ex_exit(struct pt_regs*c){return leave(&reads_ex,c,KIND_READ,1);}
SEC("uprobe/SSL_write_ex") int tls_write_ex_enter(struct pt_regs*c){return enter(&writes_ex,c,PT_REGS_PARM4(c));} SEC("uretprobe/SSL_write_ex") int tls_write_ex_exit(struct pt_regs*c){return leave(&writes_ex,c,KIND_WRITE,1);}
SEC("uprobe/SSL_free") int tls_close(struct pt_regs*c){ __u64 id=bpf_get_current_pid_tgid(); if(target_pid){if(target_pid!=id>>32)return 0;}else if(!allow_all){__u64 cgroup=bpf_get_current_cgroup_id();if(!bpf_map_lookup_elem(&allowed_cgroups,&cgroup))return 0;} struct tls_plaintext_record*r=bpf_ringbuf_reserve(&events,sizeof(*r),0);if(!r)return 0;r->timestamp_ns=bpf_ktime_get_ns();r->ssl=PT_REGS_PARM1(c);r->tgid=id>>32;r->tid=id;r->kind=KIND_CLOSE;r->data_len=0;r->original_len=0;r->pad=0;bpf_get_current_comm(r->comm,sizeof(r->comm));bpf_ringbuf_submit(r,0);return 0;}
char LICENSE[] SEC("license")="GPL";
