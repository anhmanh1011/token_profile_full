package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Microsoft Teams MSAL public client id — matches Python token_getter.
	msalClientID = "5e3ce6c0-2b1f-4285-8d4b-75ee78787346"

	// Loki-scoped token endpoint scope, URL-encoded form.
	lokiScope = "https://loki.delve.office.com//.default openid profile offline_access"

	// exchangeMaxAttempts — total attempts (1 initial + 2 retries) for transient failures.
	exchangeMaxAttempts = 3
	exchangeRetryDelay  = 1 * time.Second
)

// ErrRefreshTokenRevoked means the refresh_token is no longer valid (e.g. user
// disabled, password changed, or 24h TTL expired). Non-retryable; caller should
// mark the token dead and delete the user.
var ErrRefreshTokenRevoked = errors.New("refresh_token revoked or expired")

type exchangeResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// ExchangeRefreshToken swaps a refresh_token for a Loki-scoped access_token via
// the Microsoft OAuth2 v2.0 token endpoint. Transient failures (network, 5xx, 429)
// are retried up to exchangeMaxAttempts; HTTP 400 invalid_grant is returned as
// ErrRefreshTokenRevoked without retry.
func ExchangeRefreshToken(client *http.Client, tenantID, refreshToken string) (string, time.Duration, error) {
	if tenantID == "" || refreshToken == "" {
		return "", 0, fmt.Errorf("token/exchange: empty tenantID or refreshToken")
	}

	endpoint := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)

	form := url.Values{}
	form.Set("client_id", msalClientID)
	form.Set("redirect_uri", "https://teams.microsoft.com/v2/auth")
	form.Set("scope", lokiScope)
	form.Set("grant_type", "refresh_token")
	form.Set("client_info", "1")
	form.Set("x-client-SKU", "msal.js.browser")
	form.Set("x-client-VER", "3.30.0")
	form.Set("refresh_token", refreshToken)
	body := form.Encode()

	var lastErr error
	for attempt := 1; attempt <= exchangeMaxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(exchangeRetryDelay)
		}

		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return "", 0, fmt.Errorf("token/exchange: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Origin", "https://teams.microsoft.com")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("token/exchange: attempt %d: %w", attempt, err)
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("token/exchange: read body attempt %d: %w", attempt, readErr)
			continue
		}

		// Retry transient server errors and rate-limiting.
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("token/exchange: status %d: %s", resp.StatusCode, truncate(string(respBody), 200))
			continue
		}

		var parsed exchangeResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return "", 0, fmt.Errorf("token/exchange: parse response: %w (body: %s)", err, truncate(string(respBody), 200))
		}

		if resp.StatusCode >= 400 {
			// invalid_grant / invalid_client / etc — refresh_token no longer usable.
			if parsed.Error == "invalid_grant" || parsed.Error == "interaction_required" {
				return "", 0, ErrRefreshTokenRevoked
			}
			return "", 0, fmt.Errorf("token/exchange: status %d %s: %s", resp.StatusCode, parsed.Error, parsed.ErrorDesc)
		}

		if parsed.AccessToken == "" {
			return "", 0, fmt.Errorf("token/exchange: empty access_token in response")
		}

		expiresIn := time.Duration(parsed.ExpiresIn) * time.Second
		if expiresIn <= 0 {
			expiresIn = 55 * time.Minute // conservative default
		}
		return parsed.AccessToken, expiresIn, nil
	}

	return "", 0, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
