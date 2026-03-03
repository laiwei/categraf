package containers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newBareRegistry 创建一个不启动任何 goroutine 的 Registry，用于单元测试。
// 通过直接赋值字段绕过 NewRegistry（后者会连接 Docker/k8s）。
func newBareRegistry(cfg Config) *Registry {
	return &Registry{
		ctx:              context.Background(),
		config:           cfg,
		containers:       make(map[string]*Container),
		k8sContainerMeta: make(map[string]k8sContainerMeta),
		stopChan:         make(chan struct{}),
		pidCache:         make(map[uint32]string),
		commCache:        make(map[uint32]string),
	}
}

// ─── GetContainers ────────────────────────────────────────────

func TestGetContainers_Empty(t *testing.T) {
	r := newBareRegistry(Config{})
	cs := r.GetContainers()
	if len(cs) != 0 {
		t.Errorf("expected 0 containers, got %d", len(cs))
	}
}

func TestGetContainers_ReturnsAll(t *testing.T) {
	r := newBareRegistry(Config{})
	r.containers["c1"] = NewContainer("c1")
	r.containers["c2"] = NewContainer("c2")
	r.containers["c3"] = NewContainer("c3")

	cs := r.GetContainers()
	if len(cs) != 3 {
		t.Errorf("expected 3 containers, got %d", len(cs))
	}
}

func TestGetContainers_ReturnsCopy(t *testing.T) {
	r := newBareRegistry(Config{})
	r.containers["c1"] = NewContainer("c1")

	cs := r.GetContainers()
	// 修改切片不应影响 registry 内部
	cs = append(cs, NewContainer("extra"))
	if len(r.containers) != 1 {
		t.Error("modifying returned slice should not affect registry")
	}
}

// ─── getOrCreateContainer ─────────────────────────────────────

func TestGetOrCreateContainer_CreatesNew(t *testing.T) {
	r := newBareRegistry(Config{})
	c := r.getOrCreateContainer("new-id")
	if c == nil {
		t.Fatal("expected non-nil container")
	}
	if c.ID != "new-id" {
		t.Errorf("expected ID=new-id, got %s", c.ID)
	}
	if len(r.containers) != 1 {
		t.Errorf("expected 1 container in registry, got %d", len(r.containers))
	}
}

func TestGetOrCreateContainer_ReturnsExisting(t *testing.T) {
	r := newBareRegistry(Config{})
	existing := NewContainer("existing")
	existing.Name = "myapp"
	r.containers["existing"] = existing

	got := r.getOrCreateContainer("existing")
	if got != existing {
		t.Error("should return the same pointer for existing container")
	}
	if len(r.containers) != 1 {
		t.Error("should not create a new entry for existing container")
	}
}

func TestGetOrCreateContainer_MaxContainersLimit(t *testing.T) {
	r := newBareRegistry(Config{MaxContainers: 2})
	r.getOrCreateContainer("c1")
	r.getOrCreateContainer("c2")

	// 第三个应被拒绝
	got := r.getOrCreateContainer("c3")
	if got != nil {
		t.Error("expected nil when MaxContainers limit reached")
	}
	if len(r.containers) != 2 {
		t.Errorf("expected 2 containers, got %d", len(r.containers))
	}
}

func TestGetOrCreateContainer_MaxContainersZeroMeansUnlimited(t *testing.T) {
	r := newBareRegistry(Config{MaxContainers: 0})
	for i := 0; i < 100; i++ {
		id := string(rune('a' + i))
		c := r.getOrCreateContainer(id)
		if c == nil {
			t.Fatalf("expected non-nil at i=%d with no limit", i)
		}
	}
}

// ─── gcContainers ─────────────────────────────────────────────

func TestGcContainers_RemovesExpired(t *testing.T) {
	r := newBareRegistry(Config{})

	old := NewContainer("old")
	old.LastActivity = time.Now().Add(-(containerTimeout + time.Second))

	fresh := NewContainer("fresh")
	fresh.LastActivity = time.Now()

	r.containers["old"] = old
	r.containers["fresh"] = fresh

	r.gcContainers()

	if _, ok := r.containers["old"]; ok {
		t.Error("expired container should be removed")
	}
	if _, ok := r.containers["fresh"]; !ok {
		t.Error("fresh container should be kept")
	}
}

func TestGcContainers_KeepsActiveConnections(t *testing.T) {
	r := newBareRegistry(Config{})

	// 容器超时，但有活跃连接，不应被 GC
	c := NewContainer("active")
	c.LastActivity = time.Now().Add(-(containerTimeout + time.Hour))
	// 注入活跃连接
	c.activeConnections[1] = &ConnectionTracker{Destination: "10.0.0.1:80"}
	r.containers["active"] = c

	r.gcContainers()

	if _, ok := r.containers["active"]; !ok {
		t.Error("container with active connections should not be GC'd")
	}
}

func TestGcContainers_Empty(t *testing.T) {
	r := newBareRegistry(Config{})
	// 空注册表，不应 panic
	r.gcContainers()
	if len(r.containers) != 0 {
		t.Error("empty registry should remain empty after GC")
	}
}

func TestGcContainers_RemovesAll(t *testing.T) {
	r := newBareRegistry(Config{})
	for _, id := range []string{"a", "b", "c"} {
		c := NewContainer(id)
		c.LastActivity = time.Now().Add(-(containerTimeout + time.Hour))
		r.containers[id] = c
	}

	r.gcContainers()
	if len(r.containers) != 0 {
		t.Errorf("all expired containers should be removed, got %d", len(r.containers))
	}
}

// ─── enrichContainerWithK8sMetadata ──────────────────────────

func TestEnrichContainerWithK8sMetadata_ExactMatch(t *testing.T) {
	r := newBareRegistry(Config{})
	r.k8sContainerMeta["abc123"] = k8sContainerMeta{
		PodName:   "my-pod",
		Namespace: "production",
		Labels:    map[string]string{"app": "web"},
	}

	c := NewContainer("abc123")
	r.enrichContainerWithK8sMetadata(c)

	if c.PodName != "my-pod" {
		t.Errorf("PodName: want my-pod, got %s", c.PodName)
	}
	if c.Namespace != "production" {
		t.Errorf("Namespace: want production, got %s", c.Namespace)
	}
	if c.Labels["k8s_label_app"] != "web" {
		t.Errorf("Label k8s_label_app: want web, got %s", c.Labels["k8s_label_app"])
	}
}

func TestEnrichContainerWithK8sMetadata_PrefixMatch(t *testing.T) {
	r := newBareRegistry(Config{})
	// 注册表中有完整 64 位 ID，容器是短 ID（12 位前缀）
	fullID := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	r.k8sContainerMeta[fullID] = k8sContainerMeta{PodName: "pod-from-prefix"}

	shortID := "abcdef123456"
	c := NewContainer(shortID)
	r.enrichContainerWithK8sMetadata(c)

	if c.PodName != "pod-from-prefix" {
		t.Errorf("prefix match failed: got PodName=%q", c.PodName)
	}
}

func TestEnrichContainerWithK8sMetadata_NoMatch(t *testing.T) {
	r := newBareRegistry(Config{})
	r.k8sContainerMeta["other-id"] = k8sContainerMeta{PodName: "other-pod"}

	c := NewContainer("my-container")
	r.enrichContainerWithK8sMetadata(c)

	if c.PodName != "" {
		t.Errorf("no-match should leave PodName empty, got %q", c.PodName)
	}
}

func TestEnrichContainerWithK8sMetadata_DoesNotOverwrite(t *testing.T) {
	r := newBareRegistry(Config{})
	r.k8sContainerMeta["cid"] = k8sContainerMeta{
		PodName:   "new-pod",
		Namespace: "new-ns",
	}

	c := NewContainer("cid")
	c.PodName = "original-pod"
	c.Namespace = "original-ns"
	r.enrichContainerWithK8sMetadata(c)

	// 已有值不应被覆盖
	if c.PodName != "original-pod" {
		t.Errorf("PodName overwritten, got %q", c.PodName)
	}
	if c.Namespace != "original-ns" {
		t.Errorf("Namespace overwritten, got %q", c.Namespace)
	}
}

func TestEnrichContainerWithK8sMetadata_NilContainer(t *testing.T) {
	r := newBareRegistry(Config{})
	// 不应 panic
	r.enrichContainerWithK8sMetadata(nil)
}

func TestEnrichContainerWithK8sMetadata_EmptyMeta(t *testing.T) {
	r := newBareRegistry(Config{})
	// k8sContainerMeta 为空时跳过
	c := NewContainer("cid")
	r.enrichContainerWithK8sMetadata(c)
	if c.PodName != "" {
		t.Error("empty meta should not affect container")
	}
}

// ─── applyInspectMetadata ─────────────────────────────────────

func TestApplyInspectMetadata_BasicFields(t *testing.T) {
	r := newBareRegistry(Config{})
	c := NewContainer("c1")

	base := &container.ContainerJSONBase{Name: "/mycontainer"}
	cfg := &container.Config{
		Image:  "nginx:1.25",
		Labels: map[string]string{"env": "prod"},
	}
	ins := container.InspectResponse{
		ContainerJSONBase: base,
		Config:            cfg,
	}

	r.applyInspectMetadata(c, ins)

	if c.Name != "mycontainer" {
		t.Errorf("Name: want mycontainer, got %s", c.Name)
	}
	if c.Image != "nginx:1.25" {
		t.Errorf("Image: want nginx:1.25, got %s", c.Image)
	}
	if c.Labels["env"] != "prod" {
		t.Errorf("Label env: want prod, got %s", c.Labels["env"])
	}
}

func TestApplyInspectMetadata_K8sLabels(t *testing.T) {
	r := newBareRegistry(Config{})
	c := NewContainer("c2")

	ins := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Name: "/kube-pod"},
		Config: &container.Config{
			Labels: map[string]string{
				"io.kubernetes.pod.name":      "my-pod",
				"io.kubernetes.pod.namespace": "kube-system",
			},
		},
	}

	r.applyInspectMetadata(c, ins)

	if c.PodName != "my-pod" {
		t.Errorf("PodName: want my-pod, got %s", c.PodName)
	}
	if c.Namespace != "kube-system" {
		t.Errorf("Namespace: want kube-system, got %s", c.Namespace)
	}
}

func TestApplyInspectMetadata_NilContainer(t *testing.T) {
	r := newBareRegistry(Config{})
	// 不应 panic
	r.applyInspectMetadata(nil, container.InspectResponse{})
}

func TestApplyInspectMetadata_NilConfig(t *testing.T) {
	r := newBareRegistry(Config{})
	c := NewContainer("c3")
	ins := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Name: "/no-config"},
	}
	// Config 为 nil，不应 panic
	r.applyInspectMetadata(c, ins)
	if c.Name != "no-config" {
		t.Errorf("Name should still be set, got %q", c.Name)
	}
}

// ─── extractContainerID ───────────────────────────────────────

func TestExtractContainerID_Empty(t *testing.T) {
	if id := extractContainerID(""); id != "" {
		t.Errorf("empty input should return empty, got %q", id)
	}
}

func TestExtractContainerID_Full64HexID(t *testing.T) {
	fullID := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	line := "12:blkio:/docker/" + fullID
	if id := extractContainerID(line); id != fullID {
		t.Errorf("expected full 64-char hex ID, got %q", id)
	}
}

func TestExtractContainerID_DockerPrefix(t *testing.T) {
	id := "abcdef1234567890abcdef12"
	line := "0::/system.slice/docker-" + id + ".scope"
	got := extractContainerID(line)
	if got != id {
		t.Errorf("docker prefix: got %q, want %q", got, id)
	}
}

func TestExtractContainerID_ContainerdPrefix(t *testing.T) {
	id := "abc123def456abc123def456"
	line := "0::/system.slice/cri-containerd-" + id + ".scope"
	got := extractContainerID(line)
	if got != id {
		t.Errorf("containerd prefix: got %q, want %q", got, id)
	}
}

func TestExtractContainerID_CrioPrefix(t *testing.T) {
	id := "fedcba9876543210fedcba98"
	line := "0::/crio-" + id + ".scope"
	got := extractContainerID(line)
	if got != id {
		t.Errorf("crio prefix: got %q, want %q", got, id)
	}
}

func TestExtractContainerID_ShortTokenIgnored(t *testing.T) {
	// 少于 12 位的 token 不应被当成 container ID
	line := "11:devices:/abc123"
	got := extractContainerID(line)
	if got != "" {
		t.Errorf("short token should be ignored, got %q", got)
	}
}

func TestExtractContainerID_NonHexToken(t *testing.T) {
	line := "0::/system.slice/myservice.service"
	got := extractContainerID(line)
	if got != "" {
		t.Errorf("non-hex token should not be returned, got %q", got)
	}
}

// ─── normalizeContainerID ─────────────────────────────────────

func TestNormalizeContainerID_Empty(t *testing.T) {
	if id := normalizeContainerID(""); id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestNormalizeContainerID_WithScheme(t *testing.T) {
	raw := "docker://abcdef1234"
	if id := normalizeContainerID(raw); id != "abcdef1234" {
		t.Errorf("expected abcdef1234, got %q", id)
	}
}

func TestNormalizeContainerID_ContainerdScheme(t *testing.T) {
	raw := "containerd://my-container-id"
	if id := normalizeContainerID(raw); id != "my-container-id" {
		t.Errorf("expected my-container-id, got %q", id)
	}
}

func TestNormalizeContainerID_ScopeSuffix(t *testing.T) {
	raw := "abc123.scope"
	if id := normalizeContainerID(raw); id != "abc123" {
		t.Errorf("expected abc123, got %q", id)
	}
}

func TestNormalizeContainerID_PlainID(t *testing.T) {
	raw := "  plain-id  "
	if id := normalizeContainerID(raw); id != "plain-id" {
		t.Errorf("expected plain-id, got %q", id)
	}
}

// ─── indexPodContainerMeta ────────────────────────────────────

func TestIndexPodContainerMeta_NilPod(t *testing.T) {
	m := make(map[string]k8sContainerMeta)
	// 不应 panic
	indexPodContainerMeta(m, nil)
	if len(m) != 0 {
		t.Error("nil pod should produce no entries")
	}
}

func TestIndexPodContainerMeta_ContainerStatuses(t *testing.T) {
	m := make(map[string]k8sContainerMeta)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: "prod",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "docker://abc123"},
			},
		},
	}

	indexPodContainerMeta(m, pod)

	meta, ok := m["abc123"]
	if !ok {
		t.Fatal("expected meta for container abc123")
	}
	if meta.PodName != "web-pod" {
		t.Errorf("PodName: want web-pod, got %s", meta.PodName)
	}
	if meta.Namespace != "prod" {
		t.Errorf("Namespace: want prod, got %s", meta.Namespace)
	}
}

func TestIndexPodContainerMeta_InitAndEphemeralStatuses(t *testing.T) {
	m := make(map[string]k8sContainerMeta)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "docker://init111"},
			},
			EphemeralContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "containerd://eph222"},
			},
		},
	}

	indexPodContainerMeta(m, pod)

	if _, ok := m["init111"]; !ok {
		t.Error("init container should be indexed")
	}
	if _, ok := m["eph222"]; !ok {
		t.Error("ephemeral container should be indexed")
	}
}

func TestIndexPodContainerMeta_EmptyContainerID(t *testing.T) {
	m := make(map[string]k8sContainerMeta)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: ""}, // 空 ID 应被忽略
			},
		},
	}

	indexPodContainerMeta(m, pod)
	if len(m) != 0 {
		t.Error("empty ContainerID should not be indexed")
	}
}

// ─── Registry.Close 幂等性 ────────────────────────────────────

func TestRegistryClose_Idempotent(t *testing.T) {
	r := newBareRegistry(Config{})
	// 多次 Close 不应 panic（stopChan 已关闭）
	r.Close()
	r.Close()
}

// ─── 并发：getOrCreateContainer + GetContainers ───────────────

func TestGetOrCreate_Concurrent(t *testing.T) {
	// getOrCreateContainer 需要调用方持 r.mu 写锁，测试通过 r.mu.Lock() 保护
	r := newBareRegistry(Config{MaxContainers: 50})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		id := string(rune('A' + i%26))
		wg.Add(1)
		go func(cid string) {
			defer wg.Done()
			r.mu.Lock()
			r.getOrCreateContainer(cid)
			r.mu.Unlock()
		}(id)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.GetContainers()
		}()
	}
	wg.Wait()

	cs := r.GetContainers()
	if len(cs) > 26 {
		t.Errorf("expected at most 26 unique containers, got %d", len(cs))
	}
}

// ─── gcContainers 与 getOrCreateContainer 并发 ───────────────

func TestGcContainers_Concurrent(t *testing.T) {
	r := newBareRegistry(Config{})

	// 预置一批过期容器（gcContainers 自带锁，测试不需要外部持锁）
	old := time.Now().Add(-(containerTimeout + time.Hour))
	r.mu.Lock()
	for i := 0; i < 20; i++ {
		id := string(rune('a' + i))
		c := NewContainer(id)
		c.LastActivity = old
		r.containers[id] = c
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.gcContainers() // 内部自带 r.mu.Lock()，此处不重复加锁
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.GetContainers()
		}()
	}
	wg.Wait()

	// GC 之后所有过期容器应被清除
	cs := r.GetContainers()
	if len(cs) != 0 {
		t.Errorf("expected 0 containers after GC, got %d", len(cs))
	}
}

// ─── GetTrackedPIDs ────────────────────────────────────────────

func TestGetTrackedPIDs_Empty(t *testing.T) {
	r := newBareRegistry(Config{})
	pids := r.GetTrackedPIDs()
	if len(pids) != 0 {
		t.Errorf("expected 0 PIDs, got %d", len(pids))
	}
}

func TestGetTrackedPIDs_FromPidCache(t *testing.T) {
	r := newBareRegistry(Config{})
	// pidCache 包含容器内进程
	r.pidCache[100] = "container-abc"
	r.pidCache[200] = "container-abc"
	r.pidCache[300] = "container-xyz"

	pids := r.GetTrackedPIDs()
	for _, pid := range []uint32{100, 200, 300} {
		if _, ok := pids[pid]; !ok {
			t.Errorf("expected PID %d to be in tracked set", pid)
		}
	}
	if len(pids) != 3 {
		t.Errorf("expected 3 tracked PIDs, got %d", len(pids))
	}
}

func TestGetTrackedPIDs_FromContainerPID(t *testing.T) {
	r := newBareRegistry(Config{})
	// proc_<pid> 兑退格式：c.PID 非零
	c := NewContainer("proc_12345")
	c.PID = 12345
	r.containers["proc_12345"] = c

	pids := r.GetTrackedPIDs()
	if _, ok := pids[12345]; !ok {
		t.Error("expected PID 12345 from container.PID to be in tracked set")
	}
}

func TestGetTrackedPIDs_ProcCommContainerExcluded(t *testing.T) {
	r := newBareRegistry(Config{})
	// proc_<comm> 格式：c.PID == 0，不应加入（需走 30s 全量扫描）
	c := NewContainer("proc_nginx")
	c.PID = 0
	r.containers["proc_nginx"] = c

	pids := r.GetTrackedPIDs()
	if _, ok := pids[0]; ok {
		t.Error("PID 0 should not be in tracked set")
	}
	if len(pids) != 0 {
		t.Errorf("expected 0 tracked PIDs for proc_<comm> container, got %d", len(pids))
	}
}

func TestGetTrackedPIDs_MixedSources(t *testing.T) {
	r := newBareRegistry(Config{})
	// pidCache 来源
	r.pidCache[10] = "container-a"
	r.pidCache[20] = "container-a"
	// container.PID 来源（proc_<pid> 兑退）
	c := NewContainer("proc_99")
	c.PID = 99
	r.containers["proc_99"] = c
	// proc_<comm> 不贡献
	c2 := NewContainer("proc_mysql")
	c2.PID = 0
	r.containers["proc_mysql"] = c2

	pids := r.GetTrackedPIDs()
	for _, pid := range []uint32{10, 20, 99} {
		if _, ok := pids[pid]; !ok {
			t.Errorf("expected PID %d to be in tracked set", pid)
		}
	}
	if _, ok := pids[0]; ok {
		t.Error("PID 0 should not be present")
	}
	if len(pids) != 3 {
		t.Errorf("expected 3 tracked PIDs, got %d", len(pids))
	}
}

func TestGetTrackedPIDs_ConcurrentSafe(t *testing.T) {
	r := newBareRegistry(Config{})
	r.pidCache[1] = "c1"
	r.pidCache[2] = "c1"

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.GetTrackedPIDs()
		}()
	}
	wg.Wait()
}
