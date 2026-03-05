# Topology Aggregator

从 Prometheus 中的 `servicemap_edge_*` 和 `servicemap_listen_endpoint` 数据，
在内存中 JOIN 出 **process/container → process/container** 的 P2P 拓扑图，
并支持 K8s Service IP 解析。

## 架构

```
categraf(Host-A)              categraf(Host-B)
     │                               │
     ▼                               ▼
  :9099/metrics              :9099/metrics
  servicemap_edge_*          servicemap_listen_endpoint
     │                               │
     └──────── Prometheus scrape ────┘
                      │
               topology-aggregator
                   ┌──┴──┐
                JOIN    K8s API
                   └──┬──┘
           ┌──────────┴──────────┐
           │                     │
     /api/v1/topology     Remote Write
     /api/v1/edges         (写回 Prometheus)
           │
       Grafana
```

> **数据来源**：topology-aggregator 通过 Prometheus 查询 `servicemap_edge_*` 和
> `servicemap_listen_endpoint` 指标。这两类指标由各主机上的 categraf 采集后推送到
> Prometheus，或通过 categraf 自带的 `api_addr = ":9099"` Graph API 直接
> scrape（`/metrics` 端点）。

## 快速启动

### 1. 编译

```bash
cd inputs/servicemap/topology-aggregator
go mod tidy
make build
```

### 2. 运行（裸机/VM 场景）

```bash
./topology-aggregator \
  --prometheus-url=http://prometheus:9090 \
  --listen=:9098 \
  --interval=60s
```

### 3. 运行（含 K8s Service IP 解析）

```bash
./topology-aggregator \
  --prometheus-url=http://prometheus:9090 \
  --listen=:9098 \
  --interval=60s \
  --enable-k8s \
  --kubeconfig=$HOME/.kube/config \
  --remote-write-url=http://prometheus:9090/api/v1/write
```

### 4. Docker

```bash
make docker VERSION=v1.0.0

docker run -d \
  -p 9098:9098 \
  flashcat/topology-aggregator:v1.0.0 \
  --prometheus-url=http://prometheus:9090 \
  --listen=:9098
```

### 5. Kubernetes DaemonSet/Deployment

作为 Deployment 运行（一个集群一个实例即可）：

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: topology-aggregator
  namespace: monitoring
spec:
  replicas: 1
  selector:
    matchLabels:
      app: topology-aggregator
  template:
    metadata:
      labels:
        app: topology-aggregator
    spec:
      serviceAccountName: topology-aggregator  # 需要 list services/endpoints 权限
      containers:
        - name: aggregator
          image: flashcat/topology-aggregator:v1.0.0
          args:
            - --prometheus-url=http://prometheus:9090
            - --listen=:9098
            - --interval=60s
            - --enable-k8s
            - --remote-write-url=http://prometheus:9090/api/v1/write
          ports:
            - containerPort: 9098
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: topology-aggregator
  namespace: monitoring
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: topology-aggregator
rules:
  - apiGroups: [""]
    resources: ["services", "endpoints"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: topology-aggregator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: topology-aggregator
subjects:
  - kind: ServiceAccount
    name: topology-aggregator
    namespace: monitoring
```

## HTTP API

### `GET /api/v1/topology`

返回完整拓扑快照（JSON）。

**查询参数（可选）：**
- `source_name=nginx` — 只返回源服务名匹配的边
- `dest_name=mysql` — 只返回目标服务名匹配的边
- `namespace=production` — 只返回目标命名空间匹配的边

**响应示例：**
```json
{
  "edges": [
    {
      "source_id": "proc_curl",
      "source_name": "curl",
      "source_type": "bare_process",
      "dest_source_id": "proc_nginx",
      "dest_source_name": "nginx",
      "dest_source_type": "bare_process",
      "dest_namespace": "production",
      "active_connections": 3,
      "destination_host": "10.0.2.15",
      "destination_port": "80",
      "updated_at": "2026-03-05T10:00:00Z"
    }
  ],
  "node_count": 2,
  "edge_count": 1,
  "updated_at": "2026-03-05T10:00:00Z"
}
```

> `dest_namespace`、`dest_pod_name`、`namespace`、`pod_name` 字段为 `omitempty`，
> 仅在启用 `--enable-k8s` 且成功解析时出现。

### `GET /api/v1/edges`

返回 Grafana Node Graph Panel 所需格式。

```json
{
  "nodes": [
    { "id": "proc_curl", "title": "curl", "mainStat": 3, "secondaryStat": "bare_process" },
    { "id": "proc_nginx", "title": "nginx", "mainStat": 0, "secondaryStat": "bare_process" }
  ],
  "edges": [
    { "id": "proc_curl->proc_nginx", "source": "proc_curl", "target": "proc_nginx", "mainStat": 3 }
  ]
}
```

### `GET /healthz`

健康检查。超过 3 个聚合周期未更新数据时返回 `503`。

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--prometheus-url` | `http://localhost:9090` | Prometheus HTTP API 地址 |
| `--remote-write-url` | `""` | Prometheus Remote Write 地址（为空则不写回） |
| `--listen` | `:9098` | HTTP API 监听地址 |
| `--interval` | `60s` | 聚合周期 |
| `--query-timeout` | `30s` | Prometheus 查询超时 |
| `--enable-k8s` | `false` | 启用 K8s Service IP 解析 |
| `--kubeconfig` | `""` | kubeconfig 路径（空则用 in-cluster） |

> **提示**：如果 categraf 配置了 `api_addr = ":9099"`，可在 Prometheus 中直接 scrape
> `http://<host>:9099/metrics`，无需额外推送配置。topology-aggregator 通过 Prometheus
> 查询这些数据，不直接访问 categraf。

## Remote Write 指标

启用 `--remote-write-url` 后，aggregator 会将每次 JOIN 结果写回 Prometheus：

```
servicemap_p2p_topology_active{
  source_name,        # 调用方服务名
  source_type,        # bare_process / container
  dest_source_name,   # 被调用方服务名（经 listen_endpoint JOIN 推断）
  dest_source_type,   # 被调用方节点类型
  dest_namespace,     # 被调用方 K8s 命名空间（非 K8s 时为空）
  generated_by="topology-aggregator"
} = <active_connections>
```

此指标可配合 Grafana Node Graph Panel 展示跨主机 P2P 拓扑图。

## 前置条件

在使用 topology-aggregator 前，请确认：

1. **categraf 已配置** `api_addr = ":9099"` 并采集到 `servicemap_edge_*` 和 `servicemap_listen_endpoint` 指标
2. **Prometheus 已 scrape** 所有目标主机的 `:9099/metrics`
3. 可用 `curl 'http://prometheus:9090/api/v1/query?query=servicemap_listen_endpoint' | jq .` 验证数据存在
