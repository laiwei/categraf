//go:build linux

package containers

// registry_linux_test.go — Linux 平台专项测试
// 测试目标：
//   1. getContainerIDByPID — 读取 /proc/PID/cgroup，使用当前进程 PID
//   2. getContainerIDByPID — EnableCgroup=false 时跳过
//   3. discoverContainersByCgroup — 启动后可被 stopChan 关闭
//   4. NewRegistry — 在 Linux 上初始化（无 Docker/K8s）

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestGetContainerIDByPID_Linux_SelfPID 使用当前进程 PID 读取 /proc/PID/cgroup。
// 在容器内运行时可能拿到真实 container ID；宿主机上通常返回 ""。
func TestGetContainerIDByPID_Linux_SelfPID(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: true})

	pid := uint32(os.Getpid())
	cgroupPath := fmt.Sprintf("/proc/%d/cgroup", pid)
	if _, err := os.Stat(cgroupPath); err != nil {
		t.Skipf("cgroup file not accessible: %v", err)
	}

	id := r.getContainerIDByPID(pid)
	// 宿主机上 id="" 是正常的，容器内会是 64 位 hex 串
	t.Logf("PID=%d → containerID=%q", pid, id)
}

// TestGetContainerIDByPID_Linux_DisabledCgroup EnableCgroup=false 时应立即返回 ""。
func TestGetContainerIDByPID_Linux_DisabledCgroup(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: false})

	id := r.getContainerIDByPID(uint32(os.Getpid()))
	if id != "" {
		t.Errorf("expected empty ID with EnableCgroup=false, got %q", id)
	}
}

// TestGetContainerIDByPID_Linux_NonexistentPID 使用不存在的 PID，应返回 ""。
func TestGetContainerIDByPID_Linux_NonexistentPID(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: true})

	// PID 2^31-1 几乎不可能存在
	id := r.getContainerIDByPID(2147483647)
	if id != "" {
		t.Errorf("expected empty ID for nonexistent PID, got %q", id)
	}
}

// TestDiscoverContainersByCgroup_Linux_ShutdownViaStopChan
// 验证 discoverContainersByCgroup 在 stopChan 关闭后能干净退出。
func TestDiscoverContainersByCgroup_Linux_ShutdownViaStopChan(t *testing.T) {
	r := newBareRegistry(Config{EnableCgroup: true})

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.discoverContainersByCgroup()
	}()

	// 给 goroutine 一小段时间进入 select
	time.Sleep(20 * time.Millisecond)

	// 触发关闭
	r.Close()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("discoverContainersByCgroup did not exit after stopChan closed")
	}
}

// TestNewRegistry_Linux_MinimalConfig 在 Linux 上用最小配置创建 Registry，
// 不启用 Docker/K8s，验证不 panic 并能正常关闭。
func TestNewRegistry_Linux_MinimalConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		EnableDocker: false,
		EnableK8s:    false,
		EnableCgroup: false,
	}

	// NewRegistry 需要一个 tracer，但我们可以传 nil（代码有 nil 检查）
	r, err := NewRegistry(ctx, nil, cfg)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil Registry")
	}

	// 应该能正常关闭
	r.Close()
}

// TestNewRegistry_Linux_ContextCancel 验证 context 取消能关闭 Registry。
func TestNewRegistry_Linux_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	cfg := Config{EnableDocker: false, EnableK8s: false, EnableCgroup: false}
	r, err := NewRegistry(ctx, nil, cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	cancel() // 触发 context 取消

	// stopChan 应该在 context 取消后关闭（给后台 goroutine 一点时间）
	select {
	case <-r.stopChan:
		// OK
	case <-time.After(500 * time.Millisecond):
		t.Error("Registry stopChan not closed after context cancellation")
	}

	r.Close() // 幂等
}

// TestExtractContainerID_Linux_RealCgroupLine 测试真实 Linux cgroup 行格式。
func TestExtractContainerID_Linux_RealCgroupLine(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		wantID bool
	}{
		{
			name:   "docker v1 format",
			line:   "12:cpu:/docker/a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			wantID: true,
		},
		{
			name:   "containerd cgroup v2",
			line:   "0::/system.slice/cri-containerd-abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890.scope",
			wantID: true,
		},
		{
			name:   "systemd slice",
			line:   "0::/system.slice/docker.service",
			wantID: false,
		},
		{
			name:   "bare host process",
			line:   "0::/user.slice/user-1000.slice/session-1.scope",
			wantID: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantID: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := extractContainerID(tc.line)
			got := id != ""
			if got != tc.wantID {
				t.Errorf("extractContainerID(%q) = %q, wantID=%v", tc.line, id, tc.wantID)
			}
		})
	}
}
