// Package pingprobe implements a native ICMP echo ("Ping") probe using
// golang.org/x/net/icmp. This file holds the pure, network-free core:
// building an ICMP echo request and matching a parsed reply against the
// outstanding request state. No sockets are touched here.
package pingprobe

import (
	"bytes"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// echoProto returns the ICMP protocol number for icmp.ParseMessage (1 for v4, 58 for v6).
func echoProto(isV6 bool) int {
	if isV6 {
		return 58
	}
	return 1
}

// echoType returns the request type to send (ipv4.ICMPTypeEcho / ipv6.ICMPTypeEchoRequest).
func echoType(isV6 bool) icmp.Type {
	if isV6 {
		return ipv6.ICMPTypeEchoRequest
	}
	return ipv4.ICMPTypeEcho
}

// buildEcho marshals an ICMP Echo request. Data = token followed by zero padding so the total ICMP
// payload length == packetsize (clamped to >= len(token)). id/seq identify the echo.
func buildEcho(isV6 bool, id, seq, packetsize int, token []byte) ([]byte, error) {
	data := make([]byte, max(packetsize, len(token)))
	copy(data, token) // token at the front; rest zero padding
	msg := icmp.Message{
		Type: echoType(isV6),
		Code: 0,
		Body: &icmp.Echo{ID: id, Seq: seq, Data: data},
	}
	return msg.Marshal(nil)
}

// matchReply inspects a parsed ICMP message and, if it is an Echo *Reply* (v4 or v6) whose payload
// begins with wantToken and whose Seq is in [0,pings) and not already in seen, returns (seq, true).
// Everything else (echo request, dest-unreachable, wrong token, out-of-range/duplicate seq, other
// types) → (0,false). (No isV6 param: it accepts either family's reply type, and a v4/v6 socket only
// ever delivers its own family's replies.)
func matchReply(msg *icmp.Message, wantToken []byte, pings int, seen map[int]bool) (int, bool) {
	isReply := msg.Type == ipv4.ICMPTypeEchoReply || msg.Type == ipv6.ICMPTypeEchoReply
	if !isReply {
		return 0, false
	}
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok {
		return 0, false
	}
	if echo.Seq < 0 || echo.Seq >= pings || seen[echo.Seq] {
		return 0, false
	}
	if !bytes.HasPrefix(echo.Data, wantToken) {
		return 0, false
	}
	return echo.Seq, true
}
