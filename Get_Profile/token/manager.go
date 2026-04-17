package token

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

var (
	ErrNoTokensAvailable = errors.New("no tokens available")
)

const (
	// QuotaExhaustedThreshold — số lần fail liên tiếp với status 500
	// trước khi đánh dấu token exhausted và trigger delete user.
	QuotaExhaustedThreshold = 5
)

// TokenInfo stores the parsed token information
type TokenInfo struct {
	Username    string
	TenantID    string
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

// NewManager creates a new token manager
func NewManager() *Manager {
	return &Manager{
		tokenInfos: make([]*TokenInfo, 0),
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

// MarkQuotaExhausted marks a token as quota exhausted after consecutive 500 errors.
// Returns true if token was marked as exhausted.
func (m *Manager) MarkQuotaExhausted(accessToken string) bool {
	total := int(atomic.LoadInt32(&m.totalTokens))

	for idx := 0; idx < total; idx++ {
		info := m.tokenInfos[idx]
		if info.AccessToken == accessToken {
			newCount := atomic.AddInt32(&info.failCount, 1)

			if newCount >= QuotaExhaustedThreshold {
				if atomic.CompareAndSwapInt32(&info.exhausted, 0, 1) {
					info.exhaustedAt = time.Now()
					atomic.AddInt32(&m.exhaustedCount, 1)
					if m.deadChan != nil && info.Username != "" {
						select {
						case m.deadChan <- info.Username:
						default:
						}
					}
					return true
				}
			}
			return false
		}
	}
	return false
}

// ResetFailCount resets the fail count for a token (call on successful API response).
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

// GetAccessToken returns current access token from TokenInfo.
// Returns error if token is dead or expired (caller should mark dead and get new one).
func (m *Manager) GetAccessToken(token *TokenInfo) (string, error) {
	if token == nil {
		return "", ErrNoTokensAvailable
	}

	if atomic.LoadInt32(&token.dead) == 1 {
		return "", fmt.Errorf("token is dead")
	}

	if token.AccessToken != "" && time.Now().Before(token.ExpiresAt) {
		return token.AccessToken, nil
	}

	return "", fmt.Errorf("token expired")
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
