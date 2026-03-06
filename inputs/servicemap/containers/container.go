package containers

import (
	"fmt"
	"sync"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/l7"
	"flashcat.cloud/categraf/inputs/servicemap/tracer"
)

// TCPStats TCP连接统计
type TCPStats struct {
	DestinationAddr    string
	SuccessfulConnects uint64
	FailedConnects     uint64
	ActiveConnections  uint64
	Retransmissions    uint64
	TotalTime          uint64 // 所有连接的总耗时 (ms)
	MaxTime            uint64
	MinTime            uint64
	BytesSent          uint64
	BytesReceived      uint64
	// LastActivity 最后一次有连接事件或流量变化的时间，用于 graph edge GC。
	// 零值表示代码升级前创建的历史遗留条目，GCStaleTCPEdges 会跳过，等待容器 GC 自然回收。
	LastActivity time.Time
}

// HTTPStats HTTP统计
type HTTPStats struct {
	DestinationAddr string
	Method          string
	StatusCode      uint16
	RequestCount    uint64
	ErrorCount      uint64
	TotalLatency    uint64 // ms
	MaxLatency      uint64 // ms
	BytesSent       uint64
	BytesReceived   uint64
}

// L7Stats 非 HTTP 协议（MySQL/Postgres/Redis/Kafka）的 L7 统计
type L7Stats struct {
	DestinationAddr string
	Protocol        string // "MySQL", "Postgres", "Redis", "Kafka"
	Status          string // "ok", "failed", "unknown"
	RequestCount    uint64
	ErrorCount      uint64
	TotalLatency    uint64 // ms
	MaxLatency      uint64 // ms
}

// Container 容器对象
type Container struct {
	ID        string
	PID       uint32
	Name      string
	Image     string
	PodName   string
	Namespace string
	// 标签
	Labels map[string]string

	// 统计数据 — 由 mu 保护
	TCPStats  map[string]*TCPStats
	HTTPStats map[string]*HTTPStats
	L7Stats   map[string]*L7Stats // 非 HTTP 协议的 L7 统计

	// ListenEndpoints: 监听端口 → 监听 IP（由 registry 在 ListenOpen/ListenClose 时维护）
	// "0.0.0.0" 或 "::" 表示监听所有接口；具体 IP 表示绑定到特定接口。
	// 供 Gather 生成 servicemap_listen_endpoint 指标，用于跨主机 P2P 拓扑 JOIN。
	// 由 mu 保护。
	ListenEndpoints map[uint16]string

	// 活跃连接追踪 — 由 mu 保护
	mu                sync.RWMutex
	activeConnections map[uint64]*ConnectionTracker
	connectionsByDest map[string][]uint64

	// P0-5: 最后活跃时间，用于容器 GC
	LastActivity time.Time
}

// ConnectionTracker 连接追踪器
type ConnectionTracker struct {
	Destination     string
	OpenTime        time.Time
	LastSeen        time.Time // 最后一次被 UpdateTrafficStats 或 Open 触及的时间
	BytesSent       uint64
	BytesReceived   uint64
	Retransmissions uint64 // 上一轮记录的 tcpi_total_retrans 绝对值，用于计算增量
}

// NewContainer 创建新的容器对象
func NewContainer(id string) *Container {
	return &Container{
		ID:                id,
		Labels:            make(map[string]string),
		TCPStats:          make(map[string]*TCPStats),
		HTTPStats:         make(map[string]*HTTPStats),
		L7Stats:           make(map[string]*L7Stats),
		ListenEndpoints:   make(map[uint16]string),
		activeConnections: make(map[uint64]*ConnectionTracker),
		connectionsByDest: make(map[string][]uint64),
		LastActivity:      time.Now(),
	}
}

// OnEvent 处理来自eBPF的事件
func (c *Container) OnEvent(event *tracer.Event) {
	switch event.Type {
	case tracer.EventTypeConnectionOpen:
		c.onConnectionOpen(event)
	case tracer.EventTypeConnectionClose:
		c.onConnectionClose(event)
	case tracer.EventTypeConnectionFailed:
		c.onConnectionFailed(event)
	case tracer.EventTypeTCPRetransmit:
		c.onRetransmit(event)
	case tracer.EventTypeL7Request:
		c.onL7Request(event)
	}

	// P0-5: 更新最后活跃时间（无需加锁，time.Time 赋值对这个用途是安全的）
	c.LastActivity = time.Now()
}

// onConnectionOpen 连接打开
func (c *Container) onConnectionOpen(event *tracer.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	dest := event.DstAddr
	if dest == "" {
		return
	}

	// 记录活跃连接
	now := time.Now()
	c.activeConnections[event.Fd] = &ConnectionTracker{
		Destination: dest,
		OpenTime:    now,
		LastSeen:    now,
	}

	// 按目标地址分类连接
	if c.connectionsByDest[dest] == nil {
		c.connectionsByDest[dest] = []uint64{}
	}
	c.connectionsByDest[dest] = append(c.connectionsByDest[dest], event.Fd)

	// 更新TCP统计
	if c.TCPStats[dest] == nil {
		c.TCPStats[dest] = &TCPStats{
			DestinationAddr: dest,
			LastActivity:    now,
		}
	}
	c.TCPStats[dest].SuccessfulConnects++
	c.TCPStats[dest].ActiveConnections++
	c.TCPStats[dest].LastActivity = now
}

// onConnectionClose 连接关闭
func (c *Container) onConnectionClose(event *tracer.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, exists := c.activeConnections[event.Fd]
	if !exists {
		return
	}

	dest := conn.Destination
	stats := c.TCPStats[dest]
	if stats == nil {
		return
	}

	// 计算连接时长
	duration := time.Since(conn.OpenTime).Milliseconds()
	stats.TotalTime += uint64(duration)
	if uint64(duration) > stats.MaxTime {
		stats.MaxTime = uint64(duration)
	}
	if stats.MinTime == 0 || uint64(duration) < stats.MinTime {
		stats.MinTime = uint64(duration)
	}

	// 最终字节对账：使用 event 的绝对值与 tracker 最后记录的增量差值。
	// - 轮询模式：UpdateTrafficStats 已经增量添加过了，close 事件的 BytesSent
	//   等于 tracker.BytesSent（都来自同一轮 poll），delta=0，不重复计算。
	// - eBPF 模式：Close 事件携带连接累计 BytesSent，tracker.BytesSent=0
	//   （eBPF 模式下 UpdateTrafficStats 未填充），delta=全量。
	if event.BytesSent > conn.BytesSent {
		stats.BytesSent += event.BytesSent - conn.BytesSent
	}
	if event.BytesReceived > conn.BytesReceived {
		stats.BytesReceived += event.BytesReceived - conn.BytesReceived
	}
	// 饱和减法防止 uint64 下溢：若 Open 事件丢失而 Close 到达，拟再 count 不应变负
	if stats.ActiveConnections > 0 {
		stats.ActiveConnections--
	}
	stats.LastActivity = time.Now()

	// 清理活跃连接记录
	delete(c.activeConnections, event.Fd)

	// 从目标地址连接列表中删除
	fds := c.connectionsByDest[dest]
	for i, fd := range fds {
		if fd == event.Fd {
			fds = append(fds[:i], fds[i+1:]...)
			break
		}
	}
	c.connectionsByDest[dest] = fds
}

// onConnectionFailed TCP 主动连接失败（SYN_SENT → CLOSE，未到达 ESTABLISHED）
// 来源：eBPF tcp_set_state 在连接未建立时转入 TCP_CLOSE 状态时发出。
// 注意：轮询模式下，SYN_SENT 连接在週期内消失直接发 Close 事件（不区分失败/成功），
// 失败计数仅在 eBPF 模式下准确计数。
func (c *Container) onConnectionFailed(event *tracer.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, exists := c.activeConnections[event.Fd]
	if !exists {
		// 尚未建立连接追踪器时失败，直接将失败计数加到目标地址的统计中
		dest := event.DstAddr
		if dest == "" {
			return
		}
		now := time.Now()
		if c.TCPStats[dest] == nil {
			c.TCPStats[dest] = &TCPStats{DestinationAddr: dest, LastActivity: now}
		}
		c.TCPStats[dest].FailedConnects++
		c.TCPStats[dest].LastActivity = now
		return
	}

	dest := conn.Destination
	// 饱和减法：防止异常情况下 uint64 下溢
	stats := c.TCPStats[dest]
	if stats != nil && stats.ActiveConnections > 0 {
		stats.ActiveConnections--
	}
	if stats != nil {
		stats.FailedConnects++
		stats.LastActivity = time.Now()
	}

	// 清理连接追踪器
	delete(c.activeConnections, event.Fd)
	fds := c.connectionsByDest[dest]
	for i, fd := range fds {
		if fd == event.Fd {
			fds = append(fds[:i], fds[i+1:]...)
			break
		}
	}
	c.connectionsByDest[dest] = fds
}

// onRetransmit TCP重传
func (c *Container) onRetransmit(event *tracer.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	dest := event.DstAddr
	if dest == "" {
		return
	}

	now := time.Now()
	if c.TCPStats[dest] == nil {
		c.TCPStats[dest] = &TCPStats{
			DestinationAddr: dest,
			LastActivity:    now,
		}
	}
	c.TCPStats[dest].Retransmissions++
	c.TCPStats[dest].LastActivity = now
}

// UpdateTrafficStats 更新流量和重传统计。
// sent/received/totalRetrans 均为绝对值（轮询模式下来自 INET_DIAG_INFO tcp_info）；
// 方法内部计算增量并累加到 TCPStats。
// totalRetrans=0 表示未获取到重传数据（内核不支持或 eBPF 模式下由事件直接累计）。
func (c *Container) UpdateTrafficStats(fd uint64, sent, received, totalRetrans uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, exists := c.activeConnections[fd]
	if !exists {
		return
	}

	// 计算增量
	var sentDelta uint64
	if sent > conn.BytesSent {
		sentDelta = sent - conn.BytesSent
	}

	var receivedDelta uint64
	if received > conn.BytesReceived {
		receivedDelta = received - conn.BytesReceived
	}

	var retransDelta uint64
	if totalRetrans > conn.Retransmissions {
		retransDelta = totalRetrans - conn.Retransmissions
	}

	// 更新连接记录
	now := time.Now()
	conn.BytesSent = sent
	conn.BytesReceived = received
	conn.Retransmissions = totalRetrans
	conn.LastSeen = now

	// 更新对应 destination 的统计
	if conn.Destination == "" {
		return
	}

	stats := c.TCPStats[conn.Destination]
	if stats == nil {
		stats = &TCPStats{DestinationAddr: conn.Destination, LastActivity: now}
		c.TCPStats[conn.Destination] = stats
	}

	stats.BytesSent += sentDelta
	stats.BytesReceived += receivedDelta
	stats.Retransmissions += retransDelta
	stats.LastActivity = now
}

// ============================================================
// P2-9: L7 协议事件处理
// ============================================================

// onL7Request 处理 L7 请求/响应事件
func (c *Container) onL7Request(event *tracer.Event) {
	if event.L7Request == nil {
		return
	}

	r := event.L7Request

	switch r.Protocol {
	case l7.ProtocolHTTP:
		c.onHTTPRequest(event)
	case l7.ProtocolMySQL:
		c.onMySQLRequest(event)
	case l7.ProtocolPostgres:
		c.onPostgresRequest(event)
	case l7.ProtocolRedis:
		c.onRedisRequest(event)
	case l7.ProtocolKafka:
		c.onKafkaRequest(event)
	default:
		// 未支持的协议，静默跳过
	}
}

// onHTTPRequest 处理 HTTP L7 事件，填充 HTTPStats
func (c *Container) onHTTPRequest(event *tracer.Event) {
	r := event.L7Request
	if r == nil {
		return
	}

	// 解析 HTTP 方法和路径
	method, _ := l7.ParseHTTP(r.Payload)
	if method == "" {
		method = "UNKNOWN"
	}

	// 状态码
	statusCode := uint16(r.Status)

	// 确定目标地址
	dest := event.DstAddr
	if dest == "" {
		dest = "unknown"
	}

	// 构造聚合 key: destination + method + status_class
	// HTTPStats 的 key 使用 "dest|method|status_class" 以便细粒度聚合
	statusClass := r.Status.HTTPStatusClass()
	key := fmt.Sprintf("%s|%s|%s", dest, method, statusClass)

	c.mu.Lock()
	defer c.mu.Unlock()

	stats := c.HTTPStats[key]
	if stats == nil {
		stats = &HTTPStats{
			DestinationAddr: dest,
			Method:          method,
			StatusCode:      statusCode,
		}
		c.HTTPStats[key] = stats
	}

	stats.RequestCount++
	if r.Status.IsError() {
		stats.ErrorCount++
	}

	latencyMs := uint64(r.Duration.Milliseconds())
	stats.TotalLatency += latencyMs
	if latencyMs > stats.MaxLatency {
		stats.MaxLatency = latencyMs
	}
}

// observeL7Stats 通用的非 HTTP 协议统计汇聚辅助方法
func (c *Container) observeL7Stats(event *tracer.Event, protocol string) {
	r := event.L7Request
	if r == nil {
		return
	}

	// 忽略 MethodStatementClose（仅维护状态，不产生统计）
	if r.Method == l7.MethodStatementClose {
		return
	}

	dest := event.DstAddr
	if dest == "" {
		dest = "unknown"
	}

	status := r.Status.String()
	key := fmt.Sprintf("%s|%s|%s", dest, protocol, status)

	c.mu.Lock()
	defer c.mu.Unlock()

	stats := c.L7Stats[key]
	if stats == nil {
		stats = &L7Stats{
			DestinationAddr: dest,
			Protocol:        protocol,
			Status:          status,
		}
		c.L7Stats[key] = stats
	}

	stats.RequestCount++
	if r.Status.Error() {
		stats.ErrorCount++
	}

	latencyMs := uint64(r.Duration.Milliseconds())
	stats.TotalLatency += latencyMs
	if latencyMs > stats.MaxLatency {
		stats.MaxLatency = latencyMs
	}
}

// onMySQLRequest 处理 MySQL L7 事件
func (c *Container) onMySQLRequest(event *tracer.Event) {
	c.observeL7Stats(event, l7.ProtocolMySQL.String())
}

// onPostgresRequest 处理 PostgreSQL L7 事件
func (c *Container) onPostgresRequest(event *tracer.Event) {
	c.observeL7Stats(event, l7.ProtocolPostgres.String())
}

// onRedisRequest 处理 Redis L7 事件
func (c *Container) onRedisRequest(event *tracer.Event) {
	c.observeL7Stats(event, l7.ProtocolRedis.String())
}

// onKafkaRequest 处理 Kafka L7 事件
func (c *Container) onKafkaRequest(event *tracer.Event) {
	c.observeL7Stats(event, l7.ProtocolKafka.String())
}

// ============================================================
// P0-3: 线程安全的快照方法 — 供 Gather() 使用
// ============================================================

// GetTCPStatsSnapshot 返回 TCPStats 的深拷贝（线程安全）
func (c *Container) GetTCPStatsSnapshot() map[string]*TCPStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snapshot := make(map[string]*TCPStats, len(c.TCPStats))
	for k, v := range c.TCPStats {
		if v == nil {
			continue
		}
		cp := *v // 值拷贝
		snapshot[k] = &cp
	}
	return snapshot
}

// GetHTTPStatsSnapshot 返回 HTTPStats 的深拷贝（线程安全）
func (c *Container) GetHTTPStatsSnapshot() map[string]*HTTPStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snapshot := make(map[string]*HTTPStats, len(c.HTTPStats))
	for k, v := range c.HTTPStats {
		if v == nil {
			continue
		}
		cp := *v
		snapshot[k] = &cp
	}
	return snapshot
}

// GetL7StatsSnapshot 返回 L7Stats 的深拷贝（线程安全）
func (c *Container) GetL7StatsSnapshot() map[string]*L7Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snapshot := make(map[string]*L7Stats, len(c.L7Stats))
	for k, v := range c.L7Stats {
		if v == nil {
			continue
		}
		cp := *v
		snapshot[k] = &cp
	}
	return snapshot
}

// AddListenEndpoint 记录该容器在 port 上的监听（listenIP 可为 "0.0.0.0"/"::" 或具体 IP）
func (c *Container) AddListenEndpoint(port uint16, listenIP string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ListenEndpoints[port] = listenIP
}

// RemoveListenEndpoint 删除该容器在 port 上的监听记录
func (c *Container) RemoveListenEndpoint(port uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ListenEndpoints, port)
}

// GetListenEndpointsSnapshot 返回 ListenEndpoints 的深拷贝（线程安全）
func (c *Container) GetListenEndpointsSnapshot() map[uint16]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot := make(map[uint16]string, len(c.ListenEndpoints))
	for port, ip := range c.ListenEndpoints {
		snapshot[port] = ip
	}
	return snapshot
}

// ActiveConnectionCount 返回当前活跃连接数（线程安全）
func (c *Container) ActiveConnectionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.activeConnections)
}

// RefreshLiveConnections 批量刷新仍在系统中存活的连接的 LastSeen 时间戳。
// liveDests 是通过 netlink/gopsutil 获取的当前 ESTABLISHED 连接目标地址集合。
// 返回刷新的连接数。
//
// 同时刷新对应 TCPStats.LastActivity：
//
//	长期空闲但仍存活的 TCP 连接（如数据库连接池、nc 持久连接）不会产生任何
//	eBPF 事件，若不刷新 TCPStats.LastActivity，这些正常使用的边会被 edge GC 误删。
func (c *Container) RefreshLiveConnections(liveDests map[string]struct{}, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	refreshed := 0
	for _, conn := range c.activeConnections {
		if _, live := liveDests[conn.Destination]; live {
			conn.LastSeen = now
			// 同步刷新 TCPStats.LastActivity，防止长期空闲但存活的连接的边被 edge GC 误删
			if stats, ok := c.TCPStats[conn.Destination]; ok && stats != nil {
				stats.LastActivity = now
			}
			refreshed++
		}
	}
	return refreshed
}

// GCStaleTCPEdges 清理长时间无活跃连接且不活跃的 TCP 边（graph edge）。
//
// 清理条件（同时满足）：
//  1. stats.ActiveConnections == 0 — 无连接在途，清理安全
//  2. stats.LastActivity 非零且超过 maxAge — 确实属于死亡边
//
// LastActivity 为零值说明是代码升级前的历史遗留条目（未写过该字段），
// 为防止升级后首次 GC 大量误删，保留这些条目等待容器 GC 自然回收。
// 返回清理的条目数。
func (c *Container) GCStaleTCPEdges(maxAge time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	cleaned := 0
	for dest, stats := range c.TCPStats {
		if stats == nil {
			delete(c.TCPStats, dest)
			cleaned++
			continue
		}
		// 有活跃连接的边必须保留
		if stats.ActiveConnections > 0 {
			continue
		}
		// 零值：历史遗留条目，跳过（不主动清理）
		if stats.LastActivity.IsZero() {
			continue
		}
		if now.Sub(stats.LastActivity) < maxAge {
			continue
		}
		delete(c.TCPStats, dest)
		cleaned++
	}
	return cleaned
}

// GCStaleConnections 清理长时间没有任何流量更新的「僵尸」连接。
// 场景：eBPF ConnectionClose 事件丢失（perf buffer 溢出）或 seed 阶段创建的连接
// 与 eBPF 的 FD（sk_ptr）不匹配，导致 activeConnections 永远无法通过 onConnectionClose 清除。
// 返回清理数量。
func (c *Container) GCStaleConnections(maxAge time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	cleaned := 0
	for fd, conn := range c.activeConnections {
		if now.Sub(conn.LastSeen) < maxAge {
			continue
		}
		// 清理统计中的 ActiveConnections 计数
		if stats, ok := c.TCPStats[conn.Destination]; ok && stats.ActiveConnections > 0 {
			stats.ActiveConnections--
		}
		// 从目标地址连接列表中删除
		if fds := c.connectionsByDest[conn.Destination]; len(fds) > 0 {
			for i, f := range fds {
				if f == fd {
					c.connectionsByDest[conn.Destination] = append(fds[:i], fds[i+1:]...)
					break
				}
			}
		}
		delete(c.activeConnections, fd)
		cleaned++
	}
	return cleaned
}
