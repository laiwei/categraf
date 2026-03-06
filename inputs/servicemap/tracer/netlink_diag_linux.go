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

// ─── INET_DIAG 扩展属性常量 ────────────────────────────────────
// INET_DIAG_INFO = 2，对应响应中的 tcp_info 结构。
// inet_diag_req_v2.ext 按位请求扩展：bit(index-1)。
const (
	inetDiagInfo    = 2
	inetDiagExtInfo = uint8(1 << (inetDiagInfo - 1)) // 请求 INET_DIAG_INFO
)

// tcp_info 关键字段偏移（与 linux/uapi tcp.h 保持一致）。
const (
	tcpInfoOffTotalRetrans  = 100 // __u32 tcpi_total_retrans
	tcpInfoOffBytesAcked    = 120 // __u64 tcpi_bytes_acked (Linux 4.2+)
	tcpInfoOffBytesReceived = 128 // __u64 tcpi_bytes_received (Linux 4.2+)
	tcpInfoOffBytesSent     = 200 // __u64 tcpi_bytes_sent (Linux 5.0+)

	tcpInfoMinSize4x = tcpInfoOffBytesReceived + 8
	tcpInfoMinSize5x = tcpInfoOffBytesSent + 8
)

var nativeEndian = detectNativeEndian()

func detectNativeEndian() binary.ByteOrder {
	var x uint16 = 0x0102
	b := *(*[2]byte)(unsafe.Pointer(&x))
	if b[0] == 0x02 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

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
	// 以下字段由 INET_DIAG_INFO (tcp_info) 填充；内核不支持时为 0。
	BytesSent     uint64 // tcpi_bytes_sent (Linux 5.0+) 或 tcpi_bytes_acked 代替 (Linux 4.2+)
	BytesReceived uint64 // tcpi_bytes_received (Linux 4.2+)
	TotalRetrans  uint32 // tcpi_total_retrans
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
// 设置 Ext = inetDiagExtInfo 请求内核返回 tcp_info（INET_DIAG_INFO 属性），
// 从而一次性获得所有连接的字节和重传计数，无需逐个查询。
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
		Ext:      inetDiagExtInfo, // 请求 INET_DIAG_INFO 属性（tcp_info）
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
		if len(m.Data) < sizeOfDiagMsg {
			continue
		}
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

		// 解析 inetDiagMsg 之后的 netlink 属性（nlattr）
		// 查找 INET_DIAG_INFO 属性，获取 tcp_info 结构中的字节和重传计数。
		if attrs := parseNLAttrs(m.Data[sizeOfDiagMsg:]); len(attrs) > 0 {
			if tcpInfoBytes, ok := attrs[inetDiagInfo]; ok {
				dc.BytesSent, dc.BytesReceived, dc.TotalRetrans = parseTCPInfo(tcpInfoBytes)
			}
		}

		result = append(result, dc)
	}

	return result, nil
}

// parseNLAttrs 解析 netlink 属性列表，返回类型→数据的映射。
// 每个属性格式：[nla_len(2)] [nla_type(2)] [data(nla_len-4)] [padding to 4B]
func parseNLAttrs(b []byte) map[uint16][]byte {
	attrs := make(map[uint16][]byte)
	for len(b) >= 4 {
		nlaLen := nativeEndian.Uint16(b[0:2])
		nlaType := nativeEndian.Uint16(b[2:4])
		if nlaLen < 4 || int(nlaLen) > len(b) {
			break
		}
		attrs[nlaType] = b[4:nlaLen]
		// 4 字节对齐
		aligned := (int(nlaLen) + 3) &^ 3
		if aligned >= len(b) {
			break
		}
		b = b[aligned:]
	}
	return attrs
}

// parseTCPInfo 从原始 tcp_info 字节中提取发送字节、接收字节和重传次数。
// 字段偏移量与 Linux 内核 tcp.h ABI 对齐，不同内核版本返回不同的字段集。
//
// tcpi_bytes_sent   在 Linux 5.0+ 才存在（偏移 200）。
// tcpi_bytes_acked  在 Linux 4.2+ 存在（偏移 120），当 bytes_sent 不可用时用作代替
//
//	（含义略有不同：bytes_acked 不含尚未 ACK 的在途包）。
//
// tcpi_bytes_received 在 Linux 4.2+ 存在（偏移 128）。
// tcpi_total_retrans  在 Linux 3.x＋存在（偏移 100）。
func parseTCPInfo(b []byte) (bytesSent, bytesReceived uint64, totalRetrans uint32) {
	if len(b) >= tcpInfoOffTotalRetrans+4 {
		totalRetrans = nativeEndian.Uint32(b[tcpInfoOffTotalRetrans:])
	}
	if len(b) >= tcpInfoMinSize4x {
		bytesReceived = nativeEndian.Uint64(b[tcpInfoOffBytesReceived:])
	}
	if len(b) >= tcpInfoMinSize5x {
		// Linux 5.0+：精确发送字节数
		bytesSent = nativeEndian.Uint64(b[tcpInfoOffBytesSent:])
	} else if len(b) >= tcpInfoOffBytesAcked+8 {
		// Linux 4.2-4.x：用 bytes_acked 作为 bytes_sent 的近似值
		bytesSent = nativeEndian.Uint64(b[tcpInfoOffBytesAcked:])
	}
	return
}
