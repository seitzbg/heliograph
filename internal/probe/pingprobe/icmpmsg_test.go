package pingprobe

import (
	"bytes"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

func TestBuildEchoRoundTripAndPacketsize(t *testing.T) {
	token := []byte{1, 2, 3, 4}
	b, err := buildEcho(false, 0x1234, 7, 56, token)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := icmp.ParseMessage(echoProto(false), b)
	if err != nil {
		t.Fatal(err)
	}
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok {
		t.Fatalf("not an echo body: %T", msg.Body)
	}
	if echo.Seq != 7 {
		t.Errorf("seq=%d", echo.Seq)
	}
	if len(echo.Data) != 56 {
		t.Errorf("payload len=%d want 56", len(echo.Data))
	}
	if !bytes.HasPrefix(echo.Data, token) {
		t.Errorf("token not at payload start")
	}
}

func TestMatchReplyAcceptsAndRejects(t *testing.T) {
	token := []byte{9, 9, 9, 9}
	mk := func(typ icmp.Type, seq int, data []byte) *icmp.Message {
		return &icmp.Message{Type: typ, Code: 0, Body: &icmp.Echo{ID: 1, Seq: seq, Data: data}}
	}
	seen := map[int]bool{}
	// accept a valid reply
	if seq, ok := matchReply(mk(ipv4.ICMPTypeEchoReply, 3, token), token, 10, seen); !ok || seq != 3 {
		t.Fatalf("valid reply rejected: seq=%d ok=%v", seq, ok)
	}
	seen[3] = true
	// duplicate seq rejected
	if _, ok := matchReply(mk(ipv4.ICMPTypeEchoReply, 3, token), token, 10, seen); ok {
		t.Error("duplicate seq accepted")
	}
	// an Echo *Request* (not reply) rejected
	if _, ok := matchReply(mk(ipv4.ICMPTypeEcho, 4, token), token, 10, seen); ok {
		t.Error("echo request accepted")
	}
	// wrong token rejected
	if _, ok := matchReply(mk(ipv4.ICMPTypeEchoReply, 5, []byte{0, 0, 0, 0}), token, 10, seen); ok {
		t.Error("wrong token accepted")
	}
	// out-of-range seq rejected
	if _, ok := matchReply(mk(ipv4.ICMPTypeEchoReply, 99, token), token, 10, seen); ok {
		t.Error("out-of-range seq accepted")
	}
}
