package coroot_servicemap

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"runtime"

	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/inputs/coroot_servicemap/containers"
	"flashcat.cloud/categraf/inputs/coroot_servicemap/servicemap"
	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
	"flashcat.cloud/categraf/types"
	"github.com/vishvananda/netns"
)

// Instance 插件实例
type Instance struct {
	config.InstanceConfig

	// 配置选项
	EnableTCP        bool     `toml:"enable_tcp"`
	EnableHTTP       bool     `toml:"enable_http"`
	EnableCgroup     bool     `toml:"enable_cgroup"`
	DisableL7Tracing bool     `toml:"disable_l7_tracing"`
	IgnorePorts      []int    `toml:"ignore_ports"`
	IgnoreCIDRs      []string `toml:"ignore_cidrs"`
	DockerSocketPath string   `toml:"docker_socket_path"`
	KubeConfigPath   string   `toml:"kubeconfig_path"`

	// P1-6: 资源限制
	MaxTrackedConnections int `toml:"max_tracked_connections"`
	MaxContainers         int `toml:"max_containers"`

	// Graph API 服务地址，例如 ":9099"；为空时不启动
	APIAddr string `toml:"api_addr"`

	// 内部状态
	ctx       context.Context
	cancel    context.CancelFunc
	tracer    *tracer.Tracer
	registry  *containers.Registry
	apiServer *http.Server
}

// Init 初始化实例
func (ins *Instance) Init() error {
	log.Printf("I! coroot_servicemap: initializing instance")

	// P1-8: 创建 context 用于优雅退出
	ins.ctx, ins.cancel = context.WithCancel(context.Background())

	// P1-6: 设置默认资源限制
	if ins.MaxTrackedConnections <= 0 {
		ins.MaxTrackedConnections = 50000
	}
	if ins.MaxContainers <= 0 {
		ins.MaxContainers = 5000
	}

	hostNetNs := netns.NsHandle(-1)
	selfNetNs := hostNetNs

	// 非 Linux 平台不支持 netns，直接使用 polling 回退模式。
	if runtime.GOOS == "linux" {
		if h, err := netns.Get(); err != nil {
			log.Printf("W! coroot_servicemap: failed to get host network namespace, continue without netns: %v", err)
		} else {
			hostNetNs = h
			selfNetNs = h
		}

		if s, err := netns.GetFromPid(1); err != nil {
			log.Printf("W! coroot_servicemap: failed to get self network namespace from pid 1, fallback to host namespace: %v", err)
		} else {
			selfNetNs = s
		}
	} else {
		log.Printf("I! coroot_servicemap: netns is unsupported on %s, running with polling fallback", runtime.GOOS)
	}

	// 创建 Tracer
	t, err := tracer.NewTracer(ins.ctx, hostNetNs, selfNetNs, ins.DisableL7Tracing, ins.MaxTrackedConnections)
	if err != nil {
		return fmt.Errorf("failed to create tracer: %w", err)
	}

	// 启动 eBPF 程序
	if err := t.Start(); err != nil {
		return fmt.Errorf("failed to start eBPF programs: %w", err)
	}

	ins.tracer = t

	// 创建容器注册表
	regConfig := containers.Config{
		EnableDocker:  true,
		EnableK8s:     ins.KubeConfigPath != "",
		EnableCgroup:  ins.EnableCgroup,
		DockerSocket:  ins.DockerSocketPath,
		KubeConfig:    ins.KubeConfigPath,
		MaxContainers: ins.MaxContainers,
	}

	reg, err := containers.NewRegistry(ins.ctx, t, regConfig)
	if err != nil {
		t.Close()
		return fmt.Errorf("failed to create registry: %w", err)
	}

	ins.registry = reg

	// 启动内嵌 Graph API server
	ins.startAPIServer()

	log.Printf("I! coroot_servicemap: instance initialized successfully")
	return nil
}

// Gather 采集数据
func (ins *Instance) Gather(slist *types.SampleList) {
	if ins.registry == nil {
		log.Println("E! coroot_servicemap: registry not initialized")
		return
	}

	// 获取所有容器数据
	containers := ins.registry.GetContainers()

	if len(containers) == 0 {
		// 即使没有容器，也尝试产生基于进程的统计
		ins.collectHostStats(slist)
		ins.collectInternalStats(slist)
		return
	}

	for _, container := range containers {
		// 构建基础标签
		tags := map[string]string{
			"container_id": container.ID,
		}

		if container.Name != "" {
			tags["container_name"] = container.Name
		}
		if container.PodName != "" {
			tags["pod_name"] = container.PodName
		}
		if container.Namespace != "" {
			tags["namespace"] = container.Namespace
		}
		if container.Image != "" {
			tags["image"] = container.Image
		}

		// 添加标签
		for k, v := range container.Labels {
			tags[k] = v
		}

		// TCP连接统计
		if ins.EnableTCP {
			ins.collectTCPStats(container, tags, slist)
		}

		// HTTP请求统计
		if ins.EnableHTTP {
			ins.collectHTTPStats(container, tags, slist)
		}

		// L7 协议统计 (MySQL/Postgres/Redis/Kafka)
		if !ins.DisableL7Tracing {
			ins.collectL7ProtoStats(container, tags, slist)
		}
	}

	if ins.EnableTCP {
		ins.collectServiceMapStats(containers, slist)
	}

	// P1-7: 内部状态指标
	ins.collectInternalStats(slist)
}

// Drop 清理资源 (P1-8: 先取消 context，再等待清理完成)
func (ins *Instance) Drop() {
	if ins.cancel != nil {
		ins.cancel()
	}

	// 先关闭 API server，再关闭 registry/tracer
	ins.stopAPIServer()

	if ins.registry != nil {
		ins.registry.Close()
	}

	if ins.tracer != nil {
		ins.tracer.Close()
	}

	log.Println("I! coroot_servicemap: instance dropped")
}

// collectHostStats 收集主机级别的统计（当没有容器时）
func (ins *Instance) collectHostStats(slist *types.SampleList) {
	if ins.tracer == nil {
		return
	}

	connCount := 0
	var totalBytesSent, totalBytesReceived uint64

	ins.tracer.ForEachActiveConnection(func(connID tracer.ConnectionID, conn tracer.Connection) {
		connCount++
		totalBytesSent += conn.BytesSent
		totalBytesReceived += conn.BytesReceived
	})

	// 即使没有连接也输出指标（值为0）
	tags := map[string]string{
		"host": "local",
	}

	slist.PushFront(types.NewSample(inputName,
		"host_active_connections",
		float64(connCount),
		tags))

	slist.PushFront(types.NewSample(inputName,
		"host_bytes_sent_total",
		float64(totalBytesSent),
		tags))

	slist.PushFront(types.NewSample(inputName,
		"host_bytes_received_total",
		float64(totalBytesReceived),
		tags))
}

// collectTCPStats 采集TCP统计 (P1-5: counter 语义; P1-7: 命名规范)
func (ins *Instance) collectTCPStats(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	tcpStats := container.GetTCPStatsSnapshot()
	for dest, stats := range tcpStats {
		tags := mergeTags(baseTags, map[string]string{
			"destination": dest,
		})

		// Counters — 累积值，下游可通过 rate() 计算速率
		slist.PushFront(types.NewSample(inputName, "tcp_connects_total", float64(stats.SuccessfulConnects), tags))
		slist.PushFront(types.NewSample(inputName, "tcp_connect_failed_total", float64(stats.FailedConnects), tags))
		slist.PushFront(types.NewSample(inputName, "tcp_retransmits_total", float64(stats.Retransmissions), tags))
		slist.PushFront(types.NewSample(inputName, "tcp_bytes_sent_total", float64(stats.BytesSent), tags))
		slist.PushFront(types.NewSample(inputName, "tcp_bytes_received_total", float64(stats.BytesReceived), tags))

		// Summary-style counters — _sum/_count 支持 avg = sum / count
		slist.PushFront(types.NewSample(inputName, "tcp_connect_duration_seconds_sum", float64(stats.TotalTime)/1000.0, tags))
		slist.PushFront(types.NewSample(inputName, "tcp_connect_duration_seconds_count", float64(stats.SuccessfulConnects), tags))

		// Gauges — 瞬时值
		slist.PushFront(types.NewSample(inputName, "tcp_active_connections", float64(stats.ActiveConnections), tags))
	}
}

// collectHTTPStats 采集HTTP统计 (P1-5: counter 语义; P1-7: 命名规范; P2-9: 增加 status_class)
func (ins *Instance) collectHTTPStats(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	httpStats := container.GetHTTPStatsSnapshot()
	for _, stats := range httpStats {
		tags := mergeTags(baseTags, map[string]string{
			"destination":  stats.DestinationAddr,
			"method":       stats.Method,
			"status_code":  fmt.Sprintf("%d", stats.StatusCode),
			"status_class": httpStatusClass(stats.StatusCode),
		})

		// Counters
		slist.PushFront(types.NewSample(inputName, "http_requests_total", float64(stats.RequestCount), tags))
		slist.PushFront(types.NewSample(inputName, "http_request_errors_total", float64(stats.ErrorCount), tags))
		slist.PushFront(types.NewSample(inputName, "http_bytes_sent_total", float64(stats.BytesSent), tags))
		slist.PushFront(types.NewSample(inputName, "http_bytes_received_total", float64(stats.BytesReceived), tags))

		// Summary-style counters
		slist.PushFront(types.NewSample(inputName, "http_request_duration_seconds_sum", float64(stats.TotalLatency)/1000.0, tags))
		slist.PushFront(types.NewSample(inputName, "http_request_duration_seconds_count", float64(stats.RequestCount), tags))
	}
}

// collectL7ProtoStats 采集非 HTTP 协议（MySQL/Postgres/Redis/Kafka）的 L7 统计
func (ins *Instance) collectL7ProtoStats(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	l7Stats := container.GetL7StatsSnapshot()
	for _, stats := range l7Stats {
		tags := mergeTags(baseTags, map[string]string{
			"destination": stats.DestinationAddr,
			"protocol":    stats.Protocol,
			"status":      stats.Status,
		})

		// 使用协议名称作为指标前缀（小写）
		var prefix string
		switch stats.Protocol {
		case "MySQL":
			prefix = "mysql"
		case "Postgres":
			prefix = "postgres"
		case "Redis":
			prefix = "redis"
		case "Kafka":
			prefix = "kafka"
		default:
			prefix = "l7"
		}

		// Counters
		slist.PushFront(types.NewSample(inputName, prefix+"_requests_total", float64(stats.RequestCount), tags))
		slist.PushFront(types.NewSample(inputName, prefix+"_request_errors_total", float64(stats.ErrorCount), tags))

		// Summary-style counters: _sum/_count
		slist.PushFront(types.NewSample(inputName, prefix+"_request_duration_seconds_sum", float64(stats.TotalLatency)/1000.0, tags))
		slist.PushFront(types.NewSample(inputName, prefix+"_request_duration_seconds_count", float64(stats.RequestCount), tags))
	}
}

// httpStatusClass 将 HTTP 状态码归类为 1xx/2xx/3xx/4xx/5xx
func httpStatusClass(code uint16) string {
	switch {
	case code >= 100 && code < 200:
		return "1xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	}
	return "unknown"
}

// collectServiceMapStats 输出服务拓扑图聚合指标 (P1-7)
func (ins *Instance) collectServiceMapStats(cs []*containers.Container, slist *types.SampleList) {
	g := servicemap.Build(cs)

	for _, edge := range g.Edges {
		tags := map[string]string{
			"source_id":   edge.Source.ID,
			"source_name": edge.Source.Name,
			"destination": edge.Destination,
		}

		if edge.Source.ContainerID != "" {
			tags["container_id"] = edge.Source.ContainerID
		}
		if edge.Source.Namespace != "" {
			tags["namespace"] = edge.Source.Namespace
		}
		if edge.Source.PodName != "" {
			tags["pod_name"] = edge.Source.PodName
		}
		if edge.DestHost != "" {
			tags["destination_host"] = edge.DestHost
		}
		if edge.DestPort != "" {
			tags["destination_port"] = edge.DestPort
		}

		// Counters
		slist.PushFront(types.NewSample(inputName, "edge_connects_total", float64(edge.SuccessfulConnects), tags))
		slist.PushFront(types.NewSample(inputName, "edge_connect_failed_total", float64(edge.FailedConnects), tags))
		slist.PushFront(types.NewSample(inputName, "edge_retransmits_total", float64(edge.Retransmissions), tags))
		slist.PushFront(types.NewSample(inputName, "edge_bytes_sent_total", float64(edge.BytesSent), tags))
		slist.PushFront(types.NewSample(inputName, "edge_bytes_received_total", float64(edge.BytesReceived), tags))

		// Gauges
		slist.PushFront(types.NewSample(inputName, "edge_active_connections", float64(edge.ActiveConnections), tags))
	}

	// 拓扑概要
	slist.PushFront(types.NewSample(inputName, "graph_nodes", float64(len(g.Nodes)), map[string]string{}))
	slist.PushFront(types.NewSample(inputName, "graph_edges", float64(len(g.Edges)), map[string]string{}))
}

// mergeTags 合并标签
func mergeTags(base, additional map[string]string) map[string]string {
	result := make(map[string]string)

	for k, v := range base {
		result[k] = v
	}

	for k, v := range additional {
		result[k] = v
	}

	return result
}

// collectInternalStats 输出插件内部状态指标 (P1-7: 自监控)
func (ins *Instance) collectInternalStats(slist *types.SampleList) {
	if ins.tracer == nil {
		return
	}

	tags := map[string]string{}
	slist.PushFront(types.NewSample(inputName, "tracer_active_connections", float64(ins.tracer.ActiveConnectionCount()), tags))
	slist.PushFront(types.NewSample(inputName, "tracer_listen_ports", float64(len(ins.tracer.GetListenPorts())), tags))

	if ins.registry != nil {
		slist.PushFront(types.NewSample(inputName, "tracked_containers", float64(len(ins.registry.GetContainers())), tags))
	}
}
