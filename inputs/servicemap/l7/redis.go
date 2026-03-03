package l7

import (
	"bytes"
	"strconv"
)

var crlf = []byte{'\r', '\n'}

// ParseRedis 从 RESP (Redis Serialization Protocol) 载荷中解析命令和参数。
// 返回 (command, args)；例如 ("GET", "mykey") 或 ("LLEN", "mylist ...")。
// https://redis.io/docs/reference/protocol-spec/
func ParseRedis(payload []byte) (cmd string, args string) {
	var v, rest []byte
	var ok bool

	// RESP 数组格式: *N\r\n$L\r\nVALUE\r\n...
	v, rest, ok = bytes.Cut(payload, crlf)
	if !ok || !bytes.HasPrefix(v, []byte("*")) {
		return
	}
	arrayLen, err := strconv.ParseUint(string(v[1:]), 10, 32)
	if err != nil {
		return
	}

	readString := func() string {
		v, rest, ok = bytes.Cut(rest, crlf)
		if !ok || !bytes.HasPrefix(v, []byte("$")) {
			return ""
		}
		v, rest, ok = bytes.Cut(rest, crlf)
		if ok {
			return string(v)
		}
		return ""
	}

	cmd = readString()
	if cmd == "" {
		return
	}
	if arrayLen > 1 {
		args = readString()
		if arrayLen > 2 {
			args += " ..."
		}
	}
	return
}

// IsRedisQuery 判断载荷是否为 Redis RESP 请求
func IsRedisQuery(payload []byte) bool {
	if len(payload) < 5 {
		return false
	}
	// RESP array: *N\r\n...  where N is a digit
	if payload[0] != '*' || payload[1] < '0' || payload[1] > '9' {
		return false
	}
	// *3\r\n... (single digit)
	if payload[2] == '\r' && payload[3] == '\n' {
		return true
	}
	// *12\r\n... (double digit)
	if payload[2] >= '0' && payload[2] <= '9' && payload[3] == '\r' && payload[4] == '\n' {
		return true
	}
	return false
}
