package containers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"flashcat.cloud/categraf/inputs/coroot_servicemap/tracer"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var containerIDRegex = regexp.MustCompile(`[a-f0-9]{64}`)

// Config 容器注册表配置
type Config struct {
	EnableDocker bool
	EnableK8s    bool
	EnableCgroup bool
	DockerSocket string
	KubeConfig   string
}

const (
	// P0-5: 容器 GC 参数
	containerGCInterval = 60 * time.Second
	containerTimeout    = 10 * time.Minute
)

type k8sContainerMeta struct {
	PodName   string
	Namespace string
	Labels    map[string]string
}

// Registry 容器注册表
type Registry struct {
	config Config
	tracer *tracer.Tracer
	docker *client.Client
	kube   kubernetes.Interface

	k8sContainerMeta map[string]k8sContainerMeta

	containers map[string]*Container
	mu         sync.RWMutex

	stopChan chan struct{}
}

// NewRegistry 创建新的容器注册表
func NewRegistry(tr *tracer.Tracer, config Config) (*Registry, error) {
	r := &Registry{
		config:           config,
		tracer:           tr,
		containers:       make(map[string]*Container),
		k8sContainerMeta: make(map[string]k8sContainerMeta),
		stopChan:         make(chan struct{}),
	}

	if config.EnableDocker {
		dockerHost := "unix:///var/run/docker.sock"
		if config.DockerSocket != "" {
			dockerHost = "unix://" + config.DockerSocket
		}

		cli, err := client.NewClientWithOpts(
			client.WithHost(dockerHost),
			client.WithAPIVersionNegotiation(),
		)
		if err != nil {
			log.Printf("W! coroot_servicemap: init docker client failed: %v", err)
		} else {
			r.docker = cli
		}
	}

	if config.EnableK8s {
		kubeClient, err := newKubeClient(config.KubeConfig)
		if err != nil {
			log.Printf("W! coroot_servicemap: init kubernetes client failed: %v", err)
		} else {
			r.kube = kubeClient
			r.refreshK8sContainerMeta()
		}
	}

	// 启动事件处理
	go r.handleEvents()

	// P0-5: 启动容器 GC
	go r.containerGCLoop()

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
	r.enrichContainerMetadata(container)
	r.enrichContainerWithK8sMetadata(container)
	r.containers[id] = container
	return container
}

func (r *Registry) enrichContainerWithK8sMetadata(c *Container) {
	if c == nil || c.ID == "" || len(r.k8sContainerMeta) == 0 {
		return
	}

	meta, ok := r.k8sContainerMeta[c.ID]
	if !ok {
		for id, m := range r.k8sContainerMeta {
			if strings.HasPrefix(id, c.ID) || strings.HasPrefix(c.ID, id) {
				meta = m
				ok = true
				break
			}
		}
	}

	if !ok {
		return
	}

	if c.PodName == "" {
		c.PodName = meta.PodName
	}
	if c.Namespace == "" {
		c.Namespace = meta.Namespace
	}

	if c.Labels == nil {
		c.Labels = make(map[string]string)
	}
	for k, v := range meta.Labels {
		key := "k8s_label_" + k
		if _, exists := c.Labels[key]; !exists {
			c.Labels[key] = v
		}
	}
}

func (r *Registry) enrichContainerMetadata(c *Container) {
	if c == nil || c.ID == "" || r.docker == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ins, err := r.docker.ContainerInspect(ctx, c.ID)
	if err != nil {
		return
	}

	r.applyInspectMetadata(c, ins)
}

func (r *Registry) applyInspectMetadata(c *Container, ins container.InspectResponse) {
	if c == nil {
		return
	}

	if ins.ContainerJSONBase != nil && ins.Name != "" {
		c.Name = strings.TrimPrefix(ins.Name, "/")
	}

	if ins.Config != nil {
		if ins.Config.Image != "" {
			c.Image = ins.Config.Image
		}

		if len(ins.Config.Labels) > 0 {
			if c.Labels == nil {
				c.Labels = make(map[string]string, len(ins.Config.Labels))
			}
			for k, v := range ins.Config.Labels {
				c.Labels[k] = v
			}

			if v := ins.Config.Labels["io.kubernetes.pod.name"]; v != "" {
				c.PodName = v
			}
			if v := ins.Config.Labels["io.kubernetes.pod.namespace"]; v != "" {
				c.Namespace = v
			}
		}
	}
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

func normalizeContainerID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	if idx := strings.Index(raw, "://"); idx >= 0 {
		raw = raw[idx+3:]
	}

	raw = strings.TrimSuffix(raw, ".scope")
	return raw
}

func newKubeClient(kubeConfig string) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error

	if kubeConfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(cfg)
}

func (r *Registry) refreshK8sContainerMeta() {
	if r.kube == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pods, err := r.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("W! coroot_servicemap: list kubernetes pods failed: %v", err)
		return
	}

	metaMap := make(map[string]k8sContainerMeta)
	for i := range pods.Items {
		p := &pods.Items[i]
		indexPodContainerMeta(metaMap, p)
	}

	r.k8sContainerMeta = metaMap
}

func indexPodContainerMeta(metaMap map[string]k8sContainerMeta, p *corev1.Pod) {
	if p == nil {
		return
	}

	base := k8sContainerMeta{
		PodName:   p.Name,
		Namespace: p.Namespace,
		Labels:    p.GetLabels(),
	}

	indexStatuses := func(statuses []corev1.ContainerStatus) {
		for _, st := range statuses {
			id := normalizeContainerID(st.ContainerID)
			if id == "" {
				continue
			}
			metaMap[id] = base
		}
	}

	indexStatuses(p.Status.ContainerStatuses)
	indexStatuses(p.Status.InitContainerStatuses)
	indexStatuses(p.Status.EphemeralContainerStatuses)
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
			if r.kube != nil {
				r.mu.Lock()
				r.refreshK8sContainerMeta()
				r.mu.Unlock()
			}
			log.Println("D! coroot_servicemap: discovering containers and refreshing metadata...")
		}
	}
}

// containerGCLoop 定期清理不活跃容器 (P0-5)
func (r *Registry) containerGCLoop() {
	ticker := time.NewTicker(containerGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopChan:
			return
		case <-ticker.C:
			r.gcContainers()
		}
	}
}

// gcContainers 清理超时未活跃的容器 (P0-5)
func (r *Registry) gcContainers() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	expired := 0
	for id, c := range r.containers {
		// 跳过有活跃连接的容器
		if c.ActiveConnectionCount() > 0 {
			continue
		}
		// 跳过最近活跃的容器
		if now.Sub(c.LastActivity) < containerTimeout {
			continue
		}
		delete(r.containers, id)
		expired++
	}

	if expired > 0 {
		log.Printf("D! coroot_servicemap: container GC cleaned %d expired containers, %d remaining", expired, len(r.containers))
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
	if r.docker != nil {
		_ = r.docker.Close()
	}
	log.Println("I! coroot_servicemap: container registry closed")
}
