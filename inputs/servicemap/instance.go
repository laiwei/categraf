package servicemap

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/inputs/servicemap/containers"
	"flashcat.cloud/categraf/inputs/servicemap/graph"
	"flashcat.cloud/categraf/inputs/servicemap/tracer"
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
	EnableDocker     bool     `toml:"enable_docker"`
	EnableK8s        bool     `toml:"enable_k8s"`
	DisableL7Tracing bool     `toml:"disable_l7_tracing"`
	IgnorePorts      []int    `toml:"ignore_ports"`
	IgnoreCIDRs      []string `toml:"ignore_cidrs"`
	DockerSocketPath string   `toml:"docker_socket"`
	KubeConfigPath   string   `toml:"kubeconfig_path"`

	// P1-6: 资源限制
	MaxTrackedConnections int `toml:"max_tracked_connections"`
	MaxContainers         int `toml:"max_containers"`

	// Docker label 白名单：只有在此列表中的 label key 才会被输出为 Prometheus 标签。
	// 留空则不透传任何 Docker label（推荐，避免高基数标签导致时序爆炸）。
	LabelAllowlist []string `toml:"label_allowlist"`

	// Graph API 服务地址，例如 ":9099"；为空时不启动
	APIAddr string `toml:"api_addr"`

	// 内部状态
	ctx       context.Context
	cancel    context.CancelFunc
	tracer    *tracer.Tracer
	registry  *containers.Registry
	apiServer *http.Server
	// hostIPs 是本机所有非回环、非链路本地的 IP。
	// 用于将监听地址为 0.0.0.0/:: 的端点展开为可供跨主机 JOIN 的具体 IP。
	hostIPs []string
	// ignoredNets 是解析后的 CIDR 黑名单（对应配置项 ignore_cidrs）。
	// collectListenEndpoints 在补充 loopback 地址时会先检查黑名单，
	// 确保 ignore_cidrs=["127.0.0.0/8"] 时不会把 127.0.0.1/::1 写入 listen_endpoint 指标。
	ignoredNets []*net.IPNet

	// /metrics 端点缓存：每次 Gather() 结束时更新
	metricsMu    sync.RWMutex
	promCache    []byte
	promCacheAge time.Time
}

// Init 初始化实例
func (ins *Instance) Init() error {
	log.Printf("I! servicemap: initializing instance")

	// 收集主机非回环 IP，供监听端点 0.0.0.0 展开使用
	ins.hostIPs = gatherHostIPs()
	log.Printf("I! servicemap: host IPs detected: %v", ins.hostIPs)

	// 解析 CIDR 黑名单，供 collectListenEndpoints 过滤 loopback 补充地址使用。
	// 与 registry.NewRegistry() 中的解析逻辑保持一致：若黑名单包含 127.0.0.1，
	// 则自动追加 ::1/128，确保 IPv4/IPv6 回环地址的过滤语义对称。
	for _, cidr := range ins.IgnoreCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue // 非法 CIDR 已在 registry 侧打印过 Warning，此处静默跳过
		}
		ins.ignoredNets = append(ins.ignoredNets, ipNet)
	}
	// 自动扩展：若黑名单包含 127.0.0.1 则同时屏蔽 ::1
	for _, ipNet := range ins.ignoredNets {
		if ipNet.Contains(net.IPv4(127, 0, 0, 1)) {
			ins.ignoredNets = append(ins.ignoredNets, &net.IPNet{
				IP:   net.IPv6loopback,
				Mask: net.CIDRMask(128, 128),
			})
			break
		}
	}

	// P1-8: 创建 context 用于优雅退出
	ins.ctx, ins.cancel = context.WithCancel(context.Background())

	// P1-6: 设置默认资源限制
	if ins.MaxTrackedConnections <= 0 {
		ins.MaxTrackedConnections = 50000
	}
	if ins.MaxContainers <= 0 {
		ins.MaxContainers = 5000
	}

	// 检查指标开关：若 TCP 和 HTTP 都未启用，插件不会产生任何业务指标
	if !ins.EnableTCP && !ins.EnableHTTP {
		log.Printf("W! servicemap: both enable_tcp and enable_http are false, no metrics will be produced")
	}

	hostNetNs := netns.NsHandle(-1)
	selfNetNs := hostNetNs

	// 非 Linux 平台不支持 netns，直接使用 polling 回退模式。
	if runtime.GOOS == "linux" {
		if h, err := netns.Get(); err != nil {
			log.Printf("W! servicemap: failed to get host network namespace, continue without netns: %v", err)
		} else {
			hostNetNs = h
			selfNetNs = h
		}

		if s, err := netns.GetFromPid(1); err != nil {
			log.Printf("W! servicemap: failed to get self network namespace from pid 1, fallback to host namespace: %v", err)
		} else {
			selfNetNs = s
		}
	} else {
		log.Printf("I! servicemap: netns is unsupported on %s, running with polling fallback", runtime.GOOS)
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
		EnableDocker:  ins.EnableDocker,
		EnableK8s:     ins.EnableK8s,
		EnableCgroup:  ins.EnableCgroup,
		DockerSocket:  ins.DockerSocketPath,
		KubeConfig:    ins.KubeConfigPath,
		MaxContainers: ins.MaxContainers,
		IgnoreCIDRs:   ins.IgnoreCIDRs,
		IgnorePorts:   ins.IgnorePorts,
	}

	reg, err := containers.NewRegistry(ins.ctx, t, regConfig)
	if err != nil {
		t.Close()
		return fmt.Errorf("failed to create registry: %w", err)
	}

	ins.registry = reg

	// 启动内嵌 Graph API server（同步 bind 端口后返回，goroutine 在后台 Accept）
	ins.startAPIServer()

	// 全量扫描当前所有 LISTEN 端口并写入 registry。
	// 必须在 startAPIServer 之后调用，确保 API server 端口（如 :9099）已 bind：
	//   - eBPF 模式：seedExistingConnections 在 registry 创建前运行，且 :9099 彼时未 bind，
	//               需要此处补充；kprobe 事件也可能因竞争未被 processEvent 处理。
	//   - polling 模式：首次 pollConnections 可能早于 startAPIServer bind，
	//                  此处保证后绑端口也能被捕获。
	if ins.tracer != nil {
		ins.tracer.SeedListenPorts()
	}

	log.Printf("I! servicemap: instance initialized successfully")
	return nil
}

// Gather 采集数据
func (ins *Instance) Gather(slist *types.SampleList) {
	if ins.registry == nil {
		log.Println("E! servicemap: registry not initialized")
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
			"client_id": container.ID,
		}

		// 区分裸进程与容器化进程，便于过滤和告警分组
		if strings.HasPrefix(container.ID, "proc_") {
			tags["client_type"] = "bare_process"
		} else {
			tags["client_type"] = "container"
		}

		if container.Name != "" {
			tags["client_name"] = container.Name
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

		// 按白名单透传 Docker label，避免高基数标签导致时序爆炸
		if len(ins.LabelAllowlist) > 0 {
			allowed := make(map[string]struct{}, len(ins.LabelAllowlist))
			for _, k := range ins.LabelAllowlist {
				allowed[k] = struct{}{}
			}
			for k, v := range container.Labels {
				if _, ok := allowed[k]; ok {
					tags[k] = v
				}
			}
		}

		// TCP连接统计
		if ins.EnableTCP {
			ins.collectTCPStats(container, tags, slist)
			// 监听端点指标（用于跨主机 P2P 拓扑 JOIN）
			ins.collectListenEndpoints(container, tags, slist)
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

	// 更新 /metrics 端点缓存（非破坏性只读遍历 slist）
	ins.cacheMetrics(slist)
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

	log.Println("I! servicemap: instance dropped")
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
		slist.PushFront(types.NewSample(inputName, "tcp_session_lifetime_seconds_sum", float64(stats.TotalLifetimeMs)/1000.0, tags))
		slist.PushFront(types.NewSample(inputName, "tcp_session_lifetime_seconds_count", float64(stats.SuccessfulConnects), tags))

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
	g := graph.Build(cs)

	for _, edge := range g.Edges {
		tags := map[string]string{
			"client_id":   edge.Source.ID,
			"client_name": edge.Source.Name,
			"destination": edge.Destination,
		}

		// 区分裸进程与容器化进程
		if strings.HasPrefix(edge.Source.ID, "proc_") {
			tags["client_type"] = "bare_process"
		} else {
			tags["client_type"] = "container"
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

	// 拓扑概要：按 source_type 分拆，区分裸进程与容器的拓扑规模
	//
	// 标签设计：
	//   source_type — bare_process / container，语义自洽的分组维度
	//   kube_node   — 来自 NODE_NAME 环境变量（K8s downward API），非 K8s 时省略
	//   cluster     — 由 [instances.labels] 配置注入，框架自动附加，插件无需处理
	var nodeBareProcess, nodeContainer int
	var edgeBareProcess, edgeContainer int
	for id := range g.Nodes {
		if strings.HasPrefix(id, "proc_") {
			nodeBareProcess++
		} else {
			nodeContainer++
		}
	}
	for _, edge := range g.Edges {
		if strings.HasPrefix(edge.Source.ID, "proc_") {
			edgeBareProcess++
		} else {
			edgeContainer++
		}
	}

	// 构建上下文标签（kube_node 仅在 K8s 环境下存在）
	graphBaseTags := map[string]string{}
	if kubeNode := os.Getenv("NODE_NAME"); kubeNode != "" {
		graphBaseTags["kube_node"] = kubeNode
	}

	slist.PushFront(types.NewSample(inputName, "graph_nodes", float64(nodeBareProcess),
		mergeTags(graphBaseTags, map[string]string{"client_type": "bare_process"})))
	slist.PushFront(types.NewSample(inputName, "graph_nodes", float64(nodeContainer),
		mergeTags(graphBaseTags, map[string]string{"client_type": "container"})))
	slist.PushFront(types.NewSample(inputName, "graph_edges", float64(edgeBareProcess),
		mergeTags(graphBaseTags, map[string]string{"client_type": "bare_process"})))
	slist.PushFront(types.NewSample(inputName, "graph_edges", float64(edgeContainer),
		mergeTags(graphBaseTags, map[string]string{"client_type": "container"})))
}

// isIgnoredIP 检查给定 IP 字符串是否被 ins.ignoredNets 黑名单覆盖。
// 用于 collectListenEndpoints 在补充 loopback 地址前的过滤判断。
func (ins *Instance) isIgnoredIP(ipStr string) bool {
	if len(ins.ignoredNets) == 0 {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, ipNet := range ins.ignoredNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
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

// gatherHostIPs 返回本机所有非回环、非链路本地的 IP 地址。
// 用于将 0.0.0.0/:: 监听端点展开为可供跨主机 Prometheus JOIN 的具体 IP。
func gatherHostIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			ips = append(ips, ip.String())
		}
	}
	return ips
}

// collectListenEndpoints 采集进程/容器的监听端点指标。
// 监听地址为 0.0.0.0/:: 时，展开为主机所有非回环 IP，使 Prometheus 跨主机 JOIN 时能命中。
// 同时补充 loopback 地址，以支持经由 localhost 发起的精确匹配（常见于同主机服务间调用）；
// 但若 ignore_cidrs 配置明确覆盖了这些地址（如 "127.0.0.0/8"），则遵从用户意图跳过上报。
// 上报指标 servicemap_listen_endpoint{listen_ip, port, server_id, server_name, ...} = 1
//
// 注意：此指标中进程扮演「服务端」角色（监听端口、接受连接），使用 server_* 标签；
// 与之对应，servicemap_edge_* 中进程扮演「客户端」角色（发起连接），使用 client_* 标签。
func (ins *Instance) collectListenEndpoints(container *containers.Container, baseTags map[string]string, slist *types.SampleList) {
	endpoints := container.GetListenEndpointsSnapshot()
	if len(endpoints) == 0 {
		return
	}

	// 监听端点：进程角色是接受连接的服务端，使用 server_* 标签
	serverTags := make(map[string]string, len(baseTags))
	for k, v := range baseTags {
		switch k {
		case "client_id":
			serverTags["server_id"] = v
		case "client_name":
			serverTags["server_name"] = v
		case "client_type":
			serverTags["server_type"] = v
		default:
			serverTags[k] = v
		}
	}

	for port, boundIPs := range endpoints {
		portStr := strconv.Itoa(int(port))
		// 同一端口可能绑定多个 IP（如同时监听 127.0.0.1 和 ::1）；
		// 0.0.0.0/:: 展开为主机所有非回环 IP，具体 IP 原样保留。
		// seen 用于去重：防止 0.0.0.0 与 :: 双绑场景生成重复指标。
		seen := make(map[string]struct{})
		for _, listenIP := range boundIPs {
			var expandIPs []string
			if listenIP == "" || listenIP == "0.0.0.0" || listenIP == "::" {
				// 监听所有接口：展开为主机实际 IP（便于跨主机 JOIN）。
				// 同时补充 loopback 地址：
				//   - 0.0.0.0 → 127.0.0.1
				//   - ::      → ::1，并兼容补充 127.0.0.1 以覆盖 dual-stack localhost 场景
				// 使用 make+copy 而非 append(ins.hostIPs, ...)，
				// 避免写入 ins.hostIPs 底层数组的空余容量（slice append 陷阱）。
				expandIPs = make([]string, len(ins.hostIPs), len(ins.hostIPs)+2)
				copy(expandIPs, ins.hostIPs)
				switch listenIP {
				case "", "0.0.0.0":
					expandIPs = append(expandIPs, "127.0.0.1")
				case "::":
					expandIPs = append(expandIPs, "127.0.0.1", "::1")
				}
			} else {
				expandIPs = []string{listenIP}
			}
			for _, ip := range expandIPs {
				if ins.isIgnoredIP(ip) {
					continue
				}
				if _, dup := seen[ip]; dup {
					continue
				}
				seen[ip] = struct{}{}
				tags := mergeTags(serverTags, map[string]string{
					"listen_ip": ip,
					"port":      portStr,
				})
				// Gauge = 1（presence metric，存在即为 1，消失代表端口已关闭）
				slist.PushFront(types.NewSample(inputName, "listen_endpoint", 1.0, tags))
			}
		}
	}
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
