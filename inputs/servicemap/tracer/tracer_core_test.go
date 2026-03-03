package tracer

import (
	"bytes"
	"compress/gzip"
	"context"
	"sync"
	"testing"
	"time"

	gopsnet "github.com/shirou/gopsutil/v3/net"
)

// ─── EventType.String 全枚举测试 ──────────────────────────────

func TestEventType_String_AllVariants(t *testing.T) {
	cases := []struct {
		et   EventType
		want string
	}{
		{EventTypeProcessStart, "ProcessStart"},
		{EventTypeProcessExit, "ProcessExit"},
		{EventTypeConnectionOpen, "ConnectionOpen"},
		{EventTypeConnectionClose, "ConnectionClose"},
		{EventTypeListenOpen, "ListenOpen"},
		{EventTypeListenClose, "ListenClose"},
		{EventTypeTCPRetransmit, "TCPRetransmit"},
		{EventTypeL7Request, "L7Request"},
		{EventType(99), "Unknown(99)"},
		{EventType(0), "Unknown(0)"},
	}
	for _, c := range cases {
		if got := c.et.String(); got != c.want {
			t.Errorf("EventType(%d).String() = %q, want %q", c.et, got, c.want)
		}
	}
}

// ─── handleListenEvent ────────────────────────────────────────

func newTestTracer(t *testing.T) *Tracer {
	t.Helper()
	tr, err := NewTracer(context.Background(), -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	return tr
}

func TestHandleListenEvent_Open(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	ev := &Event{
		Type:    EventTypeListenOpen,
		SrcPort: 8080,
		SrcAddr: "0.0.0.0:8080",
	}
	tr.handleListenEvent(ev)

	if !tr.IsListening(8080) {
		t.Error("expected port 8080 to be listening after ListenOpen")
	}
}

func TestHandleListenEvent_Close(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 先注册再关闭
	open := &Event{Type: EventTypeListenOpen, SrcPort: 9090, SrcAddr: "0.0.0.0:9090"}
	tr.handleListenEvent(open)

	close := &Event{Type: EventTypeListenClose, SrcPort: 9090, SrcAddr: "0.0.0.0:9090"}
	tr.handleListenEvent(close)

	if tr.IsListening(9090) {
		t.Error("expected port 9090 NOT to be listening after ListenClose")
	}
}

func TestHandleListenEvent_Nil(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()
	// 不应 panic
	tr.handleListenEvent(nil)
}

func TestHandleListenEvent_NonListenType(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// ConnectionOpen 不应修改 listenPorts
	ev := &Event{Type: EventTypeConnectionOpen, SrcPort: 7777}
	tr.handleListenEvent(ev)

	if tr.IsListening(7777) {
		t.Error("ConnectionOpen should not add listen port")
	}
}

func TestHandleListenEvent_MultipleAddresses(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 同一端口两个地址
	tr.handleListenEvent(&Event{Type: EventTypeListenOpen, SrcPort: 443, SrcAddr: "0.0.0.0:443"})
	tr.handleListenEvent(&Event{Type: EventTypeListenOpen, SrcPort: 443, SrcAddr: "127.0.0.1:443"})

	if !tr.IsListening(443) {
		t.Error("expected port 443 to be listening")
	}

	// GetListenPorts 应该去重
	ports := tr.GetListenPorts()
	if _, ok := ports[443]; !ok {
		t.Error("port 443 should be in GetListenPorts")
	}
}

// ─── IsListening / GetListenPorts 边界 ───────────────────────

func TestIsListening_Empty(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	if tr.IsListening(80) {
		t.Error("empty tracer should not be listening on any port")
	}
}

func TestGetListenPorts_Empty(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	ports := tr.GetListenPorts()
	if len(ports) != 0 {
		t.Errorf("expected empty port map, got %d ports", len(ports))
	}
}

func TestGetListenPorts_ReturnsCopy(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	tr.handleListenEvent(&Event{Type: EventTypeListenOpen, SrcPort: 1234})
	ports := tr.GetListenPorts()
	// 修改返回值不应影响内部状态
	delete(ports, 1234)

	if !tr.IsListening(1234) {
		t.Error("modifying returned map should not affect tracer state")
	}
}

// ─── ForEachActiveConnection / ActiveConnectionCount ─────────

func TestForEachActiveConnection_Empty(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	count := 0
	tr.ForEachActiveConnection(func(id ConnectionID, conn Connection) {
		count++
	})

	if count != 0 {
		t.Errorf("expected 0 connections, got %d", count)
	}
}

func TestActiveConnectionCount_Zero(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	if n := tr.ActiveConnectionCount(); n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestForEachActiveConnection_WithConnections(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 直接注入活跃连接
	tr.activeConnMu.Lock()
	tr.activeConns[ConnectionID{FD: 1, PID: 100}] = Connection{
		Timestamp: 1000,
		LastSeen:  time.Now(),
		BytesSent: 512,
	}
	tr.activeConns[ConnectionID{FD: 2, PID: 200}] = Connection{
		Timestamp: 2000,
		LastSeen:  time.Now(),
		BytesSent: 1024,
	}
	tr.activeConnMu.Unlock()

	if n := tr.ActiveConnectionCount(); n != 2 {
		t.Errorf("expected 2, got %d", n)
	}

	var seen []ConnectionID
	tr.ForEachActiveConnection(func(id ConnectionID, conn Connection) {
		seen = append(seen, id)
	})

	if len(seen) != 2 {
		t.Errorf("ForEachActiveConnection saw %d connections, want 2", len(seen))
	}
}

// ─── gcConnections ────────────────────────────────────────────

func TestGcConnections_RemovesExpired(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	old := time.Now().Add(-(connectionTimeout + time.Second))
	fresh := time.Now()

	tr.activeConnMu.Lock()
	tr.activeConns[ConnectionID{FD: 1}] = Connection{LastSeen: old}   // 过期
	tr.activeConns[ConnectionID{FD: 2}] = Connection{LastSeen: fresh} // 活跃
	tr.activeConnMu.Unlock()

	tr.gcConnections()

	if n := tr.ActiveConnectionCount(); n != 1 {
		t.Errorf("expected 1 active connection after GC, got %d", n)
	}
}

func TestGcConnections_KeepsZeroTime(t *testing.T) {
	// LastSeen.IsZero() 的连接不应被清除
	tr := newTestTracer(t)
	defer tr.Close()

	tr.activeConnMu.Lock()
	tr.activeConns[ConnectionID{FD: 99}] = Connection{} // LastSeen is zero
	tr.activeConnMu.Unlock()

	tr.gcConnections()

	if n := tr.ActiveConnectionCount(); n != 1 {
		t.Errorf("zero-time connection should not be GC'd, got %d remaining", n)
	}
}

func TestGcConnections_NilActiveConns(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	tr.activeConnMu.Lock()
	tr.activeConns = nil
	tr.activeConnMu.Unlock()

	// 不应 panic
	tr.gcConnections()
}

func TestGcConnections_AllExpired(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	old := time.Now().Add(-(connectionTimeout + time.Hour))

	tr.activeConnMu.Lock()
	for i := uint64(0); i < 5; i++ {
		tr.activeConns[ConnectionID{FD: i}] = Connection{LastSeen: old}
	}
	tr.activeConnMu.Unlock()

	tr.gcConnections()

	if n := tr.ActiveConnectionCount(); n != 0 {
		t.Errorf("expected all connections GC'd, got %d", n)
	}
}

// ─── emitEventLocked ──────────────────────────────────────────

func TestEmitEventLocked_Delivered(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	ev := Event{Type: EventTypeConnectionOpen, Pid: 42}
	tr.emitEventLocked(ev)

	select {
	case got := <-tr.eventChan:
		if got.Pid != 42 {
			t.Errorf("got pid %d, want 42", got.Pid)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("event not delivered within timeout")
	}
}

func TestEmitEventLocked_DropOnFull(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 填满通道
	for i := 0; i < cap(tr.eventChan); i++ {
		tr.emitEventLocked(Event{Pid: uint32(i)})
	}

	// 再发一条不应 panic 也不应阻塞
	tr.emitEventLocked(Event{Pid: 9999})
}

// ─── Events() 通道暴露 ────────────────────────────────────────

func TestEvents_ReturnsSameChannel(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	ch := tr.Events()
	if ch == nil {
		t.Error("Events() returned nil channel")
	}

	// 发送一个事件，从 Events() 取出
	tr.emitEventLocked(Event{Pid: 7})
	select {
	case ev := <-ch:
		if ev.Pid != 7 {
			t.Errorf("got pid %d via Events(), want 7", ev.Pid)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("event not available via Events() channel")
	}
}

// ─── connectionFD 哈希稳定性 ─────────────────────────────────

func TestConnectionFD_RealFD(t *testing.T) {
	c := gopsnet.ConnectionStat{Fd: 1234}
	if fd := connectionFD(c); fd != 1234 {
		t.Errorf("expected 1234, got %d", fd)
	}
}

func TestConnectionFD_FallbackHash(t *testing.T) {
	// Fd=0 时应使用四元组哈希
	c := gopsnet.ConnectionStat{
		Fd:    0,
		Pid:   100,
		Laddr: gopsnet.Addr{IP: "10.0.0.1", Port: 12345},
		Raddr: gopsnet.Addr{IP: "10.0.0.2", Port: 80},
	}
	fd1 := connectionFD(c)
	fd2 := connectionFD(c)
	if fd1 == 0 {
		t.Error("expected non-zero hash FD")
	}
	if fd1 != fd2 {
		t.Error("connectionFD must be deterministic")
	}
}

func TestConnectionFD_DifferentConns_DifferentIDs(t *testing.T) {
	c1 := gopsnet.ConnectionStat{
		Fd:    0,
		Pid:   1,
		Laddr: gopsnet.Addr{IP: "10.0.0.1", Port: 1111},
		Raddr: gopsnet.Addr{IP: "10.0.0.2", Port: 80},
	}
	c2 := gopsnet.ConnectionStat{
		Fd:    0,
		Pid:   2,
		Laddr: gopsnet.Addr{IP: "10.0.0.1", Port: 2222},
		Raddr: gopsnet.Addr{IP: "10.0.0.2", Port: 80},
	}
	if connectionFD(c1) == connectionFD(c2) {
		t.Error("different connections should produce different FDs")
	}
}

// ─── isTrackedTCPConnection 边界条件 ─────────────────────────

func TestIsTrackedTCPConnection_AllTrackedStatuses(t *testing.T) {
	statuses := []string{"ESTABLISHED", "SYN_SENT", "SYN_RECV",
		"established", "syn_sent"} // 小写也应匹配

	for _, s := range statuses {
		c := gopsnet.ConnectionStat{
			Status: s,
			Raddr:  gopsnet.Addr{IP: "10.0.0.1", Port: 80},
		}
		if !isTrackedTCPConnection(c) {
			t.Errorf("status %q should be tracked", s)
		}
	}
}

func TestIsTrackedTCPConnection_UntrackedStatuses(t *testing.T) {
	for _, s := range []string{"TIME_WAIT", "CLOSE_WAIT", "FIN_WAIT1",
		"FIN_WAIT2", "CLOSING", "LAST_ACK", "LISTEN", ""} {
		c := gopsnet.ConnectionStat{
			Status: s,
			Raddr:  gopsnet.Addr{IP: "10.0.0.1", Port: 80},
		}
		if isTrackedTCPConnection(c) {
			t.Errorf("status %q should NOT be tracked", s)
		}
	}
}

func TestIsTrackedTCPConnection_EmptyRaddr(t *testing.T) {
	c := gopsnet.ConnectionStat{
		Status: "ESTABLISHED",
		Raddr:  gopsnet.Addr{IP: "", Port: 0},
	}
	if isTrackedTCPConnection(c) {
		t.Error("empty raddr should not be tracked")
	}
}

func TestIsTrackedTCPConnection_ZeroPort(t *testing.T) {
	c := gopsnet.ConnectionStat{
		Status: "ESTABLISHED",
		Raddr:  gopsnet.Addr{IP: "10.0.0.1", Port: 0},
	}
	if isTrackedTCPConnection(c) {
		t.Error("port=0 should not be tracked")
	}
}

// ─── endpoint 边界条件 ────────────────────────────────────────

func TestEndpoint_IPv4(t *testing.T) {
	if got := endpoint("192.168.1.1", 8080); got != "192.168.1.1:8080" {
		t.Errorf("got %q", got)
	}
}

func TestEndpoint_IPv6(t *testing.T) {
	got := endpoint("::1", 443)
	if got != "[::1]:443" {
		t.Errorf("got %q, want [::1]:443", got)
	}
}

func TestEndpoint_EmptyIP(t *testing.T) {
	if got := endpoint("", 80); got != "" {
		t.Errorf("empty IP should return empty, got %q", got)
	}
}

func TestEndpoint_HighPort(t *testing.T) {
	// port=65535 边界
	got := endpoint("10.0.0.1", 65535)
	if got != "10.0.0.1:65535" {
		t.Errorf("got %q", got)
	}
}

// ─── decompressEBPFProgram ────────────────────────────────────

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	return buf.Bytes()
}

func TestDecompressEBPFProgram_Valid(t *testing.T) {
	original := []byte("hello ebpf world")
	compressed := gzipBytes(original)

	out, err := decompressEBPFProgram(compressed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(out, original) {
		t.Errorf("got %q, want %q", out, original)
	}
}

func TestDecompressEBPFProgram_Empty(t *testing.T) {
	compressed := gzipBytes([]byte{})
	out, err := decompressEBPFProgram(compressed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty, got %d bytes", len(out))
	}
}

func TestDecompressEBPFProgram_NotGzip(t *testing.T) {
	_, err := decompressEBPFProgram([]byte("this is not gzip data at all"))
	if err == nil {
		t.Error("expected error for non-gzip input")
	}
}

func TestDecompressEBPFProgram_PlainText(t *testing.T) {
	// 合法字节但内容不是 gzip
	_, err := decompressEBPFProgram([]byte("raw data not gzip"))
	if err == nil {
		t.Error("expected error for non-gzip content")
	}
}

// ─── NewTracer 生命周期 ───────────────────────────────────────

func TestNewTracer_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr, err := NewTracer(ctx, -1, -1, true, 0)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}

	cancel()

	// closeChan 应在 ctx 取消后关闭（给后台 goroutine 一点时间）
	select {
	case <-tr.closeChan:
		// OK
	case <-time.After(200 * time.Millisecond):
		t.Error("closeChan not closed after context cancellation")
	}
}

func TestNewTracer_CloseIdempotent(t *testing.T) {
	tr := newTestTracer(t)
	// 多次 Close 不应 panic
	tr.Close()
	tr.Close()
}

// ─── 并发安全：handleListenEvent + GetListenPorts ─────────────

func TestHandleListenEvent_Concurrent(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		port := uint16(10000 + i)
		wg.Add(2)
		go func(p uint16) {
			defer wg.Done()
			tr.handleListenEvent(&Event{Type: EventTypeListenOpen, SrcPort: p})
		}(port)
		go func() {
			defer wg.Done()
			_ = tr.GetListenPorts()
		}()
	}
	wg.Wait()
}

// ─── 并发安全：gcConnections + ForEachActiveConnection ────────

func TestGcConnections_Concurrent(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 注入一些连接
	tr.activeConnMu.Lock()
	for i := uint64(0); i < 10; i++ {
		tr.activeConns[ConnectionID{FD: i}] = Connection{LastSeen: time.Now()}
	}
	tr.activeConnMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.gcConnections()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.ForEachActiveConnection(func(id ConnectionID, conn Connection) {})
		}()
	}
	wg.Wait()
}

// ─── pollConnections 关闭事件路径 ────────────────────────────

func TestPollConnections_EmitsCloseEvent(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 预置一个不存在于真实系统中的假连接到 lastSnapshot
	fakeID := ConnectionID{FD: 0xDEADBEEF, PID: 0xCAFEBABE}
	fakeEvent := Event{
		Type:    EventTypeConnectionOpen,
		Fd:      0xDEADBEEF,
		SrcAddr: "127.99.99.99:19999",
		DstAddr: "127.99.99.99:29999",
	}
	tr.activeConnMu.Lock()
	tr.lastSnapshot[fakeID] = fakeEvent
	tr.activeConns[fakeID] = Connection{LastSeen: time.Now()}
	tr.activeConnMu.Unlock()

	// poll — 假连接不在真实结果里 → 产生 ConnectionClose 事件
	tr.pollConnections()

	// 假连接应从 activeConns 中移除
	tr.activeConnMu.RLock()
	_, exists := tr.activeConns[fakeID]
	tr.activeConnMu.RUnlock()

	if exists {
		t.Error("fake connection should have been removed after poll (close event)")
	}
}

func TestPollConnections_NilActiveConns(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	// 将 activeConns 设为 nil 模拟 Close() 后的状态
	tr.activeConnMu.Lock()
	tr.activeConns = nil
	tr.activeConnMu.Unlock()

	// pollConnections 在 activeConns==nil 时应提前返回，不 panic
	tr.pollConnections()
}

// ─── decompressEBPFProgram — corrupt gzip body ────────────────

func TestDecompressEBPFProgram_CorruptGzipBody(t *testing.T) {
	// 构造一个有效 gzip header 但 body 被截断的流
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	_, _ = gz.Write([]byte("some valid payload data"))
	// 故意不调用 gz.Close()，body 不完整
	corrupt := raw.Bytes()
	if len(corrupt) > 10 {
		corrupt = corrupt[:10]
	}
	_, err := decompressEBPFProgram(corrupt)
	// 可能在 gzip.NewReader 或 io.ReadAll 阶段失败
	if err == nil {
		t.Error("expected error for truncated gzip body")
	}
}
