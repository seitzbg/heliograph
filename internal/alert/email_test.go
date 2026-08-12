package alert

import (
	"context"
	"net/smtp"
	"strings"
	"sync"
	"testing"
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
