package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APITokenData matches the JSON from Token API
type APITokenData struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	RefreshToken string `json:"refresh_token"`
	TenantID     string `json:"tenant_id"`
	CreatedAt    int64  `json:"created_at"`
}

// APITokenResponse is the response from GET /tokens/{tenant_id}
type APITokenResponse struct {
	Tokens []APITokenData `json:"tokens"`
	Count  int            `json:"count"`
}

// APIClient fetches tokens from the central Token API
type APIClient struct {
	baseURL  string
	tenantID string
	client   *http.Client
}

// NewAPIClient creates a new API client
func NewAPIClient(baseURL, tenantID string) *APIClient {
	return &APIClient{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		tenantID: tenantID,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FetchTokens fetches a batch of tokens from the API
func (a *APIClient) FetchTokens(count int) ([]APITokenData, error) {
	url := fmt.Sprintf("%s/tokens/%s?count=%d", a.baseURL, a.tenantID, count)

	resp, err := a.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch tokens failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result APITokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response failed: %w", err)
	}

	return result.Tokens, nil
}

type exhaustedPayload struct {
	Email string `json:"email"`
}

// ReportExhausted reports a token as exhausted to the API.
// The provided context controls cancellation of the HTTP request.
func (a *APIClient) ReportExhausted(ctx context.Context, email string) error {
	reqURL := fmt.Sprintf("%s/tokens/%s/exhausted", a.baseURL, a.tenantID)

	payload, err := json.Marshal(exhaustedPayload{Email: email})
	if err != nil {
		return fmt.Errorf("marshal payload failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("report exhausted failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
