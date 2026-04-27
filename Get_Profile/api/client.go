package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"linkedin_fetcher/models"
	"linkedin_fetcher/proxy"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	// API Base URL - using EUR region as in the sample CURL
	BaseURL = "https://eur.loki.delve.office.com/api/v1/linkedin/profiles/full"
)

// Client handles LinkedIn API requests
type Client struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates a new API client with connection pooling
func NewClient(timeoutSeconds int) *Client {
	return NewClientWithProxy(timeoutSeconds, "")
}

// NewClientWithProxy creates a new API client. proxyURL accepts either the
// legacy host:port[:user:pass] form or a full socks5h://[user:pass@]host:port
// URL; HTTP proxies are not supported here because the Loki traffic must use
// SOCKS5 to keep DNS off the local resolver.
func NewClientWithProxy(timeoutSeconds int, proxyURL string) *Client {
	timeout := time.Duration(timeoutSeconds) * time.Second

	dial, err := proxy.SOCKS5DialContext(proxy.Parse(proxyURL), 5*time.Second, 30*time.Second)
	if err != nil {
		slog.Error("api: failed to build SOCKS5 dialer, falling back to direct", "err", err)
		dial, _ = proxy.SOCKS5DialContext("", 5*time.Second, 30*time.Second)
	}

	transport := &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          4000,
		MaxIdleConnsPerHost:   3000,
		MaxConnsPerHost:       3000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		timeout: timeout,
	}
}

// FetchProfile fetches LinkedIn profile for an email
// Returns: (*models.Result, statusCode, error)
func (c *Client) FetchProfile(email, token string) (*models.Result, int, error) {
	// Build request URL with query parameters
	reqURL := c.buildURL(email)

	// Build request body
	bodyData := c.buildRequestBody(token)
	bodyBytes, err := json.Marshal(bodyData)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal body: %w", err)
	}

	// Create request
	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	c.setHeaders(req)

	// Execute request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Handle gzip compressed response
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, resp.StatusCode, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}

	// Read response body
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode == 401 {
		return nil, 401, fmt.Errorf("unauthorized - token expired")
	}

	if resp.StatusCode != 200 {
		return nil, resp.StatusCode, fmt.Errorf("API error: status %d", resp.StatusCode)
	}

	// Parse response
	var linkedInResp models.LinkedInResponse
	if err := json.Unmarshal(body, &linkedInResp); err != nil {
		return nil, 200, fmt.Errorf("failed to parse response: %w", err)
	}

	// Check for ExactMatch
	if linkedInResp.ResultTemplate != "ExactMatch" {
		return nil, 200, nil // Not an exact match, no result
	}

	// Extract profile from ExactMatch response
	if len(linkedInResp.Persons) == 0 {
		return nil, 200, nil // No person found
	}

	person := linkedInResp.Persons[0]
	result := &models.Result{
		Email:           email,
		DisplayName:     person.DisplayName,
		ConnectionCount: person.ConnectionCount,
		Location:        person.Location,
		LinkedInURL:     person.LinkedInURL,
	}

	return result, 200, nil
}

// buildURL constructs the API URL with query parameters
func (c *Client) buildURL(email string) string {
	params := url.Values{}
	params.Set("hostAppPersonaId", `{"userId":"8:orgid:6342ca6f-75f9-4789-88c4-200f31637b78","isSharedChannel":false,"hostConversationId":"48:notes"}`)
	params.Set("userPrincipalName", "401902685@jic.edu.sa")
	params.Set("teamsMri", "8:orgid:6342ca6f-75f9-4789-88c4-200f31637b78")
	params.Set("smtp", email)
	params.Set("personaType", "User")
	params.Set("aadObjectId", "6342ca6f-75f9-4789-88c4-200f31637b78")
	params.Set("displayName", "User")
	params.Set("RootCorrelationId", "aa0667e2-d1d7-4ee5-ac46-705165fcfd18")
	params.Set("CorrelationId", "aa0667e2-d1d7-4ee5-ac46-705165fcfd18")
	params.Set("ClientCorrelationId", "df49e0d0-d26c-4ef8-8776-187e3f3520fc")
	params.Set("ConvertGetPost", "true")

	return BaseURL + "?" + params.Encode()
}

// buildRequestBody creates the request body payload
func (c *Client) buildRequestBody(token string) map[string]interface{} {
	return map[string]interface{}{
		"Accept":                      "application/json",
		"Content-Type":                "application/json",
		"X-ClientType":                "Teams",
		"X-ClientFeature":             "LivePersonaCard",
		"X-ClientArchitectureVersion": "v2",
		"X-ClientScenario":            "LinkedInProfileSearchResult",
		"X-HostAppApp":                "",
		"X-HostAppPlatform":           "Web",
		"X-LPCVersion":                "1.20240819.1.0",
		"authorization":               "Bearer " + token,
		"X-HostAppRing":               "general",
		"X-HostAppVersion":            "1415/24091221318",
		"X-HostAppCapabilities":       `{"isLinkedInEnabledInHostApp":true,"isLokiContactDataDisabled":false}`,
	}
}

// setHeaders sets required HTTP headers
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:124.0) Gecko/20100101 Firefox/124.0")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Referer", "https://outlook.live.com/")
	req.Header.Set("Origin", "https://outlook.live.com")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Connection", "keep-alive")
}
