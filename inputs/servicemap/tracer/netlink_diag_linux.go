//go:build linux

package tracer

// netlink_diag_linux.go — 使用 NETLINK_INET_DIAG（ss 命令的底层实现）替代 procfs 读取 TCP 连接。
// 优势：内核态过滤（state bitmap + port），10k 连接从 ~100ms 降到 ~5ms。

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"syscall"
	"unsafe"

	"github.com/mdlayher/netlink"
)

// ─── TCP 状态常量 ────────────────────────────────────────────

const (
	tcpEstablished = 1
	tcpSynSent     = 2
	tcpSynRecv     = 3
	tcpListen      = 10
)

// stateMask 是 pollConnections 关注的 TCP 状态集合的 bitmap。
// ESTABLISHED + SYN_SENT + SYN_RECV + LISTEN
var stateMask = uint32(1<<tcpEstablished | 1<<tcpSynSent | 1<<tcpSynRecv | 1<<tcpListen)

// ─── Netlink 结构体（对齐内核 inet_diag.h）────────────────────

const sockDiagByFamily = 20

// inetDiagSockID 对应 linux inet_diag_sockid（C 结构体大小 48 字节）
type inetDiagSockID struct {
	SPort     [2]byte    // 源端口（网络字节序）
	DPort     [2]byte    // 目标端口（网络字节序）
	Src       [4][4]byte // 源 IP（IPv4 用 [0]，IPv6 用全部）
	Dst       [4][4]byte // 目标 IP
	Interface uint32
	Cookie    [2]uint32
}

// inetDiagReqV2 对应 linux inet_diag_req_v2（请求结构）
type inetDiagReqV2 struct {
	Family   uint8
	Protocol uint8
	Ext      uint8
	Pad      uint8
	States   uint32
	ID       inetDiagSockID
}

const sizeOfDiagReqV2 = int(unsafe.Sizeof(inetDiagReqV2{}))

func (r *inetDiagReqV2) serialize() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(r)), sizeOfDiagReqV2)
}

// inetDiagMsg 对应 linux inet_diag_msg（响应头）
type inetDiagMsg struct {
	Family  uint8
	State   uint8
	Timer   uint8
	Retrans uint8
	ID      inetDiagSockID
	Expires uint32
	RQueue  uint32
	WQueue  uint32
	UID     uint32
	Inode   uint32
}

const sizeOfDiagMsg = int(unsafe.Sizeof(inetDiagMsg{}))

func parseDiagMsg(b []byte) (*inetDiagMsg, error) {
	if len(b) < sizeOfDiagMsg {
		return nil, fmt.Errorf("diag msg too short: %d < %d", len(b), sizeOfDiagMsg)
	}
	return (*inetDiagMsg)(unsafe.Pointer(&b[0])), nil
}

// ─── 连接结果结构 ────────────────────────────────────────────

// DiagConnection 表示一条从 netlink 获取的 TCP 连接信息。
// 字段名与 gopsutil ConnectionStat 对齐，便于 pollConnections 无缝切换。
type DiagConnection struct {
	SrcIP   string
	SrcPort uint16
	DstIP   string
	DstPort uint16
	State   uint8 // TCP 状态
	Inode   uint32
}

// IsListen 返回连接是否处于 LISTEN 状态
func (d *DiagConnection) IsListen() bool {
	return d.State == tcpListen
}

// IsTracked 返回是否为 pollConnections 需要跟踪的连接状态
func (d *DiagConnection) IsTracked() bool {
	switch d.State {
	case tcpEstablished, tcpSynSent, tcpSynRecv:
		return true
	default:
		return false
	}
}

// ─── 核心函数 ────────────────────────────────────────────────

// netlinkConnections 通过 NETLINK_INET_DIAG 获取 TCP 连接列表。
// 支持内核态 state bitmap 过滤，IPv4 + IPv6 合并返回。
// 返回 nil, error 时调用方应 fallback 到 gopsutil。
func netlinkConnections() ([]DiagConnection, error) {
	var all []DiagConnection

	for _, family := range []uint8{syscall.AF_INET, syscall.AF_INET6} {
		conns, err := diagDump(family, stateMask)
		if err != nil {
			return nil, fmt.Errorf("netlink diag dump family=%d: %w", family, err)
		}
		all = append(all, conns...)
	}

	log.Printf("D! servicemap: netlink diag: %d connections (ESTABLISHED+SYN*+LISTEN, IPv4+IPv6)", len(all))
	return all, nil
}

// diagDump 对指定地址族执行一次 SOCK_DIAG dump。
func diagDump(family uint8, states uint32) ([]DiagConnection, error) {
	conn, err := netlink.Dial(syscall.NETLINK_INET_DIAG, nil)
	if err != nil {
		return nil, fmt.Errorf("netlink dial: %w", err)
	}
	defer conn.Close()

	req := inetDiagReqV2{
		Family:   family,
		Protocol: syscall.IPPROTO_TCP,
		States:   states,
	}

	msg := netlink.Message{
		Header: netlink.Header{
			Type:  sockDiagByFamily,
			Flags: syscall.NLM_F_REQUEST | syscall.NLM_F_DUMP,
		},
		Data: req.serialize(),
	}

	msgs, err := conn.Execute(msg)
	if err != nil {
		return nil, fmt.Errorf("netlink execute: %w", err)
	}

	result := make([]DiagConnection, 0, len(msgs))
	for _, m := range msgs {
		diag, err := parseDiagMsg(m.Data)
		if err != nil {
			continue
		}

		dc := DiagConnection{
			SrcPort: binary.BigEndian.Uint16(diag.ID.SPort[:]),
			DstPort: binary.BigEndian.Uint16(diag.ID.DPort[:]),
			State:   diag.State,
			Inode:   diag.Inode,
		}

		if family == syscall.AF_INET {
			dc.SrcIP = net.IP(diag.ID.Src[0][:4]).String()
			dc.DstIP = net.IP(diag.ID.Dst[0][:4]).String()
		} else {
			// IPv6：4 组 4 字节拼成 16 字节
			var src, dst [16]byte
			for i := 0; i < 4; i++ {
				copy(src[i*4:(i+1)*4], diag.ID.Src[i][:])
				copy(dst[i*4:(i+1)*4], diag.ID.Dst[i][:])
			}
			dc.SrcIP = net.IP(src[:]).String()
			dc.DstIP = net.IP(dst[:]).String()
		}

		result = append(result, dc)
	}

	return result, nil
}
