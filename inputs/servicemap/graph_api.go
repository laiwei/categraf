package servicemap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/containers"
	"flashcat.cloud/categraf/inputs/servicemap/graph"
)

// ─────────────────────────────────────────────────────────────
// JSON 数据结构
// ─────────────────────────────────────────────────────────────

// GraphResponse 是 /graph API 的顶层响应结构
type GraphResponse struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Summary     SummaryJSON `json:"summary"`
	Nodes       []NodeJSON  `json:"nodes"`
	Edges       []EdgeJSON  `json:"edges"`
}

// NodeJSON 代表一个服务节点（通常是一个容器或进程）
type NodeJSON struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	PodName   string            `json:"pod_name,omitempty"`
	Image     string            `json:"image,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// EdgeJSON 代表两个端点之间的一条有向连接
type EdgeJSON struct {
	// 边的标识
	ID         string `json:"id"`     // "{source}->{target}"
	Source     string `json:"source"` // node ID
	Target     string `json:"target"` // "host:port"
	TargetHost string `json:"target_host"`
	TargetPort string `json:"target_port"`

	// 该边上观察到的协议列表（e.g. ["TCP","MySQL","HTTP"]）
	Protocols []string `json:"protocols"`

	// TCP 层统计
	TCP *TCPStatsJSON `json:"tcp,omitempty"`

	// HTTP 层统计（按 method+status 分组）
	HTTP []HTTPEntryJSON `json:"http,omitempty"`

	// L7 层统计（MySQL/Postgres/Redis/Kafka，按 protocol+status 分组）
	L7 []L7EntryJSON `json:"l7,omitempty"`
}

// TCPStatsJSON TCP 连接统计
type TCPStatsJSON struct {
	ConnectsTotal        uint64  `json:"connects_total"`
	ConnectFailedTotal   uint64  `json:"connect_failed_total"`
	ActiveConnections    uint64  `json:"active_connections"`
	RetransmitsTotal     uint64  `json:"retransmits_total"`
	BytesSentTotal       uint64  `json:"bytes_sent_total"`
	BytesReceivedTotal   uint64  `json:"bytes_received_total"`
	AvgSessionLifetimeMs float64 `json:"avg_session_lifetime_ms,omitempty"`
}

// HTTPEntryJSON 单个 (method, status) 组合的 HTTP 统计
type HTTPEntryJSON struct {
	Method             string  `json:"method"`
	StatusCode         uint16  `json:"status_code"`
	StatusClass        string  `json:"status_class"`
	RequestsTotal      uint64  `json:"requests_total"`
	ErrorsTotal        uint64  `json:"errors_total"`
	BytesSentTotal     uint64  `json:"bytes_sent_total"`
	BytesReceivedTotal uint64  `json:"bytes_received_total"`
	AvgDurationMs      float64 `json:"avg_duration_ms,omitempty"`
}

// L7EntryJSON 单个 (protocol, status) 组合的 L7 统计
type L7EntryJSON struct {
	Protocol      string  `json:"protocol"`
	Status        string  `json:"status"`
	RequestsTotal uint64  `json:"requests_total"`
	ErrorsTotal   uint64  `json:"errors_total"`
	AvgDurationMs float64 `json:"avg_duration_ms,omitempty"`
}

// SummaryJSON 全局摘要，便于 AI/用户快速理解当前状态
type SummaryJSON struct {
	Nodes                   int `json:"nodes"`
	Edges                   int `json:"edges"`
	TracerActiveConnections int `json:"tracer_active_connections"`
	TracerListenPorts       int `json:"tracer_listen_ports"`
	TrackedContainers       int `json:"tracked_containers"`
}

// ─────────────────────────────────────────────────────────────
// BuildGraph — 核心构建逻辑
// ─────────────────────────────────────────────────────────────

// BuildGraph 从当前 registry 快照构建完整的 Graph 响应。
// 此方法线程安全（所有读取均通过快照方法）。
func (ins *Instance) BuildGraph() GraphResponse {
	if ins.registry == nil {
		log.Println("D! servicemap: BuildGraph called but registry is nil")
		return GraphResponse{
			GeneratedAt: time.Now(),
			Nodes:       []NodeJSON{},
			Edges:       []EdgeJSON{},
		}
	}

	cs := ins.registry.GetContainers()
	log.Printf("D! servicemap: BuildGraph: %d containers from registry", len(cs))
	resp := ins.buildGraphWithContainers(cs)

	// 收集 tracer 层面的摘要数据（在 buildGraphWithContainers 之后设置，避免被覆盖）
	if ins.tracer != nil {
		resp.Summary.TracerActiveConnections = ins.tracer.ActiveConnectionCount()
		resp.Summary.TracerListenPorts = len(ins.tracer.GetListenPorts())
	}
	resp.Summary.TrackedContainers = len(cs)

	return resp
}

// buildGraphWithContainers 从给定容器切片构建 Graph，方便测试直接注入数据。
func (ins *Instance) buildGraphWithContainers(cs []*containers.Container) GraphResponse {
	resp := GraphResponse{
		GeneratedAt: time.Now(),
		Nodes:       []NodeJSON{},
		Edges:       []EdgeJSON{},
	}

	if len(cs) == 0 {
		return resp
	}

	// 构建 TCP 拓扑图
	g := graph.Build(cs)

	// 构建 containerID → *Container 索引，用于查找 HTTP/L7 统计
	containerByID := make(map[string]*containers.Container, len(cs))
	for _, c := range cs {
		if c != nil {
			containerByID[c.ID] = c
		}
	}

	// Nodes
	nodeList := make([]NodeJSON, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		nj := NodeJSON{
			ID:        n.ID,
			Name:      n.Name,
			Namespace: n.Namespace,
			PodName:   n.PodName,
		}
		// 从容器对象补充 Image / Labels
		if c, ok := containerByID[n.ID]; ok {
			nj.Image = c.Image
			if len(c.Labels) > 0 {
				nj.Labels = make(map[string]string, len(c.Labels))
				for k, v := range c.Labels {
					nj.Labels[k] = v
				}
			}
		}
		nodeList = append(nodeList, nj)
	}
	// 保持确定性排序
	sort.Slice(nodeList, func(i, j int) bool { return nodeList[i].ID < nodeList[j].ID })
	resp.Nodes = nodeList

	// Edges — TCP 边 + HTTP/L7 统计富化
	edgeList := make([]EdgeJSON, 0, len(g.Edges))
	for _, edge := range g.Edges {
		ej := EdgeJSON{
			ID:         edge.Source.ID + "->" + edge.Destination,
			Source:     edge.Source.ID,
			Target:     edge.Destination,
			TargetHost: edge.DestHost,
			TargetPort: edge.DestPort,
			Protocols:  []string{"TCP"},
		}

		// TCP 统计
		tcpJSON := &TCPStatsJSON{
			ConnectsTotal:      edge.SuccessfulConnects,
			ConnectFailedTotal: edge.FailedConnects,
			ActiveConnections:  edge.ActiveConnections,
			RetransmitsTotal:   edge.Retransmissions,
			BytesSentTotal:     edge.BytesSent,
			BytesReceivedTotal: edge.BytesReceived,
		}
		if edge.SuccessfulConnects > 0 {
			// TotalLifetimeMs 存储在容器的 TCPStats 中；需从 container 快照读取。
			// 同时叠加活跃连接的运行时长，确保仍处于 active 状态的连接也有展示值。
			if c, ok := containerByID[edge.Source.ID]; ok {
				tcpSnapshot := c.GetTCPStatsSnapshot()
				if ts, ok := tcpSnapshot[edge.Destination]; ok && ts.SuccessfulConnects > 0 {
					activeLifetime := c.GetActiveLifetimeByDest()
					totalLifetime := ts.TotalLifetimeMs + activeLifetime[edge.Destination]
					tcpJSON.AvgSessionLifetimeMs = float64(totalLifetime) / float64(ts.SuccessfulConnects)
				}
			}
		}
		ej.TCP = tcpJSON

		// HTTP / L7 统计 — 按目标地址过滤
		if c, ok := containerByID[edge.Source.ID]; ok {
			// HTTP
			httpSnapshot := c.GetHTTPStatsSnapshot()
			var httpEntries []HTTPEntryJSON
			for _, hs := range httpSnapshot {
				if hs.DestinationAddr != edge.Destination {
					continue
				}
				he := HTTPEntryJSON{
					Method:             hs.Method,
					StatusCode:         hs.StatusCode,
					StatusClass:        httpStatusClass(hs.StatusCode),
					RequestsTotal:      hs.RequestCount,
					ErrorsTotal:        hs.ErrorCount,
					BytesSentTotal:     hs.BytesSent,
					BytesReceivedTotal: hs.BytesReceived,
				}
				if hs.RequestCount > 0 {
					he.AvgDurationMs = float64(hs.TotalLatency) / float64(hs.RequestCount)
				}
				httpEntries = append(httpEntries, he)
			}
			if len(httpEntries) > 0 {
				sort.Slice(httpEntries, func(i, j int) bool {
					if httpEntries[i].Method != httpEntries[j].Method {
						return httpEntries[i].Method < httpEntries[j].Method
					}
					return httpEntries[i].StatusCode < httpEntries[j].StatusCode
				})
				ej.HTTP = httpEntries
				addProtocol(&ej.Protocols, "HTTP")
			}

			// L7（MySQL/Postgres/Redis/Kafka）
			l7Snapshot := c.GetL7StatsSnapshot()
			var l7Entries []L7EntryJSON
			seenProtocols := map[string]bool{}
			for _, ls := range l7Snapshot {
				if ls.DestinationAddr != edge.Destination {
					continue
				}
				le := L7EntryJSON{
					Protocol:      ls.Protocol,
					Status:        ls.Status,
					RequestsTotal: ls.RequestCount,
					ErrorsTotal:   ls.ErrorCount,
				}
				if ls.RequestCount > 0 {
					le.AvgDurationMs = float64(ls.TotalLatency) / float64(ls.RequestCount)
				}
				l7Entries = append(l7Entries, le)
				seenProtocols[ls.Protocol] = true
			}
			if len(l7Entries) > 0 {
				sort.Slice(l7Entries, func(i, j int) bool {
					if l7Entries[i].Protocol != l7Entries[j].Protocol {
						return l7Entries[i].Protocol < l7Entries[j].Protocol
					}
					return l7Entries[i].Status < l7Entries[j].Status
				})
				ej.L7 = l7Entries
				for proto := range seenProtocols {
					addProtocol(&ej.Protocols, proto)
				}
			}
		}

		edgeList = append(edgeList, ej)
	}
	sort.Slice(edgeList, func(i, j int) bool { return edgeList[i].ID < edgeList[j].ID })
	resp.Edges = edgeList

	resp.Summary.Nodes = len(resp.Nodes)
	resp.Summary.Edges = len(resp.Edges)
	return resp
}

// addProtocol 向切片中添加协议名称（去重）
func addProtocol(protocols *[]string, proto string) {
	for _, p := range *protocols {
		if p == proto {
			return
		}
	}
	*protocols = append(*protocols, proto)
}

// ─────────────────────────────────────────────────────────────
// 内嵌 HTTP API 服务
// ─────────────────────────────────────────────────────────────

// startAPIServer 在 APIAddr 上启动内嵌 HTTP 服务（若 APIAddr 为空则不启动）
func (ins *Instance) startAPIServer() {
	if ins.APIAddr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/graph", ins.handleGraph)
	mux.HandleFunc("/graph/text", ins.handleGraphText)
	mux.HandleFunc("/graph/view", ins.handleGraphView)
	mux.HandleFunc("/graph/debug", ins.handleGraphDebug)
	mux.HandleFunc("/metrics", ins.handleMetrics) // Prometheus text format
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	ins.apiServer = &http.Server{
		Addr:         ins.APIAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("I! servicemap: graph API listening on http://%s/graph", ins.APIAddr)
		if err := ins.apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("E! servicemap: API server error: %v", err)
		}
	}()
}

// stopAPIServer 优雅关闭内嵌 HTTP 服务
func (ins *Instance) stopAPIServer() {
	if ins.apiServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ins.apiServer.Shutdown(ctx); err != nil {
		log.Printf("W! servicemap: API server shutdown error: %v", err)
	}
}

// handleGraph 返回 JSON 格式的 service map graph
//
// 查询参数：
//   - filter=<keyword>    按节点 ID/Name 模糊过滤（大小写不敏感），多个关键词用逗号分隔
//   - edges_only=true     仅返回有 TCP 边的节点（隐藏无连接的监听进程）
func (ins *Instance) handleGraph(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	graph := ins.BuildGraph()
	graph = ins.filterGraph(graph, r)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(graph); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleGraphText 返回人类可读 / AI 友好的纯文本格式
//
// 查询参数与 /graph 相同：filter=<keyword>, edges_only=true
func (ins *Instance) handleGraphText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	graph := ins.BuildGraph()
	graph = ins.filterGraph(graph, r)
	ins.writeGraphText(w, graph)
}

// filterGraph 根据 HTTP 查询参数过滤 GraphResponse。
// - filter: 逗号分隔的关键词列表，节点 ID 或 Name 包含任一关键词即保留（大小写不敏感）
// - edges_only: 为 "true" 时仅保留作为边 Source 或 Target 出现的节点
func (ins *Instance) filterGraph(g GraphResponse, r *http.Request) GraphResponse {
	filterStr := r.URL.Query().Get("filter")
	edgesOnly := r.URL.Query().Get("edges_only") == "true"

	if filterStr == "" && !edgesOnly {
		return g
	}

	// 解析 filter 关键词
	var keywords []string
	if filterStr != "" {
		for _, kw := range strings.Split(filterStr, ",") {
			kw = strings.TrimSpace(kw)
			if kw != "" {
				keywords = append(keywords, strings.ToLower(kw))
			}
		}
	}

	matchNode := func(n NodeJSON) bool {
		if len(keywords) == 0 {
			return true
		}
		id := strings.ToLower(n.ID)
		name := strings.ToLower(n.Name)
		for _, kw := range keywords {
			if strings.Contains(id, kw) || strings.Contains(name, kw) {
				return true
			}
		}
		return false
	}

	// edges_only: 收集所有出现在边中的节点 ID
	edgeNodeIDs := make(map[string]struct{})
	if edgesOnly {
		for _, e := range g.Edges {
			edgeNodeIDs[e.Source] = struct{}{}
			// Target 是 host:port，不是节点 ID，不加入
		}
	}

	// 过滤节点
	keptNodes := make(map[string]struct{})
	var nodes []NodeJSON
	for _, n := range g.Nodes {
		if !matchNode(n) {
			continue
		}
		if edgesOnly {
			if _, ok := edgeNodeIDs[n.ID]; !ok {
				continue
			}
		}
		nodes = append(nodes, n)
		keptNodes[n.ID] = struct{}{}
	}

	// 过滤边：只保留 Source 在保留节点中的边
	var edges []EdgeJSON
	for _, e := range g.Edges {
		if _, ok := keptNodes[e.Source]; ok {
			edges = append(edges, e)
		}
	}

	if nodes == nil {
		nodes = []NodeJSON{}
	}
	if edges == nil {
		edges = []EdgeJSON{}
	}

	g.Nodes = nodes
	g.Edges = edges
	g.Summary.Nodes = len(nodes)
	g.Summary.Edges = len(edges)
	return g
}

// writeGraphText 将 GraphResponse 渲染为纯文本写入 w，便于测试复用。
func (ins *Instance) writeGraphText(w io.Writer, graph GraphResponse) {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("=== Service Map @ %s ===\n\n", graph.GeneratedAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Summary: %d nodes, %d edges | tracer active=%d listen=%d tracked=%d\n\n",
		graph.Summary.Nodes, graph.Summary.Edges,
		graph.Summary.TracerActiveConnections,
		graph.Summary.TracerListenPorts,
		graph.Summary.TrackedContainers,
	))

	// Nodes
	sb.WriteString(fmt.Sprintf("Nodes (%d):\n", len(graph.Nodes)))
	for _, n := range graph.Nodes {
		line := fmt.Sprintf("  [%s] name=%s", n.ID, n.Name)
		if n.Namespace != "" {
			line += " ns=" + n.Namespace
		}
		if n.PodName != "" {
			line += " pod=" + n.PodName
		}
		if n.Image != "" {
			line += " image=" + n.Image
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("\n")

	// Edges
	sb.WriteString(fmt.Sprintf("Edges (%d):\n", len(graph.Edges)))
	for _, e := range graph.Edges {
		sb.WriteString(fmt.Sprintf("  %s -> %s  [%s]\n",
			e.Source, e.Target, strings.Join(e.Protocols, ", ")))

		if e.TCP != nil {
			t := e.TCP
			sb.WriteString(fmt.Sprintf("    TCP: connects=%d failed=%d active=%d retx=%d sent=%dB recv=%dB",
				t.ConnectsTotal, t.ConnectFailedTotal,
				t.ActiveConnections, t.RetransmitsTotal,
				t.BytesSentTotal, t.BytesReceivedTotal,
			))
			if t.AvgSessionLifetimeMs > 0 {
				sb.WriteString(fmt.Sprintf(" avg_lifetime=%.2fms", t.AvgSessionLifetimeMs))
			}
			sb.WriteString("\n")
		}

		for _, h := range e.HTTP {
			sb.WriteString(fmt.Sprintf("    HTTP %s %d(%s): req=%d err=%d avg=%.2fms\n",
				h.Method, h.StatusCode, h.StatusClass,
				h.RequestsTotal, h.ErrorsTotal, h.AvgDurationMs,
			))
		}

		for _, l := range e.L7 {
			sb.WriteString(fmt.Sprintf("    L7 %s[%s]: req=%d err=%d avg=%.2fms\n",
				l.Protocol, l.Status,
				l.RequestsTotal, l.ErrorsTotal, l.AvgDurationMs,
			))
		}
	}

	_, _ = fmt.Fprint(w, sb.String())

	// ASCII 拓扑图（可视化连接关系）
	if len(graph.Edges) > 0 {
		_, _ = fmt.Fprint(w, "\n")
		writeASCIITopology(w, graph)
	}
}

// ─────────────────────────────────────────────────────────────
// /graph/debug 诊断端点
// ─────────────────────────────────────────────────────────────

// handleGraphDebug 返回原始 registry 状态，用于排查 /graph 与 Gather 数据不一致的问题。
//
// 查询参数：
//   - search=<keyword>  仅显示 ID/Name 包含关键词的容器（大小写不敏感），不传则全量
func (ins *Instance) handleGraphDebug(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))

	var sb strings.Builder
	now := time.Now()

	sb.WriteString(fmt.Sprintf("=== Service Map Debug @ %s ===\n\n", now.Format(time.RFC3339)))

	// 1) 实例状态
	sb.WriteString(fmt.Sprintf("Instance: registry=%v tracer=%v EnableTCP=%v EnableHTTP=%v\n",
		ins.registry != nil, ins.tracer != nil, ins.EnableTCP, ins.EnableHTTP))

	if ins.registry == nil {
		sb.WriteString("ERROR: registry is nil, no data available\n")
		_, _ = fmt.Fprint(w, sb.String())
		return
	}

	// 2) 容器快照
	cs := ins.registry.GetContainers()

	// 统计分类
	var withEdges, withoutEdges, procContainers int
	for _, c := range cs {
		if c == nil {
			continue
		}
		tcp := c.GetTCPStatsSnapshot()
		if len(tcp) > 0 {
			withEdges++
		} else {
			withoutEdges++
		}
		if strings.HasPrefix(c.ID, "proc_") {
			procContainers++
		}
	}

	sb.WriteString(fmt.Sprintf("Containers total=%d  with_tcp_edges=%d  without_edges=%d  proc_*=%d\n",
		len(cs), withEdges, withoutEdges, procContainers))
	if search != "" {
		sb.WriteString(fmt.Sprintf("Search filter: %q\n", search))
	}
	sb.WriteString("\n")

	// 排序
	sort.Slice(cs, func(i, j int) bool {
		if cs[i] == nil || cs[j] == nil {
			return cs[i] != nil
		}
		return cs[i].ID < cs[j].ID
	})

	// 3) 容器详情
	shown := 0
	for _, c := range cs {
		if c == nil {
			continue
		}

		// 如果有搜索过滤，跳过不匹配的
		if search != "" {
			if !strings.Contains(strings.ToLower(c.ID), search) &&
				!strings.Contains(strings.ToLower(c.Name), search) {
				continue
			}
		}

		age := now.Sub(c.LastActivity).Truncate(time.Second)
		sb.WriteString(fmt.Sprintf("  [%s] name=%q pid=%d age=%s\n",
			c.ID, c.Name, c.PID, age))

		tcpStats := c.GetTCPStatsSnapshot()
		if len(tcpStats) > 0 {
			for dest, s := range tcpStats {
				sb.WriteString(fmt.Sprintf("    TCP → %s  conn=%d fail=%d active=%d retx=%d sent=%dB recv=%dB\n",
					dest, s.SuccessfulConnects, s.FailedConnects,
					s.ActiveConnections, s.Retransmissions,
					s.BytesSent, s.BytesReceived))
			}
		} else {
			sb.WriteString("    TCP → (none)\n")
		}

		httpStats := c.GetHTTPStatsSnapshot()
		if len(httpStats) > 0 {
			sb.WriteString(fmt.Sprintf("    HTTP entries: %d\n", len(httpStats)))
		}
		l7Stats := c.GetL7StatsSnapshot()
		if len(l7Stats) > 0 {
			sb.WriteString(fmt.Sprintf("    L7 entries: %d\n", len(l7Stats)))
		}

		sb.WriteString(fmt.Sprintf("    ActiveConns(tracker): %d\n", c.ActiveConnectionCount()))
		shown++
	}

	if search != "" && shown == 0 {
		sb.WriteString(fmt.Sprintf("  (no containers match %q)\n", search))
	}
	sb.WriteString("\n")

	// 4) Graph 构建验证
	g := ins.buildGraphWithContainers(cs)
	sb.WriteString(fmt.Sprintf("Graph: nodes=%d edges=%d\n", g.Summary.Nodes, g.Summary.Edges))

	// 如果有搜索条件，展示匹配的边
	if search != "" {
		for _, e := range g.Edges {
			src := strings.ToLower(e.Source)
			tgt := strings.ToLower(e.Target)
			if strings.Contains(src, search) || strings.Contains(tgt, search) {
				sb.WriteString(fmt.Sprintf("  Edge: %s -> %s\n", e.Source, e.Target))
			}
		}
	}

	// 5) Tracer 状态
	if ins.tracer != nil {
		sb.WriteString(fmt.Sprintf("\nTracer: activeConns=%d listenPorts=%d\n",
			ins.tracer.ActiveConnectionCount(), len(ins.tracer.GetListenPorts())))
	}

	sb.WriteString("\n--- Usage ---\n")
	sb.WriteString("  /graph/view                   交互式可视化拓扑（浏览器打开）\n")
	sb.WriteString("  /graph/debug                  全量诊断\n")
	sb.WriteString("  /graph/debug?search=nc        搜索含 'nc' 的容器\n")
	sb.WriteString("  /graph?filter=nc              仅输出含 'nc' 的节点和边\n")
	sb.WriteString("  /graph?edges_only=true        仅输出有 TCP 边的节点\n")
	sb.WriteString("  /graph/text?filter=nc         文本格式 + 过滤\n")

	_, _ = fmt.Fprint(w, sb.String())
}
