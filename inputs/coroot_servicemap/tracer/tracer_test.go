package tracer

import (
	"context"
	"strings"
	"testing"

	gopsnet "github.com/shirou/gopsutil/v3/net"
)

func TestEventType_String(t *testing.T) {
	tests := []struct {
		eventType EventType
		expected  string
	}{
		{EventTypeProcessStart, "ProcessStart"},
		{EventTypeConnectionOpen, "ConnectionOpen"},
		{EventTypeConnectionClose, "ConnectionClose"},
		{EventTypeTCPRetransmit, "TCPRetransmit"},
	}

	for _, tt := range tests {
		if got := tt.eventType.String(); got != tt.expected {
			t.Errorf("EventType(%d).String() = %s, want %s", tt.eventType, got, tt.expected)
		}
	}
}

func TestNewTracer(t *testing.T) {
	// 注意：这个测试需要在 Linux 系统上运行
	// 在其他系统上会跳过
	tracer, err := NewTracer(context.Background(), 0, 0, true, 0)
	if err != nil {
		t.Skipf("NewTracer failed (expected on non-Linux): %v", err)
	}

	if tracer == nil {
		t.Error("NewTracer returned nil without error")
	}

	if tracer.eventChan == nil {
		t.Error("eventChan is nil")
	}

	if tracer.closeChan == nil {
		t.Error("closeChan is nil")
	}

	// 清理
	if tracer != nil {
		tracer.Close()
	}
}

func TestIsTrackedTCPConnection(t *testing.T) {
	conn := gopsnet.ConnectionStat{
		Fd:     10,
		Status: "ESTABLISHED",
		Raddr:  gopsnet.Addr{IP: "10.0.0.2", Port: 443},
	}

	if !isTrackedTCPConnection(conn) {
		t.Fatal("expected ESTABLISHED tcp connection to be tracked")
	}

	conn.Status = "TIME_WAIT"
	if isTrackedTCPConnection(conn) {
		t.Fatal("expected TIME_WAIT to be ignored")
	}
}

func TestEndpoint(t *testing.T) {
	v4 := endpoint("127.0.0.1", 80)
	if v4 != "127.0.0.1:80" {
		t.Fatalf("unexpected endpoint: %s", v4)
	}

	v6 := endpoint("2001:db8::1", 443)
	if !strings.HasPrefix(v6, "[") || !strings.Contains(v6, "]:443") {
		t.Fatalf("unexpected ipv6 endpoint: %s", v6)
	}
}
