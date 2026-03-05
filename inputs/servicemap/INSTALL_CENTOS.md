# servicemap 插件 — CentOS / RHEL 安装编译指南

本文档针对 **CentOS 7 / CentOS Stream 8~9 / Rocky Linux 8~9 / AlmaLinux 8~9** 环境，
完整说明 `servicemap` 插件的依赖安装、eBPF 程序编译和 categraf 构建流程。

---

## 目录

1. [环境预检](#1-环境预检)
2. [安装 Go 工具链](#2-安装-go-工具链)
3. [安装 eBPF 编译依赖](#3-安装-ebpf-编译依赖)
   - [CentOS 7 / RHEL 7](#centOS-7--rhel-7)
   - [CentOS Stream 8 / Rocky 8 / Alma 8](#centos-stream-8--rocky-8--alma-8)
   - [CentOS Stream 9 / Rocky 9 / Alma 9](#centos-stream-9--rocky-9--alma-9)
4. [生成 vmlinux.h](#4-生成-vmlinuxh)
5. [编译 eBPF 程序](#5-编译-ebpf-程序)
6. [编译 categraf 二进制](#6-编译-categraf-二进制)
7. [部署与配置](#7-部署与配置)
8. [以 systemd 运行](#8-以-systemd-运行)
9. [常见问题排查](#9-常见问题排查)

---

## 1. 环境预检

### 内核版本

```bash
uname -r
```

| 内核版本 | 插件运行模式 | 说明 |
|---|---|---|
| `>= 5.1` | eBPF 完整模式（推荐） | 支持 BTF，vmlinux.h 自动生成 |
| `4.16 ~ 5.0` | eBPF 基础模式 | 可能需要手动提供 vmlinux.h |
| `< 4.16` | 轮询模式（自动降级） | 无需 eBPF 依赖，功能受限 |

> **CentOS 7 默认内核为 3.10**，低于 eBPF 最低要求，插件将自动切换轮询模式。
> 如需完整 eBPF 功能，请使用 ELRepo 升级到 5.x 内核，或改用 CentOS Stream 8/9。

### 架构

```bash
uname -m   # 应输出 x86_64 或 aarch64
```

### eBPF 内核配置检查

```bash
# 检查 BPF 编译选项
grep -E "CONFIG_BPF|CONFIG_KPROBES|CONFIG_TRACING" /boot/config-$(uname -r) 2>/dev/null \
  || zgrep -E "CONFIG_BPF|CONFIG_KPROBES" /proc/config.gz 2>/dev/null

# 检查 tracefs 挂载
ls /sys/kernel/debug/tracing 2>/dev/null \
  || ls /sys/kernel/tracing 2>/dev/null \
  || echo "tracefs 未挂载，eBPF tracepoint 不可用"

# 检查 BTF 信息（用于生成 vmlinux.h，内核 >= 5.1 才有）
ls -lh /sys/kernel/btf/vmlinux 2>/dev/null || echo "无 BTF，需手动提供 vmlinux.h"
```

---

## 2. 安装 Go 工具链

categraf 要求 **Go 1.21+**。各发行版仓库中的 Go 版本通常偏旧，建议直接从官网安装。

```bash
# 下载 Go（以 1.22.3 为例，请按需替换版本）
GO_VERSION=1.22.3
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz

# 解压到 /usr/local
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go.tar.gz

# 配置环境变量（写入 /etc/profile.d/ 对所有用户生效）
cat <<'EOF' | sudo tee /etc/profile.d/golang.sh
export PATH=$PATH:/usr/local/go/bin
export GOPATH=$HOME/go
export GOBIN=$GOPATH/bin
export PATH=$PATH:$GOBIN
EOF

source /etc/profile.d/golang.sh

# 验证
go version
```

### 可选：配置国内代理（网络受限环境）

```bash
go env -w GOPROXY=https://goproxy.cn,direct
go env -w GONOSUMCHECK=*
```

---

## 3. 安装 eBPF 编译依赖

不同版本的依赖包名称和安装方式有所不同，请按实际系统选择。

---

### CentOS 7 / RHEL 7

> **注意**：CentOS 7 默认仓库的 clang/llvm 版本（3.4）过旧，无法编译 eBPF 程序。
> 需通过 **SCL（Software Collections）** 安装 llvm-toolset-7（提供 clang 5.0）。

```bash
# 1. 安装 EPEL 和 SCL 源
sudo yum install -y epel-release centos-release-scl

# 2. 安装 llvm-toolset-7（含 clang 5.0 + llvm）
sudo yum install -y llvm-toolset-7 llvm-toolset-7-clang

# 3. 安装 kernel-devel 和 kernel-headers（需与当前内核版本一致）
sudo yum install -y "kernel-devel-$(uname -r)" "kernel-headers-$(uname -r)"

# 4. 安装 bpftool（CentOS 7 中包含在 kernel-tools 中）
sudo yum install -y kernel-tools

# 5. 安装编译工具
sudo yum install -y git make gcc elfutils-libelf-devel zlib-devel

# 6. 激活 SCL 环境（需在每个 shell session 中执行，或加入 ~/.bashrc）
scl enable llvm-toolset-7 bash

# 验证 clang 版本（需要 >= 5.0）
clang --version
```

> ⚠️ CentOS 7 内核为 3.10，不支持 eBPF。即使 clang 安装成功，
> eBPF 程序也无法在此内核上运行。如仅需在 CentOS 7 上**交叉编译**供其他机器使用，
> 上述步骤仍然有效；本机运行请升级内核或改用 CentOS Stream 8+。

#### 可选：通过 ELRepo 升级到 5.x 内核（CentOS 7 生产升级路径）

```bash
# 安装 ELRepo 源
sudo rpm --import https://www.elrepo.org/RPM-GPG-KEY-elrepo.org
sudo yum install -y https://www.elrepo.org/elrepo-release-7.el7.elrepo.noarch.rpm

# 安装长期支持版（lt）内核
sudo yum --enablerepo=elrepo-kernel install -y kernel-lt kernel-lt-devel

# 配置默认引导新内核
sudo grub2-set-default 0
sudo grub2-mkconfig -o /boot/grub2/grub.cfg

# 重启后验证
# reboot
# uname -r  # 应输出 5.x.y-xxx
```

---

### CentOS Stream 8 / Rocky 8 / Alma 8

```bash
# 1. 启用 PowerTools（CentOS Stream 8）或 CRB（Rocky/Alma 8）
sudo dnf config-manager --set-enabled powertools 2>/dev/null \
  || sudo dnf config-manager --set-enabled crb 2>/dev/null

# 2. 安装 EPEL
sudo dnf install -y epel-release

# 3. 安装 clang / llvm / bpftool
sudo dnf install -y clang llvm bpftool

# 4. 安装 kernel-devel（需与当前内核版本一致）
sudo dnf install -y "kernel-devel-$(uname -r)"

# 5. 安装编译工具和 libbpf 开发库
sudo dnf install -y git make gcc elfutils-libelf-devel zlib-devel libbpf-devel

# 验证
clang --version    # 应 >= 10.0
bpftool version
```

---

### CentOS Stream 9 / Rocky 9 / Alma 9

```bash
# 1. 启用 CRB（CodeReady Builder）仓库
sudo dnf config-manager --set-enabled crb

# 2. 安装 EPEL
sudo dnf install -y epel-release

# 3. 安装 clang / llvm / bpftool
sudo dnf install -y clang llvm bpftool

# 4. 安装 kernel-devel
sudo dnf install -y "kernel-devel-$(uname -r)"

# 5. 安装编译工具和 libbpf 开发库
sudo dnf install -y git make gcc elfutils-libelf-devel zlib-devel libbpf-devel

# 验证
clang --version    # 应 >= 14.0
bpftool version
```

---

## 4. 生成 vmlinux.h

`vmlinux.h` 包含当前内核的全部类型定义，是 eBPF 程序编译的必要头文件。

### 方法一：从 BTF 自动生成（推荐，内核 >= 5.1）

```bash
# 确认 BTF 文件存在
ls -lh /sys/kernel/btf/vmlinux

# 生成 vmlinux.h
bpftool btf dump file /sys/kernel/btf/vmlinux format c \
    > inputs/servicemap/tracer/bpf/vmlinux.h

wc -l inputs/servicemap/tracer/bpf/vmlinux.h   # 通常为数十万行
```

### 方法二：从 kernel-devel DWARF 提取（内核 < 5.1 / 无 BTF）

```bash
# 确认 vmlinux 调试文件存在（需安装 kernel-debuginfo）
sudo dnf install -y "kernel-debuginfo-$(uname -r)" 2>/dev/null \
  || sudo yum install -y "kernel-debuginfo-$(uname -r)"

VMLINUX_DBG=$(ls /usr/lib/debug/lib/modules/$(uname -r)/vmlinux 2>/dev/null \
           || ls /boot/vmlinux-$(uname -r) 2>/dev/null)

bpftool btf dump file "$VMLINUX_DBG" format c \
    > inputs/servicemap/tracer/bpf/vmlinux.h
```

### 方法三：使用 BTFHub 预生成文件（无 BTF + 无 debuginfo）

```bash
# 查看当前内核精确版本和发行版
uname -r
cat /etc/os-release | grep -E "^ID=|^VERSION_ID="

# 从 BTFHub 下载对应文件
# https://github.com/aquasecurity/btfhub-archive
# 选择对应目录：centos/<版本>/<arch>/<内核版本>.btf.tar.xz

curl -fsSL \
  "https://github.com/aquasecurity/btfhub-archive/raw/main/centos/7/x86_64/$(uname -r).btf.tar.xz" \
  -o /tmp/kernel.btf.tar.xz

tar -xf /tmp/kernel.btf.tar.xz -C /tmp/

bpftool btf dump file /tmp/$(uname -r).btf format c \
    > inputs/servicemap/tracer/bpf/vmlinux.h
```

---

## 5. 编译 eBPF 程序

```bash
# 进入项目根目录
cd /path/to/categraf

# 进入 BPF 源码目录
cd inputs/servicemap/tracer/bpf

# 查看 Makefile 支持的目标
make help 2>/dev/null || cat Makefile

# 编译（CentOS 7 SCL 环境下需先激活：scl enable llvm-toolset-7 bash）
cd ..
make

# 验证生成的 Go 嵌入文件
ls -lh ebpf_programs_generated.go
head -5 ebpf_programs_generated.go
```

编译成功后，`ebpf_programs_generated.go` 中包含 eBPF 字节码的 Go 嵌入数组，后续 `go build` 会将其一起打包进二进制文件，**无需在目标机器上安装任何 eBPF 相关依赖**。

---

## 6. 编译 categraf 二进制

```bash
cd /path/to/categraf

# 拉取依赖
go mod download

# 编译（带 servicemap 插件的 Linux/amd64 版本）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o categraf .

# 验证
./categraf --version
file categraf   # 应输出 ELF 64-bit LSB executable
```

### 交叉编译（在 macOS / 其他机器上编译，目标为 CentOS）

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o categraf_linux_amd64 .

# 上传到 CentOS 机器
scp categraf_linux_amd64 user@centos-host:/usr/local/bin/categraf
```

---

## 7. 部署与配置

```bash
# 创建目录结构
sudo mkdir -p /etc/categraf/conf/input.servicemap
sudo mkdir -p /var/log/categraf

# 复制二进制
sudo cp categraf /usr/local/bin/categraf
sudo chmod +x /usr/local/bin/categraf

# 创建主配置文件
sudo tee /etc/categraf/conf/config.toml > /dev/null <<'EOF'
[global]
  hostname = ""
  interval = 15
  providers = ["local"]

[log]
  file_name = "/var/log/categraf/categraf.log"
  level = "info"

[writer_opt]
  batch = 2000
  chan_size = 10000

[[writers]]
  url = "http://127.0.0.1:9090/api/v1/write"
  timeout = 5000
  dial_timeout = 2500
EOF

# 创建 servicemap 插件配置
sudo tee /etc/categraf/conf/input.servicemap/servicemap.toml > /dev/null <<'EOF'
[[instances]]
interval = 60

# 启用 TCP 连接跟踪
enable_tcp = true

# 启用 HTTP 请求跟踪（仅 eBPF 模式下有效）
enable_http = true
enable_l7_tracing = true

# 容器发现
enable_docker = true
enable_k8s = false
enable_cgroup = true

# 过滤配置（忽略 SSH 端口和 Prometheus 节点导出器）
ignore_ports = [22, 9100]
ignore_cidrs = ["127.0.0.0/8"]

[instances.labels]
  # env = "production"
  # cluster = "cluster-1"
EOF
```

---

## 8. 以 systemd 运行

```bash
# 创建专用用户（可选，eBPF 模式下需要 root 或 CAP_SYS_ADMIN）
# sudo useradd -r -s /sbin/nologin categraf

# 创建 systemd 服务文件
sudo tee /etc/systemd/system/categraf.service > /dev/null <<'EOF'
[Unit]
Description=Categraf Monitoring Agent
Documentation=https://github.com/flashcatcloud/categraf
After=network-online.target
Wants=network-online.target

[Service]
# eBPF 模式需要 root 权限或以下 Capabilities
User=root
Group=root

# 若不想以 root 运行，可改用非 root 用户并授予 Capabilities：
# User=categraf
# AmbientCapabilities=CAP_SYS_ADMIN CAP_NET_ADMIN CAP_SYS_PTRACE CAP_NET_RAW
# CapabilityBoundingSet=CAP_SYS_ADMIN CAP_NET_ADMIN CAP_SYS_PTRACE CAP_NET_RAW

ExecStart=/usr/local/bin/categraf --configs /etc/categraf/conf
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
LimitNPROC=32768

# 日志输出
StandardOutput=journal
StandardError=journal
SyslogIdentifier=categraf

[Install]
WantedBy=multi-user.target
EOF

# 重新加载 systemd 并启动服务
sudo systemctl daemon-reload
sudo systemctl enable --now categraf

# 查看运行状态
sudo systemctl status categraf

# 实时查看日志
sudo journalctl -u categraf -f
```

---

## 9. 常见问题排查

### 问题一：`clang: command not found`（CentOS 7）

```bash
# 确认 SCL 环境已激活
which clang || scl enable llvm-toolset-7 "which clang"

# 永久激活（写入 /etc/profile.d/）
echo 'source scl_source enable llvm-toolset-7' \
    | sudo tee /etc/profile.d/llvm-toolset-7.sh
source /etc/profile.d/llvm-toolset-7.sh
```

### 问题二：`bpftool: command not found`

```bash
# CentOS 7：在 kernel-tools 包中
sudo yum install -y kernel-tools
ls /usr/sbin/bpftool || ls /sbin/bpftool

# CentOS 8/9：
sudo dnf install -y bpftool
```

### 问题三：`kernel-devel` 版本与当前内核不匹配

```bash
# 查看已安装的内核版本
rpm -qa | grep kernel-devel

# 查看当前运行内核
uname -r

# 安装匹配版本
sudo yum install -y "kernel-devel-$(uname -r)"   # CentOS 7
sudo dnf install -y "kernel-devel-$(uname -r)"   # CentOS 8/9

# 如果仓库中找不到对应版本（内核已更新但包已下线）
# 可从 vault.centos.org 手动下载：
# https://vault.centos.org/centos/<version>/updates/x86_64/Packages/
```

### 问题四：SELinux 阻止 eBPF 加载

```bash
# 检查 SELinux 状态
getenforce   # Enforcing / Permissive / Disabled

# 临时切换为 Permissive（重启后失效）
sudo setenforce 0

# 永久设置（修改配置文件，需重启）
sudo sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config

# 或添加针对 bpf 的 SELinux 策略（推荐生产环境）
sudo ausearch -c bpf --raw | sudo audit2allow -M categraf_bpf
sudo semodule -X 300 -i categraf_bpf.pp
```

### 问题五：`/sys/kernel/debug/tracing` 不可访问

```bash
# 挂载 debugfs（重启后失效）
sudo mount -t debugfs none /sys/kernel/debug

# 永久挂载（写入 /etc/fstab）
echo "debugfs /sys/kernel/debug debugfs defaults 0 0" | sudo tee -a /etc/fstab
sudo mount -a

# 验证 tracepoint 可用
ls /sys/kernel/debug/tracing/events/syscalls/ | head -5
```

### 问题六：插件自动降级为轮询模式

日志中出现 `W! servicemap: ... fallback to polling tracer` 属正常行为，说明当前环境不满足 eBPF 要求，
轮询模式仍可提供 TCP 连接拓扑数据（无字节统计、无 L7 数据）。

检查降级原因：

```bash
sudo journalctl -u categraf | grep -E "eBPF|ebpf|fallback|polling|BTF"
```

| 常见降级原因 | 解决方法 |
|---|---|
| 内核 < 4.16 | 升级内核（ELRepo lt 版）或改用 CentOS Stream 8+ |
| 无 tracefs 挂载 | 挂载 debugfs（见问题五） |
| SELinux 阻止 | 设置 Permissive 或添加策略（见问题四） |
| 无 root 权限 | 以 root 运行，或配置 `CAP_SYS_ADMIN` |
| BTF 不存在 | 手动提供 vmlinux.h（见第4节） |

### 问题七：验证插件正常采集数据

```bash
# Graph API — 查看当前拓扑图（文本格式）
curl -s http://localhost:9099/graph/text

# Graph API — 查看 JSON 格式
curl -s http://localhost:9099/graph/view | python3 -m json.tool | head -60

# 查看 Prometheus 指标
curl -s http://localhost:9099/metrics | grep ^servicemap_

# 检查插件运行模式
curl -s http://localhost:9099/metrics | grep servicemap_tracer
```

---

## 版本兼容性速查

| 发行版 | 默认内核 | clang 安装方式 | eBPF 支持 |
|---|---|---|---|
| CentOS 7.x | 3.10 | `scl enable llvm-toolset-7` | ❌ 需升级内核 |
| CentOS 7 + ELRepo lt | 5.4 / 5.15 | SCL 或 LLVM 官方 repo | ✅ |
| CentOS Stream 8 | 4.18 | `dnf install clang` | ⚠️ 基础 eBPF（无 BTF） |
| Rocky Linux 8 | 4.18 | `dnf install clang` | ⚠️ 基础 eBPF（无 BTF） |
| CentOS Stream 9 | 5.14 | `dnf install clang` | ✅ 完整支持 |
| Rocky Linux 9 | 5.14 | `dnf install clang` | ✅ 完整支持 |
| AlmaLinux 9 | 5.14 | `dnf install clang` | ✅ 完整支持 |
