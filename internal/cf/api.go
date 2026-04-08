package cf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const cfBaseURL = "https://api.cloudflare.com/client/v4"

// apiClient is a minimal Cloudflare v4 REST API client.
type apiClient struct {
	token  string
	client *http.Client
}

func newAPIClient(token string) *apiClient {
	return &apiClient{token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

// cfEnvelope is the standard Cloudflare API response wrapper.
type cfEnvelope struct {
	Result  json.RawMessage `json:"result"`
	Success bool            `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// do executes a Cloudflare API request and returns the raw result JSON.
// body may be nil for requests without a payload.
func (a *apiClient) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	url := cfBaseURL + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB guard
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cloudflare API %s %s (status %d): decode response: %w", method, path, resp.StatusCode, err)
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return nil, fmt.Errorf("cloudflare: %s", env.Errors[0].Message)
		}
		return nil, fmt.Errorf("cloudflare: unknown error (status %d)", resp.StatusCode)
	}
	return env.Result, nil
}

// ── Shared types ──────────────────────────────────────────────────────────────

type dnsRecord struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// ingressRule is a cloudflared tunnel ingress entry.
type ingressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

type tunnelConfigResult struct {
	Config struct {
		Ingress []ingressRule `json:"ingress"`
	} `json:"config"`
}
