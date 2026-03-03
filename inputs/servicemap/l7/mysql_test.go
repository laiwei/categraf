package l7

import (
	"encoding/binary"
	"testing"
)

func TestParseMySQLComQuery(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "simple SELECT",
			payload: buildMySQLPacket(0, MysqlComQuery, []byte("SELECT 1")),
			want:    "SELECT 1",
		},
		{
			name:    "SELECT with FROM",
			payload: buildMySQLPacket(0, MysqlComQuery, []byte("SELECT * FROM users WHERE id = 1")),
			want:    "SELECT * FROM users WHERE id = 1",
		},
		{
			name:    "INSERT statement",
			payload: buildMySQLPacket(0, MysqlComQuery, []byte("INSERT INTO users (name) VALUES ('test')")),
			want:    "INSERT INTO users (name) VALUES ('test')",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseMySQL(tt.payload)
			if got != tt.want {
				t.Errorf("ParseMySQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseMySQLStateless_NonQuery(t *testing.T) {
	// COM_STMT_EXECUTE should return empty from stateless parser
	payload := buildMySQLPacket(0, MysqlComStmtExecute, []byte{0x01, 0x00, 0x00, 0x00, 0x00})
	got := ParseMySQL(payload)
	if got != "" {
		t.Errorf("ParseMySQL(COM_STMT_EXECUTE) = %q, want empty", got)
	}
}

func TestMysqlParser_PreparedStatements(t *testing.T) {
	parser := NewMysqlParser()

	// Step 1: COM_STMT_PREPARE with statementId = 42
	preparePayload := buildMySQLPacket(0, MysqlComStmtPrepare, []byte("SELECT * FROM users WHERE id = ?"))
	query := parser.Parse(preparePayload, 42)
	if query != "PREPARE 42 FROM SELECT * FROM users WHERE id = ?" {
		t.Errorf("PREPARE: got %q", query)
	}

	// Step 2: COM_STMT_EXECUTE with statementId 42 in payload
	execBody := make([]byte, 5)
	binary.LittleEndian.PutUint32(execBody, 42)
	execBody[4] = 0 // flags
	execPayload := buildMySQLPacket(0, MysqlComStmtExecute, execBody)
	query = parser.Parse(execPayload, 0)
	if query != "SELECT * FROM users WHERE id = ?" {
		t.Errorf("EXECUTE: got %q", query)
	}

	// Step 3: COM_STMT_CLOSE for statementId 42
	closeBody := make([]byte, 4)
	binary.LittleEndian.PutUint32(closeBody, 42)
	closePayload := buildMySQLPacket(0, MysqlComStmtClose, closeBody)
	query = parser.Parse(closePayload, 0)
	if query != "" {
		t.Errorf("CLOSE: got %q, want empty", query)
	}

	// Step 4: EXECUTE again should be unknown
	query = parser.Parse(execPayload, 0)
	if query != "EXECUTE 42 /* unknown */" {
		t.Errorf("EXECUTE after CLOSE: got %q", query)
	}
}

func TestMysqlParser_UnknownPreparedStatement(t *testing.T) {
	parser := NewMysqlParser()

	execBody := make([]byte, 5)
	binary.LittleEndian.PutUint32(execBody, 99)
	execBody[4] = 0
	execPayload := buildMySQLPacket(0, MysqlComStmtExecute, execBody)

	query := parser.Parse(execPayload, 0)
	if query != "EXECUTE 99 /* unknown */" {
		t.Errorf("got %q", query)
	}
}

func TestIsMySQLQuery(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{
			name:    "COM_QUERY",
			payload: buildMySQLPacket(0, MysqlComQuery, []byte("SELECT 1")),
			want:    true,
		},
		{
			name:    "COM_STMT_EXECUTE",
			payload: buildMySQLPacket(0, MysqlComStmtExecute, []byte{0x01, 0x00, 0x00, 0x00, 0x00}),
			want:    true,
		},
		{
			name:    "COM_STMT_PREPARE",
			payload: buildMySQLPacket(0, MysqlComStmtPrepare, []byte("SELECT ?")),
			want:    true,
		},
		{
			name:    "too short",
			payload: []byte{0x01, 0x00},
			want:    false,
		},
		{
			name:    "unknown command",
			payload: buildMySQLPacket(0, 0xFF, []byte{0x01}),
			want:    false,
		},
		{
			name:    "bad sequence number",
			payload: buildMySQLPacketSeq(1, MysqlComQuery, []byte("SELECT 1")),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsMySQLQuery(tt.payload)
			if got != tt.want {
				t.Errorf("IsMySQLQuery() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMySQLTruncated(t *testing.T) {
	// Build a query where payload is shorter than expected msgSize
	query := []byte("SELECT * FROM a_very_long_table_name WHERE id = 1 AND status = 'active'")
	full := buildMySQLPacket(0, MysqlComQuery, query)
	// Truncate to simulate eBPF payload limit
	truncated := full[:mysqlMsgHeaderSize+20]
	got := ParseMySQL(truncated)
	if got == "" {
		t.Error("expected non-empty result for truncated payload")
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("expected truncated query to end with '...', got %q", got)
	}
}

// buildMySQLPacket 构造 MySQL 协议包: 3-byte LE length + seq(0) + cmd + body
func buildMySQLPacket(seq byte, cmd byte, body []byte) []byte {
	return buildMySQLPacketSeq(seq, cmd, body)
}

func buildMySQLPacketSeq(seq byte, cmd byte, body []byte) []byte {
	msgLen := 1 + len(body) // cmd + body
	pkt := make([]byte, 4+msgLen)
	pkt[0] = byte(msgLen)
	pkt[1] = byte(msgLen >> 8)
	pkt[2] = byte(msgLen >> 16)
	pkt[3] = seq
	pkt[4] = cmd
	copy(pkt[5:], body)
	return pkt
}
