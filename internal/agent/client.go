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

	"github.com/seitzbg/heliograph/internal/agentwire"
)

// Client talks to one hub, authenticating with one vantage's mTLS client certificate.
type Client struct {
	hub string // base URL, no trailing slash
	hc  *http.Client
}

// NewClient builds a client. tlsClient, when non-nil, is applied to the underlying
// transport's TLSClientConfig — normally a client certificate plus the hub's CA pool (or,
// for dev/self-signed setups, InsecureSkipVerify). timeout bounds a single request.
func NewClient(hub string, tlsClient *tls.Config, timeout time.Duration) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tlsClient != nil {
		tr.TLSClientConfig = tlsClient
	}
	return &Client{hub: strings.TrimRight(hub, "/"), hc: &http.Client{Timeout: timeout, Transport: tr}}
}

// PullAssignment fetches this vantage's assignment. It replays etag in If-None-Match;
// a 304 returns changed=false with no error so the caller keeps its running job set.
func (c *Client) PullAssignment(ctx context.Context, etag string) (agentwire.Assignment, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.hub+"/agent/v1/assignment", nil)
	if err != nil {
		return agentwire.Assignment{}, false, err
	}
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
// from a transient one (5xx / 429 / auth), and drop vs retry accordingly (CODE_REVIEW #2). Of
// the two permanent cases, a 413 is further distinguished from a 400 (oversize()) because it is
// recoverable by SPLITTING the batch rather than dropping it outright — see Agent.sendBatch
// (CODE_REVIEW round-9 #1).
type pushError struct{ status int }

func (e *pushError) Error() string { return fmt.Sprintf("agent: results: status %d", e.status) }

// permanent reports whether re-sending the SAME batch is futile.
func (e *pushError) permanent() bool {
	return e.status == http.StatusRequestEntityTooLarge || e.status == http.StatusBadRequest
}

// oversize reports whether the rejection was specifically a SIZE condition (413), as opposed
// to a malformed-batch rejection (400). Unlike a 400 — which means the hub's decoder rejected
// the shape of the whole batch, so no sub-batch of it is any more sendable — a 413 is
// recoverable: a smaller sub-batch of the SAME rounds may fit under the hub's byte cap, so the
// caller can split instead of dropping every round in the batch (CODE_REVIEW round-9 #1).
func (e *pushError) oversize() bool {
	return e.status == http.StatusRequestEntityTooLarge
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
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return agentwire.ResultsResponse{}, err
	}
	defer resp.Body.Close()
	// Require the exact success status the hub emits (200), one well-formed JSON object, and
	// counters that account for every submitted round. A proxy/maintenance/misroute 2xx (204,
	// empty 200, HTML, truncated JSON) or inconsistent counts is treated as a TRANSIENT error,
	// not success, so the flush loop retains the batch instead of reclaiming rounds the hub never
	// acknowledged storing (CODE_REVIEW M1). These are non-*pushError, so sendBatch classifies
	// them as transient and retries the whole batch.
	if resp.StatusCode != http.StatusOK {
		return agentwire.ResultsResponse{}, &pushError{status: resp.StatusCode}
	}
	var out agentwire.ResultsResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&out); err != nil {
		return agentwire.ResultsResponse{}, fmt.Errorf("agent: results: unreadable hub acknowledgment: %w", err)
	}
	if dec.More() {
		return agentwire.ResultsResponse{}, fmt.Errorf("agent: results: trailing data after hub acknowledgment")
	}
	if out.Accepted < 0 || out.Dropped < 0 || out.Accepted+out.Dropped != len(rounds) {
		return agentwire.ResultsResponse{}, fmt.Errorf("agent: results: hub acknowledgment does not account for the batch (accepted=%d dropped=%d, sent=%d)", out.Accepted, out.Dropped, len(rounds))
	}
	return out, nil
}
