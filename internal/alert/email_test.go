package alert

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildEmailMessage(t *testing.T) {
	e := Event{Target: "Cloudflare/443", Alert: "loss", Firing: true, LossPct: 50, RTTms: 12.3, When: testWhen, Comment: "high loss on the uplink"}
	msg := string(buildEmailMessage(e, "smokeping@example.com", []string{"ops@example.com", "oncall@example.com"}))
	for _, want := range []string{
		"From: smokeping@example.com",
		"To: ops@example.com, oncall@example.com",
		"Subject: [FIRING] loss on Cloudflare/443",
		"Content-Type: text/plain",
		"high loss on the uplink", // the body carries the full alert message
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("email message missing %q\n---\n%s", want, msg)
		}
	}
	// A well-formed message separates headers from body with a blank CRLF line.
	if !strings.Contains(msg, "\r\n\r\n") {
		t.Errorf("headers and body must be separated by CRLF CRLF\n---\n%s", msg)
	}
}

func TestEmailNotifierSends(t *testing.T) {
	var mu sync.Mutex
	var captured [][]byte
	var gotFrom string
	var gotTo []string
	fake := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, msg)
		gotFrom, gotTo = from, to
		return nil
	}
	n := newEmailNotifier(EmailConfig{Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x", "b@x"}, QueueSize: 4, Workers: 1}, fake)
	n.Notify(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	n.Close(context.Background()) // drains the queue, then stops the worker

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("want 1 message sent, got %d", len(captured))
	}
	if gotFrom != "s@x" || strings.Join(gotTo, ",") != "a@x,b@x" {
		t.Errorf("envelope from/to = %q / %v", gotFrom, gotTo)
	}
	if !strings.Contains(string(captured[0]), "[FIRING] loss on T") {
		t.Errorf("sent message body: %s", captured[0])
	}
	if s := n.Stats(); s.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", s.Delivered)
	}
}

// A send func that fails must be retried, and it must eventually deliver once it starts
// succeeding — mirrors TestWebhookRetriesThenDelivers.
func TestEmailNotifierRetriesThenDelivers(t *testing.T) {
	var attempts atomic.Int32
	send := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		if attempts.Add(1) <= 2 { // fail the first two attempts, then succeed
			return errors.New("temporary failure")
		}
		return nil
	}
	n := newEmailNotifier(EmailConfig{
		Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x"}, QueueSize: 4, Workers: 1,
		MaxAttempts: 4, BaseBackoff: time.Millisecond,
	}, send)
	n.Notify(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	n.Close(context.Background()) // blocks until the retries (tiny backoff) finish

	st := n.Stats()
	if st.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", st.Delivered)
	}
	if st.Retried != 2 {
		t.Errorf("Retried = %d, want 2", st.Retried)
	}
	if st.Failed != 0 {
		t.Errorf("Failed = %d, want 0", st.Failed)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two failures then success)", got)
	}
}

// A send func that always fails must be abandoned after MaxAttempts and counted as
// failed, not retried forever.
func TestEmailNotifierGivesUpAfterMaxAttempts(t *testing.T) {
	var attempts atomic.Int32
	send := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		attempts.Add(1)
		return errors.New("permanent failure")
	}
	n := newEmailNotifier(EmailConfig{
		Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x"}, QueueSize: 4, Workers: 1,
		MaxAttempts: 3, BaseBackoff: time.Millisecond,
	}, send)
	n.Notify(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	n.Close(context.Background())

	st := n.Stats()
	if st.Failed != 1 {
		t.Errorf("Failed = %d, want 1", st.Failed)
	}
	if st.Retried != 2 { // MaxAttempts-1: every attempt but the last is followed by a retry
		t.Errorf("Retried = %d, want 2", st.Retried)
	}
	if st.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0", st.Delivered)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (MaxAttempts)", got)
	}
}

// A relay that accepts the TCP connection but never speaks must not hang the sender
// forever: the real smtpSender (not the injected fake) must time out.
func TestSMTPSenderDialTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c // held open, never spoken to: the client stalls on the 220 greeting
		}
	}()

	const timeout = 150 * time.Millisecond
	send := smtpSender(false, timeout)
	start := time.Now()
	err = send(l.Addr().String(), nil, "f@x", []string{"t@x"}, []byte("msg"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("smtpSender returned nil error against a relay that never speaks; want a timeout error")
	}
	if elapsed > 10*timeout {
		t.Errorf("smtpSender took %v to fail, want roughly the %v deadline", elapsed, timeout)
	}

	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(time.Second):
	}
}

// A relay that authenticates is configured (smtp.Auth != nil) but whose EHLO response
// doesn't list AUTH must fail explicitly instead of silently sending unauthenticated.
// This drives the real smtpSender against a minimal scripted SMTP server.
func TestSMTPSenderAuthNotOffered(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		write := func(s string) error {
			_, err := conn.Write([]byte(s))
			return err
		}
		if err := write("220 test.local ESMTP\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err := r.ReadString('\n')
		if err != nil {
			serverErr <- fmt.Errorf("reading EHLO: %w", err)
			return
		}
		if !strings.HasPrefix(strings.ToUpper(line), "EHLO") {
			serverErr <- fmt.Errorf("expected EHLO, got %q", line)
			return
		}
		// Single-line 250 response: no STARTTLS, no AUTH advertised.
		serverErr <- write("250 test.local\r\n")
	}()

	auth := smtp.PlainAuth("", "u", "p", "127.0.0.1")
	send := smtpSender(false, 2*time.Second)
	err = send(l.Addr().String(), auth, "f@x", []string{"t@x"}, []byte("msg"))
	if err == nil || !strings.Contains(err.Error(), "does not advertise AUTH") {
		t.Fatalf("err = %v, want the \"does not advertise AUTH\" error", err)
	}

	select {
	case serr := <-serverErr:
		if serr != nil {
			t.Fatalf("scripted server error: %v", serr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scripted server did not finish handling the connection")
	}
}
