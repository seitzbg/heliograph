package alert

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
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
	fake := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
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
	send := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
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
	send := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
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

// A permanent 5xx SMTP rejection (net/smtp surfaces server replies as *textproto.Error; Code >= 500
// is permanent — unknown recipient, relay denied) must be abandoned on the FIRST attempt: retrying
// it only burns the worker on a send that can never succeed. The attempt counter — not timing —
// catches a regression that retries, so a tiny BaseBackoff keeps the test fast either way.
func TestEmailNotifierPermanent5xxNoRetry(t *testing.T) {
	var attempts atomic.Int32
	send := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		attempts.Add(1)
		return &textproto.Error{Code: 550, Msg: "5.1.1 <a@x>: recipient address rejected: user unknown"}
	}
	n := newEmailNotifier(EmailConfig{
		Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x"}, QueueSize: 4, Workers: 1,
		MaxAttempts: 4, BaseBackoff: time.Millisecond,
	}, send)
	n.Notify(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	n.Close(context.Background())

	st := n.Stats()
	if st.Failed != 1 {
		t.Errorf("Failed = %d, want 1", st.Failed)
	}
	if st.Retried != 0 {
		t.Errorf("Retried = %d, want 0 (a 5xx rejection is permanent — never retried)", st.Retried)
	}
	if st.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0", st.Delivered)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (permanent 5xx must not be retried)", got)
	}
}

// The AUTH-not-advertised misconfiguration (errAuthNotOffered, wrapped by the real smtpSender) is
// permanent — no relay round-trip fixes it — so the notifier must give up on the first attempt,
// exactly like the 5xx case. Mirrors the sender-level TestSMTPSenderAuthNotOffered but at the
// deliver/retry layer.
func TestEmailNotifierAuthNotOfferedNoRetry(t *testing.T) {
	var attempts atomic.Int32
	send := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		attempts.Add(1)
		// The same wrapped sentinel the real smtpSender returns for this misconfiguration.
		return fmt.Errorf("smtp: authentication configured but server %q does not advertise AUTH (STARTTLS may be required): %w", addr, errAuthNotOffered)
	}
	n := newEmailNotifier(EmailConfig{
		Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x"}, QueueSize: 4, Workers: 1,
		Auth: smtp.PlainAuth("", "u", "p", "mail.example.com"), MaxAttempts: 4, BaseBackoff: time.Millisecond,
	}, send)
	n.Notify(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	n.Close(context.Background())

	st := n.Stats()
	if st.Failed != 1 {
		t.Errorf("Failed = %d, want 1", st.Failed)
	}
	if st.Retried != 0 {
		t.Errorf("Retried = %d, want 0 (the AUTH misconfiguration is permanent — never retried)", st.Retried)
	}
	if st.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0", st.Delivered)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (permanent AUTH error must not be retried)", got)
	}
}

// A 4xx SMTP reply is transient (greylisting, temporary local error), so it MUST still be retried
// and can deliver once the relay recovers — the complement of the 5xx no-retry case.
func TestEmailNotifier4xxRetriesThenDelivers(t *testing.T) {
	var attempts atomic.Int32
	send := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		if attempts.Add(1) <= 2 { // two transient 4xx rejections, then the relay accepts
			return &textproto.Error{Code: 451, Msg: "4.7.1 greylisted, please retry"}
		}
		return nil
	}
	n := newEmailNotifier(EmailConfig{
		Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x"}, QueueSize: 4, Workers: 1,
		MaxAttempts: 4, BaseBackoff: time.Millisecond,
	}, send)
	n.Notify(Event{Target: "T", Alert: "loss", Firing: true, When: testWhen})
	n.Close(context.Background())

	st := n.Stats()
	if st.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", st.Delivered)
	}
	if st.Retried != 2 {
		t.Errorf("Retried = %d, want 2 (a 4xx reply is transient — retried)", st.Retried)
	}
	if st.Failed != 0 {
		t.Errorf("Failed = %d, want 0", st.Failed)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (two transient 4xx failures then success)", got)
	}
}

// Close must not wait out a per-attempt timeout on a send that is stuck mid-transaction: once its
// deadline hits it force-cancels the in-flight send (via baseCtx) and counts it failed. Mirrors
// TestWebhookCloseCancelsInflightAtDeadline.
func TestEmailCloseCancelsInflightAtDeadline(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var startedOnce atomic.Bool
	send := func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		if startedOnce.CompareAndSwap(false, true) {
			close(started)
		}
		select {
		case <-ctx.Done(): // Close cancelled baseCtx: abort the stuck transaction
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	defer close(release)
	// Long per-attempt timeout: without deadline-driven cancellation the send would block ~30s, so
	// it would still be in flight when Close returns.
	n := newEmailNotifier(EmailConfig{
		Addr: "mail.example.com:587", From: "s@x", To: []string{"a@x"}, QueueSize: 4, Workers: 1,
		MaxAttempts: 3, BaseBackoff: time.Millisecond, Timeout: 30 * time.Second,
	}, send)
	n.Notify(Event{Target: "t", Alert: "loss", Firing: true, When: testWhen})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery never reached the sender")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	n.Close(ctx)
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Close took %v; the in-flight send was not cancelled at the deadline", el)
	}
	if f := n.Stats().Failed; f != 1 {
		t.Errorf("Failed = %d, want 1 (the in-flight delivery must be cancelled + counted, not left hanging)", f)
	}
}

// permanentSendError classifies the two non-retryable error classes (5xx *textproto.Error, wrapped
// or bare, and the errAuthNotOffered sentinel) and leaves transient ones (4xx, plain errors) to be
// retried — the decision deliver keys off.
func TestPermanentSendError(t *testing.T) {
	authErr := fmt.Errorf("smtp: server %q does not advertise AUTH: %w", "mail:587", errAuthNotOffered)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"5xx textproto", &textproto.Error{Code: 550, Msg: "user unknown"}, true},
		{"wrapped 5xx textproto", fmt.Errorf("rcpt: %w", &textproto.Error{Code: 552, Msg: "mailbox full"}), true},
		{"auth sentinel", authErr, true},
		{"4xx textproto", &textproto.Error{Code: 451, Msg: "greylisted"}, false},
		{"4xx boundary 499", &textproto.Error{Code: 499, Msg: "odd"}, false},
		{"plain transient", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := permanentSendError(c.err); got != c.want {
			t.Errorf("%s: permanentSendError = %v, want %v", c.name, got, c.want)
		}
	}
	if !errors.Is(authErr, errAuthNotOffered) {
		t.Errorf("AUTH error must satisfy errors.Is(errAuthNotOffered)")
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

	// Accept the connection and hold it open, silent, until the test ends: the client must stall on
	// the unanswered 220 greeting and hit its deadline. Tying the server-side close to the test's
	// lifetime (via done) means the accepted conn is always released, win or lose the greeting race.
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		<-done
	}()

	const timeout = 150 * time.Millisecond
	send := smtpSender(false)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	err = send(ctx, l.Addr().String(), nil, "f@x", []string{"t@x"}, []byte("msg"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("smtpSender returned nil error against a relay that never speaks; want a timeout error")
	}
	if elapsed > 10*timeout {
		t.Errorf("smtpSender took %v to fail, want roughly the %v deadline", elapsed, timeout)
	}
}

// Cancelling ctx (not its deadline) must unblock a real smtpSender stuck reading from a stalled
// relay: with a far-off deadline, SetDeadline alone can't rescue the read, so this exercises the
// watcher goroutine's conn.Close() on ctx.Done() — the load-bearing half of the Close-cancels-
// in-flight fix, which the injected-sender test above cannot reach.
func TestSMTPSenderContextCancelUnblocks(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	connected := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		close(connected) // dial has completed: the client is now blocked reading the 220 greeting
		defer c.Close()
		<-done // stall: never speak
	}()

	// Deadline is far off, so only ctx cancellation (via the watcher) can end the stalled read.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	send := smtpSender(false)
	errCh := make(chan error, 1)
	go func() {
		errCh <- send(ctx, l.Addr().String(), nil, "f@x", []string{"t@x"}, []byte("msg"))
	}()

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("client never connected")
	}
	start := time.Now()
	cancel() // trigger the watcher: it must force-close the conn and unblock the read

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("smtpSender returned nil after ctx cancel; want a non-nil error")
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("smtpSender took %v to unblock after cancel; the watcher did not force-close the conn", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("smtpSender did not return within 5s of ctx cancel; the watcher did not unblock the read")
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
	send := smtpSender(false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = send(ctx, l.Addr().String(), auth, "f@x", []string{"t@x"}, []byte("msg"))
	if err == nil || !strings.Contains(err.Error(), "does not advertise AUTH") {
		t.Fatalf("err = %v, want the \"does not advertise AUTH\" error", err)
	}
	// The operator-facing message is intact, and the error also carries the sentinel so deliver
	// can classify it as a permanent (non-retryable) failure.
	if !errors.Is(err, errAuthNotOffered) {
		t.Errorf("AUTH-not-offered error must satisfy errors.Is(errAuthNotOffered): %v", err)
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
