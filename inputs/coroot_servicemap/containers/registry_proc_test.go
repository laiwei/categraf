package containers

import (
	"fmt"
	"strings"
	"testing"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
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
		// 格式验证：proc_<digits>[_<name>]
		expected := fmt.Sprintf("proc_%d", pid)
		if id != expected && !strings.HasPrefix(id, expected+"_") {
			t.Errorf("pid=%d: unexpected format %q", pid, id)
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
	c := NewContainer("proc_123_nginx")
	enrichProcContainer(c, "proc_123_nginx")
	if c.PID != 123 {
		t.Errorf("expected PID=123, got %d", c.PID)
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
	// 无进程名时，Name 应为完整 ID
	if c.Name != "proc_0" {
		t.Errorf("expected Name='proc_0', got %q", c.Name)
	}
}

func TestEnrichProcContainer_CommWithUnderscore(t *testing.T) {
	// "proc_42_my_service" → PID=42, Name="my_service"（SplitN 保留第三段整体）
	c := NewContainer("proc_42_my_service")
	enrichProcContainer(c, "proc_42_my_service")
	if c.PID != 42 {
		t.Errorf("expected PID=42, got %d", c.PID)
	}
	if c.Name != "my_service" {
		t.Errorf("expected Name='my_service', got %q", c.Name)
	}
}

func TestEnrichProcContainer_InvalidPID(t *testing.T) {
	// 格式不合法时，PID 保持 0
	c := NewContainer("proc_abc_nginx")
	enrichProcContainer(c, "proc_abc_nginx")
	if c.PID != 0 {
		t.Errorf("expected PID=0 for invalid format, got %d", c.PID)
	}
	if c.Name != "nginx" {
		t.Errorf("expected Name='nginx', got %q", c.Name)
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
	c := r.getOrCreateContainer("proc_42_myapp")
	r.mu.Unlock()

	if c == nil {
		t.Fatal("expected container, got nil")
	}
	if c.PID != 42 {
		t.Errorf("expected PID=42, got %d", c.PID)
	}
	if c.Name != "myapp" {
		t.Errorf("expected Name='myapp', got %q", c.Name)
	}
}

func TestGetOrCreateContainer_ProcBased_NoDuplicateOnSecondCall(t *testing.T) {
	r := newBareRegistry(Config{})
	r.mu.Lock()
	c1 := r.getOrCreateContainer("proc_10_nginx")
	c2 := r.getOrCreateContainer("proc_10_nginx")
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
	c := r.getOrCreateContainer("proc_99_redis-server")
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
	if !strings.HasPrefix(c.ID, "proc_42") {
		t.Errorf("expected proc_42* ID, got %q", c.ID)
	}
	// 应有 TCPStats
	snap := c.GetTCPStatsSnapshot()
	if _, ok := snap["192.168.1.100:5432"]; !ok {
		t.Error("expected TCPStats for destination 192.168.1.100:5432")
	}
}

func TestProcessEvent_MultipleBareProcesses_DifferentContainers(t *testing.T) {
	// 不同 PID 的裸进程应得到不同 container
	r := newBareRegistry(Config{EnableCgroup: false})

	r.processEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Pid: 100, Fd: 1, DstAddr: "10.0.0.1:80"})
	r.processEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Pid: 200, Fd: 2, DstAddr: "10.0.0.2:80"})

	cs := r.GetContainers()
	if len(cs) != 2 {
		t.Fatalf("expected 2 containers (different PIDs), got %d", len(cs))
	}
	ids := map[string]bool{cs[0].ID: true, cs[1].ID: true}
	for id := range ids {
		if !strings.HasPrefix(id, "proc_") {
			t.Errorf("expected proc_ prefix, got %q", id)
		}
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
