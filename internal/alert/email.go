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
}

// EmailNotifier sends alerts by SMTP. Like the webhook pool it delivers asynchronously off a bounded
// queue, so Notify never blocks the alert-eval path, and Close drains the queue on shutdown.
type EmailNotifier struct {
	addr string
	from string
	to   []string
	auth smtp.Auth
	send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error

	queue  chan Event
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool

	queued, delivered, dropped, failed atomic.Int64
}

// NewEmailNotifier builds an SMTP notifier. It negotiates STARTTLS when the server offers it (with
// strict cert verification unless cfg.TLSSkipVerify), authenticates when cfg.Auth is set, and falls
// back to a plaintext session for a relay that offers no STARTTLS.
func NewEmailNotifier(cfg EmailConfig) *EmailNotifier {
	return newEmailNotifier(cfg, smtpSender(cfg.TLSSkipVerify))
}

// smtpSender returns a net/smtp.SendMail-compatible send function. Unlike the stdlib SendMail it
// exposes the STARTTLS tls.Config, so an internal relay with a self-signed cert works when the
// operator opts in via TLSSkipVerify; it also tolerates a relay that advertises no STARTTLS.
func smtpSender(skipVerify bool) func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	return func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		c, err := smtp.Dial(addr)
		if err != nil {
			return err
		}
		defer c.Close()
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			// InsecureSkipVerify only when the operator opted in for an internal relay; the default
			// is strict verification.
			if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: skipVerify}); err != nil {
				return err
			}
		}
		if a != nil {
			if ok, _ := c.Extension("AUTH"); ok {
				if err := c.Auth(a); err != nil {
					return err
				}
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
	n := &EmailNotifier{
		addr: cfg.Addr, from: cfg.From, to: cfg.To, auth: cfg.Auth, send: send,
		queue: make(chan Event, cfg.QueueSize),
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
		if err := n.send(n.addr, n.auth, n.from, n.to, buildEmailMessage(e, n.from, n.to)); err != nil {
			n.failed.Add(1)
			slog.Warn("email: send failed", "addr", n.addr, "alert", e.Alert, "target", e.Target, "err", err)
			continue
		}
		n.delivered.Add(1)
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
	close(n.queue)
	n.mu.Unlock()

	done := make(chan struct{})
	go func() { n.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done(): // deadline hit; an in-flight send finishes but we stop waiting
	}
}

// Stats snapshots the delivery counters (reusing WebhookStats so /metrics stays uniform).
func (n *EmailNotifier) Stats() WebhookStats {
	return WebhookStats{
		Queued:     n.queued.Load(),
		Delivered:  n.delivered.Load(),
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
