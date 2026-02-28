package l7

import (
	"bytes"
	"fmt"
)

// PostgreSQL 协议常量
// https://www.postgresql.org/docs/current/protocol-message-formats.html
const (
	PostgresFrameQuery byte = 'Q' // Simple query
	PostgresFrameBind  byte = 'B' // Bind (execute prepared statement)
	PostgresFrameParse byte = 'P' // Parse (prepare statement)
	PostgresFrameClose byte = 'C' // Close (close prepared statement)
)

// PostgresParser 有状态的 PostgreSQL 解析器（跟踪 prepared statements）
type PostgresParser struct {
	preparedStatements map[string]string
}

// NewPostgresParser 创建新的 PostgreSQL 解析器
func NewPostgresParser() *PostgresParser {
	return &PostgresParser{preparedStatements: map[string]string{}}
}

// Parse 从 PostgreSQL 协议载荷中提取 SQL 查询文本。
func (p *PostgresParser) Parse(payload []byte) string {
	l := len(payload)
	if l < 5 {
		return ""
	}
	cmd := payload[0]
	switch cmd {
	case PostgresFrameQuery:
		// Simple Query: 'Q' + 4-byte length + query string + \0
		var query string
		if q, _, ok := bytes.Cut(payload[5:], []byte{0}); ok {
			query = string(q)
		} else {
			query = string(payload[5:]) + "..."
		}
		return query

	case PostgresFrameBind:
		// Bind: 'B' + 4-byte length + destination portal\0 + prepared statement name\0 + ...
		_, rest, ok := bytes.Cut(payload[5:], []byte{0})
		if !ok {
			return ""
		}
		preparedStatementName, _, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return ""
		}
		preparedStatementNameStr := string(preparedStatementName)
		statement, ok := p.preparedStatements[preparedStatementNameStr]
		if !ok {
			statement = fmt.Sprintf(`EXECUTE %s /* unknown */`, preparedStatementNameStr)
		}
		return statement

	case PostgresFrameParse:
		// Parse: 'P' + 4-byte length + prepared statement name\0 + query string\0 + ...
		if l < 7 {
			return ""
		}
		preparedStatementName, rest, ok := bytes.Cut(payload[5:], []byte{0})
		if !ok {
			return ""
		}
		var query string
		q, _, ok := bytes.Cut(rest, []byte{0})
		if ok {
			query = string(q)
		} else {
			query = string(q) + "..."
		}
		preparedStatementNameStr := string(preparedStatementName)
		p.preparedStatements[preparedStatementNameStr] = query
		return fmt.Sprintf("PREPARE %s AS %s", preparedStatementNameStr, query)

	case PostgresFrameClose:
		// Close: 'C' + 4-byte length + 'S'(statement)/'P'(portal) + name\0
		if l < 7 {
			return ""
		}
		if payload[5] != 'S' {
			return ""
		}
		preparedStatementName, _, ok := bytes.Cut(payload[6:], []byte{0})
		if !ok {
			return ""
		}
		delete(p.preparedStatements, string(preparedStatementName))
	}
	return ""
}

// ParsePostgres 无状态的 PostgreSQL 简易解析：仅提取 Simple Query 的 SQL 文本。
func ParsePostgres(payload []byte) string {
	if len(payload) < 5 {
		return ""
	}
	if payload[0] != PostgresFrameQuery {
		return ""
	}
	if q, _, ok := bytes.Cut(payload[5:], []byte{0}); ok {
		return string(q)
	}
	return string(payload[5:]) + "..."
}

// IsPostgresQuery 判断载荷是否为 PostgreSQL 请求
func IsPostgresQuery(payload []byte) bool {
	if len(payload) < 5 {
		return false
	}
	cmd := payload[0]
	return cmd == PostgresFrameQuery || cmd == PostgresFrameParse ||
		cmd == PostgresFrameBind || cmd == PostgresFrameClose
}
