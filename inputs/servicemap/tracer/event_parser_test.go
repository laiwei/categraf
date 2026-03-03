package tracer

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseRawEvent_IPv4(t *testing.T) {
	raw := rawEvent{
		Timestamp: 123456789,
		Type:      uint32(EventTypeConnectionOpen),
		Pid:       1234,
		Fd:        5678,
		SrcAddr:   [4]uint32{0x0100007f, 0, 0, 0}, // 127.0.0.1 in little-endian
		DstAddr:   [4]uint32{0x0101a8c0, 0, 0, 0}, // 192.168.1.1 in little-endian
		SrcPort:   12345,
		DstPort:   0x5000, // 80 in network byte order (ntohs)
		Family:    afINET,
		Padding:   0,
		BytesSent: 1024,
		BytesRecv: 2048,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, &raw); err != nil {
		t.Fatalf("write raw event failed: %v", err)
	}

	event, err := parseRawEvent(buf.Bytes())
	if err != nil {
		t.Fatalf("parse raw event failed: %v", err)
	}

	if event.Type != EventTypeConnectionOpen {
		t.Errorf("expected Type %v, got %v", EventTypeConnectionOpen, event.Type)
	}
	if event.Pid != 1234 {
		t.Errorf("expected Pid 1234, got %d", event.Pid)
	}
	if event.SrcAddr != "127.0.0.1:12345" {
		t.Errorf("expected SrcAddr 127.0.0.1:12345, got %s", event.SrcAddr)
	}
	if event.DstAddr != "192.168.1.1:80" {
		t.Errorf("expected DstAddr 192.168.1.1:80, got %s", event.DstAddr)
	}
	if event.SrcPort != 12345 {
		t.Errorf("expected SrcPort 12345, got %d", event.SrcPort)
	}
	if event.DstPort != 80 {
		t.Errorf("expected DstPort 80, got %d", event.DstPort)
	}
}

func TestParseRawEvent_IPv6(t *testing.T) {
	// IPv6 localhost ::1
	raw := rawEvent{
		Timestamp: 987654321,
		Type:      uint32(EventTypeConnectionClose),
		Pid:       5678,
		Fd:        9012,
		SrcAddr:   [4]uint32{0, 0, 0, 0x01000000}, // ::1 in little-endian per uint32
		DstAddr:   [4]uint32{0x20010db8, 0, 0, 0x00000001},
		SrcPort:   54321,
		DstPort:   0xbb01, // 443 in network byte order
		Family:    afINET6,
		Padding:   0,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, &raw); err != nil {
		t.Fatalf("write raw event failed: %v", err)
	}

	event, err := parseRawEvent(buf.Bytes())
	if err != nil {
		t.Fatalf("parse raw event failed: %v", err)
	}

	if event.Type != EventTypeConnectionClose {
		t.Errorf("expected Type %v, got %v", EventTypeConnectionClose, event.Type)
	}
	if event.Pid != 5678 {
		t.Errorf("expected Pid 5678, got %d", event.Pid)
	}
	if event.SrcAddr != "[::1]:54321" {
		t.Errorf("expected SrcAddr [::1]:54321, got %s", event.SrcAddr)
	}
	if event.DstPort != 443 {
		t.Errorf("expected DstPort 443, got %d", event.DstPort)
	}
}

func TestParseRawEvent_TooShort(t *testing.T) {
	data := []byte{1, 2, 3}
	_, err := parseRawEvent(data)
	if err == nil {
		t.Error("expected error for too short data, got nil")
	}
}

func TestIntToIPv4(t *testing.T) {
	tests := []struct {
		input uint32
		want  string
	}{
		{0x0100007f, "127.0.0.1"},
		{0x0101a8c0, "192.168.1.1"},
		{0x08080808, "8.8.8.8"},
	}

	for _, tc := range tests {
		got := intToIPv4(tc.input).String()
		if got != tc.want {
			t.Errorf("intToIPv4(0x%08x) = %s, want %s", tc.input, got, tc.want)
		}
	}
}

func TestNtohs(t *testing.T) {
	tests := []struct {
		input uint16
		want  uint16
	}{
		{0x5000, 80},
		{0xbb01, 443},
		{0x5c11, 4444},
	}

	for _, tc := range tests {
		got := ntohs(tc.input)
		if got != tc.want {
			t.Errorf("ntohs(0x%04x) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// ============================================================
// P2-9: L7 事件解析测试
// ============================================================

func TestParseRawL7Event_HTTP(t *testing.T) {
	payload := []byte("GET /api/v1/health HTTP/1.1\r\n")

	raw := rawL7Event{
		Fd:                  42,
		ConnectionTimestamp: 123456789,
		Pid:                 1234,
		Status:              200,
		Duration:            5000000, // 5ms in nanoseconds
		Protocol:            1,       // ProtocolHTTP
		Method:              0,
		Padding:             0,
		StatementId:         0,
		PayloadSize:         uint64(len(payload)),
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, &raw); err != nil {
		t.Fatalf("write raw L7 event failed: %v", err)
	}
	buf.Write(payload)

	event, err := parseRawL7Event(buf.Bytes())
	if err != nil {
		t.Fatalf("parse raw L7 event failed: %v", err)
	}

	if event.Type != EventTypeL7Request {
		t.Errorf("expected Type %v, got %v", EventTypeL7Request, event.Type)
	}
	if event.Pid != 1234 {
		t.Errorf("expected Pid 1234, got %d", event.Pid)
	}
	if event.Fd != 42 {
		t.Errorf("expected Fd 42, got %d", event.Fd)
	}
	if event.L7Request == nil {
		t.Fatal("L7Request is nil")
	}
	if event.L7Request.Status != 200 {
		t.Errorf("expected Status 200, got %d", event.L7Request.Status)
	}
	if event.L7Request.Protocol != 1 {
		t.Errorf("expected Protocol HTTP(1), got %d", event.L7Request.Protocol)
	}
	if string(event.L7Request.Payload) != string(payload) {
		t.Errorf("payload mismatch: got %q", event.L7Request.Payload)
	}
}

func TestParseRawL7Event_TooShort(t *testing.T) {
	data := []byte{1, 2, 3}
	_, err := parseRawL7Event(data)
	if err == nil {
		t.Error("expected error for too short data, got nil")
	}
}

func TestParseRawL7Event_NoPayload(t *testing.T) {
	raw := rawL7Event{
		Fd:                  10,
		ConnectionTimestamp: 999,
		Pid:                 100,
		Status:              500,
		Duration:            1000000,
		Protocol:            1,
		Method:              0,
		PayloadSize:         0,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, &raw); err != nil {
		t.Fatalf("write raw L7 event failed: %v", err)
	}

	event, err := parseRawL7Event(buf.Bytes())
	if err != nil {
		t.Fatalf("parse raw L7 event failed: %v", err)
	}

	if event.L7Request == nil {
		t.Fatal("L7Request is nil")
	}
	if len(event.L7Request.Payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(event.L7Request.Payload))
	}
}
