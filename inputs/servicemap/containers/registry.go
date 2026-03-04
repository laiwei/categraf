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

	// commCache 缓存 PID → 进程名（comm），由 eBPF ProcessStart 事件预热。
	// 目的：fork 时进程一定存活，可可靠读取 /proc/<pid>/comm；
	// 后续 ConnectionOpen 事件到达时进程可能已退出，直接查缓存即可。
	// ProcessExit 时清理。由 r.mu 保护。
	commCache map[uint32]string

	// listenPorts 当前已知的监听端口集合。
	// 由 ListenOpen/ListenClose 事件维护，用于判断重传事件的方向性：
	// 如果重传事件的 SrcPort 匹配监听端口，说明是服务端重传（被动连接），
	// 应跳过 OnEvent 避免生成反向边。由 r.mu 保护。
	listenPorts map[uint16]struct{}
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
		commCache:        make(map[uint32]string),
		listenPorts:      make(map[uint16]struct{}),
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

	// P1: 注入 PID 过滤回调，用于轮询模式的连接作用域收窄
	if tr != nil {
		tr.SetPIDFilter(r.GetTrackedPIDs)
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
	if r.tracer == nil {
		// 没有 tracer 时只做定期连接统计
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.stopChan:
				return
			case <-ticker.C:
				r.updateConnectionStats()
			}
		}
	}

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
	// 裸进程路径：优先使用 commCache（eBPF fork 事件预热），再实时读 /proc
	if comm, ok := r.commCache[pid]; ok {
		return fmt.Sprintf("proc_%s", comm)
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

	// 跳过 PID=0 的事件：
	// - eBPF kprobe 在 softirq/interrupt 上下文中触发时 bpf_get_current_pid_tgid() 返回 0
	//   （如 tcp_retransmit_skb、tcp_set_state 处理 accept 的被动连接）
	// - seed 扫描到 TIME_WAIT 等无归属进程的连接时 PID=0
	// 这些事件无法关联到正确的进程，只会产生 proc_0 垃圾容器。
	// 例外：ProcessStart/ProcessExit 的 PID=0 理论上不应出现，但也安全跳过。
	if event.Pid == 0 {
		return
	}

	// 所有事件类型：如果携带了进程名（tracer 层尽早读取），更新 commCache。
	// 这保证了即使 ProcessStart 与 ConnectionOpen 跨 CPU 乱序到达，
	// ConnectionOpen 自身携带的 Comm 也能被缓存供 resolveContainerID 使用。
	if event.Comm != "" {
		r.commCache[event.Pid] = event.Comm
	}

	switch event.Type {
	case tracer.EventTypeProcessStart:
		// fork 事件仅用于预热 commCache（已在上方统一处理）。
		// 不创建容器（fork 不代表有 TCP 活动）。
		return

	case tracer.EventTypeProcessExit:
		// 清理该 PID 在所有缓存中的条目，防止 PID 复用后映射错误。
		delete(r.pidCache, event.Pid)
		delete(r.commCache, event.Pid)
		return

	case tracer.EventTypeConnectionOpen, tracer.EventTypeConnectionClose,
		tracer.EventTypeTCPRetransmit, tracer.EventTypeL7Request,
		tracer.EventTypeListenOpen, tracer.EventTypeListenClose,
		tracer.EventTypeConnectionAccepted:
		// 连接相关事件 + 监听事件 + 被动连接事件均需要创建/查找容器
		// ListenOpen 使服务端进程在开始监听时即可见于拓扑图
		// ConnectionAccepted 使服务端可见但不生成反向边
		// 维护 listenPorts 集合（用于重传方向性判断）
		if event.Type == tracer.EventTypeListenOpen && event.SrcPort > 0 {
			r.listenPorts[event.SrcPort] = struct{}{}
		} else if event.Type == tracer.EventTypeListenClose && event.SrcPort > 0 {
			delete(r.listenPorts, event.SrcPort)
		}

	default:
		return
	}

	// resolveContainerID 始终返回非空 ID：
	//   容器化进程 → cgroup-based container ID
	//   裸进程     → "proc_<comm>"（按进程名聚合）或 "proc_<pid>"（兜底）
	containerID := r.resolveContainerID(event.Pid)

	container := r.getOrCreateContainer(containerID)
	if container == nil {
		return
	}

	// 诊断日志：记录连接事件及其容器归属（含 proc_ 和 Docker 容器）。
	// 使用 D! 级别避免正常运行时日志洪泛。
	if event.Type == tracer.EventTypeConnectionOpen || event.Type == tracer.EventTypeConnectionAccepted {
		log.Printf("D! servicemap: %s pid=%d → container=%s dst=%s (fd=%d)",
			event.Type, event.Pid, containerID, event.DstAddr, event.Fd)
	}

	// 处理事件（包括 L4 和 L7 事件类型）
	// ConnectionAccepted 仅创建容器节点，不记录 TCPStats（避免生成反向边）
	if event.Type == tracer.EventTypeConnectionAccepted {
		container.LastActivity = time.Now()
		return
	}
	// 服务端重传（SrcPort 匹配监听端口）：仅更新活跃时间，不记录 TCPStats。
	// tcp_retransmit_skb 对被动连接填充的 DstAddr 是客户端地址，
	// 若调用 OnEvent 会在 TCPStats[client:port] 生成反向边。
	if event.Type == tracer.EventTypeTCPRetransmit && event.SrcPort > 0 {
		if _, isListen := r.listenPorts[event.SrcPort]; isListen {
			container.LastActivity = time.Now()
			return
		}
	}
	container.OnEvent(event)
}

// updateConnectionStats 更新连接统计（含裸进程的字节流量）
// 注意：只更新已存在的容器，不创建新容器。
// 容器创建由 processEvent 负责（通过 Open/Accepted/ListenOpen 事件触发）。
// 如果此处用 getOrCreateContainer，当进程退出后 resolveContainerID 返回不同 ID
// （如 proc_<pid> 替代 proc_<comm>），会不断创建"幽灵"容器，阻止 GC 回收。
func (r *Registry) updateConnectionStats() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tracer.ForEachActiveConnection(func(connID tracer.ConnectionID, conn tracer.Connection) {
		containerID := r.resolveContainerID(connID.PID)

		container, exists := r.containers[containerID]
		if !exists {
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

	// 第一步：清理各容器内的「僵尸」连接
	// （ConnectionClose 事件丢失或 FD 不匹配导致 activeConnections 永远不为 0）
	staleConns := 0
	for _, c := range r.containers {
		staleConns += c.GCStaleConnections(containerTimeout)
	}
	if staleConns > 0 {
		log.Printf("D! servicemap: GC cleaned %d stale connections across containers", staleConns)
	}

	// 第二步：清理无活跃连接且长时间不活跃的容器
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

// GetTrackedPIDs 返回所有已解析到容器的进程 PID 集合。
// 由轮询模式的 Tracer.pollConnections 调用，用于把无关系统进程过滤出去。
// 返回集包含：
//   - pidCache 中的全部已解析 PID（容器内所有已建连连的进程）
//   - container.PID 非零的条目（在 proc_<pid> 兑退格式下有效）
//
// 注意：裸进程（proc_<comm>）不写入 pidCache（防 PID 复用污染），
// 其连接靠 Tracer 每 30s 全量扫描覆盖。
func (r *Registry) GetTrackedPIDs() map[uint32]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pids := make(map[uint32]struct{}, len(r.pidCache)+len(r.containers))

	// pidCache 包含所有已通过 cgroup 解析到容器的进程 PID（含容器内子进程）
	for pid := range r.pidCache {
		pids[pid] = struct{}{}
	}

	// container.PID 仅在 proc_<pid> 兑退格式下非零（通常为 0，表示按进程名聚合）
	for _, c := range r.containers {
		if c.PID != 0 {
			pids[c.PID] = struct{}{}
		}
	}

	return pids
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
