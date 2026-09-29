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
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
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

// 사용자 공간이 trace protocol에 맞춰 불러오기 전에 정한다. 쓰지 않을 event를 ring buffer에 넣지 않아야 바쁜 host에서
// 필요한 event가 유실되지 않는다. 기본값은 capture처럼 모든 event를 보낸다.
volatile const __u8 emit_udp_events = 1;
// emit_dns_sent는 송신 경로의 DNS 레코드다. emit_dns_server는 로컬 port 53이 받은 질의와 보낸 응답이며, trace dns --side
// server만 켠다. 서버 쪽은 기본으로 끈다. 바쁜 DNS 서버에서는 이 레코드가 client 쪽보다 훨씬 많다.
volatile const __u8 emit_dns_sent = 1;
volatile const __u8 emit_dns_server = 0;
// TCP 송수신 hook은 trace http와 trace dns가 함께 쓴다. 불러오기 전에 어느 message를 낼지 정한다.
volatile const __u8 emit_http_messages = 0;
volatile const __u8 emit_dns_tcp_messages = 0;
// http_payload_limit는 HTTP message 앞부분을 읽는 byte 수다. 사용자 공간은 요청 줄과 Host만 쓰므로 512면 된다.
// trace http --payload만 HTTP_PAYLOAD_SIZE까지 늘린다. 레코드는 읽은 만큼만 ring buffer에 넣는다.
volatile const __u32 http_payload_limit = 512;
// 0이 아니면 inet_sock_set_state는 로컬이나 상대 port가 이 값인 socket만 본다. trace dns는 53만 본다. 상대 port는 client 쪽
// 연결, 로컬 port는 이 host의 DNS 서버가 받은 연결이다.
volatile const __u16 tcp_state_port = 0;

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

struct mm_struct {
	struct {
		unsigned long arg_start;
		unsigned long arg_end;
	};
};

struct task_struct {
	struct mm_struct *mm;
	struct task_struct *group_leader;
	char comm[16];
};

// current_process_name은 현재 thread가 아니라 thread group leader의 이름이다. thread마다 이름을 붙이는 program(Bun의
// "HTTP Client", tokio의 "tokio-rt-worker")도 ps와 /proc/<pid>/comm이 보여 주는 process 이름 하나로 묶인다.
static __always_inline void current_process_name(char (*name)[16]) {
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	// 문자열 읽기는 NUL까지만 쓴다. HTTP와 DNS 레코드는 CPU별 scratch나 지우지 않은 ring slot이라, 먼저 지우지 않으면
	// 짧은 이름 뒤에 앞 레코드의 이름이 남는다("ab\0gprocessname").
	__builtin_memset(name, 0, sizeof(*name));
	BPF_CORE_READ_STR_INTO(name, task, group_leader, comm);
}

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
	current_process_name(&event->comm);
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
	current_process_name(&owner.comm);
	bpf_map_update_elem(&sock_owners, &skaddr, &owner, BPF_ANY);
}

// 주인을 모르면 pid와 comm을 비운다. 이 event들은 대개 인터럽트 문맥이라, 현재 태스크는 swapper나 그때
// CPU에서 돌던 무관한 프로세스다. trace 전에 connect()한 socket의 재전송이 그런 이름으로 보였다.
static __always_inline void apply_sock_owner(struct event *event) {
	__u64 skaddr = event->skaddr;
	struct sock_owner *owner = 0;
	if (skaddr) {
		owner = bpf_map_lookup_elem(&sock_owners, &skaddr);
	}
	if (!owner) {
		event->pid = 0;
		event->cgroup_id = 0;
		__builtin_memset(event->comm, 0, sizeof(event->comm));
		return;
	}
	event->pid = owner->pid;
	event->cgroup_id = owner->cgroup_id;
	__builtin_memcpy(event->comm, owner->comm, sizeof(event->comm));
}

// kernel과 같은 모양으로 이름 없는 구조체 안에 둔다. CO-RE가 이 경로로 kernel의 필드를 찾는다.
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
	if (tcp_state_port && ctx->dport != tcp_state_port && ctx->sport != tcp_state_port) {
		return 0;
	}
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

struct socket {
	struct sock *sk;
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

#define DNS_PAYLOAD_SIZE 1024
#define DNS_RECEIVED 0
#define DNS_SENT 1
#define DNS_UDP 0
#define DNS_TCP 1

// port 53으로 주고받은 DNS message를 사용자 공간에 넘긴다. 질의인지 응답인지, client 쪽인지 서버 쪽인지는 사용자 공간이
// QR bit와 port로 가린다. 응답은 명령줄에 대상이 없는 프로그램의 target 이름도 짓는다. event_type은 struct event와 같은
// 위치이고, source는 struct event처럼 로컬 쪽이다.
struct dns_record {
	__u64 timestamp_ns;
	__u32 event_type;
	__u32 pid;
	__u64 cgroup_id;
	// arrival_ns는 받은 message가 socket 수신 큐에 들어간 시각이다. 모르면 0이다.
	__u64 arrival_ns;
	// skaddr와 transport는 TCP 조각을 연결마다 이어 붙이는 데 쓴다. UDP는 skaddr가 0이다.
	__u64 skaddr;
	__u32 len;
	__u16 family;
	__u8 direction;
	__u8 transport;
	__u16 sport;
	__u16 dport;
	__u8 source[16];
	__u8 destination[16];
	char comm[16];
	__u8 payload[DNS_PAYLOAD_SIZE];
};

// record가 BPF stack(512바이트)보다 커서 송신 레코드는 CPU별 scratch에서 만든다.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct dns_record);
} dns_scratch SEC(".maps");

// UDP 송신처럼 fentry에서 읽은 message를 thread별로 두고, fexit에서 전송이 성공했을 때만 보낸다.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024);
	__type(key, __u64);
	__type(value, struct dns_record);
} dns_query_pending SEC(".maps");

// DNS message가 socket 수신 큐에 들어간 시각을 skb 주소로 둔다. process가 읽을 때 같은 skb가 skb_consume_udp에 온다.
// 큐에서 버려진 skb의 시각은 LRU가 밀어낸다. 같은 주소를 다시 쓰는 skb는 큐에 들어갈 때 값을 덮는다.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 4096);
	__type(key, __u64);
	__type(value, __u64);
} dns_arrivals SEC(".maps");

// 선형 영역 밖의 payload는 page fragment에 있어 head 기준 주소로 읽으면 다른 메모리를 읽는다.
static __always_inline __u32 read_dns_payload(struct sk_buff *skb, unsigned char *payload, long len, __u8 *out) {
	unsigned char *linear_end = BPF_CORE_READ(skb, head) + BPF_CORE_READ(skb, tail);
	long available = linear_end - payload;
	if (available <= 0 || len <= 0) {
		return 0;
	}
	__u32 size = len;
	if (size > available) {
		size = available;
	}
	if (size > DNS_PAYLOAD_SIZE) {
		size = DNS_PAYLOAD_SIZE;
	}
	return bpf_probe_read_kernel(out, size, payload) == 0 ? size : 0;
}

static __always_inline void remember_dns_sent(struct sk_buff *skb, struct udp_send_pending *pending, unsigned char *payload, long len) {
	__u32 zero = 0;
	struct dns_record *record = bpf_map_lookup_elem(&dns_scratch, &zero);
	if (!record) {
		return;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->event_type = 9;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->arrival_ns = 0;
	record->family = pending->family;
	record->direction = DNS_SENT;
	record->transport = DNS_UDP;
	record->skaddr = 0;
	record->sport = pending->sport;
	record->dport = pending->dport;
	__builtin_memcpy(record->source, pending->source, 16);
	__builtin_memcpy(record->destination, pending->destination, 16);
	current_process_name(&record->comm);
	record->len = read_dns_payload(skb, payload, len, record->payload);
	if (record->len == 0) {
		return;
	}
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&dns_query_pending, &key, record, BPF_ANY);
}

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
	if (emit_dns_sent && (pending.dport == 53 || (emit_dns_server && pending.sport == 53))) {
		remember_dns_sent(skb, &pending, head + transport + 8, payload);
	}
	return 0;
}

static __always_inline int emit_udp_send(void *ctx, int ret) {
	__u64 key = bpf_get_current_pid_tgid();
	struct udp_send_pending *pending = bpf_map_lookup_elem(&udp_send_pending, &key);
	if (!pending) {
		return 0;
	}
	// 방화벽이 버린 송신도 여기서 오류로 돌아온다. 보냄으로 세면 차단 문제를 가린다.
	if (ret == 0 && emit_udp_events) {
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
	if (pending->dport == 53 || pending->sport == 53) {
		struct dns_record *sent = bpf_map_lookup_elem(&dns_query_pending, &key);
		if (sent) {
			if (ret == 0) {
				bpf_ringbuf_output(&events, sent, sizeof(*sent), 0);
			}
			bpf_map_delete_elem(&dns_query_pending, &key);
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

// __udp_enqueue_schedule_skb는 UDP datagram을 socket 수신 큐에 넣는다. 받은 쪽 process가 읽기 전이다.
SEC("fentry/__udp_enqueue_schedule_skb")
int udp_enqueue_entry(__u64 *ctx) {
	struct sk_buff *skb = (struct sk_buff *)ctx[1];
	if (!skb) {
		return 0;
	}
	__be16 ports[2] = {};
	bpf_probe_read_kernel(ports, sizeof(ports), BPF_CORE_READ(skb, head) + BPF_CORE_READ(skb, transport_header));
	if (bpf_ntohs(ports[0]) != 53 && !(emit_dns_server && bpf_ntohs(ports[1]) == 53)) {
		return 0;
	}
	__u64 key = (__u64)skb;
	__u64 now = bpf_ktime_get_ns();
	bpf_map_update_elem(&dns_arrivals, &key, &now, BPF_ANY);
	return 0;
}

// skb_consume_udp는 udp_recvmsg와 udpv6_recvmsg가 datagram을 복사한 뒤 한 번 부른다. recv()처럼 주소를
// 받지 않는 호출도 여기서는 header에서 보낸 쪽 주소를 읽을 수 있다.
static __always_inline void emit_dns_received(struct sk_buff *skb, unsigned char *network, unsigned char *transport, __u8 version, __be16 *ports, int len) {
	struct dns_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		return;
	}
	record->len = read_dns_payload(skb, transport + 8, len, record->payload);
	if (record->len == 0) {
		bpf_ringbuf_discard(record, 0);
		return;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->event_type = 9;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->arrival_ns = 0;
	__u64 key = (__u64)skb;
	__u64 *arrival = bpf_map_lookup_elem(&dns_arrivals, &key);
	if (arrival) {
		record->arrival_ns = *arrival;
		bpf_map_delete_elem(&dns_arrivals, &key);
	}
	record->direction = DNS_RECEIVED;
	record->transport = DNS_UDP;
	record->skaddr = 0;
	record->sport = bpf_ntohs(ports[1]);
	record->dport = bpf_ntohs(ports[0]);
	// bpf_ringbuf_reserve는 slot을 지우지 않는다. IPv4는 앞 4바이트만 쓰므로 나머지에 옛 값이 남지 않게 먼저 지운다.
	__builtin_memset(record->source, 0, sizeof(record->source));
	__builtin_memset(record->destination, 0, sizeof(record->destination));
	if (version == 4) {
		record->family = AF_INET;
		bpf_probe_read_kernel(record->source, 4, network + 16);
		bpf_probe_read_kernel(record->destination, 4, network + 12);
	} else {
		record->family = AF_INET6;
		bpf_probe_read_kernel(record->source, 16, network + 24);
		bpf_probe_read_kernel(record->destination, 16, network + 8);
	}
	current_process_name(&record->comm);
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
	// ports[0]은 보낸 쪽 port, ports[1]은 로컬 port다.
	if (bpf_ntohs(ports[0]) == 53 || (emit_dns_server && bpf_ntohs(ports[1]) == 53)) {
		emit_dns_received(skb, network, transport, version, ports, len);
	}
	if (!emit_udp_events) {
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

// trace 전부터 떠 있던 서버는 listen()을 이미 지났다. accept()의 첫 인자가 listen socket이라 여기서 주인을
// 배운다. 첫 인자만 읽으므로 6.10에서 바뀐 뒤쪽 인자와 무관하다.
SEC("fentry/inet_csk_accept")
int inet_csk_accept_entry(__u64 *ctx) {
	announce_owner(ctx[0]);
	remember_sock_owner(ctx[0]);
	return 0;
}

// 경로가 없는 IPv6 connect()는 SYN_SENT 전에 실패하고, socket을 닫을 때 tcp_destroy만 남긴다. connect()는 부른
// 프로세스 문맥에서 이 함수를 지나므로 여기서 주인을 배운다. 성공하는 connect는 SYN_SENT에서 같은 주인을 다시 적는다.
SEC("fentry/__inet_stream_connect")
int inet_stream_connect_entry(__u64 *ctx) {
	__u64 sk = (__u64)BPF_CORE_READ((struct socket *)ctx[0], sk);
	announce_owner(sk);
	remember_sock_owner(sk);
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

// trace http는 TCP로 주고받는 평문 HTTP/1.x message의 앞부분을 읽는다. 요청 줄, Host, 상태 줄이 이 안에 들어간다.
// 나머지 header와 body는 사용자 공간이 해석한 뒤 버린다.
#define HTTP_PAYLOAD_SIZE 4096
#define HTTP_RECEIVED 0
#define HTTP_SENT 1
#define MSG_PEEK 2

struct http_record {
	__u64 timestamp_ns;
	__u32 event_type;
	__u32 pid;
	__u64 cgroup_id;
	__u64 skaddr;
	__u32 len;
	__u16 family;
	__u8 direction;
	__u8 reserved;
	__u16 sport;
	__u16 dport;
	__u8 source[16];
	__u8 destination[16];
	char comm[16];
	__u8 payload[HTTP_PAYLOAD_SIZE];
};

// ringbuf에만 쓰는 구조체는 BTF에 남지 않는다. bpf2go -type이 Go 구조체를 만들어 offset을 테스트하도록 남긴다.
const struct http_record *unused_http_record __attribute__((unused));

// record가 BPF stack(512바이트)보다 커서 CPU별 scratch에서 만든다.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct http_record);
} http_scratch SEC(".maps");

struct iovec {
	void *iov_base;
	__u64 iov_len;
};

enum iter_type {
	ITER_UBUF,
	ITER_IOVEC,
};

// iter_type은 5.14, ubuf는 6.0, __iov는 6.4에 생겼다. 이 BPF 객체는 모든 trace가 함께 불러오므로, 없는 필드는 CO-RE
// 존재 확인으로 감싼다. 감싸지 않으면 오래된 kernel에서 trace tcp까지 불러오기에 실패한다.
struct iov_iter {
	__u8 iter_type;
	__u64 iov_offset;
	__u64 count;
	const struct iovec *__iov;
	void *ubuf;
	__u64 nr_segs;
};

struct msghdr {
	struct iov_iter msg_iter;
};

// http_user_buffer는 사용자 버퍼의 시작과 첫 조각의 길이다. 여러 조각이면 첫 조각만 읽는다. 요청 줄은 첫 조각에 있다.
static __always_inline void *http_user_buffer(struct msghdr *msg, __u64 *limit) {
	if (!bpf_core_field_exists(msg->msg_iter.iter_type)) {
		return 0;
	}
	__u8 type = BPF_CORE_READ(msg, msg_iter.iter_type);
	__u64 offset = BPF_CORE_READ(msg, msg_iter.iov_offset);
	if (bpf_core_field_exists(msg->msg_iter.ubuf) && bpf_core_enum_value_exists(enum iter_type, ITER_UBUF) &&
	    type == bpf_core_enum_value(enum iter_type, ITER_UBUF)) {
		*limit = BPF_CORE_READ(msg, msg_iter.count);
		return (char *)BPF_CORE_READ(msg, msg_iter.ubuf) + offset;
	}
	if (bpf_core_field_exists(msg->msg_iter.__iov) && bpf_core_enum_value_exists(enum iter_type, ITER_IOVEC) &&
	    type == bpf_core_enum_value(enum iter_type, ITER_IOVEC)) {
		const struct iovec *iov = BPF_CORE_READ(msg, msg_iter.__iov);
		__u64 length = BPF_CORE_READ(iov, iov_len);
		if (length <= offset) {
			return 0;
		}
		*limit = length - offset;
		return (char *)BPF_CORE_READ(iov, iov_base) + offset;
	}
	return 0;
}

// 모든 TCP 송수신에서 불리므로 앞 4바이트만 먼저 본다. 사용자 공간이 요청 줄과 상태 줄을 다시 확인한다.
static __always_inline int http_start(const __u8 *p) {
	return (p[0] == 'G' && p[1] == 'E' && p[2] == 'T' && p[3] == ' ') || (p[0] == 'P' && p[1] == 'O' && p[2] == 'S' && p[3] == 'T') ||
	       (p[0] == 'P' && p[1] == 'U' && p[2] == 'T' && p[3] == ' ') || (p[0] == 'H' && p[1] == 'E' && p[2] == 'A' && p[3] == 'D') ||
	       (p[0] == 'D' && p[1] == 'E' && p[2] == 'L' && p[3] == 'E') || (p[0] == 'P' && p[1] == 'A' && p[2] == 'T' && p[3] == 'C') ||
	       (p[0] == 'O' && p[1] == 'P' && p[2] == 'T' && p[3] == 'I') || (p[0] == 'H' && p[1] == 'T' && p[2] == 'T' && p[3] == 'P');
}

static __always_inline void emit_http(struct sock *sk, const void *buffer, __u64 size, __u8 direction) {
	if (!sk || !buffer || size < 4) {
		return;
	}
	__u8 peek[4];
	if (bpf_probe_read_user(peek, sizeof(peek), buffer) || !http_start(peek)) {
		return;
	}
	__u32 zero = 0;
	struct http_record *record = bpf_map_lookup_elem(&http_scratch, &zero);
	if (!record) {
		return;
	}
	__u32 len = size;
	if (len > http_payload_limit) {
		len = http_payload_limit;
	}
	if (len > HTTP_PAYLOAD_SIZE) {
		len = HTTP_PAYLOAD_SIZE;
	}
	if (bpf_probe_read_user(record->payload, len, buffer)) {
		return;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->event_type = 10;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->skaddr = (__u64)sk;
	record->len = len;
	record->direction = direction;
	record->reserved = 0;
	record->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	record->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	record->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	__builtin_memset(record->source, 0, sizeof(record->source));
	__builtin_memset(record->destination, 0, sizeof(record->destination));
	if (record->family == AF_INET) {
		__be32 source = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(record->source, &source, 4);
		__builtin_memcpy(record->destination, &destination, 4);
	} else {
		BPF_CORE_READ_INTO(&record->source, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&record->destination, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
	}
	current_process_name(&record->comm);
	if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct http_record, payload) + len, 0)) {
		__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
	}
}

// DNS over TCP는 로컬이나 상대 port 53의 연결이다. 로컬 53은 이 host의 DNS 서버라서 --side server일 때만 본다.
static __always_inline int dns_tcp_socket(struct sock *sk) {
	__u16 local = BPF_CORE_READ(sk, __sk_common.skc_num);
	__u16 remote = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	return remote == 53 || (emit_dns_server && local == 53);
}

// emit_dns_tcp는 TCP로 주고받은 조각의 앞부분을 DNS 레코드로 넘긴다. 조각 앞의 2바이트 길이는 그대로 두고, 사용자 공간이
// 연결마다 길이와 message를 맞춘다.
static __always_inline void emit_dns_tcp(struct sock *sk, const void *buffer, __u64 size, __u8 direction) {
	if (!sk || !buffer || size == 0) {
		return;
	}
	struct dns_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		__u32 key = 0;
		__u64 *lost = bpf_map_lookup_elem(&lost_events, &key);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
		return;
	}
	__u32 len = size;
	if (len > DNS_PAYLOAD_SIZE) {
		len = DNS_PAYLOAD_SIZE;
	}
	if (bpf_probe_read_user(record->payload, len, buffer)) {
		bpf_ringbuf_discard(record, 0);
		return;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->event_type = 9;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->arrival_ns = 0;
	record->skaddr = (__u64)sk;
	record->len = len;
	record->direction = direction;
	record->transport = DNS_TCP;
	record->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	record->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	record->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	__builtin_memset(record->source, 0, sizeof(record->source));
	__builtin_memset(record->destination, 0, sizeof(record->destination));
	if (record->family == AF_INET) {
		__be32 source = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(record->source, &source, 4);
		__builtin_memcpy(record->destination, &destination, 4);
	} else {
		BPF_CORE_READ_INTO(&record->source, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&record->destination, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
	}
	current_process_name(&record->comm);
	bpf_ringbuf_submit(record, 0);
}

// iov_second_buffer는 둘째 iovec이다. glibc, systemd-resolved, BIND는 DNS over TCP의 2바이트 길이와 message를 writev로
// 두 조각에 나눠 쓴다. 첫 조각만 읽으면 message를 놓친다.
static __always_inline void *iov_second_buffer(struct msghdr *msg, __u64 *limit) {
	if (!bpf_core_field_exists(msg->msg_iter.iter_type) || !bpf_core_field_exists(msg->msg_iter.__iov) ||
	    !bpf_core_field_exists(msg->msg_iter.nr_segs) || !bpf_core_enum_value_exists(enum iter_type, ITER_IOVEC)) {
		return 0;
	}
	if (BPF_CORE_READ(msg, msg_iter.iter_type) != bpf_core_enum_value(enum iter_type, ITER_IOVEC) || BPF_CORE_READ(msg, msg_iter.nr_segs) < 2 ||
	    BPF_CORE_READ(msg, msg_iter.iov_offset) != 0) {
		return 0;
	}
	const struct iovec *iov = BPF_CORE_READ(msg, msg_iter.__iov);
	*limit = BPF_CORE_READ(iov + 1, iov_len);
	return BPF_CORE_READ(iov + 1, iov_base);
}

SEC("fentry/tcp_sendmsg")
int tcp_sendmsg_entry(__u64 *ctx) {
	struct sock *sk = (struct sock *)ctx[0];
	struct msghdr *msg = (struct msghdr *)ctx[1];
	__u64 size = ctx[2];
	__u64 limit = 0;
	void *buffer = http_user_buffer(msg, &limit);
	__u64 first = size < limit ? size : limit;
	if (emit_dns_tcp_messages && sk && dns_tcp_socket(sk)) {
		emit_dns_tcp(sk, buffer, first, DNS_SENT);
		if (first == 2 && size > 2) {
			__u64 second_limit = 0;
			void *second = iov_second_buffer(msg, &second_limit);
			emit_dns_tcp(sk, second, size - 2 < second_limit ? size - 2 : second_limit, DNS_SENT);
		}
		return 0;
	}
	if (emit_http_messages) {
		emit_http(sk, buffer, first, HTTP_SENT);
	}
	return 0;
}

// tcp_recvmsg가 끝나야 사용자 버퍼에 data가 있다. 시작할 때 버퍼 위치를 thread별로 두고 끝날 때 읽는다.
struct http_recv_pending {
	__u64 skaddr;
	__u64 buffer;
	__u64 limit;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct http_recv_pending);
} http_recv_pending SEC(".maps");

SEC("fentry/tcp_recvmsg")
int tcp_recvmsg_entry(__u64 *ctx) {
	// MSG_PEEK로 읽은 data는 다음 recv가 다시 읽는다. 같은 message를 두 번 내지 않는다.
	if ((int)ctx[3] & MSG_PEEK) {
		return 0;
	}
	struct http_recv_pending pending = {.skaddr = ctx[0]};
	void *buffer = http_user_buffer((struct msghdr *)ctx[1], &pending.limit);
	if (!buffer) {
		return 0;
	}
	pending.buffer = (__u64)buffer;
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&http_recv_pending, &key, &pending, BPF_ANY);
	return 0;
}

SEC("fexit/tcp_recvmsg")
int tcp_recvmsg_exit(__u64 *ctx) {
	__u64 key = bpf_get_current_pid_tgid();
	struct http_recv_pending *pending = bpf_map_lookup_elem(&http_recv_pending, &key);
	if (!pending) {
		return 0;
	}
	int copied = (int)ctx[5];
	if (copied > 0) {
		__u64 size = (__u64)copied < pending->limit ? (__u64)copied : pending->limit;
		struct sock *sk = (struct sock *)pending->skaddr;
		if (emit_dns_tcp_messages && sk && dns_tcp_socket(sk)) {
			emit_dns_tcp(sk, (void *)pending->buffer, size, DNS_RECEIVED);
		} else if (emit_http_messages) {
			emit_http(sk, (void *)pending->buffer, size, HTTP_RECEIVED);
		}
	}
	bpf_map_delete_elem(&http_recv_pending, &key);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
