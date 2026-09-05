package pingprobe

import (
	"fmt"

	"golang.org/x/net/icmp"
)

// listenFunc matches icmp.ListenPacket's signature (network, address) → (*icmp.PacketConn, error);
// injected so tests exercise the fallback ordering without real privileges.
type listenFunc func(network, address string) (*icmp.PacketConn, error)

// attempt pairs a network to try with the kind it yields on success.
type attempt struct {
	network string
	kind    string
}

// openListener opens an ICMP listener for the address family per mode, using ln. It returns the conn,
// the chosen kind ("datagram"|"raw" — the caller uses it to build the write destination), and an error
// naming both remedies if nothing opened. Order: unprivileged→[udp]; privileged→[ip]; auto→[udp, ip].
func openListener(ln listenFunc, mode string, isV6 bool) (conn *icmp.PacketConn, kind string, err error) {
	datagramNetwork, rawNetwork, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if isV6 {
		datagramNetwork, rawNetwork, addr = "udp6", "ip6:ipv6-icmp", "::"
	}
	datagram := attempt{datagramNetwork, "datagram"}
	raw := attempt{rawNetwork, "raw"}

	var attempts []attempt
	switch mode {
	case "unprivileged":
		attempts = []attempt{datagram}
	case "privileged":
		attempts = []attempt{raw}
	default: // "auto" and anything else
		attempts = []attempt{datagram, raw}
	}

	var lastErr error
	for _, a := range attempts {
		c, e := ln(a.network, addr)
		if e == nil {
			return c, a.kind, nil
		}
		lastErr = e
	}
	return nil, "", fmt.Errorf(
		"pingprobe: no ICMP listener available (mode=%s): tried and failed. To fix, either enable "+
			"unprivileged ICMP datagram sockets (Linux: sysctl net.ipv4.ping_group_range; macOS: enabled "+
			"by default; FreeBSD: unsupported — run as root with mode=privileged, or use the FPing probe), "+
			"or grant this process raw-socket privilege (Linux: CAP_NET_RAW; otherwise run as root): %w",
		mode, lastErr)
}
