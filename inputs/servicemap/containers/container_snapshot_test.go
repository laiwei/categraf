package containers

import (
	"sync"
	"testing"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/l7"
	"flashcat.cloud/categraf/inputs/servicemap/tracer"
)

// ─── GetTCPStatsSnapshot ─────────────────────────────────────

func TestGetTCPStatsSnapshot_Empty(t *testing.T) {
	c := NewContainer("c1")
	snap := c.GetTCPStatsSnapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot, got %d entries", len(snap))
	}
}

func TestGetTCPStatsSnapshot_DeepCopy(t *testing.T) {
	c := NewContainer("c2")
	c.TCPStats["10.0.0.1:3306"] = &TCPStats{
		DestinationAddr:    "10.0.0.1:3306",
		SuccessfulConnects: 10,
		BytesSent:          4096,
	}

	snap := c.GetTCPStatsSnapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}

	// 修改快照不应影响原始数据
	snap["10.0.0.1:3306"].SuccessfulConnects = 999
	if c.TCPStats["10.0.0.1:3306"].SuccessfulConnects != 10 {
		t.Error("snapshot modification should not affect original")
	}
}

func TestGetTCPStatsSnapshot_NilEntry(t *testing.T) {
	c := NewContainer("c3")
	// 直接注入 nil 值，测试 nil 过滤
	c.TCPStats["bad"] = nil
	c.TCPStats["good"] = &TCPStats{DestinationAddr: "good", SuccessfulConnects: 1}

	snap := c.GetTCPStatsSnapshot()
	if _, ok := snap["bad"]; ok {
		t.Error("nil TCPStats entry should be filtered from snapshot")
	}
	if _, ok := snap["good"]; !ok {
		t.Error("non-nil entry should be in snapshot")
	}
}

func TestGetTCPStatsSnapshot_AllFields(t *testing.T) {
	c := NewContainer("c4")
	c.TCPStats["dst"] = &TCPStats{
		DestinationAddr:    "dst",
		SuccessfulConnects: 1,
		FailedConnects:     2,
		ActiveConnections:  3,
		Retransmissions:    4,
		TotalTime:          500,
		MaxTime:            100,
		MinTime:            10,
		BytesSent:          8192,
		BytesReceived:      16384,
	}

	snap := c.GetTCPStatsSnapshot()
	s := snap["dst"]
	if s == nil {
		t.Fatal("snapshot entry is nil")
	}
	if s.FailedConnects != 2 || s.ActiveConnections != 3 || s.Retransmissions != 4 {
		t.Error("snapshot fields mismatch")
	}
	if s.MaxTime != 100 || s.MinTime != 10 || s.TotalTime != 500 {
		t.Error("time fields mismatch")
	}
	if s.BytesSent != 8192 || s.BytesReceived != 16384 {
		t.Error("byte fields mismatch")
	}
}

func TestGetTCPStatsSnapshot_Concurrent(t *testing.T) {
	c := NewContainer("c5")
	c.TCPStats["10.0.0.1:80"] = &TCPStats{SuccessfulConnects: 1}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = c.GetTCPStatsSnapshot()
		}()
		go func(n int) {
			defer wg.Done()
			key := "10.0.0.2:80"
			c.mu.Lock()
			c.TCPStats[key] = &TCPStats{SuccessfulConnects: uint64(n)}
			c.mu.Unlock()
		}(i)
	}
	wg.Wait()
}

// ─── ActiveConnectionCount ────────────────────────────────────

func TestActiveConnectionCount_Empty(t *testing.T) {
	c := NewContainer("a1")
	if n := c.ActiveConnectionCount(); n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestActiveConnectionCount_AfterOpen(t *testing.T) {
	c := NewContainer("a2")
	ev := &tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      1,
		DstAddr: "10.0.0.1:80",
	}
	c.OnEvent(ev)
	if n := c.ActiveConnectionCount(); n != 1 {
		t.Errorf("expected 1 after open, got %d", n)
	}
}

func TestActiveConnectionCount_AfterOpenClose(t *testing.T) {
	c := NewContainer("a3")
	open := &tracer.Event{Type: tracer.EventTypeConnectionOpen, Fd: 10, DstAddr: "10.0.0.1:8080"}
	close := &tracer.Event{Type: tracer.EventTypeConnectionClose, Fd: 10}
	c.OnEvent(open)
	c.OnEvent(close)
	if n := c.ActiveConnectionCount(); n != 0 {
		t.Errorf("expected 0 after close, got %d", n)
	}
}

func TestActiveConnectionCount_MultipleConns(t *testing.T) {
	c := NewContainer("a4")
	for i := uint64(0); i < 5; i++ {
		c.OnEvent(&tracer.Event{
			Type:    tracer.EventTypeConnectionOpen,
			Fd:      i,
			DstAddr: "10.0.0.1:80",
		})
	}
	if n := c.ActiveConnectionCount(); n != 5 {
		t.Errorf("expected 5, got %d", n)
	}
}

// ─── GetHTTPStatsSnapshot / GetL7StatsSnapshot 快照完整性 ─────

func TestGetHTTPStatsSnapshot_NilFilter(t *testing.T) {
	c := NewContainer("h1")
	c.HTTPStats["bad"] = nil
	c.HTTPStats["good"] = &HTTPStats{RequestCount: 1}

	snap := c.GetHTTPStatsSnapshot()
	if _, ok := snap["bad"]; ok {
		t.Error("nil HTTPStats should be filtered")
	}
	if _, ok := snap["good"]; !ok {
		t.Error("valid HTTPStats should appear in snapshot")
	}
}

func TestGetL7StatsSnapshot_NilFilter(t *testing.T) {
	c := NewContainer("l1")
	c.L7Stats["bad"] = nil
	c.L7Stats["good"] = &L7Stats{RequestCount: 2}

	snap := c.GetL7StatsSnapshot()
	if _, ok := snap["bad"]; ok {
		t.Error("nil L7Stats should be filtered")
	}
}

// ─── UpdateTrafficStats 边界 ──────────────────────────────────

func TestUpdateTrafficStats_NoTrackedFD(t *testing.T) {
	c := NewContainer("u1")
	// fd=99 未被 onConnectionOpen 追踪，应静默忽略
	c.UpdateTrafficStats(99, 1024, 2048, 0)
	if len(c.TCPStats) != 0 {
		t.Error("expected no TCP stats for untracked FD")
	}
}

func TestUpdateTrafficStats_Delta(t *testing.T) {
	c := NewContainer("u2")
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Fd: 1, DstAddr: "1.2.3.4:80"})

	// 第一次更新
	c.UpdateTrafficStats(1, 100, 200, 0)
	// 第二次更新（增量 = 150-100=50, 300-200=100）
	c.UpdateTrafficStats(1, 150, 300, 0)

	c.mu.RLock()
	stats := c.TCPStats["1.2.3.4:80"]
	c.mu.RUnlock()

	if stats == nil {
		t.Fatal("no TCP stats for destination")
	}
	if stats.BytesSent != 150 {
		t.Errorf("BytesSent: want 150, got %d", stats.BytesSent)
	}
	if stats.BytesReceived != 300 {
		t.Errorf("BytesReceived: want 300, got %d", stats.BytesReceived)
	}
}

func TestUpdateTrafficStats_NoDecrement(t *testing.T) {
	c := NewContainer("u3")
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Fd: 2, DstAddr: "5.5.5.5:443"})

	c.UpdateTrafficStats(2, 100, 100, 0)
	// 减小不应产生负的增量
	c.UpdateTrafficStats(2, 50, 50, 0)

	c.mu.RLock()
	stats := c.TCPStats["5.5.5.5:443"]
	c.mu.RUnlock()

	// 只有第一次更新（delta 100），第二次减小应被忽略
	if stats.BytesSent != 100 || stats.BytesReceived != 100 {
		t.Errorf("decreasing counters should not reduce stats: sent=%d recv=%d",
			stats.BytesSent, stats.BytesReceived)
	}
}

// ─── onRetransmit 边界 ────────────────────────────────────────

func TestOnRetransmit_UnknownDest(t *testing.T) {
	c := NewContainer("r1")
	// 目标地址为空，应被忽略
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeTCPRetransmit, DstAddr: ""})
	if len(c.TCPStats) != 0 {
		t.Error("empty DstAddr retransmit should not create stats")
	}
}

func TestOnRetransmit_CreatesStats(t *testing.T) {
	c := NewContainer("r2")
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeTCPRetransmit, DstAddr: "10.0.0.1:80"})
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeTCPRetransmit, DstAddr: "10.0.0.1:80"})

	if c.TCPStats["10.0.0.1:80"] == nil {
		t.Fatal("retransmit should create TCPStats entry")
	}
	if c.TCPStats["10.0.0.1:80"].Retransmissions != 2 {
		t.Errorf("expected 2 retransmissions, got %d", c.TCPStats["10.0.0.1:80"].Retransmissions)
	}
}

// ─── onConnectionClose 边界：未知 FD ─────────────────────────

func TestOnConnectionClose_UnknownFD(t *testing.T) {
	c := NewContainer("cl1")
	// 未经过 Open 直接 Close，应静默忽略（不 panic，不修改状态）
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeConnectionClose, Fd: 42})
	if len(c.TCPStats) != 0 {
		t.Error("close of unknown fd should not affect stats")
	}
}

func TestOnConnectionClose_DurationTracking(t *testing.T) {
	c := NewContainer("cl2")
	c.OnEvent(&tracer.Event{Type: tracer.EventTypeConnectionOpen, Fd: 5, DstAddr: "9.9.9.9:53"})

	time.Sleep(5 * time.Millisecond) // 确保有一点时间差

	c.OnEvent(&tracer.Event{Type: tracer.EventTypeConnectionClose, Fd: 5})

	s := c.TCPStats["9.9.9.9:53"]
	if s == nil {
		t.Fatal("close should not remove TCPStats entry")
	}
	if s.TotalTime == 0 {
		t.Error("expected non-zero TotalTime after close")
	}
	if s.ActiveConnections != 0 {
		t.Errorf("expected 0 active connections after close, got %d", s.ActiveConnections)
	}
}

// ─── L7 MethodStatementClose 被忽略 ──────────────────────────

func TestObserveL7Stats_IgnoresStatementClose(t *testing.T) {
	c := NewContainer("lm1")
	ev := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:5432",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolPostgres,
			Method:   l7.MethodStatementClose,
		},
	}
	c.OnEvent(ev)
	if len(c.L7Stats) != 0 {
		t.Error("MethodStatementClose should be ignored and produce no L7Stats")
	}
}

// ─── onL7Request 不支持的协议 ────────────────────────────────

func TestOnL7Request_UnknownProtocol(t *testing.T) {
	c := NewContainer("lp1")
	ev := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:9999",
		L7Request: &l7.RequestData{
			Protocol: l7.Protocol(255), // 未知协议
		},
	}
	// 不应 panic
	c.OnEvent(ev)
	if len(c.L7Stats) != 0 {
		t.Error("unknown protocol should not create L7Stats")
	}
}

func TestOnL7Request_NilPayload(t *testing.T) {
	c := NewContainer("lp2")
	ev := &tracer.Event{
		Type:      tracer.EventTypeL7Request,
		L7Request: nil, // nil L7Request
	}
	// 不应 panic
	c.OnEvent(ev)
}

// ─── LastActivity 更新 ────────────────────────────────────────

func TestLastActivity_UpdatedOnEvent(t *testing.T) {
	c := NewContainer("la1")
	before := c.LastActivity

	time.Sleep(2 * time.Millisecond)

	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      1,
		DstAddr: "10.0.0.1:80",
	})

	if !c.LastActivity.After(before) {
		t.Error("LastActivity should be updated after OnEvent")
	}
}
