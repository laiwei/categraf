//go:build darwin

package tracer

import (
	"testing"
)

func TestParseNetstatDarwin(t *testing.T) {
	input := `Active Internet connections
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)          rxbytes      txbytes
tcp4       0      0  192.168.1.100.55555    1.2.3.4.443            ESTABLISHED         5056         1631
tcp4       0      0  127.0.0.1.7890         127.0.0.1.59909        ESTABLISHED         4102        13555
tcp6       0      0  ::1.9090               ::1.52543              ESTABLISHED        12345        67890
tcp4       0      0  *.80                   *.*                    LISTEN               123          456`

	result := parseNetstatDarwin(input)

	// *.* remote address should be skipped (LISTEN socket with wildcard)
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}

	// Verify IPv4 entry
	key1 := connByteKey{SrcIP: "192.168.1.100", SrcPort: 55555, DstIP: "1.2.3.4", DstPort: 443}
	bs1, ok := result[key1]
	if !ok {
		t.Fatal("expected entry for 192.168.1.100:55555 -> 1.2.3.4:443")
	}
	if bs1.RxBytes != 5056 || bs1.TxBytes != 1631 {
		t.Errorf("expected rx=5056 tx=1631, got rx=%d tx=%d", bs1.RxBytes, bs1.TxBytes)
	}

	// Verify loopback entry
	key2 := connByteKey{SrcIP: "127.0.0.1", SrcPort: 7890, DstIP: "127.0.0.1", DstPort: 59909}
	bs2, ok := result[key2]
	if !ok {
		t.Fatal("expected entry for 127.0.0.1:7890 -> 127.0.0.1:59909")
	}
	if bs2.RxBytes != 4102 || bs2.TxBytes != 13555 {
		t.Errorf("expected rx=4102 tx=13555, got rx=%d tx=%d", bs2.RxBytes, bs2.TxBytes)
	}

	// Verify IPv6 entry
	key3 := connByteKey{SrcIP: "::1", SrcPort: 9090, DstIP: "::1", DstPort: 52543}
	bs3, ok := result[key3]
	if !ok {
		t.Fatal("expected entry for ::1:9090 -> ::1:52543")
	}
	if bs3.RxBytes != 12345 || bs3.TxBytes != 67890 {
		t.Errorf("expected rx=12345 tx=67890, got rx=%d tx=%d", bs3.RxBytes, bs3.TxBytes)
	}
}

func TestParseNetstatDarwin_EmptyOutput(t *testing.T) {
	result := parseNetstatDarwin("")
	if len(result) != 0 {
		t.Errorf("expected 0 entries from empty output, got %d", len(result))
	}
}

func TestParseNetstatDarwin_MalformedLines(t *testing.T) {
	input := `Active Internet connections
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  192.168.1.100.55555    1.2.3.4.443            ESTABLISHED
tcp4       0      0  badaddr                badaddr2               ESTABLISHED       100       200
udp4       0      0  192.168.1.100.55555    1.2.3.4.443            ESTABLISHED       100       200`

	result := parseNetstatDarwin(input)
	// Line 1: no rxbytes/txbytes (only 7 fields)
	// Line 2: badaddr has no dot → parseDarwinNetstatAddr fails
	// Line 3: udp4 is not tcp → skipped
	if len(result) != 0 {
		t.Errorf("expected 0 valid entries from malformed output, got %d", len(result))
	}
}

func TestParseDarwinNetstatAddr(t *testing.T) {
	tests := []struct {
		addr     string
		wantIP   string
		wantPort uint16
		wantOK   bool
	}{
		{"192.168.1.100.55555", "192.168.1.100", 55555, true},
		{"127.0.0.1.7890", "127.0.0.1", 7890, true},
		{"10.0.0.1.443", "10.0.0.1", 443, true},
		{"::1.9090", "::1", 9090, true},
		{"fe80::1%lo0.12345", "fe80::1%lo0", 12345, true},
		{"*.*", "", 0, false},
		{"*.80", "", 0, false},
		{"", "", 0, false},
		{"noport", "", 0, false},       // no dot at all
		{"192.168.1.1.", "", 0, false}, // trailing dot, empty port
	}

	for _, tt := range tests {
		ip, port, ok := parseDarwinNetstatAddr(tt.addr)
		if ok != tt.wantOK {
			t.Errorf("parseDarwinNetstatAddr(%q): ok=%v, want %v", tt.addr, ok, tt.wantOK)
			continue
		}
		if ok && (ip != tt.wantIP || port != tt.wantPort) {
			t.Errorf("parseDarwinNetstatAddr(%q): got (%q, %d), want (%q, %d)",
				tt.addr, ip, port, tt.wantIP, tt.wantPort)
		}
	}
}

func TestGetPerConnByteStats_Integration(t *testing.T) {
	// Integration test: actually run netstat -b and verify we get data.
	stats := getPerConnByteStats()
	if stats == nil {
		t.Skip("netstat -b not available")
	}

	t.Logf("got %d connection byte entries from netstat -b", len(stats))

	// On a running macOS system, there should be at least a few TCP connections.
	if len(stats) == 0 {
		t.Log("warning: no TCP connections found (machine may have no active connections)")
		return
	}

	// Verify that at least some entries have non-zero byte counts.
	hasNonZero := false
	for key, bs := range stats {
		if bs.RxBytes > 0 || bs.TxBytes > 0 {
			t.Logf("  sample: %s:%d -> %s:%d  rx=%d tx=%d",
				key.SrcIP, key.SrcPort, key.DstIP, key.DstPort, bs.RxBytes, bs.TxBytes)
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Error("all connections have zero bytes, netstat -b parsing may be wrong")
	}
}
