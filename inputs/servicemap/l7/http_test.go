package l7

import (
	"testing"
)

func TestParseHTTP(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantMethod string
		wantPath   string
	}{
		{
			name:       "GET request",
			payload:    "GET /api/v1/status HTTP/1.1\r\nHost: localhost\r\n\r\n",
			wantMethod: "GET",
			wantPath:   "/api/v1/status",
		},
		{
			name:       "POST request",
			payload:    "POST /api/v1/data HTTP/1.1\r\nContent-Type: application/json\r\n\r\n{\"key\":\"value\"}",
			wantMethod: "POST",
			wantPath:   "/api/v1/data",
		},
		{
			name:       "HEAD request",
			payload:    "HEAD /health HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n",
			wantMethod: "HEAD",
			wantPath:   "/health",
		},
		{
			name:       "PUT request",
			payload:    "PUT /resource/1 HTTP/1.1\r\n\r\n",
			wantMethod: "PUT",
			wantPath:   "/resource/1",
		},
		{
			name:       "DELETE request",
			payload:    "DELETE /resource/1 HTTP/1.1\r\n\r\n",
			wantMethod: "DELETE",
			wantPath:   "/resource/1",
		},
		{
			name:       "PATCH request",
			payload:    "PATCH /resource/1 HTTP/1.1\r\n\r\n",
			wantMethod: "PATCH",
			wantPath:   "/resource/1",
		},
		{
			name:       "OPTIONS request",
			payload:    "OPTIONS / HTTP/1.1\r\n\r\n",
			wantMethod: "OPTIONS",
			wantPath:   "/",
		},
		{
			name:       "truncated request line",
			payload:    "GET /very-long-uri-without-version",
			wantMethod: "GET",
			wantPath:   "/very-long-uri-without-version...",
		},
		{
			name:       "empty payload",
			payload:    "",
			wantMethod: "",
			wantPath:   "",
		},
		{
			name:       "not HTTP",
			payload:    "\x16\x03\x01\x00\xf1",
			wantMethod: "",
			wantPath:   "",
		},
		{
			name:       "invalid method",
			payload:    "FOOBAR /path HTTP/1.1\r\n",
			wantMethod: "",
			wantPath:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, path := ParseHTTP([]byte(tt.payload))
			if method != tt.wantMethod {
				t.Errorf("method = %q, want %q", method, tt.wantMethod)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

func TestIsHTTPRequest(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{"GET", "GET / HTTP/1.1\r\n", true},
		{"POST", "POST /api HTTP/1.1\r\n", true},
		{"PUT", "PUT /res HTTP/1.1\r\n", true},
		{"DELETE", "DELETE /res HTTP/1.1\r\n", true},
		{"HEAD", "HEAD / HTTP/1.1\r\n", true},
		{"OPTIONS", "OPTIONS / HTTP/1.1\r\n", true},
		{"PATCH", "PATCH /res HTTP/1.1\r\n", true},
		{"CONNECT", "CONNECT host:443 HTTP/1.1\r\n", true},
		{"TLS", "\x16\x03\x01\x00", false},
		{"empty", "", false},
		{"short", "GE", false},
		{"mysql", "\x45\x00\x00\x00\x03", false},
		{"redis", "*3\r\n$3\r\nSET\r\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHTTPRequest([]byte(tt.payload))
			if got != tt.want {
				t.Errorf("IsHTTPRequest(%q) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
}

func TestIsHTTPResponse(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{"200 OK", "HTTP/1.1 200 OK\r\n", true},
		{"404 Not Found", "HTTP/1.0 404 Not Found\r\n", true},
		{"500", "HTTP/1.1 500 Internal Server Error\r\n", true},
		{"too short", "HTTP/1.", false},
		{"not HTTP", "GET / HTTP/1.1", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHTTPResponse([]byte(tt.payload))
			if got != tt.want {
				t.Errorf("IsHTTPResponse(%q) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
}

func TestParseHTTPStatus(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    Status
	}{
		{"200 OK", "HTTP/1.1 200 OK\r\n", 200},
		{"404 Not Found", "HTTP/1.0 404 Not Found\r\n", 404},
		{"500 Internal", "HTTP/1.1 500 Internal Server Error\r\n", 500},
		{"301 Redirect", "HTTP/1.1 301 Moved\r\n", 301},
		{"too short", "HTTP/1.1 ", StatusUnknown},
		{"not HTTP", "BLAH", StatusUnknown},
		{"empty", "", StatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseHTTPStatus([]byte(tt.payload))
			if got != tt.want {
				t.Errorf("ParseHTTPStatus(%q) = %d, want %d", tt.payload, got, tt.want)
			}
		})
	}
}

func TestStatus_HTTPStatusClass(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{100, "1xx"},
		{200, "2xx"},
		{201, "2xx"},
		{301, "3xx"},
		{400, "4xx"},
		{404, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{0, "unknown"},
		{-1, "unknown"},
		{600, "unknown"},
	}

	for _, tt := range tests {
		got := tt.status.HTTPStatusClass()
		if got != tt.want {
			t.Errorf("Status(%d).HTTPStatusClass() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestStatus_IsError(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{200, false},
		{201, false},
		{301, false},
		{400, true},
		{404, true},
		{500, true},
		{0, false},
	}

	for _, tt := range tests {
		got := tt.status.IsError()
		if got != tt.want {
			t.Errorf("Status(%d).IsError() = %v, want %v", tt.status, got, tt.want)
		}
	}
}
