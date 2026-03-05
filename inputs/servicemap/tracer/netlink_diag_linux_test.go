//go:build linux

package tracer

import (
	"testing"
)

// ─── netlinkConnections ─────────────────────────────────────────

func TestNetlinkConnections_Linux_NoError(t *testing.T) {
	conns, err := netlinkConnections()
	if err != nil {
		t.Fatalf("netlinkConnections() error: %v", err)
	}
	// Linux 上至少有一些连接或监听端口（本测试进程自身也在用 socket）
	t.Logf("netlinkConnections returned %d connections", len(conns))
}

func TestNetlinkConnections_Linux_ContainsListen(t *testing.T) {
	conns, err := netlinkConnections()
	if err != nil {
		t.Fatalf("netlinkConnections() error: %v", err)
	}

	listenCount := 0
	for i := range conns {
		if conns[i].IsListen() {
			listenCount++
		}
	}

	// Linux 上 sshd/systemd 等至少有一个 LISTEN 端口
	t.Logf("netlinkConnections: %d LISTEN ports", listenCount)
	if listenCount == 0 {
		t.Log("WARN: no LISTEN ports found (might be normal in minimal containers)")
	}
}

func TestNetlinkConnections_Linux_EstablishedHasAddrs(t *testing.T) {
	conns, err := netlinkConnections()
	if err != nil {
		t.Fatalf("netlinkConnections() error: %v", err)
	}

	for i := range conns {
		dc := &conns[i]
		if !dc.IsTracked() {
			continue
		}
		// ESTABLISHED/SYN_SENT/SYN_RECV 必须有 DstIP + DstPort
		if dc.DstIP == "" {
			t.Errorf("tracked connection has empty DstIP: %+v", dc)
		}
		if dc.DstPort == 0 {
			t.Errorf("tracked connection has zero DstPort: %+v", dc)
		}
		if dc.SrcIP == "" {
			t.Errorf("tracked connection has empty SrcIP: %+v", dc)
		}
		// 只检查前 5 个避免日志洪泛
		if i > 5 {
			break
		}
	}
}

func TestDiagConnection_IsListen(t *testing.T) {
	dc := DiagConnection{State: tcpListen}
	if !dc.IsListen() {
		t.Error("state=LISTEN should return IsListen()=true")
	}

	dc.State = tcpEstablished
	if dc.IsListen() {
		t.Error("state=ESTABLISHED should return IsListen()=false")
	}
}

func TestDiagConnection_IsTracked(t *testing.T) {
	cases := []struct {
		state uint8
		want  bool
	}{
		{tcpEstablished, true},
		{tcpSynSent, true},
		{tcpSynRecv, true},
		{tcpListen, false},
		{0, false},
		{7, false}, // TCP_CLOSE_WAIT
	}
	for _, c := range cases {
		dc := DiagConnection{State: c.state}
		if got := dc.IsTracked(); got != c.want {
			t.Errorf("DiagConnection{State: %d}.IsTracked() = %v, want %v", c.state, got, c.want)
		}
	}
}

// ─── diagDump ───────────────────────────────────────────────────

func TestDiagDump_IPv4(t *testing.T) {
	conns, err := diagDump(2 /* AF_INET */, stateMask)
	if err != nil {
		t.Fatalf("diagDump(AF_INET): %v", err)
	}
	t.Logf("diagDump(AF_INET): %d results", len(conns))
}

func TestDiagDump_IPv6(t *testing.T) {
	conns, err := diagDump(10 /* AF_INET6 */, stateMask)
	if err != nil {
		t.Fatalf("diagDump(AF_INET6): %v", err)
	}
	t.Logf("diagDump(AF_INET6): %d results", len(conns))
}

// ─── collectFromNetlink 集成测试 ────────────────────────────────

func TestCollectFromNetlink_Integration(t *testing.T) {
	tr := newTestTracer(t)
	defer tr.Close()

	diagConns, err := netlinkConnections()
	if err != nil {
		t.Fatalf("netlinkConnections: %v", err)
	}

	current := make(map[ConnectionID]Event)
	listens := make(map[ListenKey]struct{})
	listenEvents := make(map[ListenKey]Event)

	tr.collectFromNetlink(diagConns, current, listens, listenEvents, uint64(0))

	t.Logf("collectFromNetlink: %d tracked, %d listen ports", len(current), len(listens))

	// 验证 listen 端口有 Addr 和 Port
	for key := range listens {
		if key.Port == 0 {
			t.Errorf("listen port should not be zero: %+v", key)
		}
		if key.Addr == "" {
			t.Errorf("listen addr should not be empty: %+v", key)
		}
	}

	// 验证 tracked 连接有完整地址
	for id, e := range current {
		if e.DstAddr == "" {
			t.Errorf("tracked event has empty DstAddr: id=%+v event=%+v", id, e)
		}
		if e.SrcAddr == "" {
			t.Errorf("tracked event has empty SrcAddr: id=%+v event=%+v", id, e)
		}
	}
}
