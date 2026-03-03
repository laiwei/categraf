# Coroot ServiceMap 插件使用场景手册

> 本文档系统性地描述 `servicemap` 插件的典型使用场景，每个场景包含：背景说明、配置方式、关键指标、PromQL 查询示例及可视化建议。

---

## 目录

| # | 场景 | 核心价值 |
|---|------|---------|
| 1 | [微服务架构服务拓扑自动发现](#场景一微服务架构服务拓扑自动发现) | 零代码侵入，自动绘制调用关系图 |
| 2 | [Kubernetes 无侵入可观测性](#场景二kubernetes-无侵入可观测性) | 无需 Sidecar，DaemonSet 覆盖全集群 |
| 3 | [数据库访问分析](#场景三数据库访问分析) | 精准定位慢查询、高错误率的数据库连接 |
| 4 | [网络故障诊断与根因分析](#场景四网络故障诊断与根因分析) | TCP 重传、连接失败快速定位 |
| 5 | [服务间流量与容量规划](#场景五服务间流量与容量规划) | 带宽用量、连接数趋势，指导扩容 |
| 6 | [安全审计——未知连接发现](#场景六安全审计未知连接发现) | 发现异常的东西向流量 |
| 7 | [AI 辅助运维（Graph API + LLM）](#场景七ai-辅助运维graph-api--llm) | 供 AI Agent 消费的拓扑接口 |
| 8 | [混沌工程与容错验证](#场景八混沌工程与容错验证) | 观察故障注入前后的拓扑变化 |
| 9 | [非 Linux 开发环境（轮询模式）](#场景九非-linux-开发环境轮询模式) | macOS/Windows 开发机本地调试 |
| 10 | [与 Prometheus / Grafana / VictoriaMetrics 集成](#场景十与-prometheus--grafana--victoriametrics-集成) | 生产级监控体系接入 |

---

## 场景一：微服务架构服务拓扑自动发现

### 背景

在微服务架构中，随着服务数量增长，人工维护服务依赖关系图变得困难且易过时。传统的做法需要：

- 在每个服务中埋点（修改代码）
- 维护手动绘制的架构图
- 部署 APM Agent（侵入业务进程）

`servicemap` 通过 **eBPF 在内核层** 捕获真实的 TCP 连接，**无需修改任何业务代码**，自动发现服务间调用关系。

### 适用场景

- 接管遗留系统，尚未摸清服务依赖
- 新系统上线前的依赖关系审查
- 架构演进中的"实际调用"与"设计意图"对比
- 微服务拆分时的边界识别

### 配置示例

```toml
# conf/input.servicemap/servicemap.toml
[[instances]]
interval = 30

enable_tcp   = true
enable_http  = true
enable_cgroup = true

# 忽略运维端口，避免干扰
ignore_ports = [22, 9100, 9090]
ignore_cidrs = ["127.0.0.0/8"]

# 启用 Graph API，供前端/AI 实时查询拓扑
api_addr = ":9099"
```

### 插件工作原理

```
业务容器 A ──TCP connect──▶ 业务容器 B
                │
         eBPF hook (内核)
                │
         servicemap
                │
    ┌───────────┴────────────┐
    │  容器注册表（Registry）  │
    │  Docker / K8s / cgroup  │
    └───────────┬────────────┘
                │
    ┌───────────┴────────────┐
    │  服务拓扑图（Graph）    │
    │  nodes + edges          │
    └───────────┬────────────┘
                │
    Prometheus 指标  +  /graph API
```

### 关键指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `servicemap_edge_active_connections` | Gauge | A→B 当前活跃连接数 |
| `servicemap_edge_connects_total` | Counter | A→B 累计连接次数 |
| `servicemap_graph_nodes` | Gauge | 当前发现的服务节点数 |
| `servicemap_graph_edges` | Gauge | 当前发现的服务调用边数 |
| `servicemap_tracked_containers` | Gauge | 正在追踪的容器数量 |

### PromQL 查询

```promql
# 当前活跃的服务调用边（非零连接）
servicemap_edge_active_connections > 0

# 按来源服务分组，查看每个服务的出站连接数
sum by (source_name, destination_host) (
  servicemap_edge_active_connections
)

# 近 5 分钟内有新连接建立的服务对
sum by (source_name, destination_host, destination_port) (
  increase(servicemap_edge_connects_total[5m])
) > 0

# 当前拓扑图规模
servicemap_graph_nodes
servicemap_graph_edges
```

### 拓扑 API 查询

启用 `api_addr = ":9099"` 后，可直接获取当前拓扑：

```bash
# JSON 格式（供程序消费）
curl http://localhost:9099/graph | jq .

# 文本格式（供人类阅读/AI 消费）
curl http://localhost:9099/graph/text

# 示例输出：
# === Service Map @ 2026-03-02T10:00:00Z ===
# Summary: 8 nodes, 12 edges | tracer active=45 listen=8 tracked=8
#
# Nodes (8):
#   [abc123] name=api-gateway ns=production pod=api-gateway-7d8f9b
#   [def456] name=user-service ns=production pod=user-service-5c6d7e
#   ...
#
# Edges (12):
#   abc123 -> 10.0.1.5:8080  [TCP, HTTP]
#     TCP: connects=1240 failed=0 active=8 retx=2 sent=15MB recv=42MB
#     HTTP GET 200(2xx): req=1200 err=0 avg=12.5ms
```

### 非容器化进程能否被追踪？

**是的，eBPF Tracer 在内核层面会捕获主机上所有进程的 TCP 连接，不区分容器与否。** 但不同层级的支持程度有差异：

| 层级 | 容器化进程 | 非容器化（裸进程）|
|------|-----------|-----------------|
| TCP 连接事件捕获 | ✅ 完整 | ✅ 完整（eBPF 无差别捕获） |
| L7 协议解析（HTTP/MySQL 等） | ✅ | ✅ |
| 容器元数据（name/pod/namespace） | ✅ | ❌ 无法关联 |
| 字节流量统计（bytes sent/received） | ✅ 精确 | ⚠️ 部分丢失（见下方说明）|
| Prometheus 指标输出 | ✅ 带完整标签 | ⚠️ 降级为 `unknown` 或仅主机聚合 |
| 服务拓扑图中可见 | ✅ | ⚠️ 有限支持 |

**详细行为说明：**

1. **事件层**：当 eBPF 捕获到来自裸进程的 TCP 事件，Registry 通过 `/proc/<pid>/cgroup` 尝试提取容器 ID。裸进程的 cgroup 路径中无 Docker/containerd 容器 ID，插件改为读取 `/proc/<pid>/comm` 获取进程名，生成合成 ID `proc_<进程名>`（如 `proc_nginx`），以进程名为粒度聚合同类进程——**同名的多个进程实例共享一条时序**，保证时序稳定、基数可控。

2. **流量统计层**（字节数）：裸进程合成 ID（`proc_<comm>` 或 `proc_<pid>`）已支持字节流量统计，与容器化进程同等对待。

3. **主机聚合统计**：`collectHostStats` 会输出 `host_active_connections`、`host_bytes_sent_total`、`host_bytes_received_total`，但该函数**仅在容器列表为空时触发**，作为兜底降级路径。

**实际建议：**

```
纯裸机部署（无容器）     →  裸进程以 proc_<进程名> 聚合，完整支持 TCP 追踪和字节统计
纯容器部署              →  完整支持，推荐场景
容器 + 裸进程混合部署   →  两者均完整支持，source_type 标签区分来源
```

> ✅ **已实现**：当 `getContainerIDByPID` 返回空时，插件通过读取 `/proc/<pid>/comm` 将裸进程以 `proc_<进程名>`（或 `proc_<pid>` 兜底）作为合成 source ID 注册，提供与容器同等的 TCP 连接追踪和字节流量统计，并在指标中通过 `source_type="bare_process"` 标签加以区分。相关实现见 [containers/registry.go](containers/registry.go)（`resolveContainerID` / `resolveProcID` / `enrichProcContainer`）。

---

### 与传统方案对比

| 维度 | 传统 APM 埋点 | Service Mesh Sidecar | servicemap |
|------|-------------|---------------------|-------------------|
| 代码侵入 | ✗ 需要 | ✗ 需要配置 | ✅ 零侵入 |
| 性能开销 | 中~高 | 中（额外进程） | 低（eBPF 内核级） |
| 覆盖范围 | 仅已埋点服务 | 仅 K8s Pod | 所有 TCP 连接 |
| 历史服务兼容 | ✗ 需改代码 | ✗ 需注入 | ✅ 直接支持 |
| 非容器化进程支持 | ✅（若已埋点） | ❌ | ⚠️ 部分（无元数据标签） |
| L7 协议感知 | ✅ | ✅ | ✅ HTTP/MySQL/Postgres/Redis/Kafka |

---

## 场景二：Kubernetes 无侵入可观测性

### 背景

Kubernetes 环境中，传统可观测方案面临以下问题：

- **Service Mesh**（Istio/Linkerd）：需要为每个 Pod 注入 Sidecar，增加资源消耗和运维复杂度
- **应用 APM**：需要各语言 SDK，跨语言兼容性差
- **网络插件（CNI）**：只提供网络层，无法识别 L7 协议

`servicemap` 以 **DaemonSet** 方式部署，每个节点一个 Pod，即可覆盖该节点上所有容器的网络流量，自动关联 Pod 名称、Namespace、Label 等 K8s 元数据。

### Kubernetes 部署配置

#### DaemonSet 清单

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: categraf-servicemap
  namespace: monitoring
spec:
  selector:
    matchLabels:
      app: categraf-servicemap
  template:
    metadata:
      labels:
        app: categraf-servicemap
    spec:
      hostNetwork: true
      hostPID: true
      tolerations:
        - operator: Exists
      containers:
        - name: categraf
          image: flashcatcloud/categraf:latest
          securityContext:
            privileged: true           # eBPF 需要特权模式
            capabilities:
              add:
                - SYS_ADMIN
                - NET_ADMIN
                - SYS_PTRACE
          volumeMounts:
            - name: sys-fs-cgroup
              mountPath: /sys/fs/cgroup
              readOnly: true
            - name: proc
              mountPath: /host/proc
              readOnly: true
            - name: docker-sock
              mountPath: /var/run/docker.sock
              readOnly: true
            - name: config
              mountPath: /etc/categraf/conf
          env:
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
      volumes:
        - name: sys-fs-cgroup
          hostPath:
            path: /sys/fs/cgroup
        - name: proc
          hostPath:
            path: /proc
        - name: docker-sock
          hostPath:
            path: /var/run/docker.sock
        - name: config
          configMap:
            name: categraf-servicemap-config
```

#### 插件配置（ConfigMap）

```toml
[[instances]]
interval = 60
enable_tcp    = true
enable_http   = true
enable_cgroup = true

# 启用 K8s 元数据关联（自动发现 Pod/Namespace/Label）
# kubeconfig_path = ""  # 留空时使用 in-cluster config

ignore_ports = [22, 9100, 10250, 10255]
ignore_cidrs = ["127.0.0.0/8", "169.254.0.0/16"]

api_addr = ":9099"
```

### K8s 元数据自动关联

插件自动为每条指标附加 Kubernetes 元数据标签：

```
servicemap_edge_active_connections{
  source_id="abc123",
  source_name="api-gateway",
  namespace="production",
  pod_name="api-gateway-7d8f9b-xk9p2",
  destination="10.0.1.5:8080",
  destination_host="10.0.1.5",
  destination_port="8080"
}
```

### 关键指标与 PromQL

```promql
# 按 Namespace 汇总活跃连接数
sum by (namespace) (
  servicemap_edge_active_connections
)

# 查找 production namespace 中连接到数据库（3306）的服务
servicemap_edge_active_connections{
  namespace="production",
  destination_port="3306"
} > 0

# 某个 Pod 的出站 HTTP 错误率
sum by (pod_name, destination) (
  rate(servicemap_http_request_errors_total{
    namespace="production"
  }[5m])
)
/
sum by (pod_name, destination) (
  rate(servicemap_http_requests_total{
    namespace="production"
  }[5m])
)

# 跨 Namespace 调用（安全合规审计）
servicemap_edge_active_connections{
  namespace!="kube-system"
} > 0
```

### 与 Namespace 网络策略联动

当 Kubernetes NetworkPolicy 配置不当时，可通过以下查询发现违规调用：

```promql
# 发现来自 dev namespace 到 production namespace 的意外连接
# （需要结合业务逻辑判断，指标本身不区分方向，但 source/destination 标签可以推断）
servicemap_edge_connects_total{
  namespace="dev",
  destination_port=~"8080|8443|3306"
}
```

---

## 场景三：数据库访问分析

### 背景

数据库是大多数业务系统的性能瓶颈来源。传统监控通常只在数据库侧采集指标（慢查询日志、QPS、连接数），无法回答：

- **哪个服务**在消耗数据库连接？
- **哪条调用路径**产生了慢查询？
- **数据库错误**是某个特定服务触发的吗？

`servicemap` 通过 eBPF 在网络层解析 L7 协议，无需访问数据库本身，即可从**客户端视角**获取每条连接的请求量、错误率和延迟分布。

### 支持的数据库协议

| 协议 | 指标前缀 | 说明 |
|------|---------|------|
| MySQL | `servicemap_mysql_*` | 含错误响应识别 |
| PostgreSQL | `servicemap_postgres_*` | 含错误响应识别 |
| Redis | `servicemap_redis_*` | 含 WRONGTYPE 等错误 |
| Kafka | `servicemap_kafka_*` | Producer/Consumer 请求 |

### 配置示例

```toml
[[instances]]
interval = 30
enable_tcp = true
enable_http = true

# 确保 L7 解析开启（默认开启）
disable_l7_tracing = false

# 不过滤数据库端口（确保能采集到）
# ignore_ports = []  # 默认不忽略 3306/5432/6379/9092

ignore_cidrs = ["127.0.0.0/8"]
```

### MySQL 分析

#### 关键指标

| 指标 | 说明 |
|------|------|
| `servicemap_mysql_requests_total` | 累计请求数 |
| `servicemap_mysql_request_errors_total` | 累计错误数（如 SQL 语法错误、权限错误） |
| `servicemap_mysql_request_duration_seconds_sum` | 请求总耗时（秒） |
| `servicemap_mysql_request_duration_seconds_count` | 请求次数（同 requests_total） |

标签：`source_id`, `source_name`, `source_type`, `destination`（MySQL 服务地址 `ip:3306`）, `protocol`=`MySQL`, `status`（`ok`/`error`）

#### PromQL 查询

```promql
# 各服务对 MySQL 的请求速率（QPS）
sum by (source_id, destination) (
  rate(servicemap_mysql_requests_total[5m])
)

# MySQL 错误率（按来源服务和目标实例）
sum by (source_id, destination) (
  rate(servicemap_mysql_request_errors_total[5m])
)
/
sum by (source_id, destination) (
  rate(servicemap_mysql_requests_total[5m])
)

# MySQL 平均响应延迟（ms）
sum by (source_id, destination) (
  rate(servicemap_mysql_request_duration_seconds_sum[5m])
)
/
sum by (source_id, destination) (
  rate(servicemap_mysql_request_duration_seconds_count[5m])
) * 1000

# 告警：MySQL 平均延迟超过 100ms
(
  sum by (source_id, destination) (
    rate(servicemap_mysql_request_duration_seconds_sum[5m])
  )
  /
  sum by (source_id, destination) (
    rate(servicemap_mysql_request_duration_seconds_count[5m])
  )
) * 1000 > 100
```

### PostgreSQL 分析

```promql
# Postgres 请求 QPS，按目标数据库实例分组
sum by (destination) (
  rate(servicemap_postgres_requests_total[5m])
)

# Postgres 连接失败（TCP 层）
rate(servicemap_tcp_connect_failed_total{
  destination=~".*:5432"
}[5m])

# Postgres 高延迟告警（>200ms）
(
  rate(servicemap_postgres_request_duration_seconds_sum[5m])
  /
  rate(servicemap_postgres_request_duration_seconds_count[5m])
) * 1000 > 200
```

### Redis 分析

```promql
# Redis 请求总量（所有实例合计）
sum(rate(servicemap_redis_requests_total[5m]))

# Redis 错误请求（WRONGTYPE、NOAUTH 等）
sum by (source_id, destination, status) (
  rate(servicemap_redis_request_errors_total[5m])
)

# Redis 平均响应时间（正常应 < 1ms）
(
  rate(servicemap_redis_request_duration_seconds_sum[5m])
  /
  rate(servicemap_redis_request_duration_seconds_count[5m])
) * 1000

# 告警：Redis 响应时间超过 5ms（可能存在大 Key 或网络问题）
(
  rate(servicemap_redis_request_duration_seconds_sum[5m])
  /
  rate(servicemap_redis_request_duration_seconds_count[5m])
) * 1000 > 5
```

### Kafka 分析

```promql
# Kafka 生产/消费请求速率
sum by (source_id, destination) (
  rate(servicemap_kafka_requests_total[5m])
)

# Kafka 错误率
sum by (source_id, destination) (
  rate(servicemap_kafka_request_errors_total[5m])
)
/
sum by (source_id, destination) (
  rate(servicemap_kafka_requests_total[5m])
)
```

### 多数据库对比仪表板

在 Grafana 中可以创建变量 `$protocol`，切换查看不同协议：

```promql
# 通用模板（$protocol = mysql / postgres / redis / kafka）
sum by (source_id, destination) (
  rate(servicemap_${protocol}_requests_total[5m])
)
```

### Graph API 查看数据库依赖

```bash
# 查看所有连接到 MySQL(3306) 的服务
curl http://localhost:9099/graph/text | grep ":3306"

# 示例输出：
#   user-service-abc123 -> 10.0.2.10:3306  [TCP, MySQL]
#     TCP: connects=850 failed=0 active=5 retx=0 sent=2MB recv=8MB avg_connect=0.8ms
#     L7 MySQL[ok]: req=42000 err=12 avg=8.2ms
#     L7 MySQL[error]: req=12 err=12 avg=1.1ms
```

---

---

## 场景四：网络故障诊断与根因分析

### 背景

网络故障往往是最难排查的问题之一：应用层看到的是超时或错误，但根因可能在于：

- TCP 连接建立失败（目标服务宕机、防火墙策略变更）
- 网络丢包导致大量 TCP 重传（物理网卡故障、网络拥塞）
- 连接延迟突增（DNS 解析慢、内核参数不当）
- 连接数耗尽（连接池泄漏、慢查询持续占用连接）

`servicemap` 从内核层直接观测 TCP 状态机，提供最接近"真相"的网络诊断数据。

### 关键诊断指标

| 指标 | 类型 | 诊断意义 |
|------|------|---------|
| `servicemap_tcp_connect_failed_total` | Counter | 连接建立失败次数（目标不可达、拒绝连接） |
| `servicemap_tcp_retransmits_total` | Counter | TCP 重传次数（丢包信号） |
| `servicemap_tcp_connect_duration_seconds_sum/count` | Counter | 连接建立时延（SYN→SYN-ACK 耗时） |
| `servicemap_tcp_active_connections` | Gauge | 当前活跃连接数（连接泄漏检测） |
| `servicemap_tcp_connects_total` | Counter | 成功建立连接总数 |

### 故障场景一：服务不可达（连接失败率飙升）

**现象**：某服务突然出现大量超时报警。

**诊断查询**：
```promql
# 连接失败率（按服务对）
sum by (source_id, destination) (
  rate(servicemap_tcp_connect_failed_total[5m])
)
/
(
  sum by (source_id, destination) (
    rate(servicemap_tcp_connects_total[5m])
  )
  +
  sum by (source_id, destination) (
    rate(servicemap_tcp_connect_failed_total[5m])
  )
) > 0.01

# 绝对值：近 1 分钟新增连接失败数
sum by (source_id, destination) (
  increase(servicemap_tcp_connect_failed_total[1m])
) > 5

# 对比：同目标的历史成功连接（判断是否从未连通过）
sum by (destination) (
  increase(servicemap_tcp_connects_total[10m])
)
```

**告警规则（Prometheus AlertRule）**：
```yaml
groups:
  - name: servicemap.tcp
    rules:
      - alert: TCPConnectFailureHigh
        expr: |
          sum by (source_id, destination) (
            rate(servicemap_tcp_connect_failed_total[5m])
          ) > 0.5
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "服务 {{ $labels.source_name }} 到 {{ $labels.destination }} 连接失败率高"
          description: "每秒连接失败 {{ $value | humanize }} 次，请检查目标服务和防火墙策略"
```

### 故障场景二：网络丢包（重传率升高）

**现象**：服务延迟抖动，但连接未断开，TCP 重传计数器持续增长。

**诊断查询**：
```promql
# 重传速率（每秒）
sum by (source_id, destination) (
  rate(servicemap_tcp_retransmits_total[5m])
)

# 重传比率 = 重传次数 / 成功连接次数（近似丢包率指标）
sum by (destination) (
  rate(servicemap_tcp_retransmits_total[5m])
)
/
sum by (destination) (
  rate(servicemap_tcp_connects_total[5m])
)

# 告警：某路径重传速率 > 10/s
sum by (source_id, destination) (
  rate(servicemap_tcp_retransmits_total[5m])
) > 10
```

**告警规则**：
```yaml
      - alert: TCPRetransmitHigh
        expr: |
          sum by (source_id, destination) (
            rate(servicemap_tcp_retransmits_total[5m])
          ) > 5
        for: 3m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.source_name }} → {{ $labels.destination }} 存在大量 TCP 重传"
          description: "重传速率 {{ $value | humanize }}/s，可能存在网络丢包，建议检查物理链路和网卡"
```

### 故障场景三：连接建立延迟高（SYN 延迟）

**现象**：正常情况下 TCP 握手应 < 1ms（同机房），如果延迟升高说明存在网络拥塞或内核处理瓶颈。

**诊断查询**：
```promql
# 平均 TCP 连接建立时延（毫秒）
sum by (source_id, destination) (
  rate(servicemap_tcp_connect_duration_seconds_sum[5m])
)
/
sum by (source_id, destination) (
  rate(servicemap_tcp_connect_duration_seconds_count[5m])
) * 1000

# 告警：同机房连接时延 > 5ms
(
  sum by (source_id, destination) (
    rate(servicemap_tcp_connect_duration_seconds_sum[5m])
  )
  /
  sum by (source_id, destination) (
    rate(servicemap_tcp_connect_duration_seconds_count[5m])
  )
) * 1000 > 5
```

### 故障场景四：连接数泄漏

**现象**：服务运行一段时间后内存上涨，怀疑连接未正确释放。

**诊断查询**：
```promql
# 活跃连接数随时间的变化趋势
sum by (source_id, destination) (
  servicemap_tcp_active_connections
)

# 对比新建连接速率（如果新建 >> 关闭，说明泄漏）
# 新建速率
sum by (source_id) (
  rate(servicemap_tcp_connects_total[5m])
)

# 如果 active_connections 持续线性增长而 connects_total 增速平稳 → 连接泄漏
# 通过 deriv() 检测连接数是否持续增长
deriv(
  sum by (source_id, destination) (
    servicemap_tcp_active_connections
  )[10m:]
) > 1
```

### 故障诊断流程图

```
应用报超时/错误
      │
      ▼
① 检查连接失败率
  tcp_connect_failed_total ↑ ?
      │ Yes                      │ No
      ▼                          ▼
  目标服务/防火墙问题        ② 检查重传率
                             tcp_retransmits_total ↑ ?
                                  │ Yes              │ No
                                  ▼                  ▼
                             网络丢包/拥塞      ③ 检查连接时延
                                             connect_duration ↑ ?
                                                  │ Yes       │ No
                                                  ▼           ▼
                                            DNS/内核瓶颈   ④ 检查活跃连接数
                                                         active_connections ↑ ?
                                                              │ Yes
                                                              ▼
                                                         连接泄漏/池耗尽
```

---

## 场景五：服务间流量与容量规划

### 背景

容量规划需要回答：

- 各服务间的带宽消耗是多少？
- 当前连接数是否接近系统瓶颈？
- 下个季度业务翻倍，网络带宽够用吗？
- 哪条调用链路的流量最大，需要优先优化？

`servicemap` 提供字节级别的流量统计和连接数指标，是容量规划的重要数据来源。

### 关键指标

| 指标 | 说明 |
|------|------|
| `servicemap_tcp_bytes_sent_total` | 容器维度：累计发送字节数 |
| `servicemap_tcp_bytes_received_total` | 容器维度：累计接收字节数 |
| `servicemap_edge_bytes_sent_total` | 边维度：A→B 累计发送字节数 |
| `servicemap_edge_bytes_received_total` | 边维度：A→B 累计接收字节数 |
| `servicemap_tcp_active_connections` | 当前活跃连接数 |
| `servicemap_edge_active_connections` | 指定调用边的活跃连接数 |
| `servicemap_host_bytes_sent_total` | 主机维度：总发送字节（无容器时） |
| `servicemap_host_bytes_received_total` | 主机维度：总接收字节（无容器时） |

### 带宽消耗分析

```promql
# 各服务对之间的出站带宽（bytes/s）
sum by (source_name, destination_host, destination_port) (
  rate(servicemap_edge_bytes_sent_total[5m])
)

# 各服务对之间的入站带宽（bytes/s）
sum by (source_name, destination_host, destination_port) (
  rate(servicemap_edge_bytes_received_total[5m])
)

# Top 10 带宽消耗的调用边（出站）
topk(10,
  sum by (source_name, destination) (
    rate(servicemap_edge_bytes_sent_total[5m])
  )
)

# 单个容器的总带宽（发送+接收）
sum by (source_id) (
  rate(servicemap_tcp_bytes_sent_total[5m])
  +
  rate(servicemap_tcp_bytes_received_total[5m])
)

# 以 MB/s 为单位显示
sum by (source_name, destination) (
  rate(servicemap_edge_bytes_sent_total[5m])
) / 1024 / 1024
```

### 连接数分析与容量评估

```promql
# 当前全局活跃连接总数
sum(servicemap_tcp_active_connections)

# 按目标服务分组，查看每个下游服务承受的连接压力
sum by (destination) (
  servicemap_edge_active_connections
)

# 某数据库实例承受的客户端连接数
sum by (destination) (
  servicemap_tcp_active_connections{
    destination=~".*:3306"
  }
)

# 告警：到某 MySQL 实例的连接数 > 80（接近连接池上限）
sum by (destination) (
  servicemap_tcp_active_connections{
    destination=~".*:3306"
  }
) > 80

# 当前 tracer 追踪的全局连接数（插件自身容量监控）
servicemap_tracer_active_connections
```

### 流量趋势与增长预测

```promql
# 计算过去 7 天的日均带宽增长率（适用于 VictoriaMetrics 长期存储）
(
  sum(rate(servicemap_edge_bytes_sent_total[1d] offset 0d))
  -
  sum(rate(servicemap_edge_bytes_sent_total[1d] offset 7d))
)
/
sum(rate(servicemap_edge_bytes_sent_total[1d] offset 7d))
* 100

# 环比：本周 vs 上周同时段带宽
sum(rate(servicemap_tcp_bytes_sent_total[1h]))
/
sum(rate(servicemap_tcp_bytes_sent_total[1h] offset 7d))
```

### HTTP 流量分析

```promql
# 各接口（method）的请求速率
sum by (source_name, destination, method) (
  rate(servicemap_http_requests_total[5m])
)

# HTTP 响应体积（带宽消耗）
sum by (source_name, destination) (
  rate(servicemap_http_bytes_received_total[5m])
) / 1024 / 1024  # MB/s

# 大请求体检测：平均每次请求接收字节数 > 1MB
sum by (source_name, destination, method) (
  rate(servicemap_http_bytes_received_total[5m])
)
/
sum by (source_name, destination, method) (
  rate(servicemap_http_requests_total[5m])
) > 1048576
```

### 容量规划报告模板

以下 Grafana Panel 组合可构成一份完整的容量规划仪表板：

| Panel | 查询 | 用途 |
|-------|------|------|
| 总带宽趋势 | `sum(rate(edge_bytes_sent_total[5m]))` | 整体带宽趋势 |
| Top 10 调用边带宽 | `topk(10, sum by (source_name,destination)(rate(edge_bytes_sent_total[5m])))` | 最重要的流量路径 |
| 数据库连接数 | `sum by (destination)(active_connections{destination=~".*:3306\|.*:5432"})` | 数据库连接压力 |
| HTTP QPS | `sum(rate(http_requests_total[5m]))` | 整体 HTTP 吞吐 |
| HTTP 平均延迟 | `rate(_sum[5m]) / rate(_count[5m]) * 1000` | 接口性能基线 |
| 插件自身容量 | `tracer_active_connections`, `tracked_containers` | 插件运行健康度 |

---

## 场景六：安全审计——未知连接发现

### 背景

在生产环境中，意外的服务间连接可能意味着：

- **配置错误**：服务连到了错误的环境（如 dev 服务连接 prod 数据库）
- **安全漏洞**：被攻击后建立的反向连接或横向渗透
- **依赖蔓延**：某服务悄悄开始调用了不该调用的下游
- **合规风险**：数据库被未授权服务访问

`servicemap` 持续记录所有 TCP 连接，天然适合作为**东西向流量审计**的数据来源。

### 安全审计核心查询

```promql
# 列出所有当前活跃的服务调用边（审计基线）
servicemap_edge_active_connections > 0

# 发现连接到敏感端口的服务
servicemap_edge_active_connections{
  destination_port=~"3306|5432|6379|27017|9092"
} > 0

# 发现向外部 IP 建立连接的容器（目标不在内网段）
# 需结合 ignore_cidrs 配置使用；内网 CIDR 外的连接即为潜在外连
servicemap_edge_active_connections{
  destination_host!~"^10\\.|^172\\.(1[6-9]|2[0-9]|3[01])\\.|^192\\.168\\."
} > 0

# 发现某时间段内新出现的连接（与基线对比）
# 基线：过去 24 小时从未出现的服务对，当前却有连接
(
  servicemap_edge_active_connections > 0
)
unless
(
  sum_over_time(servicemap_edge_active_connections[24h]) > 0
)
```

### 建立安全基线并告警

**步骤一：导出当前连接白名单**

```bash
# 通过 Graph API 获取当前所有合法连接
curl http://localhost:9099/graph | jq '
  .edges[] | {
    source: .source,
    target: .target,
    target_host: .target_host,
    target_port: .target_port,
    protocols: .protocols
  }
' > approved_connections_baseline.json
```

**步骤二：配置告警规则**

```yaml
groups:
  - name: servicemap.security
    rules:
      # 连接到数据库端口的新服务出现
      - alert: UnexpectedDatabaseAccess
        expr: |
          sum by (source_name, destination, destination_port) (
            servicemap_edge_active_connections{
              destination_port=~"3306|5432|6379|27017"
            }
          ) > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "发现对数据库的连接：{{ $labels.source_name }} → {{ $labels.destination }}"
          description: "服务 {{ $labels.source_name }} 正在访问数据库端口 {{ $labels.destination_port }}，请确认是否合法"

      # 连接到敏感管理端口
      - alert: SensitivePortAccess
        expr: |
          servicemap_edge_active_connections{
            destination_port=~"22|2379|2380|10250|6443"
          } > 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "发现对敏感管理端口的连接"
          description: "{{ $labels.source_name }} 正在连接 {{ $labels.destination }}（端口 {{ $labels.destination_port }}），立即核查"

      # 连接到外部 IP（非内网）
      - alert: ExternalOutboundConnection
        expr: |
          servicemap_edge_active_connections{
            destination_host!~"^10\\.|^172\\.(1[6-9]|2[0-9]|3[01])\\.|^192\\.168\\.|^127\\."
          } > 0
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "发现对外部 IP 的出站连接"
          description: "{{ $labels.source_name }} 正在连接外部地址 {{ $labels.destination_host }}:{{ $labels.destination_port }}"
```

### 跨环境访问检测

```promql
# 检测 dev namespace 的服务是否在访问 prod 的数据库
# （需要 Namespace 标签正确配置）
servicemap_edge_active_connections{
  namespace="dev",
  destination=~".*prod.*|10\\.0\\.1\\..+"
} > 0
```

### 连接历史分析（结合长期存储）

```promql
# 过去 7 天出现过但今天消失的连接（可能是攻击者清理痕迹）
(
  sum_over_time(servicemap_edge_active_connections[7d] offset 1d) > 0
)
unless
(
  servicemap_edge_active_connections > 0
)

# 某容器在深夜（0~6点）建立的连接次数（异常时间窗口活动）
# 需配合 Grafana 时间过滤或 recording rule 实现
```

### Graph API 辅助安全审查

```bash
# 每日自动审查并与基线对比
#!/bin/bash
TODAY=$(curl -s http://localhost:9099/graph | jq -r '
  .edges[] | "\(.source) -> \(.target_host):\(.target_port)"
' | sort)

BASELINE=$(cat approved_connections_baseline.txt)

# 找出新增连接
NEW_CONNECTIONS=$(comm -23 <(echo "$TODAY") <(echo "$BASELINE"))

if [ -n "$NEW_CONNECTIONS" ]; then
  echo "⚠️  发现未授权的新连接："
  echo "$NEW_CONNECTIONS"
  # 发送告警（钉钉/飞书/PagerDuty）
fi
```

### 与 SIEM 系统集成

Graph API 的 JSON 输出可以直接被 Elasticsearch/Splunk 等 SIEM 系统消费：

```bash
# 定时推送拓扑快照到 Elasticsearch
curl http://localhost:9099/graph | \
  curl -X POST "http://elasticsearch:9200/servicemap-$(date +%Y%m%d)/_doc" \
  -H "Content-Type: application/json" \
  -d @-
```

---

---

## 场景七：AI 辅助运维（Graph API + LLM）

### 背景

随着 AI Agent 和大语言模型（LLM）在运维领域的普及，工程师开始让 AI 自动分析系统状态、定位故障根因。但 LLM 需要**结构化的上下文输入**，而传统监控系统的数据（时序指标、日志）格式复杂，难以直接喂给模型。

`servicemap` 内置 **Graph API**，提供两种 AI 友好的数据格式：

- **`/graph`**：标准 JSON，供程序化 AI Agent 解析
- **`/graph/text`**：纯文本，直接嵌入 LLM Prompt，无需预处理

### Graph API 接口说明

| 接口 | 格式 | 适用场景 |
|------|------|---------|
| `GET /graph` | JSON | AI Agent、自动化脚本、前端渲染 |
| `GET /graph/text` | 纯文本 | 直接插入 LLM Prompt |
| `GET /health` | 纯文本 `ok` | 健康检查、存活探针 |

#### 启用配置

```toml
[[instances]]
interval = 30
enable_tcp  = true
enable_http = true
api_addr    = ":9099"   # 启用 Graph API
```

### `/graph` JSON 格式解析

```bash
curl http://localhost:9099/graph | jq .
```

```json
{
  "generated_at": "2026-03-02T10:00:00Z",
  "summary": {
    "nodes": 8,
    "edges": 12,
    "tracer_active_connections": 45,
    "tracer_listen_ports": 8,
    "tracked_containers": 8
  },
  "nodes": [
    {
      "id": "abc123",
      "name": "api-gateway",
      "namespace": "production",
      "pod_name": "api-gateway-7d8f9b-xk9p2",
      "image": "mycompany/api-gateway:v1.2.3",
      "labels": {"app": "api-gateway", "version": "v1.2.3"}
    }
  ],
  "edges": [
    {
      "id": "abc123->10.0.1.5:8080",
      "source": "abc123",
      "target": "10.0.1.5:8080",
      "target_host": "10.0.1.5",
      "target_port": "8080",
      "protocols": ["TCP", "HTTP"],
      "tcp": {
        "connects_total": 1240,
        "connect_failed_total": 0,
        "active_connections": 8,
        "retransmits_total": 2,
        "bytes_sent_total": 15728640,
        "bytes_received_total": 44040192,
        "avg_connect_duration_ms": 0.8
      },
      "http": [
        {
          "method": "GET",
          "status_code": 200,
          "status_class": "2xx",
          "requests_total": 1200,
          "errors_total": 0,
          "avg_duration_ms": 12.5
        },
        {
          "method": "POST",
          "status_code": 500,
          "status_class": "5xx",
          "requests_total": 3,
          "errors_total": 3,
          "avg_duration_ms": 245.0
        }
      ]
    }
  ]
}
```

### `/graph/text` 纯文本格式（直接用于 LLM Prompt）

```bash
curl http://localhost:9099/graph/text
```

```
=== Service Map @ 2026-03-02T10:00:00Z ===

Summary: 8 nodes, 12 edges | tracer active=45 listen=8 tracked=8

Nodes (8):
  [abc123] name=api-gateway ns=production pod=api-gateway-7d8f9b-xk9p2 image=mycompany/api-gateway:v1.2.3
  [def456] name=user-service ns=production pod=user-service-5c6d7e image=mycompany/user-service:v2.1.0
  [ghi789] name=order-service ns=production pod=order-service-3a4b5c image=mycompany/order-service:v1.8.1
  ...

Edges (12):
  abc123 -> 10.0.1.5:8080  [TCP, HTTP]
    TCP: connects=1240 failed=0 active=8 retx=2 sent=15000000B recv=44000000B avg_connect=0.8ms
    HTTP GET 200(2xx): req=1200 err=0 avg=12.5ms
    HTTP POST 500(5xx): req=3 err=3 avg=245.0ms

  def456 -> 10.0.2.10:3306  [TCP, MySQL]
    TCP: connects=850 failed=0 active=5 retx=0 sent=2000000B recv=8000000B avg_connect=0.9ms
    L7 MySQL[ok]: req=42000 err=0 avg=8.2ms

  ghi789 -> 10.0.3.20:6379  [TCP, Redis]
    TCP: connects=320 failed=0 active=2 retx=0 sent=500000B recv=1200000B avg_connect=0.3ms
    L7 Redis[ok]: req=95000 err=12 avg=0.8ms
    L7 Redis[error]: req=12 err=12 avg=1.1ms
```

### 与 LLM 集成：故障分析 Prompt 模板

```python
import requests

def analyze_servicemap_with_llm(llm_client):
    # 获取当前拓扑文本
    graph_text = requests.get("http://localhost:9099/graph/text").text

    prompt = f"""你是一位经验丰富的 SRE 工程师。以下是当前生产环境的服务拓扑快照，
请分析并回答：

1. 是否存在异常的错误率？（HTTP 5xx、L7 协议错误）
2. 是否存在高延迟的调用链路？
3. 是否存在 TCP 重传或连接失败？
4. 哪些服务是核心瓶颈节点（入度/出度最高）？
5. 给出优先需要关注的 Top 3 风险项。

=== 服务拓扑 ===
{graph_text}

请用中文输出分析报告。"""

    response = llm_client.chat(prompt)
    return response
```

### 与 AI Agent 集成：自动故障定位

```python
import requests, json

class ServiceMapTool:
    """供 AI Agent 调用的服务拓扑工具"""

    name = "get_service_map"
    description = "获取当前生产环境的服务拓扑图，包含节点、连接、协议、错误率和延迟信息"

    def run(self, format: str = "json"):
        if format == "text":
            return requests.get("http://localhost:9099/graph/text").text
        else:
            return requests.get("http://localhost:9099/graph").json()

class HighErrorEdgesTool:
    """过滤出高错误率的调用边"""

    name = "find_high_error_edges"
    description = "找出错误率超过阈值的服务调用边"

    def run(self, threshold: float = 0.01):
        graph = requests.get("http://localhost:9099/graph").json()
        problematic = []
        for edge in graph["edges"]:
            for h in edge.get("http", []):
                if h["requests_total"] > 0:
                    err_rate = h["errors_total"] / h["requests_total"]
                    if err_rate > threshold:
                        problematic.append({
                            "edge": edge["id"],
                            "method": h["method"],
                            "status_code": h["status_code"],
                            "error_rate": f"{err_rate:.1%}",
                            "avg_duration_ms": h["avg_duration_ms"]
                        })
        return problematic

# Agent 工具注册（以 LangChain 风格为例）
tools = [ServiceMapTool(), HighErrorEdgesTool()]
```

### 定时拓扑快照与趋势分析

```bash
#!/bin/bash
# 每分钟保存一次拓扑快照，供事后分析
SNAPSHOT_DIR="/var/log/servicemap-snapshots"
mkdir -p "$SNAPSHOT_DIR"

while true; do
    TIMESTAMP=$(date +%Y%m%d_%H%M%S)
    curl -s http://localhost:9099/graph \
        > "$SNAPSHOT_DIR/graph_${TIMESTAMP}.json"
    sleep 60
done
```

```python
# 对比两个时间点的拓扑变化
import json

def diff_graphs(snapshot_before: str, snapshot_after: str):
    before = json.load(open(snapshot_before))
    after  = json.load(open(snapshot_after))

    before_edges = {e["id"] for e in before["edges"]}
    after_edges  = {e["id"] for e in after["edges"]}

    new_edges     = after_edges  - before_edges
    removed_edges = before_edges - after_edges

    print(f"新增连接: {new_edges}")
    print(f"消失连接: {removed_edges}")
```

---

## 场景八：混沌工程与容错验证

### 背景

混沌工程（Chaos Engineering）通过主动注入故障来验证系统的容错能力。但注入故障后，需要观测手段来确认：

- 故障是否按预期传播？
- 熔断器是否正确触发？
- 降级策略是否生效？
- 故障恢复后拓扑是否恢复正常？

`servicemap` 提供实时的连接状态和 L7 指标，是混沌实验的理想观测工具。

### 典型混沌实验观测流程

```
实验前                    注入故障                   恢复后
  │                          │                          │
  ▼                          ▼                          ▼
记录基线拓扑           观测故障传播             验证拓扑恢复
graph/text snapshot    tcp_connect_failed↑      active_connections恢复
edge_active_connections  retransmits↑           http错误率归零
http_requests正常       http_5xx↑               基线对比一致
```

### 实验一：Kill 某个下游服务

**注入**：停止 `user-service` 容器。

**观测查询**：
```promql
# 立即检测到连接失败
sum by (source_name, destination) (
  rate(servicemap_tcp_connect_failed_total[1m])
) > 0

# HTTP 错误率飙升（调用方视角）
sum by (source_name, destination) (
  rate(servicemap_http_request_errors_total[1m])
)
/
sum by (source_name, destination) (
  rate(servicemap_http_requests_total[1m])
) > 0.5

# 活跃连接数下降到 0
servicemap_edge_active_connections{
  destination=~".*:8080"   # user-service 端口
} == 0
```

**验证熔断器**：
```promql
# 熔断后调用方应停止发起连接（connects_total 不再增加）
increase(servicemap_tcp_connects_total{
  destination=~".*:8080"
}[1m]) == 0
```

### 实验二：网络延迟注入

**注入**：使用 `tc netem` 给某容器网卡增加 200ms 延迟。

```bash
# 注入 200ms 延迟
tc qdisc add dev eth0 root netem delay 200ms
```

**观测查询**：
```promql
# TCP 连接建立时延应升高
(
  rate(servicemap_tcp_connect_duration_seconds_sum[1m])
  /
  rate(servicemap_tcp_connect_duration_seconds_count[1m])
) * 1000 > 150

# HTTP 请求平均延迟应升高
(
  rate(servicemap_http_request_duration_seconds_sum[1m])
  /
  rate(servicemap_http_request_duration_seconds_count[1m])
) * 1000

# 超时导致的 HTTP 5xx 比例
rate(servicemap_http_request_errors_total{status_class="5xx"}[1m])
/
rate(servicemap_http_requests_total[1m])
```

### 实验三：网络丢包注入

**注入**：模拟 10% 丢包率。

```bash
tc qdisc add dev eth0 root netem loss 10%
```

**观测查询**：
```promql
# 重传率应显著上升
sum by (source_id, destination) (
  rate(servicemap_tcp_retransmits_total[1m])
)

# 吞吐量下降（bytes/s）
rate(servicemap_tcp_bytes_sent_total[1m])
```

### 实验四：数据库连接池耗尽

**注入**：持续建立 MySQL 连接但不释放。

**观测查询**：
```promql
# 到 MySQL 的活跃连接数持续增长
sum by (destination) (
  servicemap_tcp_active_connections{
    destination=~".*:3306"
  }
)

# MySQL 请求延迟飙升（等待连接）
(
  rate(servicemap_mysql_request_duration_seconds_sum[1m])
  /
  rate(servicemap_mysql_request_duration_seconds_count[1m])
) * 1000 > 500

# MySQL 错误率上升（连接超时）
rate(servicemap_mysql_request_errors_total[1m])
/
rate(servicemap_mysql_requests_total[1m]) > 0.1
```

### 混沌实验自动化检查脚本

```bash
#!/bin/bash
# chaos_verify.sh — 混沌实验前后的拓扑完整性检查

GRAPH_API="http://localhost:9099"

echo "=== 实验前基线 ==="
BEFORE=$(curl -s "$GRAPH_API/graph/text")
BEFORE_EDGES=$(echo "$BEFORE" | grep "^  " | grep "\->" | wc -l)
echo "活跃连接边数：$BEFORE_EDGES"

echo ""
echo "=== 注入故障（等待 30s 观测） ==="
sleep 30

echo ""
echo "=== 故障期间状态 ==="
DURING=$(curl -s "$GRAPH_API/graph/text")
DURING_EDGES=$(echo "$DURING" | grep "^  " | grep "\->" | wc -l)
echo "活跃连接边数：$DURING_EDGES"
echo "减少的连接边：$((BEFORE_EDGES - DURING_EDGES))"

echo ""
echo "=== 恢复后验证（等待 60s） ==="
sleep 60
AFTER=$(curl -s "$GRAPH_API/graph/text")
AFTER_EDGES=$(echo "$AFTER" | grep "^  " | grep "\->" | wc -l)
echo "恢复后连接边数：$AFTER_EDGES"

if [ "$AFTER_EDGES" -ge "$BEFORE_EDGES" ]; then
  echo "✅ 拓扑完全恢复"
else
  echo "❌ 拓扑未完全恢复，缺失 $((BEFORE_EDGES - AFTER_EDGES)) 条连接边"
  exit 1
fi
```

---

## 场景九：非 Linux 开发环境（轮询模式）

### 背景

eBPF 仅支持 Linux，但开发团队通常在 macOS 或 Windows 上工作。`servicemap` 在非 Linux 平台上会自动降级为**轮询模式**，使用 `gopsutil` 从 `/proc` 或系统 API 获取网络连接信息，保持大部分功能可用。

### 轮询模式 vs eBPF 模式对比

| 特性 | eBPF 模式（Linux） | 轮询模式（macOS/非 Linux） |
|------|------------------|--------------------------|
| 平台要求 | Linux >= 4.16 | 任意平台 |
| 权限要求 | root / CAP_SYS_ADMIN | 普通用户（部分功能需 root） |
| TCP 连接追踪 | ✅ 实时内核事件 | ✅ 定期轮询 `/proc/net/tcp` |
| L7 协议解析 | ✅ HTTP/MySQL/Postgres/Redis/Kafka | ❌ 不支持 |
| 容器发现 | ✅ Docker + K8s + cgroup | ✅ Docker（需 Docker Desktop） |
| 字节流量统计 | ✅ | ✅（部分） |
| 连接时延 | ✅ 精确 | ❌ 不支持 |
| 性能开销 | 极低（内核级） | 低（用户态轮询） |

### macOS 本地开发配置

```toml
[[instances]]
interval = 15              # 轮询间隔可适当缩短

enable_tcp   = true
enable_http  = false       # macOS 不支持 L7 解析

# L7 tracing 在非 Linux 自动禁用，此处可显式关闭
disable_l7_tracing = true

ignore_ports = [22]
ignore_cidrs = ["127.0.0.0/8"]

# Graph API 照常可用
api_addr = ":9099"

# Docker Desktop socket（macOS）
docker_socket_path = "/var/run/docker.sock"
```

### 在 macOS 上运行

```bash
# 编译（macOS 上自动跳过 eBPF 编译）
cd /path/to/categraf
go build -o categraf .

# 运行（无需 root，但 Docker socket 可能需要权限）
./categraf --inputs servicemap

# 日志中会看到：
# I! servicemap: netns is unsupported on darwin, running with polling fallback
# I! servicemap: graph API listening on http://:9099/graph
```

### 本地开发调试工作流

```bash
# 1. 启动本地 Docker Compose 服务
docker compose up -d

# 2. 启动 categraf（轮询模式自动生效）
./categraf --inputs servicemap

# 3. 查看本地服务拓扑
curl http://localhost:9099/graph/text

# 4. 检查健康状态
curl http://localhost:9099/health

# 5. 用 jq 过滤特定服务的连接
curl http://localhost:9099/graph | jq '
  .edges[] | select(.target_port == "3306")
  | {source: .source, target: .target, tcp: .tcp}
'
```

### Windows 上运行（WSL2）

在 Windows 上推荐通过 WSL2（Linux 子系统）运行，可以获得完整的 eBPF 支持：

```powershell
# 在 PowerShell 中启动 WSL2
wsl

# 进入 WSL2 后按 Linux 方式操作
uname -r   # 检查内核版本（WSL2 通常 >= 5.15）
sudo ./categraf --inputs servicemap
```

---

## 场景十：与 Prometheus / Grafana / VictoriaMetrics 集成

### 架构概览

```
categraf (servicemap)
        │
        │ remote_write / scrape
        ▼
┌───────────────────┐     ┌─────────────────────┐
│   Prometheus      │ 或  │  VictoriaMetrics     │
│   (短期存储)       │     │  (长期存储, 推荐)    │
└────────┬──────────┘     └──────────┬───────────┘
         │                           │
         ▼                           ▼
┌────────────────────────────────────────────────┐
│              Grafana                           │
│  - ServiceMap 仪表板                           │
│  - TCP 连接总览                                │
│  - HTTP/L7 协议延迟与错误率                    │
│  - 容量规划趋势图                              │
└────────────────────────────────────────────────┘
         │
         ▼
┌────────────────┐
│  AlertManager  │
│  告警规则      │
└────────────────┘
```

### categraf 输出配置

#### 输出到 Prometheus Remote Write

```toml
# conf/config.toml
[writer_opt]
  batch = 2000
  chan_size = 100000

[[writers]]
  url = "http://prometheus:9090/api/v1/write"
  timeout = 5000
  dial_timeout = 2500
  max_idle_conns_per_host = 100
```

#### 输出到 VictoriaMetrics（推荐用于长期存储）

```toml
[[writers]]
  url = "http://victoriametrics:8428/api/v1/write"
  timeout = 5000
```

#### 被 Prometheus Scrape（Pull 模式）

```toml
# 在 conf/config.toml 中开启内置 HTTP Server
[http]
  enable = true
  address = ":9100"
  print_access_log = false
```

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'categraf-servicemap'
    static_configs:
      - targets: ['categraf-host:9100']
    relabel_configs:
      - source_labels: [__address__]
        target_label: instance
```

### Grafana 仪表板设计

#### 推荐 Panel 布局

```
Row 1: 总览
┌──────────────┬──────────────┬──────────────┬──────────────┐
│  服务节点数   │  连接边数    │ 总活跃连接数  │ 追踪容器数   │
│ graph_nodes  │ graph_edges  │tracer_active │tracked_cont  │
└──────────────┴──────────────┴──────────────┴──────────────┘

Row 2: TCP 健康度
┌───────────────────────────┬───────────────────────────────┐
│  连接失败率（热力图）      │  TCP 重传速率（折线图）        │
│  tcp_connect_failed        │  tcp_retransmits_total         │
└───────────────────────────┴───────────────────────────────┘

Row 3: HTTP 性能
┌───────────────────────────┬───────────────────────────────┐
│  HTTP 请求速率（QPS）      │  HTTP 平均延迟（ms）           │
│  http_requests_total       │  _duration_sum / _count        │
└───────────────────────────┴───────────────────────────────┘

Row 4: 数据库
┌──────────────┬──────────────┬──────────────┬──────────────┐
│ MySQL QPS    │ MySQL 延迟   │ Redis QPS    │ Redis 延迟   │
└──────────────┴──────────────┴──────────────┴──────────────┘

Row 5: 容量
┌───────────────────────────┬───────────────────────────────┐
│  Top 10 带宽调用边         │  数据库连接数趋势              │
│  edge_bytes_sent_total     │  tcp_active_connections        │
└───────────────────────────┴───────────────────────────────┘
```

#### 关键 Grafana 变量

```
$namespace   = label_values(servicemap_edge_active_connections, namespace)
$source      = label_values(servicemap_edge_active_connections, source_name)
$destination = label_values(servicemap_edge_active_connections, destination_host)
$source_type = [bare_process, container]
$protocol    = [mysql, postgres, redis, kafka]
$interval    = [1m, 5m, 15m, 1h]
```

### 完整 AlertManager 规则集

```yaml
groups:
  - name: servicemap
    interval: 30s
    rules:
      # ── TCP 层 ──────────────────────────────────────────
      - alert: TCPConnectFailureHigh
        expr: |
          sum by (source_name, destination) (
            rate(servicemap_tcp_connect_failed_total[5m])
          ) > 1
        for: 2m
        labels:
          severity: warning
          category: network
        annotations:
          summary: "TCP 连接失败率高：{{ $labels.source_name }} → {{ $labels.destination }}"

      - alert: TCPRetransmitHigh
        expr: |
          sum by (source_id, destination) (
            rate(servicemap_tcp_retransmits_total[5m])
          ) > 5
        for: 3m
        labels:
          severity: warning
          category: network

      # ── HTTP 层 ──────────────────────────────────────────
      - alert: HTTPErrorRateHigh
        expr: |
          sum by (source_name, destination, method) (
            rate(servicemap_http_request_errors_total[5m])
          )
          /
          sum by (source_name, destination, method) (
            rate(servicemap_http_requests_total[5m])
          ) > 0.05
        for: 2m
        labels:
          severity: critical
          category: application

      - alert: HTTPLatencyHigh
        expr: |
          (
            sum by (source_name, destination) (
              rate(servicemap_http_request_duration_seconds_sum[5m])
            )
            /
            sum by (source_name, destination) (
              rate(servicemap_http_request_duration_seconds_count[5m])
            )
          ) * 1000 > 500
        for: 5m
        labels:
          severity: warning
          category: performance

      # ── 数据库层 ──────────────────────────────────────────
      - alert: MySQLLatencyHigh
        expr: |
          (
            rate(servicemap_mysql_request_duration_seconds_sum[5m])
            /
            rate(servicemap_mysql_request_duration_seconds_count[5m])
          ) * 1000 > 100
        for: 5m
        labels:
          severity: warning
          category: database

      - alert: RedisLatencyHigh
        expr: |
          (
            rate(servicemap_redis_request_duration_seconds_sum[5m])
            /
            rate(servicemap_redis_request_duration_seconds_count[5m])
          ) * 1000 > 10
        for: 3m
        labels:
          severity: warning
          category: database

      # ── 插件自身健康 ──────────────────────────────────────
      - alert: ServiceMapTrackerOverload
        expr: |
          servicemap_tracer_active_connections > 40000
        for: 5m
        labels:
          severity: warning
          category: agent
        annotations:
          summary: "ServiceMap Tracer 追踪连接数接近上限（当前 {{ $value }}，上限 50000）"
          description: "考虑增大 max_tracked_connections 或添加 ignore_cidrs 过滤非重要流量"
```

### VictoriaMetrics 长期存储最佳实践

```yaml
# victoriametrics 启动参数（长期保留）
-retentionPeriod=12        # 保留 12 个月
-storageDataPath=/data/vm

# 推荐 Recording Rules（预聚合，提升查询性能）
groups:
  - name: servicemap_recording
    interval: 1m
    rules:
      - record: job:servicemap_http_error_rate:5m
        expr: |
          sum by (source_name, destination) (
            rate(servicemap_http_request_errors_total[5m])
          )
          /
          sum by (source_name, destination) (
            rate(servicemap_http_requests_total[5m])
          )

      - record: job:servicemap_http_latency_ms:5m
        expr: |
          (
            sum by (source_name, destination) (
              rate(servicemap_http_request_duration_seconds_sum[5m])
            )
            /
            sum by (source_name, destination) (
              rate(servicemap_http_request_duration_seconds_count[5m])
            )
          ) * 1000

      - record: job:servicemap_edge_bandwidth_mbps:5m
        expr: |
          sum by (source_name, destination_host, destination_port) (
            rate(servicemap_edge_bytes_sent_total[5m])
            +
            rate(servicemap_edge_bytes_received_total[5m])
          ) / 1048576
```

---

## 附录：完整配置参考

### 完整 TOML 配置模板

```toml
# conf/input.servicemap/servicemap.toml

# 采集间隔（秒）
# 推荐：生产环境 60s，调试时可缩短到 15s
interval = 60

[[instances]]
# ── 功能开关 ───────────────────────────────────────────────
# 启用 TCP 连接追踪（建议始终开启）
enable_tcp = true

# 启用 HTTP L7 解析（需要 eBPF，对 CPU 有轻微影响）
enable_http = true

# 启用 cgroup 容器发现（推荐开启）
enable_cgroup = true

# 禁用 L7 追踪（关闭后不采集 HTTP/MySQL/Postgres/Redis/Kafka 层指标）
# 在 CPU 敏感环境或 eBPF 不支持时可开启
disable_l7_tracing = false

# ── 过滤配置 ───────────────────────────────────────────────
# 忽略的端口（不追踪这些端口的连接）
# 建议加入：SSH(22)、监控自身端口(9100)、K8s 内部端口(10250)
ignore_ports = [22, 9100, 10250, 10255, 2379, 2380]

# 忽略的 CIDR（不追踪这些网段的连接）
# 建议至少忽略本地回环
ignore_cidrs = ["127.0.0.0/8", "169.254.0.0/16"]

# Docker label 白名单：只有列出的 label key 才会透传为 Prometheus 标签
# 留空则不透传任何 Docker label（推荐，防止高基数标签导致时序爆炸）
# label_allowlist = ["app", "version", "team"]

# ── 容器发现 ───────────────────────────────────────────────
# Docker socket 路径（留空使用默认 /var/run/docker.sock）
docker_socket_path = ""

# Kubernetes 配置文件路径（留空使用 in-cluster config）
# 仅在集群外运行时需要指定
kubeconfig_path = ""

# ── 资源限制 ───────────────────────────────────────────────
# 最大追踪连接数（超出后旧连接被 GC）
max_tracked_connections = 50000

# 最大追踪容器数
max_containers = 5000

# ── Graph API ──────────────────────────────────────────────
# 内嵌 HTTP API 地址（留空不启动）
# 启用后提供：
#   GET /graph       → JSON 格式服务拓扑
#   GET /graph/text  → 文本格式（适合 AI/人工阅读）
#   GET /health      → 健康检查
api_addr = ":9099"

# ── 附加标签 ───────────────────────────────────────────────
[instances.labels]
  env     = "production"
  cluster = "cluster-01"
  region  = "cn-beijing"
```

### 指标命名速查表

| 指标名 | 类型 | 维度标签 |
|--------|------|---------|
| `servicemap_tcp_connects_total` | Counter | `source_id`, `source_name`, `source_type`, `destination` |
| `servicemap_tcp_connect_failed_total` | Counter | 同上 |
| `servicemap_tcp_retransmits_total` | Counter | 同上 |
| `servicemap_tcp_bytes_sent_total` | Counter | 同上 |
| `servicemap_tcp_bytes_received_total` | Counter | 同上 |
| `servicemap_tcp_connect_duration_seconds_sum` | Counter | 同上 |
| `servicemap_tcp_connect_duration_seconds_count` | Counter | 同上 |
| `servicemap_tcp_active_connections` | Gauge | 同上 |
| `servicemap_http_requests_total` | Counter | `source_id`, `source_name`, `source_type`, `destination`, `method`, `status_code`, `status_class` |
| `servicemap_http_request_errors_total` | Counter | 同上 |
| `servicemap_http_request_duration_seconds_sum` | Counter | 同上 |
| `servicemap_http_request_duration_seconds_count` | Counter | 同上 |
| `servicemap_http_bytes_sent_total` | Counter | 同上 |
| `servicemap_http_bytes_received_total` | Counter | 同上 |
| `servicemap_mysql_requests_total` | Counter | `source_id`, `source_name`, `source_type`, `destination`, `protocol`, `status` |
| `servicemap_mysql_request_errors_total` | Counter | 同上 |
| `servicemap_mysql_request_duration_seconds_sum` | Counter | 同上 |
| `servicemap_mysql_request_duration_seconds_count` | Counter | 同上 |
| `servicemap_postgres_*` | 同 MySQL | 同上 |
| `servicemap_redis_*` | 同 MySQL | 同上 |
| `servicemap_kafka_*` | 同 MySQL | 同上 |
| `servicemap_edge_connects_total` | Counter | `source_id`, `source_name`, `source_type`, `destination`, `destination_host`, `destination_port`, `namespace`*, `pod_name`* |
| `servicemap_edge_connect_failed_total` | Counter | 同上 |
| `servicemap_edge_retransmits_total` | Counter | 同上 |
| `servicemap_edge_bytes_sent_total` | Counter | 同上 |
| `servicemap_edge_bytes_received_total` | Counter | 同上 |
| `servicemap_edge_active_connections` | Gauge | 同上 |
| `servicemap_graph_nodes` | Gauge | `source_type`, `kube_node`* |
| `servicemap_graph_edges` | Gauge | `source_type`, `kube_node`* |
| `servicemap_tracer_active_connections` | Gauge | （无） |
| `servicemap_tracer_listen_ports` | Gauge | （无） |
| `servicemap_tracked_containers` | Gauge | （无） |
| `servicemap_host_active_connections` | Gauge | `host` |
| `servicemap_host_bytes_sent_total` | Counter | `host` |
| `servicemap_host_bytes_received_total` | Counter | `host` |

> *标有 `*` 的标签为条件输出：`namespace`/`pod_name` 仅在 K8s 场景输出；`kube_node` 仅在环境变量 `NODE_NAME` 存在时输出。`cluster` 标签通过 `[instances.labels]` 配置注入，框架自动附加到所有指标，无需插件特殊处理。

---

*文档版本：v1.2 · 2026-03-03 · 标签命名统一（source_id/source_name/source_type）；graph_nodes/edges 增加 source_type/kube_node；新增 label_allowlist 配置；裸进程格式更新为 proc_<进程名>*
