package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is the host-side typed client for the daemon API. BaseURL is
// http://<machine-ip>:<agentPort>; Token is the shared bearer secret.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient returns a Client with a sane default timeout. Mutations (restore,
// import) can outlast it; callers pass a context and may set a longer HTTP
// client for those.
func NewClient(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) Version(ctx context.Context) (VersionResponse, error) {
	var out VersionResponse
	err := c.do(ctx, http.MethodGet, "/v1/version", nil, &out)
	return out, err
}

// Healthy reports whether the daemon answers /v1/healthz (no auth needed).
func (c *Client) Healthy(ctx context.Context) bool {
	var out HealthResponse
	err := c.do(ctx, http.MethodGet, "/v1/healthz", nil, &out)
	return err == nil && out.OK
}

func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var out StatusResponse
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

func (c *Client) Snapshots(ctx context.Context, slot string) (SnapshotsResponse, error) {
	var out SnapshotsResponse
	err := c.do(ctx, http.MethodGet, "/v1/snapshots?slot="+url.QueryEscape(slot), nil, &out)
	return out, err
}

func (c *Client) Promote(ctx context.Context) (PromoteResponse, error) {
	var out PromoteResponse
	err := c.do(ctx, http.MethodPost, "/v1/promote", struct{}{}, &out)
	return out, err
}

func (c *Client) Snapshot(ctx context.Context, req SnapshotRequest) (OpResponse, error) {
	var out OpResponse
	err := c.do(ctx, http.MethodPost, "/v1/snapshot", req, &out)
	return out, err
}

func (c *Client) Restore(ctx context.Context, req RestoreRequest) (OpResponse, error) {
	var out OpResponse
	err := c.do(ctx, http.MethodPost, "/v1/restore", req, &out)
	return out, err
}

func (c *Client) Reconcile(ctx context.Context) (ReconcileResponse, error) {
	var out ReconcileResponse
	err := c.do(ctx, http.MethodPost, "/v1/reconcile", struct{}{}, &out)
	return out, err
}

func (c *Client) Up(ctx context.Context) (StatusResponse, error) {
	var out StatusResponse
	err := c.do(ctx, http.MethodPost, "/v1/up", struct{}{}, &out)
	return out, err
}

func (c *Client) Down(ctx context.Context) (OpResponse, error) {
	var out OpResponse
	err := c.do(ctx, http.MethodPost, "/v1/down", struct{}{}, &out)
	return out, err
}

func (c *Client) StartStaging(ctx context.Context) (OpResponse, error) {
	var out OpResponse
	err := c.do(ctx, http.MethodPost, "/v1/staging/start", struct{}{}, &out)
	return out, err
}

func (c *Client) StopStaging(ctx context.Context) (OpResponse, error) {
	var out OpResponse
	err := c.do(ctx, http.MethodPost, "/v1/staging/stop", struct{}{}, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var eb errorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
			return fmt.Errorf("agent: %s", eb.Error)
		}
		return fmt.Errorf("agent: %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
