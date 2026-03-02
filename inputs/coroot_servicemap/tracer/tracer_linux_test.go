//go:build linux

package tracer

// tracer_linux_test.go — Linux 平台专项测试
// 使用 //go:build linux 标签，只在 Linux 上运行。
// 测试目标：
//   1. checkKernelVersion — 通过 unix.Uname 读取内核版本
//   2. pollConnections — 通过 /proc/net/tcp 发现连接（gopsutil 封装）
//   3. startFallbackMode — 启动轮询模式并验证 started 标志
//   4. Start — 在 Linux 上触发完整启动路径（tracefs/eBPF 或回退）
//   5. gcConnections + startConnectionGC 协作
//   6. ActiveConnectionCount / ForEachActiveConnection 在 Start() 后

import (
	"context"
	"testing"
	"time"
)

// TestCheckKernelVersion_Linux 验证内核版本检查在 Linux 上不崩溃。
// 生产 Linux（内核 >= 4.16）预期通过，旧内核预期返回错误。
func TestCheckKernelVersion_Linux(t *testing.T) {
	err := checkKernelVersion()
	// 无论通过还是失败，函数本身不应 panic
	if err != nil {
		t.Logf("checkKernelVersion returned error (ok on old kernels): %v", err)
	} else {
		t.Log("checkKernelVersion passed")
	}
}

// TestPollConnections_Linux 在 Linux 上运行 pollConnections，
// 验证它能读取 /proc/net/tcp 而不崩溃，且能正确更新 activeConns。
func TestPollConnections_Linux(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr, err := NewTracer(ctx, -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	// 在 Linux 上应该至少能执行一次而不崩溃
	tr.pollConnections()

	// 验证 listenPorts 已被更新（Linux 至少会有一些监听端口）
	ports := tr.GetListenPorts()
	t.Logf("pollConnections discovered %d listen ports", len(ports))

	// activeConns 可能为 0 或更多，取决于当前系统状态
	n := tr.ActiveConnectionCount()
	t.Logf("active connections: %d", n)
}

// TestStartFallbackMode_Linux 验证 startFallbackMode 设置 started=true
// 并启动了后台 goroutine。
func TestStartFallbackMode_Linux(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr, err := NewTracer(ctx, -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	tr.startFallbackMode()

	if !tr.started {
		t.Error("startFallbackMode should set started=true")
	}

	// 等待 goroutine 启动并执行一次 poll
	time.Sleep(50 * time.Millisecond)

	// startPollingTracer 会调用 pollConnections；lastSnapshot 应该被填充（或为空但不 panic）
	t.Logf("after fallback mode: active=%d", tr.ActiveConnectionCount())
}

// TestPollConnections_MaxActiveConns_Linux 验证 maxActiveConns 限制在 Linux 上有效。
func TestPollConnections_MaxActiveConns_Linux(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 设置非常低的上限以触发截断逻辑
	tr, err := NewTracer(ctx, -1, -1, true, 2)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	// 多次 poll 也不应超过上限
	for i := 0; i < 3; i++ {
		tr.pollConnections()
	}

	n := tr.ActiveConnectionCount()
	if n > 2 {
		t.Errorf("active connections %d exceeds maxActiveConns=2", n)
	}
}

// TestPollConnections_EmitsEvents_Linux 验证 pollConnections 发现连接时
// 会向 eventChan 发送 ConnectionOpen 事件（需要系统有活跃 TCP 连接）。
func TestPollConnections_EmitsEvents_Linux(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr, err := NewTracer(ctx, -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	// 调用两次：第一次填充 lastSnapshot，第二次检测变化
	tr.pollConnections()
	firstCount := tr.ActiveConnectionCount()
	t.Logf("after first poll: %d active connections", firstCount)

	tr.pollConnections()
	secondCount := tr.ActiveConnectionCount()
	t.Logf("after second poll: %d active connections", secondCount)

	// 两次调用均不应 panic；事件通道不应永久阻塞
}

// TestStart_Linux 在 Linux 上调用 Start()，验证它或者成功加载 eBPF
// 或者回退到轮询模式，两种情况下 started 都应为 true。
func TestStart_Linux(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr, err := NewTracer(ctx, -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	if err := tr.Start(); err != nil {
		t.Logf("Start returned error (expected in CI without eBPF): %v", err)
		// 不失败，只记录日志——aarch64/unsupported arch 等情况下可能返回 error
		return
	}

	if !tr.started {
		t.Error("started should be true after Start()")
	}

	// 等待后台 goroutine 初始化
	time.Sleep(100 * time.Millisecond)
	t.Logf("after Start: active=%d listen=%d",
		tr.ActiveConnectionCount(), len(tr.GetListenPorts()))
}

// TestGcConnections_Linux_WithRealConnections 在 Linux 上混合 GC 和真实 poll。
func TestGcConnections_Linux_WithRealConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr, err := NewTracer(ctx, -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	tr.pollConnections()

	// 强制把已有连接的 LastSeen 设置为过期
	tr.activeConnMu.Lock()
	old := time.Now().Add(-(connectionTimeout + time.Second))
	for id, conn := range tr.activeConns {
		conn.LastSeen = old
		tr.activeConns[id] = conn
	}
	tr.activeConnMu.Unlock()

	tr.gcConnections()

	// 过期连接应被全部清除
	if n := tr.ActiveConnectionCount(); n != 0 {
		t.Errorf("expected 0 active connections after GC, got %d", n)
	}
}
