package l7

import (
	"fmt"
	"testing"
)

func TestParseRedis(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		wantCmd  string
		wantArgs string
	}{
		{
			name:     "GET command",
			payload:  buildRedisCommand("GET", "mykey"),
			wantCmd:  "GET",
			wantArgs: "mykey",
		},
		{
			name:     "SET command with value",
			payload:  buildRedisCommand("SET", "mykey", "myvalue"),
			wantCmd:  "SET",
			wantArgs: "mykey ...",
		},
		{
			name:     "DEL command",
			payload:  buildRedisCommand("DEL", "key1"),
			wantCmd:  "DEL",
			wantArgs: "key1",
		},
		{
			name:     "PING no args",
			payload:  buildRedisCommand("PING"),
			wantCmd:  "PING",
			wantArgs: "",
		},
		{
			name:     "LRANGE with multiple args",
			payload:  buildRedisCommand("LRANGE", "mylist", "0", "-1"),
			wantCmd:  "LRANGE",
			wantArgs: "mylist ...",
		},
		{
			name:     "HSET hash",
			payload:  buildRedisCommand("HSET", "myhash", "field1", "value1"),
			wantCmd:  "HSET",
			wantArgs: "myhash ...",
		},
		{
			name:     "empty payload",
			payload:  []byte{},
			wantCmd:  "",
			wantArgs: "",
		},
		{
			name:     "invalid not RESP",
			payload:  []byte("NOT RESP"),
			wantCmd:  "",
			wantArgs: "",
		},
		{
			name:     "incomplete RESP",
			payload:  []byte("*2\r\n$3\r\n"),
			wantCmd:  "",
			wantArgs: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, args := ParseRedis(tt.payload)
			if cmd != tt.wantCmd {
				t.Errorf("cmd = %q, want %q", cmd, tt.wantCmd)
			}
			if args != tt.wantArgs {
				t.Errorf("args = %q, want %q", args, tt.wantArgs)
			}
		})
	}
}

func TestIsRedisQuery(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{
			name:    "GET command",
			payload: buildRedisCommand("GET", "key"),
			want:    true,
		},
		{
			name:    "SET with multiple args",
			payload: buildRedisCommand("SET", "key", "value"),
			want:    true,
		},
		{
			name:    "PING",
			payload: buildRedisCommand("PING"),
			want:    true,
		},
		{
			name:    "too short",
			payload: []byte("*1\r"),
			want:    false,
		},
		{
			name:    "not RESP",
			payload: []byte("HELLO WORLD!!"),
			want:    false,
		},
		{
			name:    "HTTP request",
			payload: []byte("GET /path HTTP/1.1\r\n"),
			want:    false,
		},
		{
			name:    "double-digit array length",
			payload: []byte("*12\r\n$4\r\nMGET\r\n"),
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRedisQuery(tt.payload)
			if got != tt.want {
				t.Errorf("IsRedisQuery() = %v, want %v", got, tt.want)
			}
		})
	}
}

// buildRedisCommand 构造 RESP 协议的命令数据。
// 格式: *N\r\n$L\r\nVALUE\r\n...
func buildRedisCommand(parts ...string) []byte {
	result := fmt.Sprintf("*%d\r\n", len(parts))
	for _, p := range parts {
		result += fmt.Sprintf("$%d\r\n%s\r\n", len(p), p)
	}
	return []byte(result)
}
