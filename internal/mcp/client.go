package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrConfigInvalid  = errors.New("config invalid")
	ErrConfigConflict = errors.New("config version conflict")
	ErrNoAdmin        = errors.New("admin password not configured")
)

type Config struct {
	BaseURL   string
	BasicUser string
	BasicPass string
	AdminPass string
}

type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client

	mu       sync.Mutex
	loggedIn bool
}

func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("heliograph URL is required (set -url or HELIOGRAPH_URL)")
	}
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("bad URL %q: %w", cfg.BaseURL, err)
	}
	jar, _ := cookiejar.New(nil)
	return &Client{cfg: cfg, base: u, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}, nil
}

func (c *Client) url(path string, q url.Values) string {
	u := *c.base
	u.Path = c.base.Path + path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// setBasicAuth attaches the configured proxy Basic Auth credentials, when set.
func (c *Client) setBasicAuth(req *http.Request) {
	if c.cfg.BasicUser != "" {
		req.SetBasicAuth(c.cfg.BasicUser, c.cfg.BasicPass)
	}
}

// do sends req with Basic Auth (when configured). On a 401 for an /api/admin path,
// it logs in once and retries — the session cookie may have expired. A body-less
// request (e.g. GET) is always retryable; a request with a body is only retryable
// if it carries a GetBody so the body can be replayed.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	c.setBasicAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.HasPrefix(req.URL.Path, "/api/admin") ||
		c.cfg.AdminPass == "" ||
		!(req.Body == nil || req.GetBody != nil) {
		return resp, nil
	}
	resp.Body.Close()
	if err := c.login(req.Context(), true); err != nil {
		return nil, err
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	// http.Client.Do appends cookies from the jar rather than replacing them, so
	// reusing req without clearing the header would send the stale cookie the first
	// attempt attached ahead of the fresh one the login above just stored.
	req.Header.Del("Cookie")
	c.setBasicAuth(req)
	return c.http.Do(req)
}

// login posts the admin password and stores the session cookie in the jar. force
// re-logs even if already logged in.
func (c *Client) login(ctx context.Context, force bool) error {
	if c.cfg.AdminPass == "" {
		return ErrNoAdmin
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn && !force {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"password": c.cfg.AdminPass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/admin/login", nil), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setBasicAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("admin login failed: %s", resp.Status)
	}
	c.loggedIn = true
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	body, err := c.getBytes(ctx, path, q)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (c *Client) getBytes(ctx context.Context, path string, q url.Values) ([]byte, error) {
	// An open /api/admin read still benefits from the admin cookie (unredacted); log in first if we can.
	if strings.HasPrefix(path, "/api/admin") && c.cfg.AdminPass != "" {
		_ = c.login(ctx, false)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path, q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, serverError(body))
	}
	return body, nil
}

func (c *Client) getConfigDoc(ctx context.Context, source string) (json.RawMessage, int, error) {
	q := url.Values{}
	if source != "" {
		q.Set("source", source)
	}
	var env struct {
		Version int             `json:"version"`
		Doc     json.RawMessage `json:"doc"`
	}
	if err := c.getJSON(ctx, "/api/admin/config", q, &env); err != nil {
		return nil, 0, err
	}
	if len(bytes.TrimSpace(env.Doc)) == 0 {
		env.Doc = json.RawMessage(`{}`)
	}
	return env.Doc, env.Version, nil
}

func (c *Client) putConfig(ctx context.Context, doc json.RawMessage, expectedVersion int) (int, error) {
	if c.cfg.AdminPass == "" {
		return 0, ErrNoAdmin
	}
	if err := c.login(ctx, false); err != nil {
		return 0, err
	}
	payload, _ := json.Marshal(map[string]any{"version": expectedVersion, "doc": doc})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("/api/admin/config", nil), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Version int `json:"version"`
		}
		_ = json.Unmarshal(body, &out)
		return out.Version, nil
	case http.StatusBadRequest:
		return 0, fmt.Errorf("%w: %s", ErrConfigInvalid, serverError(body))
	case http.StatusConflict:
		return 0, ErrConfigConflict
	default:
		return 0, fmt.Errorf("apply failed: %s: %s", resp.Status, serverError(body))
	}
}

// serverError pulls the "error" field out of a JSON error body, falling back to the raw body.
func serverError(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(body))
}
