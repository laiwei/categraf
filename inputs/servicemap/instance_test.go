package servicemap

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/containers"
	"flashcat.cloud/categraf/inputs/servicemap/l7"
	"flashcat.cloud/categraf/inputs/servicemap/tracer"
	"flashcat.cloud/categraf/types"
)

// ─── helpers ──────────────────────────────────────────────────

func sampleMap(slist *types.SampleList) map[string][]*types.Sample {
	all := slist.PopBackAll()
	m := make(map[string][]*types.Sample, len(all))
	for _, s := range all {
		m[s.Metric] = append(m[s.Metric], s)
	}
	return m
}

func findByLabel(samples []*types.Sample, key, val string) *types.Sample {
	for _, s := range samples {
		if s.Labels[key] == val {
			return s
		}
	}
	return nil
}

func assertMetric(t *testing.T, m map[string][]*types.Sample, metric string, wantValue float64) {
	t.Helper()
	samples, ok := m[metric]
	if !ok {
		var names []string
		for k := range m {
			names = append(names, k)
		}
		t.Errorf("metric %q not found (got: %v)", metric, names)
		return
	}
	val := toFloat(samples[0].Value)
	if val != wantValue {
		t.Errorf("metric %q: value=%.4f, want %.4f", metric, val, wantValue)
	}
}

// assertMetricByTag 按指定标签键值过滤，断言匹配的第一条 sample 的値。
func assertMetricByTag(t *testing.T, m map[string][]*types.Sample, metric, tagKey, tagValue string, wantValue float64) {
	t.Helper()
	samples, ok := m[metric]
	if !ok {
		t.Errorf("metric %q not found", metric)
		return
	}
	for _, s := range samples {
		if s.Labels[tagKey] == tagValue {
			val := toFloat(s.Value)
			if val != wantValue {
				t.Errorf("metric %q{%s=%q}: value=%.4f, want %.4f", metric, tagKey, tagValue, val, wantValue)
			}
			return
		}
	}
	t.Errorf("metric %q with %s=%q not found", metric, tagKey, tagValue)
}

func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func newTestInstance() *Instance {
	return &Instance{
		EnableTCP:        true,
		EnableHTTP:       true,
		DisableL7Tracing: false,
		MaxContainers:    100,
	}
}

// ─── TCP 指标 ──────────────────────────────────────────────────

func TestCollectTCPStats_MetricNames(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-001")
	c.Name = "nginx"
	c.Namespace = "prod"
	c.PodName = "nginx-abc"
	c.TCPStats["10.0.0.5:5432"] = &containers.TCPStats{
		SuccessfulConnects: 42,
		FailedConnects:     3,
		ActiveConnections:  5,
		Retransmissions:    2,
		BytesSent:          1024,
		BytesReceived:      2048,
		TotalLifetimeMs:    5000,
	}

	baseTags := map[string]string{
		"container_id":   c.ID,
		"container_name": c.Name,
		"namespace":      c.Namespace,
		"pod_name":       c.PodName,
	}
	ins.collectTCPStats(c, baseTags, slist)
	m := sampleMap(slist)

	wantMetrics := map[string]float64{
		inputName + "_tcp_connects_total":                 42,
		inputName + "_tcp_connect_failed_total":           3,
		inputName + "_tcp_retransmits_total":              2,
		inputName + "_tcp_bytes_sent_total":               1024,
		inputName + "_tcp_bytes_received_total":           2048,
		inputName + "_tcp_active_connections":             5,
		inputName + "_tcp_session_lifetime_seconds_count": 42,
		inputName + "_tcp_session_lifetime_seconds_sum":   5.0, // 5000ms → 5s
	}
	for metric, wantVal := range wantMetrics {
		assertMetric(t, m, metric, wantVal)
	}
}

func TestCollectTCPStats_Labels(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-002")
	c.Name = "backend"
	c.Namespace = "staging"
	c.PodName = "backend-xyz"
	c.TCPStats["192.168.1.100:3306"] = &containers.TCPStats{SuccessfulConnects: 10}

	baseTags := map[string]string{
		"container_id":   c.ID,
		"container_name": c.Name,
		"namespace":      c.Namespace,
		"pod_name":       c.PodName,
	}
	ins.collectTCPStats(c, baseTags, slist)
	m := sampleMap(slist)

	metric := inputName + "_tcp_connects_total"
	samples, ok := m[metric]
	if !ok {
		t.Fatalf("metric %q not found", metric)
	}
	s := samples[0]

	wantLabels := map[string]string{
		"container_id":   "cid-002",
		"container_name": "backend",
		"namespace":      "staging",
		"pod_name":       "backend-xyz",
		"destination":    "192.168.1.100:3306",
	}
	for k, want := range wantLabels {
		if got := s.Labels[k]; got != want {
			t.Errorf("label %q: got %q, want %q", k, got, want)
		}
	}
}

func TestCollectTCPStats_MultiDestination(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-003")
	c.TCPStats["10.0.0.1:80"] = &containers.TCPStats{SuccessfulConnects: 5}
	c.TCPStats["10.0.0.2:443"] = &containers.TCPStats{SuccessfulConnects: 3}
	c.TCPStats["10.0.0.3:5432"] = &containers.TCPStats{SuccessfulConnects: 8}

	ins.collectTCPStats(c, map[string]string{}, slist)
	m := sampleMap(slist)

	if total := len(m[inputName+"_tcp_connects_total"]); total != 3 {
		t.Errorf("expected 3 tcp_connects_total samples, got %d", total)
	}

	s80 := findByLabel(m[inputName+"_tcp_connects_total"], "destination", "10.0.0.1:80")
	if s80 == nil {
		t.Fatal("missing sample for 10.0.0.1:80")
	}
	if toFloat(s80.Value) != 5 {
		t.Errorf("10.0.0.1:80 connects: got %.0f, want 5", toFloat(s80.Value))
	}

	s5432 := findByLabel(m[inputName+"_tcp_connects_total"], "destination", "10.0.0.3:5432")
	if s5432 == nil {
		t.Fatal("missing sample for 10.0.0.3:5432")
	}
	if toFloat(s5432.Value) != 8 {
		t.Errorf("10.0.0.3:5432 connects: got %.0f, want 8", toFloat(s5432.Value))
	}
}

// ─── HTTP 指标 ─────────────────────────────────────────────────

func TestCollectHTTPStats_MetricNames(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-http")
	c.Name = "frontend"
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:8080",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 15 * time.Millisecond,
			Payload:  []byte("GET /api/v1/users HTTP/1.1\r\n"),
		},
	})
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:8080",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   500,
			Duration: 200 * time.Millisecond,
			Payload:  []byte("POST /api/v1/orders HTTP/1.1\r\n"),
		},
	})

	baseTags := map[string]string{"container_id": c.ID, "container_name": c.Name}
	ins.collectHTTPStats(c, baseTags, slist)
	m := sampleMap(slist)

	// GET(2xx) + POST(5xx) = 2 个 key
	if cnt := len(m[inputName+"_http_requests_total"]); cnt != 2 {
		t.Errorf("expected 2 http_requests_total samples, got %d", cnt)
	}

	sGET := findByLabel(m[inputName+"_http_requests_total"], "method", "GET")
	if sGET == nil {
		t.Fatal("missing GET sample")
	}
	if toFloat(sGET.Value) != 1 {
		t.Errorf("GET request count: want 1, got %.0f", toFloat(sGET.Value))
	}

	errSamples := m[inputName+"_http_request_errors_total"]
	sPOSTerr := findByLabel(errSamples, "method", "POST")
	if sPOSTerr == nil {
		t.Fatal("missing POST error sample")
	}
	if toFloat(sPOSTerr.Value) != 1 {
		t.Errorf("POST error count: want 1, got %.0f", toFloat(sPOSTerr.Value))
	}
}

func TestCollectHTTPStats_StatusClassLabel(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-sc")
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.2:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   404,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("GET /missing HTTP/1.1\r\n"),
		},
	})

	ins.collectHTTPStats(c, map[string]string{}, slist)
	m := sampleMap(slist)

	s := m[inputName+"_http_requests_total"]
	if len(s) == 0 {
		t.Fatal("no http_requests_total")
	}
	if got := s[0].Labels["status_class"]; got != "4xx" {
		t.Errorf("status_class: want 4xx, got %q", got)
	}
	if got := s[0].Labels["status_code"]; got != "404" {
		t.Errorf("status_code: want 404, got %q", got)
	}
}

func TestCollectHTTPStats_DurationSeconds(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-dur")
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.3:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 250 * time.Millisecond,
			Payload:  []byte("GET / HTTP/1.1\r\n"),
		},
	})

	ins.collectHTTPStats(c, map[string]string{}, slist)
	m := sampleMap(slist)

	// 250ms → 0.25s
	s := m[inputName+"_http_request_duration_seconds_sum"]
	if len(s) == 0 {
		t.Fatal("no duration_seconds_sum")
	}
	if got := toFloat(s[0].Value); got != 0.25 {
		t.Errorf("duration_sum: want 0.25, got %.4f", got)
	}
}

// ─── L7 协议指标 ───────────────────────────────────────────────

func TestCollectL7ProtoStats_MySQL(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-mysql")
	c.Name = "app"
	c.Namespace = "prod"

	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.10:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 3 * time.Millisecond,
		},
	})
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.10:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusFailed,
			Duration: 50 * time.Millisecond,
		},
	})

	baseTags := map[string]string{"container_id": c.ID, "namespace": c.Namespace}
	ins.collectL7ProtoStats(c, baseTags, slist)
	m := sampleMap(slist)

	// ok + failed = 2 个 key
	if cnt := len(m[inputName+"_mysql_requests_total"]); cnt != 2 {
		t.Errorf("expected 2 mysql_requests_total samples, got %d", cnt)
	}

	sOK := findByLabel(m[inputName+"_mysql_requests_total"], "status", "ok")
	if sOK == nil {
		t.Fatal("missing ok sample")
	}
	if sOK.Labels["protocol"] != "MySQL" {
		t.Errorf("protocol: want MySQL, got %q", sOK.Labels["protocol"])
	}
	if sOK.Labels["destination"] != "10.0.0.10:3306" {
		t.Errorf("destination: want 10.0.0.10:3306, got %q", sOK.Labels["destination"])
	}
	if sOK.Labels["namespace"] != "prod" {
		t.Errorf("namespace: want prod, got %q", sOK.Labels["namespace"])
	}

	sFailed := findByLabel(m[inputName+"_mysql_request_errors_total"], "status", "failed")
	if sFailed == nil {
		t.Fatal("missing mysql_request_errors_total[failed]")
	}
	if toFloat(sFailed.Value) != 1 {
		t.Errorf("mysql error count: want 1, got %.0f", toFloat(sFailed.Value))
	}
}

func TestCollectL7ProtoStats_AllProtocols(t *testing.T) {
	protocols := []struct {
		proto   l7.Protocol
		dstAddr string
		prefix  string
	}{
		{l7.ProtocolPostgres, "10.0.0.1:5432", "postgres"},
		{l7.ProtocolRedis, "10.0.0.2:6379", "redis"},
		{l7.ProtocolKafka, "10.0.0.3:9092", "kafka"},
	}

	for _, tt := range protocols {
		t.Run(tt.prefix, func(t *testing.T) {
			ins := newTestInstance()
			slist := types.NewSampleList()

			c := containers.NewContainer("cid-" + tt.prefix)
			c.OnEvent(&tracer.Event{
				Type:    tracer.EventTypeL7Request,
				DstAddr: tt.dstAddr,
				L7Request: &l7.RequestData{
					Protocol: tt.proto,
					Status:   l7.StatusOK,
					Duration: 5 * time.Millisecond,
				},
			})

			ins.collectL7ProtoStats(c, map[string]string{}, slist)
			m := sampleMap(slist)

			reqMetric := inputName + "_" + tt.prefix + "_requests_total"
			if _, ok := m[reqMetric]; !ok {
				t.Errorf("metric %q not found", reqMetric)
			}

			durMetric := inputName + "_" + tt.prefix + "_request_duration_seconds_sum"
			ds := m[durMetric]
			if len(ds) == 0 {
				t.Fatalf("metric %q not found", durMetric)
			}
			if got := toFloat(ds[0].Value); got != 0.005 {
				t.Errorf("%s duration_sum: want 0.005, got %.6f", tt.prefix, got)
			}
		})
	}
}

func TestCollectL7ProtoStats_StatementCloseSkipped(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-close")
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.5:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Method:   l7.MethodStatementClose,
		},
	})

	ins.collectL7ProtoStats(c, map[string]string{}, slist)
	m := sampleMap(slist)

	if _, ok := m[inputName+"_mysql_requests_total"]; ok {
		t.Error("MethodStatementClose should NOT produce mysql_requests_total")
	}
}

// ─── Service Map 指标 ──────────────────────────────────────────

func TestCollectServiceMapStats_NodesAndEdges(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c1 := containers.NewContainer("svc-a")
	c1.Name = "service-a"
	c1.Namespace = "prod"
	c1.TCPStats["10.0.0.2:8080"] = &containers.TCPStats{SuccessfulConnects: 100, ActiveConnections: 5}

	c2 := containers.NewContainer("svc-b")
	c2.Name = "service-b"
	c2.Namespace = "prod"
	c2.TCPStats["10.0.0.3:5432"] = &containers.TCPStats{SuccessfulConnects: 50}

	ins.collectServiceMapStats([]*containers.Container{c1, c2}, slist)
	m := sampleMap(slist)

	assertMetricByTag(t, m, inputName+"_graph_nodes", "client_type", "container", 2)
	assertMetricByTag(t, m, inputName+"_graph_edges", "client_type", "container", 2)
}

func TestCollectServiceMapStats_EdgeLabels(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("frontend-001")
	c.Name = "frontend"
	c.Namespace = "prod"
	c.PodName = "frontend-pod-abc"
	c.TCPStats["10.20.30.40:5432"] = &containers.TCPStats{
		SuccessfulConnects: 10,
		ActiveConnections:  2,
		BytesSent:          4096,
		BytesReceived:      8192,
		Retransmissions:    1,
	}

	ins.collectServiceMapStats([]*containers.Container{c}, slist)
	m := sampleMap(slist)

	eConns := m[inputName+"_edge_connects_total"]
	if len(eConns) == 0 {
		t.Fatal("edge_connects_total not found")
	}
	s := eConns[0]

	wantLabels := map[string]string{
		"client_id":        "frontend-001",
		"client_name":      "frontend",
		"namespace":        "prod",
		"pod_name":         "frontend-pod-abc",
		"destination_host": "10.20.30.40",
		"destination_port": "5432",
	}
	for k, want := range wantLabels {
		if got := s.Labels[k]; got != want {
			t.Errorf("label %q: want %q, got %q", k, want, got)
		}
	}
}

func TestCollectServiceMapStats_EdgeValues(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("backend-001")
	c.TCPStats["10.0.0.1:3306"] = &containers.TCPStats{
		SuccessfulConnects: 200,
		FailedConnects:     5,
		ActiveConnections:  8,
		Retransmissions:    3,
		BytesSent:          65536,
		BytesReceived:      131072,
	}

	ins.collectServiceMapStats([]*containers.Container{c}, slist)
	m := sampleMap(slist)

	wantMetrics := map[string]float64{
		inputName + "_edge_connects_total":       200,
		inputName + "_edge_connect_failed_total": 5,
		inputName + "_edge_active_connections":   8,
		inputName + "_edge_retransmits_total":    3,
		inputName + "_edge_bytes_sent_total":     65536,
		inputName + "_edge_bytes_received_total": 131072,
	}
	for metric, wantVal := range wantMetrics {
		assertMetric(t, m, metric, wantVal)
	}
}

func TestCollectServiceMapStats_Empty(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	ins.collectServiceMapStats([]*containers.Container{}, slist)
	m := sampleMap(slist)

	assertMetric(t, m, inputName+"_graph_nodes", 0)
	assertMetric(t, m, inputName+"_graph_edges", 0)
}

func TestCollectServiceMapStats_NilContainerNoPanic(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic on nil container: %v", r)
		}
	}()
	ins.collectServiceMapStats([]*containers.Container{nil}, slist)

	m := sampleMap(slist)
	assertMetric(t, m, inputName+"_graph_nodes", 0)
}

// ─── 全链路 Gather 集成测试 ────────────────────────────────────

func TestGather_NilRegistryNoPanic(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Gather() panicked: %v", r)
		}
	}()
	ins.Gather(slist) // registry=nil → 早返回
}

func TestGather_FullPipeline(t *testing.T) {
	ins := newTestInstance()
	slist := types.NewSampleList()

	c := containers.NewContainer("app-001")
	c.Name = "webapp"
	c.Namespace = "default"
	c.PodName = "webapp-pod-xyz"

	// TCP 连接
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeConnectionOpen,
		Fd:      1,
		DstAddr: "10.0.0.100:5432",
	})

	// HTTP 请求
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.200:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 10 * time.Millisecond,
			Payload:  []byte("GET /health HTTP/1.1\r\n"),
		},
	})

	// MySQL 请求
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.100:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 2 * time.Millisecond,
		},
	})

	cs := []*containers.Container{c}
	baseTags := map[string]string{
		"container_id":   c.ID,
		"container_name": c.Name,
		"namespace":      c.Namespace,
		"pod_name":       c.PodName,
	}

	ins.collectTCPStats(c, baseTags, slist)
	ins.collectHTTPStats(c, baseTags, slist)
	ins.collectL7ProtoStats(c, baseTags, slist)
	ins.collectServiceMapStats(cs, slist)

	m := sampleMap(slist)

	// 必须存在的指标族前缀
	for _, prefix := range []string{
		inputName + "_tcp_",
		inputName + "_http_",
		inputName + "_mysql_",
		inputName + "_edge_",
		inputName + "_graph_",
	} {
		found := false
		for metric := range m {
			if strings.HasPrefix(metric, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no metrics with prefix %q found", prefix)
		}
	}

	// service map: 1 容器 → 1 个 TCP 目标 = 1 条边
	assertMetricByTag(t, m, inputName+"_graph_nodes", "client_type", "container", 1)
	assertMetricByTag(t, m, inputName+"_graph_edges", "client_type", "container", 1)

	// HTTP: 1 次成功
	assertMetric(t, m, inputName+"_http_requests_total", 1)

	// MySQL: 1 次 ok
	sMySQL := findByLabel(m[inputName+"_mysql_requests_total"], "status", "ok")
	if sMySQL == nil {
		t.Error("missing mysql_requests_total[ok]")
	}
}

// ─── 并发安全 ──────────────────────────────────────────────────

func TestCollectTCPStats_ConcurrentSafe(t *testing.T) {
	ins := newTestInstance()
	c := containers.NewContainer("cid-concurrent")

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.OnEvent(&tracer.Event{
				Type:    tracer.EventTypeConnectionOpen,
				Fd:      uint64(i),
				DstAddr: "10.0.0.1:80",
			})
		}
		close(done)
	}()

	for i := 0; i < 10; i++ {
		ins.collectTCPStats(c, map[string]string{}, types.NewSampleList())
	}
	<-done
}

// ─── 工具函数测试 ──────────────────────────────────────────────

func TestMergeTags(t *testing.T) {
	base := map[string]string{"a": "1", "b": "2"}
	extra := map[string]string{"b": "override", "c": "3"}
	result := mergeTags(base, extra)

	if result["a"] != "1" {
		t.Errorf("a: want 1, got %s", result["a"])
	}
	if result["b"] != "override" {
		t.Errorf("b: want override, got %s", result["b"])
	}
	if result["c"] != "3" {
		t.Errorf("c: want 3, got %s", result["c"])
	}
	if base["c"] != "" {
		t.Error("base map was mutated")
	}
}

func TestHTTPStatusClass(t *testing.T) {
	cases := []struct {
		code uint16
		want string
	}{
		{100, "1xx"}, {200, "2xx"}, {201, "2xx"}, {301, "3xx"},
		{400, "4xx"}, {404, "4xx"}, {500, "5xx"}, {503, "5xx"},
		{0, "unknown"}, {99, "unknown"}, {600, "unknown"},
	}
	for _, tc := range cases {
		if got := httpStatusClass(tc.code); got != tc.want {
			t.Errorf("httpStatusClass(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// ─── 配置开关测试 ──────────────────────────────────────────────

// EnableTCP=false 时不产生 TCP 指标也不产生 service map 指标
func TestConfig_DisableTCP(t *testing.T) {
	ins := &Instance{
		EnableTCP:        false,
		EnableHTTP:       true,
		DisableL7Tracing: false,
		MaxContainers:    100,
	}
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-notcp")
	c.TCPStats["10.0.0.1:5432"] = &containers.TCPStats{SuccessfulConnects: 99}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("GET / HTTP/1.1\r\n"),
		},
	})

	baseTags := map[string]string{}

	// EnableTCP=false → 不调 collectTCPStats 和 collectServiceMapStats
	if ins.EnableTCP {
		ins.collectTCPStats(c, baseTags, slist)
	}
	if ins.EnableHTTP {
		ins.collectHTTPStats(c, baseTags, slist)
	}
	if ins.EnableTCP {
		ins.collectServiceMapStats([]*containers.Container{c}, slist)
	}

	m := sampleMap(slist)

	// TCP 相关指标不应产生
	for metric := range m {
		if strings.HasPrefix(metric, inputName+"_tcp_") {
			t.Errorf("DisableTCP: unexpected metric %q", metric)
		}
		if strings.HasPrefix(metric, inputName+"_edge_") {
			t.Errorf("DisableTCP: unexpected edge metric %q", metric)
		}
	}

	// HTTP 指标应正常产生
	if _, ok := m[inputName+"_http_requests_total"]; !ok {
		t.Error("http_requests_total should be produced when EnableHTTP=true")
	}
}

// EnableHTTP=false 时不产生 HTTP 指标
func TestConfig_DisableHTTP(t *testing.T) {
	ins := &Instance{
		EnableTCP:        true,
		EnableHTTP:       false,
		DisableL7Tracing: false,
		MaxContainers:    100,
	}
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-nohttp")
	c.TCPStats["10.0.0.1:80"] = &containers.TCPStats{SuccessfulConnects: 10}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("GET / HTTP/1.1\r\n"),
		},
	})

	baseTags := map[string]string{}
	if ins.EnableTCP {
		ins.collectTCPStats(c, baseTags, slist)
	}
	if ins.EnableHTTP {
		ins.collectHTTPStats(c, baseTags, slist)
	}

	m := sampleMap(slist)

	// HTTP 指标不应产生
	for metric := range m {
		if strings.HasPrefix(metric, inputName+"_http_") {
			t.Errorf("DisableHTTP: unexpected metric %q", metric)
		}
	}

	// TCP 指标应正常产生
	if _, ok := m[inputName+"_tcp_connects_total"]; !ok {
		t.Error("tcp_connects_total should be produced when EnableTCP=true")
	}
}

// DisableL7Tracing=true 时不产生 MySQL/Postgres/Redis/Kafka 指标
func TestConfig_DisableL7Tracing(t *testing.T) {
	ins := &Instance{
		EnableTCP:        true,
		EnableHTTP:       true,
		DisableL7Tracing: true,
		MaxContainers:    100,
	}
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-nol7")
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:3306",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolMySQL,
			Status:   l7.StatusOK,
			Duration: 2 * time.Millisecond,
		},
	})
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:5432",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolPostgres,
			Status:   l7.StatusOK,
			Duration: 3 * time.Millisecond,
		},
	})

	baseTags := map[string]string{}
	// DisableL7Tracing=true → 不调 collectL7ProtoStats
	if !ins.DisableL7Tracing {
		ins.collectL7ProtoStats(c, baseTags, slist)
	}

	m := sampleMap(slist)

	l7Prefixes := []string{
		inputName + "_mysql_",
		inputName + "_postgres_",
		inputName + "_redis_",
		inputName + "_kafka_",
	}
	for metric := range m {
		for _, pfx := range l7Prefixes {
			if strings.HasPrefix(metric, pfx) {
				t.Errorf("DisableL7Tracing: unexpected metric %q", metric)
			}
		}
	}
}

// EnableTCP=false AND EnableHTTP=false → 完全没有指标
func TestConfig_AllDisabled(t *testing.T) {
	ins := &Instance{
		EnableTCP:        false,
		EnableHTTP:       false,
		DisableL7Tracing: true,
		MaxContainers:    100,
	}
	slist := types.NewSampleList()

	c := containers.NewContainer("cid-alldisabled")
	c.TCPStats["10.0.0.1:80"] = &containers.TCPStats{SuccessfulConnects: 5}
	c.OnEvent(&tracer.Event{
		Type:    tracer.EventTypeL7Request,
		DstAddr: "10.0.0.1:80",
		L7Request: &l7.RequestData{
			Protocol: l7.ProtocolHTTP,
			Status:   200,
			Duration: 5 * time.Millisecond,
			Payload:  []byte("GET / HTTP/1.1\r\n"),
		},
	})

	baseTags := map[string]string{}
	if ins.EnableTCP {
		ins.collectTCPStats(c, baseTags, slist)
	}
	if ins.EnableHTTP {
		ins.collectHTTPStats(c, baseTags, slist)
	}
	if !ins.DisableL7Tracing {
		ins.collectL7ProtoStats(c, baseTags, slist)
	}
	if ins.EnableTCP {
		ins.collectServiceMapStats([]*containers.Container{c}, slist)
	}

	all := slist.PopBackAll()
	if len(all) != 0 {
		var names []string
		for _, s := range all {
			names = append(names, s.Metric)
		}
		t.Errorf("expected 0 metrics when all disabled, got %d: %v", len(all), names)
	}
}

// MaxContainers 限制：只处理前 N 个容器的 service map 边
func TestConfig_MaxContainers(t *testing.T) {
	ins := &Instance{
		EnableTCP:     true,
		MaxContainers: 2,
	}
	slist := types.NewSampleList()

	// 创建 3 个容器，各有 1 条 TCP 连接
	var cs []*containers.Container
	for i := 0; i < 3; i++ {
		c := containers.NewContainer(fmt.Sprintf("cid-%d", i))
		c.TCPStats[fmt.Sprintf("10.0.0.%d:80", i+1)] = &containers.TCPStats{
			SuccessfulConnects: 1,
			ActiveConnections:  1,
		}
		cs = append(cs, c)
	}

	// 只传入前 MaxContainers 个
	limited := cs
	if len(limited) > ins.MaxContainers {
		limited = limited[:ins.MaxContainers]
	}
	ins.collectServiceMapStats(limited, slist)
	m := sampleMap(slist)

	// graph_nodes 应为 2（MaxContainers 限制）
	assertMetricByTag(t, m, inputName+"_graph_nodes", "client_type", "container", 2)
	// graph_edges 应为 2（每个容器 1 条边）
	assertMetricByTag(t, m, inputName+"_graph_edges", "client_type", "container", 2)
}
