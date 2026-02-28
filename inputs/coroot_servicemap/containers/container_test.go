package containers

import (
	"testing"
	"time"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/l7"
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

func TestContainer_UpdateTrafficStats(t *testing.T) {
	container := NewContainer("test")
	openEvent := &tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      123,
		DstAddr: "10.0.0.1:80",
	}
	container.OnEvent(openEvent)

	container.UpdateTrafficStats(123, 100, 60)
	container.UpdateTrafficStats(123, 150, 90)

	stats := container.TCPStats["10.0.0.1:80"]
	if stats == nil {
		t.Fatal("tcp stats not found")
	}

	if stats.BytesSent != 150 {
		t.Fatalf("unexpected bytes sent: %d", stats.BytesSent)
	}
	if stats.BytesReceived != 90 {
		t.Fatalf("unexpected bytes received: %d", stats.BytesReceived)
	}
}

// ============================================================
// P2-9: L7 协议事件测试
// ============================================================

func TestContainer_OnL7Request_HTTP(t *testing.T) {
	container := NewContainer("test-l7")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     1234,
		Fd:      42,
		DstAddr: "10.0.0.1:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("GET /api/v1/health HTTP/1.1\r\nHost: 10.0.0.1\r\n\r\n"),
		},
	}

	container.OnEvent(event)

	// 应该有一条 HTTPStats 记录
	httpStats := container.GetHTTPStatsSnapshot()
	if len(httpStats) != 1 {
		t.Fatalf("expected 1 HTTP stats entry, got %d", len(httpStats))
	}

	// 验证统计数据
	for _, stats := range httpStats {
		if stats.DestinationAddr != "10.0.0.1:80" {
			t.Errorf("expected destination 10.0.0.1:80, got %s", stats.DestinationAddr)
		}
		if stats.Method != "GET" {
			t.Errorf("expected method GET, got %s", stats.Method)
		}
		if stats.StatusCode != 200 {
			t.Errorf("expected status code 200, got %d", stats.StatusCode)
		}
		if stats.RequestCount != 1 {
			t.Errorf("expected 1 request, got %d", stats.RequestCount)
		}
		if stats.ErrorCount != 0 {
			t.Errorf("expected 0 errors, got %d", stats.ErrorCount)
		}
		if stats.TotalLatency != 5 {
			t.Errorf("expected latency 5ms, got %d", stats.TotalLatency)
		}
	}
}

func TestContainer_OnL7Request_HTTPError(t *testing.T) {
	container := NewContainer("test-l7-error")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     1234,
		Fd:      42,
		DstAddr: "10.0.0.1:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   500,
			Duration: 100 * time.Millisecond,
			Payload:  []byte("POST /api/data HTTP/1.1\r\n\r\n"),
		},
	}

	container.OnEvent(event)

	httpStats := container.GetHTTPStatsSnapshot()
	if len(httpStats) != 1 {
		t.Fatalf("expected 1 HTTP stats entry, got %d", len(httpStats))
	}

	for _, stats := range httpStats {
		if stats.Method != "POST" {
			t.Errorf("expected method POST, got %s", stats.Method)
		}
		if stats.RequestCount != 1 {
			t.Errorf("expected 1 request, got %d", stats.RequestCount)
		}
		if stats.ErrorCount != 1 {
			t.Errorf("expected 1 error, got %d", stats.ErrorCount)
		}
		if stats.TotalLatency != 100 {
			t.Errorf("expected latency 100ms, got %d", stats.TotalLatency)
		}
	}
}

func TestContainer_OnL7Request_Multiple(t *testing.T) {
	container := NewContainer("test-l7-multi")

	// 发送多个不同状态的 HTTP 请求
	events := []*tracer.Event{
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 1, DstAddr: "10.0.0.1:80",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolHTTP, Status: 200, Duration: 5 * time.Millisecond,
				Payload: []byte("GET /health HTTP/1.1\r\n")},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 2, DstAddr: "10.0.0.1:80",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolHTTP, Status: 200, Duration: 10 * time.Millisecond,
				Payload: []byte("GET /health HTTP/1.1\r\n")},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 3, DstAddr: "10.0.0.1:80",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolHTTP, Status: 404, Duration: 3 * time.Millisecond,
				Payload: []byte("GET /notfound HTTP/1.1\r\n")},
		},
	}

	for _, e := range events {
		container.OnEvent(e)
	}

	httpStats := container.GetHTTPStatsSnapshot()

	// 应该有两个 key: GET+2xx 和 GET+4xx
	if len(httpStats) != 2 {
		t.Fatalf("expected 2 HTTP stats entries, got %d (keys: %v)", len(httpStats), httpStatsKeys(httpStats))
	}

	// 检查 2xx 条目
	var found2xx, found4xx bool
	for _, stats := range httpStats {
		if stats.StatusCode == 200 {
			found2xx = true
			if stats.RequestCount != 2 {
				t.Errorf("2xx: expected 2 requests, got %d", stats.RequestCount)
			}
			if stats.TotalLatency != 15 { // 5 + 10
				t.Errorf("2xx: expected latency 15ms, got %d", stats.TotalLatency)
			}
			if stats.MaxLatency != 10 {
				t.Errorf("2xx: expected max latency 10ms, got %d", stats.MaxLatency)
			}
			if stats.ErrorCount != 0 {
				t.Errorf("2xx: expected 0 errors, got %d", stats.ErrorCount)
			}
		}
		if stats.StatusCode == 404 {
			found4xx = true
			if stats.RequestCount != 1 {
				t.Errorf("4xx: expected 1 request, got %d", stats.RequestCount)
			}
			if stats.ErrorCount != 1 {
				t.Errorf("4xx: expected 1 error, got %d", stats.ErrorCount)
			}
		}
	}

	if !found2xx {
		t.Error("missing 2xx stats entry")
	}
	if !found4xx {
		t.Error("missing 4xx stats entry")
	}
}

func TestContainer_OnL7Request_NilL7Request(t *testing.T) {
	container := NewContainer("test-l7-nil")

	// L7Request 为 nil 时不应 panic
	event := &tracer.Event{
		Type:      tracer.EventTypeL7Request,
		L7Request: nil,
	}

	container.OnEvent(event)

	httpStats := container.GetHTTPStatsSnapshot()
	if len(httpStats) != 0 {
		t.Errorf("expected 0 HTTP stats entries, got %d", len(httpStats))
	}
}

func TestContainer_OnL7Request_UnknownProtocol(t *testing.T) {
	container := NewContainer("test-l7-unknown")

	// 未支持的协议应被忽略
	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.Protocol(99), // 未知协议编号
			Status:   200,
			Duration: 1 * time.Millisecond,
			Payload:  []byte{0x03, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'},
		},
	}

	container.OnEvent(event)

	httpStats := container.GetHTTPStatsSnapshot()
	if len(httpStats) != 0 {
		t.Errorf("expected 0 HTTP stats entries for unsupported protocol, got %d", len(httpStats))
	}

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 0 {
		t.Errorf("expected 0 L7 stats entries for unsupported protocol, got %d", len(l7Stats))
	}
}

// httpStatsKeys 辅助函数，用于调试输出
func httpStatsKeys(stats map[string]*HTTPStats) []string {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	return keys
}

// ============================================================
// L7 协议事件测试: MySQL / Postgres / Redis / Kafka
// ============================================================

func TestContainer_OnL7Request_MySQL(t *testing.T) {
	container := NewContainer("test-l7-mysql")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     2000,
		Fd:      10,
		DstAddr: "10.0.0.2:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 2 * time.Millisecond,
			Payload:  []byte("SELECT 1"),
		},
	}

	container.OnEvent(event)

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 1 {
		t.Fatalf("expected 1 L7 stats entry, got %d", len(l7Stats))
	}

	for _, stats := range l7Stats {
		if stats.Protocol != "MySQL" {
			t.Errorf("expected protocol MySQL, got %s", stats.Protocol)
		}
		if stats.DestinationAddr != "10.0.0.2:3306" {
			t.Errorf("expected destination 10.0.0.2:3306, got %s", stats.DestinationAddr)
		}
		if stats.RequestCount != 1 {
			t.Errorf("expected 1 request, got %d", stats.RequestCount)
		}
		if stats.ErrorCount != 0 {
			t.Errorf("expected 0 errors, got %d", stats.ErrorCount)
		}
		if stats.Status != "ok" {
			t.Errorf("expected status ok, got %s", stats.Status)
		}
	}
}

func TestContainer_OnL7Request_MySQLError(t *testing.T) {
	container := NewContainer("test-l7-mysql-err")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     2000,
		Fd:      10,
		DstAddr: "10.0.0.2:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusFailed,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("INSERT INTO t1"),
		},
	}

	container.OnEvent(event)

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 1 {
		t.Fatalf("expected 1 L7 stats entry, got %d", len(l7Stats))
	}

	for _, stats := range l7Stats {
		if stats.ErrorCount != 1 {
			t.Errorf("expected 1 error, got %d", stats.ErrorCount)
		}
		if stats.Status != "failed" {
			t.Errorf("expected status failed, got %s", stats.Status)
		}
		if stats.TotalLatency != 5 {
			t.Errorf("expected latency 5ms, got %d", stats.TotalLatency)
		}
	}
}

func TestContainer_OnL7Request_MySQL_StatementClose(t *testing.T) {
	container := NewContainer("test-l7-mysql-close")

	// MethodStatementClose should NOT produce stats
	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     2000,
		Fd:      10,
		DstAddr: "10.0.0.2:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 1 * time.Millisecond,
			Method:   l7.MethodStatementClose,
		},
	}

	container.OnEvent(event)

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 0 {
		t.Errorf("expected 0 L7 stats entries for MethodStatementClose, got %d", len(l7Stats))
	}
}

func TestContainer_OnL7Request_Postgres(t *testing.T) {
	container := NewContainer("test-l7-pg")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     3000,
		Fd:      20,
		DstAddr: "10.0.0.3:5432",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolPostgres,
			Status:   l7.StatusOK,
			Duration: 3 * time.Millisecond,
			Payload:  []byte("SELECT * FROM users"),
		},
	}

	container.OnEvent(event)

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 1 {
		t.Fatalf("expected 1 L7 stats entry, got %d", len(l7Stats))
	}

	for _, stats := range l7Stats {
		if stats.Protocol != "Postgres" {
			t.Errorf("expected protocol Postgres, got %s", stats.Protocol)
		}
		if stats.DestinationAddr != "10.0.0.3:5432" {
			t.Errorf("expected destination 10.0.0.3:5432, got %s", stats.DestinationAddr)
		}
		if stats.RequestCount != 1 {
			t.Errorf("expected 1 request, got %d", stats.RequestCount)
		}
	}
}

func TestContainer_OnL7Request_Redis(t *testing.T) {
	container := NewContainer("test-l7-redis")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     4000,
		Fd:      30,
		DstAddr: "10.0.0.4:6379",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolRedis,
			Status:   l7.StatusOK,
			Duration: 1 * time.Millisecond,
			Payload:  []byte("*2\r\n$3\r\nGET\r\n$5\r\nmykey\r\n"),
		},
	}

	container.OnEvent(event)

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 1 {
		t.Fatalf("expected 1 L7 stats entry, got %d", len(l7Stats))
	}

	for _, stats := range l7Stats {
		if stats.Protocol != "Redis" {
			t.Errorf("expected protocol Redis, got %s", stats.Protocol)
		}
		if stats.DestinationAddr != "10.0.0.4:6379" {
			t.Errorf("expected destination 10.0.0.4:6379, got %s", stats.DestinationAddr)
		}
		if stats.RequestCount != 1 {
			t.Errorf("expected 1 request, got %d", stats.RequestCount)
		}
	}
}

func TestContainer_OnL7Request_Kafka(t *testing.T) {
	container := NewContainer("test-l7-kafka")

	event := &tracer.Event{
		Type:    tracer.EventTypeL7Request,
		Pid:     5000,
		Fd:      40,
		DstAddr: "10.0.0.5:9092",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolKafka,
			Status:   l7.StatusOK,
			Duration: 10 * time.Millisecond,
		},
	}

	container.OnEvent(event)

	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 1 {
		t.Fatalf("expected 1 L7 stats entry, got %d", len(l7Stats))
	}

	for _, stats := range l7Stats {
		if stats.Protocol != "Kafka" {
			t.Errorf("expected protocol Kafka, got %s", stats.Protocol)
		}
		if stats.DestinationAddr != "10.0.0.5:9092" {
			t.Errorf("expected destination 10.0.0.5:9092, got %s", stats.DestinationAddr)
		}
		if stats.TotalLatency != 10 {
			t.Errorf("expected latency 10ms, got %d", stats.TotalLatency)
		}
	}
}

func TestContainer_OnL7Request_MultipleProtocols(t *testing.T) {
	container := NewContainer("test-l7-multi-proto")

	// Send events for different protocols
	events := []*tracer.Event{
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 1, DstAddr: "10.0.0.2:3306",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolMySQL, Status: l7.StatusOK, Duration: 2 * time.Millisecond},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 2, DstAddr: "10.0.0.3:5432",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolPostgres, Status: l7.StatusOK, Duration: 3 * time.Millisecond},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 3, DstAddr: "10.0.0.4:6379",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolRedis, Status: l7.StatusOK, Duration: 1 * time.Millisecond},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 4, DstAddr: "10.0.0.5:9092",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolKafka, Status: l7.StatusOK, Duration: 5 * time.Millisecond},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 5, DstAddr: "10.0.0.1:80",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolHTTP, Status: 200, Duration: 10 * time.Millisecond,
				Payload: []byte("GET /api HTTP/1.1\r\n")},
		},
	}

	for _, e := range events {
		container.OnEvent(e)
	}

	// HTTP goes to HTTPStats
	httpStats := container.GetHTTPStatsSnapshot()
	if len(httpStats) != 1 {
		t.Errorf("expected 1 HTTP stats entry, got %d", len(httpStats))
	}

	// MySQL/PG/Redis/Kafka go to L7Stats
	l7Stats := container.GetL7StatsSnapshot()
	if len(l7Stats) != 4 {
		t.Errorf("expected 4 L7 stats entries, got %d", len(l7Stats))
	}

	// Check each protocol exists
	protocols := map[string]bool{}
	for _, stats := range l7Stats {
		protocols[stats.Protocol] = true
	}
	for _, p := range []string{"MySQL", "Postgres", "Redis", "Kafka"} {
		if !protocols[p] {
			t.Errorf("missing protocol %s in L7 stats", p)
		}
	}
}

func TestContainer_OnL7Request_L7AggregatesByStatusAndDest(t *testing.T) {
	container := NewContainer("test-l7-aggregate")

	// Same protocol + dest, different status
	events := []*tracer.Event{
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 1, DstAddr: "10.0.0.2:3306",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolMySQL, Status: l7.StatusOK, Duration: 1 * time.Millisecond},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 2, DstAddr: "10.0.0.2:3306",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolMySQL, Status: l7.StatusOK, Duration: 2 * time.Millisecond},
		},
		{
			Type: tracer.EventTypeL7Request, Pid: 1, Fd: 3, DstAddr: "10.0.0.2:3306",
			L7Request: &l7.RequestData{Protocol: l7.ProtocolMySQL, Status: l7.StatusFailed, Duration: 5 * time.Millisecond},
		},
	}

	for _, e := range events {
		container.OnEvent(e)
	}

	l7Stats := container.GetL7StatsSnapshot()
	// Should have 2 entries: ok + failed
	if len(l7Stats) != 2 {
		t.Fatalf("expected 2 L7 stats entries (ok + failed), got %d", len(l7Stats))
	}

	for _, stats := range l7Stats {
		if stats.Status == "ok" {
			if stats.RequestCount != 2 {
				t.Errorf("ok: expected 2 requests, got %d", stats.RequestCount)
			}
			if stats.TotalLatency != 3 { // 1 + 2
				t.Errorf("ok: expected latency 3ms, got %d", stats.TotalLatency)
			}
			if stats.MaxLatency != 2 {
				t.Errorf("ok: expected max latency 2ms, got %d", stats.MaxLatency)
			}
			if stats.ErrorCount != 0 {
				t.Errorf("ok: expected 0 errors, got %d", stats.ErrorCount)
			}
		}
		if stats.Status == "failed" {
			if stats.RequestCount != 1 {
				t.Errorf("failed: expected 1 request, got %d", stats.RequestCount)
			}
			if stats.ErrorCount != 1 {
				t.Errorf("failed: expected 1 error, got %d", stats.ErrorCount)
			}
		}
	}
}
