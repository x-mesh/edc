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
#define TCP_ESTABLISHED 1

#define TCP_DIAG_RTT 1
#define TCP_DIAG_RTTVAR 2
#define TCP_DIAG_CWND 4
#define TCP_DIAG_SSTHRESH 8
#define TCP_DIAG_UNACKED 16
#define TCP_DIAG_LOST 32
#define TCP_DIAG_ZERO_WINDOW 64

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
	__u32 tcp_valid;
	__u32 rtt_us;
	__u32 rttvar_us;
	__u32 cwnd;
	__u32 ssthresh;
	__u32 unacked;
	__u32 lost;
	__u32 zero_window;
	__u64 accept_latency_ns;
	__u32 accept_queue_used;
	__u32 accept_queue_max;
};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 24);
} events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u64);
} tcp_established_at SEC(".maps");

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
// http_port가 0이 아니면 trace http는 로컬이나 상대 port가 이 값인 socket만 본다. client 쪽에서는 상대 서버의 port,
// 서버 쪽에서는 로컬 서버의 port다. 걸러진 socket은 사용자 메모리를 읽지 않아 바쁜 host의 부담도 준다.
volatile const __u16 http_port = 0;
// http_message_limit가 0이 아니면 trace http --payload=all이다. message 하나를 이 byte 수까지 따라가며, 한 번의 읽기나
// 쓰기에서 레코드 하나를 넘는 부분, writev의 다음 버퍼, 이어지는 읽기와 쓰기를 조각 레코드로 넘긴다.
volatile const __u32 http_message_limit = 0;
// 0이 아니면 inet_sock_set_state는 로컬이나 상대 port가 이 값인 socket만 본다. trace dns는 53만 본다. 상대 port는 client 쪽
// 연결, 로컬 port는 이 host의 DNS 서버가 받은 연결이다.
volatile const __u16 tcp_state_port = 0;
// mysql_port가 0이 아니면 trace mysql이다. 로컬이나 상대 port가 이 값인 socket의 읽기와 쓰기 앞부분을 넘긴다. 0이면 trace
// mysql이 아니라서 verifier가 이 값에 걸린 분기를 지운다.
volatile const __u16 mysql_port = 0;
// Linux 5.15에는 sock_send_length와 sock_recv_length tracepoint가 없다. 사용자 공간이 그때만 이 값을 켠다.
volatile const __u8 tcp_length_fallback = 0;
// emit_tls_plaintext는 trace http --tls다. OpenSSL uprobe가 암호화 전과 복호화 뒤의 평문을 넘긴다. 이 객체는 모든
// trace가 불러오므로, 꺼져 있으면 uprobe program이 첫 명령에서 돌아가고 TCP hook도 SSL 상태를 보지 않는다.
volatile const __u8 emit_tls_plaintext = 0;
// uprobe_arch는 uprobe 인자를 읽을 pt_regs 배치다. bpfel 객체 하나가 amd64와 arm64를 함께 섬겨서 compile 때 정할 수 없다.
volatile const __u8 uprobe_arch = 0;

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

struct sock;
static __always_inline void emit_tcp_sample(struct sock *sk);


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
	if (ctx->newstate == TCP_ESTABLISHED) {
		// 5.15 verifier는 ring buffer 메모리를 map key로 받지 않는다.
		__u64 skaddr = event->skaddr;
		__u64 established_at = event->timestamp_ns;
		bpf_map_update_elem(&tcp_established_at, &skaddr, &established_at, BPF_ANY);
	}
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
	if (ctx->newstate == TCP_ESTABLISHED) {
		emit_tcp_sample((struct sock *)ctx->skaddr);
	}
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
	__u8 skc_state;
};

struct sock {
	struct sock_common __sk_common;
	unsigned int sk_ack_backlog;
	unsigned int sk_max_ack_backlog;
};

struct tcp_sock {
	__u32 write_seq;
	__u32 snd_ssthresh;
	__u32 snd_cwnd;
	__u32 srtt_us;
	__u32 rttvar_us;
	__u32 packets_out;
	__u32 lost_out;
	__u32 snd_wnd;
};

struct tcp_diagnostics {
	__u32 valid;
	__u32 rtt_us;
	__u32 rttvar_us;
	__u32 cwnd;
	__u32 ssthresh;
	__u32 unacked;
	__u32 lost;
	__u32 zero_window;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, struct tcp_diagnostics);
} tcp_diagnostics SEC(".maps");

struct inet_sock {
	__be16 inet_sport;
};

struct socket {
	struct sock *sk;
};

static __always_inline void fill_tcp_addresses(struct event *event, struct sock *sk) {
	event->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	event->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
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
}

static __always_inline void read_tcp_diagnostics(struct sock *sk, struct tcp_diagnostics *diag) {
	struct tcp_sock *tcp = (struct tcp_sock *)sk;
	if (bpf_core_field_exists(tcp->srtt_us)) { diag->valid |= TCP_DIAG_RTT; diag->rtt_us = BPF_CORE_READ(tcp, srtt_us) >> 3; }
	if (bpf_core_field_exists(tcp->rttvar_us)) { diag->valid |= TCP_DIAG_RTTVAR; diag->rttvar_us = BPF_CORE_READ(tcp, rttvar_us) >> 2; }
	if (bpf_core_field_exists(tcp->snd_cwnd)) { diag->valid |= TCP_DIAG_CWND; diag->cwnd = BPF_CORE_READ(tcp, snd_cwnd); }
	if (bpf_core_field_exists(tcp->snd_ssthresh)) { diag->valid |= TCP_DIAG_SSTHRESH; diag->ssthresh = BPF_CORE_READ(tcp, snd_ssthresh); }
	if (bpf_core_field_exists(tcp->packets_out)) { diag->valid |= TCP_DIAG_UNACKED; diag->unacked = BPF_CORE_READ(tcp, packets_out); }
	if (bpf_core_field_exists(tcp->lost_out)) { diag->valid |= TCP_DIAG_LOST; diag->lost = BPF_CORE_READ(tcp, lost_out); }
	if (bpf_core_field_exists(tcp->snd_wnd) && BPF_CORE_READ(sk, __sk_common.skc_state) == TCP_ESTABLISHED) {
		diag->valid |= TCP_DIAG_ZERO_WINDOW;
		diag->zero_window = BPF_CORE_READ(tcp, snd_wnd) == 0;
	}
}

static __always_inline void emit_tcp_sample(struct sock *sk) {
	if (!sk) return;
	__u64 key = (__u64)sk;
	struct tcp_diagnostics current = {};
	read_tcp_diagnostics(sk, &current);
	struct tcp_diagnostics *previous = bpf_map_lookup_elem(&tcp_diagnostics, &key);
	if (previous && __builtin_memcmp(previous, &current, sizeof(current)) == 0) return;
	bpf_map_update_elem(&tcp_diagnostics, &key, &current, BPF_ANY);
	struct event *event = start_event(sk, 12);
	if (!event) return;
	event->skaddr = key;
	apply_sock_owner(event);
	event->tcp_valid = current.valid;
	event->rtt_us = current.rtt_us;
	event->rttvar_us = current.rttvar_us;
	event->cwnd = current.cwnd;
	event->ssthresh = current.ssthresh;
	event->unacked = current.unacked;
	event->lost = current.lost;
	event->zero_window = current.zero_window;
	finish_event(event);
}

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

static __always_inline int emit_tcp_length_fallback(struct sock *sk, int bytes, __u32 type) {
	if (!tcp_length_fallback || !sk || bytes <= 0) {
		return 0;
	}
	announce_owner((__u64)sk);
	struct event *event = start_event(sk, type);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)sk;
	event->protocol = IPPROTO_TCP;
	remember_sock_owner(event->skaddr);
	event->bytes = (__u64)bytes;
	event->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	event->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
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
int tcp_retransmit_skb(struct tcp_event_ctx *ctx) {
	emit_tcp_event(ctx, 2);
	emit_tcp_sample((struct sock *)ctx->skaddr);
	return 0;
}

SEC("tracepoint/tcp/tcp_send_reset")
int tcp_send_reset(struct tcp_reset_ctx *ctx) { return emit_reset_event(ctx, 3); }

SEC("tracepoint/tcp/tcp_receive_reset")
int tcp_receive_reset(struct tcp_socket_ctx *ctx) { return emit_socket_event(ctx, 4); }

SEC("tracepoint/tcp/tcp_destroy_sock")
int tcp_destroy_sock(struct tcp_socket_ctx *ctx) {
	emit_socket_event(ctx, 5);
	__u64 skaddr = (__u64)ctx->skaddr;
	// kernel이 해제한 socket 주소를 새 socket에 다시 쓰므로, 남겨 두면 새 socket에 옛 주인이 붙는다.
	forget_owner(skaddr);
	bpf_map_delete_elem(&tcp_diagnostics, &skaddr);
	bpf_map_delete_elem(&tcp_established_at, &skaddr);
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

static __always_inline int finish_inet_csk_accept(__u64 *ctx, struct sock *child) {
	struct sock *listener = (struct sock *)ctx[0];
	if (!listener || !child) {
		return 0;
	}
	__u64 child_key = (__u64)child;
	__u64 *established_at = bpf_map_lookup_elem(&tcp_established_at, &child_key);
	if (!established_at) {
		return 0;
	}
	struct event *event = start_event(ctx, 13);
	if (!event) {
		return 0;
	}
	event->skaddr = child_key;
	apply_sock_owner(event);
	fill_tcp_addresses(event, child);
	event->accept_latency_ns = event->timestamp_ns - *established_at;
	event->accept_queue_used = BPF_CORE_READ(listener, sk_ack_backlog);
	event->accept_queue_max = BPF_CORE_READ(listener, sk_max_ack_backlog);
	finish_event(event);
	bpf_map_delete_elem(&tcp_established_at, &child_key);
	return 0;
}

// inet_csk_accept는 6.17에서 (sk, arg) 두 인자를, 5.15에서 (sk, flags, err, kern) 네 인자를 받는다. fexit의 반환값은
// 인자 뒤에 오고 verifier는 없는 인자 위치를 거부하므로, 사용자 공간이 kernel BTF를 보고 둘 중 하나를 고른다.
SEC("fexit/inet_csk_accept")
int inet_csk_accept_exit(__u64 *ctx) {
	return finish_inet_csk_accept(ctx, (struct sock *)ctx[2]);
}

SEC("fexit/inet_csk_accept")
int inet_csk_accept_exit_legacy(__u64 *ctx) {
	return finish_inet_csk_accept(ctx, (struct sock *)ctx[4]);
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
// HTTP_PAYLOAD_SIZE는 레코드 하나에 담는 byte 수다. 조각이 클수록 한 번의 읽기나 쓰기를 넘기는 반복이 준다.
// per-CPU scratch 값은 32KiB를 넘을 수 없다.
#define HTTP_PAYLOAD_SIZE 16384
// 한 번의 읽기나 쓰기에서 도는 반복의 상한이다. 1MiB를 조각 64개로 넘기고, writev 버퍼를 16개까지 넘긴다. bpf_loop는
// 5.17에 생겨서, 쓰면 오래된 kernel에서 이 객체 전체(trace tcp 포함)를 불러오지 못한다. 그래서 횟수가 정해진 반복을 쓴다.
#define HTTP_MAX_SEGMENTS 16
#define HTTP_MAX_STEPS 80
#define HTTP_RECEIVED 0
#define HTTP_SENT 1
// HTTP_DECRYPTED는 OpenSSL uprobe가 넘긴 평문 레코드의 방향 bit다. 같은 socket의 암호문과 stream 상태를 나눠 둔다.
#define HTTP_DECRYPTED 0x80
#define HTTP_START 0
#define HTTP_CONTINUATION 1
#define MSG_PEEK 2
// HTTP_SPLIT_SIZE보다 짧게 시작한 읽기와 쓰기는 첫 줄이 끝나지 않았을 수 있다. caddy는 요청의 첫 14 byte를 먼저 읽고
// 나머지를 다시 읽는다. --payload=all이 아니면 이런 socket과 방향에서만 다음 조각 하나를 이어서 넘긴다.
#define HTTP_SPLIT_SIZE 64
// HTTP2_STREAM은 http_streams 값에서 h2c 연결을 표시하는 bit다. 수신 방향에서는 나머지 bit가 지금까지 읽은 byte 수다.
// 송신 방향은 TCP 순번을 위치로 쓴다. HTTP/1의 위치는 message 하나 안이라 이 bit에 닿지 않는다.
#define HTTP2_STREAM (1ULL << 63)
// HTTP2_PREFACE_LEN은 h2c 연결 머리의 길이다. "PRI "로 시작하는 HTTP/1 body를 h2c로 보면 그 연결의 byte를 모두 넘기고
// 이후 HTTP/1 요청을 놓치므로, 머리 전체가 맞을 때만 h2c로 본다.
#define HTTP2_PREFACE_LEN 24
// HTTP_NO_SEQ는 http_capture에 TCP 순번이 없다는 표시다. 수신은 실제로 읽은 byte만 넘기므로 순번 없이 센다.
#define HTTP_NO_SEQ (~0ULL)
static const char http2_preface_bytes[HTTP2_PREFACE_LEN + 1] = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
// HTTPS는 암호문이라 ClientHello만 읽는다. SNI와 ALPN이 그 안에 평문으로 있다. post-quantum key share를 보내는 client는
// ClientHello가 2KB에 가깝고 확장 순서를 섞어서, SNI가 앞 512 byte 밖에 있을 수 있다.
#define TLS_HELLO_SIZE 4096
#define TLS_RECORD_HEADER_SIZE 5
#define TLS_CLIENT_HELLO 0x01
// TLS_HANDSHAKE는 record 머리 없이 handshake message로 시작하는 레코드다. 머리만 따로 읽는 서버에서 나온다.
#define TLS_HANDSHAKE 2
#define TLS_HANDSHAKE_HEADER_SIZE 4
#define TLS_PREFIX_SIZE (TLS_RECORD_HEADER_SIZE + TLS_HANDSHAKE_HEADER_SIZE)
#define TLS_CLIENT_HELLO_MIN_LENGTH 34
#define TLS_PREFIX_NON_TLS 1
#define TLS_PREFIX_CLIENT_HELLO 2
#define TLS_PREFIX_DONE 3
// TLS 접두 9 byte는 읽기 하나의 앞쪽 iovec 세 개까지만 따라간다. 바이트마다 펼친 반복이라 개수를 늘리면 verifier 비용이
// 곱으로 는다. [5, 4, 본문]처럼 record 머리와 handshake 머리를 따로 받는 readv도 세 개면 담긴다.
#define TLS_SEGMENTS 3

struct http_record {
	__u64 timestamp_ns;
	__u32 event_type;
	__u32 pid;
	__u64 cgroup_id;
	__u64 skaddr;
	__u32 len;
	__u16 family;
	__u8 direction;
	__u8 kind;
	__u16 sport;
	__u16 dport;
	__u8 source[16];
	__u8 destination[16];
	char comm[16];
	// offset은 조각이 message 안에서 시작하는 위치다. 사용자 공간은 기다리던 위치와 다르면 조각을 잃은 것으로 본다.
	// h2c에서는 연결 안의 위치다. 송신은 TCP 순번(write_seq), 수신은 그 방향에서 읽은 byte 수이고 둘 다 2^32에서 감긴다.
	// trace mysql에서는 kind가 로컬 port가 MySQL port인 서버 쪽이면 1이고, offset은 이번 읽기나 쓰기의 전체 byte 수다.
	__u32 offset;
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

// http_streams는 socket과 방향마다 지금 message에서 주고받은 byte 수다. HTTP로 시작하지 않는 읽기와 쓰기는 이 값이
// http_message_limit보다 작을 때만 앞 message의 이어지는 조각으로 넘긴다. --payload=all이 아니면 HTTP_SPLIT_SIZE보다
// 짧게 시작한 message만 둔다.
struct http_stream_key {
	__u64 skaddr;
	__u64 direction;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct http_stream_key);
	__type(value, __u64);
} http_streams SEC(".maps");

// tls_prefixes는 TLS record header와 handshake header까지만 기억한다. recvmsg 경계가 이 아홉 byte 사이 어디에
// 있어도 ClientHello 길이를 확인하고, 이후 암호문은 다시 검사하지 않는다.
struct tls_prefix {
	__u8 bytes[TLS_PREFIX_SIZE];
	__u8 count;
	__u8 terminal;
	__u32 offset;
	__u32 hello_end;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct http_stream_key);
	__type(value, struct tls_prefix);
} tls_prefixes SEC(".maps");

// http_cursor는 http_capture 반복의 위치다. 반복 상태를 register에 두면 verifier가 반복마다 값의 범위를 따로 추적해
// 경로를 합치지 못하고, 100만 명령 한도를 넘는다. map에서 다시 읽은 값은 범위를 모르는 값이라 경로가 합쳐진다.
struct http_cursor {
	__u64 pointer;
	__u64 left;
	__u64 remaining;
	__u64 offset;
	__u64 segment;
	__u64 iov;
	__u64 nr_segs;
	__u64 kind;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct http_cursor);
} http_cursors SEC(".maps");

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
	const struct iovec *iov;
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
	if (bpf_core_field_exists(msg->msg_iter.iov) && bpf_core_enum_value_exists(enum iter_type, ITER_IOVEC) &&
	    type == bpf_core_enum_value(enum iter_type, ITER_IOVEC)) {
		const struct iovec *iov = BPF_CORE_READ(msg, msg_iter.iov);
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
	       (p[0] == 'O' && p[1] == 'P' && p[2] == 'T' && p[3] == 'I') || (p[0] == 'H' && p[1] == 'T' && p[2] == 'T' && p[3] == 'P') ||
	       (p[0] == 'P' && p[1] == 'R' && p[2] == 'I' && p[3] == ' ');
}

// tls_start는 TLS handshake record의 머리다. version은 SSL 3.0부터 TLS 1.3까지다.
static __always_inline int tls_start(const __u8 *p) {
	return p[0] == 0x16 && p[1] == 0x03 && p[2] <= 0x04;
}

// tls_client_hello는 buffer의 at 위치가 ClientHello handshake인지 본다. ServerHello와 다른 handshake는 읽지 않는다.
static __always_inline int tls_client_hello(__u64 buffer, __u64 limit, __u64 at) {
	__u8 type = 0;
	return limit > at && !bpf_probe_read_user(&type, sizeof(type), (void *)(buffer + at)) && type == TLS_CLIENT_HELLO;
}

// tls_segments는 이번 읽기의 앞쪽 iovec이다. 0번은 msg_iter의 offset을 뺀 첫 buffer다.
struct tls_segments {
	__u64 base[TLS_SEGMENTS];
	__u64 len[TLS_SEGMENTS];
};

static __always_inline void tls_read_segments(struct tls_segments *segments, const struct iovec *iov, __u64 nr_segs) {
	#pragma unroll
	for (int index = 1; index < TLS_SEGMENTS; index++) {
		struct iovec vector = {};
		if (!iov || (__u64)index >= nr_segs || bpf_probe_read_kernel(&vector, sizeof(vector), iov + index)) {
			return;
		}
		segments->base[index] = (__u64)vector.iov_base;
		segments->len[index] = vector.iov_len;
	}
}

// tls_segment_at은 이번 읽기의 at번째 byte가 있는 iovec과 그 주소, 그 iovec에 남은 길이를 돌린다. 앞쪽 iovec을
// 모두 넘으면 -1이다. at이 앞쪽 iovec의 끝과 같으면 남은 길이 0을 돌려, 호출한 쪽이 다음 iovec으로 넘어가게 한다.
static __always_inline int tls_segment_at(const struct tls_segments *segments, __u64 at, __u64 *pointer, __u64 *left) {
	#pragma unroll
	for (int index = 0; index < TLS_SEGMENTS; index++) {
		if (at < segments->len[index]) {
			*pointer = segments->base[index] + at;
			*left = segments->len[index] - at;
			return index;
		}
		at -= segments->len[index];
	}
	if (at == 0) {
		*pointer = segments->base[TLS_SEGMENTS - 1] + segments->len[TLS_SEGMENTS - 1];
		*left = 0;
		return TLS_SEGMENTS - 1;
	}
	return -1;
}

// tls_prefix_client_hello는 이번 읽기가 첫 ClientHello type byte를 줄 때만 true를 돌린다. terminal 상태는 이후
// TLS 암호문을 새 record로 다시 읽지 않는다. 위치는 첫 iovec 길이가 아니라 이번 읽기의 byte 수(size)로 옮긴다.
static __always_inline int tls_prefix_client_hello(const struct http_stream_key *key, const struct tls_segments *segments, __u64 size, __u64 *output, __u64 *captured, __u8 *handshake) {
	struct tls_prefix next = {};
	struct tls_prefix *stored = bpf_map_lookup_elem(&tls_prefixes, key);
	if (stored) {
		if (stored->terminal == TLS_PREFIX_NON_TLS) {
			return 0;
		}
		if (stored->terminal == TLS_PREFIX_CLIENT_HELLO) {
			if (stored->offset >= stored->hello_end) {
				return 0;
			}
			__u64 available = size;
			__u64 remaining = stored->hello_end - stored->offset;
			if (available > remaining) {
				available = remaining;
			}
			next = *stored;
			next.offset += available;
			if (next.offset >= next.hello_end) {
				next.terminal = TLS_PREFIX_DONE;
			}
			bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
			*output = stored->offset;
			*captured = available;
			return 2;
		}
		__builtin_memcpy(&next, stored, sizeof(next));
	}
	if (next.count >= TLS_PREFIX_SIZE) {
		return 0;
	}
	__u64 available = size;
	if (available > TLS_PREFIX_SIZE - next.count) {
		available = TLS_PREFIX_SIZE - next.count;
	}
	#pragma unroll
	for (int index = 0; index < TLS_PREFIX_SIZE; index++) {
		if ((__u64)index >= available) {
			break;
		}
		__u8 byte = 0;
		__u64 pointer = 0;
		__u64 left = 0;
		// 읽지 못한 byte를 건너뛰면 다음 읽기의 byte를 이 자리에 이어 붙이게 된다. 그 socket은 더 따라가지 않는다.
		if (tls_segment_at(segments, index, &pointer, &left) < 0 || !left || bpf_probe_read_user(&byte, sizeof(byte), (void *)pointer)) {
			next.terminal = TLS_PREFIX_NON_TLS;
			bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
			return 0;
		}
		__u8 position = next.count;
		if (position == 0) {
			next.bytes[0] = byte;
		} else if (position == 1) {
			next.bytes[1] = byte;
		} else if (position == 2) {
			next.bytes[2] = byte;
		} else if (position == 3) {
			next.bytes[3] = byte;
		} else if (position == 4) {
			next.bytes[4] = byte;
		} else if (position == 5) {
			next.bytes[5] = byte;
		} else if (position == 6) {
			next.bytes[6] = byte;
		} else if (position == 7) {
			next.bytes[7] = byte;
		} else if (position == 8) {
			next.bytes[8] = byte;
		} else {
			return 0;
		}
		next.count = position + 1;
		if ((position == 0 && byte != 0x16) || (position == 1 && byte != 0x03) || (position == 2 && byte > 0x04)) {
			next.terminal = TLS_PREFIX_NON_TLS;
			bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
			return 0;
		}
		if (position == TLS_RECORD_HEADER_SIZE - 1) {
			__u16 record_length = ((__u16)next.bytes[3] << 8) | next.bytes[4];
			if (record_length < TLS_HANDSHAKE_HEADER_SIZE) {
				next.terminal = TLS_PREFIX_NON_TLS;
				bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
				return 0;
			}
		}
		if (position == TLS_PREFIX_SIZE - 1) {
			__u16 record_length = ((__u16)next.bytes[3] << 8) | next.bytes[4];
			__u32 handshake_length = ((__u32)next.bytes[6] << 16) | ((__u32)next.bytes[7] << 8) | next.bytes[8];
			if (next.bytes[5] != TLS_CLIENT_HELLO || handshake_length < TLS_CLIENT_HELLO_MIN_LENGTH || TLS_HANDSHAKE_HEADER_SIZE + handshake_length > record_length) {
				next.terminal = TLS_PREFIX_NON_TLS;
				bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
				return 0;
			}
			next.hello_end = TLS_HANDSHAKE_HEADER_SIZE + handshake_length;
			if (next.hello_end > TLS_HELLO_SIZE) {
				next.hello_end = TLS_HELLO_SIZE;
			}
			__u64 consumed = index + 1;
			__u64 body = size - consumed;
			if (body > next.hello_end - TLS_HANDSHAKE_HEADER_SIZE) {
				body = next.hello_end - TLS_HANDSHAKE_HEADER_SIZE;
			}
			next.offset = TLS_HANDSHAKE_HEADER_SIZE + body;
			if (next.offset >= next.hello_end) {
				next.offset = next.hello_end;
				next.terminal = TLS_PREFIX_DONE;
			} else {
				next.terminal = TLS_PREFIX_CLIENT_HELLO;
			}
			bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
		handshake[0] = next.bytes[5];
		handshake[1] = next.bytes[6];
		handshake[2] = next.bytes[7];
		handshake[3] = next.bytes[8];
			*output = consumed;
			*captured = body;
			return 1;
		}
	}
	bpf_map_update_elem(&tls_prefixes, key, &next, BPF_ANY);
	return 0;
}

static __always_inline int http_socket(struct sock *sk) {
	if (!http_port) {
		return 1;
	}
	__u16 local = BPF_CORE_READ(sk, __sk_common.skc_num);
	__u16 remote = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	return local == http_port || remote == http_port;
}

// mysql_socket은 MySQL port의 socket이 어느 쪽인지 돌려준다. 로컬 port가 mysql_port면 서버, 상대 port가 mysql_port면
// client이고 아니면 0이다. RST 뒤에는 kernel이 port를 지우므로 시작할 때 확인한 값을 쓴다.
#define MYSQL_NONE 0
#define MYSQL_CLIENT 1
#define MYSQL_SERVER 2
// 요청은 client가 보내고 서버가 받는 방향이다. 요청에 명령 SQL이 있어 응답보다 넉넉히 읽는다.
#define MYSQL_REQUEST_SIZE 4096
#define MYSQL_RESPONSE_SIZE 1024
static __always_inline __u8 mysql_socket(struct sock *sk) {
	if (BPF_CORE_READ(sk, __sk_common.skc_num) == mysql_port) {
		return MYSQL_SERVER;
	}
	if (bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport)) == mysql_port) {
		return MYSQL_CLIENT;
	}
	return MYSQL_NONE;
}

static __always_inline void http_fill_record(struct http_record *record, struct sock *sk, __u8 direction) {
	record->event_type = 10;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->skaddr = (__u64)sk;
	record->direction = direction;
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
}

static __always_inline int h2c_preface(__u64 buffer, __u64 limit, __u64 size) {
	__u8 head[HTTP2_PREFACE_LEN] = {};
	if (size < HTTP2_PREFACE_LEN || limit < HTTP2_PREFACE_LEN || bpf_probe_read_user(head, sizeof(head), (void *)buffer)) {
		return 0;
	}
	for (int i = 0; i < HTTP2_PREFACE_LEN; i++) {
		if (head[i] != (__u8)http2_preface_bytes[i]) {
			return 0;
		}
	}
	return 1;
}

// http_capture는 한 번의 읽기나 쓰기를 레코드로 넘긴다. buffer와 limit는 첫 버퍼이고, iov가 있으면 writev의 다음 버퍼를
// 이어서 읽는다. size는 이번 호출에서 주고받은 byte 수다. --payload=all이 아니면 HTTP로 시작하는 첫 버퍼의 앞
// http_payload_limit byte만 레코드 하나로 넘긴다. TLS는 ClientHello의 앞 TLS_HELLO_SIZE byte만 넘긴다.
static __always_inline void http_capture(struct sock *sk, __u64 buffer, __u64 limit, const struct iovec *iov, __u64 nr_segs, __u64 size, __u8 direction, __u64 seq) {
	// port는 부르는 쪽이 시작할 때(fentry) 확인한다. 끝날 때는 이미 늦을 수 있다. loopback에서 상대가 닫은 socket에 쓰면
	// RST가 같은 호출 안에서 처리되어, tcp_sendmsg가 끝날 때는 kernel이 로컬 port를 0으로 지워 두었다.
	if (!sk || !buffer || size == 0) {
		return;
	}
	struct http_stream_key key = {.skaddr = (__u64)sk, .direction = direction};
	__u8 peek[4] = {};
	__u64 peek_len = size < sizeof(peek) ? size : sizeof(peek);
	int peeked = limit >= peek_len && !bpf_probe_read_user(peek, peek_len, (void *)buffer);
	int readable = peek_len == sizeof(peek) && peeked;
	int start = readable && http_start(peek);
	// h2c 연결은 HTTP2_PREFACE_LEN byte 머리로 시작한다. 짧은 조각은 HTTP/1 서버가 몇 byte씩 엿보는 경우와 구별할 수 없다.
	int http2_start = readable && peek[0] == 'P' && peek[1] == 'R' && peek[2] == 'I' && peek[3] == ' ' && h2c_preface(buffer, limit, size);
	// TCP Fast Open은 송신 호출 안에서 SYN과 첫 순번을 정하므로 시작할 때의 write_seq가 첫 data의 순번이 아니다. 이런
	// 연결은 client 쪽 h2c를 따라가지 않는다. 위치를 틀리게 잡으면 첫 쓰기 뒤의 frame을 모두 버리게 된다. http_start는
	// "PRI "도 시작으로 보므로 start도 지워서, preface만 HTTP/1 레코드로 나가 반쪽 상태가 생기지 않게 한다.
	if (http2_start && direction == HTTP_SENT) {
		__u8 state = BPF_CORE_READ(sk, __sk_common.skc_state);
		if (state == TCP_CLOSE || state == TCP_SYN_SENT) {
			http2_start = 0;
			start = 0;
		}
	}
	// 모든 TCP 송수신이 여기를 지나므로 이어 받을 조각이 있는 socket에서만 map에 값이 있다. 이어 받는 조각을 TLS 판별보다
	// 먼저 보므로, 0x16 0x03으로 시작하는 HTTP body 조각을 TLS로 읽지 않는다. h2c 연결의 frame 안에서 "GET "처럼 보이는
	// 조각이 와도 HTTP/1 message로 보지 않도록 HTTP 시작보다 먼저 확인한다.
	__u64 *seen = bpf_map_lookup_elem(&http_streams, &key);
	int http2_going = seen && (*seen & HTTP2_STREAM);
	int http2 = http2_going || http2_start;
	start = !http2_going && (start || http2_start);
	__u8 kind = start ? HTTP_START : HTTP_CONTINUATION;
	__u64 offset = 0;
	__u64 budget = http_payload_limit;
	__u8 tls_handshake[TLS_HANDSHAKE_HEADER_SIZE] = {};
	int tls_synthetic = 0;
	__u64 segment = 0;
	int dropped = 0;
	struct http_stream_key peer = {.skaddr = (__u64)sk, .direction = direction == HTTP_SENT ? HTTP_RECEIVED : HTTP_SENT};
	int peer_marked = 0;
	if (!start && !seen) {
		__u64 tls_offset = 0;
		__u64 tls_captured = 0;
		struct tls_segments segments = {.base = {buffer}, .len = {limit}};
		tls_read_segments(&segments, iov, nr_segs);
		int tls = tls_prefix_client_hello(&key, &segments, size, &tls_offset, &tls_captured, tls_handshake);
		if (!tls) {
			return;
		}
		if (tls == 1) {
			// 본문은 접두가 끝난 byte부터 시작한다. 그 byte는 첫 iovec이 아닐 수 있다.
			int at = tls_segment_at(&segments, tls_offset, &buffer, &limit);
			if (at < 0) {
				return;
			}
			segment = at;
			size = tls_captured;
			offset = TLS_HANDSHAKE_HEADER_SIZE;
			kind = HTTP_CONTINUATION;
			tls_synthetic = 1;
		} else {
			size = tls_captured;
			offset = tls_offset;
			kind = HTTP_CONTINUATION;
		}
		budget = TLS_HELLO_SIZE;
	} else if (http2) {
		// h2c는 frame이 읽기와 쓰기 경계를 넘나들고 HPACK 표가 앞 frame에 기대므로, 연결의 byte를 빠짐없이 순서대로 넘긴다.
		// 위치를 레코드 offset에 실어, 잃은 byte가 사용자 공간에 위치 차이로 보인다.
		// 송신은 시작할 때 길이를 다 넘기지만 non-blocking socket은 일부만 보내고 나머지를 다시 쓴다. 그래서 송신 위치는
		// TCP 순번(write_seq)으로 둔다. 다시 쓴 byte는 같은 순번으로 오고 사용자 공간이 겹친 앞부분을 버린다.
		offset = seq != HTTP_NO_SEQ ? seq : http2_going ? *seen & ~HTTP2_STREAM : 0;
		__u64 next = HTTP2_STREAM | (offset + size);
		bpf_map_update_elem(&http_streams, &key, &next, BPF_ANY);
		if (!http2_going) {
			__u64 *other = bpf_map_lookup_elem(&http_streams, &peer);
			if (!other || !(*other & HTTP2_STREAM)) {
				__u64 begin = HTTP2_STREAM;
				bpf_map_update_elem(&http_streams, &peer, &begin, BPF_ANY);
				peer_marked = 1;
			}
		}
		budget = size;
	} else if (http_message_limit) {
		budget = http_message_limit;
		if (!start) {
			if (!seen || *seen >= http_message_limit) {
				return;
			}
			offset = *seen;
			budget = http_message_limit - offset;
		}
		// 레코드를 내지 못해도 실제로 주고받은 만큼 위치를 옮긴다. 그래야 잃은 조각이 사용자 공간에서 위치 차이로 드러난다.
		__u64 next = offset + size;
		bpf_map_update_elem(&http_streams, &key, &next, BPF_ANY);
	} else if (!start) {
		// 짧은 첫 조각을 본 socket이다.
		if (!seen) {
			return;
		}
		offset = *seen;
		bpf_map_delete_elem(&http_streams, &key);
		if (offset >= budget) {
			return;
		}
		budget -= offset;
		iov = 0;
	} else {
		if (size < HTTP_SPLIT_SIZE) {
			__u64 seen = size;
			bpf_map_update_elem(&http_streams, &key, &seen, BPF_ANY);
		}
		iov = 0;
	}
	__u32 zero = 0;
	struct http_record *record = bpf_map_lookup_elem(&http_scratch, &zero);
	if (!record) {
		return;
	}
	struct http_cursor *cursor = bpf_map_lookup_elem(&http_cursors, &zero);
	if (!cursor) {
		return;
	}
	http_fill_record(record, sk, direction);
	if (tls_synthetic) {
		__builtin_memcpy(record->payload, tls_handshake, TLS_HANDSHAKE_HEADER_SIZE);
		record->timestamp_ns = bpf_ktime_get_ns();
		record->len = TLS_HANDSHAKE_HEADER_SIZE;
		record->kind = TLS_HANDSHAKE;
		record->offset = 0;
		if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct http_record, payload) + TLS_HANDSHAKE_HEADER_SIZE, 0)) {
			return;
		}
	}
	cursor->pointer = buffer;
	cursor->left = limit;
	cursor->remaining = size < budget ? size : budget;
	cursor->offset = offset;
	cursor->segment = segment;
	cursor->iov = (__u64)iov;
	cursor->nr_segs = nr_segs;
	cursor->kind = kind;
	for (int step = 0; step < HTTP_MAX_STEPS; step++) {
		if (!cursor->remaining) {
			break;
		}
		if (!cursor->left) {
			__u64 segment = cursor->segment + 1;
			if (!cursor->iov || segment >= cursor->nr_segs || segment >= HTTP_MAX_SEGMENTS) {
				break;
			}
			struct iovec vector = {};
			if (bpf_probe_read_kernel(&vector, sizeof(vector), (const struct iovec *)cursor->iov + segment)) {
				break;
			}
			cursor->segment = segment;
			cursor->pointer = (__u64)vector.iov_base;
			cursor->left = vector.iov_len;
			continue;
		}
		__u64 len = cursor->left < cursor->remaining ? cursor->left : cursor->remaining;
		if (len > HTTP_PAYLOAD_SIZE) {
			len = HTTP_PAYLOAD_SIZE;
		}
		if (bpf_probe_read_user(record->payload, len, (void *)cursor->pointer)) {
			break;
		}
		record->timestamp_ns = bpf_ktime_get_ns();
		record->len = len;
		record->kind = cursor->kind;
		record->offset = cursor->offset;
		if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct http_record, payload) + len, 0)) {
			__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
			if (lost) {
				__sync_fetch_and_add(lost, 1);
			}
			dropped = 1;
			break;
		}
		cursor->pointer += len;
		cursor->left -= len;
		cursor->remaining -= len;
		cursor->offset += len;
		cursor->kind = HTTP_CONTINUATION;
	}
	// h2c에서 넘기지 못한 byte가 남으면 frame 경계와 HPACK 표를 되찾을 수 없다. 그 방향은 더 넘기지 않고 잃은 event로 센다.
	if (http2 && cursor->remaining) {
		bpf_map_delete_elem(&http_streams, &key);
		// 이 호출이 h2c를 시작하고 레코드를 하나도 넘기지 못했으면 사용자 공간은 이 연결을 모른다. 반대 방향도 넘기지 않는다.
		if (peer_marked && cursor->offset == offset) {
			bpf_map_delete_elem(&http_streams, &peer);
		}
		__u64 *lost = dropped ? 0 : bpf_map_lookup_elem(&lost_events, &zero);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
	}
}

// emit_mysql은 MySQL port socket의 읽기나 쓰기 앞부분을 레코드 하나로 넘긴다. TLS record로 시작하면 암호문이라 넘기지 않는다.
// 사용자 공간이 packet 경계를 따라가므로 이 함수는 상태를 두지 않는다.
static __always_inline void emit_mysql_iov(struct sock *sk, __u64 buffer, __u64 limit, const struct iovec *iov, __u64 nr_segs, __u64 size, __u8 direction, __u8 server) {
	if (!buffer || size == 0) {
		return;
	}
	__u32 zero = 0;
	struct http_record *record = bpf_map_lookup_elem(&http_scratch, &zero);
	if (!record) {
		return;
	}
	__u32 cap = server != (direction == HTTP_SENT) ? MYSQL_REQUEST_SIZE : MYSQL_RESPONSE_SIZE;
	if (cap >= MYSQL_REQUEST_SIZE) {
		cap = MYSQL_REQUEST_SIZE - 1;
	}
	struct http_cursor *cursor = bpf_map_lookup_elem(&http_cursors, &zero);
	if (!cursor) {
		return;
	}
	cursor->pointer = buffer;
	cursor->left = limit;
	cursor->remaining = size < cap ? size : cap;
	cursor->offset = 0;
	cursor->segment = 0;
	cursor->iov = (__u64)iov;
	cursor->nr_segs = nr_segs;
	for (int step = 0; step < HTTP_MAX_STEPS; step++) {
		if (!cursor->remaining || cursor->offset >= MYSQL_REQUEST_SIZE) {
			break;
		}
		if (!cursor->left) {
			__u64 segment = cursor->segment + 1;
			if (!cursor->iov || segment >= cursor->nr_segs || segment >= HTTP_MAX_SEGMENTS) {
				break;
			}
			struct iovec vector = {};
			if (bpf_probe_read_kernel(&vector, sizeof(vector), (const struct iovec *)cursor->iov + segment)) {
				break;
			}
			cursor->segment = segment;
			cursor->pointer = (__u64)vector.iov_base;
			cursor->left = vector.iov_len;
			continue;
		}
		__u32 offset = cursor->offset & (MYSQL_REQUEST_SIZE - 1);
		__u64 len = cursor->left < cursor->remaining ? cursor->left : cursor->remaining;
		if (len > 512) {
			len = 512;
		}
		if (len > MYSQL_REQUEST_SIZE - offset) {
			len = MYSQL_REQUEST_SIZE - offset;
		}
		if (bpf_probe_read_user(record->payload + offset, len, (void *)cursor->pointer)) {
			break;
		}
		cursor->pointer += len;
		cursor->left -= len;
		cursor->remaining -= len;
		cursor->offset += len;
	}
	__u32 copied = cursor->offset & (MYSQL_REQUEST_SIZE - 1);
	if (!copied) {
		return;
	}
	if (copied >= 3 && record->payload[0] >= 0x14 && record->payload[0] <= 0x17 && record->payload[1] == 0x03 && (record->payload[2] == 0x01 || record->payload[2] == 0x03)) {
		return;
	}
	http_fill_record(record, sk, direction);
	record->event_type = 11;
	record->timestamp_ns = bpf_ktime_get_ns();
	record->len = copied;
	record->kind = server;
	record->offset = (__u32)size;
	if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct http_record, payload) + copied, 0)) {
		__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
	}
}

static __always_inline void emit_mysql_buffer(struct sock *sk, __u64 buffer, __u64 limit, __u64 size, __u8 direction, __u8 server) {
	if (!buffer || size == 0) {
		return;
	}
	__u32 zero = 0;
	struct http_record *record = bpf_map_lookup_elem(&http_scratch, &zero);
	if (!record) {
		return;
	}
	__u64 cap = server != (direction == HTTP_SENT) ? MYSQL_REQUEST_SIZE : MYSQL_RESPONSE_SIZE;
	__u64 len = size < limit ? size : limit;
	if (len > cap) {
		len = cap;
	}
	if (len > MYSQL_REQUEST_SIZE) {
		len = MYSQL_REQUEST_SIZE;
	}
	if (len == 0 || bpf_probe_read_user(record->payload, len, (void *)buffer)) {
		return;
	}
	if (len >= 3 && record->payload[0] >= 0x14 && record->payload[0] <= 0x17 && record->payload[1] == 0x03 && (record->payload[2] == 0x01 || record->payload[2] == 0x03)) {
		return;
	}
	http_fill_record(record, sk, direction);
	record->event_type = 11;
	record->timestamp_ns = bpf_ktime_get_ns();
	record->len = len;
	record->kind = server;
	record->offset = (__u32)size;
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
	if (!bpf_core_field_exists(msg->msg_iter.iter_type) ||
	    (!bpf_core_field_exists(msg->msg_iter.__iov) && !bpf_core_field_exists(msg->msg_iter.iov)) ||
	    !bpf_core_field_exists(msg->msg_iter.nr_segs) || !bpf_core_enum_value_exists(enum iter_type, ITER_IOVEC)) {
		return 0;
	}
	if (BPF_CORE_READ(msg, msg_iter.iter_type) != bpf_core_enum_value(enum iter_type, ITER_IOVEC) || BPF_CORE_READ(msg, msg_iter.nr_segs) < 2 ||
	    BPF_CORE_READ(msg, msg_iter.iov_offset) != 0) {
		return 0;
	}
	const struct iovec *iov = bpf_core_field_exists(msg->msg_iter.__iov) ? BPF_CORE_READ(msg, msg_iter.__iov) : BPF_CORE_READ(msg, msg_iter.iov);
	*limit = BPF_CORE_READ(iov + 1, iov_len);
	return BPF_CORE_READ(iov + 1, iov_base);
}

// http_iov는 writev의 kernel iovec 배열이다. 첫 버퍼는 http_user_buffer가 iov_offset까지 반영해 읽는다.
static __always_inline const struct iovec *http_iov(struct msghdr *msg, __u64 *nr_segs) {
	if (!bpf_core_field_exists(msg->msg_iter.iter_type) ||
	    (!bpf_core_field_exists(msg->msg_iter.__iov) && !bpf_core_field_exists(msg->msg_iter.iov)) ||
	    !bpf_core_field_exists(msg->msg_iter.nr_segs) || !bpf_core_enum_value_exists(enum iter_type, ITER_IOVEC)) {
		return 0;
	}
	if (BPF_CORE_READ(msg, msg_iter.iter_type) != bpf_core_enum_value(enum iter_type, ITER_IOVEC)) {
		return 0;
	}
	*nr_segs = BPF_CORE_READ(msg, msg_iter.nr_segs);
	return bpf_core_field_exists(msg->msg_iter.__iov) ? BPF_CORE_READ(msg, msg_iter.__iov) : BPF_CORE_READ(msg, msg_iter.iov);
}

// --payload=all은 tcp_sendmsg가 끝난 뒤 실제로 보낸 byte 수만큼 넘긴다. non-blocking socket은 요청보다 적게 보내고
// 나머지를 다시 보내므로, 시작할 때 크기로 넘기면 같은 byte를 두 번 잇는다. 시작할 때는 버퍼 위치만 thread별로 둔다.
struct http_send_pending {
	__u64 skaddr;
	__u64 buffer;
	__u64 limit;
	__u64 iov;
	__u64 nr_segs;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct http_send_pending);
} http_send_pending SEC(".maps");

struct mysql_send_pending {
	struct http_send_pending send;
	__u8 server;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct mysql_send_pending);
} mysql_send_pending SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, __u64);
} tcp_length_pending SEC(".maps");

// ssl_snapshot은 SSL 호출 안에서 배운 socket의 주소다. 나중 호출은 struct sock*를 다시 읽지 않고 이 사본을 쓴다.
// 그 사이에 socket이 해제되어 주소가 새 socket에 쓰였을 수 있다.
struct ssl_snapshot {
	__u64 skaddr;
	__u16 family;
	__u16 sport;
	__u16 dport;
	__u8 source[16];
	__u8 destination[16];
};

// ssl_pending은 thread에서 진행 중인 SSL_read와 SSL_write다. 평문은 함수가 돌아와야 길이가 정해지고, SSL_read는 그때
// buffer에 평문이 있다. 그 사이에 같은 thread가 부르는 tcp_sendmsg와 tcp_recvmsg가 socket을 알려 준다.
struct ssl_pending {
	__u64 ssl;
	__u64 buffer;
	__u64 num;
	__u64 out;
	__u8 direction;
	__u8 learned;
	struct ssl_snapshot snapshot;
};

// uretprobe는 SSL 호출 안에서 끝난 thread에서는 실행되지 않아 항목이 남는다. HASH가 차면 새 thread의 평문을 아무 표시
// 없이 잃으므로, 남은 항목은 LRU가 밀어낸다.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct ssl_pending);
} ssl_pending SEC(".maps");

struct ssl_key {
	__u64 tgid;
	__u64 ssl;
};

// ssl_socks는 SSL 객체마다 마지막으로 배운 socket이다. OpenSSL은 미리 읽어 둔 record에서 평문을 돌려줄 때 socket을 읽지
// 않으므로, 그 호출은 앞 호출에서 배운 주소를 쓴다.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct ssl_key);
	__type(value, struct ssl_snapshot);
} ssl_socks SEC(".maps");

// tls_unmapped는 --port가 있을 때 socket을 몰라 버린 평문 레코드 수다. port를 확인할 수 없어 보이지 않은 수를 알린다.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} tls_unmapped SEC(".maps");

static __always_inline void ssl_snapshot_fill(struct ssl_snapshot *snapshot, struct sock *sk) {
	snapshot->skaddr = (__u64)sk;
	snapshot->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	snapshot->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	snapshot->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	__builtin_memset(snapshot->source, 0, sizeof(snapshot->source));
	__builtin_memset(snapshot->destination, 0, sizeof(snapshot->destination));
	if (snapshot->family == AF_INET) {
		__be32 source = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 destination = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(snapshot->source, &source, 4);
		__builtin_memcpy(snapshot->destination, &destination, 4);
	} else {
		BPF_CORE_READ_INTO(&snapshot->source, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&snapshot->destination, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
	}
}

// ssl_learn은 SSL 호출 안에서 불린 TCP 송수신의 socket을 그 호출과 SSL 객체에 적는다. port와 관계없이 배우고, --port는
// 평문을 낼 때 확인한다. 배우지 않으면 다른 port의 SSL 객체가 주소 없는 평문으로 잘못 세어진다.
static __always_inline void ssl_learn(struct sock *sk) {
	__u64 key = bpf_get_current_pid_tgid();
	struct ssl_pending *pending = bpf_map_lookup_elem(&ssl_pending, &key);
	if (!pending) {
		return;
	}
	ssl_snapshot_fill(&pending->snapshot, sk);
	pending->learned = 1;
	struct ssl_key sock_key = {.tgid = key >> 32, .ssl = pending->ssl};
	bpf_map_update_elem(&ssl_socks, &sock_key, &pending->snapshot, BPF_ANY);
}

#define UPROBE_ARM64 1

// SSL_CONTEXT_WORD는 kprobe ctx(struct pt_regs)의 off byte 위치를 읽는다. verifier는 ctx를 상수 offset으로만 읽게 한다.
// C로 쓰면 clang이 두 아키텍처의 읽기를 offset 고르기와 읽기 하나로 합쳐서 거절된다. inline asm은 합쳐지지 않는다.
#define SSL_CONTEXT_WORD(ctx, off)                                                    \
	({                                                                            \
		__u64 word;                                                           \
		asm volatile("%0 = *(u64 *)(%1 + " #off ")" : "=r"(word) : "r"(ctx)); \
		word;                                                                 \
	})

// ssl_argument는 uprobe의 n번째 정수 인자다. x86_64 pt_regs는 di, si, dx, cx가 112, 104, 96, 88 byte 위치이고 arm64는
// x0부터 8 byte씩이다. 두 아키텍처의 offset 모두 양쪽 pt_regs 크기 안이라 어느 갈래든 verifier를 통과한다.
static __always_inline __u64 ssl_argument(void *ctx, int n) {
	if (uprobe_arch == UPROBE_ARM64) {
		switch (n) {
		case 0:
			return SSL_CONTEXT_WORD(ctx, 0);
		case 1:
			return SSL_CONTEXT_WORD(ctx, 8);
		case 2:
			return SSL_CONTEXT_WORD(ctx, 16);
		default:
			return SSL_CONTEXT_WORD(ctx, 24);
		}
	}
	switch (n) {
	case 0:
		return SSL_CONTEXT_WORD(ctx, 112);
	case 1:
		return SSL_CONTEXT_WORD(ctx, 104);
	case 2:
		return SSL_CONTEXT_WORD(ctx, 96);
	default:
		return SSL_CONTEXT_WORD(ctx, 88);
	}
}

// ssl_return_value는 uretprobe의 int 반환값이다. x86_64는 ax(80 byte 위치), arm64는 x0다.
static __always_inline int ssl_return_value(void *ctx) {
	if (uprobe_arch == UPROBE_ARM64) {
		return (int)SSL_CONTEXT_WORD(ctx, 0);
	}
	return (int)SSL_CONTEXT_WORD(ctx, 80);
}

static __always_inline void ssl_fill_record(struct http_record *record, struct ssl_snapshot *snapshot, __u8 direction) {
	record->event_type = 10;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->skaddr = snapshot->skaddr;
	record->direction = direction;
	record->family = snapshot->family;
	record->sport = snapshot->sport;
	record->dport = snapshot->dport;
	__builtin_memcpy(record->source, snapshot->source, sizeof(record->source));
	__builtin_memcpy(record->destination, snapshot->destination, sizeof(record->destination));
	current_process_name(&record->comm);
}

// ssl_capture는 SSL 호출 하나의 평문을 레코드로 넘긴다. http_capture와 같은 stream 규칙을 따르되, 방향에 HTTP_DECRYPTED를
// 더한 key를 써서 같은 socket의 암호문 상태와 섞지 않는다. 평문은 buffer 하나이고 TLS 판별을 하지 않는다. TCP 송수신마다
// 도는 http_capture의 verifier 비용을 늘리지 않으려고 따로 둔다.
static __always_inline void ssl_capture(struct ssl_snapshot *snapshot, __u64 buffer, __u64 size, __u8 direction) {
	__u8 flagged = direction | HTTP_DECRYPTED;
	struct http_stream_key key = {.skaddr = snapshot->skaddr, .direction = flagged};
	__u8 peek[4] = {};
	int readable = size >= 4 && !bpf_probe_read_user(peek, sizeof(peek), (void *)buffer);
	int http2_start = readable && peek[0] == 'P' && peek[1] == 'R' && peek[2] == 'I' && peek[3] == ' ' && h2c_preface(buffer, size, size);
	__u64 *seen = bpf_map_lookup_elem(&http_streams, &key);
	int http2_going = seen && (*seen & HTTP2_STREAM);
	int http2 = http2_going || http2_start;
	int start = !http2_going && readable && (http_start(peek) || http2_start);
	__u8 kind = start ? HTTP_START : HTTP_CONTINUATION;
	__u64 offset = 0;
	__u64 budget = http_payload_limit;
	struct http_stream_key peer = {.skaddr = snapshot->skaddr, .direction = (direction == HTTP_SENT ? HTTP_RECEIVED : HTTP_SENT) | HTTP_DECRYPTED};
	int peer_marked = 0;
	if (http2) {
		// HTTP/2는 frame이 SSL 호출 경계를 넘나들고 HPACK 표가 앞 frame에 기대므로, h2c처럼 연결의 평문을 빠짐없이 순서대로
		// 넘긴다. SSL 호출은 돌려준 byte만 주고받으므로 두 방향 모두 0부터 센 위치를 offset에 싣는다.
		offset = http2_going ? *seen & ~HTTP2_STREAM : 0;
		__u64 next = HTTP2_STREAM | (offset + size);
		bpf_map_update_elem(&http_streams, &key, &next, BPF_ANY);
		if (!http2_going) {
			__u64 *other = bpf_map_lookup_elem(&http_streams, &peer);
			if (!other || !(*other & HTTP2_STREAM)) {
				__u64 begin = HTTP2_STREAM;
				bpf_map_update_elem(&http_streams, &peer, &begin, BPF_ANY);
				peer_marked = 1;
			}
		}
		budget = size;
	} else if (http_message_limit) {
		budget = http_message_limit;
		if (!start) {
			if (!seen || *seen >= http_message_limit) {
				return;
			}
			offset = *seen;
			budget = http_message_limit - offset;
		}
		__u64 next = offset + size;
		bpf_map_update_elem(&http_streams, &key, &next, BPF_ANY);
	} else if (!start) {
		if (!seen) {
			return;
		}
		offset = *seen;
		bpf_map_delete_elem(&http_streams, &key);
		if (offset >= budget) {
			return;
		}
		budget -= offset;
	} else if (size < HTTP_SPLIT_SIZE) {
		__u64 first = size;
		bpf_map_update_elem(&http_streams, &key, &first, BPF_ANY);
	}
	__u32 zero = 0;
	struct http_record *record = bpf_map_lookup_elem(&http_scratch, &zero);
	if (!record) {
		return;
	}
	struct http_cursor *cursor = bpf_map_lookup_elem(&http_cursors, &zero);
	if (!cursor) {
		return;
	}
	ssl_fill_record(record, snapshot, flagged);
	cursor->pointer = buffer;
	cursor->remaining = size < budget ? size : budget;
	cursor->offset = offset;
	cursor->kind = kind;
	int dropped = 0;
	for (int step = 0; step < HTTP_MAX_STEPS; step++) {
		if (!cursor->remaining) {
			break;
		}
		__u64 len = cursor->remaining;
		if (len > HTTP_PAYLOAD_SIZE) {
			len = HTTP_PAYLOAD_SIZE;
		}
		if (bpf_probe_read_user(record->payload, len, (void *)cursor->pointer)) {
			break;
		}
		record->timestamp_ns = bpf_ktime_get_ns();
		record->len = len;
		record->kind = cursor->kind;
		record->offset = cursor->offset;
		if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct http_record, payload) + len, 0)) {
			__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
			if (lost) {
				__sync_fetch_and_add(lost, 1);
			}
			dropped = 1;
			break;
		}
		cursor->pointer += len;
		cursor->remaining -= len;
		cursor->offset += len;
		cursor->kind = HTTP_CONTINUATION;
	}
	// HTTP/2에서 넘기지 못한 byte가 남으면 frame 경계와 HPACK 표를 되찾을 수 없다. h2c와 같이 그 방향을 멈추고 잃은 event로 센다.
	if (http2 && cursor->remaining) {
		bpf_map_delete_elem(&http_streams, &key);
		if (peer_marked && cursor->offset == offset) {
			bpf_map_delete_elem(&http_streams, &peer);
		}
		__u64 *lost = dropped ? 0 : bpf_map_lookup_elem(&lost_events, &zero);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
	}
}

SEC("fentry/tcp_sendmsg")
int tcp_sendmsg_entry(__u64 *ctx) {
	struct sock *sk = (struct sock *)ctx[0];
	// SSL_write 안의 송신이면 그 SSL 객체에 socket을 적는다. 아래의 port와 protocol 확인보다 먼저 해야 한다.
	if (emit_tls_plaintext && sk) {
		ssl_learn(sk);
	}
	struct msghdr *msg = (struct msghdr *)ctx[1];
	__u64 size = ctx[2];
	__u64 limit = 0;
	void *buffer = http_user_buffer(msg, &limit);
	__u64 first = size < limit ? size : limit;
	if (tcp_length_fallback) {
		__u64 key = bpf_get_current_pid_tgid();
		__u64 skaddr = (__u64)sk;
		bpf_map_update_elem(&tcp_length_pending, &key, &skaddr, BPF_ANY);
		return 0;
	}
	if (emit_dns_tcp_messages && sk && dns_tcp_socket(sk)) {
		emit_dns_tcp(sk, buffer, first, DNS_SENT);
		if (first == 2 && size > 2) {
			__u64 second_limit = 0;
			void *second = iov_second_buffer(msg, &second_limit);
			emit_dns_tcp(sk, second, size - 2 < second_limit ? size - 2 : second_limit, DNS_SENT);
		}
		return 0;
	}
	if (mysql_port && sk) {
		__u8 side = mysql_socket(sk);
		if (side) {
			struct mysql_send_pending pending = {.send = {.skaddr = (__u64)sk, .buffer = (__u64)buffer, .limit = limit, .nr_segs = 1}, .server = side == MYSQL_SERVER};
			pending.send.iov = (__u64)http_iov(msg, &pending.send.nr_segs);
			__u64 key = bpf_get_current_pid_tgid();
			bpf_map_update_elem(&mysql_send_pending, &key, &pending, BPF_ANY);
			return 0;
		}
	}
	if (!emit_http_messages || !sk || !buffer || !http_socket(sk)) {
		return 0;
	}
	if (!http_message_limit) {
		__u64 nr_segs = 1;
		const struct iovec *iov = http_iov(msg, &nr_segs);
		http_capture(sk, (__u64)buffer, limit, iov, nr_segs, size, HTTP_SENT, BPF_CORE_READ((struct tcp_sock *)sk, write_seq));
		return 0;
	}
	struct http_send_pending pending = {.skaddr = (__u64)sk, .buffer = (__u64)buffer, .limit = limit, .nr_segs = 1};
	pending.iov = (__u64)http_iov(msg, &pending.nr_segs);
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&http_send_pending, &key, &pending, BPF_ANY);
	return 0;
}

SEC("fexit/tcp_sendmsg")
int tcp_sendmsg_exit(__u64 *ctx) {
	__u64 key = bpf_get_current_pid_tgid();
	__u64 *tcp_stored = bpf_map_lookup_elem(&tcp_length_pending, &key);
	if (tcp_stored) {
		__u64 skaddr = *tcp_stored;
		bpf_map_delete_elem(&tcp_length_pending, &key);
		return emit_tcp_length_fallback((struct sock *)skaddr, (int)ctx[3], 6);
	}
	struct mysql_send_pending *mysql_stored = bpf_map_lookup_elem(&mysql_send_pending, &key);
	if (mysql_stored) {
		struct mysql_send_pending pending = *mysql_stored;
		bpf_map_delete_elem(&mysql_send_pending, &key);
		int sent = (int)ctx[3];
		if (sent > 0) {
			emit_mysql_iov((struct sock *)pending.send.skaddr, pending.send.buffer, pending.send.limit, (const struct iovec *)pending.send.iov, pending.send.nr_segs, sent, HTTP_SENT, pending.server);
		}
		return 0;
	}
	struct http_send_pending *stored = bpf_map_lookup_elem(&http_send_pending, &key);
	if (!stored) {
		return 0;
	}
	struct http_send_pending pending = *stored;
	bpf_map_delete_elem(&http_send_pending, &key);
	int sent = (int)ctx[3];
	if (sent > 0) {
		// 끝날 때 write_seq는 보낸 만큼 앞서 있다. BPF_CORE_READ 안에서 pending을 읽으면 그 접근도 kernel 타입으로
		// 옮기려다 실패하므로 pointer를 먼저 꺼낸다.
		struct tcp_sock *tcp = (struct tcp_sock *)pending.skaddr;
		__u32 seq = BPF_CORE_READ(tcp, write_seq) - sent;
		http_capture((struct sock *)pending.skaddr, pending.buffer, pending.limit, (const struct iovec *)pending.iov, pending.nr_segs, sent, HTTP_SENT, seq);
	}
	return 0;
}

SEC("fentry/tcp_cleanup_rbuf")
int tcp_cleanup_rbuf_entry(__u64 *ctx) {
	return emit_tcp_length_fallback((struct sock *)ctx[0], (int)ctx[1], 7);
}

// tcp_recvmsg가 끝나야 사용자 버퍼에 data가 있다. 시작할 때 버퍼 위치를 thread별로 두고 끝날 때 읽는다.
struct http_recv_pending {
	__u64 skaddr;
	__u64 buffer;
	__u64 limit;
	__u64 iov;
	__u64 nr_segs;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct http_recv_pending);
} http_recv_pending SEC(".maps");

SEC("fentry/tcp_recvmsg")
int tcp_recvmsg_entry(__u64 *ctx) {
	struct sock *sk = (struct sock *)ctx[0];
	// SSL_read 안의 수신이면 그 SSL 객체에 socket을 적는다. MSG_PEEK와 port 확인보다 먼저 해야 한다.
	if (emit_tls_plaintext && sk) {
		ssl_learn(sk);
	}
	// MSG_PEEK로 읽은 data는 다음 recv가 다시 읽는다. 같은 message를 두 번 내지 않는다.
	if ((int)ctx[3] & MSG_PEEK) {
		return 0;
	}
	// 끝날 때 쓰지 않을 socket이면 버퍼 위치를 기록하지 않는다. recv마다 map을 갱신하는 비용이 크다.
	if (!sk || !((emit_dns_tcp_messages && dns_tcp_socket(sk)) || (emit_http_messages && http_socket(sk)) || (mysql_port && mysql_socket(sk)))) {
		return 0;
	}
	struct http_recv_pending pending = {.skaddr = ctx[0], .nr_segs = 1};
	void *buffer = http_user_buffer((struct msghdr *)ctx[1], &pending.limit);
	if (!buffer) {
		return 0;
	}
	pending.buffer = (__u64)buffer;
	// readv와 recvmsg는 받은 byte를 여러 iovec에 나눠 담는다. 첫 iovec만 읽으면 그 뒤의 byte를 놓친다.
	pending.iov = (__u64)http_iov((struct msghdr *)ctx[1], &pending.nr_segs);
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&http_recv_pending, &key, &pending, BPF_ANY);
	return 0;
}

static __always_inline int finish_tcp_recvmsg(__u64 *ctx, int copied) {
	__u64 key = bpf_get_current_pid_tgid();
	struct http_recv_pending *pending = bpf_map_lookup_elem(&http_recv_pending, &key);
	if (!pending) {
		return 0;
	}
	if (copied > 0) {
		__u64 size = (__u64)copied < pending->limit ? (__u64)copied : pending->limit;
		struct sock *sk = (struct sock *)pending->skaddr;
		if (emit_dns_tcp_messages && sk && dns_tcp_socket(sk)) {
			emit_dns_tcp(sk, (void *)pending->buffer, size, DNS_RECEIVED);
		} else if (mysql_port && sk) {
			__u8 side = mysql_socket(sk);
			if (side) {
				emit_mysql_buffer(sk, pending->buffer, pending->limit, (__u64)copied, HTTP_RECEIVED, side == MYSQL_SERVER);
			}
		} else if (emit_http_messages) {
			http_capture(sk, pending->buffer, pending->limit, (const struct iovec *)pending->iov, pending->nr_segs, (__u64)copied, HTTP_RECEIVED, HTTP_NO_SEQ);
		}
	}
	bpf_map_delete_elem(&http_recv_pending, &key);
	return 0;
}

SEC("fexit/tcp_recvmsg")
int tcp_recvmsg_exit(__u64 *ctx) {
	return finish_tcp_recvmsg(ctx, *(int *)&ctx[5]);
}

SEC("fexit/tcp_recvmsg")
int tcp_recvmsg_exit_legacy(__u64 *ctx) {
	return finish_tcp_recvmsg(ctx, *(int *)&ctx[6]);
}

// trace http는 끝난 socket의 짝짓기 상태를 지운다. kernel은 해제한 socket의 주소를 새 socket에 다시 써서, 응답을 놓친
// 요청이 남으면 새 연결의 응답이 그 요청과 짝지어진다. tracepoint는 tracefs가 있어야 붙으므로, tracefs가 없는
// container에서도 붙는 tp_btf를 쓴다. trace tcp는 tcp_destroy_sock tracepoint를 따로 쓴다.
SEC("tp_btf/tcp_destroy_sock")
int http_tcp_destroy_sock(__u64 *ctx) {
	struct sock *sk = (struct sock *)ctx[0];
	if (!sk) {
		return 0;
	}
	// 짧은 첫 조각의 위치나 TLS 접두 상태가 남으면, 같은 주소를 다시 쓴 새 연결의 첫 읽기를 앞 message의 이어지는
	// 조각으로 넘기거나 ClientHello를 놓친다. 받은 socket은 닫힐 때 local port를 먼저 놓으므로, --port가 있으면
	// 여기서 http_socket이 거짓이 된다. 그래서 port와 관계없이 지운다.
	if (emit_http_messages) {
		struct http_stream_key key = {.skaddr = (__u64)sk, .direction = HTTP_SENT};
		bpf_map_delete_elem(&http_streams, &key);
		bpf_map_delete_elem(&tls_prefixes, &key);
		key.direction = HTTP_RECEIVED;
		bpf_map_delete_elem(&http_streams, &key);
		bpf_map_delete_elem(&tls_prefixes, &key);
	}
	if (!((emit_http_messages && http_socket(sk)) || (mysql_port && mysql_socket(sk)) || (emit_dns_tcp_messages && dns_tcp_socket(sk)))) {
		return 0;
	}
	struct event *event = start_event(ctx, 5);
	if (!event) {
		return 0;
	}
	event->skaddr = (__u64)sk;
	finish_event(event);
	return 0;
}

// ssl_synthetic_socket은 평문 레코드의 짝짓기 id다. 상위 32 bit가 tgid라 2^54보다 작아서, 상위 bit가 모두 1인 kernel의
// socket 주소와 겹치지 않는다. 한 process에서 SSL 객체 둘의 하위 32 bit가 같으려면 주소가 4GiB 간격이어야 한다.
static __always_inline __u64 ssl_synthetic_socket(__u64 pid_tgid, __u64 ssl) {
	return ((pid_tgid >> 32) << 32) | (ssl & 0xffffffff);
}

// ssl_enter는 SSL_read, SSL_write와 _ex의 시작이다. 인자는 (SSL *ssl, void *buf, num[, size_t *out])이다. SSL_read와
// SSL_write의 num은 int라서 상위 32 bit를 버린다. 중첩된 호출은 덮어쓰고 반환에서 지우므로 레코드는 한 번만 나간다.
static __always_inline int ssl_enter(void *ctx, __u8 direction, int ex, int wide) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	struct ssl_pending pending = {};
	pending.ssl = ssl_argument(ctx, 0);
	pending.buffer = ssl_argument(ctx, 1);
	pending.num = wide ? ssl_argument(ctx, 2) : (__u32)ssl_argument(ctx, 2);
	if (ex) {
		pending.out = ssl_argument(ctx, 3);
	}
	pending.direction = direction;
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&ssl_pending, &key, &pending, BPF_ANY);
	return 0;
}

// ssl_leave는 반환에서 길이를 정한다. SSL_read와 SSL_write는 양수 반환값이 byte 수이고, _ex는 1을 돌려주고 길이를 out에
// 쓴다. 0 이하(오류, WANT_READ, WANT_WRITE, 종료)는 평문이 없다.
static __always_inline int ssl_leave(void *ctx, int ex, int success) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	__u64 key = bpf_get_current_pid_tgid();
	struct ssl_pending *stored = bpf_map_lookup_elem(&ssl_pending, &key);
	if (!stored) {
		return 0;
	}
	struct ssl_pending pending = *stored;
	bpf_map_delete_elem(&ssl_pending, &key);
	int result = ssl_return_value(ctx);
	__u64 size = 0;
	if (ex) {
		if (result != success || !pending.out || bpf_probe_read_user(&size, sizeof(size), (void *)pending.out)) {
			return 0;
		}
	} else {
		if (result <= 0) {
			return 0;
		}
		size = result;
	}
	if (size > pending.num) {
		size = pending.num;
	}
	if (!size || !pending.buffer) {
		return 0;
	}
	struct ssl_snapshot snapshot = {};
	if (pending.learned) {
		snapshot = pending.snapshot;
	} else {
		struct ssl_key sock_key = {.tgid = key >> 32, .ssl = pending.ssl};
		struct ssl_snapshot *known = bpf_map_lookup_elem(&ssl_socks, &sock_key);
		if (known) {
			snapshot = *known;
		}
	}
	if (!snapshot.skaddr) {
		// memory BIO(node)나 trace 전에 받아 둔 record에서 온 평문이다. --port가 있으면 port를 확인할 수 없어 버리고 센다.
		if (http_port) {
			__u32 zero = 0;
			__u64 *unmapped = bpf_map_lookup_elem(&tls_unmapped, &zero);
			if (unmapped) {
				__sync_fetch_and_add(unmapped, 1);
			}
			return 0;
		}
	} else if (http_port && snapshot.sport != http_port && snapshot.dport != http_port) {
		return 0;
	}
	// 짝짓기 id는 socket을 배웠어도 SSL 객체로 둔다. OpenSSL은 handshake 때 미리 읽어 둔 요청을 socket 없이 돌려주고, 응답을
	// 쓸 때 socket을 배운다. socket 주소로 바꾸면 그 요청과 응답이 다른 연결로 나뉜다. 주소는 배운 값을 그대로 싣는다.
	snapshot.skaddr = ssl_synthetic_socket(key, pending.ssl);
	ssl_capture(&snapshot, pending.buffer, size, pending.direction);
	return 0;
}

SEC("uretprobe/rustls_connection_read")
int rustls_exit(void *ctx) {
	return ssl_leave(ctx, 1, 7000);
}

SEC("uprobe/mbedtls_ssl_read")
int mbed_read_entry(void *ctx) {
	return ssl_enter(ctx, HTTP_RECEIVED, 0, 1);
}

SEC("uprobe/mbedtls_ssl_write")
int mbed_write_entry(void *ctx) {
	return ssl_enter(ctx, HTTP_SENT, 0, 1);
}

SEC("uprobe/SSL_write")
int ssl_write_entry(void *ctx) {
	return ssl_enter(ctx, HTTP_SENT, 0, 0);
}

SEC("uretprobe/SSL_write")
int ssl_write_exit(void *ctx) {
	return ssl_leave(ctx, 0, 0);
}

SEC("uprobe/SSL_write_ex")
int ssl_write_ex_entry(void *ctx) {
	return ssl_enter(ctx, HTTP_SENT, 1, 1);
}

SEC("uretprobe/SSL_write_ex")
int ssl_write_ex_exit(void *ctx) {
	return ssl_leave(ctx, 1, 1);
}

SEC("uprobe/SSL_read")
int ssl_read_entry(void *ctx) {
	return ssl_enter(ctx, HTTP_RECEIVED, 0, 0);
}

SEC("uretprobe/SSL_read")
int ssl_read_exit(void *ctx) {
	return ssl_leave(ctx, 0, 0);
}

SEC("uprobe/SSL_read_ex")
int ssl_read_ex_entry(void *ctx) {
	return ssl_enter(ctx, HTTP_RECEIVED, 1, 1);
}

SEC("uretprobe/SSL_read_ex")
int ssl_read_ex_exit(void *ctx) {
	return ssl_leave(ctx, 1, 1);
}

// SSL_free는 SSL 객체의 끝이다. 평문 레코드는 모두 SSL 객체의 짝짓기 id를 쓰므로, 여기서 배운 socket과 평문 stream 상태를
// 지우고 끝 레코드를 내서 사용자 공간이 응답을 놓친 요청과 조각 상태를 지우게 한다.
static __always_inline int ssl_forget(void *ctx, __u64 ssl) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	struct ssl_key sock_key = {.tgid = pid_tgid >> 32, .ssl = ssl};
	bpf_map_delete_elem(&ssl_socks, &sock_key);
	__u64 synthetic = ssl_synthetic_socket(pid_tgid, ssl);
	struct http_stream_key key = {.skaddr = synthetic, .direction = HTTP_SENT | HTTP_DECRYPTED};
	bpf_map_delete_elem(&http_streams, &key);
	key.direction = HTTP_RECEIVED | HTTP_DECRYPTED;
	bpf_map_delete_elem(&http_streams, &key);
	struct event *event = start_event(ctx, 5);
	if (!event) {
		return 0;
	}
	event->skaddr = synthetic;
	finish_event(event);
	return 0;
}

SEC("uprobe/SSL_free")
int ssl_free_entry(void *ctx) {
	return ssl_forget(ctx, ssl_argument(ctx, 0));
}

#define NSS_SECURITY 1
#define NSS_MODE_ON 1
#define NSS_MODE_OFF 2
#define NSS_COPY_CONFIG 1
#define NSS_SET_OPTION 2
#define NSS_SET_DEFAULT 3
#define NSS_ACCEPT_CONFIG 4
#define NSPR_MSG_PEEK 2

struct nss_call_key {
	__u64 thread;
	__u64 stack;
	__u64 cookie;
};

struct nss_control {
	__u64 fd;
	__u32 mode;
	__u32 kind;
};

struct nss_io_key {
	__u64 thread;
	__u64 stack;
};

struct nss_io_call {
	__u64 cookie;
	struct ssl_pending pending;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct ssl_key);
	__type(value, __u32);
} nss_configs SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 10240);
	__type(key, __u32);
	__type(value, __u32);
} nss_defaults SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 10240);
	__type(key, struct nss_call_key);
	__type(value, struct nss_control);
} nss_control_calls SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 10240);
	__type(key, struct nss_io_key);
	__type(value, struct nss_io_call);
} nss_io_calls SEC(".maps");

static __always_inline __u64 nss_stack(void *ctx, int returning) {
	if (uprobe_arch == UPROBE_ARM64) {
		return SSL_CONTEXT_WORD(ctx, 248);
	}
	__u64 stack = SSL_CONTEXT_WORD(ctx, 152);
	return returning ? stack - 8 : stack;
}

static __always_inline void nss_lost(void) {
	__u32 zero = 0;
	__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
	if (lost) {
		__sync_fetch_and_add(lost, 1);
	}
}

static __always_inline __u32 nss_mode(__u64 thread, __u64 fd) {
	struct ssl_key key = {.tgid = thread >> 32, .ssl = fd};
	__u32 *mode = bpf_map_lookup_elem(&nss_configs, &key);
	return mode ? *mode : 0;
}

static __always_inline int nss_control_enter(void *ctx, struct nss_control *control) {
	struct nss_call_key key = {
		.thread = bpf_get_current_pid_tgid(),
		.stack = nss_stack(ctx, 0),
		.cookie = bpf_get_attach_cookie(ctx),
	};
	if (bpf_map_update_elem(&nss_control_calls, &key, control, BPF_ANY)) {
		nss_lost();
	}
	return 0;
}

SEC("uprobe/SSL_ImportFD")
int nss_import_entry(void *ctx) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	__u64 thread = bpf_get_current_pid_tgid();
	__u64 model = ssl_argument(ctx, 0);
	struct nss_control control = {.kind = NSS_COPY_CONFIG};
	if (model) {
		control.mode = nss_mode(thread, model);
	} else {
		__u32 pid = thread >> 32;
		__u32 *mode = bpf_map_lookup_elem(&nss_defaults, &pid);
		// NSS의 SSL_SECURITY 기본값은 on이다. 이후의 기본값 변경은 SSL_OptionSetDefault에서 기록한다.
		control.mode = mode ? *mode : NSS_MODE_ON;
	}
	return nss_control_enter(ctx, &control);
}

SEC("uprobe/SSL_OptionSet")
int nss_option_entry(void *ctx) {
	if (!emit_tls_plaintext || (__u32)ssl_argument(ctx, 1) != NSS_SECURITY) {
		return 0;
	}
	struct nss_control control = {
		.fd = ssl_argument(ctx, 0),
		.mode = ssl_argument(ctx, 2) ? NSS_MODE_ON : NSS_MODE_OFF,
		.kind = NSS_SET_OPTION,
	};
	return nss_control_enter(ctx, &control);
}

SEC("uprobe/SSL_OptionSetDefault")
int nss_default_entry(void *ctx) {
	if (!emit_tls_plaintext || (__u32)ssl_argument(ctx, 0) != NSS_SECURITY) {
		return 0;
	}
	struct nss_control control = {
		.mode = ssl_argument(ctx, 1) ? NSS_MODE_ON : NSS_MODE_OFF,
		.kind = NSS_SET_DEFAULT,
	};
	return nss_control_enter(ctx, &control);
}

SEC("uprobe/PR_Accept")
int nss_accept_entry(void *ctx) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	struct nss_control control = {
		.mode = nss_mode(bpf_get_current_pid_tgid(), ssl_argument(ctx, 0)),
		.kind = NSS_ACCEPT_CONFIG,
	};
	if (!control.mode) {
		return 0;
	}
	return nss_control_enter(ctx, &control);
}

SEC("uretprobe/NSS_config")
int nss_control_exit(void *ctx) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	struct nss_call_key key = {
		.thread = bpf_get_current_pid_tgid(),
		.stack = nss_stack(ctx, 1),
		.cookie = bpf_get_attach_cookie(ctx),
	};
	struct nss_control *stored = bpf_map_lookup_elem(&nss_control_calls, &key);
	if (!stored) {
		return 0;
	}
	struct nss_control control = *stored;
	bpf_map_delete_elem(&nss_control_calls, &key);
	if (control.kind == NSS_COPY_CONFIG || control.kind == NSS_ACCEPT_CONFIG) {
		control.fd = uprobe_arch == UPROBE_ARM64 ? SSL_CONTEXT_WORD(ctx, 0) : SSL_CONTEXT_WORD(ctx, 80);
		if (!control.fd) {
			return 0;
		}
	} else if (ssl_return_value(ctx) != 0) {
		return 0;
	}
	if (control.kind == NSS_SET_DEFAULT) {
		__u32 pid = key.thread >> 32;
		if (bpf_map_update_elem(&nss_defaults, &pid, &control.mode, BPF_ANY)) {
			nss_lost();
		}
		return 0;
	}
	struct ssl_key config_key = {.tgid = key.thread >> 32, .ssl = control.fd};
	if (!control.mode) {
		bpf_map_delete_elem(&nss_configs, &config_key);
	} else if (bpf_map_update_elem(&nss_configs, &config_key, &control.mode, BPF_ANY)) {
		nss_lost();
	}
	return ssl_forget(ctx, control.fd);
}

static __always_inline int nss_io_enter(void *ctx, __u8 direction) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	__u64 thread = bpf_get_current_pid_tgid();
	if (nss_mode(thread, ssl_argument(ctx, 0)) != NSS_MODE_ON) {
		return 0;
	}
	// 같은 반환 frame의 항목은 덮어쓴다. 반환 probe가 돌지 않아 남은 항목이 이후 호출을 막지 않고, tail call로 같은 frame에
	// 두 함수가 들어와도 반환에서 cookie가 맞는 마지막 호출만 평문을 낸다.
	struct nss_io_key key = {.thread = thread, .stack = nss_stack(ctx, 0)};
	ssl_enter(ctx, direction, 0, 0);
	struct ssl_pending *pending = bpf_map_lookup_elem(&ssl_pending, &thread);
	if (!pending) {
		nss_lost();
		return 0;
	}
	struct nss_io_call call = {.cookie = bpf_get_attach_cookie(ctx), .pending = *pending};
	if (bpf_map_update_elem(&nss_io_calls, &key, &call, BPF_ANY)) {
		bpf_map_delete_elem(&ssl_pending, &thread);
		nss_lost();
	}
	return 0;
}

SEC("uprobe/PR_Read")
int nss_read_entry(void *ctx) {
	return nss_io_enter(ctx, HTTP_RECEIVED);
}

SEC("uprobe/PR_Recv")
int nss_recv_entry(void *ctx) {
	if ((__u32)ssl_argument(ctx, 3) & NSPR_MSG_PEEK) {
		return 0;
	}
	return nss_io_enter(ctx, HTTP_RECEIVED);
}

SEC("uprobe/PR_Write")
int nss_write_entry(void *ctx) {
	return nss_io_enter(ctx, HTTP_SENT);
}

SEC("uretprobe/NSPR_IO")
int nss_io_exit(void *ctx) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	struct nss_io_key key = {.thread = bpf_get_current_pid_tgid(), .stack = nss_stack(ctx, 1)};
	struct nss_io_call *stored = bpf_map_lookup_elem(&nss_io_calls, &key);
	if (!stored || stored->cookie != bpf_get_attach_cookie(ctx)) {
		return 0;
	}
	struct ssl_pending pending = stored->pending;
	bpf_map_delete_elem(&nss_io_calls, &key);
	if (bpf_map_update_elem(&ssl_pending, &key.thread, &pending, BPF_ANY)) {
		nss_lost();
		return 0;
	}
	return ssl_leave(ctx, 0, 0);
}

SEC("uprobe/PR_Close")
int nss_close_entry(void *ctx) {
	if (!emit_tls_plaintext) {
		return 0;
	}
	__u64 fd = ssl_argument(ctx, 0);
	struct ssl_key key = {.tgid = bpf_get_current_pid_tgid() >> 32, .ssl = fd};
	if (!bpf_map_lookup_elem(&nss_configs, &key)) {
		return 0;
	}
	bpf_map_delete_elem(&nss_configs, &key);
	return ssl_forget(ctx, fd);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
