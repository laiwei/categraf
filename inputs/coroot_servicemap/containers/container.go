package containers

import (
	"sync"
	"time"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
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

	// 统计数据
	TCPStats  map[string]*TCPStats
	HTTPStats map[string]*HTTPStats

	// 活跃连接追踪
	mu                sync.RWMutex
	activeConnections map[uint64]*ConnectionTracker
	connectionsByDest map[string][]uint64
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
		activeConnections: make(map[uint64]*ConnectionTracker),
		connectionsByDest: make(map[string][]uint64),
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
	}
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
