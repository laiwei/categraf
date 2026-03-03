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

	"flashcat.cloud/categraf/inputs/servicemap/tracer"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var containerIDRegex = regexp.MustCompile(`[a-f0-9]{64}`)

// Config 容器注册表配置
type Config struct {
	EnableDocker  bool
	EnableK8s     bool
	EnableCgroup  bool
	DockerSocket  string
	KubeConfig    string
	MaxContainers int // P1-6: 容器数上限，0 = 不限制
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
	ctx    context.Context // P1-8: 生命周期 context
	config Config
	tracer *tracer.Tracer
	docker *client.Client
	kube   kubernetes.Interface

	k8sContainerMeta map[string]k8sContainerMeta

	containers map[string]*Container
	mu         sync.RWMutex

	stopChan chan struct{}
	wg       sync.WaitGroup // P1-8: 追踪后台 goroutine

	// pidCache 缓存 PID → cgroup-based container ID 的映射，避免重复读 /proc/<pid>/cgroup。
	// 仅存储容器化进程的稳定 ID；裸进程不缓存（防止 PID 复用产生错误映射）。
	// 由 r.mu 保护（所有访问路径均已持有 r.mu）。
	pidCache map[uint32]string
}

// NewRegistry 创建新的容器注册表
func NewRegistry(ctx context.Context, tr *tracer.Tracer, config Config) (*Registry, error) {
	r := &Registry{
		ctx:              ctx,
		config:           config,
		tracer:           tr,
		containers:       make(map[string]*Container),
		k8sContainerMeta: make(map[string]k8sContainerMeta),
		stopChan:         make(chan struct{}),
		pidCache:         make(map[uint32]string),
	}

	// P1-8: 监听 context 取消，自动触发关闭
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-r.stopChan:
			default:
				close(r.stopChan)
			}
		case <-r.stopChan:
		}
	}()

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
			log.Printf("W! servicemap: init docker client failed: %v", err)
		} else {
			r.docker = cli
		}
	}

	if config.EnableK8s {
		kubeClient, err := newKubeClient(config.KubeConfig)
		if err != nil {
			log.Printf("W! servicemap: init kubernetes client failed: %v", err)
		} else {
			r.kube = kubeClient
			r.refreshK8sContainerMeta()
		}
	}

	// 启动事件处理
	r.launchBackground(r.handleEvents)

	// P0-5: 启动容器 GC
	r.launchBackground(r.containerGCLoop)

	// 启动容器发现
	if config.EnableCgroup {
		r.launchBackground(r.discoverContainersByCgroup)
	}

	log.Println("I! servicemap: container registry initialized")
	return r, nil
}

// launchBackground 启动后台 goroutine 并追踪生命周期 (P1-8)
func (r *Registry) launchBackground(fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
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

// ─────────────────────────────────────────────────────────────
// 裸进程支持：PID → container ID 解析
// ─────────────────────────────────────────────────────────────

// resolveContainerID 将 PID 映射为 container ID，始终返回非空字符串。
// 必须在持有 r.mu 的情况下调用。
// 优先级：
//  1. pidCache（仅含 cgroup-based 稳定 ID）
//  2. cgroup-based 容器发现（Docker / K8s 容器）→ 结果写入 pidCache
//  3. 裸进程兜底："proc_<comm>"（按进程名聚合，不缓存防 PID 复用污染）
//     fallback："proc_<pid>"（/proc/<pid>/comm 不可读时，如非 Linux 平台）
func (r *Registry) resolveContainerID(pid uint32) string {
	if id, ok := r.pidCache[pid]; ok {
		return id
	}
	if id := r.getContainerIDByPID(pid); id != "" {
		r.pidCache[pid] = id
		return id
	}
	return resolveProcID(pid)
}

// resolveProcID 为裸进程（非容器化）生成合成 container ID。
// 格式：
//   - "proc_<comm>"（/proc/<pid>/comm 可读时）← 稳定，按进程名聚合同类进程
//   - "proc_<pid>"（否则，包括非 Linux 平台）← 兜底，保留 PID 以便调试
//
// 设计选择：以进程名而非 PID 作为主标识，原因：
//  1. 时间序列稳定：进程重启 PID 变化，进程名不变，Grafana 曲线不断裂
//  2. 基数可控：N 个同名进程实例共享 1 个时间序列，避免 cardinality 爆炸
//  3. 服务拓扑语义：关心「nginx 与谁通信」而非「PID 80793 与谁通信」
func resolveProcID(pid uint32) string {
	commPath := filepath.Join("/proc", fmt.Sprintf("%d", pid), "comm")
	if b, err := os.ReadFile(commPath); err == nil {
		comm := strings.TrimSpace(string(b))
		comm = sanitizeProcLabel(comm)
		if comm != "" {
			return fmt.Sprintf("proc_%s", comm) // proc_nginx, proc_python3, ...
		}
	}
	return fmt.Sprintf("proc_%d", pid) // fallback: proc_80793
}

// sanitizeProcLabel 将进程名中的非法字符替换为下划线，使其适合作为 Prometheus 标签值。
func sanitizeProcLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// enrichProcContainer 从合成 ID（proc_<comm> 或 proc_<pid>）中提取进程名，
// 填充裸进程容器的元数据，跳过 Docker / K8s API 查询。
//
// 注意：新格式 proc_<comm> 不含 PID，c.PID 保持 0（代表一类进程而非单个实例）。
// 当 /proc/<pid>/comm 不可读时（非 Linux 或 PID 已消亡），ID 退化为 proc_<pid>，
// 此时 c.Name 保留完整 ID（如 "proc_80793"）以便调试定位。
func enrichProcContainer(c *Container, id string) {
	suffix := strings.TrimPrefix(id, "proc_")
	if suffix == "" {
		c.Name = id
		return
	}
	// 若后缀全为数字（proc_<pid> 兜底格式），保留完整 ID 作为显示名（便于调试）；
	// 否则取后缀作为进程名（如 "nginx"、"my_service"）。
	allDigits := true
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		c.Name = id // "proc_80793" — 保留 proc_ 前缀，与 container_id 一致
	} else {
		c.Name = suffix // "nginx"、"my_service" 等
	}
	// PID 不嵌入 ID（proc_<comm> 聚合同名进程），c.PID 保持默认值 0
}

// processEvent 处理单个事件
func (r *Registry) processEvent(event *tracer.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// resolveContainerID 始终返回非空 ID：
	//   容器化进程 → cgroup-based container ID
	//   裸进程     → "proc_<comm>"（按进程名聚合）或 "proc_<pid>"（兜底）
	containerID := r.resolveContainerID(event.Pid)

	container := r.getOrCreateContainer(containerID)
	if container == nil {
		return
	}

	// 处理事件（包括 L4 和 L7 事件类型）
	container.OnEvent(event)
}

// updateConnectionStats 更新连接统计（含裸进程的字节流量）
func (r *Registry) updateConnectionStats() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tracer.ForEachActiveConnection(func(connID tracer.ConnectionID, conn tracer.Connection) {
		// resolveContainerID 对裸进程同样返回有效 ID（proc_<comm> 或 proc_<pid>），
		// 从而修复了裸进程字节流量统计被静默丢弃的问题。
		containerID := r.resolveContainerID(connID.PID)

		container := r.getOrCreateContainer(containerID)
		if container == nil {
			return
		}
		container.UpdateTrafficStats(connID.FD, conn.BytesSent, conn.BytesReceived)
	})
}

// getOrCreateContainer 获取或创建容器
func (r *Registry) getOrCreateContainer(id string) *Container {
	if container, exists := r.containers[id]; exists {
		return container
	}

	// P1-6: 检查容器数上限
	if r.config.MaxContainers > 0 && len(r.containers) >= r.config.MaxContainers {
		log.Printf("W! servicemap: max containers limit (%d) reached, skipping container %s", r.config.MaxContainers, id)
		return nil
	}

	container := NewContainer(id)
	if strings.HasPrefix(id, "proc_") {
		// 裸进程容器：从合成 ID 解析 PID 和进程名，跳过 Docker/K8s API 查询
		enrichProcContainer(container, id)
	} else {
		r.enrichContainerMetadata(container)
		r.enrichContainerWithK8sMetadata(container)
	}
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
		log.Printf("W! servicemap: list kubernetes pods failed: %v", err)
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
			log.Println("D! servicemap: discovering containers and refreshing metadata...")
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
		log.Printf("D! servicemap: container GC cleaned %d expired containers, %d remaining", expired, len(r.containers))
		// 清空 PID 缓存，防止 PID 复用后继续映射到已退出容器的旧 container ID
		r.pidCache = make(map[uint32]string)
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

// Close 关闭注册表 (P1-8: 等待后台 goroutine 退出)
func (r *Registry) Close() {
	select {
	case <-r.stopChan:
		// 已关闭
	default:
		close(r.stopChan)
	}
	r.wg.Wait()
	if r.docker != nil {
		_ = r.docker.Close()
	}
	log.Println("I! servicemap: container registry closed")
}
