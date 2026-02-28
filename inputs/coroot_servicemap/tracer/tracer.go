package tracer

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
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
}

// ConnectionID 连接标识
type ConnectionID struct {
	FD  uint64
	PID uint32
}

// Connection 连接统计
type Connection struct {
	Timestamp     uint64
	BytesSent     uint64
	BytesReceived uint64
}

// Tracer eBPF追踪器
type Tracer struct {
	hostNetNs        netns.NsHandle
	selfNetNs        netns.NsHandle
	disableL7Tracing bool

	collection *ebpf.Collection
	readers    map[string]*perf.Reader
	uprobes    map[string]*ebpf.Program
	links      []link.Link

	activeConnMu sync.RWMutex
	activeConns  map[ConnectionID]Connection
	lastSnapshot map[ConnectionID]Event

	started bool
	stopOnce sync.Once

	eventChan chan Event
	closeChan chan struct{}
}

// NewTracer 创建新的Tracer
func NewTracer(hostNetNs, selfNetNs netns.NsHandle, disableL7Tracing bool) (*Tracer, error) {
	if disableL7Tracing {
		log.Println("I! coroot_servicemap: L7 tracing is disabled")
	}

	return &Tracer{
		hostNetNs:        hostNetNs,
		selfNetNs:        selfNetNs,
		disableL7Tracing: disableL7Tracing,
		readers:          make(map[string]*perf.Reader),
		uprobes:          make(map[string]*ebpf.Program),
		activeConns:      make(map[ConnectionID]Connection),
		lastSnapshot:     make(map[ConnectionID]Event),
		eventChan:        make(chan Event, 1000),
		closeChan:        make(chan struct{}),
	}, nil
}

// Start 启动eBPF程序
func (t *Tracer) Start() error {
	if t.started {
		return nil
	}

	log.Println("I! coroot_servicemap: loading eBPF programs...")

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
		log.Println("W! coroot_servicemap: tracefs unavailable, fallback to polling tracer")
		go t.startPollingTracer()
		t.started = true
		return nil
	}

	if err := checkKernelVersion(); err != nil {
		log.Printf("W! coroot_servicemap: kernel check failed, fallback to polling tracer: %v", err)
		go t.startPollingTracer()
		t.started = true
		return nil
	}

	// 在实际部署时需要：
	// 1. 编写eBPF C代码
	// 2. 使用clang编译为字节码
	// 3. 嵌入到Go程序中
	// 由于eBPF程序需要预编译，这里暂时返回提示信息
	// 注意: 这里需要实际的eBPF字节码

	log.Println("W! coroot_servicemap: eBPF program loading is not yet implemented, fallback to polling tracer")
	go t.startPollingTracer()
	t.started = true
	return nil
}

// loadEBPF 加载eBPF程序（待实现）
func (t *Tracer) loadEBPF() error {
	// 设置资源限制
	_ = unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{
		Cur: unix.RLIM_INFINITY,
		Max: unix.RLIM_INFINITY,
	})

	// TODO: 加载预编译的eBPF程序
	// 这需要：
	// 1. eBPF C代码 (tcp连接跟踪、L7解析等)
	// 2. 编译脚本
	// 3. 字节码嵌入
	return fmt.Errorf("eBPF program loading not implemented")
}

// Events 返回事件通道
func (t *Tracer) Events() <-chan Event {
	return t.eventChan
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

// Close 关闭Tracer
func (t *Tracer) Close() {
	t.stopOnce.Do(func() {
		close(t.closeChan)
	})

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

	log.Println("I! coroot_servicemap: tracer closed")
}

func (t *Tracer) startPollingTracer() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	t.pollConnections()

	for {
		select {
		case <-t.closeChan:
			return
		case <-ticker.C:
			t.pollConnections()
		}
	}
}

func (t *Tracer) pollConnections() {
	conns, err := gopsnet.Connections("tcp")
	if err != nil {
		log.Printf("W! coroot_servicemap: polling tcp connections failed: %v", err)
		return
	}

	now := uint64(time.Now().UnixNano())
	current := make(map[ConnectionID]Event, len(conns))

	for _, c := range conns {
		if !isTrackedTCPConnection(c) {
			continue
		}

		id := ConnectionID{FD: uint64(c.Fd), PID: uint32(c.Pid)}
		e := Event{
			Type:      EventTypeConnectionOpen,
			Timestamp: now,
			Pid:       uint32(c.Pid),
			Fd:        uint64(c.Fd),
			SrcAddr:   endpoint(c.Laddr.IP, c.Laddr.Port),
			DstAddr:   endpoint(c.Raddr.IP, c.Raddr.Port),
			SrcPort:   uint16(c.Laddr.Port),
			DstPort:   uint16(c.Raddr.Port),
		}
		current[id] = e
	}

	t.activeConnMu.Lock()
	defer t.activeConnMu.Unlock()

	for id, e := range current {
		if _, ok := t.lastSnapshot[id]; !ok {
			t.emitEventLocked(e)
		}

		if existing, ok := t.activeConns[id]; ok {
			t.activeConns[id] = Connection{
				Timestamp:     existing.Timestamp,
				BytesSent:     existing.BytesSent,
				BytesReceived: existing.BytesReceived,
			}
		} else {
			t.activeConns[id] = Connection{Timestamp: e.Timestamp}
		}
	}

	for id, old := range t.lastSnapshot {
		if _, ok := current[id]; ok {
			continue
		}

		old.Type = EventTypeConnectionClose
		old.Timestamp = now
		t.emitEventLocked(old)
		delete(t.activeConns, id)
	}

	t.lastSnapshot = current
}

func (t *Tracer) emitEventLocked(e Event) {
	select {
	case t.eventChan <- e:
	default:
		log.Println("W! coroot_servicemap: event channel is full, dropping event")
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

	return c.Fd > 0
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
	reader, err := gzip.NewReader(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(compressed)))
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
	log.Printf("I! coroot_servicemap: kernel version: %s", release)

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
	log.Printf("I! coroot_servicemap: kernel version check passed")

	return nil
}
