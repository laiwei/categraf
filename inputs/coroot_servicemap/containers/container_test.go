package containers

import (
	"testing"
	"time"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
)

func TestNewContainer(t *testing.T) {
	container := NewContainer("test-container-123")

	if container.ID != "test-container-123" {
		t.Errorf("Expected ID test-container-123, got %s", container.ID)
	}

	if container.TCPStats == nil {
		t.Error("TCPStats is nil")
	}

	if container.HTTPStats == nil {
		t.Error("HTTPStats is nil")
	}

	if container.activeConnections == nil {
		t.Error("activeConnections is nil")
	}
}

func TestContainer_OnConnectionOpen(t *testing.T) {
	container := NewContainer("test")

	event := &tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      123,
		DstAddr: "10.0.0.1:80",
	}

	container.OnEvent(event)

	// 检查统计
	if len(container.TCPStats) != 1 {
		t.Errorf("Expected 1 destination, got %d", len(container.TCPStats))
	}

	for _, stats := range container.TCPStats {
		if stats.SuccessfulConnects != 1 {
			t.Errorf("Expected 1 connect, got %d", stats.SuccessfulConnects)
		}

		if stats.ActiveConnections != 1 {
			t.Errorf("Expected 1 active, got %d", stats.ActiveConnections)
		}
	}

	// 检查活跃连接
	container.mu.RLock()
	if len(container.activeConnections) != 1 {
		t.Errorf("Expected 1 active connection, got %d", len(container.activeConnections))
	}
	container.mu.RUnlock()
}

func TestContainer_OnConnectionClose(t *testing.T) {
	container := NewContainer("test")

	// 先打开连接
	openEvent := &tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      123,
		DstAddr: "10.0.0.1:80",
	}
	container.OnEvent(openEvent)

	// 等待一小段时间
	time.Sleep(10 * time.Millisecond)

	// 关闭连接
	closeEvent := &tracer.Event{
		Type:    tracer.EventTypeConnectionClose,
		Fd:      123,
		DstAddr: "10.0.0.1:80",
	}
	container.OnEvent(closeEvent)

	// 检查统计
	for _, stats := range container.TCPStats {
		if stats.ActiveConnections != 0 {
			t.Errorf("Expected 0 active connections, got %d", stats.ActiveConnections)
		}

		if stats.TotalTime == 0 {
			t.Error("TotalTime should not be 0")
		}
	}

	// 检查活跃连接已删除
	container.mu.RLock()
	if len(container.activeConnections) != 0 {
		t.Errorf("Expected 0 active connections, got %d", len(container.activeConnections))
	}
	container.mu.RUnlock()
}

func TestContainer_OnRetransmit(t *testing.T) {
	container := NewContainer("test")

	event := &tracer.Event{
		Type:    tracer.EventTypeTCPRetransmit,
		DstAddr: "10.0.0.1:80",
	}

	container.OnEvent(event)

	// 检查重传统计
	for _, stats := range container.TCPStats {
		if stats.Retransmissions != 1 {
			t.Errorf("Expected 1 retransmission, got %d", stats.Retransmissions)
		}
	}
}
