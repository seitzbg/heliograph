// Package agent is the smoke-agent runtime: pull a per-vantage assignment from the
// hub, run the shared probe/scheduler pipeline over it, and push raw rounds back
// with a bounded in-memory store-and-forward buffer.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"smokeping-modern/internal/agentwire"
)

// Client talks to one hub with one vantage's API key.
type Client struct {
	hub string // base URL, no trailing slash
	key string
	hc  *http.Client
}

// NewClient builds a client. insecure=true skips TLS verification (dev / self-signed
// before ACME); normal operation uses public-CA verification (D5). timeout bounds a
// single request.
func NewClient(hub, key string, insecure bool, timeout time.Duration) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 — opt-in
	}
	return &Client{hub: strings.TrimRight(hub, "/"), key: key, hc: &http.Client{Timeout: timeout, Transport: tr}}
}

// PullAssignment fetches this vantage's assignment. It replays etag in If-None-Match;
// a 304 returns changed=false with no error so the caller keeps its running job set.
func (c *Client) PullAssignment(ctx context.Context, etag string) (agentwire.Assignment, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.hub+"/agent/v1/assignment", nil)
	if err != nil {
		return agentwire.Assignment{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return agentwire.Assignment{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return agentwire.Assignment{}, false, nil
	case http.StatusOK:
		var a agentwire.Assignment
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&a); err != nil {
			return agentwire.Assignment{}, false, fmt.Errorf("agent: decode assignment: %w", err)
		}
		return a, true, nil
	default:
		return agentwire.Assignment{}, false, fmt.Errorf("agent: assignment: unexpected status %d", resp.StatusCode)
	}
}

// PushResults POSTs a batch; a non-2xx (or transport error) is returned so the caller
// retains the batch and retries.
// pushError carries the HTTP status of a failed results push so the flush loop can tell a
// PERMANENT rejection (the hub will never accept this batch — oversize 413 or malformed 400)
// from a transient one (5xx / 429 / auth), and drop vs retry accordingly (CODE_REVIEW #2).
type pushError struct{ status int }

func (e *pushError) Error() string { return fmt.Sprintf("agent: results: status %d", e.status) }

// permanent reports whether re-sending the SAME batch is futile.
func (e *pushError) permanent() bool {
	return e.status == http.StatusRequestEntityTooLarge || e.status == http.StatusBadRequest
}

func (c *Client) PushResults(ctx context.Context, rounds []agentwire.RoundReport) (agentwire.ResultsResponse, error) {
	body, err := json.Marshal(agentwire.ResultsRequest{Results: rounds})
	if err != nil {
		return agentwire.ResultsResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.hub+"/agent/v1/results", bytes.NewReader(body))
	if err != nil {
		return agentwire.ResultsResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return agentwire.ResultsResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return agentwire.ResultsResponse{}, &pushError{status: resp.StatusCode}
	}
	var out agentwire.ResultsResponse
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	return out, nil
}
