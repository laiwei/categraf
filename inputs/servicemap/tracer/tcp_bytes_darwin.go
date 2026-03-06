//go:build darwin

package tracer

import (
	"bufio"
	"log"
	"os/exec"
	"strconv"
	"strings"
)

// connByteKey identifies a TCP connection by its 4-tuple for byte stats lookup.
type connByteKey struct {
	SrcIP   string
	SrcPort uint16
	DstIP   string
	DstPort uint16
}

// connBytes holds per-connection byte counters from macOS netstat.
type connBytes struct {
	RxBytes uint64
	TxBytes uint64
}

// getPerConnByteStats runs `netstat -b -n -p tcp` on macOS and returns
// per-connection byte counters keyed by 4-tuple.
// Returns nil on error (caller should treat nil as "byte data unavailable").
//
// macOS `netstat -b` adds rxbytes/txbytes columns to each socket entry,
// providing the per-connection traffic data that gopsutil does not expose.
// This is the macOS equivalent of Linux NETLINK_INET_DIAG + tcp_info.
func getPerConnByteStats() map[connByteKey]connBytes {
	out, err := exec.Command("netstat", "-b", "-n", "-p", "tcp").Output()
	if err != nil {
		log.Printf("D! servicemap: netstat -b failed: %v", err)
		return nil
	}
	return parseNetstatDarwin(string(out))
}

// parseNetstatDarwin parses macOS `netstat -b -n -p tcp` output.
//
// Example output:
//
//	Active Internet connections
//	Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)          rxbytes      txbytes
//	tcp4       0      0  192.168.1.100.55555    1.2.3.4.443            ESTABLISHED         5056         1631
//	tcp6       0      0  ::1.9090               ::1.52543              ESTABLISHED        12345        67890
//
// Each line has at least 8 space-separated fields. The last two fields are rxbytes and txbytes.
// Addresses use the macOS format where IP and port are separated by the last dot.
func parseNetstatDarwin(output string) map[connByteKey]connBytes {
	result := make(map[connByteKey]connBytes)
	scanner := bufio.NewScanner(strings.NewReader(output))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		// Expect: proto recv-q send-q local_addr remote_addr state rxbytes txbytes
		// Minimum 8 fields for connections with byte data.
		if len(fields) < 8 {
			continue
		}

		proto := fields[0]
		if proto != "tcp4" && proto != "tcp6" && proto != "tcp46" {
			continue
		}

		// rxbytes and txbytes are always the last two fields.
		rxBytes, err1 := strconv.ParseUint(fields[len(fields)-2], 10, 64)
		txBytes, err2 := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}

		srcIP, srcPort, ok1 := parseDarwinNetstatAddr(fields[3])
		dstIP, dstPort, ok2 := parseDarwinNetstatAddr(fields[4])
		if !ok1 || !ok2 {
			continue
		}

		result[connByteKey{
			SrcIP:   srcIP,
			SrcPort: srcPort,
			DstIP:   dstIP,
			DstPort: dstPort,
		}] = connBytes{RxBytes: rxBytes, TxBytes: txBytes}
	}

	return result
}

// parseDarwinNetstatAddr parses a macOS netstat address like "192.168.1.100.55555"
// or "::1.9090" into (ip, port, ok). The last dot-separated component is always the port.
//
// Examples:
//
//	"192.168.1.100.55555" → ("192.168.1.100", 55555, true)
//	"::1.9090"            → ("::1", 9090, true)
//	"fe80::1%lo0.12345"   → ("fe80::1%lo0", 12345, true)
//	"*.*"                 → ("", 0, false)
func parseDarwinNetstatAddr(addr string) (string, uint16, bool) {
	lastDot := strings.LastIndex(addr, ".")
	if lastDot < 0 || lastDot == len(addr)-1 {
		return "", 0, false
	}

	ip := addr[:lastDot]
	portStr := addr[lastDot+1:]

	if portStr == "*" || ip == "*" {
		return "", 0, false
	}

	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, false
	}

	return ip, uint16(port), true
}
