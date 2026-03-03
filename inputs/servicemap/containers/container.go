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

	// 活跃连接追踪 — 由 mu 保护
	mu                sync.RWMutex
	activeConnections map[uint64]*ConnectionTracker
	connectionsByDest map[string][]uint64

	// P0-5: 最后活跃时间，用于容器 GC
	LastActivity time.Time
}

// ConnectionTracker 连接追踪器
type ConnectionTracker struct {
	Destination   string
	OpenTime      time.Time
	BytesSent     uint64
	BytesReceived uint64
}

// NewContainer 创建新的容器对象
func NewContainer(id string) *Container {
	return &Container{
		ID:                id,
		Labels:            make(map[string]string),
		TCPStats:          make(map[string]*TCPStats),
		HTTPStats:         make(map[string]*HTTPStats),
		L7Stats:           make(map[string]*L7Stats),
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
	c.activeConnections[event.Fd] = &ConnectionTracker{
		Destination: dest,
		OpenTime:    time.Now(),
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
		}
	}
	c.TCPStats[dest].SuccessfulConnects++
	c.TCPStats[dest].ActiveConnections++
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

	// 更新统计
	stats.BytesSent += conn.BytesSent
	stats.BytesReceived += conn.BytesReceived
	stats.ActiveConnections--

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

// onRetransmit TCP重传
func (c *Container) onRetransmit(event *tracer.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	dest := event.DstAddr
	if dest == "" {
		return
	}

	if c.TCPStats[dest] == nil {
		c.TCPStats[dest] = &TCPStats{
			DestinationAddr: dest,
		}
	}
	c.TCPStats[dest].Retransmissions++
}

// UpdateTrafficStats 更新流量统计
func (c *Container) UpdateTrafficStats(fd uint64, sent, received uint64) {
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

	// 更新连接记录
	conn.BytesSent = sent
	conn.BytesReceived = received

	// 更新对应 destination 的统计
	if conn.Destination == "" {
		return
	}

	stats := c.TCPStats[conn.Destination]
	if stats == nil {
		stats = &TCPStats{DestinationAddr: conn.Destination}
		c.TCPStats[conn.Destination] = stats
	}

	stats.BytesSent += sentDelta
	stats.BytesReceived += receivedDelta
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

// ActiveConnectionCount 返回当前活跃连接数（线程安全）
func (c *Container) ActiveConnectionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.activeConnections)
}
