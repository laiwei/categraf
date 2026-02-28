package tracer

import (
	"testing"
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
	tracer, err := NewTracer(0, 0, true)
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
