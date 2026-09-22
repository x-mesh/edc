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

#define AF_INET 2
#define AF_INET6 10
#define IPPROTO_TCP 6
#define IPPROTO_UDP 17
#define BPF_ANY 0
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_RINGBUF 27

struct event {
	__u64 timestamp_ns;
	__u32 event_type;
	__u32 pid;
	__u64 cgroup_id;
	__u64 skaddr;
	__u32 old_state;
	__u32 new_state;
	__u16 family;
	__u16 sport;
	__u16 dport;
	__u16 protocol;
	__u8 source[16];
	__u8 destination[16];
	char comm[16];
	__u64 bytes;
};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 24);
} events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} lost_events SEC(".maps");

struct inet_sock_set_state_ctx {
	__u64 unused;
	const void *skaddr;
	int oldstate;
	int newstate;
	__u16 sport;
	__u16 dport;
	__u16 family;
	__u16 protocol;
	__u8 saddr[4];
	__u8 daddr[4];
	__u8 saddr_v6[16];
	__u8 daddr_v6[16];
};

struct tcp_event_ctx {
	__u64 unused;
	const void *skbaddr;
	const void *skaddr;
	int state;
	__u16 sport;
	__u16 dport;
	__u16 family;
	__u8 saddr[4];
	__u8 daddr[4];
	__u8 saddr_v6[16];
	__u8 daddr_v6[16];
	__u64 sock_cookie;
};

static __always_inline struct event *start_event(void *ctx, __u32 type) {
	struct event *event = bpf_ringbuf_reserve(&events, sizeof(*event), 0);
	if (!event) {
		__u32 key = 0;
		__u64 *lost = bpf_map_lookup_elem(&lost_events, &key);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
		return 0;
	}
	event->timestamp_ns = bpf_ktime_get_ns();
	event->event_type = type;
	event->pid = bpf_get_current_pid_tgid() >> 32;
	event->cgroup_id = bpf_get_current_cgroup_id();
	event->protocol = 0;
	event->bytes = 0;
	bpf_get_current_comm(&event->comm, sizeof(event->comm));
	return event;
}

static __always_inline void finish_event(struct event *event) {
	if (event) {
		bpf_ringbuf_submit(event, 0);
	}
}

SEC("tracepoint/sock/inet_sock_set_state")
int inet_sock_set_state(struct inet_sock_set_state_ctx *ctx) {
	struct event *event = start_event(ctx, 1);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)ctx->skaddr;
	event->old_state = ctx->oldstate;
	event->new_state = ctx->newstate;
	event->family = ctx->family;
	event->sport = ctx->sport;
	event->dport = ctx->dport;
	if (ctx->family == AF_INET) {
		__builtin_memcpy(event->source, ctx->saddr, 4);
		__builtin_memcpy(event->destination, ctx->daddr, 4);
	} else if (ctx->family == AF_INET6) {
		__builtin_memcpy(event->source, ctx->saddr_v6, 16);
		__builtin_memcpy(event->destination, ctx->daddr_v6, 16);
	}
	finish_event(event);
	return 0;
}

static __always_inline int emit_tcp_event(struct tcp_event_ctx *ctx, __u32 type) {
	struct event *event = start_event(ctx, type);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)ctx->skaddr;
	event->old_state = ctx->state;
	event->family = ctx->family;
	event->sport = ctx->sport;
	event->dport = ctx->dport;
	if (ctx->family == AF_INET) {
		__builtin_memcpy(event->source, ctx->saddr, 4);
		__builtin_memcpy(event->destination, ctx->daddr, 4);
	} else if (ctx->family == AF_INET6) {
		__builtin_memcpy(event->source, ctx->saddr_v6, 16);
		__builtin_memcpy(event->destination, ctx->daddr_v6, 16);
	}
	finish_event(event);
	return 0;
}

struct tcp_socket_ctx {
	__u64 unused;
	const void *skaddr;
	__u16 sport;
	__u16 dport;
	__u16 family;
	__u8 saddr[4];
	__u8 daddr[4];
	__u8 saddr_v6[16];
	__u8 daddr_v6[16];
	__u64 sock_cookie;
};

struct tcp_reset_ctx {
	__u64 unused;
	const void *skbaddr;
	const void *skaddr;
	int state;
};

struct in6_addr {
	struct {
		__u8 u6_addr8[16];
	} in6_u;
};

struct sock_common {
	__be32 skc_daddr;
	__be32 skc_rcv_saddr;
	__be16 skc_dport;
	__u16 skc_num;
	unsigned short skc_family;
	struct in6_addr skc_v6_daddr;
	struct in6_addr skc_v6_rcv_saddr;
};

struct sock {
	struct sock_common __sk_common;
};

struct sock_length_ctx {
	__u64 unused;
	struct sock *sk;
	__u16 family;
	__u16 protocol;
	int ret;
	int flags;
};

static __always_inline int emit_length_event(struct sock_length_ctx *ctx, __u32 type) {
	if (!ctx || !ctx->sk || ctx->ret <= 0 ||
	    (ctx->protocol != IPPROTO_TCP && ctx->protocol != IPPROTO_UDP)) {
		return 0;
	}
	struct event *event = start_event(ctx, type);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)ctx->sk;
	event->protocol = ctx->protocol;
	event->bytes = (__u64)ctx->ret;
	event->family = BPF_CORE_READ(ctx->sk, __sk_common.skc_family);
	event->sport = BPF_CORE_READ(ctx->sk, __sk_common.skc_num);
	event->dport = BPF_CORE_READ(ctx->sk, __sk_common.skc_dport);
	if (event->family == AF_INET) {
		__be32 source = BPF_CORE_READ(ctx->sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(ctx->sk, __sk_common.skc_daddr);
		__builtin_memcpy(event->source, &source, 4);
		__builtin_memcpy(event->destination, &destination, 4);
	} else if (event->family == AF_INET6) {
		BPF_CORE_READ_INTO(event->source, ctx->sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(event->destination, ctx->sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
	}
	finish_event(event);
	return 0;
}

static __always_inline int emit_socket_event(struct tcp_socket_ctx *ctx, __u32 type) {
	struct event *event = start_event(ctx, type);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)ctx->skaddr;
	event->family = ctx->family;
	event->sport = ctx->sport;
	event->dport = ctx->dport;
	if (ctx->family == AF_INET) {
		__builtin_memcpy(event->source, ctx->saddr, 4);
		__builtin_memcpy(event->destination, ctx->daddr, 4);
	} else if (ctx->family == AF_INET6) {
		__builtin_memcpy(event->source, ctx->saddr_v6, 16);
		__builtin_memcpy(event->destination, ctx->daddr_v6, 16);
	}
	finish_event(event);
	return 0;
}

static __always_inline int emit_reset_event(struct tcp_reset_ctx *ctx, __u32 type) {
	struct event *event = start_event(ctx, type);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)ctx->skaddr;
	event->old_state = ctx->state;
	finish_event(event);
	return 0;
}

SEC("tracepoint/tcp/tcp_retransmit_skb")
int tcp_retransmit_skb(struct tcp_event_ctx *ctx) { return emit_tcp_event(ctx, 2); }

SEC("tracepoint/tcp/tcp_send_reset")
int tcp_send_reset(struct tcp_reset_ctx *ctx) { return emit_reset_event(ctx, 3); }

SEC("tracepoint/tcp/tcp_receive_reset")
int tcp_receive_reset(struct tcp_socket_ctx *ctx) { return emit_socket_event(ctx, 4); }

SEC("tracepoint/tcp/tcp_destroy_sock")
int tcp_destroy_sock(struct tcp_socket_ctx *ctx) { return emit_socket_event(ctx, 5); }

SEC("tracepoint/sock/sock_send_length")
int udp_send_length(struct sock_length_ctx *ctx) {
	return emit_length_event(ctx, 6);
}

SEC("tracepoint/sock/sock_recv_length")
int udp_recv_length(struct sock_length_ctx *ctx) {
	return emit_length_event(ctx, 7);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
