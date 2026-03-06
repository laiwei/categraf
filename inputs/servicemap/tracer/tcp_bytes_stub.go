//go:build !darwin

package tracer

// connByteKey identifies a TCP connection by its 4-tuple for byte stats lookup.
type connByteKey struct {
	SrcIP   string
	SrcPort uint16
	DstIP   string
	DstPort uint16
}

// connBytes holds per-connection byte counters.
type connBytes struct {
	RxBytes uint64
	TxBytes uint64
}

// getPerConnByteStats is a no-op on non-macOS platforms.
// On Linux, byte counters are provided by NETLINK_INET_DIAG (tcp_info);
// on other platforms, per-connection byte data is unavailable via polling.
func getPerConnByteStats() map[connByteKey]connBytes {
	return nil
}
