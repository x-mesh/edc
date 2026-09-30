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

// 이 객체는 trace socket만 불러온다. capture_events_bpf.c에 넣으면 모든 trace가 unix_* hook이 있는 kernel에서만
// 불러와진다. unix가 module로 build된 kernel에서는 이 함수들이 vmlinux BTF에 없다.

#define BPF_ANY 0
#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
#define BPF_MAP_TYPE_LRU_HASH 9
#define BPF_MAP_TYPE_RINGBUF 27
#define MSG_PEEK 2
#define EAGAIN 11

#define SOCKET_CONNECT 1
#define SOCKET_SEND 2
#define SOCKET_RECV 3
#define SOCKET_CLOSE 4
#define SOCKET_ACCEPT 5
#define SOCKET_CLIENT 0
#define SOCKET_SERVER 1
// SOCKET_UNREADABLE는 payload를 읽지 못한 호출이다. 사용자 공간은 조각을 기다리지 않고 바로 event를 낸다.
#define SOCKET_UNREADABLE 1
#define SOCKET_PAYLOAD_SIZE 16384
// 한 번의 읽기나 쓰기에서 도는 반복의 상한이다. 1MiB를 조각 64개로 넘기고, writev 버퍼를 16개까지 넘긴다. bpf_loop는
// 5.17에 생겨서 쓰지 않는다.
#define SOCKET_MAX_SEGMENTS 16
#define SOCKET_MAX_STEPS 80
#define SOCKET_PATH_SIZE 108

// socket_record는 호출 하나의 event와 payload 조각이다. 한 호출의 조각은 timestamp_ns가 같고, offset은 조각이 호출의
// data 안에서 시작하는 위치다. result는 주고받은 byte 수나 음수 errno다. wait_ns는 accept event에서 connect부터 accept까지
// backlog에서 기다린 시간이고, 모르면 0이다.
struct socket_record {
	__u64 timestamp_ns;
	__u64 skaddr;
	__u64 peer_skaddr;
	__u64 cgroup_id;
	__s64 result;
	__u64 wait_ns;
	__u32 event_type;
	__u32 pid;
	__u32 peer_pid;
	__u32 len;
	__u32 offset;
	__u8 side;
	__u8 flags;
	__u8 pad[2];
	char comm[16];
	__u8 payload[SOCKET_PAYLOAD_SIZE];
};

// ringbuf에만 쓰는 구조체는 BTF에 남지 않는다. bpf2go -type이 Go 구조체를 만들어 offset을 테스트하도록 남긴다.
const struct socket_record *unused_socket_record __attribute__((unused));

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

// payload_limit는 호출마다 넘길 payload byte 수다. 0이면 byte 수만 넘긴다.
volatile const __u32 payload_limit = 0;
// target_path는 실패한 connect를 고를 때 쓴다. 실패하면 상대 socket이 없어서 inode로 비교할 수 없다.
volatile const char target_path[SOCKET_PATH_SIZE] = {};

// socket_target은 대상 socket 파일의 inode다. dev는 kernel 형식(major << 20 | minor)이다. 사용자 공간이 경로를 다시
// stat해서 socket 파일이 새로 만들어지면 새 inode를 더한다. 이전 inode는 남겨서 이미 맺은 연결을 계속 본다.
struct socket_target {
	__u64 ino;
	__u32 dev;
	__u32 pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__type(key, struct socket_target);
	__type(value, __u8);
} socket_targets SEC(".maps");

struct super_block {
	__u32 s_dev;
};

struct inode {
	unsigned long i_ino;
	struct super_block *i_sb;
};

struct dentry {
	struct inode *d_inode;
};

struct path {
	struct dentry *dentry;
};

struct upid {
	int nr;
};

struct pid {
	struct upid numbers[1];
};

struct sock {
	struct pid *sk_peer_pid;
};

struct unix_sock {
	struct path path;
	struct sock *peer;
};

struct socket {
	struct sock *sk;
};

struct sockaddr_un {
	__u16 sun_family;
	char sun_path[SOCKET_PATH_SIZE];
};

struct task_struct {
	struct task_struct *group_leader;
	char comm[16];
};

struct iovec {
	void *iov_base;
	__u64 iov_len;
};

enum iter_type {
	ITER_UBUF,
	ITER_IOVEC,
};

// iter_type은 5.14, ubuf는 6.0, __iov는 6.4에 생겼다. 없는 필드는 CO-RE 존재 확인으로 감싸고, 그 kernel에서는 payload
// 없이 byte 수만 넘긴다.
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

// socket_matches는 dentry가 대상 socket 파일이면 1이다.
static __always_inline int socket_matches(struct dentry *dentry) {
	if (!dentry) {
		return 0;
	}
	struct inode *inode = BPF_CORE_READ(dentry, d_inode);
	if (!inode) {
		return 0;
	}
	struct socket_target key = {};
	key.ino = BPF_CORE_READ(inode, i_ino);
	key.dev = BPF_CORE_READ(inode, i_sb, s_dev);
	return bpf_map_lookup_elem(&socket_targets, &key) != 0;
}

// socket_call은 대상 socket의 호출 하나다. buffer, limit, iov, nr_segs는 시작할 때 본 사용자 버퍼다. recvmsg가 끝나면
// msg_iter가 읽은 만큼 앞으로 가 있으므로 시작할 때 기록한다.
struct socket_call {
	__u64 skaddr;
	__u64 peer_skaddr;
	__u64 buffer;
	__u64 limit;
	__u64 iov;
	__u64 nr_segs;
	__u32 peer_pid;
	__u32 event_type;
	__u8 side;
};

// socket_sides는 대상 socket으로 확인한 socket의 쪽이다. 서버 쪽 socket이 닫히면 kernel이 그 socket의 경로를 비워서,
// 클라이언트 쪽은 그 뒤의 EOF와 close를 경로로 고를 수 없다. 그래서 한 번 확인한 socket을 기억하고 close할 때 지운다.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u64);
	__type(value, __u8);
} socket_sides SEC(".maps");

// socket_servers는 클라이언트 socket마다 서버 쪽에서 그 연결을 처리하는 process다. SO_PEERCRED는 listen()한 process라서
// socket activation(systemd)이나 accept한 뒤 fork하는 서버에서는 실제 상대가 아니다. accept와 서버 쪽 호출마다 기록한다.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u64);
	__type(value, __u32);
} socket_servers SEC(".maps");

// socket_connects는 클라이언트 socket이 connect를 시작한 시각이다. accept event가 backlog에서 기다린 시간을 잰다. 기다리던
// 서버는 connect가 끝나기 전에 accept를 마칠 수 있으므로 connect가 시작할 때 기록한다.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u64);
	__type(value, __u64);
} socket_connects SEC(".maps");

// socket_side는 sk가 대상 socket에 속하면 call을 채우고 1을 돌려준다. accept로 생긴 서버 쪽 socket은 listener의 경로를
// 물려받고, 클라이언트 쪽 socket은 상대(peer)가 그 경로를 가진다.
static __always_inline int socket_side(struct sock *sk, struct socket_call *call) {
	if (!sk) {
		return 0;
	}
	__u64 skaddr = (__u64)sk;
	struct unix_sock *unix = (struct unix_sock *)sk;
	struct sock *peer = BPF_CORE_READ(unix, peer);
	__u8 *known = bpf_map_lookup_elem(&socket_sides, &skaddr);
	if (known) {
		call->side = *known;
	} else if (socket_matches(BPF_CORE_READ(unix, path.dentry))) {
		call->side = SOCKET_SERVER;
	} else if (peer && socket_matches(BPF_CORE_READ((struct unix_sock *)peer, path.dentry))) {
		call->side = SOCKET_CLIENT;
	} else {
		return 0;
	}
	if (!known) {
		bpf_map_update_elem(&socket_sides, &skaddr, &call->side, BPF_ANY);
	}
	call->skaddr = skaddr;
	call->peer_skaddr = (__u64)peer;
	// sk_peer_pid는 SO_PEERCRED 값이다. 서버 쪽에서는 connect한 process, 클라이언트 쪽에서는 listen한 process다.
	call->peer_pid = BPF_CORE_READ(sk, sk_peer_pid, numbers[0].nr);
	return 1;
}

// socket_note_server는 서버 쪽 호출의 process를 그 연결의 클라이언트 상대로 기록한다. accept, send, recv에서만 부른다.
// close는 부르지 않는다. journald처럼 helper process((sd-close))가 닫는 서버에서는 그 helper가 상대로 남기 때문이다.
static __always_inline void socket_note_server(struct socket_call *call) {
	if (call->side != SOCKET_SERVER || !call->peer_skaddr) {
		return;
	}
	__u64 client = call->peer_skaddr;
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	__u32 *server = bpf_map_lookup_elem(&socket_servers, &client);
	if (!server || *server != tgid) {
		bpf_map_update_elem(&socket_servers, &client, &tgid, BPF_ANY);
	}
}

// socket_user_buffer는 사용자 버퍼의 시작과 첫 조각의 길이다. writev와 readv의 다음 버퍼는 socket_iov로 넘긴다.
static __always_inline void *socket_user_buffer(struct msghdr *msg, __u64 *limit) {
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

static __always_inline const struct iovec *socket_iov(struct msghdr *msg, __u64 *nr_segs) {
	if (!bpf_core_field_exists(msg->msg_iter.iter_type) || !bpf_core_field_exists(msg->msg_iter.__iov) ||
	    !bpf_core_field_exists(msg->msg_iter.nr_segs) || !bpf_core_enum_value_exists(enum iter_type, ITER_IOVEC)) {
		return 0;
	}
	if (BPF_CORE_READ(msg, msg_iter.iter_type) != bpf_core_enum_value(enum iter_type, ITER_IOVEC)) {
		return 0;
	}
	*nr_segs = BPF_CORE_READ(msg, msg_iter.nr_segs);
	return BPF_CORE_READ(msg, msg_iter.__iov);
}

// record는 BPF stack(512바이트)보다 커서 CPU별 scratch에서 만든다.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct socket_record);
} socket_scratch SEC(".maps");

// socket_cursor는 payload 반복의 위치다. 반복 상태를 register에 두면 verifier가 반복마다 값의 범위를 따로 추적해 경로를
// 합치지 못한다. map에서 다시 읽은 값은 범위를 모르는 값이라 경로가 합쳐진다.
struct socket_cursor {
	__u64 pointer;
	__u64 left;
	__u64 remaining;
	__u64 offset;
	__u64 segment;
	__u64 iov;
	__u64 nr_segs;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct socket_cursor);
} socket_cursors SEC(".maps");

// socket_calls는 thread마다 진행 중인 sendmsg나 recvmsg다. 끝날 때 실제로 주고받은 byte 수만큼 payload를 읽는다.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct socket_call);
} socket_calls SEC(".maps");

static __always_inline void count_lost(void) {
	__u32 zero = 0;
	__u64 *lost = bpf_map_lookup_elem(&lost_events, &zero);
	if (lost) {
		__sync_fetch_and_add(lost, 1);
	}
}

static __always_inline struct socket_record *socket_record_start(struct socket_call *call, __u32 type, __s64 result) {
	__u32 zero = 0;
	struct socket_record *record = bpf_map_lookup_elem(&socket_scratch, &zero);
	if (!record) {
		return 0;
	}
	record->timestamp_ns = bpf_ktime_get_ns();
	record->skaddr = call->skaddr;
	record->peer_skaddr = call->peer_skaddr;
	record->cgroup_id = bpf_get_current_cgroup_id();
	record->result = result;
	record->wait_ns = 0;
	record->event_type = type;
	record->pid = bpf_get_current_pid_tgid() >> 32;
	record->peer_pid = call->peer_pid;
	// 클라이언트는 서버보다 먼저 recv에 들어가 기다리므로, 상대는 호출이 시작할 때가 아니라 레코드를 낼 때 찾는다.
	if (call->side == SOCKET_CLIENT) {
		__u32 *server = bpf_map_lookup_elem(&socket_servers, &call->skaddr);
		if (server) {
			record->peer_pid = *server;
		}
	}
	record->len = 0;
	record->offset = 0;
	record->side = call->side;
	record->flags = 0;
	// 문자열 읽기는 NUL까지만 쓴다. scratch는 앞 레코드의 값이 남아 있으므로 먼저 지운다. process 이름은 thread가 아니라
	// thread group leader의 이름이다.
	__builtin_memset(record->comm, 0, sizeof(record->comm));
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	BPF_CORE_READ_STR_INTO(&record->comm, task, group_leader, comm);
	return record;
}

// socket_output은 payload 없이 머리만 넘긴다. 크기는 상수여야 한다. map에서 다시 읽은 len은 verifier가 범위를 모른다.
static __always_inline void socket_output(struct socket_record *record) {
	record->len = 0;
	if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct socket_record, payload), 0)) {
		count_lost();
	}
}

// socket_emit은 호출 하나를 레코드로 넘긴다. payload를 읽지 않으면 머리만 넘기고, 읽으면 16KiB 조각으로 나눈다.
static __always_inline void socket_emit(struct socket_call *call, __s64 result) {
	struct socket_record *record = socket_record_start(call, call->event_type, result);
	if (!record) {
		return;
	}
	if (!payload_limit || result <= 0) {
		socket_output(record);
		return;
	}
	__u32 zero = 0;
	struct socket_cursor *cursor = bpf_map_lookup_elem(&socket_cursors, &zero);
	if (!cursor || !call->buffer) {
		record->flags = SOCKET_UNREADABLE;
		socket_output(record);
		return;
	}
	cursor->pointer = call->buffer;
	cursor->left = call->limit;
	cursor->remaining = (__u64)result < payload_limit ? (__u64)result : payload_limit;
	cursor->offset = 0;
	cursor->segment = 0;
	cursor->iov = call->iov;
	cursor->nr_segs = call->nr_segs;
	for (int step = 0; step < SOCKET_MAX_STEPS; step++) {
		if (!cursor->remaining) {
			break;
		}
		if (!cursor->left) {
			__u64 segment = cursor->segment + 1;
			if (!cursor->iov || segment >= cursor->nr_segs || segment >= SOCKET_MAX_SEGMENTS) {
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
		if (len > SOCKET_PAYLOAD_SIZE) {
			len = SOCKET_PAYLOAD_SIZE;
		}
		if (bpf_probe_read_user(record->payload, len, (void *)cursor->pointer)) {
			break;
		}
		record->len = len;
		record->offset = cursor->offset;
		if (bpf_ringbuf_output(&events, record, __builtin_offsetof(struct socket_record, payload) + len, 0)) {
			count_lost();
			return;
		}
		cursor->pointer += len;
		cursor->left -= len;
		cursor->remaining -= len;
		cursor->offset += len;
	}
	// 첫 조각도 읽지 못했으면 머리만 넘긴다. 그래야 호출 자체는 event로 남는다.
	if (!cursor->offset) {
		record->flags = SOCKET_UNREADABLE;
		socket_output(record);
	}
}

static __always_inline int socket_call_start(struct socket *sock, struct msghdr *msg, __u32 type) {
	struct socket_call call = {.event_type = type, .nr_segs = 1};
	if (!socket_side(BPF_CORE_READ(sock, sk), &call)) {
		return 0;
	}
	socket_note_server(&call);
	if (payload_limit) {
		call.buffer = (__u64)socket_user_buffer(msg, &call.limit);
		call.iov = (__u64)socket_iov(msg, &call.nr_segs);
	}
	__u64 key = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&socket_calls, &key, &call, BPF_ANY);
	return 0;
}

static __always_inline int socket_call_end(__u32 type, __s64 result) {
	__u64 key = bpf_get_current_pid_tgid();
	struct socket_call *stored = bpf_map_lookup_elem(&socket_calls, &key);
	if (!stored) {
		return 0;
	}
	struct socket_call call = *stored;
	bpf_map_delete_elem(&socket_calls, &key);
	// non-blocking socket은 보낼 자리나 읽을 data가 없으면 EAGAIN을 돌려준다. event loop는 이 값을 볼 때까지 읽으므로
	// 오류가 아니다.
	if (call.event_type != type || result == -EAGAIN) {
		return 0;
	}
	socket_emit(&call, result);
	return 0;
}

SEC("fentry/unix_stream_sendmsg")
int unix_stream_sendmsg_entry(__u64 *ctx) {
	return socket_call_start((struct socket *)ctx[0], (struct msghdr *)ctx[1], SOCKET_SEND);
}

SEC("fexit/unix_stream_sendmsg")
int unix_stream_sendmsg_exit(__u64 *ctx) {
	return socket_call_end(SOCKET_SEND, (__s64)(int)ctx[3]);
}

SEC("fentry/unix_stream_recvmsg")
int unix_stream_recvmsg_entry(__u64 *ctx) {
	// MSG_PEEK로 읽은 data는 다음 recv가 다시 읽는다. 같은 data를 두 번 내지 않는다.
	if ((int)ctx[3] & MSG_PEEK) {
		return 0;
	}
	return socket_call_start((struct socket *)ctx[0], (struct msghdr *)ctx[1], SOCKET_RECV);
}

SEC("fexit/unix_stream_recvmsg")
int unix_stream_recvmsg_exit(__u64 *ctx) {
	return socket_call_end(SOCKET_RECV, (__s64)(int)ctx[4]);
}

// socket_path_is_target은 connect에 넘긴 경로가 대상 경로와 같으면 1이다. 상대 경로나 다른 이름(symlink)으로 연결한
// 실패는 고르지 못한다.
static __always_inline int socket_path_is_target(struct sockaddr_un *address) {
	char path[SOCKET_PATH_SIZE] = {};
	if (bpf_probe_read_kernel(path, sizeof(path), address->sun_path)) {
		return 0;
	}
	for (int i = 0; i < SOCKET_PATH_SIZE; i++) {
		if (path[i] != target_path[i]) {
			return 0;
		}
		if (!path[i]) {
			return 1;
		}
	}
	return 1;
}

SEC("fentry/unix_stream_connect")
int unix_stream_connect_entry(__u64 *ctx) {
	__u64 skaddr = (__u64)BPF_CORE_READ((struct socket *)ctx[0], sk);
	__u64 now = bpf_ktime_get_ns();
	if (skaddr) {
		bpf_map_update_elem(&socket_connects, &skaddr, &now, BPF_ANY);
	}
	return 0;
}

SEC("fexit/unix_stream_connect")
int unix_stream_connect_exit(__u64 *ctx) {
	struct socket *sock = (struct socket *)ctx[0];
	int result = (int)ctx[4];
	struct socket_call call = {.event_type = SOCKET_CONNECT};
	__u64 skaddr = (__u64)BPF_CORE_READ(sock, sk);
	if (result == 0) {
		if (!socket_side(BPF_CORE_READ(sock, sk), &call)) {
			// 시작할 때는 대상인지 몰라 모든 connect의 시각을 두었다. 대상이 아닌 연결은 accept가 찾지 않는다.
			bpf_map_delete_elem(&socket_connects, &skaddr);
			return 0;
		}
	} else {
		bpf_map_delete_elem(&socket_connects, &skaddr);
		if (!target_path[0] || !socket_path_is_target((struct sockaddr_un *)ctx[1])) {
			return 0;
		}
		call.skaddr = skaddr;
		call.side = SOCKET_CLIENT;
	}
	struct socket_record *record = socket_record_start(&call, SOCKET_CONNECT, result);
	if (record) {
		socket_output(record);
	}
	return 0;
}

// unix_accept의 인자는 6.10에서 (sock, newsock, flags, kern)에서 (sock, newsock, arg)로 바뀌어 반환값의 위치가 다르다. 두
// 형태에 공통인 newsock만 읽고, accept가 성공했는지는 newsock->sk가 채워졌는지로 본다.
SEC("fexit/unix_accept")
int unix_accept_exit(__u64 *ctx) {
	struct socket *newsock = (struct socket *)ctx[1];
	struct socket_call call = {.event_type = SOCKET_ACCEPT};
	if (!socket_side(BPF_CORE_READ(newsock, sk), &call) || call.side != SOCKET_SERVER) {
		return 0;
	}
	socket_note_server(&call);
	struct socket_record *record = socket_record_start(&call, SOCKET_ACCEPT, 0);
	if (!record) {
		return 0;
	}
	__u64 client = call.peer_skaddr;
	__u64 *connected = bpf_map_lookup_elem(&socket_connects, &client);
	if (connected) {
		if (*connected < record->timestamp_ns) {
			record->wait_ns = record->timestamp_ns - *connected;
		}
		bpf_map_delete_elem(&socket_connects, &client);
	}
	socket_output(record);
	return 0;
}

// unix_release가 시작할 때는 socket의 경로와 상대가 아직 남아 있다.
SEC("fentry/unix_release")
int unix_release_entry(__u64 *ctx) {
	struct socket *sock = (struct socket *)ctx[0];
	struct sock *sk = BPF_CORE_READ(sock, sk);
	__u64 skaddr = (__u64)sk;
	struct socket_call call = {.event_type = SOCKET_CLOSE};
	if (socket_side(sk, &call)) {
		struct socket_record *record = socket_record_start(&call, SOCKET_CLOSE, 0);
		if (record) {
			socket_output(record);
		}
	}
	// 대상인지와 관계없이 지운다. 기억한 쪽이 LRU에서 밀려나 대상인지 모르게 되어도, 같은 주소를 받은 새 socket이 남은 서버나
	// connect 시각을 물려받지 않게 한다.
	bpf_map_delete_elem(&socket_sides, &skaddr);
	bpf_map_delete_elem(&socket_servers, &skaddr);
	bpf_map_delete_elem(&socket_connects, &skaddr);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
