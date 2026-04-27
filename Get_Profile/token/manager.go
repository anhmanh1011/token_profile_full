package token

import (
	"errors"
	"fmt"
	"linkedin_fetcher/proxy"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNoTokensAvailable = errors.New("no tokens available")
)

const (
	// QuotaExhaustedThreshold — số lần fail 500 liên tiếp trước khi đánh dấu token
	// exhausted và delete user. Đặt thấp để kill nhanh token đã hết quota,
	// tránh lãng phí CPM vào token hỏng.
	QuotaExhaustedThreshold = 5

	// accessTokenRefreshMargin — refresh access_token nếu sắp hết hạn trong khoảng này.
	accessTokenRefreshMargin = 60 * time.Second
)

// TokenInfo stores the parsed token information.
// RefreshToken is stable for the ~24h refresh_token TTL; AccessToken is
// minted from it via ExchangeRefreshToken and cached with ExpiresAt until
// it nears expiry, then re-minted using the same RefreshToken.
type TokenInfo struct {
	Username     string
	TenantID     string
	RefreshToken string

	mu          sync.Mutex // guards AccessToken + ExpiresAt swap during lazy exchange
	AccessToken string
	ExpiresAt   time.Time

	dead        int32     // atomic flag: 0=alive, 1=dead
	exhausted   int32     // atomic flag: 0=có quota, 1=hết quota
	failCount   int32     // atomic: số lần fail liên tiếp với status 500
	exhaustedAt time.Time // thời điểm bị đánh dấu exhausted
}

// Manager handles token acquisition from a channel-backed queue.
type Manager struct {
	tokenInfos []*TokenInfo

	// HTTP client used for refresh_token → access_token exchange.
	exchangeClient *http.Client

	// Token queue — workers acquire/release here.
	tokenQueue chan *TokenInfo
	queueMode  bool

	// Statistics
	totalTokens    int32
	deadCount      int32
	exhaustedCount int32

	// Dead token notification channel (for user cleanup).
	deadChan chan<- string
}

// NewManager creates a new token manager with no proxy.
func NewManager() *Manager {
	return NewManagerWithProxy("")
}

// NewManagerWithProxy creates a token manager whose refresh_token →
// access_token exchange traffic flows through the SOCKS5 proxy at proxyURL.
// Pass an empty string to dial directly. Malformed proxy URLs degrade to
// direct dialing with a logged error rather than failing startup.
func NewManagerWithProxy(proxyURL string) *Manager {
	dial, err := proxy.SOCKS5DialContext(proxy.Parse(proxyURL), 5*time.Second, 30*time.Second)
	if err != nil {
		slog.Error("token/manager: failed to build SOCKS5 dialer, falling back to direct", "err", err)
		dial, _ = proxy.SOCKS5DialContext("", 5*time.Second, 30*time.Second)
	}
	return &Manager{
		tokenInfos: make([]*TokenInfo, 0),
		exchangeClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext:           dial,
				MaxIdleConns:          200,
				MaxIdleConnsPerHost:   100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// SetDeadChan sets the channel for dead token email notifications.
func (m *Manager) SetDeadChan(ch chan<- string) {
	m.deadChan = ch
}

// AddToken adds a single token to the manager and pushes it to the queue
// if queue mode is active. Used for lazy loading from API.
func (m *Manager) AddToken(t *TokenInfo) {
	m.tokenInfos = append(m.tokenInfos, t)
	atomic.AddInt32(&m.totalTokens, 1)
	if m.queueMode && m.tokenQueue != nil {
		select {
		case m.tokenQueue <- t:
		default:
		}
	}
}

// MarkQuotaExhausted increments the consecutive-500 counter for token; when it
// reaches QuotaExhaustedThreshold, marks the token exhausted and notifies the
// dead channel for user cleanup. Returns true if the threshold was just crossed.
func (m *Manager) MarkQuotaExhausted(token *TokenInfo) bool {
	if token == nil {
		return false
	}
	newCount := atomic.AddInt32(&token.failCount, 1)
	if newCount < QuotaExhaustedThreshold {
		return false
	}
	if !atomic.CompareAndSwapInt32(&token.exhausted, 0, 1) {
		return false
	}
	token.exhaustedAt = time.Now()
	atomic.AddInt32(&m.exhaustedCount, 1)
	if m.deadChan != nil && token.Username != "" {
		select {
		case m.deadChan <- token.Username:
		default:
		}
	}
	return true
}

// ResetFailCount resets the fail count for a token (call on successful API response).
func (m *Manager) ResetFailCount(token *TokenInfo) {
	if token == nil {
		return
	}
	atomic.StoreInt32(&token.failCount, 0)
}

// Stats returns token statistics.
func (m *Manager) Stats() (total, alive, dead int) {
	total = int(atomic.LoadInt32(&m.totalTokens))
	dead = int(atomic.LoadInt32(&m.deadCount))
	alive = total - dead
	return
}

// FullStats returns full token statistics including exhausted.
func (m *Manager) FullStats() (total, alive, dead, exhausted int) {
	total = int(atomic.LoadInt32(&m.totalTokens))
	dead = int(atomic.LoadInt32(&m.deadCount))
	exhausted = int(atomic.LoadInt32(&m.exhaustedCount))
	alive = total - dead - exhausted
	return
}

// HasAliveTokens returns true if there are still alive tokens.
func (m *Manager) HasAliveTokens() bool {
	dead := atomic.LoadInt32(&m.deadCount)
	exhausted := atomic.LoadInt32(&m.exhaustedCount)
	total := atomic.LoadInt32(&m.totalTokens)
	return (dead + exhausted) < total
}

// InitEmptyQueue initializes queue mode with an empty queue.
// Tokens are added later via AddToken().
func (m *Manager) InitEmptyQueue(bufferSize int) {
	m.tokenQueue = make(chan *TokenInfo, bufferSize)
	m.queueMode = true
}

// AcquireToken gets a token from queue (blocks until available or closed).
// Returns nil if queue is closed.
func (m *Manager) AcquireToken() *TokenInfo {
	token, ok := <-m.tokenQueue
	if !ok {
		return nil
	}
	return token
}

// ReleaseToken returns a token to the queue for reuse.
func (m *Manager) ReleaseToken(token *TokenInfo) {
	if token == nil {
		return
	}
	if atomic.LoadInt32(&token.dead) == 0 {
		select {
		case m.tokenQueue <- token:
		default:
		}
	}
}

// MarkDeadAndRelease marks token as dead (don't return to queue)
// and notifies the dead channel for user cleanup.
func (m *Manager) MarkDeadAndRelease(token *TokenInfo) {
	if token == nil {
		return
	}
	if atomic.CompareAndSwapInt32(&token.dead, 0, 1) {
		atomic.AddInt32(&m.deadCount, 1)
		if m.deadChan != nil && token.Username != "" {
			select {
			case m.deadChan <- token.Username:
			default:
			}
		}
	}
}

// GetAccessToken returns a valid Loki access_token for the given token,
// lazily exchanging the refresh_token when the cached access_token is missing
// or about to expire. Returns an error when the token is dead or the exchange
// fails permanently (refresh_token revoked).
func (m *Manager) GetAccessToken(token *TokenInfo) (string, error) {
	if token == nil {
		return "", ErrNoTokensAvailable
	}

	if atomic.LoadInt32(&token.dead) == 1 {
		return "", fmt.Errorf("token is dead")
	}

	token.mu.Lock()
	defer token.mu.Unlock()

	if token.AccessToken != "" && time.Now().Before(token.ExpiresAt.Add(-accessTokenRefreshMargin)) {
		return token.AccessToken, nil
	}

	if token.RefreshToken == "" {
		return "", fmt.Errorf("token has no refresh_token")
	}

	accessToken, expiresIn, err := ExchangeRefreshToken(m.exchangeClient, token.TenantID, token.RefreshToken)
	if err != nil {
		return "", err
	}

	token.AccessToken = accessToken
	token.ExpiresAt = time.Now().Add(expiresIn)
	slog.Info("token/manager: exchanged refresh_token",
		"user", token.Username,
		"expires_in", expiresIn,
	)
	return accessToken, nil
}

// QueueLen returns current queue length.
func (m *Manager) QueueLen() int {
	if m.tokenQueue == nil {
		return 0
	}
	return len(m.tokenQueue)
}

// CloseQueue closes the token queue (signals workers to stop).
func (m *Manager) CloseQueue() {
	if m.tokenQueue != nil {
		close(m.tokenQueue)
	}
}
