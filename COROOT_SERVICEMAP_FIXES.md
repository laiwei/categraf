# Coroot ServiceMap Plugin - macOS 兼容性修复总结

## 问题描述

在 macOS 上运行 Categraf 时，coroot_servicemap 插件遇到以下问题：

1. **初始化错误**: `failed to get host network namespace: not implemented`
2. **警告日志**: 多个关于 netns 和 tracefs 的警告日志（在非 Linux 平台上不适用）
3. **轮询模式兼容性**: macOS 上没有 eBPF，需要使用轮询模式，但连接文件描述符处理有问题
4. **测试模式输出缺失**: `./categraf --test --inputs coroot_servicemap` 没有输出任何指标

## 修复方案

### 1. 平台检测与优雅降级

#### 文件: [inputs/coroot_servicemap/instance.go](inputs/coroot_servicemap/instance.go)

**修改内容**:
- 在 `Init()` 方法中添加 `runtime.GOOS` 检查
- 当运行在非 Linux 平台时，跳过 netns 初始化，改用轮询模式

```go
if runtime.GOOS != "linux" {
    log.Printf("I! coroot_servicemap: netns is unsupported on %s, running with polling fallback", runtime.GOOS)
    // 使用轮询模式...
}
```

#### 文件: [inputs/coroot_servicemap/tracer/tracer.go](inputs/coroot_servicemap/tracer/tracer.go)

**修改内容**:
- 在 `Start()` 方法中添加 `runtime.GOOS` 检查
- 当非 Linux 时，直接启动轮询模式，不尝试加载 eBPF 模块

### 2. 轮询模式增强

#### 文件: [inputs/coroot_servicemap/tracer/tracer.go](inputs/coroot_servicemap/tracer/tracer.go)

**问题**: macOS 上通过 gopsutil 获取的连接对象中，`fd` 字段通常为 0，导致连接被过滤掉

**解决方案**:
- 添加 `connectionFD()` 函数，使用 FNV-1a 哈希生成稳定的连接 ID
  - 基于 (pid, laddr.IP, laddr.Port, raddr.IP, raddr.Port) 计算
  - 在真实 fd 不可用时使用哈希值
  
- 修改 `isTrackedTCPConnection()` 函数
  - 移除 `fd > 0` 的硬性要求（macOS 上总是为 0）
  - 改为检查连接状态和目的地址有效性

```go
func connectionFD(c gopsnet.ConnectionStat) uint64 {
    // 如果有真实fd，直接使用
    if c.Fd > 0 {
        return c.Fd
    }
    // 否则根据连接五元组计算哈希
    h := fnv.New64a()
    h.Write([]byte(strconv.FormatUint(uint64(c.Pid), 10)))
    h.Write([]byte(c.Laddr.IP.String()))
    // ... 计算完整哈希
    return h.Sum64()
}
```

### 3. Gather 方法签名修复

#### 文件: [inputs/coroot_servicemap/instance.go](inputs/coroot_servicemap/instance.go)

**问题**: `Gather()` 方法签名不符合 `SampleGatherer` 接口，导致未被调用

**修改**:
- 改从: `func (ins *Instance) Gather(slist *types.SampleList) error`
- 改为: `func (ins *Instance) Gather(slist *types.SampleList)`

Categraf 框架期望的接口签名不返回 error。

### 4. 主机级别指标收集

#### 文件: [inputs/coroot_servicemap/instance.go](inputs/coroot_servicemap/instance.go)

**添加功能**: `collectHostStats()` 方法

当没有容器被发现时（常见于开发环境），仍然输出主机级别的统计：
- `coroot_servicemap_host_active_connections` - 活跃连接数
- `coroot_servicemap_host_bytes_sent_total` - 发送字节总数
- `coroot_servicemap_host_bytes_received_total` - 接收字节总数

### 5. 配置文件修复

#### 文件: [conf/input.coroot_servicemap/servicemap.toml](conf/input.coroot_servicemap/servicemap.toml)

**修改**:
- 将 `interval` 从 `[[instances]]` 部分移到顶级
- 改为 5 秒间隔（方便测试）

```toml
# 原来（错误）:
[[instances]]
interval = 5

# 现在（正确）:
interval = 5

[[instances]]
# ...
```

## 验证结果

### 测试命令
```bash
./categraf --test --inputs coroot_servicemap
```

### 预期输出
- ✅ 无初始化错误
- ✅ 无 netns/tracefs 警告
- ✅ 正确输出 coroot_servicemap_* 指标到 stdout

### 输出样例
```
1772293218 23:40:18 coroot_servicemap_host_active_connections agent_hostname=bogon host=local 0
1772293218 23:40:18 coroot_servicemap_tcp_active_connections agent_hostname=bogon container_id=unknown destination=116.128.169.13:8081 2
1772293218 23:40:18 coroot_servicemap_tcp_bytes_sent_total agent_hostname=bogon container_id=unknown destination=116.128.169.13:8081 0
```

## 关键技术要点

1. **跨平台兼容性**: 使用 `runtime.GOOS` 进行平台检测，避免平台特定功能失败
2. **优雅降级**: 当 eBPF 不可用时，自动使用轮询模式
3. **稳定标识**: 使用哈希值生成稳定的连接 ID，不依赖操作系统提供的 fd
4. **接口合规**: 确保实现方法签名与框架期望的接口相匹配
5. **测试友好**: 配置文件采用顶级 `interval` 参数，便于测试模式

## 文件修改列表

1. [inputs/coroot_servicemap/instance.go](inputs/coroot_servicemap/instance.go)
   - 添加平台检测
   - 修改 Gather() 签名
   - 添加 collectHostStats() 方法

2. [inputs/coroot_servicemap/tracer/tracer.go](inputs/coroot_servicemap/tracer/tracer.go)
   - 添加平台检测
   - 修改 isTrackedTCPConnection() 逻辑
   - 添加 connectionFD() 函数

3. [conf/input.coroot_servicemap/servicemap.toml](conf/input.coroot_servicemap/servicemap.toml)
   - 移动 interval 配置位置

4. [agent/metrics_reader.go](agent/metrics_reader.go)
   - 移除调试日志（临时，用于排查问题）
