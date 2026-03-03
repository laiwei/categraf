package tracer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"flashcat.cloud/categraf/inputs/servicemap/l7"
)

// rawEvent 对应 eBPF C 端的 struct event
// 必须与 tcp_tracer.bpf.c 中的定义保持一致
type rawEvent struct {
	Timestamp uint64
	Type      uint32
	Pid       uint32
	Fd        uint64
	SrcAddr   [4]uint32 // IPv4 只用第一个元素，IPv6 用全部
	DstAddr   [4]uint32
	SrcPort   uint16
	DstPort   uint16
	Family    uint16 // AF_INET=2 or AF_INET6=10
	BytesSent uint64
	BytesRecv uint64
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
	}

	// 解析地址
	if raw.Family == afINET {
		event.SrcAddr = intToIPv4(raw.SrcAddr[0]).String()
		event.DstAddr = intToIPv4(raw.DstAddr[0]).String()
	} else if raw.Family == afINET6 {
		event.SrcAddr = intArrayToIPv6(raw.SrcAddr).String()
		event.DstAddr = intArrayToIPv6(raw.DstAddr).String()
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
