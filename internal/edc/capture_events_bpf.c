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
#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_LRU_HASH 9
#define BPF_MAP_TYPE_RINGBUF 27
#define TCP_SYN_SENT 2
#define TCP_CLOSE 7
#define TCP_LISTEN 10

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

// TCP 상태 변화, 재전송, RST는 대개 패킷을 받는 인터럽트 문맥에서 일어난다. 그때 CPU의 태스크는
// swapper라서 event가 socket을 쓰는 프로세스와 끊긴다. 프로세스 문맥에서 본 주인을 socket별로 둔다.
struct sock_owner {
	__u64 cgroup_id;
	__u32 pid;
	char comm[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, struct sock_owner);
} sock_owners SEC(".maps");

// 프로세스 문맥에서만 부른다. 인터럽트 문맥의 현재 태스크는 socket과 무관하다.
static __always_inline void remember_sock_owner(__u64 skaddr) {
	if (!skaddr) {
		return;
	}
	struct sock_owner owner = {};
	owner.cgroup_id = bpf_get_current_cgroup_id();
	owner.pid = bpf_get_current_pid_tgid() >> 32;
	bpf_get_current_comm(&owner.comm, sizeof(owner.comm));
	bpf_map_update_elem(&sock_owners, &skaddr, &owner, BPF_ANY);
}

static __always_inline void apply_sock_owner(struct event *event) {
	__u64 skaddr = event->skaddr;
	if (!skaddr) {
		return;
	}
	struct sock_owner *owner = bpf_map_lookup_elem(&sock_owners, &skaddr);
	if (!owner) {
		return;
	}
	event->pid = owner->pid;
	event->cgroup_id = owner->cgroup_id;
	__builtin_memcpy(event->comm, owner->comm, sizeof(event->comm));
}

// kernel과 같은 모양으로 이름 없는 구조체 안에 둔다. CO-RE가 이 경로로 kernel의 필드를 찾는다.
struct mm_struct {
	struct {
		unsigned long arg_start;
		unsigned long arg_end;
	};
};

struct task_struct {
	struct mm_struct *mm;
};

#define OWNER_ARGS_SIZE 512

// 사용자 공간은 /proc/<pid>/cmdline으로 target을 찾는데, event를 읽을 때 짧게 사는 프로세스는 이미
// 끝나 있다. 프로세스 문맥에서 명령줄을 함께 보내 두면 읽는 시점과 무관해진다. event_type은 struct
// event와 같은 위치라서 사용자 공간이 먼저 보고 구분한다.
struct owner_record {
	__u64 timestamp_ns;
	__u32 event_type;
	__u32 pid;
	__u32 len;
	__u32 reserved;
	char args[OWNER_ARGS_SIZE];
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u32);
} announced_owners SEC(".maps");

// 프로세스 문맥에서만 부른다. ring buffer는 예약한 순서로 읽히므로, 같은 순간의 event보다 먼저 불러야
// 사용자 공간이 명령줄을 먼저 받는다.
static __always_inline void announce_owner(__u64 skaddr) {
	if (!skaddr) {
		return;
	}
	__u32 pid = bpf_get_current_pid_tgid() >> 32;
	__u32 *last = bpf_map_lookup_elem(&announced_owners, &skaddr);
	if (last && *last == pid) {
		return;
	}
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	struct mm_struct *mm = BPF_CORE_READ(task, mm);
	if (!mm) {
		return;
	}
	unsigned long start = BPF_CORE_READ(mm, arg_start);
	unsigned long end = BPF_CORE_READ(mm, arg_end);
	if (end <= start) {
		return;
	}
	struct owner_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		// 알리지 못한 socket은 다음 기회에 다시 알린다. 그 사이 event는 /proc 조회로 target을 찾는다.
		return;
	}
	__u32 len = end - start;
	if (len > OWNER_ARGS_SIZE) {
		len = OWNER_ARGS_SIZE;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->event_type = 8;
	record->pid = pid;
	record->reserved = 0;
	record->len = bpf_probe_read_user(record->args, len, (void *)start) == 0 ? len : 0;
	bpf_ringbuf_submit(record, 0);
	bpf_map_update_elem(&announced_owners, &skaddr, &pid, BPF_ANY);
}

static __always_inline void forget_owner(__u64 skaddr) {
	bpf_map_delete_elem(&sock_owners, &skaddr);
	bpf_map_delete_elem(&announced_owners, &skaddr);
}

static __always_inline void finish_event(struct event *event) {
	if (event) {
		bpf_ringbuf_submit(event, 0);
	}
}

SEC("tracepoint/sock/inet_sock_set_state")
int inet_sock_set_state(struct inet_sock_set_state_ctx *ctx) {
	// SYN_SENT와 LISTEN 전이는 connect()와 listen() 안에서 일어나므로 현재 태스크가 socket의 주인이다.
	int owner_context = (ctx->newstate == TCP_SYN_SENT || ctx->newstate == TCP_LISTEN) && ctx->protocol == IPPROTO_TCP;
	if (owner_context) {
		announce_owner((__u64)ctx->skaddr);
	}
	struct event *event = start_event(ctx, 1);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)ctx->skaddr;
	event->old_state = ctx->oldstate;
	event->new_state = ctx->newstate;
	if (owner_context) {
		remember_sock_owner(event->skaddr);
	}
	apply_sock_owner(event);
	// listen socket은 tcp_destroy_sock을 거치지 않으므로 닫힐 때 여기서 지운다.
	if (ctx->oldstate == TCP_LISTEN && ctx->newstate == TCP_CLOSE) {
		forget_owner(event->skaddr);
	}
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
	apply_sock_owner(event);
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

struct inet_sock {
	__be16 inet_sport;
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
	announce_owner((__u64)ctx->sk);
	struct event *event = start_event(ctx, type);
	if (!event) {
		return 0;
	}
	// tracepoint ctx는 kernel BTF에 없는 고정 배치다. sk를 먼저 꺼내야 BPF_CORE_READ가
	// ctx->sk까지 CO-RE relocation으로 잡지 않고, 그렇지 않으면 program load가 실패한다.
	struct sock *sk = ctx->sk;
	event->skaddr = (__u64)sk;
	event->protocol = ctx->protocol;
	// sendmsg와 recvmsg 안에서 불리므로, accept한 socket도 첫 입출력부터 주인을 알 수 있다.
	remember_sock_owner(event->skaddr);
	event->bytes = (__u64)ctx->ret;
	event->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	event->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	// RST를 받아 송수신 중에 닫힌 socket은 kernel이 포트 바인딩을 풀면서 skc_num을 0으로 지운다.
	// inet_sport는 남아 있어서, 없으면 서버 socket의 마지막 송신이 상대 포트로 따로 묶인다.
	if (!event->sport) {
		event->sport = bpf_ntohs(BPF_CORE_READ((struct inet_sock *)sk, inet_sport));
	}
	event->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	if (event->family == AF_INET) {
		__be32 source = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(event->source, &source, 4);
		__builtin_memcpy(event->destination, &destination, 4);
	} else if (event->family == AF_INET6) {
		BPF_CORE_READ_INTO(&event->source, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&event->destination, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
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
	apply_sock_owner(event);
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
	apply_sock_owner(event);
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
int tcp_destroy_sock(struct tcp_socket_ctx *ctx) {
	emit_socket_event(ctx, 5);
	// kernel이 해제한 socket 주소를 새 socket에 다시 쓰므로, 남겨 두면 새 socket에 옛 주인이 붙는다.
	forget_owner((__u64)ctx->skaddr);
	return 0;
}

SEC("tracepoint/sock/sock_send_length")
int tcp_send_length(struct sock_length_ctx *ctx) {
	return emit_length_event(ctx, 6);
}

SEC("tracepoint/sock/sock_recv_length")
int tcp_recv_length(struct sock_length_ctx *ctx) {
	return emit_length_event(ctx, 7);
}

struct sk_buff {
	struct sock *sk;
	unsigned int len;
	unsigned int tail;
	unsigned char *head;
	unsigned char *data;
	__u16 transport_header;
	__u16 network_header;
};

union flowi_uli {
	struct {
		__be16 dport;
		__be16 sport;
	} ports;
};

struct flowi4 {
	__be32 saddr;
	__be32 daddr;
	union flowi_uli uli;
};

struct flowi6 {
	struct in6_addr daddr;
	struct in6_addr saddr;
	union flowi_uli uli;
};

// udp_send_skb는 전송 중에 skb를 해제하므로 fexit에서는 skb를 읽을 수 없다. fentry에서 읽은 값을
// thread별로 잠시 두고, fexit에서 전송이 성공했을 때만 event로 보낸다.
struct udp_send_pending {
	__u64 skaddr;
	__u64 bytes;
	__u16 family;
	__u16 sport;
	__u16 dport;
	__u8 source[16];
	__u8 destination[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct udp_send_pending);
} udp_send_pending SEC(".maps");

static __always_inline int remember_udp_send(struct sk_buff *skb, struct flowi4 *fl4, struct flowi6 *fl6) {
	if (!skb) {
		return 0;
	}
	announce_owner((__u64)BPF_CORE_READ(skb, sk));
	struct udp_send_pending pending = {};
	unsigned char *head = BPF_CORE_READ(skb, head);
	unsigned char *data = BPF_CORE_READ(skb, data);
	__u16 transport = BPF_CORE_READ(skb, transport_header);
	// udp_send_skb가 datagram 길이를 구하는 방식과 같다. 끝의 8은 UDP header다.
	long payload = (long)BPF_CORE_READ(skb, len) - ((long)transport - (data - head)) - 8;
	if (payload < 0) {
		return 0;
	}
	pending.skaddr = (__u64)BPF_CORE_READ(skb, sk);
	pending.bytes = (__u64)payload;
	if (fl4) {
		pending.family = AF_INET;
		pending.sport = bpf_ntohs(BPF_CORE_READ(fl4, uli.ports.sport));
		pending.dport = bpf_ntohs(BPF_CORE_READ(fl4, uli.ports.dport));
		__be32 source = BPF_CORE_READ(fl4, saddr);
		__be32 destination = BPF_CORE_READ(fl4, daddr);
		__builtin_memcpy(pending.source, &source, 4);
		__builtin_memcpy(pending.destination, &destination, 4);
	} else {
		pending.family = AF_INET6;
		pending.sport = bpf_ntohs(BPF_CORE_READ(fl6, uli.ports.sport));
		pending.dport = bpf_ntohs(BPF_CORE_READ(fl6, uli.ports.dport));
		BPF_CORE_READ_INTO(&pending.source, fl6, saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&pending.destination, fl6, daddr.in6_u.u6_addr8);
	}
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&udp_send_pending, &key, &pending, BPF_ANY);
	return 0;
}

static __always_inline int emit_udp_send(void *ctx, int ret) {
	__u64 key = bpf_get_current_pid_tgid();
	struct udp_send_pending *pending = bpf_map_lookup_elem(&udp_send_pending, &key);
	if (!pending) {
		return 0;
	}
	// 방화벽이 버린 송신도 여기서 오류로 돌아온다. 보냄으로 세면 차단 문제를 가린다.
	if (ret == 0) {
		struct event *event = start_event(ctx, 6);
		if (event) {
			event->skaddr = pending->skaddr;
			event->protocol = IPPROTO_UDP;
			event->bytes = pending->bytes;
			event->family = pending->family;
			event->sport = pending->sport;
			event->dport = pending->dport;
			__builtin_memcpy(event->source, pending->source, 16);
			__builtin_memcpy(event->destination, pending->destination, 16);
			finish_event(event);
		}
	}
	bpf_map_delete_elem(&udp_send_pending, &key);
	return 0;
}

SEC("fentry/udp_send_skb")
int udp_send_skb_entry(__u64 *ctx) {
	return remember_udp_send((struct sk_buff *)ctx[0], (struct flowi4 *)ctx[1], 0);
}

SEC("fexit/udp_send_skb")
int udp_send_skb_exit(__u64 *ctx) {
	return emit_udp_send(ctx, (int)ctx[3]);
}

SEC("fentry/udp_v6_send_skb")
int udp_v6_send_skb_entry(__u64 *ctx) {
	return remember_udp_send((struct sk_buff *)ctx[0], 0, (struct flowi6 *)ctx[1]);
}

SEC("fexit/udp_v6_send_skb")
int udp_v6_send_skb_exit(__u64 *ctx) {
	return emit_udp_send(ctx, (int)ctx[3]);
}

// skb_consume_udp는 udp_recvmsg와 udpv6_recvmsg가 datagram을 복사한 뒤 한 번 부른다. recv()처럼 주소를
// 받지 않는 호출도 여기서는 header에서 보낸 쪽 주소를 읽을 수 있다.
#define DNS_PAYLOAD_SIZE 1024

// 명령줄에 대상이 없는 프로그램도 이름을 얻도록 DNS 응답을 사용자 공간에 넘긴다. 받은 프로세스가 곧 조회한
// 프로세스라서 pid를 함께 보낸다. event_type은 struct event와 같은 위치다.
struct dns_record {
	__u64 timestamp_ns;
	__u32 event_type;
	__u32 pid;
	__u32 len;
	__u32 reserved;
	__u8 payload[DNS_PAYLOAD_SIZE];
};

static __always_inline void emit_dns_answer(struct sk_buff *skb, unsigned char *payload, int len) {
	// 선형 영역 밖의 payload는 page fragment에 있어 head 기준 주소로 읽으면 다른 메모리를 읽는다.
	unsigned char *linear_end = BPF_CORE_READ(skb, head) + BPF_CORE_READ(skb, tail);
	long available = linear_end - payload;
	if (available <= 0) {
		return;
	}
	__u32 size = len;
	if (size > available) {
		size = available;
	}
	if (size > DNS_PAYLOAD_SIZE) {
		size = DNS_PAYLOAD_SIZE;
	}
	struct dns_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		return;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->event_type = 9;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->reserved = 0;
	record->len = bpf_probe_read_kernel(record->payload, size, payload) == 0 ? size : 0;
	bpf_ringbuf_submit(record, 0);
}

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
	announce_owner((__u64)sk);
	if (bpf_ntohs(ports[0]) == 53) {
		emit_dns_answer(skb, transport + 8, len);
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

// trace 전부터 떠 있던 서버는 listen()을 이미 지났다. accept()의 첫 인자가 listen socket이라 여기서 주인을
// 배운다. 첫 인자만 읽으므로 6.10에서 바뀐 뒤쪽 인자와 무관하다.
SEC("fentry/inet_csk_accept")
int inet_csk_accept_entry(__u64 *ctx) {
	announce_owner(ctx[0]);
	remember_sock_owner(ctx[0]);
	return 0;
}

// tcp_accept event는 서버가 accept()를 부르기 전에 인터럽트 문맥에서 생긴다. 새 socket이 만들어질 때
// listen socket의 주인을 물려주어야 그 event부터 서버 프로세스로 기록된다.
SEC("fexit/tcp_create_openreq_child")
int tcp_create_openreq_child_exit(__u64 *ctx) {
	__u64 listener = ctx[0];
	__u64 child = ctx[3];
	if (!child) {
		return 0;
	}
	struct sock_owner *owner = bpf_map_lookup_elem(&sock_owners, &listener);
	if (!owner) {
		return 0;
	}
	struct sock_owner copy = *owner;
	bpf_map_update_elem(&sock_owners, &child, &copy, BPF_ANY);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
