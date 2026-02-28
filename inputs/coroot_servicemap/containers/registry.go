package containers

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
)

var containerIDRegex = regexp.MustCompile(`[a-f0-9]{64}`)

// Config 容器注册表配置
type Config struct {
	EnableDocker bool
	EnableK8s    bool
	EnableCgroup bool
	DockerSocket string
}

// Registry 容器注册表
type Registry struct {
	config Config
	tracer *tracer.Tracer

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

	r.tracer.ForEachActiveConnection(func(connID tracer.ConnectionID, conn tracer.Connection) {
		// 查找对应的容器
		containerID := r.getContainerIDByPID(connID.PID)
		if containerID == "" {
			return
		}

		container := r.getOrCreateContainer(containerID)
		container.UpdateTrafficStats(connID.FD, conn.BytesSent, conn.BytesReceived)
	})
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
	if !r.config.EnableCgroup {
		return ""
	}

	cgroupPath := filepath.Join("/proc", fmt.Sprintf("%d", pid), "cgroup")
	b, err := os.ReadFile(cgroupPath)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}

		id := extractContainerID(line)
		if id != "" {
			return id
		}
	}

	return ""
}

func extractContainerID(cgroupLine string) string {
	if cgroupLine == "" {
		return ""
	}

	if id := containerIDRegex.FindString(cgroupLine); id != "" {
		return id
	}

	parts := strings.Split(cgroupLine, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		token := strings.TrimSpace(parts[i])
		token = strings.TrimSuffix(token, ".scope")

		token = strings.TrimPrefix(token, "docker-")
		token = strings.TrimPrefix(token, "cri-containerd-")
		token = strings.TrimPrefix(token, "crio-")

		if len(token) >= 12 {
			isHex := true
			for _, ch := range token {
				if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
					isHex = false
					break
				}
			}
			if isHex {
				return token
			}
		}
	}

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
