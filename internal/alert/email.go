package alert

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errAuthNotOffered marks the permanent misconfiguration where the operator configured SMTP AUTH
// but the relay's EHLO doesn't advertise it (often a missing STARTTLS negotiation). No relay
// round-trip will ever make such a send succeed, so deliver classifies it as non-retryable via
// errors.Is — matching robustly on the sentinel rather than the human-readable message string.
var errAuthNotOffered = errors.New("permanent SMTP AUTH misconfiguration")

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
	send func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error

	queue       chan Event
	done        chan struct{}      // closed by Close to interrupt in-flight backoffs
	baseCtx     context.Context    // parent of every attempt's send context
	baseCancel  context.CancelFunc // cancels the in-flight send when the drain deadline hits
	wg          sync.WaitGroup
	timeout     time.Duration
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
	return newEmailNotifier(cfg, smtpSender(cfg.TLSSkipVerify))
}

// smtpSender returns a net/smtp.SendMail-compatible send function. Unlike the stdlib SendMail it
// exposes the STARTTLS tls.Config, so an internal relay with a self-signed cert works when the
// operator opts in via TLSSkipVerify; it also tolerates a relay that advertises no STARTTLS. The
// call is bound to ctx: its deadline caps the whole transaction (dial + STARTTLS + auth + data) with
// a single budget, and its cancellation (a Close-triggered shutdown) force-closes the connection so
// a send blocked on a stalled relay can't outlive the drain — net/smtp has no context of its own.
// Callers should pass a ctx carrying a deadline (deliver wraps each attempt in context.WithTimeout);
// without one the transaction is bounded only by ctx cancellation.
func smtpSender(skipVerify bool) func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	return func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		// One deadline for the whole transaction (dial already honored ctx above), so a slow dial
		// can't buy the exchange a second full timeout window.
		if deadline, ok := ctx.Deadline(); ok {
			if err := conn.SetDeadline(deadline); err != nil {
				conn.Close()
				return err
			}
		}
		// Force-close the conn when ctx is cancelled (e.g. Close hit its drain deadline) so a read or
		// write blocked on a hung relay unblocks with an error instead of hanging to the deadline.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-stop:
			}
		}()
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
				// Keep the operator-facing message intact and wrap the sentinel, so deliver can
				// recognize this permanent misconfiguration with errors.Is (see errAuthNotOffered).
				return fmt.Errorf("smtp: authentication configured but server %q does not advertise AUTH (STARTTLS may be required): %w", addr, errAuthNotOffered)
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
func newEmailNotifier(cfg EmailConfig, send func(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error) *EmailNotifier {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
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
		timeout:     cfg.Timeout,
		maxAttempts: cfg.MaxAttempts,
		baseBackoff: cfg.BaseBackoff,
	}
	// Every attempt's send derives from baseCtx, so Close can cancel an in-flight transaction once
	// the drain deadline is reached (not just interrupt the backoff wait) — mirrors WebhookNotifier.
	n.baseCtx, n.baseCancel = context.WithCancel(context.Background())
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

// permanentSendError reports whether err can never succeed on retry, so deliver should give up
// immediately instead of burning the whole retry/backoff budget (~41.5s = 4×10s timeout + backoff)
// on a doomed send — during an incident burst that budget is a worker the queue needs for other
// alerts. Two classes qualify: a 5xx SMTP reply (net/smtp surfaces server replies as
// *textproto.Error; Code >= 500 is a permanent rejection — unknown recipient, relay denied — while
// 4xx is transient and retryable), and the AUTH-not-advertised configuration error
// (errAuthNotOffered), which no relay round-trip will fix.
func permanentSendError(err error) bool {
	if errors.Is(err, errAuthNotOffered) {
		return true
	}
	var tperr *textproto.Error
	if errors.As(err, &tperr) && tperr.Code >= 500 {
		return true
	}
	return false
}

// deliver sends one event, retrying with exponential backoff up to maxAttempts. A permanent failure
// (see permanentSendError) is abandoned on the first attempt — no retry can rescue it. The backoff
// wait is interrupted by Close, so a shutdown drain doesn't stall on a down relay.
func (n *EmailNotifier) deliver(e Event) {
	msg := buildEmailMessage(e, n.from, n.to)
	backoff := n.baseBackoff
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(n.baseCtx, n.timeout)
		err := n.send(ctx, n.addr, n.auth, n.from, n.to, msg)
		cancel()
		if err == nil {
			n.delivered.Add(1)
			return
		}
		if permanentSendError(err) {
			// A 5xx rejection or the AUTH misconfiguration can never succeed on retry: give up now
			// rather than tie up the worker for the whole retry budget on a doomed send.
			n.failed.Add(1)
			slog.Warn("email: giving up: permanent failure", "addr", n.addr, "alert", e.Alert, "target", e.Target, "attempt", attempt, "err", err)
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

// Close stops accepting events and drains the queue, waiting for the workers up to ctx's deadline;
// past the deadline it interrupts the remaining backoff waits and force-cancels any in-flight SMTP
// transaction, then returns. Idempotent.
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
		n.baseCancel() // release the base context
	case <-ctx.Done():
		// Deadline hit: stop the remaining backoff waits and cancel the in-flight send so a hung
		// relay can't keep a worker (and its open connection) past the shutdown budget.
		close(n.done)
		n.baseCancel()
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
