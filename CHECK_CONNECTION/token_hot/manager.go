package token_hot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNoTokensAvailable = errors.New("no tokens available")
	ErrAllTokensDead     = errors.New("all tokens are dead")
)

const (
	// QuotaExhaustedThreshold - số lần fail liên tiếp trước khi đánh dấu quota exhausted
	// Sau N lần lỗi 500 liên tiếp, token bị đánh dấu exhausted và không dùng nữa
	QuotaExhaustedThreshold = 5
)

// TokenInfo stores the parsed token information
type TokenInfo struct {
	Username     string
	Password     string
	RefreshToken string
	TenantID     string
	AccessToken  string
	ExpiresAt    time.Time
	dead         int32      // atomic flag: 0=alive, 1=dead (refresh token invalid)
	exhausted    int32      // atomic flag: 0=có quota, 1=hết quota
	failCount    int32      // atomic: số lần fail liên tiếp với status 500
	exhaustedAt  time.Time  // thời điểm bị đánh dấu exhausted
	mu           sync.Mutex // per-token lock for refresh
}

// RefreshTokenResponse is the response from Microsoft OAuth endpoint
type RefreshTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// Manager handles token rotation and management
type Manager struct {
	tokenInfos []*TokenInfo
	currentIdx uint64 // atomic, use uint64 for better atomic ops

	// Token Queue for new architecture
	tokenQueue chan *TokenInfo
	queueMode  bool

	// HTTP client for refresh requests - with connection pooling
	httpClient *http.Client

	// Statistics
	totalTokens    int32
	deadCount      int32
	exhaustedCount int32

	// Refresh token saving
	refreshedTokens chan *TokenInfo // Channel to save refreshed tokens
	outputFilePath  string          // Path to save refreshed tokens
	saveWg          sync.WaitGroup  // Wait for save goroutine to finish
}

// NewManager creates a new token manager without proxy
func NewManager() *Manager {
	return NewManagerWithProxy("")
}

// NewManagerWithProxy creates a new token manager with optional proxy support
// proxyURL format: http://host:port, http://user:pass@host:port, socks5://host:port
func NewManagerWithProxy(proxyURL string) *Manager {
	// Optimized HTTP client for token refresh
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	// Add proxy if specified
	if proxyURL != "" {
		if parsedURL, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsedURL)
		}
	}

	return &Manager{
		tokenInfos: make([]*TokenInfo, 0),
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// LoadFromFile loads tokens from a file
// Supports two formats:
//  1. Old format: username:pass|refresh_token or username:pass|refresh_token|TENANT_ID
//  2. New format: email:pass:refresh_token (no TENANT_ID)
func (m *Manager) LoadFromFile(filepath string) error {
	file, err := os.Open(filepath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Use larger buffer for faster reading
	reader := bufio.NewReaderSize(file, 1024*1024)
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		// Fast trim
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var tokenInfo *TokenInfo

		// Check format by presence of pipe character
		if strings.Contains(line, "|") {
			// Old format: username:pass|refresh_token or username:pass|refresh_token|TENANT_ID
			tokenInfo = m.parseOldFormat(line)
		} else {
			// New format: email:pass:refresh_token
			tokenInfo = m.parseNewFormat(line)
		}

		if tokenInfo != nil {
			m.tokenInfos = append(m.tokenInfos, tokenInfo)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	m.totalTokens = int32(len(m.tokenInfos))
	if m.totalTokens == 0 {
		return ErrNoTokensAvailable
	}

	return nil
}

// parseOldFormat parses: username:pass|refresh_token or username:pass|refresh_token|TENANT_ID
func (m *Manager) parseOldFormat(line string) *TokenInfo {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return nil
	}

	credentials := parts[0]
	refreshToken := strings.TrimSpace(parts[1])
	tenantID := ""
	if len(parts) >= 3 {
		tenantID = strings.TrimSpace(parts[2])
	}

	credParts := strings.SplitN(credentials, ":", 2)
	if len(credParts) != 2 {
		return nil
	}

	return &TokenInfo{
		Username:     strings.TrimSpace(credParts[0]),
		Password:     strings.TrimSpace(credParts[1]),
		RefreshToken: refreshToken,
		TenantID:     tenantID,
	}
}

// parseNewFormat parses: email:pass:refresh_token
func (m *Manager) parseNewFormat(line string) *TokenInfo {
	// Split into exactly 3 parts: email, password, refresh_token
	parts := strings.SplitN(line, ":", 3)
	if len(parts) != 3 {
		return nil
	}

	email := strings.TrimSpace(parts[0])
	password := strings.TrimSpace(parts[1])
	refreshToken := strings.TrimSpace(parts[2])

	if email == "" || refreshToken == "" {
		return nil
	}

	return &TokenInfo{
		Username:     email,
		Password:     password,
		RefreshToken: refreshToken,
		TenantID:     "", // No TENANT_ID in new format
	}
}

// refreshAccessToken calls Microsoft OAuth endpoint (per-token lock) with retry
// HOT VERSION: Uses new API endpoint for consumers with Outlook headers
func (m *Manager) refreshAccessToken(info *TokenInfo) error {
	info.mu.Lock()
	defer info.mu.Unlock()

	// Check if quota exhausted - không refresh nếu đã hết quota
	if atomic.LoadInt32(&info.exhausted) == 1 {
		// Kiểm tra xem đã sang ngày mới chưa (reset quota)
		if !m.isNewDay(info.exhaustedAt) {
			return fmt.Errorf("quota exhausted, wait until tomorrow")
		}
		// Reset quota cho ngày mới
		atomic.StoreInt32(&info.exhausted, 0)
		atomic.StoreInt32(&info.failCount, 0)
		atomic.AddInt32(&m.exhaustedCount, -1)
	}

	// Double check - another goroutine might have refreshed
	if info.AccessToken != "" && time.Now().Before(info.ExpiresAt) {
		return nil
	}

	// HOT API: Use consumers endpoint instead of tenant-specific
	tokenURL := "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"

	// HOT API: New parameters
	data := url.Values{}
	data.Set("client_id", "9199bf20-a13f-4107-85dc-02114787ef48")
	data.Set("redirect_uri", "https://outlook.live.com/mail/oauthRedirect.html")
	data.Set("scope", "liveprofilecard.access openid profile offline_access")
	data.Set("grant_type", "refresh_token")
	data.Set("client_info", "1")
	data.Set("x-client-SKU", "msal.js.browser")
	data.Set("x-client-VER", "4.12.0")
	data.Set("x-ms-lib-capability", "retry-after, h429")
	data.Set("refresh_token", info.RefreshToken)
	data.Set("claims", `{"access_token":{"xms_cc":{"values":["CP1"]}}}`)

	// Retry mechanism: 3 attempts with exponential backoff
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoffDuration := time.Duration(1<<attempt) * time.Second
			time.Sleep(backoffDuration)
		}

		req, err := http.NewRequest("POST", tokenURL, bytes.NewBufferString(data.Encode()))
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %w", err)
			continue
		}

		// HOT API: New headers matching Outlook web client
		req.Header.Set("Host", "login.microsoftonline.com")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		req.Header.Set("Origin", "https://outlook.live.com")
		req.Header.Set("Referer", "https://outlook.live.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
		req.Header.Set("Sec-Ch-Ua", `"Not.A/Brand";v="99", "Chromium";v="136"`)
		req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
		req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Priority", "u=1, i")

		resp, err := m.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			continue
		}

		// Rate limit - retry after backoff
		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("rate limited (429)")
			fmt.Printf("[TOKEN_DEBUG] %s: Rate limited (429), retrying...\n", info.Username)
			continue
		}

		if resp.StatusCode != 200 {
			// Parse error response for detailed logging
			var errResp map[string]interface{}
			json.Unmarshal(body, &errResp)
			errorCode := ""
			errorDesc := ""
			if e, ok := errResp["error"].(string); ok {
				errorCode = e
			}
			if d, ok := errResp["error_description"].(string); ok {
				errorDesc = d
			}

			lastErr = fmt.Errorf("refresh token failed with status %d: %s - %s", resp.StatusCode, errorCode, errorDesc)

			// Log detailed error for investigation
			fmt.Printf("[TOKEN_DEAD] %s | Status: %d | Error: %s | Desc: %.100s\n",
				info.Username, resp.StatusCode, errorCode, errorDesc)

			// Don't retry for auth errors (400, 401, 403) - token is truly dead
			if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
				return lastErr
			}
			continue
		}

		var tokenResp RefreshTokenResponse
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			lastErr = fmt.Errorf("failed to parse response: %w", err)
			continue
		}

		info.AccessToken = tokenResp.AccessToken

		// Stagger expiration: random offset 0-300 seconds (0-5 minutes)
		// This prevents thundering herd when all tokens expire at the same time
		randomOffset := rand.Intn(300)
		info.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn-60-randomOffset) * time.Second)

		if tokenResp.RefreshToken != "" {
			info.RefreshToken = tokenResp.RefreshToken
			// Queue token for saving to file
			m.queueRefreshedToken(info)
		}

		// Reset fail count on successful refresh
		atomic.StoreInt32(&info.failCount, 0)

		return nil
	}

	return lastErr
}

// isNewDay checks if exhaustedAt is from a previous day
func (m *Manager) isNewDay(exhaustedAt time.Time) bool {
	if exhaustedAt.IsZero() {
		return false
	}
	now := time.Now()
	// So sánh ngày (bỏ qua giờ phút giây)
	return now.Year() > exhaustedAt.Year() ||
		now.YearDay() > exhaustedAt.YearDay() ||
		(now.Year() == exhaustedAt.Year() && now.YearDay() > exhaustedAt.YearDay())
}

// GetToken returns the current active access token (lock-free hot path)
func (m *Manager) GetToken() (string, error) {
	total := int(atomic.LoadInt32(&m.totalTokens))
	if total == 0 {
		return "", ErrNoTokensAvailable
	}

	dead := int(atomic.LoadInt32(&m.deadCount))
	exhausted := int(atomic.LoadInt32(&m.exhaustedCount))
	if dead+exhausted >= total {
		return "", ErrAllTokensDead
	}

	// Lock-free round-robin with atomic increment
	startIdx := atomic.AddUint64(&m.currentIdx, 1) - 1

	for i := 0; i < total; i++ {
		idx := int(startIdx+uint64(i)) % total
		info := m.tokenInfos[idx]

		// Check if dead (atomic read, no lock)
		if atomic.LoadInt32(&info.dead) == 1 {
			continue
		}

		// Check if quota exhausted
		if atomic.LoadInt32(&info.exhausted) == 1 {
			// Kiểm tra xem đã sang ngày mới chưa
			if !m.isNewDay(info.exhaustedAt) {
				continue
			}
			// Reset cho ngày mới
			atomic.StoreInt32(&info.exhausted, 0)
			atomic.StoreInt32(&info.failCount, 0)
			atomic.AddInt32(&m.exhaustedCount, -1)
		}

		// Check if token is valid (read without lock - slight race is OK)
		if info.AccessToken != "" && time.Now().Before(info.ExpiresAt) {
			return info.AccessToken, nil
		}

		// Need to refresh (with per-token lock)
		if err := m.refreshAccessToken(info); err != nil {
			// Mark as dead
			if atomic.CompareAndSwapInt32(&info.dead, 0, 1) {
				atomic.AddInt32(&m.deadCount, 1)
			}
			continue
		}

		return info.AccessToken, nil
	}

	return "", ErrAllTokensDead
}

// MarkQuotaExhausted marks a token as quota exhausted after consecutive 500 errors
// Returns true if token was marked as exhausted
func (m *Manager) MarkQuotaExhausted(accessToken string) bool {
	total := int(atomic.LoadInt32(&m.totalTokens))

	for idx := 0; idx < total; idx++ {
		info := m.tokenInfos[idx]
		if info.AccessToken == accessToken {
			// Tăng fail count
			newCount := atomic.AddInt32(&info.failCount, 1)

			// Nếu vượt threshold, đánh dấu exhausted
			if newCount >= QuotaExhaustedThreshold {
				if atomic.CompareAndSwapInt32(&info.exhausted, 0, 1) {
					info.exhaustedAt = time.Now()
					atomic.AddInt32(&m.exhaustedCount, 1)
					return true
				}
			}
			return false
		}
	}
	return false
}

// ResetFailCount resets the fail count for a token (call on successful API response)
func (m *Manager) ResetFailCount(accessToken string) {
	total := int(atomic.LoadInt32(&m.totalTokens))

	for idx := 0; idx < total; idx++ {
		info := m.tokenInfos[idx]
		if info.AccessToken == accessToken {
			atomic.StoreInt32(&info.failCount, 0)
			return
		}
	}
}

// MarkDead marks a token as dead (lock-free)
func (m *Manager) MarkDead(accessToken string) {
	total := int(atomic.LoadInt32(&m.totalTokens))

	for idx := 0; idx < total; idx++ {
		info := m.tokenInfos[idx]
		if info.AccessToken == accessToken {
			// Atomic CAS - only increment deadCount once
			if atomic.CompareAndSwapInt32(&info.dead, 0, 1) {
				atomic.AddInt32(&m.deadCount, 1)
			}
			return
		}
	}
}

// Rotate moves to the next token (for load balancing) - now no-op since GetToken auto-rotates
func (m *Manager) Rotate() {
	// No-op - GetToken already uses atomic increment for round-robin
}

// Stats returns token statistics
func (m *Manager) Stats() (total, alive, dead int) {
	total = int(atomic.LoadInt32(&m.totalTokens))
	dead = int(atomic.LoadInt32(&m.deadCount))
	alive = total - dead
	return
}

// FullStats returns full token statistics including exhausted
func (m *Manager) FullStats() (total, alive, dead, exhausted int) {
	total = int(atomic.LoadInt32(&m.totalTokens))
	dead = int(atomic.LoadInt32(&m.deadCount))
	exhausted = int(atomic.LoadInt32(&m.exhaustedCount))
	alive = total - dead - exhausted
	return
}

// HasAliveTokens returns true if there are still alive tokens
func (m *Manager) HasAliveTokens() bool {
	dead := atomic.LoadInt32(&m.deadCount)
	exhausted := atomic.LoadInt32(&m.exhaustedCount)
	total := atomic.LoadInt32(&m.totalTokens)
	return (dead + exhausted) < total
}

// ============== Token Queue Mode ==============

// InitQueue initializes token queue mode
// Call this after LoadFromFile to enable queue mode
func (m *Manager) InitQueue() {
	m.tokenQueue = make(chan *TokenInfo, len(m.tokenInfos))
	m.queueMode = true

	// Push all tokens into queue
	for _, token := range m.tokenInfos {
		if atomic.LoadInt32(&token.dead) == 0 {
			m.tokenQueue <- token
		}
	}
}

// AcquireToken gets a token from queue (blocks if empty)
// Returns nil if queue is closed (no more tokens)
func (m *Manager) AcquireToken() *TokenInfo {
	token, ok := <-m.tokenQueue
	if !ok {
		return nil // Queue closed
	}
	return token
}

// TryAcquireToken tries to get a token without blocking
// Returns nil immediately if no token available
func (m *Manager) TryAcquireToken() *TokenInfo {
	select {
	case token := <-m.tokenQueue:
		return token
	default:
		return nil
	}
}

// ReleaseToken returns a token to the queue (for reuse)
func (m *Manager) ReleaseToken(token *TokenInfo) {
	if token == nil {
		return
	}
	// Only return alive and non-exhausted tokens
	if atomic.LoadInt32(&token.dead) == 0 && atomic.LoadInt32(&token.exhausted) == 0 {
		select {
		case m.tokenQueue <- token:
			// Token returned to queue
		default:
			// Queue full, token lost (shouldn't happen)
		}
	}
}

// MarkDeadAndRelease marks token as dead (don't return to queue)
func (m *Manager) MarkDeadAndRelease(token *TokenInfo) {
	if token == nil {
		return
	}
	if atomic.CompareAndSwapInt32(&token.dead, 0, 1) {
		atomic.AddInt32(&m.deadCount, 1)
	}
	// Don't return to queue - token is dead
}

// RefreshTokenDirect refreshes a specific token (for queue mode)
func (m *Manager) RefreshTokenDirect(token *TokenInfo) error {
	return m.refreshAccessToken(token)
}

// GetAccessToken returns current access token from TokenInfo
// Refreshes if expired
func (m *Manager) GetAccessToken(token *TokenInfo) (string, error) {
	if token == nil {
		return "", ErrNoTokensAvailable
	}

	// Check if token is dead
	if atomic.LoadInt32(&token.dead) == 1 {
		return "", fmt.Errorf("token is dead")
	}

	// Check if token is valid
	if token.AccessToken != "" && time.Now().Before(token.ExpiresAt) {
		return token.AccessToken, nil
	}

	// Need refresh
	if err := m.refreshAccessToken(token); err != nil {
		return "", err
	}

	return token.AccessToken, nil
}

// QueueLen returns current queue length
func (m *Manager) QueueLen() int {
	if m.tokenQueue == nil {
		return 0
	}
	return len(m.tokenQueue)
}

// CloseQueue closes the token queue (signals workers to stop)
func (m *Manager) CloseQueue() {
	if m.tokenQueue != nil {
		close(m.tokenQueue)
	}
}

// IsQueueMode returns true if queue mode is enabled
func (m *Manager) IsQueueMode() bool {
	return m.queueMode
}

// ============== Refresh Token Saving ==============

// StartRefreshTokenSaver starts a goroutine to save refreshed tokens to file
func (m *Manager) StartRefreshTokenSaver(outputPath string) {
	m.outputFilePath = outputPath
	m.refreshedTokens = make(chan *TokenInfo, 10000)

	m.saveWg.Add(1)
	go m.saveRefreshedTokensLoop()
}

// saveRefreshedTokensLoop continuously saves refreshed tokens to file
func (m *Manager) saveRefreshedTokensLoop() {
	defer m.saveWg.Done()

	// Open file for appending
	file, err := os.OpenFile(m.outputFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("[ERROR] Failed to open refresh token output file: %v\n", err)
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case token, ok := <-m.refreshedTokens:
			if !ok {
				// Channel closed, flush and exit
				writer.Flush()
				return
			}
			// Write token in format: username:password|refresh_token|TENANT_ID
			line := fmt.Sprintf("%s:%s|%s|%s\n",
				token.Username,
				token.Password,
				token.RefreshToken,
				token.TenantID)
			writer.WriteString(line)

		case <-ticker.C:
			// Periodic flush
			writer.Flush()
		}
	}
}

// StopRefreshTokenSaver stops the saver and waits for it to finish
func (m *Manager) StopRefreshTokenSaver() {
	if m.refreshedTokens != nil {
		close(m.refreshedTokens)
		m.saveWg.Wait()
	}
}

// queueRefreshedToken queues a token to be saved (non-blocking)
func (m *Manager) queueRefreshedToken(token *TokenInfo) {
	if m.refreshedTokens == nil {
		return
	}
	select {
	case m.refreshedTokens <- token:
		// Queued successfully
	default:
		// Channel full, skip (shouldn't happen with 10K buffer)
	}
}

// SaveAliveTokens saves all alive tokens to a file
// This allows resuming with only alive tokens on next run
func (m *Manager) SaveAliveTokens(filename string) (int, error) {
	file, err := os.Create(filename)
	if err != nil {
		return 0, fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	count := 0

	for _, token := range m.tokenInfos {
		// Only save alive tokens
		if atomic.LoadInt32(&token.dead) == 0 {
			// Write in original format: username:password|refresh_token|TENANT_ID
			line := fmt.Sprintf("%s:%s|%s|%s\n",
				token.Username,
				token.Password,
				token.RefreshToken,
				token.TenantID)
			writer.WriteString(line)
			count++
		}
	}

	if err := writer.Flush(); err != nil {
		return count, fmt.Errorf("failed to flush: %w", err)
	}

	return count, nil
}
