package coroot_servicemap

import (
	"fmt"
	"log"

	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/inputs/coroot_servicemap/containers"
	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
	"flashcat.cloud/categraf/types"
	"github.com/vishvananda/netns"
)

// Instance 插件实例
type Instance struct {
	config.InstanceConfig

	// 配置选项
	EnableTCP         bool   `toml:"enable_tcp"`
	EnableHTTP        bool   `toml:"enable_http"`
	EnableCgroup      bool   `toml:"enable_cgroup"`
	DisableL7Tracing  bool   `toml:"disable_l7_tracing"`
	IgnorePorts       []int  `toml:"ignore_ports"`
	IgnoreCIDRs       []string `toml:"ignore_cidrs"`
	DockerSocketPath  string `toml:"docker_socket_path"`
	KubeConfigPath    string `toml:"kubeconfig_path"`

	// 内部状态
	tracer   *tracer.Tracer
	registry *containers.Registry
}

// Init 初始化实例
func (ins *Instance) Init() error {
	log.Printf("I! coroot_servicemap: initializing instance")

	// 获取网络命名空间
	hostNetNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get host network namespace: %w", err)
	}
	defer hostNetNs.Close()

	selfNetNs, err := netns.GetFromPid(1)
	if err != nil {
		// 如果在容器中，直接使用当前命名空间
		selfNetNs = hostNetNs
	}
	defer selfNetNs.Close()

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
func (ins *Instance) Gather(slist *types.SampleList) error {
	if ins.registry == nil {
		return fmt.Errorf("registry not initialized")
	}

	// 获取所有容器数据
	containers := ins.registry.GetContainers()
	if len(containers) == 0 {
		log.Println("D! coroot_servicemap: no containers found")
		return nil
	}

	for _, container := range containers {
		// 构建基础标签
		tags := map[string]string{
			"container_id": container.ID,
		}

		if container.Name != "" {
			tags["container_name"] = container.Name
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

	return nil
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

// collectTCPStats 采集TCP统计
func (ins *Instance) collectTCPStats(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	for dest, stats := range container.TCPStats {
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
	for dest, stats := range container.HTTPStats {
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
