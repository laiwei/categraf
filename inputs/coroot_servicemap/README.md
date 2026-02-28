# Coroot Service Map Plugin

## 简介

`coroot_servicemap` 插件使用 eBPF 技术跟踪 TCP 连接和 HTTP 请求，自动构建服务间的调用关系图（Service Map）。它可以帮助你：

- 🔍 自动发现服务间的依赖关系
- 📊 监控服务间的网络流量和连接状态
- 🐛 快速定位网络通信问题
- 🎯 支持容器和 Kubernetes 环境

本插件基于 [coroot-node-agent](https://github.com/coroot/coroot-node-agent) 的核心功能移植而来。

## 系统要求

### 必需条件

- **Linux 内核**: >= 4.16 (推荐 5.1+)
- **权限**: root 或 CAP_SYS_ADMIN capability
- **架构**: amd64 或 arm64

### 内核功能要求

检查内核是否支持 eBPF:

```bash
# 检查内核版本
uname -r

# 检查 BPF 功能
zgrep CONFIG_BPF /proc/config.gz
zgrep CONFIG_BPF_SYSCALL /proc/config.gz
```

### 容器环境

如果在容器中运行，需要：

```yaml
# Docker
docker run --privileged \
  -v /sys/fs/cgroup:/sys/fs/cgroup:ro \
  -v /proc:/host/proc:ro \
  ...

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

创建配置文件 `conf/input.coroot_servicemap/servicemap.toml`:

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
```

## 采集的指标

### 容器信息

```
coroot_servicemap_container_info{container_id, container_name, pod_name, namespace, image}
```

### TCP 连接指标

```
# 成功连接次数
coroot_servicemap_tcp_successful_connects_total{container_id, destination, actual_destination}

# 连接总时长（秒）
coroot_servicemap_tcp_connection_time_seconds_total{container_id, destination, actual_destination}

# 当前活跃连接数
coroot_servicemap_tcp_active_connections{container_id, destination, actual_destination}

# 发送字节数
coroot_servicemap_tcp_bytes_sent_total{container_id, destination, actual_destination}

# 接收字节数
coroot_servicemap_tcp_bytes_received_total{container_id, destination, actual_destination}

# TCP 重传次数
coroot_servicemap_tcp_retransmissions_total{container_id, destination, actual_destination}
```

### HTTP 请求指标

```
# HTTP 请求总数
coroot_servicemap_http_requests_total{container_id, destination, status, method}

# HTTP 请求总时长（秒）
coroot_servicemap_http_request_duration_seconds_total{container_id, destination, status, method}
```

## 使用示例

### 查询服务调用关系

```promql
# 查看某个服务的出站连接
coroot_servicemap_tcp_active_connections{pod_name="my-app"}

# 查看服务间的 HTTP 调用
rate(coroot_servicemap_http_requests_total[5m])

# 查看连接失败率
rate(coroot_servicemap_tcp_failed_connects_total[5m]) 
  / 
rate(coroot_servicemap_tcp_successful_connects_total[5m])
```

### Grafana 可视化

推荐使用 [Coroot](https://github.com/coroot/coroot) 进行可视化，它提供了开箱即用的 Service Map 视图。

## 故障排查

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

**当前状态**: 🚧 开发中

- [x] 基础框架
- [x] 插件接口实现
- [x] 配置文件设计
- [ ] eBPF 程序编译和加载
- [ ] TCP 连接完整跟踪
- [ ] HTTP 请求解析
- [ ] 容器元数据提取
- [ ] 完整测试

## 参考资料

- [Coroot Node Agent](https://github.com/coroot/coroot-node-agent)
- [eBPF Documentation](https://ebpf.io/)
- [Cilium eBPF Library](https://github.com/cilium/ebpf)

## 许可证

Apache License 2.0
