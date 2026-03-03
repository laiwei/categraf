# Coroot ServiceMap 插件 - 当前状态

## 📊 项目概况

将 coroot-node-agent 的服务拓扑图生成功能集成到 Categraf 作为插件。

**目标：** 通过 eBPF 技术采集 TCP 连接和 HTTP 流量，生成服务间调用关系图。

## ✅ 已完成工作

### 1. 插件框架搭建 (100%)
- ✅ 创建插件目录结构 `inputs/coroot_servicemap/`
- ✅ 实现 Categraf Input 接口 (`servicemap.go`)
- ✅ 注册到 Categraf 插件系统 (`agent/metrics_agent.go`)
- ✅ 配置文件模板 (`conf/input.coroot_servicemap/servicemap.toml`)

### 2. 数据结构设计 (100%)
- ✅ Instance 配置结构 (`instance.go`)
- ✅ 容器元数据结构 (`containers/container.go`)
- ✅ TCP/HTTP 统计结构
- ✅ 事件类型定义

### 3. 核心组件框架 (80%)
- ✅ Instance 实现基本的 `Init()`, `Gather()`, `Drop()` 方法
- ✅ Container Registry 事件处理框架 (`containers/registry.go`)
- ✅ 容器统计聚合逻辑
- ⚠️ eBPF Tracer 框架结构 (但实际加载逻辑缺失)

### 4. 文档 (90%)
- ✅ 用户文档 `README.md`
- ✅ 开发指南 `DEVELOPMENT.md`
- ✅ 配置示例和说明

## ⚠️ 待完成工作 - 关键阻塞项

### 🔴 核心阻塞：eBPF 程序实现 (0%)

**问题：** `tracer/tracer.go` 文件在之前的创建过程中损坏，需要重新创建。

**需要做的事情：**

1. **重新创建 tracer.go 文件** - 包含基础的 Tracer 结构和接口
2. **创建 eBPF C 代码** - TCP 连接跟踪、HTTP 解析
3. **编译 eBPF 程序** - clang 编译为字节码
4. **嵌入到 Go 代码** - base64 编码内嵌
5. **实现加载逻辑** - cilium/ebpf 库加载和 attach

### 📝 次要任务

1. **容器发现完善** (50%)
   - ⚠️ cgroup 解析未完成
   - ⚠️ Docker API 集成未完成
   - ⚠️ Kubernetes 元数据提取未完成

2. **测试** (20%)
   - ✅ 基础单元测试框架
   - ❌ eBPF 功能测试
   - ❌ 集成测试
   - ❌ 性能测试

3. **L7 协议解析** (0%)
   - ❌ HTTP 请求/响应解析
   - ❌ HTTPS uprobe 支持
   - ❌ DNS 解析记录

## 📦 当前文件清单

```
inputs/coroot_servicemap/
├── servicemap.go              ✅ 插件入口
├── instance.go                ✅ 实例实现
├── README.md                  ✅ 用户文档
├── DEVELOPMENT.md             ✅ 开发指南
├── STATUS.md                  ✅ 状态文档 (本文件)
├── containers/
│   ├── container.go           ✅ 容器对象
│   ├── registry.go            ✅ 容器注册表
│   └── container_test.go      ✅ 测试
└── tracer/
    ├── tracer.go              🔴 损坏，需要重建
    └── tracer_test.go         ✅ 测试框架

conf/input.coroot_servicemap/
└── servicemap.toml            ✅ 配置模板
```

##  下一步行动方案

###  方案 A：完整实现 (推荐用于生产)

**时间估计：** 1-2 周

**步骤：**
1. 重新创建 `tracer/tracer.go` - 基础结构
2. 从 coroot-node-agent 移植 eBPF C 代码
3. 设置编译pipeline (clang + llvm-strip)
4. 实现 eBPF 程序加载逻辑
5. 实现容器元数据提取
6. 添加完整测试
7. 性能优化和文档完善

###  方案 B：最小可行版本 (快速验证)

**时间估计：** 2-3 天

**步骤：**
1. 重新创建简化版 `tracer/tracer.go`
2. 直接从 coroot-node-agent 复制预编译的 eBPF 字节码
3. 实现基础的字节码加载
4. 实现简单的cgroup容器ID提取
5. 编写基本测试验证数据流

### 🎯 方案 C：仅完成代码框架 (当前建议)

**时间估计：** 30分钟

**理由：** 实际的 eBPF 程序开发需要：
- Linux 开发环境 (>=4.16)
- root 权限进行测试
- clang/llvm 工具链
- 对 eBPF 和内核的深入理解

**步骤：**
1. 创建完整的 `tracer/tracer.go` 框架代码
2. 添加必要的接口和类型定义
3. 在关键位置添加 `TODO` 和详细注释
4. 确保代码编译通过 (`go build`)
5. 单元测试通过 (跳过需要eBPF的测试)
6. 提供详细的后续实现指南

## 🛠️ 开发环境要求

### 最低要求
- Go 1.19+
- Linux Kernel 4.16+ (用于eBPF)
- root 权限 (加载eBPF程序)

### eBPF 开发需要
- clang 10+
- llvm
- linux-headers
- libbpf-dev

### 容器支持
- Docker (可选)
- Kubernetes (可选)

## 📚 参考资源

- [coroot-node-agent GitHub](https://github.com/coroot/coroot-node-agent)
- [Categraf 输入插件开发](https://github.com/flashcatcloud/categraf/tree/main/inputs)
- [cilium/ebpf 文档](https://pkg.go.dev/github.com/cilium/ebpf)
- [eBPF 开发教程](https://ebpf.io/get-started/)

## 🤝 下一步建议

**建议采用方案 C**，因为：
1. eBPF 程序需要专门的 Linux 开发环境
2. 需要 root 权限和内核特性支持
3. 框架已经完整，缺的是实现细节
4. 可以先提交框架代码，eBPF 实现可以后续单独开发和测试

**执行方案 C 后，代码将具备：**
- ✅ 完整的插件框架
- ✅ 清晰的接口定义
- ✅ 详细的实现指南
- ✅ 可编译通过的代码
- ✅ 基础单元测试覆盖

**后续可以：**
- 在 Linux VM 中开发 eBPF 程序
- 逐步实现容器发现功能
- 添加更多协议支持
- 性能优化和测试
