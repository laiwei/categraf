package l7

import (
	"bytes"
)

// HTTP 方法列表
var httpMethods = map[string]struct{}{
	"GET":     {},
	"POST":    {},
	"PUT":     {},
	"DELETE":  {},
	"HEAD":    {},
	"OPTIONS": {},
	"PATCH":   {},
	"CONNECT": {},
}

var space = []byte{' '}

// ParseHTTP 从 eBPF 截获的请求有效载荷中解析 HTTP 方法和 URI。
// 返回 (method, path)；若解析失败，两者均为空字符串。
func ParseHTTP(payload []byte) (string, string) {
	method, rest, ok := bytes.Cut(payload, space)
	if !ok {
		return "", ""
	}
	if !IsHTTPMethod(string(method)) {
		return "", ""
	}
	uri, _, ok := bytes.Cut(rest, space)
	if !ok {
		// 不完整的请求行，截断标记
		uri = append(uri, []byte("...")...)
	}
	return string(method), string(uri)
}

// IsHTTPMethod 判断给定字符串是否是标准 HTTP 方法
func IsHTTPMethod(m string) bool {
	_, ok := httpMethods[m]
	return ok
}

// ParseHTTPStatus 从 HTTP 响应首行提取状态码。
// 格式: "HTTP/1.1 200 OK"
// 返回 0 表示无法解析。
func ParseHTTPStatus(payload []byte) Status {
	// 最短合法响应: "HTTP/X.Y ZZZ" = 12 字节
	if len(payload) < 12 {
		return StatusUnknown
	}

	// 检查 "HTTP/" 前缀
	if payload[0] != 'H' || payload[1] != 'T' || payload[2] != 'T' ||
		payload[3] != 'P' || payload[4] != '/' {
		return StatusUnknown
	}

	// 跳过版本号 "X.Y "
	if payload[5] < '0' || payload[5] > '9' {
		return StatusUnknown
	}
	if payload[6] != '.' {
		return StatusUnknown
	}
	if payload[7] < '0' || payload[7] > '9' {
		return StatusUnknown
	}
	if payload[8] != ' ' {
		return StatusUnknown
	}

	// 解析三位状态码
	d1, d2, d3 := payload[9], payload[10], payload[11]
	if d1 < '0' || d1 > '9' || d2 < '0' || d2 > '9' || d3 < '0' || d3 > '9' {
		return StatusUnknown
	}

	code := int32(d1-'0')*100 + int32(d2-'0')*10 + int32(d3-'0')
	return Status(code)
}

// IsHTTPRequest 判断有效载荷是否以标准 HTTP 请求方法开头
func IsHTTPRequest(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}

	// 快速检查首字母
	switch payload[0] {
	case 'G': // GET
		return len(payload) >= 4 && payload[1] == 'E' && payload[2] == 'T' && payload[3] == ' '
	case 'P': // POST, PUT, PATCH
		if len(payload) >= 5 && payload[1] == 'O' && payload[2] == 'S' && payload[3] == 'T' && payload[4] == ' ' {
			return true
		}
		if len(payload) >= 4 && payload[1] == 'U' && payload[2] == 'T' && payload[3] == ' ' {
			return true
		}
		if len(payload) >= 6 && payload[1] == 'A' && payload[2] == 'T' && payload[3] == 'C' && payload[4] == 'H' && payload[5] == ' ' {
			return true
		}
		return false
	case 'H': // HEAD
		return len(payload) >= 5 && payload[1] == 'E' && payload[2] == 'A' && payload[3] == 'D' && payload[4] == ' '
	case 'D': // DELETE
		return len(payload) >= 7 && payload[1] == 'E' && payload[2] == 'L' && payload[3] == 'E' && payload[4] == 'T' && payload[5] == 'E' && payload[6] == ' '
	case 'O': // OPTIONS
		return len(payload) >= 8 && payload[1] == 'P' && payload[2] == 'T' && payload[3] == 'I' && payload[4] == 'O' && payload[5] == 'N' && payload[6] == 'S' && payload[7] == ' '
	case 'C': // CONNECT
		return len(payload) >= 8 && payload[1] == 'O' && payload[2] == 'N' && payload[3] == 'N' && payload[4] == 'E' && payload[5] == 'C' && payload[6] == 'T' && payload[7] == ' '
	}
	return false
}

// IsHTTPResponse 判断有效载荷是否以 "HTTP/" 响应前缀开头
func IsHTTPResponse(payload []byte) bool {
	if len(payload) < 12 {
		return false
	}
	return payload[0] == 'H' && payload[1] == 'T' && payload[2] == 'T' &&
		payload[3] == 'P' && payload[4] == '/'
}
