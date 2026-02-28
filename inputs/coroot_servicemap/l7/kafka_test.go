package l7

import (
	"encoding/binary"
	"testing"
)

func TestIsKafkaRequest(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{
			name:    "Produce request",
			payload: buildKafkaRequest(KafkaAPIProduce, 9, 1),
			want:    true,
		},
		{
			name:    "Fetch request",
			payload: buildKafkaRequest(KafkaAPIFetch, 12, 2),
			want:    true,
		},
		{
			name:    "Metadata request",
			payload: buildKafkaRequest(KafkaAPIMetadata, 9, 3),
			want:    true,
		},
		{
			name:    "ApiVersions request",
			payload: buildKafkaRequest(18, 3, 100),
			want:    true,
		},
		{
			name:    "too short",
			payload: []byte{0, 0, 0, 8, 0, 0},
			want:    false,
		},
		{
			name:    "bad api key (too large)",
			payload: buildKafkaRequest(100, 0, 1),
			want:    false,
		},
		{
			name:    "bad api version",
			payload: buildKafkaRequest(0, 30, 1),
			want:    false,
		},
		{
			name:    "correlation_id = 0",
			payload: buildKafkaRequest(0, 0, 0),
			want:    false,
		},
		{
			name:    "not Kafka",
			payload: []byte("GET /health HTTP/1.1\r\n"),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsKafkaRequest(tt.payload)
			if got != tt.want {
				t.Errorf("IsKafkaRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKafkaAPIKeyName(t *testing.T) {
	tests := []struct {
		key  int16
		want string
	}{
		{0, "Produce"},
		{1, "Fetch"},
		{2, "ListOffsets"},
		{3, "Metadata"},
		{18, "ApiVersions"},
		{99, ""},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := KafkaAPIKeyName(tt.key)
			if got != tt.want {
				t.Errorf("KafkaAPIKeyName(%d) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// buildKafkaRequest 构造 Kafka 请求:
//
//	length(4) + api_key(2) + api_version(2) + correlation_id(4) + client_id_len(2)
func buildKafkaRequest(apiKey, apiVersion int16, correlationID int32) []byte {
	// body = api_key(2) + api_version(2) + correlation_id(4) + client_id_len(2) = 10
	bodyLen := 10
	pkt := make([]byte, 4+bodyLen)
	binary.BigEndian.PutUint32(pkt[0:4], uint32(bodyLen))
	binary.BigEndian.PutUint16(pkt[4:6], uint16(apiKey))
	binary.BigEndian.PutUint16(pkt[6:8], uint16(apiVersion))
	binary.BigEndian.PutUint32(pkt[8:12], uint32(correlationID))
	// client_id_len = 0
	binary.BigEndian.PutUint16(pkt[12:14], 0)
	return pkt
}
