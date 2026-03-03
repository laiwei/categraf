// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>

#define AF_INET 2
#define AF_INET6 10

// 事件类型
enum event_type {
	EVENT_PROCESS_START = 1,
	EVENT_PROCESS_EXIT = 2,
	EVENT_CONNECTION_OPEN = 3,
	EVENT_CONNECTION_CLOSE = 4,
	EVENT_TCP_RETRANSMIT = 5,
	EVENT_LISTEN_OPEN = 6,
	EVENT_LISTEN_CLOSE = 7,
	EVENT_CONNECTION_ACCEPTED = 8,  // 服务端 accept 的被动连接（与 OPEN 区分方向）
};

// 与 Go 端 Event 结构对应
struct event {
	__u64 timestamp;
	__u32 type;
	__u32 pid;
	__u64 fd;
	__u32 src_addr[4];  // IPv4 只用第一个元素
	__u32 dst_addr[4];
	__u16 src_port;
	__u16 dst_port;
	__u16 family;       // AF_INET or AF_INET6
	__u64 bytes_sent;
	__u64 bytes_received;
};

// Perf event buffer
struct {
	__uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, sizeof(__u32));
} events SEC(".maps");

// 存储进程的活跃连接信息（用于关闭时找回地址）
struct conn_key {
	__u64 sk_ptr;  // sock 指针作为唯一 key（不含 PID，避免 softirq 上下文中 PID 错误）
};

struct conn_info {
	__u32 pid;         // 在 connect 时保存，close 时取回
	__u32 src_addr[4];
	__u32 dst_addr[4];
	__u16 src_port;
	__u16 dst_port;
	__u16 family;
	__u64 bytes_sent;
	__u64 bytes_received;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, struct conn_key);
	__type(value, struct conn_info);
} active_connections SEC(".maps");

// 辅助函数：发送事件
static __always_inline void send_event(void *ctx, struct event *e) {
	e->timestamp = bpf_ktime_get_ns();
	bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, e, sizeof(*e));
}

// 从 sock 提取地址信息
static __always_inline int fill_addr_from_sock(struct sock *sk, struct event *e) {
	__u16 family = BPF_CORE_READ(sk, __sk_common.skc_family);
	e->family = family;

	if (family == AF_INET) {
		e->src_addr[0] = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		e->dst_addr[0] = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		e->src_port = BPF_CORE_READ(sk, __sk_common.skc_num);  // 已是主机字节序
		e->dst_port = BPF_CORE_READ(sk, __sk_common.skc_dport); // 网络字节序，Go 端 ntohs
	} else if (family == AF_INET6) {
		BPF_CORE_READ_INTO(&e->src_addr, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr32);
		BPF_CORE_READ_INTO(&e->dst_addr, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr32);
		e->src_port = BPF_CORE_READ(sk, __sk_common.skc_num);  // 已是主机字节序
		e->dst_port = BPF_CORE_READ(sk, __sk_common.skc_dport); // 网络字节序，Go 端 ntohs
	}

	return 0;
}

// Hook tcp_connect - 连接建立
SEC("kprobe/tcp_connect")
int BPF_KPROBE(tcp_connect, struct sock *sk) {
	struct event e = {};
	e.type = EVENT_CONNECTION_OPEN;
	e.pid = bpf_get_current_pid_tgid() >> 32;

	fill_addr_from_sock(sk, &e);

	// 保存连接信息（以 sock 指针为 key，存储 PID 供 close 时取回）
	struct conn_key key = {
		.sk_ptr = (__u64)sk,
	};
	struct conn_info info = {};
	info.pid = e.pid;
	__builtin_memcpy(info.src_addr, e.src_addr, sizeof(info.src_addr));
	__builtin_memcpy(info.dst_addr, e.dst_addr, sizeof(info.dst_addr));
	info.src_port = e.src_port;
	info.dst_port = e.dst_port;
	info.family = e.family;
	bpf_map_update_elem(&active_connections, &key, &info, BPF_ANY);

	send_event(ctx, &e);
	return 0;
}

// Hook tcp_set_state - 连接状态变化
// 捕获两种状态转换：
//   TCP_ESTABLISHED (1): 服务端 accept 完成的被动连接（tcp_connect 主动发起的连接已有 kprobe 覆盖）
//   TCP_CLOSE (7):       连接关闭
SEC("kprobe/tcp_set_state")
int BPF_KPROBE(tcp_set_state, struct sock *sk, int state) {
	struct conn_key key = {
		.sk_ptr = (__u64)sk,
	};

	if (state == 1) {
		// TCP_ESTABLISHED — 仅处理被动接受的连接（服务端）。
		// tcp_connect 发起的主动连接已经在 kprobe/tcp_connect 中写入了 active_connections，
		// 如果 map 中已有此 sk_ptr 则说明是主动连接，跳过以避免重复 Open 事件。
		struct conn_info *existing = bpf_map_lookup_elem(&active_connections, &key);
		if (existing)
			return 0;  // 主动连接，已由 tcp_connect 处理

		// 被动连接（accept）：创建 Accepted 事件（与主动 Open 区分方向）
		struct event e = {};
		e.type = EVENT_CONNECTION_ACCEPTED;
		e.pid = bpf_get_current_pid_tgid() >> 32;

		fill_addr_from_sock(sk, &e);

		// 保存到 active_connections，供 close 时取回 PID 和地址
		struct conn_info info = {};
		info.pid = e.pid;
		__builtin_memcpy(info.src_addr, e.src_addr, sizeof(info.src_addr));
		__builtin_memcpy(info.dst_addr, e.dst_addr, sizeof(info.dst_addr));
		info.src_port = e.src_port;
		info.dst_port = e.dst_port;
		info.family = e.family;
		bpf_map_update_elem(&active_connections, &key, &info, BPF_ANY);

		send_event(ctx, &e);
		return 0;
	}

	if (state == 7) {
		// TCP_CLOSE — 连接关闭
		struct event e = {};
		e.type = EVENT_CONNECTION_CLOSE;

		fill_addr_from_sock(sk, &e);

		// 查找并删除活跃连接（从 map 中取回 connect/accept 时保存的 PID）
		struct conn_info *info = bpf_map_lookup_elem(&active_connections, &key);
		if (info) {
			e.pid = info->pid;
			e.bytes_sent = info->bytes_sent;
			e.bytes_received = info->bytes_received;
			bpf_map_delete_elem(&active_connections, &key);
		} else {
			// map 中找不到，fallback 到当前上下文 PID（可能不准确）
			e.pid = bpf_get_current_pid_tgid() >> 32;
		}

		send_event(ctx, &e);
		return 0;
	}

	return 0;
}

// Hook tcp_retransmit_skb - TCP重传
SEC("kprobe/tcp_retransmit_skb")
int BPF_KPROBE(tcp_retransmit_skb, struct sock *sk) {
	struct event e = {};
	e.type = EVENT_TCP_RETRANSMIT;
	e.pid = bpf_get_current_pid_tgid() >> 32;

	fill_addr_from_sock(sk, &e);
	send_event(ctx, &e);
	return 0;
}

// Hook inet_listen - 监听端口
SEC("kprobe/inet_listen")
int BPF_KPROBE(inet_listen, struct socket *sock, int backlog) {
	struct event e = {};
	e.type = EVENT_LISTEN_OPEN;
	e.pid = bpf_get_current_pid_tgid() >> 32;

	struct sock *sk = BPF_CORE_READ(sock, sk);
	if (sk) {
		e.family = BPF_CORE_READ(sk, __sk_common.skc_family);
		if (e.family == AF_INET) {
			e.src_addr[0] = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
			e.src_port = BPF_CORE_READ(sk, __sk_common.skc_num); // 已是主机字节序
		} else if (e.family == AF_INET6) {
			BPF_CORE_READ_INTO(&e.src_addr, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr32);
			e.src_port = BPF_CORE_READ(sk, __sk_common.skc_num); // 已是主机字节序
		}
	}

	send_event(ctx, &e);
	return 0;
}

// Hook fork/exec - 进程启动
SEC("tracepoint/sched/sched_process_fork")
int handle_fork(struct trace_event_raw_sched_process_fork *ctx) {
	struct event e = {};
	e.type = EVENT_PROCESS_START;
	e.pid = ctx->child_pid;

	send_event(ctx, &e);
	return 0;
}

// Hook exit - 进程退出
SEC("tracepoint/sched/sched_process_exit")
int handle_exit(struct trace_event_raw_sched_process_template *ctx) {
	struct event e = {};
	e.type = EVENT_PROCESS_EXIT;
	e.pid = bpf_get_current_pid_tgid() >> 32;

	send_event(ctx, &e);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
