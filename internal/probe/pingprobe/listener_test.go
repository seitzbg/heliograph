package pingprobe

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/net/icmp"
)

func TestOpenListenerFallbackOrder(t *testing.T) {
	var attempts []string
	fakeOK := &icmp.PacketConn{} // zero value is fine; we never use it
	// auto: datagram fails, raw succeeds → falls back, kind "raw", attempts [udp4, ip4:icmp]
	ln := func(network, addr string) (*icmp.PacketConn, error) {
		attempts = append(attempts, network)
		if network == "udp4" {
			return nil, errors.New("no ping_group_range")
		}
		return fakeOK, nil
	}
	conn, kind, err := openListener(ln, "auto", false)
	if err != nil || conn == nil || kind != "raw" {
		t.Fatalf("auto fallback: kind=%q err=%v", kind, err)
	}
	if len(attempts) != 2 || attempts[0] != "udp4" || attempts[1] != "ip4:icmp" {
		t.Fatalf("attempt order = %v", attempts)
	}
	// unprivileged: only udp attempted; failure → error mentioning ping_group_range, no raw attempt
	attempts = nil
	ln2 := func(network, addr string) (*icmp.PacketConn, error) {
		attempts = append(attempts, network)
		return nil, errors.New("x")
	}
	if _, _, err := openListener(ln2, "unprivileged", false); err == nil {
		t.Fatal("want error")
	}
	if len(attempts) != 1 || attempts[0] != "udp4" {
		t.Fatalf("unprivileged attempts=%v", attempts)
	}
	// v6 datagram network is udp6
	attempts = nil
	openListener(func(n, a string) (*icmp.PacketConn, error) { attempts = append(attempts, n); return fakeOK, nil }, "unprivileged", true)
	if attempts[0] != "udp6" {
		t.Fatalf("v6 network=%v", attempts)
	}
}

// TestOpenListenerAllFailMentionsBothRemedies verifies the error returned when every attempt fails
// names both operator remedies portably — unprivileged datagram sockets and raw sockets — and does
// not present the Linux sysctl as the only fix. An operator hitting this on Linux, macOS, or FreeBSD
// should learn what to try next without reading source code. FreeBSD in particular has no
// unprivileged ICMP datagram socket, so its remedy (privileged mode / the FPing probe) must appear.
func TestOpenListenerAllFailMentionsBothRemedies(t *testing.T) {
	ln := func(network, addr string) (*icmp.PacketConn, error) {
		return nil, errors.New("permission denied")
	}
	_, _, err := openListener(ln, "auto", false)
	if err == nil {
		t.Fatal("want error when all attempts fail")
	}
	msg := err.Error()
	// Unprivileged-datagram remedy, with the Linux sysctl as a hint (not the only fix).
	if !strings.Contains(msg, "ping_group_range") {
		t.Errorf("error %q does not mention ping_group_range", msg)
	}
	// Raw-socket remedy.
	if !strings.Contains(msg, "CAP_NET_RAW") {
		t.Errorf("error %q does not mention CAP_NET_RAW", msg)
	}
	// OS-aware: FreeBSD's remedy differs (no unprivileged ICMP), so it must be named.
	if !strings.Contains(msg, "FreeBSD") {
		t.Errorf("error %q is not OS-aware: does not name the FreeBSD remedy", msg)
	}
	if !strings.Contains(msg, "permission denied") {
		t.Errorf("error %q does not include last underlying error", msg)
	}
}
