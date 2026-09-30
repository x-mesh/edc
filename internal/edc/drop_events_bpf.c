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

// 이 객체는 trace drop만 불러온다. kfree_skb tracepoint의 필드는 kernel마다 달라서(reason은 5.17, rx_sk는 그 뒤에 생겼다),
// 모든 trace가 불러오는 capture_events_bpf.c에 넣지 않는다.

#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
#define BPF_MAP_TYPE_RINGBUF 27
#define ETH_P_IP 0x0800
#define ETH_P_IPV6 0x86DD
#define IPPROTO_TCP 6
#define IPPROTO_UDP 17
#define TCP_TIME_WAIT 6
#define TCP_NEW_SYN_RECV 12
// DROP_REASON_MAX는 이유별 counter의 크기다. 6.17의 enum skb_drop_reason은 127개이고, 넘는 값은 마지막 칸에 센다.
#define DROP_REASON_MAX 512
#define NSEC_PER_SEC 1000000000ULL

// drop_record는 버려진 패킷 하나다. location은 kfree_skb를 부른 kernel 코드 주소이고, sock_ino는 패킷에 local socket이
// 있을 때 그 socket의 inode다. 사용자 공간은 이 inode로 process를 찾는다.
struct drop_record {
	__u64 timestamp_ns;
	__u64 location;
	__u64 sock_ino;
	__u32 reason;
	__u32 len;
	__u16 protocol;
	__u8 l4;
	__u8 family;
	__u16 sport;
	__u16 dport;
	__u8 saddr[16];
	__u8 daddr[16];
};

// ringbuf에만 쓰는 구조체는 BTF에 남지 않는다. bpf2go -type이 Go 구조체를 만들어 offset을 테스트하도록 남긴다.
const struct drop_record *unused_drop_record __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 22);
} events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} lost_events SEC(".maps");

// drop_counts는 이유별로 버려진 패킷 수다. event를 표본으로 줄여도 합계는 정확하다.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, DROP_REASON_MAX);
	__type(key, __u32);
	__type(value, __u64);
} drop_counts SEC(".maps");

// drop_windows는 CPU마다 지금 1초 창에서 보낸 event 수다. 공격처럼 버림이 폭증하면 event만 줄이고 합계는 계속 센다.
struct drop_window {
	__u64 start_ns;
	__u64 sent;
	__u64 suppressed;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct drop_window);
} drop_windows SEC(".maps");

// event_limit는 CPU마다 1초에 보내는 event의 상한이다.
volatile const __u32 event_limit = 1000;
// reason_filter는 --reason으로 고른 이유의 bitmap이다. filter_reasons가 0이면 모든 이유를 보낸다.
volatile const __u8 filter_reasons = 0;
volatile const __u64 reason_filter[DROP_REASON_MAX / 64] = {};

enum skb_drop_reason {
	SKB_DROP_REASON_NOT_SPECIFIED = 2,
};

struct trace_entry {
	__u16 type;
	__u8 flags;
	__u8 preempt_count;
	__s32 pid;
};

// kfree_skb tracepoint의 필드 위치는 kernel마다 다르다. 6.17은 skbaddr, location, rx_sk, protocol, reason 순서이고,
// 이전 kernel에는 rx_sk나 reason이 없다. CO-RE가 필드를 이름으로 찾아 위치를 고친다.
struct trace_event_raw_kfree_skb {
	struct trace_entry ent;
	void *skbaddr;
	void *location;
	void *rx_sk;
	__u16 protocol;
	enum skb_drop_reason reason;
} __attribute__((preserve_access_index));

struct inode {
	unsigned long i_ino;
};

struct file {
	struct inode *f_inode;
};

struct socket {
	struct file *file;
};

struct sock_common {
	unsigned char skc_state;
};

struct sock {
	struct sock_common __sk_common;
	struct socket *sk_socket;
};

struct sk_buff {
	struct sock *sk;
	unsigned int len;
	unsigned char *head;
	__u16 transport_header;
	__u16 network_header;
};

struct iphdr {
	__u8 ihl_version;
	__u8 tos;
	__be16 tot_len;
	__be16 id;
	__be16 frag_off;
	__u8 ttl;
	__u8 protocol;
	__u16 check;
	__be32 saddr;
	__be32 daddr;
};

struct ipv6hdr {
	__u8 priority_version;
	__u8 flow_lbl[3];
	__be16 payload_len;
	__u8 nexthdr;
	__u8 hop_limit;
	__u8 saddr[16];
	__u8 daddr[16];
};

// sock_inode는 full socket의 inode다. TIME_WAIT와 SYN_RECV의 socket은 크기가 작은 다른 구조체라 sk_socket을 읽지 않는다.
static __always_inline __u64 sock_inode(struct sock *sk) {
	if (!sk) {
		return 0;
	}
	unsigned char state = BPF_CORE_READ(sk, __sk_common.skc_state);
	if (state == TCP_TIME_WAIT || state == TCP_NEW_SYN_RECV) {
		return 0;
	}
	return BPF_CORE_READ(sk, sk_socket, file, f_inode, i_ino);
}

// fill_addresses는 IPv4나 IPv6 header와 TCP나 UDP port를 읽는다. header 위치가 정해지지 않은 패킷은 주소 없이 둔다.
static __always_inline void fill_addresses(struct drop_record *record, struct sk_buff *skb) {
	unsigned char *head = BPF_CORE_READ(skb, head);
	__u16 network = BPF_CORE_READ(skb, network_header);
	__u16 transport = BPF_CORE_READ(skb, transport_header);
	if (!head || network == (__u16)~0U) {
		return;
	}
	__u16 l4offset = 0;
	if (record->protocol == ETH_P_IP) {
		struct iphdr ip = {};
		if (bpf_probe_read_kernel(&ip, sizeof(ip), head + network) || (ip.ihl_version >> 4) != 4) {
			return;
		}
		record->family = 4;
		record->l4 = ip.protocol;
		__builtin_memcpy(record->saddr, &ip.saddr, 4);
		__builtin_memcpy(record->daddr, &ip.daddr, 4);
		l4offset = network + (ip.ihl_version & 0x0f) * 4;
	} else if (record->protocol == ETH_P_IPV6) {
		struct ipv6hdr ip = {};
		if (bpf_probe_read_kernel(&ip, sizeof(ip), head + network) || (ip.priority_version >> 4) != 6) {
			return;
		}
		record->family = 6;
		record->l4 = ip.nexthdr;
		__builtin_memcpy(record->saddr, ip.saddr, 16);
		__builtin_memcpy(record->daddr, ip.daddr, 16);
		l4offset = network + sizeof(ip);
	} else {
		return;
	}
	if (record->l4 != IPPROTO_TCP && record->l4 != IPPROTO_UDP) {
		return;
	}
	// transport header가 정해져 있으면 그 위치를 쓴다. IPv6 확장 header가 있어도 맞는 위치다.
	if (transport != (__u16)~0U && transport > network) {
		l4offset = transport;
	}
	__be16 ports[2] = {};
	if (bpf_probe_read_kernel(ports, sizeof(ports), head + l4offset)) {
		return;
	}
	record->sport = __builtin_bswap16(ports[0]);
	record->dport = __builtin_bswap16(ports[1]);
}

static __always_inline int reason_selected(__u32 reason) {
	if (!filter_reasons) {
		return 1;
	}
	if (reason >= DROP_REASON_MAX) {
		return 0;
	}
	return (reason_filter[reason / 64] >> (reason % 64)) & 1;
}

// event_allowed는 CPU마다 1초 창에서 보낸 event 수를 세고, 상한을 넘으면 0을 돌려준다.
static __always_inline int event_allowed(__u64 now) {
	__u32 zero = 0;
	struct drop_window *window = bpf_map_lookup_elem(&drop_windows, &zero);
	if (!window) {
		return 0;
	}
	if (now - window->start_ns >= NSEC_PER_SEC) {
		window->start_ns = now;
		window->sent = 0;
	}
	if (window->sent >= event_limit) {
		window->suppressed++;
		return 0;
	}
	window->sent++;
	return 1;
}

SEC("tracepoint/skb/kfree_skb")
int kfree_skb(struct trace_event_raw_kfree_skb *ctx) {
	__u32 reason = 0;
	if (bpf_core_field_exists(ctx->reason)) {
		reason = ctx->reason;
	}
	__u32 slot = reason < DROP_REASON_MAX ? reason : DROP_REASON_MAX - 1;
	__u64 *count = bpf_map_lookup_elem(&drop_counts, &slot);
	if (count) {
		*count += 1;
	}
	if (!reason_selected(reason)) {
		return 0;
	}
	__u64 now = bpf_ktime_get_ns();
	if (!event_allowed(now)) {
		return 0;
	}
	struct drop_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
	if (!record) {
		__u32 zero = 0;
		__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
		if (lost) {
			__sync_fetch_and_add(lost, 1);
		}
		return 0;
	}
	// ringbuf slot은 앞 레코드의 값이 남아 있으므로 지운다.
	__builtin_memset(record, 0, sizeof(*record));
	struct sk_buff *skb = (struct sk_buff *)ctx->skbaddr;
	record->timestamp_ns = now;
	record->location = (__u64)ctx->location;
	record->reason = reason;
	record->protocol = ctx->protocol;
	record->len = BPF_CORE_READ(skb, len);
	// 받은 패킷은 rx_sk가 받는 socket이다. 보내는 패킷과 rx_sk가 없는 kernel은 skb->sk를 쓴다.
	struct sock *sk = 0;
	if (bpf_core_field_exists(ctx->rx_sk)) {
		sk = (struct sock *)ctx->rx_sk;
	}
	if (!sk) {
		sk = BPF_CORE_READ(skb, sk);
	}
	record->sock_ino = sock_inode(sk);
	fill_addresses(record, skb);
	bpf_ringbuf_submit(record, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
