package alert

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EmailConfig configures the SMTP notifier.
type EmailConfig struct {
	Addr          string    // host:port; STARTTLS is negotiated when the server offers it
	From          string    // envelope + header From
	To            []string  // recipients
	Auth          smtp.Auth // nil for an unauthenticated relay
	TLSSkipVerify bool      // skip STARTTLS cert verification (for an internal relay with a self-signed cert)
	QueueSize     int       // bounded send queue (default 1024)
	Workers       int       // concurrent senders (default 2)

	Timeout     time.Duration // per-attempt SMTP transaction deadline: dial+STARTTLS+auth+data (default 10s)
	MaxAttempts int           // delivery attempts per event before giving up (default 4)
	BaseBackoff time.Duration // initial retry backoff, doubling each attempt (default 500ms)
}

// EmailNotifier sends alerts by SMTP. Like the webhook pool it delivers asynchronously off a bounded
// queue, so Notify never blocks the alert-eval path, and Close drains the queue on shutdown.
type EmailNotifier struct {
	addr string
	from string
	to   []string
	auth smtp.Auth
	send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error

	queue       chan Event
	done        chan struct{} // closed by Close to interrupt in-flight backoffs
	wg          sync.WaitGroup
	maxAttempts int
	baseBackoff time.Duration

	mu     sync.Mutex
	closed bool

	queued, delivered, retried, dropped, failed atomic.Int64
}

// NewEmailNotifier builds an SMTP notifier. It negotiates STARTTLS when the server offers it (with
// strict cert verification unless cfg.TLSSkipVerify), authenticates when cfg.Auth is set, and falls
// back to a plaintext session for a relay that offers no STARTTLS.
func NewEmailNotifier(cfg EmailConfig) *EmailNotifier {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return newEmailNotifier(cfg, smtpSender(cfg.TLSSkipVerify, timeout))
}

// smtpSender returns a net/smtp.SendMail-compatible send function. Unlike the stdlib SendMail it
// exposes the STARTTLS tls.Config, so an internal relay with a self-signed cert works when the
// operator opts in via TLSSkipVerify; it also tolerates a relay that advertises no STARTTLS. timeout
// bounds the whole transaction (dial + STARTTLS + auth + data) so a stalled relay can't hang a
// worker forever.
func smtpSender(skipVerify bool, timeout time.Duration) func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	return func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return err
		}
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			conn.Close()
			return err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return err
		}
		defer c.Close()
		if ok, _ := c.Extension("STARTTLS"); ok {
			// InsecureSkipVerify only when the operator opted in for an internal relay; the default
			// is strict verification.
			if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: skipVerify}); err != nil {
				return err
			}
		}
		if a != nil {
			// Fail loudly instead of silently falling back to an unauthenticated session: a relay
			// that doesn't advertise AUTH when the operator configured credentials is a
			// misconfiguration (often a missing STARTTLS negotiation), not something to paper over.
			ok, _ := c.Extension("AUTH")
			if !ok {
				return fmt.Errorf("smtp: authentication configured but server %q does not advertise AUTH (STARTTLS may be required)", addr)
			}
			if err := c.Auth(a); err != nil {
				return err
			}
		}
		if err := c.Mail(from); err != nil {
			return err
		}
		for _, r := range to {
			if err := c.Rcpt(r); err != nil {
				return err
			}
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
}

// newEmailNotifier is the injectable-sender constructor used by tests.
func newEmailNotifier(cfg EmailConfig, send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error) *EmailNotifier {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 500 * time.Millisecond
	}
	n := &EmailNotifier{
		addr: cfg.Addr, from: cfg.From, to: cfg.To, auth: cfg.Auth, send: send,
		queue:       make(chan Event, cfg.QueueSize),
		done:        make(chan struct{}),
		maxAttempts: cfg.MaxAttempts,
		baseBackoff: cfg.BaseBackoff,
	}
	for i := 0; i < cfg.Workers; i++ {
		n.wg.Add(1)
		go n.worker()
	}
	return n
}

func (n *EmailNotifier) Notify(e Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	select {
	case n.queue <- e:
		n.queued.Add(1)
	default:
		n.dropped.Add(1)
		slog.Warn("email: delivery queue full, dropping event", "alert", e.Alert, "target", e.Target, "queue_size", cap(n.queue))
	}
}

func (n *EmailNotifier) worker() {
	defer n.wg.Done()
	for e := range n.queue {
		n.deliver(e)
	}
}

// deliver sends one event, retrying with exponential backoff up to maxAttempts. The backoff wait is
// interrupted by Close, so a shutdown drain doesn't stall on a down relay.
func (n *EmailNotifier) deliver(e Event) {
	msg := buildEmailMessage(e, n.from, n.to)
	backoff := n.baseBackoff
	for attempt := 1; ; attempt++ {
		err := n.send(n.addr, n.auth, n.from, n.to, msg)
		if err == nil {
			n.delivered.Add(1)
			return
		}
		slog.Warn("email: send attempt failed", "addr", n.addr, "alert", e.Alert, "target", e.Target, "attempt", attempt, "err", err)
		if attempt >= n.maxAttempts {
			n.failed.Add(1)
			slog.Warn("email: giving up after retries", "addr", n.addr, "alert", e.Alert, "target", e.Target, "attempts", attempt)
			return
		}
		n.retried.Add(1)
		select {
		case <-n.done: // draining on shutdown: stop retrying this event
			n.failed.Add(1)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// Close stops accepting events and drains the queue within ctx's deadline, then returns. Idempotent.
func (n *EmailNotifier) Close(ctx context.Context) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	close(n.queue) // stop accepting; workers finish the remaining items then exit their range loop
	n.mu.Unlock()

	// Do NOT interrupt retries yet: a graceful drain should spend its deadline retrying queued
	// events. n.done is closed once the drain finishes (a no-op then, nothing is waiting on it) or
	// once the deadline hits (to interrupt in-flight backoffs) — mirrors WebhookNotifier.Close.
	drained := make(chan struct{})
	go func() { n.wg.Wait(); close(drained) }()
	select {
	case <-drained:
		close(n.done)
	case <-ctx.Done(): // deadline hit; stop remaining backoff waits so drain doesn't stall on them
		close(n.done)
		<-drained
	}
}

// Stats snapshots the delivery counters (reusing WebhookStats so /metrics stays uniform).
func (n *EmailNotifier) Stats() WebhookStats {
	return WebhookStats{
		Queued:     n.queued.Load(),
		Delivered:  n.delivered.Load(),
		Retried:    n.retried.Load(),
		Dropped:    n.dropped.Load(),
		Failed:     n.failed.Load(),
		QueueDepth: len(n.queue),
	}
}

// WriteMetrics appends the email delivery counters in Prometheus text format. Its own metric family
// (heliograph_email_*), so it never collides with the webhook/slack/discord family.
func (n *EmailNotifier) WriteMetrics(b *strings.Builder) {
	s := n.Stats()
	fmt.Fprintf(b, "# HELP heliograph_email_queued_total Email alerts accepted onto the delivery queue.\n# TYPE heliograph_email_queued_total counter\nheliograph_email_queued_total %d\n", s.Queued)
	fmt.Fprintf(b, "# HELP heliograph_email_delivered_total Email alerts handed to the SMTP server.\n# TYPE heliograph_email_delivered_total counter\nheliograph_email_delivered_total %d\n", s.Delivered)
	fmt.Fprintf(b, "# HELP heliograph_email_retried_total Email delivery attempts retried after a failure.\n# TYPE heliograph_email_retried_total counter\nheliograph_email_retried_total %d\n", s.Retried)
	fmt.Fprintf(b, "# HELP heliograph_email_dropped_total Email alerts dropped because the delivery queue was full.\n# TYPE heliograph_email_dropped_total counter\nheliograph_email_dropped_total %d\n", s.Dropped)
	fmt.Fprintf(b, "# HELP heliograph_email_failed_total Email alerts that failed to send.\n# TYPE heliograph_email_failed_total counter\nheliograph_email_failed_total %d\n", s.Failed)
	fmt.Fprintf(b, "# HELP heliograph_email_queue_depth Current email delivery queue depth.\n# TYPE heliograph_email_queue_depth gauge\nheliograph_email_queue_depth %d\n", s.QueueDepth)
}

// oneLine strips CR/LF so an operator-authored target/alert name can't inject extra SMTP headers.
func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// buildEmailMessage renders an RFC 5322 message: a one-line subject + the shared alert message as a
// plain-text body.
func buildEmailMessage(e Event, from string, to []string) []byte {
	subject := fmt.Sprintf("[%s] %s on %s", e.Status(), oneLine(e.Alert), oneLine(e.Target))
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", e.When.UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(alertMessage(e), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
