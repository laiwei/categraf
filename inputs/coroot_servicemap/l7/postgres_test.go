package l7

import (
	"encoding/binary"
	"testing"
)

func TestParsePostgresSimpleQuery(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "SELECT",
			payload: buildPgSimpleQuery("SELECT 1"),
			want:    "SELECT 1",
		},
		{
			name:    "SELECT with FROM",
			payload: buildPgSimpleQuery("SELECT * FROM users WHERE id = 1"),
			want:    "SELECT * FROM users WHERE id = 1",
		},
		{
			name:    "INSERT",
			payload: buildPgSimpleQuery("INSERT INTO users (name) VALUES ('test')"),
			want:    "INSERT INTO users (name) VALUES ('test')",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePostgres(tt.payload)
			if got != tt.want {
				t.Errorf("ParsePostgres() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostgresParser_PreparedStatements(t *testing.T) {
	parser := NewPostgresParser()

	// Step 1: Parse (prepare) a named statement
	parsePayload := buildPgParseFrame("stmt1", "SELECT * FROM users WHERE id = $1")
	query := parser.Parse(parsePayload)
	if query != "PREPARE stmt1 AS SELECT * FROM users WHERE id = $1" {
		t.Errorf("PARSE: got %q", query)
	}

	// Step 2: Bind to execute prepared statement
	bindPayload := buildPgBindFrame("", "stmt1")
	query = parser.Parse(bindPayload)
	if query != "SELECT * FROM users WHERE id = $1" {
		t.Errorf("BIND: got %q", query)
	}

	// Step 3: Close the prepared statement
	closePayload := buildPgCloseFrame("stmt1")
	query = parser.Parse(closePayload)
	if query != "" {
		t.Errorf("CLOSE: got %q, want empty", query)
	}

	// Step 4: Bind again should be unknown
	query = parser.Parse(bindPayload)
	if query != "EXECUTE stmt1 /* unknown */" {
		t.Errorf("BIND after CLOSE: got %q", query)
	}
}

func TestPostgresParser_UnnamedPreparedStatement(t *testing.T) {
	parser := NewPostgresParser()

	// Parse with empty name (unnamed statement)
	parsePayload := buildPgParseFrame("", "SELECT 1")
	query := parser.Parse(parsePayload)
	if query != "PREPARE  AS SELECT 1" {
		t.Errorf("PARSE unnamed: got %q", query)
	}

	// Bind with empty name
	bindPayload := buildPgBindFrame("", "")
	query = parser.Parse(bindPayload)
	if query != "SELECT 1" {
		t.Errorf("BIND unnamed: got %q", query)
	}
}

func TestIsPostgresQuery(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{
			name:    "Simple Query",
			payload: buildPgSimpleQuery("SELECT 1"),
			want:    true,
		},
		{
			name:    "Parse frame",
			payload: buildPgParseFrame("s1", "SELECT ?"),
			want:    true,
		},
		{
			name:    "Bind frame",
			payload: buildPgBindFrame("", "s1"),
			want:    true,
		},
		{
			name:    "Close frame",
			payload: buildPgCloseFrame("s1"),
			want:    true,
		},
		{
			name:    "too short",
			payload: []byte{'Q', 0x00},
			want:    false,
		},
		{
			name:    "unknown frame type",
			payload: []byte{'X', 0x00, 0x00, 0x00, 0x04},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPostgresQuery(tt.payload)
			if got != tt.want {
				t.Errorf("IsPostgresQuery() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParsePostgresTruncated(t *testing.T) {
	// Simple query without null terminator (simulating truncation)
	payload := make([]byte, 5+10) // Q + 4-byte length + partial query
	payload[0] = PostgresFrameQuery
	binary.BigEndian.PutUint32(payload[1:5], uint32(4+100)) // claims bigger size
	copy(payload[5:], "SELECT * F")

	got := ParsePostgres(payload)
	if got != "SELECT * F..." {
		t.Errorf("truncated: got %q, want %q", got, "SELECT * F...")
	}
}

func TestPostgresParser_ClosePortal(t *testing.T) {
	parser := NewPostgresParser()

	// Parse a statement first
	parser.Parse(buildPgParseFrame("s1", "SELECT 1"))

	// Close a portal (type 'P'), not a statement — should be ignored
	payload := make([]byte, 0, 20)
	payload = append(payload, PostgresFrameClose)
	body := append([]byte{0, 0, 0, 0}, 'P') // 'P' = portal
	body = append(body, []byte("p1")...)
	body = append(body, 0) // null terminator
	binary.BigEndian.PutUint32(body[:4], uint32(len(body)))
	payload = append(payload, body...)

	query := parser.Parse(payload)
	if query != "" {
		t.Errorf("close portal: got %q, want empty", query)
	}

	// Statement should still exist
	bindPayload := buildPgBindFrame("", "s1")
	query = parser.Parse(bindPayload)
	if query != "SELECT 1" {
		t.Errorf("bind after portal close: got %q", query)
	}
}

// ---- helper functions ----

// buildPgSimpleQuery 构造 PostgreSQL Simple Query 帧: 'Q' + length(4) + query + \0
func buildPgSimpleQuery(query string) []byte {
	// length = 4 (self) + len(query) + 1 (null terminator)
	length := 4 + len(query) + 1
	pkt := make([]byte, 1+length)
	pkt[0] = PostgresFrameQuery
	binary.BigEndian.PutUint32(pkt[1:5], uint32(length))
	copy(pkt[5:], query)
	pkt[5+len(query)] = 0
	return pkt
}

// buildPgParseFrame 构造 PostgreSQL Parse 帧: 'P' + length + name\0 + query\0 + param_count(2)
func buildPgParseFrame(name, query string) []byte {
	// length = 4 + len(name)+1 + len(query)+1 + 2
	bodyLen := len(name) + 1 + len(query) + 1 + 2
	length := 4 + bodyLen
	pkt := make([]byte, 1+length)
	pkt[0] = PostgresFrameParse
	binary.BigEndian.PutUint32(pkt[1:5], uint32(length))
	offset := 5
	copy(pkt[offset:], name)
	offset += len(name)
	pkt[offset] = 0
	offset++
	copy(pkt[offset:], query)
	offset += len(query)
	pkt[offset] = 0
	offset++
	// param count = 0
	pkt[offset] = 0
	pkt[offset+1] = 0
	return pkt
}

// buildPgBindFrame 构造 PostgreSQL Bind 帧: 'B' + length + portal\0 + stmt_name\0 + ...
func buildPgBindFrame(portal, stmtName string) []byte {
	bodyLen := len(portal) + 1 + len(stmtName) + 1 + 2 + 2 + 2 // + format codes + params + result format
	length := 4 + bodyLen
	pkt := make([]byte, 1+length)
	pkt[0] = PostgresFrameBind
	binary.BigEndian.PutUint32(pkt[1:5], uint32(length))
	offset := 5
	copy(pkt[offset:], portal)
	offset += len(portal)
	pkt[offset] = 0
	offset++
	copy(pkt[offset:], stmtName)
	offset += len(stmtName)
	pkt[offset] = 0
	return pkt
}

// buildPgCloseFrame 构造 PostgreSQL Close 帧: 'C' + length + 'S' + name\0
func buildPgCloseFrame(name string) []byte {
	bodyLen := 1 + len(name) + 1 // 'S' + name + \0
	length := 4 + bodyLen
	pkt := make([]byte, 1+length)
	pkt[0] = PostgresFrameClose
	binary.BigEndian.PutUint32(pkt[1:5], uint32(length))
	pkt[5] = 'S' // statement (not portal)
	copy(pkt[6:], name)
	pkt[6+len(name)] = 0
	return pkt
}
