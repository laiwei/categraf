package servicemap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/containers"
	"flashcat.cloud/categraf/inputs/servicemap/l7"
	"flashcat.cloud/categraf/inputs/servicemap/tracer"
)

// ─── BuildGraph 结构测试 ───────────────────────────────────────

func TestBuildGraph_NilRegistry(t *testing.T) {
	ins := &Instance{}
	resp := ins.BuildGraph()

	if resp.Summary.Nodes != 0 {
		t.Errorf("nodes: want 0, got %d", resp.Summary.Nodes)
	}
	if resp.Summary.Edges != 0 {
		t.Errorf("edges: want 0, got %d", resp.Summary.Edges)
	}
	if resp.Nodes == nil {
		t.Error("Nodes should be non-nil empty slice")
	}
}

func TestBuildGraph_EmptyContainers(t *testing.T) {
	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{})

	if resp.Summary.Nodes != 0 {
		t.Errorf("nodes: want 0, got %d", resp.Summary.Nodes)
	}
	if len(resp.Edges) != 0 {
		t.Errorf("edges: want 0, got %d", len(resp.Edges))
	}
}

func TestBuildGraph_SingleContainerTCPOnly(t *testing.T) {
	c := containers.NewContainer("app-001")
	c.Name = "webapp"
	c.Namespace = "prod"
	c.PodName = "webapp-pod-abc"
	c.Image = "nginx:1.25"
	c.TCPStats["10.0.0.1:5432"] = &containers.TCPStats{
		SuccessfulConnects: 100,
		FailedConnects:     5,
		ActiveConnections:  8,
		Retransmissions:    2,
		BytesSent:          65536,
		BytesReceived:      131072,
		TotalLifetimeMs:    300, // 300ms total → avg 3ms
	}

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	// 摘要
	if resp.Summary.Nodes != 1 {
		t.Errorf("summary.nodes: want 1, got %d", resp.Summary.Nodes)
	}
	if resp.Summary.Edges != 1 {
		t.Errorf("summary.edges: want 1, got %d", resp.Summary.Edges)
	}

	// 节点
	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes len: want 1, got %d", len(resp.Nodes))
	}
	n := resp.Nodes[0]
	if n.ID != "app-001" {
		t.Errorf("node.id: want app-001, got %s", n.ID)
	}
	if n.Name != "webapp" {
		t.Errorf("node.name: want webapp, got %s", n.Name)
	}
	if n.Namespace != "prod" {
		t.Errorf("node.namespace: want prod, got %s", n.Namespace)
	}
	if n.PodName != "webapp-pod-abc" {
		t.Errorf("node.pod_name: want webapp-pod-abc, got %s", n.PodName)
	}
	if n.Image != "nginx:1.25" {
		t.Errorf("node.image: want nginx:1.25, got %s", n.Image)
	}

	// 边
	if len(resp.Edges) != 1 {
		t.Fatalf("edges len: want 1, got %d", len(resp.Edges))
	}
	e := resp.Edges[0]
	if e.Source != "app-001" {
		t.Errorf("edge.source: want app-001, got %s", e.Source)
	}
	if e.Target != "10.0.0.1:5432" {
		t.Errorf("edge.target: want 10.0.0.1:5432, got %s", e.Target)
	}
	if e.TargetHost != "10.0.0.1" {
		t.Errorf("edge.target_host: want 10.0.0.1, got %s", e.TargetHost)
	}
	if e.TargetPort != "5432" {
		t.Errorf("edge.target_port: want 5432, got %s", e.TargetPort)
	}

	// TCP 统计
	if e.TCP == nil {
		t.Fatal("edge.tcp should not be nil")
	}
	if e.TCP.ConnectsTotal != 100 {
		t.Errorf("tcp.connects_total: want 100, got %d", e.TCP.ConnectsTotal)
	}
	if e.TCP.ConnectFailedTotal != 5 {
		t.Errorf("tcp.connect_failed_total: want 5, got %d", e.TCP.ConnectFailedTotal)
	}
	if e.TCP.ActiveConnections != 8 {
		t.Errorf("tcp.active_connections: want 8, got %d", e.TCP.ActiveConnections)
	}
	if e.TCP.BytesSentTotal != 65536 {
		t.Errorf("tcp.bytes_sent_total: want 65536, got %d", e.TCP.BytesSentTotal)
	}
	// avg_lifetime = 300ms / 100 = 3ms
	if e.TCP.AvgSessionLifetimeMs != 3.0 {
		t.Errorf("tcp.avg_session_lifetime_ms: want 3.0, got %.2f", e.TCP.AvgSessionLifetimeMs)
	}

	// protocols 只有 TCP
	if len(e.Protocols) != 1 || e.Protocols[0] != "TCP" {
		t.Errorf("protocols: want [TCP], got %v", e.Protocols)
	}
}

func TestBuildGraph_ActiveConnectionLifetime(t *testing.T) {
	// 模拟一个只有活跃连接（未关闭）的场景：
	// connects=1, active=1, TotalLifetimeMs=0
	// 预期 avg_session_lifetime_ms > 0（包含活跃连接的运行时长）
	c := containers.NewContainer("active-001")
	c.Name = "long-lived-svc"

	// 通过 OnEvent 打开连接（这会设置 activeConnections + TCPStats）
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		DstAddr: "10.0.0.1:443",
		Fd:      99,
	})

	// 等待一小段时间，确保 duration > 0
	time.Sleep(10 * time.Millisecond)

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	if len(resp.Edges) != 1 {
		t.Fatalf("edges: want 1, got %d", len(resp.Edges))
	}
	e := resp.Edges[0]
	if e.TCP == nil {
		t.Fatal("edge.tcp should not be nil")
	}
	if e.TCP.ActiveConnections != 1 {
		t.Errorf("active: want 1, got %d", e.TCP.ActiveConnections)
	}
	// 关键断言：即使连接未关闭，avg_session_lifetime_ms 也应 > 0
	if e.TCP.AvgSessionLifetimeMs <= 0 {
		t.Errorf("avg_session_lifetime_ms should be > 0 for active connection, got %.2f", e.TCP.AvgSessionLifetimeMs)
	}
}

func TestBuildGraph_MixedActiveAndClosedLifetime(t *testing.T) {
	// 模拟混合场景：1 个已关闭连接 (lifetime=100ms) + 1 个活跃连接
	// 预期 avg = (100 + active_duration) / 2
	c := containers.NewContainer("mixed-001")
	c.Name = "mixed-svc"

	// 打开连接 1 并关闭（手动设置 TCPStats 模拟）
	c.TCPStats["10.0.0.1:80"] = &containers.TCPStats{
		DestinationAddr:    "10.0.0.1:80",
		SuccessfulConnects: 2,
		ActiveConnections:  1,
		TotalLifetimeMs:    100, // 第一个连接关闭时记录 100ms
	}

	// 手动注入一个活跃连接
	past := time.Now().Add(-50 * time.Millisecond)
	c.InjectActiveConnection(1, &containers.ConnectionTracker{
		Destination: "10.0.0.1:80",
		OpenTime:    past,
	})

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	if len(resp.Edges) != 1 {
		t.Fatalf("edges: want 1, got %d", len(resp.Edges))
	}
	e := resp.Edges[0]

	// avg = (100 + >=50) / 2 >= 75
	if e.TCP.AvgSessionLifetimeMs < 75 {
		t.Errorf("avg_session_lifetime_ms should be >= 75, got %.2f", e.TCP.AvgSessionLifetimeMs)
	}
}

func TestBuildGraph_HTTPEnrichment(t *testing.T) {
	c := containers.NewContainer("frontend-001")
	c.TCPStats["10.0.0.2:80"] = &containers.TCPStats{SuccessfulConnects: 50}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.2:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 20 * time.Millisecond,
			Payload:  []byte("GET /api HTTP/1.1\r\n"),
		},
	})
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.2:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   500,
			Duration: 100 * time.Millisecond,
			Payload:  []byte("POST /submit HTTP/1.1\r\n"),
		},
	})

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	if len(resp.Edges) != 1 {
		t.Fatalf("edges: want 1, got %d", len(resp.Edges))
	}
	e := resp.Edges[0]

	// HTTP entries: GET 200 + POST 500
	if len(e.HTTP) != 2 {
		t.Fatalf("http entries: want 2, got %d", len(e.HTTP))
	}
	// 按 method 排序：GET < POST
	if e.HTTP[0].Method != "GET" || e.HTTP[0].StatusCode != 200 {
		t.Errorf("http[0]: want GET/200, got %s/%d", e.HTTP[0].Method, e.HTTP[0].StatusCode)
	}
	if e.HTTP[1].Method != "POST" || e.HTTP[1].StatusCode != 500 {
		t.Errorf("http[1]: want POST/500, got %s/%d", e.HTTP[1].Method, e.HTTP[1].StatusCode)
	}
	if e.HTTP[0].StatusClass != "2xx" {
		t.Errorf("GET status_class: want 2xx, got %s", e.HTTP[0].StatusClass)
	}
	if e.HTTP[1].ErrorsTotal != 1 {
		t.Errorf("POST errors_total: want 1, got %d", e.HTTP[1].ErrorsTotal)
	}
	// avg duration: GET 1次 20ms → avg=20ms
	if e.HTTP[0].AvgDurationMs != 20.0 {
		t.Errorf("GET avg_duration_ms: want 20.0, got %.2f", e.HTTP[0].AvgDurationMs)
	}

	// protocols 应包含 HTTP
	hasHTTP := false
	for _, p := range e.Protocols {
		if p == "HTTP" {
			hasHTTP = true
		}
	}
	if !hasHTTP {
		t.Errorf("protocols should contain HTTP, got %v", e.Protocols)
	}
}

func TestBuildGraph_L7Enrichment(t *testing.T) {
	c := containers.NewContainer("backend-001")
	c.TCPStats["10.0.0.5:3306"] = &containers.TCPStats{SuccessfulConnects: 200}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.5:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 4 * time.Millisecond,
		},
	})
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.5:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusFailed,
			Duration: 10 * time.Millisecond,
		},
	})

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	if len(resp.Edges) != 1 {
		t.Fatalf("edges: want 1, got %d", len(resp.Edges))
	}
	e := resp.Edges[0]

	// L7: failed < ok（字母序）
	if len(e.L7) != 2 {
		t.Fatalf("l7 entries: want 2, got %d", len(e.L7))
	}
	if e.L7[0].Protocol != "MySQL" || e.L7[0].Status != "failed" {
		t.Errorf("l7[0]: want MySQL/failed, got %s/%s", e.L7[0].Protocol, e.L7[0].Status)
	}
	if e.L7[1].Protocol != "MySQL" || e.L7[1].Status != "ok" {
		t.Errorf("l7[1]: want MySQL/ok, got %s/%s", e.L7[1].Protocol, e.L7[1].Status)
	}
	if e.L7[1].AvgDurationMs != 4.0 {
		t.Errorf("ok avg_duration_ms: want 4.0, got %.2f", e.L7[1].AvgDurationMs)
	}

	// protocols 应包含 MySQL
	hasMySQL := false
	for _, p := range e.Protocols {
		if p == "MySQL" {
			hasMySQL = true
		}
	}
	if !hasMySQL {
		t.Errorf("protocols should contain MySQL, got %v", e.Protocols)
	}
}

func TestBuildGraph_MultiProtocolEdge(t *testing.T) {
	// 同一条边上有 TCP + HTTP + MySQL
	c := containers.NewContainer("svc-001")
	c.TCPStats["10.0.0.9:3306"] = &containers.TCPStats{SuccessfulConnects: 10}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.9:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("GET / HTTP/1.1\r\n"),
		},
	})
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.9:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 3 * time.Millisecond,
		},
	})

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})
	e := resp.Edges[0]

	protocolSet := map[string]bool{}
	for _, p := range e.Protocols {
		protocolSet[p] = true
	}
	if !protocolSet["TCP"] || !protocolSet["HTTP"] || !protocolSet["MySQL"] {
		t.Errorf("protocols: want TCP+HTTP+MySQL, got %v", e.Protocols)
	}
	if len(e.HTTP) == 0 {
		t.Error("HTTP entries should be non-empty")
	}
	if len(e.L7) == 0 {
		t.Error("L7 entries should be non-empty")
	}
}

func TestBuildGraph_MultiContainers(t *testing.T) {
	c1 := containers.NewContainer("svc-a")
	c1.Name = "service-a"
	c1.Namespace = "prod"
	c1.TCPStats["10.0.0.1:5432"] = &containers.TCPStats{SuccessfulConnects: 10}

	c2 := containers.NewContainer("svc-b")
	c2.Name = "service-b"
	c2.Namespace = "prod"
	c2.TCPStats["10.0.0.2:6379"] = &containers.TCPStats{SuccessfulConnects: 20}

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c1, c2})

	if resp.Summary.Nodes != 2 {
		t.Errorf("nodes: want 2, got %d", resp.Summary.Nodes)
	}
	if resp.Summary.Edges != 2 {
		t.Errorf("edges: want 2, got %d", resp.Summary.Edges)
	}
	// 节点按 ID 排序：svc-a < svc-b
	if resp.Nodes[0].ID != "svc-a" || resp.Nodes[1].ID != "svc-b" {
		t.Errorf("nodes order: got %s, %s", resp.Nodes[0].ID, resp.Nodes[1].ID)
	}
}

func TestBuildGraph_NodeLabels(t *testing.T) {
	c := containers.NewContainer("cid-labels")
	c.Name = "my-app"
	c.Labels = map[string]string{"team": "backend", "env": "prod"}
	c.TCPStats["10.0.0.1:80"] = &containers.TCPStats{SuccessfulConnects: 1}

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	n := resp.Nodes[0]
	if n.Labels["team"] != "backend" {
		t.Errorf("label team: want backend, got %s", n.Labels["team"])
	}
	if n.Labels["env"] != "prod" {
		t.Errorf("label env: want prod, got %s", n.Labels["env"])
	}
}

func TestBuildGraph_DeterministicOrder(t *testing.T) {
	// 多次调用结果顺序一致
	c1 := containers.NewContainer("zzz-last")
	c1.TCPStats["10.0.0.3:80"] = &containers.TCPStats{SuccessfulConnects: 1}
	c2 := containers.NewContainer("aaa-first")
	c2.TCPStats["10.0.0.1:80"] = &containers.TCPStats{SuccessfulConnects: 1}
	c3 := containers.NewContainer("mmm-mid")
	c3.TCPStats["10.0.0.2:80"] = &containers.TCPStats{SuccessfulConnects: 1}

	ins := &Instance{}
	for i := 0; i < 5; i++ {
		resp := ins.buildGraphWithContainers([]*containers.Container{c1, c2, c3})
		if resp.Nodes[0].ID != "aaa-first" {
			t.Errorf("run %d: first node should be aaa-first, got %s", i, resp.Nodes[0].ID)
		}
		if resp.Nodes[2].ID != "zzz-last" {
			t.Errorf("run %d: last node should be zzz-last, got %s", i, resp.Nodes[2].ID)
		}
	}
}

// ─── JSON 序列化测试 ───────────────────────────────────────────

func TestBuildGraph_JSONRoundtrip(t *testing.T) {
	c := containers.NewContainer("json-test")
	c.Name = "myservice"
	c.Namespace = "default"
	c.TCPStats["192.168.1.1:3306"] = &containers.TCPStats{
		SuccessfulConnects: 50,
		ActiveConnections:  3,
	}

	ins := &Instance{}
	resp := ins.buildGraphWithContainers([]*containers.Container{c})

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded GraphResponse
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if decoded.Summary.Nodes != 1 {
		t.Errorf("decoded nodes: want 1, got %d", decoded.Summary.Nodes)
	}
	if decoded.Nodes[0].Name != "myservice" {
		t.Errorf("decoded node name: want myservice, got %s", decoded.Nodes[0].Name)
	}
	if decoded.Edges[0].TCP.ConnectsTotal != 50 {
		t.Errorf("decoded tcp.connects_total: want 50, got %d", decoded.Edges[0].TCP.ConnectsTotal)
	}
}

// ─── HTTP Handler 测试 ────────────────────────────────────────

func TestHandleGraph_JSON(t *testing.T) {
	c := containers.NewContainer("http-test")
	c.Name = "api-server"
	c.TCPStats["10.0.1.1:5432"] = &containers.TCPStats{SuccessfulConnects: 99}

	ins := &Instance{}
	// 临时 override: 使用 httptest + buildGraphWithContainers
	// 通过 handleGraph 的 handler 直接测试（handler 调用 BuildGraph，需要 registry）
	// 这里改用 ResponseRecorder 直接测试 JSON 输出
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ins.buildGraphWithContainers([]*containers.Container{c})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	})

	req := httptest.NewRequest("GET", "/graph", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: want 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("content-type: want json, got %s", ct)
	}

	var gr GraphResponse
	if err := json.NewDecoder(rec.Body).Decode(&gr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if gr.Summary.Nodes != 1 {
		t.Errorf("nodes: want 1, got %d", gr.Summary.Nodes)
	}
	if gr.Edges[0].TCP.ConnectsTotal != 99 {
		t.Errorf("connects_total: want 99, got %d", gr.Edges[0].TCP.ConnectsTotal)
	}
}

func TestHandleGraphText_Format(t *testing.T) {
	c := containers.NewContainer("text-test")
	c.Name = "text-service"
	c.Namespace = "staging"
	c.TCPStats["10.0.2.2:6379"] = &containers.TCPStats{
		SuccessfulConnects: 42,
		ActiveConnections:  7,
	}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.2.2:6379",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolRedis,
			Status:   l7.StatusOK,
			Duration: 1 * time.Millisecond,
		},
	})

	ins := &Instance{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ins.buildGraphWithContainers([]*containers.Container{c})
		ins.writeGraphText(w, resp)
	})

	req := httptest.NewRequest("GET", "/graph/text", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()

	wantStrings := []string{
		"Service Map",
		"nodes", "edges",
		"text-test",
		"text-service",
		"10.0.2.2:6379",
		"TCP:",
		"L7 Redis[ok]",
	}
	for _, want := range wantStrings {
		if !strings.Contains(body, want) {
			t.Errorf("text response missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestHandleGraph_ServerStartStop(t *testing.T) {
	ins := newTestInstance()
	ins.APIAddr = "127.0.0.1:0" // 使用随机端口

	// 直接测试 startAPIServer 不 panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("startAPIServer panicked: %v", r)
		}
	}()

	ins.startAPIServer()
	if ins.apiServer == nil {
		t.Error("apiServer should be non-nil after startAPIServer")
	}
	ins.stopAPIServer()
}

func TestHandleGraph_EmptyAddr_NoServer(t *testing.T) {
	ins := newTestInstance()
	ins.APIAddr = "" // 不启动

	ins.startAPIServer()
	if ins.apiServer != nil {
		t.Error("apiServer should be nil when APIAddr is empty")
	}
}

func TestAddProtocol_Dedup(t *testing.T) {
	protocols := []string{"TCP"}
	addProtocol(&protocols, "TCP")
	addProtocol(&protocols, "HTTP")
	addProtocol(&protocols, "HTTP")
	addProtocol(&protocols, "MySQL")

	if len(protocols) != 3 {
		t.Errorf("want 3 protocols, got %d: %v", len(protocols), protocols)
	}
}
