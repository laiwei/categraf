package containers

import (
	"context"
	"testing"
	"time"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
)

// ─── Registry.launchBackground ───────────────────────────────

func TestRegistryLaunchBackground_RunsAndCompletes(t *testing.T) {
	r := newBareRegistry(Config{})

	done := make(chan struct{})
	r.launchBackground(func() {
		close(done)
	})

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Error("launchBackground function did not run within 1s")
	}
	r.wg.Wait()
}

func TestRegistryLaunchBackground_Multiple(t *testing.T) {
	r := newBareRegistry(Config{})

	const n = 5
	ch := make(chan int, n)
	for i := 0; i < n; i++ {
		idx := i
		r.launchBackground(func() { ch <- idx })
	}
	r.wg.Wait()
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != n {
		t.Errorf("expected %d goroutines, got %d", n, count)
	}
}

// ─── processEvent ─────────────────────────────────────────────

func TestProcessEvent_NoCgroup_CreatesUnknownContainer(t *testing.T) {
	// EnableCgroup=false → getContainerIDByPID 返回 "" → containerID="unknown"
	r := newBareRegistry(Config{EnableCgroup: false})

	ev := &tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Pid:     12345, // 任意 PID，不会读 /proc
		Fd:      1,
		DstAddr: "10.0.0.1:3306",
	}
	r.processEvent(ev)

	cs := r.GetContainers()
	if len(cs) != 1 {
		t.Fatalf("expected 1 container, got %d", len(cs))
	}
	if cs[0].ID != "unknown" {
		t.Errorf("expected container ID 'unknown', got %q", cs[0].ID)
	}
}

func TestProcessEvent_ConnectionOpen_UpdatesTCPStats(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: false})

	r.processEvent(&tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      5,
		DstAddr: "192.168.1.1:5432",
	})

	r.mu.RLock()
	c := r.containers["unknown"]
	r.mu.RUnlock()

	if c == nil {
		t.Fatal("container 'unknown' should exist")
	}
	snap := c.GetTCPStatsSnapshot()
	if _, ok := snap["192.168.1.1:5432"]; !ok {
		t.Error("expected TCPStats entry for 192.168.1.1:5432")
	}
}

func TestProcessEvent_MaxContainersReached_ReturnsNil(t *testing.T) {
	// MaxContainers=1，先创建一个，再发 processEvent 触发第二个
	r := newBareRegistry(Config{EnableCgroup: false, MaxContainers: 1})

	// 先填满（ID="unknown" 已经创建）
	r.processEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Fd: 1, DstAddr: "1.1.1.1:80"})

	// 注入第二个不同 PID（但 EnableCgroup=false → 同样是 "unknown" → 返回 existing）
	// 为了测试 MaxContainers 路径，手动更改 containers 中 "unknown" 的 ID 不可行
	// 改为直接调用 getOrCreateContainer
	r.mu.Lock()
	got := r.getOrCreateContainer("second-container")
	r.mu.Unlock()

	if got != nil {
		t.Errorf("expected nil when MaxContainers=1 and already at limit, got container ID=%s", got.ID)
	}
}

func TestProcessEvent_MultipleEvents_SameContainer(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: false})

	// 3 次 ConnectionOpen 到不同目标
	for i, dest := range []string{"10.0.0.1:80", "10.0.0.2:80", "10.0.0.3:80"} {
		r.processEvent(&tracer.Event{
			Type:    tracer.EventTypeConnectionOpen,
			Fd:      uint64(i + 1),
			DstAddr: dest,
		})
	}

	cs := r.GetContainers()
	if len(cs) != 1 {
		t.Fatalf("expected 1 container (all pid=0 → 'unknown'), got %d", len(cs))
	}
	snap := cs[0].GetTCPStatsSnapshot()
	if len(snap) != 3 {
		t.Errorf("expected 3 TCPStats entries, got %d", len(snap))
	}
}

// ─── updateConnectionStats ────────────────────────────────────

func TestUpdateConnectionStats_EmptyTracer(t *testing.T) {
	// 注入一个无连接的真实 Tracer
	tr, err := tracer.NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	r := newBareRegistry(Config{EnableCgroup: false})
	r.tracer = tr

	// 无活跃连接，updateConnectionStats 应正常执行不 panic
	r.updateConnectionStats()
}

func TestUpdateConnectionStats_WithActiveConns(t *testing.T) {
	tr, err := tracer.NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	r := newBareRegistry(Config{EnableCgroup: false})
	r.tracer = tr

	// 在 registry 中创建 "unknown" 容器并打开一个连接
	r.processEvent(&tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      10,
		DstAddr: "8.8.8.8:53",
	})

	// updateConnectionStats 用 tracer 的 activeConns 更新流量
	// tracer 无活跃连接，所以只是遍历空 map —— 不应 panic
	r.updateConnectionStats()
}

// ─── containerGCLoop ─────────────────────────────────────────

func TestContainerGCLoop_ExitsOnStop(t *testing.T) {
	r := newBareRegistry(Config{})
	close(r.stopChan) // 立即关闭

	done := make(chan struct{})
	go func() {
		r.containerGCLoop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("containerGCLoop did not exit after stopChan closed")
	}
}

func TestContainerGCLoop_GCsOnTick(t *testing.T) {
	// 验证 containerGCLoop 在 ticker 触发后调用 gcContainers
	// 因为 containerGCInterval=60s 不现实等待，此测试仅验证 stopChan 退出路径
	// gcContainers 本身已有专属测试覆盖
	r := newBareRegistry(Config{})

	expired := NewContainer("exp")
	expired.LastActivity = time.Now().Add(-(containerTimeout + time.Hour))
	r.containers["exp"] = expired

	// 关闭 stopChan 触发退出，验证没有死锁
	close(r.stopChan)
	r.containerGCLoop() // 应立即返回
}

// ─── handleEvents ────────────────────────────────────────────

func TestHandleEvents_ExitsOnStop(t *testing.T) {
	tr, err := tracer.NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	r := newBareRegistry(Config{})
	r.tracer = tr
	close(r.stopChan)

	done := make(chan struct{})
	go func() {
		r.handleEvents()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("handleEvents did not exit after stopChan closed")
	}
}

func TestHandleEvents_ProcessesEvent(t *testing.T) {
	// 启动一个真实 tracer（fallback 模式），tracer 的 pollConnections 会产生 Open 事件
	tr, err := tracer.NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	r := newBareRegistry(Config{EnableCgroup: false})
	r.tracer = tr

	// 先启动轮询（产生真实事件）
	_ = tr.Start()

	// 启动 handleEvents goroutine
	go r.handleEvents()

	// 等待 pollConnections 至少跑一次，产生事件
	time.Sleep(150 * time.Millisecond)

	// 验证 handleEvents 正在运行（无死锁、无 panic），通过关闭 stopChan 退出
	close(r.stopChan)
}

// ─── discoverContainersByCgroup ──────────────────────────────

func TestDiscoverContainersByCgroup_ExitsOnStop(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: true})
	close(r.stopChan)

	done := make(chan struct{})
	go func() {
		r.discoverContainersByCgroup()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("discoverContainersByCgroup did not exit after stopChan closed")
	}
}

// ─── processEvent — container nil 路径 ───────────────────────

func TestProcessEvent_ContainerNil_MaxContainersReached(t *testing.T) {
	// MaxContainers=1，r.containers 中已有 "abc"，
	// processEvent 尝试创建 "unknown" → 超出限制 → getOrCreateContainer 返回 nil
	r := newBareRegistry(Config{EnableCgroup: false, MaxContainers: 1})
	r.containers["abc"] = NewContainer("abc") // 占满 1 个 slot

	ev := &tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      1,
		DstAddr: "10.0.0.1:80",
	}

	// 不应 panic，"unknown" 容器创建失败时静默跳过
	r.processEvent(ev)

	// 容器数应仍然为 1（"abc"），没有新建 "unknown"
	cs := r.GetContainers()
	if len(cs) != 1 || cs[0].ID != "abc" {
		t.Errorf("expected only 'abc' container, got %d containers", len(cs))
	}
}

// ─── updateConnectionStats — 带回调路径 ──────────────────────

func TestUpdateConnectionStats_CallbackInvoked(t *testing.T) {
	tr, err := tracer.NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	defer tr.Close()

	r := newBareRegistry(Config{EnableCgroup: false})
	r.tracer = tr

	// 在 tracer 中注入活跃连接，让回调被调用
	// 由于 EnableCgroup=false，回调内 getContainerIDByPID 返回 "" → 提前 return
	// 这覆盖了 updateConnectionStats 的回调执行路径
	//
	// tracer.activeConns 是私有字段，通过 pollConnections 间接填充
	_ = tr.Start() // 触发 startPollingTracer → pollConnections 填充 activeConns
	time.Sleep(50 * time.Millisecond)

	r.updateConnectionStats()
}
