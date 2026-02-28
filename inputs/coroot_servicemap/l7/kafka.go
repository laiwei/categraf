package l7

import (
	"encoding/binary"
)

// Kafka API Keys (常用)
const (
	KafkaAPIProduce         int16 = 0
	KafkaAPIFetch           int16 = 1
	KafkaAPIListOffsets     int16 = 2
	KafkaAPIMetadata        int16 = 3
	KafkaAPIOffsetCommit    int16 = 8
	KafkaAPIOffsetFetch     int16 = 9
	KafkaAPIFindCoordinator int16 = 10
	KafkaAPIJoinGroup       int16 = 11
	KafkaAPIHeartbeat       int16 = 12
	KafkaAPILeaveGroup      int16 = 13
	KafkaAPISyncGroup       int16 = 14
	KafkaAPIMaxKnown        int16 = 67
)

var kafkaAPINames = map[int16]string{
	0:  "Produce",
	1:  "Fetch",
	2:  "ListOffsets",
	3:  "Metadata",
	8:  "OffsetCommit",
	9:  "OffsetFetch",
	10: "FindCoordinator",
	11: "JoinGroup",
	12: "Heartbeat",
	13: "LeaveGroup",
	14: "SyncGroup",
	18: "ApiVersions",
	19: "CreateTopics",
	20: "DeleteTopics",
	36: "SaslAuthenticate",
}

// KafkaAPIKeyName 返回 Kafka API key 的可读名称
func KafkaAPIKeyName(key int16) string {
	if name, ok := kafkaAPINames[key]; ok {
		return name
	}
	return ""
}

// IsKafkaRequest 判断载荷是否为 Kafka 请求。
// Kafka 协议格式: length(4 bytes, big-endian) + api_key(2) + api_version(2) + correlation_id(4)
func IsKafkaRequest(payload []byte) bool {
	if len(payload) < 12 {
		return false
	}
	// 前 4 字节为消息长度 (big-endian)
	length := int(binary.BigEndian.Uint32(payload[:4]))
	if length <= 0 || length+4 < 12 {
		return false
	}
	// api_key: 0..67 (已知范围)
	apiKey := int16(binary.BigEndian.Uint16(payload[4:6]))
	if apiKey < 0 || apiKey > KafkaAPIMaxKnown {
		return false
	}
	// api_version: 不会超过 20
	apiVersion := int16(binary.BigEndian.Uint16(payload[6:8]))
	if apiVersion < 0 || apiVersion > 20 {
		return false
	}
	// correlation_id 通常 > 0
	correlationID := int32(binary.BigEndian.Uint32(payload[8:12]))
	return correlationID > 0
}
