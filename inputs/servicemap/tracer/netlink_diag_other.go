//go:build !linux

package tracer

import "errors"

// netlinkConnections 非 Linux 平台不支持 NETLINK_INET_DIAG，返回错误强制 fallback 到 gopsutil。
func netlinkConnections() ([]DiagConnection, error) {
	return nil, errors.New("netlink inet_diag not supported on this platform")
}

// DiagConnection 非 Linux 平台的 stub 定义（保持类型一致）。
type DiagConnection struct {
	SrcIP   string
	SrcPort uint16
	DstIP   string
	DstPort uint16
	State   uint8
	Inode   uint32
	// 以下字段由 INET_DIAG_INFO 填充（Linux only）；非 Linux 始终为 0。
	BytesSent     uint64
	BytesReceived uint64
	TotalRetrans  uint32
}

func (d *DiagConnection) IsListen() bool  { return false }
func (d *DiagConnection) IsTracked() bool { return false }
