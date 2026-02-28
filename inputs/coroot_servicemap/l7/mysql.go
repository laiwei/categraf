package l7

import (
	"encoding/binary"
	"fmt"
	"strconv"
)

// MySQL 协议常量
// https://dev.mysql.com/doc/dev/mysql-server/latest/PAGE_PROTOCOL.html
const (
	MysqlComQuery       byte = 3    // COM_QUERY
	MysqlComStmtPrepare byte = 0x16 // COM_STMT_PREPARE
	MysqlComStmtExecute byte = 0x17 // COM_STMT_EXECUTE
	MysqlComStmtClose   byte = 0x19 // COM_STMT_CLOSE

	mysqlMsgHeaderSize = 4 // 3-byte length + 1-byte sequence
)

// MysqlParser 有状态的 MySQL 解析器（跟踪 prepared statements）
type MysqlParser struct {
	preparedStatements map[string]string
}

// NewMysqlParser 创建新的 MySQL 解析器
func NewMysqlParser() *MysqlParser {
	return &MysqlParser{preparedStatements: map[string]string{}}
}

// Parse 从 MySQL 协议载荷中提取 SQL 查询文本。
// statementId 来自 eBPF 事件（用于 COM_STMT_EXECUTE 匹配已 prepare 的语句）。
func (p *MysqlParser) Parse(payload []byte, statementId uint32) string {
	payloadSize := len(payload)
	if payloadSize < mysqlMsgHeaderSize+1 {
		return ""
	}

	// MySQL 消息: 3-byte little-endian length, 1-byte sequence, payload
	msgSize := int(payload[0]) | int(payload[1])<<8 | int(payload[2])<<16
	cmd := payload[4]

	readQuery := func() (query string) {
		to := mysqlMsgHeaderSize + msgSize
		partial := false
		if to > payloadSize {
			to = payloadSize
			partial = true
		}
		if to <= mysqlMsgHeaderSize+1 {
			return ""
		}
		query = string(payload[mysqlMsgHeaderSize+1 : to])
		if partial {
			query += "..."
		}
		return query
	}

	readStatementId := func() string {
		if payloadSize < mysqlMsgHeaderSize+5 {
			return "0"
		}
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(payload[mysqlMsgHeaderSize+1:])), 10)
	}

	switch cmd {
	case MysqlComQuery:
		return readQuery()
	case MysqlComStmtExecute:
		statementIdStr := readStatementId()
		statement, ok := p.preparedStatements[statementIdStr]
		if !ok {
			statement = fmt.Sprintf(`EXECUTE %s /* unknown */`, statementIdStr)
		}
		return statement
	case MysqlComStmtPrepare:
		query := readQuery()
		statementIdStr := strconv.FormatUint(uint64(statementId), 10)
		p.preparedStatements[statementIdStr] = query
		return fmt.Sprintf("PREPARE %s FROM %s", statementIdStr, query)
	case MysqlComStmtClose:
		statementIdStr := readStatementId()
		delete(p.preparedStatements, statementIdStr)
	}
	return ""
}

// ParseMySQL 无状态的 MySQL 简易解析：仅提取 COM_QUERY 中的 SQL 文本。
// 对于不需要 prepared statement 跟踪的场景使用。
func ParseMySQL(payload []byte) string {
	if len(payload) < mysqlMsgHeaderSize+1 {
		return ""
	}

	cmd := payload[4]
	if cmd != MysqlComQuery {
		return ""
	}

	msgSize := int(payload[0]) | int(payload[1])<<8 | int(payload[2])<<16
	to := mysqlMsgHeaderSize + msgSize
	if to > len(payload) {
		return string(payload[mysqlMsgHeaderSize+1:]) + "..."
	}
	if to <= mysqlMsgHeaderSize+1 {
		return ""
	}
	return string(payload[mysqlMsgHeaderSize+1 : to])
}

// IsMySQLQuery 判断载荷是否为 MySQL 请求
func IsMySQLQuery(payload []byte) bool {
	if len(payload) < 5 {
		return false
	}
	// 3-byte LE length + 1-byte sequence(must be 0 for request)
	msgSize := int(payload[0]) | int(payload[1])<<8 | int(payload[2])<<16
	if msgSize+4 != len(payload) || payload[3] != 0 {
		return false
	}
	cmd := payload[4]
	return cmd == MysqlComQuery || cmd == MysqlComStmtExecute ||
		cmd == MysqlComStmtPrepare || cmd == MysqlComStmtClose
}
