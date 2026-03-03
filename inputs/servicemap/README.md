# servicemap 插件

## 简介

`servicemap` 插件使用 eBPF 技术跟踪 TCP 连接和 HTTP 请求，自动构建服务间的调用关系图（Service Map）。它可以帮助你：

- 🔍 自动发现服务间的依赖关系
- 📊 监控服务间的网络流量和连接状态
- 🐛 快速定位网络通信问题
- 🎯 同时支持裸进程、Docker 容器和 Kubernetes Pod

本插件基于 [coroot-node-agent](https://github.com/coroot/coroot-node-agent) 的核心功能移植而来。

## 系统要求

### 运行模式

插件支持两种运行模式，**自动降级，无需手动配置**：

| 模式 | 触发条件 | 功能 |
|---|---|---|
| **eBPF 模式**（完整功能） | Linux >= 4.16，amd64/arm64，有 tracefs，eBPF 加载成功 | TCP + L7（HTTP/MySQL/Redis 等）全量追踪 |
| **轮询模式**（自动降级） | 非 Linux 平台、旧内核、无 tracefs、eBPF 加载失败时，自动触发 | TCP 连接追踪（无 L7 协议解析） |

> 插件在任何情况下都不会因环境不满足而启动失败。eBPF 不可用时自动切换轮询模式，日志中会打印 `W! servicemap: ... fallback to polling tracer`。

### eBPF 模式要求（可选，获得完整功能）

- **Linux 内核**: >= 4.16（推荐 5.1+）
- **架构**: amd64 或 arm64
- **权限**: root 或 CAP_SYS_ADMIN capability
- **tracefs 挂载**: `/sys/kernel/debug/tracing` 或 `/sys/kernel/tracing` 可访问

检查 eBPF 支持：

```bash
# 检查内核版本
uname -r

# 检查 BPF 功能
zgrep CONFIG_BPF /proc/config.gz
zgrep CONFIG_BPF_SYSCALL /proc/config.gz

# 检查 tracefs
ls /sys/kernel/debug/tracing 2>/dev/null || ls /sys/kernel/tracing
```

### 轮询模式（任意平台可用）

在以下情况下自动使用轮询模式（gopsutil 实现）：

- macOS / Windows 开发机
- Linux 内核 < 4.16
- 容器内未挂载 tracefs
- eBPF 程序加载失败（权限不足等）

### 容器环境（eBPF 模式）

```yaml
# Kubernetes
securityContext:
  privileged: true
  capabilities:
    add:
      - SYS_ADMIN
volumeMounts:
  - name: cgroup
    mountPath: /sys/fs/cgroup
    readOnly: true
  - name: proc
    mountPath: /host/proc
    readOnly: true
```

## 运行模式对比

### 数据采集机制

| 维度 | eBPF 模式 | 轮询模式 |
|---|---|---|
| **触发方式** | 内核事件驱动（tracepoint hook） | 每 **2 秒**扫描一次 `/proc/net/tcp` |
| **数据来源** | `inet_sock_set_state`、`sys_enter_connect` 等内核 tracepoint | `gopsutil.Connections("tcp")` 读 procfs |
| **事件粒度** | 每个 connect / close / retransmit 单独事件 | 前后两次快照对比，推断 open / close |

### 功能差异

| 功能 | eBPF 模式 | 轮询模式 |
|---|---|---|
| TCP 连接追踪 | ✅ | ✅ |
| 监听端口发现 | ✅（ListenOpen/ListenClose 事件） | ✅（扫描 LISTEN 状态） |
| 字节数统计（BytesSent/Received） | ✅（内核直接计数） | ❌ 始终为 0（gopsutil 无法获取） |
| TCP Retransmit 事件 | ✅ | ❌ |
| **L7 协议解析**（HTTP/MySQL/Redis 等） | ✅（独立 `l7_events` perf buffer） | ❌ 完全不支持 |
| 连接精确时间戳 | ✅（纳秒级） | ⚠️ 取当前时刻，精度约 ±2s |
| **短连接捕获**（< 2s 即断开） | ✅ 不丢 | ❌ 两次快照之间打开并关闭的连接**不可见** |

> **关键限制**：轮询模式下 `servicemap_edge_bytes_sent_total`、`servicemap_edge_bytes_received_total`、`servicemap_edge_retransmits_total` 恒为 0，L7 相关指标系列（`servicemap_mysql_*` 等）不会产生数据。

### 性能对比

| 指标 | eBPF 模式 | 轮询模式 |
|---|---|---|
| **CPU 开销** | 极低：内核态过滤 + perf ring buffer 异步消费，只有事件触发时才有用户态开销 | 固定周期开销：每 2s 系统调用读取全量连接表，连接数越多开销线性增长 |
| **内存开销** | 需分配 eBPF Map + perf buffer（默认 `pagesize × 16 ≈ 64 KB`），内核空间额外占用约 1–2 MB | 仅 Go 堆内存，无内核空间占用 |
| **事件延迟** | 亚毫秒（tracepoint 同步触发） | 最长 **2 秒**（下次 poll 才感知） |
| **高连接密度** | 百万级连接/秒不退化 | 连接数 > 1 万时 procfs 读取开销显著增加 |

### 指标可用性汇总

| 指标系列 | eBPF 模式 | 轮询模式 |
|---|---|---|
| `servicemap_tcp_*` | ✅ 全量 | ✅ 连接数 / ❌ 字节 / ❌ 重传 |
| `servicemap_edge_*` | ✅ 全量 | ✅ 连接数 / ❌ 字节 / ❌ 重传 |
| `servicemap_http_*` | ✅ | ❌ |
| `servicemap_mysql_*` / `redis_*` 等 | ✅ | ❌ |
| `servicemap_graph_*` | ✅ | ✅ |
| `servicemap_tracer_*` | ✅ | ✅ |

### 选型建议

| 场景 | 建议 |
|---|---|
| 生产 Linux 环境（K8s / 裸机） | 配置 `CAP_SYS_ADMIN` 权限，启用 eBPF 获取完整数据 |
| 开发机 / macOS | 轮询模式足够验证拓扑逻辑，无需额外配置 |
| Linux 旧内核（< 4.16）| 轮询模式可用，字节统计和 L7 数据缺失，告警规则中相关指标需设兜底处理 |
| 高连接密度旧内核（> 5000 连接）| 评估 procfs 扫描的 CPU 成本，优先考虑升级内核到 4.16+ |

---

## 安装

### 1. 编译 eBPF 程序

#### Linux 环境（推荐）

在 Linux 系统上编译 eBPF 程序：

```bash
# 安装依赖
# Ubuntu/Debian
sudo apt-get install -y clang llvm libbpf-dev linux-tools-common bpftool

# CentOS/RHEL
sudo yum install -y clang llvm bpftool

# 生成 vmlinux.h
cd inputs/coroot_servicemap/tracer/bpf
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h

# 编译 eBPF 程序
cd ..
make

# 验证生成的文件
ls -lh ebpf_programs_generated.go
```

#### 使用预编译字节码

如果无法在本地编译，可以：

1. 在 CI/CD 环境中编译
2. 使用预生成的 `ebpf_programs_generated.go`
3. 跨平台交叉编译（需要 target 系统的 vmlinux.h）

详细说明请参考 [tracer/bpf/README.md](./tracer/bpf/README.md)。

#### macOS / 其他系统

eBPF 仅支持 Linux。在非 Linux 系统上：
- 插件会自动回退到**轮询模式**（使用 gopsutil）
- 功能稍弱但仍可正常工作
- 无需编译 eBPF 程序

### 2. 配置

创建配置文件 `conf/input.servicemap/servicemap.toml`:

```toml
[[instances]]
interval = 60

# 启用 TCP 连接跟踪
enable_tcp = true

# 启用 HTTP 请求跟踪
enable_http = true
enable_l7_tracing = true

# 容器发现
enable_docker = true
enable_k8s = true
enable_cgroup = true

# 过滤配置
ignore_ports = [22, 9100]
ignore_cidrs = ["127.0.0.0/8"]

# Docker label 白名单：只有列出的 label key 才会透传为 Prometheus 标签
# 留空则不透传任何 Docker label（推荐，防止高基数标签导致时序爆炸）
# label_allowlist = ["app", "version", "team"]

# 附加标签（框架自动注入到所有指标）
[instances.labels]
  # env = "production"
  # cluster = "cluster-1"
```

## 指标列表

所有指标名称以 `servicemap_` 为前缀。

---

### 公共标签说明

以下标签在多个指标系列中复用，含义相同：

| 标签 | 示例值 | 说明 |
|---|---|---|
| `source_id` | `proc_nginx` / `a3f8c1d2` | 源节点唯一标识。裸进程为 `proc_<进程名>`（或 `proc_<pid>` 兜底），容器为 Docker 短 ID |
| `source_name` | `nginx` / `api-server` | 源节点可读名称。裸进程为进程名，容器为 Docker 容器名 |
| `source_type` | `bare_process` / `container` | 区分裸进程与容器化进程，用于告警分组和过滤 |
| `namespace` | `production` | K8s 命名空间（非 K8s 时不输出） |
| `pod_name` | `api-server-7d9f8` | K8s Pod 名称（非 K8s 时不输出） |
| `image` | `nginx:1.25` | 容器镜像名（仅容器场景） |
| `destination` | `10.0.0.1:3306` | 目标端点完整地址，`host:port` 格式 |

---

### TCP 连接指标

> 需启用 `enable_tcp = true`

**标签**：公共标签 + `destination`

| 指标名 | 类型 | 说明 |
|---|---|---|
| `servicemap_tcp_connects_total` | Counter | 成功建立的 TCP 连接次数（累计） |
| `servicemap_tcp_connect_failed_total` | Counter | TCP 连接失败次数（累计） |
| `servicemap_tcp_retransmits_total` | Counter | TCP 重传次数（累计） |
| `servicemap_tcp_bytes_sent_total` | Counter | 向目标发送的字节数（累计） |
| `servicemap_tcp_bytes_received_total` | Counter | 从目标接收的字节数（累计） |
| `servicemap_tcp_connect_duration_seconds_sum` | Counter | 所有成功连接的建连耗时总和（秒，累计） |
| `servicemap_tcp_connect_duration_seconds_count` | Counter | 成功连接次数（与 `_sum` 配合计算平均建连时延） |
| `servicemap_tcp_active_connections` | Gauge | 当前活跃连接数（瞬时值） |

**PromQL 示例**：
```promql
# 某进程的连接失败率
rate(servicemap_tcp_connect_failed_total{source_name="nginx"}[5m])
  / rate(servicemap_tcp_connects_total{source_name="nginx"}[5m])

# 平均 TCP 建连时延（毫秒）
rate(servicemap_tcp_connect_duration_seconds_sum[5m])
  / rate(servicemap_tcp_connect_duration_seconds_count[5m]) * 1000
```

---

### HTTP 请求指标

> 需启用 `enable_http = true`

**标签**：公共标签 + `destination` + `method` + `status_code` + `status_class`

| 标签 | 示例值 | 说明 |
|---|---|---|
| `method` | `GET` / `POST` | HTTP 请求方法 |
| `status_code` | `200` / `404` | HTTP 响应状态码 |
| `status_class` | `2xx` / `4xx` / `5xx` | 状态码分类，便于聚合告警 |

| 指标名 | 类型 | 说明 |
|---|---|---|
| `servicemap_http_requests_total` | Counter | HTTP 请求总数（累计） |
| `servicemap_http_request_errors_total` | Counter | HTTP 请求错误数（4xx/5xx，累计） |
| `servicemap_http_bytes_sent_total` | Counter | HTTP 请求发送字节数（累计） |
| `servicemap_http_bytes_received_total` | Counter | HTTP 响应接收字节数（累计） |
| `servicemap_http_request_duration_seconds_sum` | Counter | 所有请求的响应时延总和（秒，累计） |
| `servicemap_http_request_duration_seconds_count` | Counter | 请求总数（与 `_sum` 配合计算平均时延） |

**PromQL 示例**：
```promql
# HTTP 错误率
rate(servicemap_http_request_errors_total[5m])
  / rate(servicemap_http_requests_total[5m])

# 平均响应时延（毫秒）
rate(servicemap_http_request_duration_seconds_sum[5m])
  / rate(servicemap_http_request_duration_seconds_count[5m]) * 1000
```

---

### L7 协议指标（MySQL / PostgreSQL / Redis / Kafka）

> 需启用 `disable_l7_tracing = false`（默认开启）

**标签**：公共标签 + `destination` + `protocol` + `status`

| 标签 | 示例值 | 说明 |
|---|---|---|
| `protocol` | `MySQL` / `Postgres` / `Redis` / `Kafka` | L7 协议名称 |
| `status` | `ok` / `failed` / `unknown` | 调用结果状态 |

指标名以协议小写名为前缀（`mysql_` / `postgres_` / `redis_` / `kafka_`）：

| 指标名（以 `mysql_` 为例） | 类型 | 说明 |
|---|---|---|
| `servicemap_mysql_requests_total` | Counter | 请求总数（累计） |
| `servicemap_mysql_request_errors_total` | Counter | 请求错误数（累计） |
| `servicemap_mysql_request_duration_seconds_sum` | Counter | 请求耗时总和（秒，累计） |
| `servicemap_mysql_request_duration_seconds_count` | Counter | 请求总数（与 `_sum` 配合计算平均时延） |

---

### 服务拓扑边指标（edge）

> 需启用 `enable_tcp = true`。每条边代表一个源节点到一个目标端点的聚合调用关系。

**标签**：

| 标签 | 是否必填 | 示例值 | 说明 |
|---|---|---|---|
| `source_id` | ✅ | `proc_nginx` / `a3f8c1` | 源节点唯一标识 |
| `source_name` | ✅ | `nginx` / `api-server` | 源节点可读名称 |
| `source_type` | ✅ | `bare_process` / `container` | 区分裸进程与容器 |
| `destination` | ✅ | `10.0.0.1:3306` | 目标端点完整地址 |
| `destination_host` | 条件 | `10.0.0.1` | 目标主机（解析失败时不输出） |
| `destination_port` | 条件 | `3306` | 目标端口（解析失败时不输出） |
| `namespace` | 条件 | `production` | 源侧 K8s 命名空间（非 K8s 时不输出） |
| `pod_name` | 条件 | `api-7d9f8` | 源侧 K8s Pod 名称（非 K8s 时不输出） |

| 指标名 | 类型 | 说明 |
|---|---|---|
| `servicemap_edge_connects_total` | Counter | 源→目标的 TCP 成功建连次数（累计） |
| `servicemap_edge_connect_failed_total` | Counter | 源→目标的 TCP 连接失败次数（累计） |
| `servicemap_edge_retransmits_total` | Counter | 源→目标的 TCP 重传次数（累计） |
| `servicemap_edge_bytes_sent_total` | Counter | 源→目标发送字节数（累计） |
| `servicemap_edge_bytes_received_total` | Counter | 源→目标接收字节数（累计） |
| `servicemap_edge_active_connections` | Gauge | 当前活跃连接数（瞬时值） |

**边的聚合粒度**：边的唯一键为 `source_id + "->" + destination`。同名进程的多个实例（如 4 个 nginx worker）共享同一条边。

**PromQL 示例**：
```promql
# 服务拓扑图：查看所有服务间的活跃连接
servicemap_edge_active_connections > 0

# 某服务对 MySQL 的调用连接失败率
rate(servicemap_edge_connect_failed_total{source_name="api-server", destination_port="3306"}[5m])
```

---

### 拓扑规模指标（graph）

> 按 `source_type` 分拆输出，分别统计裸进程和容器的拓扑规模。

**标签**：

| 标签 | 示例值 | 说明 |
|---|---|---|
| `source_type` | `bare_process` / `container` | 节点/边类型分组 |
| `kube_node` | `node-1` | K8s 节点名，来自 `NODE_NAME` 环境变量（非 K8s 时不输出） |
| `cluster` | `prod` | 集群名，由 `[instances.labels]` 配置注入（可选） |

| 指标名 | 类型 | 说明 |
|---|---|---|
| `servicemap_graph_nodes` | Gauge | 当前拓扑图中节点（服务/进程）总数 |
| `servicemap_graph_edges` | Gauge | 当前拓扑图中边（服务间调用关系）总数 |

**PromQL 示例**：
```promql
# 容器服务节点总数
servicemap_graph_nodes{source_type="container"}

# 裸进程边总数（观察非容器化服务的调用关系规模）
servicemap_graph_edges{source_type="bare_process"}

# 节点数骤降告警（服务异常下线）
servicemap_graph_nodes < 5
```

---

### 主机级统计（无容器时兜底）

> 当未发现任何容器时输出，作为兜底统计。

**标签**：`host="local"`

| 指标名 | 类型 | 说明 |
|---|---|---|
| `servicemap_host_active_connections` | Gauge | 主机当前活跃 TCP 连接总数 |
| `servicemap_host_bytes_sent_total` | Counter | 主机总发送字节数（累计） |
| `servicemap_host_bytes_received_total` | Counter | 主机总接收字节数（累计） |

---

### 插件自监控指标

**标签**：无

| 指标名 | 类型 | 说明 |
|---|---|---|
| `servicemap_tracer_active_connections` | Gauge | eBPF tracer 当前追踪的活跃连接数 |
| `servicemap_tracer_listen_ports` | Gauge | 当前监听端口数量 |
| `servicemap_tracked_containers` | Gauge | 当前注册表中追踪的容器/进程总数 |

### 1. eBPF 程序加载失败

```bash
# 检查内核版本
uname -r  # 应该 >= 4.16

# 检查权限
id  # 应该是 root 或有 CAP_SYS_ADMIN

# 检查 BPF 文件系统
mount | grep bpf
```

### 2. 看不到容器信息

```bash
# 检查 cgroup 挂载
ls -la /sys/fs/cgroup/

# 检查容器运行时
docker ps
crictl ps
```

### 3. 性能问题

如果遇到性能问题，可以：

- 增大采集间隔 (`interval`)
- 禁用 L7 跟踪 (`enable_l7_tracing = false`)
- 添加更多过滤规则

## 开发状态

**当前状态**: ✅ 可用

- [x] 基础框架与插件接口
- [x] eBPF TCP/HTTP 追踪
- [x] L7 协议解析（MySQL / PostgreSQL / Redis / Kafka）
- [x] Docker 容器发现
- [x] Kubernetes Pod 发现（含 K8s 元数据注入）
- [x] 裸进程追踪（`proc_<进程名>` 聚合）
- [x] 服务拓扑图构建与 Graph API
- [x] 全量单元测试（race 模式）

## 参考资料

- [Coroot Node Agent](https://github.com/coroot/coroot-node-agent)
- [eBPF Documentation](https://ebpf.io/)
- [Cilium eBPF Library](https://github.com/cilium/ebpf)
- [Graph API 使用说明](./graph_api.go)

## 许可证

Apache License 2.0
