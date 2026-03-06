package tracer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/l7"
)

// rawEvent 对应 eBPF C 端的 struct event
// 必须与 tcp_tracer.bpf.c 中的定义保持一致（修改时同步两边）
type rawEvent struct {
	Timestamp uint64
	Type      uint32
	Pid       uint32
	Fd        uint64    // sock 指针作为唯一连接标识
	SrcAddr   [4]uint32 // IPv4 只用第一个元素，IPv6 用全部
	DstAddr   [4]uint32
	SrcPort   uint16
	DstPort   uint16
	Family    uint16 // AF_INET=2 or AF_INET6=10
	Padding   uint16 // C struct 显式对齐填充（_pad）
	BytesSent uint64
	BytesRecv uint64
	Comm      [16]byte // 进程名（由 eBPF bpf_get_current_comm 填充）
	NetnsInum uint32   // network namespace inode（用于过滤跨 VM/容器的事件）
	Padding2  uint32   // 8 字节对齐填充（_pad2）
}

const (
	afINET  = 2
	afINET6 = 10
)

// parseRawEvent 将原始字节解析为 Event
func parseRawEvent(data []byte) (*Event, error) {
	if len(data) < int(binary.Size(rawEvent{})) {
		return nil, fmt.Errorf("raw event data too short: %d bytes", len(data))
	}

	var raw rawEvent
	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.LittleEndian, &raw); err != nil {
		return nil, fmt.Errorf("parse raw event failed: %w", err)
	}

	event := &Event{
		Type:      EventType(raw.Type),
		Timestamp: raw.Timestamp,
		Pid:       raw.Pid,
		Fd:        raw.Fd,
		SrcPort:   raw.SrcPort,
		DstPort:   ntohs(raw.DstPort), // eBPF 端存储为网络字节序
		BytesSent: raw.BytesSent,
		// raw struct 字段名是 BytesRecv（兼容历史命名），映射到 Event.BytesReceived
		BytesReceived: raw.BytesRecv,
		NetnsInum:     raw.NetnsInum,
	}

	// 提取 eBPF 侧填充的进程名（null-terminated C string）
	if idx := bytes.IndexByte(raw.Comm[:], 0); idx > 0 {
		event.Comm = string(raw.Comm[:idx])
	} else if idx < 0 {
		// 无 null 终止符，使用完整 buffer
		event.Comm = strings.TrimSpace(string(raw.Comm[:]))
	}

	// 解析地址 — 格式化为 "ip:port"，与轮询模式 endpoint() 格式一致
	if raw.Family == afINET {
		srcIP := intToIPv4(raw.SrcAddr[0]).String()
		dstIP := intToIPv4(raw.DstAddr[0]).String()
		event.SrcAddr = net.JoinHostPort(srcIP, strconv.Itoa(int(event.SrcPort)))
		event.DstAddr = net.JoinHostPort(dstIP, strconv.Itoa(int(event.DstPort)))
	} else if raw.Family == afINET6 {
		srcIP := intArrayToIPv6(raw.SrcAddr).String()
		dstIP := intArrayToIPv6(raw.DstAddr).String()
		event.SrcAddr = net.JoinHostPort(srcIP, strconv.Itoa(int(event.SrcPort)))
		event.DstAddr = net.JoinHostPort(dstIP, strconv.Itoa(int(event.DstPort)))
	}

	return event, nil
}

// intToIPv4 将 uint32 转为 IPv4 地址
func intToIPv4(addr uint32) net.IP {
	ip := make(net.IP, 4)
	// Little-endian
	ip[0] = byte(addr)
	ip[1] = byte(addr >> 8)
	ip[2] = byte(addr >> 16)
	ip[3] = byte(addr >> 24)
	return ip
}

// intArrayToIPv6 将 [4]uint32 转为 IPv6 地址
func intArrayToIPv6(addr [4]uint32) net.IP {
	ip := make(net.IP, 16)
	for i := 0; i < 4; i++ {
		// Little-endian per uint32
		offset := i * 4
		ip[offset] = byte(addr[i])
		ip[offset+1] = byte(addr[i] >> 8)
		ip[offset+2] = byte(addr[i] >> 16)
		ip[offset+3] = byte(addr[i] >> 24)
	}
	return ip
}

// ntohs 网络字节序转主机字节序（16位）
func ntohs(n uint16) uint16 {
	return (n >> 8) | (n << 8)
}

// ============================================================
// P2-9: L7 事件解析
// ============================================================

// rawL7Event 对应 eBPF C 端的 struct l7_event
// 必须与 eBPF 程序中的定义保持一致
type rawL7Event struct {
	Fd                  uint64
	ConnectionTimestamp uint64
	Pid                 uint32
	Status              int32
	Duration            uint64 // nanoseconds
	Protocol            uint8
	Method              uint8
	Padding             uint16
	StatementId         uint32
	PayloadSize         uint64
}

// parseRawL7Event 将 L7 perf buffer 原始字节解析为 Event
func parseRawL7Event(data []byte) (*Event, error) {
	headerSize := int(binary.Size(rawL7Event{}))
	if len(data) < headerSize {
		return nil, fmt.Errorf("raw L7 event data too short: %d bytes", len(data))
	}

	var raw rawL7Event
	reader := bytes.NewReader(data[:headerSize])
	if err := binary.Read(reader, binary.LittleEndian, &raw); err != nil {
		return nil, fmt.Errorf("parse raw L7 event failed: %w", err)
	}

	// 提取 payload
	payloadSize := int(raw.PayloadSize)
	if payloadSize > l7.MaxPayloadSize {
		payloadSize = l7.MaxPayloadSize
	}

	var payload []byte
	if payloadSize > 0 && len(data) >= headerSize+payloadSize {
		payload = make([]byte, payloadSize)
		copy(payload, data[headerSize:headerSize+payloadSize])
	}

	event := &Event{
		Type:      EventTypeL7Request,
		Timestamp: raw.ConnectionTimestamp,
		Pid:       raw.Pid,
		Fd:        raw.Fd,
		L7Request: &l7.RequestData{
			Protocol:    l7.Protocol(raw.Protocol),
			Status:      l7.Status(raw.Status),
			Duration:    time.Duration(raw.Duration),
			Method:      l7.Method(raw.Method),
			StatementId: raw.StatementId,
			Payload:     payload,
		},
	}

	return event, nil
}
