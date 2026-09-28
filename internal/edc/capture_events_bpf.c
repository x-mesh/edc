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
#include "bpf_endian.h"

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
	// bpf_ringbuf_reserve는 slot을 0으로 밀지 않는다. 랩어라운드된 slot에는 그 전에
	// 그 자리를 쓴 무관한 event의 값이 남아 있으므로, 여기서 한 번에 지운다.
	// 이 뒤로는 각 emit_* 함수가 실제로 아는 field만 채우면 되고, 모르는 field는
	// 항상 0이지 stale 값이 아니다.
	__builtin_memset(event, 0, sizeof(*event));
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

// UDP는 udp_sendmsg와 skb_consume_udp 훅이 기록한다. 이 tracepoint는 socket의 연결 상대만 알아서,
// 연결하지 않은 UDP socket의 목적지를 0.0.0.0:0으로 남긴다.
static __always_inline int emit_length_event(struct sock_length_ctx *ctx, __u32 type) {
	if (!ctx || !ctx->sk || ctx->ret <= 0 || ctx->protocol != IPPROTO_TCP) {
		return 0;
	}
	struct event *event = start_event(ctx, type);
	if (!event) {
		return 0;
	}
	// tracepoint ctx는 kernel BTF에 없는 고정 배치다. sk를 먼저 꺼내야 BPF_CORE_READ가
	// ctx->sk까지 CO-RE relocation으로 잡지 않고, 그렇지 않으면 program load가 실패한다.
	struct sock *sk = ctx->sk;
	event->skaddr = (__u64)sk;
	event->protocol = ctx->protocol;
	event->bytes = (__u64)ctx->ret;
	event->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	event->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	event->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	if (event->family == AF_INET) {
		__be32 source = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(event->source, &source, 4);
		__builtin_memcpy(event->destination, &destination, 4);
	} else if (event->family == AF_INET6) {
		BPF_CORE_READ_INTO(event->source, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(event->destination, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
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
int tcp_send_length(struct sock_length_ctx *ctx) {
	return emit_length_event(ctx, 6);
}

SEC("tracepoint/sock/sock_recv_length")
int tcp_recv_length(struct sock_length_ctx *ctx) {
	return emit_length_event(ctx, 7);
}

struct msghdr {
	void *msg_name;
};

struct sk_buff {
	unsigned char *head;
	__u16 transport_header;
	__u16 network_header;
};

// sockaddr_in과 sockaddr_in6의 앞부분이다. UAPI 배치라 kernel마다 바뀌지 않으므로 CO-RE 없이 읽는다.
struct udp_sockaddr {
	__u16 family;
	__be16 port;
	union {
		__u8 v4[4];
		struct {
			__u32 flowinfo;
			__u8 v6[16];
		};
	};
};

static __always_inline int ipv4_mapped(const __u8 *address) {
	for (int index = 0; index < 10; index++) {
		if (address[index] != 0) {
			return 0;
		}
	}
	return address[10] == 0xff && address[11] == 0xff;
}

static __always_inline int emit_udp_send(void *ctx, struct sock *sk, struct msghdr *msg, int ret, int ipv6) {
	if (!sk || !msg || ret <= 0) {
		return 0;
	}
	struct udp_sockaddr address = {};
	void *name = BPF_CORE_READ(msg, msg_name);
	if (name) {
		bpf_probe_read_kernel(&address, sizeof(address), name);
	}
	__u8 peer[16] = {};
	if (ipv6) {
		// udpv6_sendmsg는 IPv4 목적지(AF_INET 또는 v4-mapped)를 udp_sendmsg로 넘긴다. 그쪽 훅이 기록하므로
		// 여기서도 세면 같은 datagram을 두 번 센다.
		if (name) {
			if (address.family != AF_INET6 || ipv4_mapped(address.v6)) {
				return 0;
			}
			__builtin_memcpy(peer, address.v6, 16);
		} else {
			BPF_CORE_READ_INTO(&peer, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
			if (ipv4_mapped(peer)) {
				return 0;
			}
		}
	}
	struct event *event = start_event(ctx, 6);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)sk;
	event->protocol = IPPROTO_UDP;
	event->bytes = (__u64)ret;
	event->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	event->dport = bpf_ntohs(name ? address.port : BPF_CORE_READ(sk, __sk_common.skc_dport));
	if (ipv6) {
		event->family = AF_INET6;
		BPF_CORE_READ_INTO(event->source, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		__builtin_memcpy(event->destination, peer, 16);
	} else {
		// udp_sendmsg는 AF_UNSPEC 주소의 sin_addr도 목적지로 쓰므로 family를 가리지 않는다.
		event->family = AF_INET;
		__be32 source = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(event->source, &source, 4);
		if (name) {
			__builtin_memcpy(event->destination, address.v4, 4);
		} else {
			__builtin_memcpy(event->destination, &destination, 4);
		}
	}
	finish_event(event);
	return 0;
}

SEC("fexit/udp_sendmsg")
int udp_sendmsg_exit(__u64 *ctx) {
	return emit_udp_send(ctx, (struct sock *)ctx[0], (struct msghdr *)ctx[1], (int)ctx[3], 0);
}

SEC("fexit/udpv6_sendmsg")
int udpv6_sendmsg_exit(__u64 *ctx) {
	return emit_udp_send(ctx, (struct sock *)ctx[0], (struct msghdr *)ctx[1], (int)ctx[3], 1);
}

// skb_consume_udp는 udp_recvmsg와 udpv6_recvmsg가 datagram을 복사한 뒤 한 번 부른다. recv()처럼 주소를
// 받지 않는 호출도 여기서는 header에서 보낸 쪽 주소를 읽을 수 있다.
SEC("fentry/skb_consume_udp")
int skb_consume_udp_entry(__u64 *ctx) {
	struct sock *sk = (struct sock *)ctx[0];
	struct sk_buff *skb = (struct sk_buff *)ctx[1];
	int len = (int)ctx[2];
	if (!sk || !skb || len <= 0) {
		return 0;
	}
	unsigned char *head = BPF_CORE_READ(skb, head);
	unsigned char *network = head + BPF_CORE_READ(skb, network_header);
	unsigned char *transport = head + BPF_CORE_READ(skb, transport_header);
	__u8 version = 0;
	__be16 ports[2] = {};
	bpf_probe_read_kernel(&version, sizeof(version), network);
	bpf_probe_read_kernel(ports, sizeof(ports), transport);
	version >>= 4;
	if (version != 4 && version != 6) {
		return 0;
	}
	struct event *event = start_event(ctx, 7);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)sk;
	event->protocol = IPPROTO_UDP;
	event->bytes = (__u64)len;
	event->sport = bpf_ntohs(ports[1]);
	event->dport = bpf_ntohs(ports[0]);
	if (version == 4) {
		event->family = AF_INET;
		bpf_probe_read_kernel(event->source, 4, network + 16);
		bpf_probe_read_kernel(event->destination, 4, network + 12);
	} else {
		event->family = AF_INET6;
		bpf_probe_read_kernel(event->source, 16, network + 24);
		bpf_probe_read_kernel(event->destination, 16, network + 8);
	}
	finish_event(event);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
