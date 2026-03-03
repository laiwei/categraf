本项目 (Coroot ServiceMap Categraf Plugin) 的完整索引和导航

# 📑 项目文档导航

## 🎯 新手入门
从这里开始了解项目：
1. [README.md](README.md) - **必读** 功能概述和使用指南
2. [QUICKSTART.md](QUICKSTART.md) - 快速开始和常见问题
3. [项目结构](#项目结构) - 文件组织说明 (本文件)

## 📚 详细文档

### 用户文档
- [README.md](README.md) - 功能特性、安装、配置、指标说明
- [QUICKSTART.md](QUICKSTART.md) - 快速参考、常见问题、故障排除

### 开发文档
- [DEVELOPMENT.md](DEVELOPMENT.md) - eBPF 开发指南、编译步骤、调试技巧
- [STATUS.md](STATUS.md) - 项目状态、完成清单、下一步方案

### 项目报告
- [COMPLETION_REPORT.md](COMPLETION_REPORT.md) - 详细的完成报告和架构说明

## 🏗️ 项目结构

```
inputs/coroot_servicemap/                    # 插件主目录
├── 📄 servicemap.go                        # 插件入口 (41 行)
│   ├─ ServiceMapPlugin 结构体
│   ├─ init() 注册函数
│   └─ Input 接口实现
│
├── 📄 instance.go                          # 实例实现 (258 行)
│   ├─ Instance 结构体
│   ├─ Init() 初始化
│   ├─ Gather() 采集数据
│   └─ Drop() 清理资源
│
├── tracer/                                 # eBPF 追踪器包
│   ├── 📄 tracer.go                       # Tracer 实现 (206 行)
│   │   ├─ EventType 事件类型
│   │   ├─ Event 结构体
│   │   ├─ Tracer 结构体
│   │   ├─ Start() eBPF 程序启动
│   │   └─ Events() 事件通道
│   │
│   └── 📄 tracer_test.go                  # Tracer 测试
│       ├─ TestEventType_String
│       └─ TestNewTracer
│
├── containers/                            # 容器管理包
│   ├── 📄 container.go                    # 容器对象 (194 行)
│   │   ├─ Container 结构体
│   │   ├─ TCPStats 统计数据
│   │   ├─ HTTPStats 统计数据
│   │   ├─ OnEvent() 事件处理
│   │   └─ UpdateTrafficStats() 流量更新
│   │
│   ├── 📄 container_test.go               # 容器测试
│   │   ├─ TestNewContainer
│   │   ├─ TestContainer_OnConnectionOpen
│   │   ├─ TestContainer_OnConnectionClose
│   │   └─ TestContainer_OnRetransmit
│   │
│   └── 📄 registry.go                     # 容器注册表 (168 行)
│       ├─ Registry 结构体
│       ├─ handleEvents() 事件处理器
│       ├─ updateConnectionStats() 统计更新
│       └─ discoverContainersByCgroup() 容器发现
│
├── 📋 README.md                           # 用户文档 (214 行)
│   ├─ 功能介绍
│   ├─ 系统要求
│   ├─ 安装和配置
│   ├─ 指标说明
│   ├─ 示例和故障排除
│   └─ 常见问题
│
├── 📋 QUICKSTART.md                       # 快速参考 (274 行)
│   ├─ 项目完成度
│   ├─ 快速开始
│   ├─ 核心类和函数
│   ├─ 配置选项
│   ├─ 测试运行
│   └─ 故障排除
│
├── 📋 DEVELOPMENT.md                      # 开发指南 (385 行)
│   ├─ 开发状态
│   ├─ eBPF 程序实现步骤
│   ├─ 编译 pipeline
│   ├─ 容器元数据提取
│   ├─ 编译和运行
│   ├─ 测试和调试
│   └─ 参考资源
│
├── 📋 STATUS.md                           # 项目状态 (174 行)
│   ├─ 已完成工作
│   ├─ 待完成工作
│   ├─ 文件清单
│   ├─ 下一步行动方案
│   └─ 开发环境要求
│
└── 📋 COMPLETION_REPORT.md                # 完成报告 (412 行)
    ├─ 项目概述
    ├─ 完成的工作
    ├─ 测试结果
    ├─ 工作流程架构
    ├─ 数据流说明
    ├─ 技术栈
    ├─ 限制和待实现
    ├─ 后续步骤
    └─ 代码亮点

conf/input.coroot_servicemap/              # 配置目录
└── 📄 servicemap.toml                     # 配置模板 (33 行)
    ├─ 功能开关
    ├─ 容器发现配置
    ├─ 黑名单设置
    └─ 平台特定配置
```

## 📊 统计信息

### 代码统计
- **Go 源代码**: 1,089 行
  - servicemap.go: 41 行
  - instance.go: 258 行
  - tracer/tracer.go: 206 行
  - containers/container.go: 194 行
  - containers/registry.go: 168 行
  - 测试代码: ~222 行

- **文档**: 1,459 行
  - README.md: 214 行
  - QUICKSTART.md: 274 行
  - DEVELOPMENT.md: 385 行
  - STATUS.md: 174 行
  - COMPLETION_REPORT.md: 412 行

- **配置**: 33 行 (servicemap.toml)

### 测试覆盖
- **6 个单元测试全部通过** ✓
  - tracer 包: 2 个测试
  - containers 包: 4 个测试

### 编译状态
- **go build**: ✓ 成功
- **导入检查**: ✓ 通过
- **编译错误**: 无

## 🔍 按用途查看

### 我是用户，想了解如何使用
→ 先读 [README.md](README.md)，然后参考 [QUICKSTART.md](QUICKSTART.md)

### 我是开发者，想了解架构
→ 阅读 [COMPLETION_REPORT.md](COMPLETION_REPORT.md) 的"工作流程架构"部分

### 我想实现 eBPF 程序
→ 按照 [DEVELOPMENT.md](DEVELOPMENT.md) 的步骤操作

### 我想知道项目进度
→ 查看 [STATUS.md](STATUS.md) 的"已完成工作"和"待完成工作"

### 我遇到了问题
→ 首先查看 [QUICKSTART.md](QUICKSTART.md) 的"故障排除"部分

### 我想查看详细的技术细节
→ 阅读源代码注释和 [COMPLETION_REPORT.md](COMPLETION_REPORT.md)

## 🎓 学习路径

### 新手路径
1. 读 README.md 理解功能
2. 阅读 QUICKSTART.md 了解快速开始
3. 查看 tracer/ 和 containers/ 目录的代码
4. 运行 `go test ./inputs/coroot_servicemap/...` 验证

### 开发者路径
1. 了解 COMPLETION_REPORT.md 的架构
2. 研究 servicemap.go 和 instance.go 的接口
3. 学习 tracer.go 的 eBPF 框架
4. 查看 containers 包的数据结构
5. 按 DEVELOPMENT.md 实现 eBPF 部分

### 维护者路径
1. 阅读 STATUS.md 了解完成度
2. 查看各文件的 TODO 注释
3. 按 COMPLETION_REPORT.md 的后续步骤执行
4. 参考 DEVELOPMENT.md 的参考资源

## 🔗 相关资源

### 原始项目
- [coroot-node-agent](https://github.com/coroot/coroot-node-agent) - 本插件的灵感来源

### 主项目
- [Categraf](https://github.com/flashcatcloud/categraf) - 此插件的宿主

### 技术库
- [cilium/ebpf](https://github.com/cilium/ebpf) - eBPF Go 库
- [vishvananda/netns](https://github.com/vishvananda/netns) - 网络命名空间

### 文档和教程
- [eBPF 官方教程](https://ebpf.io/get-started/)
- [Linux 内核追踪](https://www.kernel.org/doc/html/latest/trace/)
- [Prometheus 指标](https://prometheus.io/docs/concepts/data_model/)

## 📞 常见问题速查

| 问题 | 文档位置 |
|------|---------|
| 如何安装和配置? | [README.md](README.md#安装和配置) |
| 支持什么指标? | [README.md](README.md#输出指标) |
| 如何处理权限错误? | [QUICKSTART.md](QUICKSTART.md#故障排除) |
| 如何编写 eBPF 程序? | [DEVELOPMENT.md](DEVELOPMENT.md#ebpf-程序实现指南) |
| 项目进度如何? | [STATUS.md](STATUS.md) |
| 如何扩展功能? | [DEVELOPMENT.md](DEVELOPMENT.md) |
| 性能怎样? | [COMPLETION_REPORT.md](COMPLETION_REPORT.md#后续实现步骤) |

## ✨ 文档更新日期

- README.md: 2024-02-28
- QUICKSTART.md: 2024-02-28
- DEVELOPMENT.md: 2024-02-28
- STATUS.md: 2024-02-28
- COMPLETION_REPORT.md: 2024-02-28
- 本文件: 2024-02-28

---

**快速导航**: [回到 README](README.md) | [快速开始](QUICKSTART.md) | [项目状态](STATUS.md)
