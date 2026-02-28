package coroot_servicemap

import (
	"fmt"
	"log"
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

	// 内部状态
	tracer   *tracer.Tracer
	registry *containers.Registry
}

// Init 初始化实例
func (ins *Instance) Init() error {
	log.Printf("I! coroot_servicemap: initializing instance")

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
	t, err := tracer.NewTracer(hostNetNs, selfNetNs, ins.DisableL7Tracing)
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
		EnableDocker: true,
		EnableK8s:    ins.KubeConfigPath != "",
		EnableCgroup: ins.EnableCgroup,
		DockerSocket: ins.DockerSocketPath,
		KubeConfig:   ins.KubeConfigPath,
	}

	reg, err := containers.NewRegistry(t, regConfig)
	if err != nil {
		t.Close()
		return fmt.Errorf("failed to create registry: %w", err)
	}

	ins.registry = reg

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
	}

	if ins.EnableTCP {
		ins.collectServiceMapStats(containers, slist)
	}
}

// Drop 清理资源
func (ins *Instance) Drop() {
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

// collectTCPStats 采集TCP统计
func (ins *Instance) collectTCPStats(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	// P0-3: 使用快照方法避免并发读写竞争
	tcpStats := container.GetTCPStatsSnapshot()
	for dest, stats := range tcpStats {
		tags := mergeTags(baseTags, map[string]string{
			"destination": dest,
		})

		// 转换时间单位：毫秒 -> 秒
		totalSeconds := float64(stats.TotalTime) / 1000.0
		maxSeconds := float64(stats.MaxTime) / 1000.0
		minSeconds := float64(stats.MinTime) / 1000.0

		slist.PushFront(types.NewSample(inputName,
			"tcp_successful_connects_total",
			float64(stats.SuccessfulConnects),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"tcp_connection_time_seconds_total",
			totalSeconds,
			tags))

		if stats.SuccessfulConnects > 0 {
			slist.PushFront(types.NewSample(inputName,
				"tcp_connection_time_seconds_avg",
				totalSeconds/float64(stats.SuccessfulConnects),
				tags))

			slist.PushFront(types.NewSample(inputName,
				"tcp_connection_time_seconds_max",
				maxSeconds,
				tags))

			slist.PushFront(types.NewSample(inputName,
				"tcp_connection_time_seconds_min",
				minSeconds,
				tags))
		}

		slist.PushFront(types.NewSample(inputName,
			"tcp_active_connections",
			float64(stats.ActiveConnections),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"tcp_retransmissions_total",
			float64(stats.Retransmissions),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"tcp_bytes_sent_total",
			float64(stats.BytesSent),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"tcp_bytes_received_total",
			float64(stats.BytesReceived),
			tags))
	}
}

// collectHTTPStats 采集HTTP统计
func (ins *Instance) collectHTTPStats(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	// P0-3: 使用快照方法避免并发读写竞争
	httpStats := container.GetHTTPStatsSnapshot()
	for dest, stats := range httpStats {
		tags := mergeTags(baseTags, map[string]string{
			"destination": dest,
			"method":      stats.Method,
			"status_code": fmt.Sprintf("%d", stats.StatusCode),
		})

		// 转换时间单位：毫秒 -> 秒
		totalLatency := float64(stats.TotalLatency) / 1000.0
		maxLatency := float64(stats.MaxLatency) / 1000.0

		slist.PushFront(types.NewSample(inputName,
			"http_requests_total",
			float64(stats.RequestCount),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"http_request_errors_total",
			float64(stats.ErrorCount),
			tags))

		if stats.RequestCount > 0 {
			slist.PushFront(types.NewSample(inputName,
				"http_request_latency_seconds_total",
				totalLatency,
				tags))

			slist.PushFront(types.NewSample(inputName,
				"http_request_latency_seconds_avg",
				totalLatency/float64(stats.RequestCount),
				tags))

			slist.PushFront(types.NewSample(inputName,
				"http_request_latency_seconds_max",
				maxLatency,
				tags))
		}

		slist.PushFront(types.NewSample(inputName,
			"http_bytes_sent_total",
			float64(stats.BytesSent),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"http_bytes_received_total",
			float64(stats.BytesReceived),
			tags))
	}
}

// collectServiceMapStats 输出服务拓扑图聚合指标
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

		slist.PushFront(types.NewSample(inputName,
			"service_map_edge_successful_connects_total",
			float64(edge.SuccessfulConnects),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"service_map_edge_failed_connects_total",
			float64(edge.FailedConnects),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"service_map_edge_active_connections",
			float64(edge.ActiveConnections),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"service_map_edge_retransmissions_total",
			float64(edge.Retransmissions),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"service_map_edge_bytes_sent_total",
			float64(edge.BytesSent),
			tags))

		slist.PushFront(types.NewSample(inputName,
			"service_map_edge_bytes_received_total",
			float64(edge.BytesReceived),
			tags))
	}

	slist.PushFront(types.NewSample(inputName,
		"service_map_nodes",
		float64(len(g.Nodes)),
		map[string]string{}))

	slist.PushFront(types.NewSample(inputName,
		"service_map_edges",
		float64(len(g.Edges)),
		map[string]string{}))
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
