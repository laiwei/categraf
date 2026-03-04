// SPDX-License-Identifier: GPL-2.0 OR BSD-3-Clause
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>

#define AF_INET 2
#define AF_INET6 10

// ─── Network Namespace 过滤 ────────────────────────────────
// OrbStack / 容器环境共享内核，kprobe 是全局 hook，会触发所有 VM/容器的事件。
// 通过比较 socket 所属的 network namespace inode 与 categraf 自身的 namespace，
// 在内核侧直接丢弃跨 namespace 的事件，避免 perf buffer 和 Go 端不必要的开销。
//
// config_map[0] = 目标 network namespace inode（由 Go 端在 eBPF 加载后写入）。
// 值为 0 表示禁用过滤（兼容无法获取 netns inode 的环境）。
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u32);
} config_map SEC(".maps");

// 从 sock 读取 network namespace inode
static __always_inline __u32 get_netns_from_sock(struct sock *sk) {
	// sock->__sk_common.skc_net -> net->ns.inum
	struct net *net = BPF_CORE_READ(sk, __sk_common.skc_net.net);
	if (!net)
		return 0;
	return BPF_CORE_READ(net, ns.inum);
}

// 从当前 task_struct 读取 network namespace inode（用于无 sock 的 hook）
static __always_inline __u32 get_netns_from_task(void) {
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	if (!task)
		return 0;
	// task->nsproxy->net_ns->ns.inum
	struct nsproxy *nsproxy = BPF_CORE_READ(task, nsproxy);
	if (!nsproxy)
		return 0;
	struct net *net = BPF_CORE_READ(nsproxy, net_ns);
	if (!net)
		return 0;
	return BPF_CORE_READ(net, ns.inum);
}

// 检查 netns inode 是否匹配 config_map 中的目标值。
// 返回 true 表示匹配（应处理），false 表示不匹配（应丢弃）。
static __always_inline bool match_netns(__u32 netns) {
	__u32 key = 0;
	__u32 *target = bpf_map_lookup_elem(&config_map, &key);
	if (!target || *target == 0)
		return true;  // 目标值为 0 或未设置 → 禁用过滤，全部放行
	return netns == *target;
}

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

// 与 Go 端 rawEvent 结构对应（修改时必须同步 event_parser.go）
struct event {
	__u64 timestamp;
	__u32 type;
	__u32 pid;
	__u64 fd;           // sock 指针作为唯一连接标识（替代不可用的 fd）
	__u32 src_addr[4];  // IPv4 只用第一个元素
	__u32 dst_addr[4];
	__u16 src_port;
	__u16 dst_port;
	__u16 family;       // AF_INET or AF_INET6
	__u16 _pad;         // 显式对齐填充（与 Go 端 Padding uint16 对应）
	__u64 bytes_sent;
	__u64 bytes_received;
	char comm[16];      // 进程名（bpf_get_current_comm），避免用户态竞态读取失败
	__u32 netns_inum;   // network namespace inode，Go 端用于二次过滤
	__u32 _pad2;        // 8 字节对齐填充
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
	__u16 _pad;        // 显式对齐填充
	__u64 bytes_sent;
	__u64 bytes_received;
	char comm[16];     // 进程名，从 tcp_connect 保存，供 close/retransmit 取回
	__u32 netns_inum;  // 连接所属 network namespace inode
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
	// Network namespace 过滤：丢弃非本 VM/容器的事件
	__u32 netns = get_netns_from_sock(sk);
	if (!match_netns(netns))
		return 0;

	struct event e = {};
	e.type = EVENT_CONNECTION_OPEN;
	e.pid = bpf_get_current_pid_tgid() >> 32;
	e.fd = (__u64)sk;
	e.netns_inum = netns;
	bpf_get_current_comm(e.comm, sizeof(e.comm));

	fill_addr_from_sock(sk, &e);

	// 保存连接信息（以 sock 指针为 key，存储 PID + comm 供 close 时取回）
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
	__builtin_memcpy(info.comm, e.comm, sizeof(info.comm));
	info.netns_inum = netns;
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
		struct conn_info *existing = bpf_map_lookup_elem(&active_connections, &key);
		if (existing)
			return 0;  // 主动连接，已由 tcp_connect 处理

		// Network namespace 过滤
		__u32 netns = get_netns_from_sock(sk);
		if (!match_netns(netns))
			return 0;

		struct event e = {};
		e.type = EVENT_CONNECTION_ACCEPTED;
		e.pid = bpf_get_current_pid_tgid() >> 32;
		e.fd = (__u64)sk;
		e.netns_inum = netns;
		bpf_get_current_comm(e.comm, sizeof(e.comm));

		fill_addr_from_sock(sk, &e);

		// 保存到 active_connections，供 close 时取回 PID 和地址
		struct conn_info info = {};
		info.pid = e.pid;
		__builtin_memcpy(info.src_addr, e.src_addr, sizeof(info.src_addr));
		__builtin_memcpy(info.dst_addr, e.dst_addr, sizeof(info.dst_addr));
		info.src_port = e.src_port;
		info.dst_port = e.dst_port;
		info.family = e.family;
		__builtin_memcpy(info.comm, e.comm, sizeof(info.comm));
		info.netns_inum = netns;
		bpf_map_update_elem(&active_connections, &key, &info, BPF_ANY);

		send_event(ctx, &e);
		return 0;
	}

	if (state == 7) {
		// TCP_CLOSE — 连接关闭
		// 对于 close 事件，先尝试从 active_connections 获取 netns；
		// 如果没有（未跟踪的连接），用 sock 读取。
		struct conn_info *info = bpf_map_lookup_elem(&active_connections, &key);
		__u32 netns = info ? info->netns_inum : get_netns_from_sock(sk);
		if (!match_netns(netns)) {
			if (info)
				bpf_map_delete_elem(&active_connections, &key);
			return 0;
		}

		struct event e = {};
		e.type = EVENT_CONNECTION_CLOSE;
		e.fd = key.sk_ptr;
		e.netns_inum = netns;

		fill_addr_from_sock(sk, &e);

		if (info) {
			e.pid = info->pid;
			e.bytes_sent = info->bytes_sent;
			e.bytes_received = info->bytes_received;
			__builtin_memcpy(e.comm, info->comm, sizeof(e.comm));
			bpf_map_delete_elem(&active_connections, &key);
		} else {
			e.pid = bpf_get_current_pid_tgid() >> 32;
		}

		send_event(ctx, &e);
		return 0;
	}

	return 0;
}

// Hook tcp_retransmit_skb - TCP重传
// 关键修复：tcp_retransmit_skb 通常在 softirq 上下文中被调用，
// bpf_get_current_pid_tgid() 返回 0 或无关进程的 PID。
// 因此必须从 active_connections map 中查找正确的 PID 和 comm。
SEC("kprobe/tcp_retransmit_skb")
int BPF_KPROBE(tcp_retransmit_skb, struct sock *sk) {
	struct conn_key key = {
		.sk_ptr = (__u64)sk,
	};

	// Network namespace 过滤：优先从 active_connections 获取，fallback 到 sock
	struct conn_info *_info = bpf_map_lookup_elem(&active_connections, &key);
	__u32 netns = _info ? _info->netns_inum : get_netns_from_sock(sk);
	if (!match_netns(netns))
		return 0;

	struct event e = {};
	e.type = EVENT_TCP_RETRANSMIT;
	e.fd = (__u64)sk;
	e.netns_inum = netns;

	// 从 active_connections 取回正确的 PID 和 comm（已在上方查过 _info）
	if (_info) {
		e.pid = _info->pid;
		__builtin_memcpy(e.comm, _info->comm, sizeof(e.comm));
	} else {
		// map 中找不到，fallback（可能不准确，Go 端会跳过 PID=0）
		e.pid = bpf_get_current_pid_tgid() >> 32;
	}

	fill_addr_from_sock(sk, &e);
	send_event(ctx, &e);
	return 0;
}

// Hook inet_listen - 监听端口
SEC("kprobe/inet_listen")
int BPF_KPROBE(inet_listen, struct socket *sock, int backlog) {
	struct sock *sk = BPF_CORE_READ(sock, sk);
	if (!sk)
		return 0;

	// Network namespace 过滤
	__u32 netns = get_netns_from_sock(sk);
	if (!match_netns(netns))
		return 0;

	struct event e = {};
	e.type = EVENT_LISTEN_OPEN;
	e.pid = bpf_get_current_pid_tgid() >> 32;
	e.netns_inum = netns;
	bpf_get_current_comm(e.comm, sizeof(e.comm));

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
	// Network namespace 过滤（使用当前 task 的 netns）
	__u32 netns = get_netns_from_task();
	if (!match_netns(netns))
		return 0;

	struct event e = {};
	e.type = EVENT_PROCESS_START;
	e.pid = ctx->child_pid;
	e.netns_inum = netns;

	send_event(ctx, &e);
	return 0;
}

// Hook exit - 进程退出
SEC("tracepoint/sched/sched_process_exit")
int handle_exit(struct trace_event_raw_sched_process_template *ctx) {
	// Network namespace 过滤
	__u32 netns = get_netns_from_task();
	if (!match_netns(netns))
		return 0;

	struct event e = {};
	e.type = EVENT_PROCESS_EXIT;
	e.pid = bpf_get_current_pid_tgid() >> 32;
	e.netns_inum = netns;

	send_event(ctx, &e);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
