package token

import (
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result APITokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response failed: %w", err)
	}

	return result.Tokens, nil
}

// ReportExhausted reports a token as exhausted to the API
func (a *APIClient) ReportExhausted(email string) error {
	url := fmt.Sprintf("%s/tokens/%s/exhausted", a.baseURL, a.tenantID)

	payload := fmt.Sprintf(`{"email":"%s"}`, email)
	resp, err := a.client.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("report exhausted failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
