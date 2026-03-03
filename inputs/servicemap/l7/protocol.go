package l7

import (
	"strconv"
	"time"
)

// Protocol L7 协议类型（与 coroot-node-agent 保持一致的编号）
type Protocol uint8

const (
	ProtocolUnknown  Protocol = 0
	ProtocolHTTP     Protocol = 1
	ProtocolPostgres Protocol = 2
	ProtocolRedis    Protocol = 3
	ProtocolMySQL    Protocol = 5
	ProtocolKafka    Protocol = 7
)

func (p Protocol) String() string {
	switch p {
	case ProtocolHTTP:
		return "HTTP"
	case ProtocolPostgres:
		return "Postgres"
	case ProtocolRedis:
		return "Redis"
	case ProtocolMySQL:
		return "MySQL"
	case ProtocolKafka:
		return "Kafka"
	}
	return "UNKNOWN:" + strconv.Itoa(int(p))
}

// Method L7 方法类型（与 coroot-node-agent 保持一致的编号）
type Method uint8

const (
	MethodUnknown          Method = 0
	MethodProduce          Method = 1
	MethodConsume          Method = 2
	MethodStatementPrepare Method = 3
	MethodStatementClose   Method = 4
)

func (m Method) String() string {
	switch m {
	case MethodUnknown:
		return "unknown"
	case MethodProduce:
		return "produce"
	case MethodConsume:
		return "consume"
	case MethodStatementPrepare:
		return "statement_prepare"
	case MethodStatementClose:
		return "statement_close"
	}
	return "UNKNOWN:" + strconv.Itoa(int(m))
}

// Status L7 响应状态（对 HTTP 即状态码）
type Status int32

const (
	StatusUnknown Status = 0
	StatusOK      Status = 200
	StatusFailed  Status = 500
)

func (s Status) String() string {
	switch s {
	case StatusUnknown:
		return "unknown"
	case StatusOK:
		return "ok"
	case StatusFailed:
		return "failed"
	}
	return strconv.Itoa(int(s))
}

// HTTPStatusClass 将 HTTP 状态码归类为 1xx/2xx/3xx/4xx/5xx
func (s Status) HTTPStatusClass() string {
	switch {
	case s >= 100 && s < 200:
		return "1xx"
	case s >= 200 && s < 300:
		return "2xx"
	case s >= 300 && s < 400:
		return "3xx"
	case s >= 400 && s < 500:
		return "4xx"
	case s >= 500 && s < 600:
		return "5xx"
	}
	return "unknown"
}

// IsError 判断状态码是否表示错误（4xx 或 5xx）
func (s Status) IsError() bool {
	return s >= 400
}

// Error 判断状态是否为失败（用于非 HTTP 协议: StatusFailed = 500）
func (s Status) Error() bool {
	return s == StatusFailed
}

// RequestData L7 请求数据（从 eBPF 事件解析或由 Go 侧构造）
type RequestData struct {
	Protocol    Protocol
	Status      Status
	Duration    time.Duration
	Method      Method
	StatementId uint32 // MySQL prepared statement ID
	Payload     []byte // eBPF 截获的请求有效载荷（最多 MaxPayloadSize 字节）
}

// MaxPayloadSize eBPF 可传递的最大有效载荷长度
const MaxPayloadSize = 1024
