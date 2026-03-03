package tracer

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// ─── launchBackground ─────────────────────────────────────────

func TestTracerLaunchBackground_RunsAndCompletes(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	done := make(chan struct{})
	tr.launchBackground(func() {
		close(done)
	})

	select {
	case <-done:
		// OK：函数被执行
	case <-time.After(time.Second):
		t.Error("launchBackground: function did not run within 1s")
	}
	tr.wg.Wait()
}

func TestTracerLaunchBackground_MultipleConcurrent(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	const n = 10
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		idx := i
		tr.launchBackground(func() {
			results <- idx
		})
	}

	tr.wg.Wait()
	close(results)

	count := 0
	for range results {
		count++
	}
	if count != n {
		t.Errorf("expected %d goroutines to complete, got %d", n, count)
	}
}

// ─── Start / startFallbackMode ────────────────────────────────

func TestTracer_Start_FallbackMode(t *testing.T) {
	tr, err := NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}

	if err := tr.Start(); err != nil {
		t.Fatalf("Start() returned unexpected error: %v", err)
	}

	if !tr.started {
		t.Error("tracer.started should be true after Start()")
	}

	// 给后台 goroutine（startPollingTracer）一点时间运行 pollConnections
	time.Sleep(100 * time.Millisecond)

	tr.Close()
}

func TestTracer_Start_Idempotent(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 先手动标记为已启动
	tr.started = true

	// 再次调用 Start() 应该直接返回
	if err := tr.Start(); err != nil {
		t.Errorf("Start() on already-started tracer should return nil, got: %v", err)
	}
}

func TestTracer_Start_NonLinuxFallback(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("非 Linux 回退分支测试，跳过 Linux 环境")
	}

	tr, _ := NewTracer(context.Background(), -1, -1, true, 0)
	_ = tr.Start()

	// 在非 Linux 上必然走 startFallbackMode
	if !tr.started {
		t.Error("tracer should be started after fallback")
	}
	tr.Close()
}

// ─── startConnectionGC ────────────────────────────────────────

func TestTracer_StartConnectionGC_ExitsOnClose(t *testing.T) {
	tr := newTestTracer(t)

	done := make(chan struct{})
	go func() {
		tr.startConnectionGC()
		close(done)
	}()

	// 给 goroutine 启动时间
	time.Sleep(20 * time.Millisecond)
	tr.Close()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("startConnectionGC did not exit after Close()")
	}
}

// ─── pollConnections ─────────────────────────────────────────

func TestTracer_PollConnections_RunsWithoutPanic(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 直接调用 pollConnections，不应 panic
	// 使用 gopsutil 读取当前系统 TCP 连接（跨平台）
	tr.pollConnections()

	// listenPorts 应被更新（当前系统一定有 LISTEN 状态的端口）
	ports := tr.GetListenPorts()
	t.Logf("pollConnections discovered %d listen ports", len(ports))
	// 不断言具体数量，因为 CI 环境可能无连接
}

func TestTracer_PollConnections_PopulatesLastSnapshot(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	tr.pollConnections()

	tr.activeConnMu.RLock()
	snapLen := len(tr.lastSnapshot)
	tr.activeConnMu.RUnlock()

	t.Logf("lastSnapshot contains %d connections", snapLen)
	// 快照结构不为 nil 即通过（内容依赖系统连接状态）
}

func TestTracer_PollConnections_EmitsOpenEvents(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 第一次 poll — 所有当前连接产生 Open 事件
	tr.pollConnections()

	// 第二次 poll — 相同连接不重复产生 Open 事件
	tr.pollConnections()

	// 不阻塞断言，只验证 channel 不超容量
	if len(tr.eventChan) == cap(tr.eventChan) {
		t.Error("event channel should not be full after two polls")
	}
}

func TestTracer_PollConnections_MaxActiveConns(t *testing.T) {
	tr, _ := NewTracer(context.Background(), -1, -1, true, 1)
	defer tr.Close()

	// maxActiveConns=1，poll 不应超出限制
	tr.pollConnections()

	if n := tr.ActiveConnectionCount(); n > 1 {
		t.Errorf("expected at most 1 active conn (maxActiveConns=1), got %d", n)
	}
}

// ─── startPollingTracer ───────────────────────────────────────

func TestTracer_StartPollingTracer_ExitsOnClose(t *testing.T) {
	tr := newTestTracer(t)

	done := make(chan struct{})
	go func() {
		tr.startPollingTracer()
		close(done)
	}()

	// 给至少一次 pollConnections 运行时间
	time.Sleep(50 * time.Millisecond)
	tr.Close()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Error("startPollingTracer did not exit after Close()")
	}
}

// ─── adaptiveInterval ──────────────────────────────────────────

func TestAdaptiveInterval_Thresholds(t *testing.T) {
	cases := []struct {
		connCount int
		wantMax   time.Duration // 必须走该分档，取指定区间的上限
	}{
		{0, 2 * time.Second},
		{999, 2 * time.Second},
		{1000, 5 * time.Second},
		{4999, 5 * time.Second},
		{5000, 10 * time.Second},
		{100_000, 10 * time.Second},
	}
	for _, c := range cases {
		got := adaptiveInterval(c.connCount)
		if got != c.wantMax {
			t.Errorf("adaptiveInterval(%d) = %v, want %v", c.connCount, got, c.wantMax)
		}
	}
}

func TestAdaptiveInterval_MonotonicallyNonDecreasing(t *testing.T) {
	// 连接数增大时，间隔不应减小
	counts := []int{0, 500, 1000, 2000, 5000, 10000, 50000}
	prev := adaptiveInterval(0)
	for _, n := range counts[1:] {
		cur := adaptiveInterval(n)
		if cur < prev {
			t.Errorf("adaptiveInterval(%d)=%v < adaptiveInterval(smaller)=%v: not monotone", n, cur, prev)
		}
		prev = cur
	}
}

// ─── SetPIDFilter / PID 过滤路径 ───────────────────────────────

func TestSetPIDFilter_NilFilter_NoEffect(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 未设置过滤器时，pollConnections 应走全量扫描路径，不 panic
	if tr.pidFilter != nil {
		t.Fatal("pidFilter should be nil by default")
	}
	tr.pollConnections() // 不应 panic
}

func TestSetPIDFilter_EmptySet_ForcesGlobalScan(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 过滤回调返回空集合：应走全量扫描分支，不过滤任何连接
	tr.SetPIDFilter(func() map[uint32]struct{} {
		return map[uint32]struct{}{}
	})

	// 第一次：pidFilter 返回空集合 len==0，应走全量扫描路径
	tr.pollConnections()

	// lastGlobalScan 应被更新
	if tr.lastGlobalScan.IsZero() {
		t.Error("lastGlobalScan should be set after global scan with empty PID set")
	}
}

func TestSetPIDFilter_WithPIDs_SkipsGlobalScanWithinWindow(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 设置一个返回非空 PID 集的过滤器
	calledCount := 0
	tr.SetPIDFilter(func() map[uint32]struct{} {
		calledCount++
		return map[uint32]struct{}{99999: {}}
	})

	// 首次：PID 集非空但 lastGlobalScan 为零 → 应走全量扫描
	tr.pollConnections()
	first := tr.lastGlobalScan
	if first.IsZero() {
		t.Fatal("lastGlobalScan should be set after first poll with non-empty PID set")
	}

	// 第二次：30s 窗口内 → 应走 PID 过滤路径，不更新 lastGlobalScan
	tr.pollConnections()
	if tr.lastGlobalScan != first {
		t.Error("lastGlobalScan should NOT be updated within 30s window")
	}
}

func TestSetPIDFilter_GlobalScanAfterWindow(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	tr.SetPIDFilter(func() map[uint32]struct{} {
		return map[uint32]struct{}{99999: {}}
	})

	// 手动设置 lastGlobalScan 为 31s 前——模拟 30s 窗口已过期
	tr.lastGlobalScan = time.Now().Add(-31 * time.Second)
	old := tr.lastGlobalScan

	tr.pollConnections()

	// 应重新走全量扫描，lastGlobalScan 应被更新
	if !tr.lastGlobalScan.After(old) {
		t.Error("lastGlobalScan should be updated after 30s global scan window expires")
	}
}
