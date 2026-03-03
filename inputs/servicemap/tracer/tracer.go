package tracer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/l7"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/rlimit"
	gopsnet "github.com/shirou/gopsutil/v3/net"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// EventType eBPF事件类型
type EventType uint32

const (
	EventTypeProcessStart    EventType = 1
	EventTypeProcessExit     EventType = 2
	EventTypeConnectionOpen  EventType = 3
	EventTypeConnectionClose EventType = 4
	EventTypeListenOpen      EventType = 6
	EventTypeListenClose     EventType = 7
	EventTypeTCPRetransmit   EventType = 9
	EventTypeL7Request       EventType = 10 // P2-9: L7 协议事件
)

func (t EventType) String() string {
	switch t {
	case EventTypeProcessStart:
		return "ProcessStart"
	case EventTypeProcessExit:
		return "ProcessExit"
	case EventTypeConnectionOpen:
		return "ConnectionOpen"
	case EventTypeConnectionClose:
		return "ConnectionClose"
	case EventTypeListenOpen:
		return "ListenOpen"
	case EventTypeListenClose:
		return "ListenClose"
	case EventTypeTCPRetransmit:
		return "TCPRetransmit"
	case EventTypeL7Request:
		return "L7Request"
	default:
		return fmt.Sprintf("Unknown(%d)", t)
	}
}

// Event eBPF事件
type Event struct {
	Type      EventType
	Timestamp uint64
	Pid       uint32
	Fd        uint64
	SrcAddr   string
	DstAddr   string
	SrcPort   uint16
	DstPort   uint16

	// P2-9: L7 请求数据（仅当 Type == EventTypeL7Request 时有效）
	L7Request *l7.RequestData
}

// ConnectionID 连接标识
type ConnectionID struct {
	FD  uint64
	PID uint32
}

// Connection 连接统计
type Connection struct {
	Timestamp     uint64
	LastSeen      time.Time // P0-2: 用于过期清理
	BytesSent     uint64
	BytesReceived uint64
}

// ListenKey 监听端口标识 (P0-4)
type ListenKey struct {
	Port uint16
	Addr string
}

const (
	// P0-2: 连接过期超时
	connectionGCInterval = 30 * time.Second
	connectionTimeout    = 5 * time.Minute
)

// Tracer eBPF追踪器
type Tracer struct {
	ctx              context.Context // P1-8: 生命周期 context
	hostNetNs        netns.NsHandle
	selfNetNs        netns.NsHandle
	disableL7Tracing bool
	maxActiveConns   int // P1-6: 活跃连接数上限，0 = 不限制

	collection *ebpf.Collection
	readers    map[string]*perf.Reader
	uprobes    map[string]*ebpf.Program
	links      []link.Link

	// P0-3: 活跃连接，由 activeConnMu 保护
	activeConnMu sync.RWMutex
	activeConns  map[ConnectionID]Connection
	lastSnapshot map[ConnectionID]Event

	// P0-4: 监听端口追踪，由 listenMu 保护
	listenMu    sync.RWMutex
	listenPorts map[ListenKey]struct{}

	// 轮询模式性能优化字段（仅在 startFallbackMode 路径下使用）
	pidFilter      func() map[uint32]struct{} // P1: PID 作用域过滤回调，由 Registry 初始化时注入
	pollBuf        map[ConnectionID]Event     // P0-D: 双缓冲复用，避免每轮 poll 重新分配 map
	lastGlobalScan time.Time                  // P1: 上次全量扫描时间戳，控制强制全量扫描频率

	started  bool
	stopOnce sync.Once
	wg       sync.WaitGroup // P1-8: 追踪后台 goroutine

	eventChan chan Event
	closeChan chan struct{}
}

// NewTracer 创建新的Tracer
func NewTracer(ctx context.Context, hostNetNs, selfNetNs netns.NsHandle, disableL7Tracing bool, maxActiveConns int) (*Tracer, error) {
	if disableL7Tracing {
		log.Println("I! servicemap: L7 tracing is disabled")
	}

	t := &Tracer{
		ctx:              ctx,
		hostNetNs:        hostNetNs,
		selfNetNs:        selfNetNs,
		disableL7Tracing: disableL7Tracing,
		maxActiveConns:   maxActiveConns,
		readers:          make(map[string]*perf.Reader),
		uprobes:          make(map[string]*ebpf.Program),
		activeConns:      make(map[ConnectionID]Connection),
		lastSnapshot:     make(map[ConnectionID]Event),
		pollBuf:          make(map[ConnectionID]Event), // P0-D: 双缓冲初始化
		listenPorts:      make(map[ListenKey]struct{}),
		eventChan:        make(chan Event, 10000),
		closeChan:        make(chan struct{}),
	}

	// P1-8: 监听 context 取消，自动触发关闭
	go func() {
		select {
		case <-ctx.Done():
			t.stopOnce.Do(func() { close(t.closeChan) })
		case <-t.closeChan:
		}
	}()

	return t, nil
}

// launchBackground 启动后台 goroutine 并追踪生命周期 (P1-8)
func (t *Tracer) launchBackground(fn func()) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		fn()
	}()
}

// SetPIDFilter 注入 PID 作用域过滤回调（由 Registry 在初始化时调用）。
// 轮询模式下，每 30s 做一次全量扫描发现新进程，其余周期仅处理已知 PID 的连接。
func (t *Tracer) SetPIDFilter(fn func() map[uint32]struct{}) {
	t.pidFilter = fn
}

// adaptiveInterval 根据上次快照连接数动态返回下次轮询间隔。
// 连接数越多，轮询间隔越长，避免高连接密度下 procfs 扫描开销持续积累。
func adaptiveInterval(connCount int) time.Duration {
	switch {
	case connCount < 1_000:
		return 2 * time.Second
	case connCount < 5_000:
		return 5 * time.Second
	default:
		return 10 * time.Second
	}
}

// Start 启动eBPF程序
func (t *Tracer) Start() error {
	if t.started {
		return nil
	}

	if runtime.GOOS != "linux" {
		log.Printf("I! servicemap: eBPF is unsupported on %s, fallback to polling tracer", runtime.GOOS)
		t.startFallbackMode()
		return nil
	}

	log.Println("I! servicemap: loading eBPF programs...")

	// 检查架构支持
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}

	// 检查tracefs
	var traceFsPath string
	for _, p := range []string{"/sys/kernel/debug/tracing", "/sys/kernel/tracing"} {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err == nil {
			traceFsPath = p
			break
		}
	}
	if traceFsPath == "" {
		log.Println("W! servicemap: tracefs unavailable, fallback to polling tracer")
		t.startFallbackMode()
		return nil
	}

	if err := checkKernelVersion(); err != nil {
		log.Printf("W! servicemap: kernel check failed, fallback to polling tracer: %v", err)
		t.startFallbackMode()
		return nil
	}

	if err := t.loadEBPF(); err != nil {
		log.Printf("W! servicemap: load eBPF failed, fallback to polling tracer: %v", err)
		t.startFallbackMode()
		return nil
	}

	t.launchBackground(t.startConnectionGC)
	log.Println("I! servicemap: eBPF tracer started")
	t.started = true
	return nil
}

// startFallbackMode 启动轮询回退模式
func (t *Tracer) startFallbackMode() {
	t.launchBackground(t.startPollingTracer)
	t.launchBackground(t.startConnectionGC)
	t.started = true
}

// loadEBPF 加载eBPF程序（待实现）
func (t *Tracer) loadEBPF() error {
	// 移除 MEMLOCK 限制：
	// - 内核 >= 5.11: 使用 cgroup 记账，无需修改 rlimit
	// - 内核 <  5.11: 设置 RLIMIT_MEMLOCK = RLIM_INFINITY
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Printf("W! servicemap: remove memlock rlimit failed (need CAP_SYS_RESOURCE?): %v", err)
	}

	program, err := getEmbeddedEBPFProgram(runtime.GOARCH)
	if err != nil {
		return err
	}

	progBytes, err := decompressEBPFProgram(program)
	if err != nil {
		return fmt.Errorf("decompress ebpf program failed: %w", err)
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(progBytes))
	if err != nil {
		return fmt.Errorf("load ebpf collection spec failed: %w", err)
	}

	collection, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("create ebpf collection failed: %w", err)
	}

	t.collection = collection

	if err := t.attachProbes(); err != nil {
		return err
	}

	if err := t.initPerfReaders(); err != nil {
		return err
	}

	return nil
}

func (t *Tracer) attachProbes() error {
	if t.collection == nil {
		return fmt.Errorf("collection is nil")
	}

	attached := 0
	for name, prog := range t.collection.Programs {
		switch name {
		case "trace_inet_sock_set_state":
			l, err := link.Tracepoint("sock", "inet_sock_set_state", prog, nil)
			if err != nil {
				return fmt.Errorf("attach %s failed: %w", name, err)
			}
			t.links = append(t.links, l)
			attached++
		case "trace_sys_enter_connect":
			l, err := link.Tracepoint("syscalls", "sys_enter_connect", prog, nil)
			if err != nil {
				return fmt.Errorf("attach %s failed: %w", name, err)
			}
			t.links = append(t.links, l)
			attached++
		case "trace_sys_exit_connect":
			l, err := link.Tracepoint("syscalls", "sys_exit_connect", prog, nil)
			if err != nil {
				return fmt.Errorf("attach %s failed: %w", name, err)
			}
			t.links = append(t.links, l)
			attached++
		}
	}

	if attached == 0 {
		return fmt.Errorf("no known tracepoint program found in ebpf collection")
	}

	return nil
}

func (t *Tracer) initPerfReaders() error {
	if t.collection == nil {
		return fmt.Errorf("collection is nil")
	}

	// 主事件 perf buffer
	m, ok := t.collection.Maps["events"]
	if !ok {
		return nil
	}

	r, err := perf.NewReader(m, os.Getpagesize()*16)
	if err != nil {
		return fmt.Errorf("create perf reader failed: %w", err)
	}

	t.readers["events"] = r
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.runEventReader(r)
	}()

	// P2-9: L7 事件 perf buffer
	if !t.disableL7Tracing {
		if l7m, ok := t.collection.Maps["l7_events"]; ok {
			l7r, err := perf.NewReader(l7m, os.Getpagesize()*16)
			if err != nil {
				log.Printf("W! servicemap: create L7 perf reader failed: %v", err)
			} else {
				t.readers["l7_events"] = l7r
				t.wg.Add(1)
				go func() {
					defer t.wg.Done()
					t.runL7EventReader(l7r)
				}()
				log.Println("I! servicemap: L7 perf reader started")
			}
		}
	}

	return nil
}

func (t *Tracer) runEventReader(r *perf.Reader) {
	for {
		rec, err := r.Read()
		if err != nil {
			select {
			case <-t.closeChan:
				return
			default:
				log.Printf("W! servicemap: read perf event failed: %v", err)
				continue
			}
		}

		if rec.LostSamples > 0 {
			log.Printf("W! servicemap: perf events lost samples=%d", rec.LostSamples)
			continue
		}

		// 解析原始事件数据
		event, err := parseRawEvent(rec.RawSample)
		if err != nil {
			log.Printf("W! servicemap: parse event failed: %v", err)
			continue
		}

		// P0-4: 处理 ListenOpen/ListenClose 事件
		t.handleListenEvent(event)

		// 发送到事件通道
		select {
		case t.eventChan <- *event:
		case <-t.closeChan:
			return
		default:
			// 通道满，丢弃事件
		}
	}
}

// Events 返回事件通道
func (t *Tracer) Events() <-chan Event {
	return t.eventChan
}

// runL7EventReader 读取 L7 perf buffer 中的协议事件 (P2-9)
func (t *Tracer) runL7EventReader(r *perf.Reader) {
	for {
		rec, err := r.Read()
		if err != nil {
			select {
			case <-t.closeChan:
				return
			default:
				log.Printf("W! servicemap: read L7 perf event failed: %v", err)
				continue
			}
		}

		if rec.LostSamples > 0 {
			log.Printf("W! servicemap: L7 perf events lost samples=%d", rec.LostSamples)
			continue
		}

		event, err := parseRawL7Event(rec.RawSample)
		if err != nil {
			log.Printf("W! servicemap: parse L7 event failed: %v", err)
			continue
		}

		select {
		case t.eventChan <- *event:
		case <-t.closeChan:
			return
		default:
			// 通道满，丢弃事件
		}
	}
}

// GetActiveConnections 获取活跃连接迭代器
func (t *Tracer) GetActiveConnections() *ebpf.MapIterator {
	if t.collection == nil || t.collection.Maps["active_connections"] == nil {
		return nil
	}
	return t.collection.Maps["active_connections"].Iterate()
}

// ForEachActiveConnection 遍历活跃连接（支持 eBPF 与回退模式）
func (t *Tracer) ForEachActiveConnection(fn func(connID ConnectionID, conn Connection)) {
	t.activeConnMu.RLock()
	defer t.activeConnMu.RUnlock()

	for id, c := range t.activeConns {
		fn(id, c)
	}
}

// ActiveConnectionCount 返回当前活跃连接数
func (t *Tracer) ActiveConnectionCount() int {
	t.activeConnMu.RLock()
	defer t.activeConnMu.RUnlock()
	return len(t.activeConns)
}

// IsListening 检查指定端口是否在监听 (P0-4)
func (t *Tracer) IsListening(port uint16) bool {
	t.listenMu.RLock()
	defer t.listenMu.RUnlock()

	for key := range t.listenPorts {
		if key.Port == port {
			return true
		}
	}
	return false
}

// GetListenPorts 返回所有监听端口的快照 (P0-4)
func (t *Tracer) GetListenPorts() map[uint16]struct{} {
	t.listenMu.RLock()
	defer t.listenMu.RUnlock()

	ports := make(map[uint16]struct{}, len(t.listenPorts))
	for key := range t.listenPorts {
		ports[key.Port] = struct{}{}
	}
	return ports
}

// handleListenEvent 处理 ListenOpen / ListenClose 事件 (P0-4)
func (t *Tracer) handleListenEvent(event *Event) {
	if event == nil {
		return
	}

	key := ListenKey{Port: event.DstPort, Addr: event.DstAddr}

	switch event.Type {
	case EventTypeListenOpen:
		t.listenMu.Lock()
		t.listenPorts[key] = struct{}{}
		t.listenMu.Unlock()
	case EventTypeListenClose:
		t.listenMu.Lock()
		delete(t.listenPorts, key)
		t.listenMu.Unlock()
	}
}

// Close 关闭Tracer (P1-8: 等待所有后台 goroutine 退出后再释放资源)
func (t *Tracer) Close() {
	t.stopOnce.Do(func() {
		close(t.closeChan)
	})
	// P1-8: 等待所有后台 goroutine 退出
	t.wg.Wait()

	for _, p := range t.uprobes {
		_ = p.Close()
	}

	for _, l := range t.links {
		_ = l.Close()
	}

	for _, r := range t.readers {
		_ = r.Close()
	}

	if t.collection != nil {
		t.collection.Close()
	}

	if t.selfNetNs >= 0 {
		_ = t.selfNetNs.Close()
	}
	if t.hostNetNs >= 0 && t.hostNetNs != t.selfNetNs {
		_ = t.hostNetNs.Close()
	}

	// P0-1: 清空内部状态，确保无残留
	t.activeConnMu.Lock()
	t.activeConns = nil
	t.lastSnapshot = nil
	t.activeConnMu.Unlock()

	t.listenMu.Lock()
	t.listenPorts = nil
	t.listenMu.Unlock()

	log.Println("I! servicemap: tracer closed")
}

func (t *Tracer) startPollingTracer() {
	log.Println("I! servicemap: polling tracer started")

	// 立即执行一次以便快速初始化，获取初始连接数用于首次间隔计算
	lastCount := t.pollConnections()

	for {
		// P0: 自适应间隔——连接数越多，轮询越稀疏，降低高连接密度下的 procfs 扫描开销
		timer := time.NewTimer(adaptiveInterval(lastCount))
		select {
		case <-t.closeChan:
			timer.Stop()
			return
		case <-timer.C:
			lastCount = t.pollConnections()
		}
	}
}

// startConnectionGC 定期清理过期连接，防止内存泄漏 (P0-2)
func (t *Tracer) startConnectionGC() {
	ticker := time.NewTicker(connectionGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.closeChan:
			return
		case <-ticker.C:
			t.gcConnections()
		}
	}
}

// gcConnections 清理已过期的活跃连接 (P0-2)
func (t *Tracer) gcConnections() {
	t.activeConnMu.Lock()
	defer t.activeConnMu.Unlock()

	if t.activeConns == nil {
		return
	}

	now := time.Now()
	expired := 0
	for id, conn := range t.activeConns {
		if !conn.LastSeen.IsZero() && now.Sub(conn.LastSeen) > connectionTimeout {
			delete(t.activeConns, id)
			expired++
		}
	}

	if expired > 0 {
		log.Printf("D! servicemap: GC cleaned %d expired connections, %d remaining", expired, len(t.activeConns))
	}
}

// pollConnections 扫描当前 TCP 连接并与上次快照做差量比较，发出 Open/Close 事件。
// 返回本次有效连接数，供调用方计算下次自适应轮询间隔。
//
// 数据采集分两层：
//  1. 优先使用 NETLINK_INET_DIAG（Linux only，内核态过滤，性能高 10x）
//  2. 失败则 fallback 到 gopsutil（跨平台，读 /proc/net/tcp）
func (t *Tracer) pollConnections() int {
	now := time.Now()
	nowNano := uint64(now.UnixNano())

	// P0-D: 复用 pollBuf（与 lastSnapshot 双缓冲轮换），避免每次轮询重新分配 map
	current := t.pollBuf
	for k := range current {
		delete(current, k)
	}

	discoveredListens := make(map[ListenKey]struct{})

	// ─── 数据采集层：netlink 优先，gopsutil 兜底 ───────────
	diagConns, netlinkErr := netlinkConnections()
	if netlinkErr == nil {
		t.collectFromNetlink(diagConns, current, discoveredListens, nowNano)
	} else {
		log.Printf("D! servicemap: netlink unavailable (%v), fallback to gopsutil", netlinkErr)
		if err := t.collectFromGopsutil(current, discoveredListens, nowNano); err != nil {
			log.Printf("W! servicemap: polling tcp connections failed: %v", err)
			return 0
		}
	}

	log.Printf("D! servicemap: polled %d tcp connections", len(current))

	// ─── 差量比较层（与采集方式无关）─────────────────────────

	// P0-4: 更新监听端口列表
	t.listenMu.Lock()
	t.listenPorts = discoveredListens
	t.listenMu.Unlock()

	t.activeConnMu.Lock()
	defer t.activeConnMu.Unlock()

	if t.activeConns == nil {
		return 0
	}

	for id, e := range current {
		if _, ok := t.lastSnapshot[id]; !ok {
			t.emitEventLocked(e)
		}

		if existing, ok := t.activeConns[id]; ok {
			// P0-2: 已有连接，更新 LastSeen
			t.activeConns[id] = Connection{
				Timestamp:     existing.Timestamp,
				LastSeen:      now,
				BytesSent:     existing.BytesSent,
				BytesReceived: existing.BytesReceived,
			}
		} else {
			// P1-6: 检查连接数上限
			if t.maxActiveConns > 0 && len(t.activeConns) >= t.maxActiveConns {
				continue
			}
			t.activeConns[id] = Connection{
				Timestamp: e.Timestamp,
				LastSeen:  now,
			}
		}
	}

	for id, old := range t.lastSnapshot {
		if _, ok := current[id]; ok {
			continue
		}

		old.Type = EventTypeConnectionClose
		old.Timestamp = nowNano
		t.emitEventLocked(old)
		delete(t.activeConns, id)
	}

	// P0-D: 双缓冲轮换——旧 lastSnapshot 变为下次的 pollBuf，零分配
	t.pollBuf = t.lastSnapshot
	t.lastSnapshot = current

	return len(current)
}

// collectFromNetlink 使用 NETLINK_INET_DIAG 结果填充 current 和 discoveredListens。
// netlink 已在内核态按 state bitmap 过滤，无需用户态再过滤状态。
func (t *Tracer) collectFromNetlink(
	diagConns []DiagConnection,
	current map[ConnectionID]Event,
	discoveredListens map[ListenKey]struct{},
	nowNano uint64,
) {
	for i := range diagConns {
		dc := &diagConns[i]

		if dc.IsListen() {
			key := ListenKey{
				Port: dc.SrcPort,
				Addr: endpoint(dc.SrcIP, uint32(dc.SrcPort)),
			}
			discoveredListens[key] = struct{}{}
			continue
		}

		if !dc.IsTracked() {
			continue
		}

		if dc.DstIP == "" || dc.DstPort == 0 {
			continue
		}

		// netlink 不返回 PID/FD，使用 inode 作为稳定 FD 替代
		fd := uint64(dc.Inode)
		id := ConnectionID{FD: fd, PID: 0}
		e := Event{
			Type:      EventTypeConnectionOpen,
			Timestamp: nowNano,
			Pid:       0, // netlink 不提供 PID（需额外 INET_DIAG_INFO 查询，此处暂不实现）
			Fd:        fd,
			SrcAddr:   endpoint(dc.SrcIP, uint32(dc.SrcPort)),
			DstAddr:   endpoint(dc.DstIP, uint32(dc.DstPort)),
			SrcPort:   dc.SrcPort,
			DstPort:   dc.DstPort,
		}
		current[id] = e
	}
}

// collectFromGopsutil 使用 gopsutil 读取 /proc/net/tcp 填充 current 和 discoveredListens。
// 包含 P1 PID 过滤逻辑（每 30s 全量扫描，其余时间按已知 PID 过滤）。
func (t *Tracer) collectFromGopsutil(
	current map[ConnectionID]Event,
	discoveredListens map[ListenKey]struct{},
	nowNano uint64,
) error {
	conns, err := gopsnet.Connections("tcp")
	if err != nil {
		return err
	}

	// P1: PID 作用域过滤
	if t.pidFilter != nil {
		pids := t.pidFilter()
		if len(pids) > 0 && time.Since(t.lastGlobalScan) < 30*time.Second {
			n := 0
			for _, c := range conns {
				if _, ok := pids[uint32(c.Pid)]; ok || strings.ToUpper(c.Status) == "LISTEN" {
					conns[n] = c
					n++
				}
			}
			log.Printf("D! servicemap: pid-filtered: %d → %d connections", len(conns), n)
			conns = conns[:n]
		} else {
			t.lastGlobalScan = time.Now()
			log.Printf("D! servicemap: global scan: %d connections", len(conns))
		}
	}

	for _, c := range conns {
		if strings.ToUpper(c.Status) == "LISTEN" {
			key := ListenKey{
				Port: uint16(c.Laddr.Port),
				Addr: endpoint(c.Laddr.IP, c.Laddr.Port),
			}
			discoveredListens[key] = struct{}{}
			continue
		}

		if !isTrackedTCPConnection(c) {
			continue
		}

		fd := connectionFD(c)
		id := ConnectionID{FD: fd, PID: uint32(c.Pid)}
		e := Event{
			Type:      EventTypeConnectionOpen,
			Timestamp: nowNano,
			Pid:       uint32(c.Pid),
			Fd:        fd,
			SrcAddr:   endpoint(c.Laddr.IP, c.Laddr.Port),
			DstAddr:   endpoint(c.Raddr.IP, c.Raddr.Port),
			SrcPort:   uint16(c.Laddr.Port),
			DstPort:   uint16(c.Raddr.Port),
		}
		current[id] = e
	}

	return nil
}

func (t *Tracer) emitEventLocked(e Event) {
	select {
	case t.eventChan <- e:
	default:
		// 通道满，静默丢弃避免日志洪泛
	}
}

func isTrackedTCPConnection(c gopsnet.ConnectionStat) bool {
	if c.Status == "" {
		return false
	}

	status := strings.ToUpper(c.Status)
	if status != "ESTABLISHED" && status != "SYN_SENT" && status != "SYN_RECV" {
		return false
	}

	if c.Raddr.IP == "" || c.Raddr.Port == 0 {
		return false
	}

	return true
}

func connectionFD(c gopsnet.ConnectionStat) uint64 {
	if c.Fd > 0 {
		return uint64(c.Fd)
	}

	// 某些平台（如 macOS）可能无法拿到真实 fd，使用连接四元组生成稳定 ID。
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("%d|%s|%d|%s|%d", c.Pid, c.Laddr.IP, c.Laddr.Port, c.Raddr.IP, c.Raddr.Port)))
	return h.Sum64()
}

func endpoint(ip string, port uint32) string {
	if ip == "" {
		return ""
	}

	return net.JoinHostPort(ip, strconv.FormatUint(uint64(port), 10))
}

// 辅助函数

// decompressEBPFProgram 解压eBPF程序（用于后续加载预编译的字节码）
func decompressEBPFProgram(compressed []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("invalid program encoding: %w", err)
	}
	defer reader.Close()

	prog, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to ungzip program: %w", err)
	}

	return prog, nil
}

// checkKernelVersion 检查内核版本（简化版）
func checkKernelVersion() error {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		return err
	}

	release := string(bytes.Split(uname.Release[:], []byte{0})[0])
	log.Printf("I! servicemap: kernel version: %s", release)

	// 简单检查：至少需要 4.16
	parts := strings.Split(release, ".")
	if len(parts) < 2 {
		return errors.New("cannot parse kernel version")
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("parse kernel major version failed: %w", err)
	}

	minorStr := parts[1]
	if idx := strings.IndexAny(minorStr, "-"); idx > 0 {
		minorStr = minorStr[:idx]
	}

	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return fmt.Errorf("parse kernel minor version failed: %w", err)
	}

	if major < 4 || (major == 4 && minor < 16) {
		return fmt.Errorf("kernel version %s is too old, require >= 4.16", release)
	}

	// 这里应该做更详细的版本检查
	log.Printf("I! servicemap: kernel version check passed")

	return nil
}
