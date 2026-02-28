package coroot_servicemap

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

	"flashcat.cloud/categraf/inputs/coroot_servicemap/containers"
	"flashcat.cloud/categraf/inputs/coroot_servicemap/servicemap"
)

// ─────────────────────────────────────────────────────────────
// JSON 数据结构
// ─────────────────────────────────────────────────────────────

// GraphResponse 是 /graph API 的顶层响应结构
type GraphResponse struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Summary     SummaryJSON  `json:"summary"`
	Nodes       []NodeJSON   `json:"nodes"`
	Edges       []EdgeJSON   `json:"edges"`
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
	ID         string `json:"id"`           // "{source}->{target}"
	Source     string `json:"source"`       // node ID
	Target     string `json:"target"`       // "host:port"
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
	ConnectsTotal         uint64  `json:"connects_total"`
	ConnectFailedTotal    uint64  `json:"connect_failed_total"`
	ActiveConnections     uint64  `json:"active_connections"`
	RetransmitsTotal      uint64  `json:"retransmits_total"`
	BytesSentTotal        uint64  `json:"bytes_sent_total"`
	BytesReceivedTotal    uint64  `json:"bytes_received_total"`
	AvgConnectDurationMs  float64 `json:"avg_connect_duration_ms,omitempty"`
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
	Nodes                  int `json:"nodes"`
	Edges                  int `json:"edges"`
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
	resp := GraphResponse{
		GeneratedAt: time.Now(),
		Nodes:       []NodeJSON{},
		Edges:       []EdgeJSON{},
	}

	// 收集 tracer 层面的摘要数据
	if ins.tracer != nil {
		resp.Summary.TracerActiveConnections = ins.tracer.ActiveConnectionCount()
		resp.Summary.TracerListenPorts = len(ins.tracer.GetListenPorts())
	}

	if ins.registry == nil {
		return resp
	}

	cs := ins.registry.GetContainers()
	resp.Summary.TrackedContainers = len(cs)

	return ins.buildGraphWithContainers(cs)
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
	g := servicemap.Build(cs)

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
		if c, ok := containerByID[n.ContainerID]; ok {
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
			// TotalTime 存储在容器的 TCPStats 中；需从 container 快照读取
			if c, ok := containerByID[edge.Source.ContainerID]; ok {
				tcpSnapshot := c.GetTCPStatsSnapshot()
				if ts, ok := tcpSnapshot[edge.Destination]; ok && ts.SuccessfulConnects > 0 {
					tcpJSON.AvgConnectDurationMs = float64(ts.TotalTime) / float64(ts.SuccessfulConnects)
				}
			}
		}
		ej.TCP = tcpJSON

		// HTTP / L7 统计 — 按目标地址过滤
		if c, ok := containerByID[edge.Source.ContainerID]; ok {
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
		log.Printf("I! coroot_servicemap: graph API listening on http://%s/graph", ins.APIAddr)
		if err := ins.apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("E! coroot_servicemap: API server error: %v", err)
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
		log.Printf("W! coroot_servicemap: API server shutdown error: %v", err)
	}
}

// handleGraph 返回 JSON 格式的 service map graph
func (ins *Instance) handleGraph(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	graph := ins.BuildGraph()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(graph); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleGraphText 返回人类可读 / AI 友好的纯文本格式
func (ins *Instance) handleGraphText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	graph := ins.BuildGraph()
	ins.writeGraphText(w, graph)
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
			if t.AvgConnectDurationMs > 0 {
				sb.WriteString(fmt.Sprintf(" avg_connect=%.2fms", t.AvgConnectDurationMs))
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
}
