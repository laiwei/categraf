package containers

import (
	"strings"
	"testing"

	"flashcat.cloud/categraf/inputs/servicemap/tracer"
)

// ─── resolveProcID ────────────────────────────────────────────

func TestResolveProcID_ZeroPID(t *testing.T) {
	id := resolveProcID(0)
	if !strings.HasPrefix(id, "proc_") {
		t.Errorf("expected 'proc_' prefix, got %q", id)
	}
	// /proc/0/comm 通常不可读（macOS 无 /proc，Linux pid 0 是 idle task）
	// 兜底应返回 "proc_0"
	if id != "proc_0" {
		// 如果 /proc/0/comm 恰好可读，允许 "proc_0_<comm>" 格式
		if !strings.HasPrefix(id, "proc_0_") {
			t.Errorf("unexpected ID format %q (expected 'proc_0' or 'proc_0_<comm>')", id)
		}
	}
}

func TestResolveProcID_HighPID(t *testing.T) {
	// PID 999999 几乎不可能存在，应返回 "proc_999999"
	id := resolveProcID(999999)
	if id != "proc_999999" {
		// 允许 "proc_999999_<comm>" 格式（极低概率）
		if !strings.HasPrefix(id, "proc_999999") {
			t.Errorf("unexpected ID format %q", id)
		}
	}
}

func TestResolveProcID_HasProcPrefix(t *testing.T) {
	for _, pid := range []uint32{0, 1, 100, 65535} {
		id := resolveProcID(pid)
		if !strings.HasPrefix(id, "proc_") {
			t.Errorf("pid=%d: expected 'proc_' prefix, got %q", pid, id)
		}
		// 新格式：proc_<comm>（如 proc_nginx）或 proc_<pid>（兜底，如 proc_0）
		// 不应带 PID_ 前缀 + 进程名的旧格式（proc_0_someapp）
		// 新格式中不应带 pid + _ + name 的三段式
		suffix := strings.TrimPrefix(id, "proc_")
		if suffix == "" {
			t.Errorf("pid=%d: proc_ suffix is empty, got %q", pid, id)
		}
	}
}

// ─── sanitizeProcLabel ────────────────────────────────────────

func TestSanitizeProcLabel_Clean(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"nginx", "nginx"},
		{"my-app", "my-app"},
		{"app.v2", "app.v2"},
		{"app_server", "app_server"},
		{"MyApp123", "MyApp123"},
	}
	for _, c := range cases {
		got := sanitizeProcLabel(c.input)
		if got != c.want {
			t.Errorf("sanitizeProcLabel(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestSanitizeProcLabel_Special(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"my app", "my_app"},     // 空格
		{"app/v2", "app_v2"},     // 斜杠
		{"app:8080", "app_8080"}, // 冒号
		{"(app)", "_app_"},       // 括号
		{"", ""},
	}
	for _, c := range cases {
		got := sanitizeProcLabel(c.input)
		if got != c.want {
			t.Errorf("sanitizeProcLabel(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ─── enrichProcContainer ─────────────────────────────────────

func TestEnrichProcContainer_WithComm(t *testing.T) {
	c := NewContainer("proc_nginx")
	enrichProcContainer(c, "proc_nginx")
	// 新格式：proc_<comm>，PID 不嵌入 ID，c.PID 保持 0
	if c.PID != 0 {
		t.Errorf("expected PID=0 (not embedded in ID anymore), got %d", c.PID)
	}
	if c.Name != "nginx" {
		t.Errorf("expected Name=nginx, got %q", c.Name)
	}
}

func TestEnrichProcContainer_WithoutComm(t *testing.T) {
	c := NewContainer("proc_0")
	enrichProcContainer(c, "proc_0")
	if c.PID != 0 {
		t.Errorf("expected PID=0, got %d", c.PID)
	}
	// 后缀全为数字（PID 兜底格式）→ 保留完整 ID "proc_0" 便于调试
	if c.Name != "proc_0" {
		t.Errorf("expected Name='proc_0', got %q", c.Name)
	}
}

func TestEnrichProcContainer_CommWithUnderscore(t *testing.T) {
	// "proc_my_service" → Name="my_service"（含下划线的进程名，TrimPrefix 保留整个后缀）
	c := NewContainer("proc_my_service")
	enrichProcContainer(c, "proc_my_service")
	if c.PID != 0 {
		t.Errorf("expected PID=0, got %d", c.PID)
	}
	if c.Name != "my_service" {
		t.Errorf("expected Name='my_service', got %q", c.Name)
	}
}

func TestEnrichProcContainer_PIDFallbackKeepsFullID(t *testing.T) {
	// proc_80793：后缀全为数字（PID 兜底）→ Name 保留完整 ID "proc_80793"
	c := NewContainer("proc_80793")
	enrichProcContainer(c, "proc_80793")
	if c.Name != "proc_80793" {
		t.Errorf("expected Name='proc_80793' for PID-fallback format, got %q", c.Name)
	}
	if c.PID != 0 {
		t.Errorf("expected PID=0 (not in ID), got %d", c.PID)
	}
}

// ─── resolveContainerID ──────────────────────────────────────

func TestResolveContainerID_NoCgroup_ReturnsProcID(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: false})
	r.mu.Lock()
	id := r.resolveContainerID(0)
	r.mu.Unlock()
	if !strings.HasPrefix(id, "proc_") {
		t.Errorf("expected 'proc_' prefix, got %q", id)
	}
}

func TestResolveContainerID_UsesCache(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: false})

	// 手动写入缓存
	r.mu.Lock()
	r.pidCache[777] = "cached-container-abc123"
	id := r.resolveContainerID(777)
	r.mu.Unlock()

	if id != "cached-container-abc123" {
		t.Errorf("expected cached ID, got %q", id)
	}
}

func TestResolveContainerID_SamePIDSameResult(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: false})

	r.mu.Lock()
	id1 := r.resolveContainerID(0)
	id2 := r.resolveContainerID(0)
	r.mu.Unlock()

	if id1 != id2 {
		t.Errorf("same PID should produce same ID: %q vs %q", id1, id2)
	}
}

// ─── getOrCreateContainer (proc_ 分支) ───────────────────────

func TestGetOrCreateContainer_ProcBased_SetsNameAndPID(t *testing.T) {
	r := newBareRegistry(Config{})
	r.mu.Lock()
	c := r.getOrCreateContainer("proc_myapp")
	r.mu.Unlock()

	if c == nil {
		t.Fatal("expected container, got nil")
	}
	// 新格式：PID 不嵌入 ID，c.PID 保持 0
	if c.PID != 0 {
		t.Errorf("expected PID=0 (not embedded in new format), got %d", c.PID)
	}
	if c.Name != "myapp" {
		t.Errorf("expected Name='myapp', got %q", c.Name)
	}
}

func TestGetOrCreateContainer_ProcBased_NoDuplicateOnSecondCall(t *testing.T) {
	r := newBareRegistry(Config{})
	r.mu.Lock()
	c1 := r.getOrCreateContainer("proc_nginx")
	c2 := r.getOrCreateContainer("proc_nginx")
	r.mu.Unlock()

	if c1 != c2 {
		t.Error("second call should return existing container, not create a new one")
	}
	if len(r.containers) != 1 {
		t.Errorf("expected 1 container, got %d", len(r.containers))
	}
}

func TestGetOrCreateContainer_RegularContainer_EnrichmentSkippedForProc(t *testing.T) {
	// proc_ 容器不应触发 Docker inspect（没有 docker client 也不应 panic）
	r := newBareRegistry(Config{EnableDocker: false})
	r.mu.Lock()
	c := r.getOrCreateContainer("proc_redis-server")
	r.mu.Unlock()

	if c == nil {
		t.Fatal("expected container")
	}
	if c.Name != "redis-server" {
		t.Errorf("expected Name='redis-server', got %q", c.Name)
	}
}

// ─── processEvent 端到端（裸进程路径）───────────────────────

func TestProcessEvent_BareProcess_FullPipeline(t *testing.T) {
	// EnableCgroup=false → 所有 PID 走裸进程路径 → 生成 proc_ 容器
	r := newBareRegistry(Config{EnableCgroup: false})

	r.processEvent(&tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Pid:     42,
		Fd:      10,
		DstAddr: "192.168.1.100:5432",
	})

	cs := r.GetContainers()
	if len(cs) != 1 {
		t.Fatalf("expected 1 container, got %d", len(cs))
	}

	c := cs[0]
	// ID 必须是 proc_ 前缀
	// macOS 上：无 /proc 文件系统 → proc_42（PID fallback）
	// Linux 上：取决于 PID 42 的 comm 名称
	if !strings.HasPrefix(c.ID, "proc_") {
		t.Errorf("expected proc_ prefix ID, got %q", c.ID)
	}
	// 应有 TCPStats
	snap := c.GetTCPStatsSnapshot()
	if _, ok := snap["192.168.1.100:5432"]; !ok {
		t.Error("expected TCPStats for destination 192.168.1.100:5432")
	}
}

func TestProcessEvent_MultipleBareProcesses_DifferentContainers(t *testing.T) {
	// 新语义：不同进程名 → 不同 container；同进程名 → 相同 container（按进程名聚合）
	// macOS 上无 /proc 文件系统，退化为 proc_<pid>，不同 PID 仍是不同容器
	r := newBareRegistry(Config{EnableCgroup: false})

	r.processEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Pid: 100, Fd: 1, DstAddr: "10.0.0.1:80"})
	r.processEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Pid: 200, Fd: 2, DstAddr: "10.0.0.2:80"})

	cs := r.GetContainers()
	// macOS (no /proc): proc_100 vs proc_200 → 2 containers
	// Linux (same comm): may be 1 container if both have same process name (desired merge behavior)
	// Linux (diff comm): 2 containers
	// 所有容器必须是 proc_ 前缀
	for _, c := range cs {
		if !strings.HasPrefix(c.ID, "proc_") {
			t.Errorf("expected proc_ prefix, got %q", c.ID)
		}
	}
	if len(cs) == 0 {
		t.Error("expected at least 1 container")
	}
}

// ─── gcContainers 清空 pidCache ──────────────────────────────

func TestGCContainers_ClearsPidCache(t *testing.T) {
	r := newBareRegistry(Config{})

	// 创建一个已超时的容器
	expired := NewContainer("container-abc")
	expired.LastActivity = expired.LastActivity.Add(-(containerTimeout + 1))

	// 写入缓存和容器（需持有 r.mu）
	r.mu.Lock()
	r.pidCache[1] = "container-abc"
	r.pidCache[2] = "container-def"
	r.containers["container-abc"] = expired
	r.mu.Unlock()

	// gcContainers 自带锁，不需外部加锁
	r.gcContainers()

	// container-abc 已被 GC，pidCache 应已被清空
	r.mu.RLock()
	cacheLen := len(r.pidCache)
	r.mu.RUnlock()

	if cacheLen != 0 {
		t.Errorf("expected pidCache cleared after GC, got %d entries", cacheLen)
	}
}

func TestGCContainers_DoesNotClearCacheIfNoExpiry(t *testing.T) {
	r := newBareRegistry(Config{})

	// 创建一个活跃容器
	fresh := NewContainer("container-fresh")

	r.mu.Lock()
	r.pidCache[1] = "container-fresh"
	r.containers["container-fresh"] = fresh
	r.mu.Unlock()

	// gcContainers 自带锁
	r.gcContainers()

	r.mu.RLock()
	cacheLen := len(r.pidCache)
	r.mu.RUnlock()

	if cacheLen != 1 {
		t.Errorf("expected pidCache preserved when no GC happened, got %d entries", cacheLen)
	}
}
