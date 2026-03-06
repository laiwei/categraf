//go:build linux

package tracer

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
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

// ─── 真实流量集成测试（bytes/retrans）──────────────────────────────

func TestNetlinkConnections_Integration_ReportsBytes(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp4: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial tcp4: %v", err)
	}
	defer client.Close()

	var server net.Conn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("accept timeout")
	}
	defer server.Close()

	// 服务端持续读取，确保客户端发送字节被内核统计为已消费/已确认。
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = io.Copy(io.Discard, server)
	}()

	payload := bytes.Repeat([]byte("a"), 8*1024)
	const rounds = 16
	var written int
	for i := 0; i < rounds; i++ {
		n, werr := client.Write(payload)
		if werr != nil {
			t.Fatalf("client write failed at round %d: %v", i, werr)
		}
		written += n
	}

	clientPort := uint16(client.LocalAddr().(*net.TCPAddr).Port)
	serverPort := uint16(ln.Addr().(*net.TCPAddr).Port)

	var found bool
	var sent, recv uint64
	for i := 0; i < 20; i++ {
		conns, err := netlinkConnections()
		if err != nil {
			t.Fatalf("netlinkConnections: %v", err)
		}
		for j := range conns {
			dc := &conns[j]
			if dc.SrcPort == clientPort && dc.DstPort == serverPort && dc.IsTracked() {
				found = true
				sent = dc.BytesSent
				recv = dc.BytesReceived
				break
			}
		}
		if found && (sent > 0 || recv > 0) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !found {
		t.Skipf("did not observe loopback client socket in netlink dump (clientPort=%d serverPort=%d)", clientPort, serverPort)
	}
	if sent == 0 && recv == 0 {
		t.Skip("tcp_info byte counters unavailable on this kernel (likely < 4.2 or restricted)")
	}

	if sent == 0 {
		t.Fatalf("expected client socket bytes_sent > 0, got 0 (written=%d)", written)
	}
	t.Logf("observed loopback bytes: sent=%d recv=%d (written=%d)", sent, recv, written)
}

func TestCollectFromNetlink_Integration_PropagatesCounters(t *testing.T) {
	conns, err := netlinkConnections()
	if err != nil {
		t.Fatalf("netlinkConnections: %v", err)
	}

	// 选一条 tracked 连接做透传验证；若当前系统没有 tracked 连接则跳过。
	var sample *DiagConnection
	for i := range conns {
		if conns[i].IsTracked() {
			sample = &conns[i]
			break
		}
	}
	if sample == nil {
		t.Skip("no tracked TCP connection available for integration check")
	}

	tr := newTestTracer(t)
	defer tr.Close()

	current := make(map[ConnectionID]Event)
	listens := make(map[ListenKey]struct{})
	listenEvents := make(map[ListenKey]Event)
	tr.collectFromNetlink([]DiagConnection{*sample}, current, listens, listenEvents, 123)

	if len(current) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(current))
	}
	for _, e := range current {
		if e.BytesSent != sample.BytesSent {
			t.Fatalf("BytesSent mismatch: event=%d sample=%d", e.BytesSent, sample.BytesSent)
		}
		if e.BytesReceived != sample.BytesReceived {
			t.Fatalf("BytesReceived mismatch: event=%d sample=%d", e.BytesReceived, sample.BytesReceived)
		}
		if e.Retransmissions != uint64(sample.TotalRetrans) {
			t.Fatalf("Retransmissions mismatch: event=%d sample=%d", e.Retransmissions, sample.TotalRetrans)
		}
	}
}

// ─── native endian 解析兼容性测试 ────────────────────────────────

func TestParseTCPInfo_NativeEndian(t *testing.T) {
	b := make([]byte, tcpInfoMinSize5x)
	nativeEndian.PutUint32(b[tcpInfoOffTotalRetrans:], 9)
	nativeEndian.PutUint64(b[tcpInfoOffBytesReceived:], 12345)
	nativeEndian.PutUint64(b[tcpInfoOffBytesSent:], 67890)

	sent, recv, retx := parseTCPInfo(b)
	if sent != 67890 || recv != 12345 || retx != 9 {
		t.Fatalf("parseTCPInfo mismatch: sent=%d recv=%d retx=%d", sent, recv, retx)
	}
}

func TestParseNLAttrs_NativeEndian(t *testing.T) {
	// 构造一个 attr：type=INET_DIAG_INFO, len=12, data=8 bytes, 并对齐到 4 字节。
	buf := make([]byte, 12)
	nativeEndian.PutUint16(buf[0:2], 12)
	nativeEndian.PutUint16(buf[2:4], uint16(inetDiagInfo))
	copy(buf[4:12], []byte{1, 2, 3, 4, 5, 6, 7, 8})

	attrs := parseNLAttrs(buf)
	v, ok := attrs[uint16(inetDiagInfo)]
	if !ok {
		t.Fatalf("expected attr type %d", inetDiagInfo)
	}
	if len(v) != 8 {
		t.Fatalf("expected data len 8, got %d", len(v))
	}
}
