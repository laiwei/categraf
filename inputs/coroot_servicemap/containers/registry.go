package containers

import (
	"log"
	"sync"
	"time"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
)

// Config 容器注册表配置
type Config struct {
	EnableDocker bool
	EnableK8s    bool
	EnableCgroup bool
	DockerSocket string
}

// Registry 容器注册表
type Registry struct {
	config  Config
	tracer  *tracer.Tracer

	containers map[string]*Container
	mu         sync.RWMutex

	stopChan chan struct{}
}

// NewRegistry 创建新的容器注册表
func NewRegistry(tr *tracer.Tracer, config Config) (*Registry, error) {
	r := &Registry{
		config:     config,
		tracer:     tr,
		containers: make(map[string]*Container),
		stopChan:   make(chan struct{}),
	}

	// 启动事件处理
	go r.handleEvents()

	// 启动容器发现
	if config.EnableCgroup {
		go r.discoverContainersByCgroup()
	}

	log.Println("I! coroot_servicemap: container registry initialized")
	return r, nil
}

// handleEvents 处理eBPF事件
func (r *Registry) handleEvents() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopChan:
			return
		case event := <-r.tracer.Events():
			r.processEvent(&event)
		case <-ticker.C:
			r.updateConnectionStats()
		}
	}
}

// processEvent 处理单个事件
func (r *Registry) processEvent(event *tracer.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 根据PID查找或创建容器
	containerID := r.getContainerIDByPID(event.Pid)
	if containerID == "" {
		// 未知容器，使用PID作为ID
		containerID = "unknown"
	}

	container := r.getOrCreateContainer(containerID)

	// 处理事件
	container.OnEvent(event)
}

// updateConnectionStats 更新连接统计
func (r *Registry) updateConnectionStats() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 从eBPF map读取连接统计
	iter := r.tracer.GetActiveConnections()
	if iter == nil {
		return
	}

	var connID tracer.ConnectionID
	var conn tracer.Connection

	for iter.Next(&connID, &conn) {
		// 查找对应的容器
		containerID := r.getContainerIDByPID(connID.PID)
		if containerID == "" {
			continue
		}

		container := r.getOrCreateContainer(containerID)
		container.UpdateTrafficStats(connID.FD, conn.BytesSent, conn.BytesReceived)
	}

	if err := iter.Err(); err != nil {
		log.Printf("E! coroot_servicemap: error iterating connections: %v", err)
	}
}

// getOrCreateContainer 获取或创建容器
func (r *Registry) getOrCreateContainer(id string) *Container {
	if container, exists := r.containers[id]; exists {
		return container
	}

	container := NewContainer(id)
	r.containers[id] = container
	return container
}

// getContainerIDByPID 根据PID获取容器ID（简化版）
func (r *Registry) getContainerIDByPID(pid uint32) string {
	// TODO: 通过cgroup路径识别容器ID
	// 读取 /proc/<pid>/cgroup
	// 解析容器ID (Docker/Kubernetes)
	return ""
}

// discoverContainersByCgroup 通过cgroup发现容器（待实现）
func (r *Registry) discoverContainersByCgroup() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopChan:
			return
		case <-ticker.C:
			// TODO: 扫描 /sys/fs/cgroup
			// 发现新容器
			log.Println("D! coroot_servicemap: discovering containers...")
		}
	}
}

// GetContainers 获取所有容器
func (r *Registry) GetContainers() []*Container {
	r.mu.RLock()
	defer r.mu.RUnlock()

	containers := make([]*Container, 0, len(r.containers))
	for _, container := range r.containers {
		containers = append(containers, container)
	}
	return containers
}

// Close 关闭注册表
func (r *Registry) Close() {
	close(r.stopChan)
	log.Println("I! coroot_servicemap: container registry closed")
}
