# eBPF 程序编译说明

## 前置要求

### 1. 安装 bpftool 和 clang

**Ubuntu/Debian:**
```bash
sudo apt-get update
sudo apt-get install -y clang llvm libbpf-dev linux-tools-common linux-tools-generic bpftool
```

**macOS (仅用于交叉编译):**
```bash
brew install llvm
```

### 2. 生成 vmlinux.h

`vmlinux.h` 包含内核类型定义，需要在目标 Linux 系统上生成：

```bash
# 在 Linux 系统上运行
cd inputs/coroot_servicemap/tracer/bpf
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
```

如果系统不支持 BTF，可以从构建的内核中提取：
```bash
bpftool btf dump file /boot/vmlinuz-$(uname -r) format c > vmlinux.h
```

或者使用预生成的 vmlinux.h (适用于标准内核版本)。

## 编译步骤

### 方式一：使用 Makefile (推荐)

```bash
cd inputs/coroot_servicemap/tracer
make
```

这将：
1. 编译 eBPF C 代码为 .o 文件
2. 使用 gzip 压缩字节码
3. 生成 Go 文件 `ebpf_programs_generated.go` 包含嵌入的字节码

### 方式二：手动编译

```bash
cd inputs/coroot_servicemap/tracer/bpf

# 编译 eBPF 程序
clang -O2 -g -target bpf -D__TARGET_ARCH_x86_64 \
  -I/usr/include/bpf \
  -c tcp_tracer.bpf.c -o tcp_tracer.bpf.o

# 压缩字节码
gzip -c tcp_tracer.bpf.o > tcp_tracer.bpf.o.gz

# 转换为 Go embed
go run ../../../scripts/embed_ebpf.go tcp_tracer.bpf.o.gz > ../ebpf_programs_generated.go
```

## 架构支持

当前支持：
- `amd64` (x86_64)
- `arm64` (aarch64)

为不同架构编译需要调整 `-target` 参数：
- x86_64: `-target bpf -D__TARGET_ARCH_x86_64`
- arm64: `-target bpf -D__TARGET_ARCH_arm64`

## 嵌入到 Categraf

编译后的字节码通过 `ebpf_programs_generated.go` 嵌入到 Go 二进制文件中：

```go
var embeddedPrograms = map[string][]EmbeddedProgram{
    "amd64": {{
        MinKernel: "4.14",
        Flags:     "",
        Program:   []byte{...}, // gzip 压缩的 eBPF 字节码
    }},
}
```

运行时，tracer 会：
1. 根据 `runtime.GOARCH` 选择对应架构的程序
2. 解压 gzip 数据
3. 使用 cilium/ebpf 加载到内核
4. 附加到 kprobe/tracepoint

## 测试

```bash
# 编译整个插件
cd /path/to/categraf
go build ./inputs/coroot_servicemap/...

# 运行测试
go test -v ./inputs/coroot_servicemap/tracer
```

## 故障排除

### 问题：编译时找不到内核头文件

**解决方案：** 安装内核头文件
```bash
sudo apt-get install linux-headers-$(uname -r)
```

### 问题：vmlinux.h 缺失

**解决方案：** 从 [libbpf-bootstrap](https://github.com/libbpf/libbpf-bootstrap/tree/master/vmlinux) 下载预生成的 vmlinux.h

### 问题：eBPF 程序加载失败

**解决方案：**
1. 检查内核版本是否 >= 4.14
2. 确保启用 BTF: `CONFIG_DEBUG_INFO_BTF=y`
3. 检查权限: 需要 CAP_BPF 或 root
4. 查看日志: `dmesg | grep bpf`

### 问题：在 macOS 上无法编译

**解决方案：** eBPF 仅支持 Linux 内核。在 macOS 上：
- 使用交叉编译
- 或在 Docker/VM 中编译
- 或使用 GitHub Actions CI 自动编译
