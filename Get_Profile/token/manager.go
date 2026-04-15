package token

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
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
	Username    string
	Password    string
	TenantID    string
	AccessToken string
	ExpiresAt   time.Time
	dead        int32     // atomic flag: 0=alive, 1=dead
	exhausted   int32     // atomic flag: 0=có quota, 1=hết quota
	failCount   int32     // atomic: số lần fail liên tiếp với status 500
	exhaustedAt time.Time // thời điểm bị đánh dấu exhausted
}

// Manager handles token rotation and management
type Manager struct {
	tokenInfos []*TokenInfo
	currentIdx uint64 // atomic, use uint64 for better atomic ops

	// Token Queue for new architecture
	tokenQueue chan *TokenInfo
	queueMode  bool

	// Statistics
	totalTokens    int32
	deadCount      int32
	exhaustedCount int32

	// Dead token notification channel (for Redis cleanup)
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

// LoadFromSlice loads tokens from a pre-parsed slice (from Redis).
func (m *Manager) LoadFromSlice(tokens []*TokenInfo) error {
	if len(tokens) == 0 {
		return ErrNoTokensAvailable
	}

	m.tokenInfos = tokens
	m.totalTokens = int32(len(tokens))
	return nil
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

// LoadFromFile loads tokens from a file
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

		// Parse format: username:pass|refresh_token|TENANT_ID
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			continue
		}

		credentials := parts[0]
		tenantID := strings.TrimSpace(parts[2])

		credParts := strings.SplitN(credentials, ":", 2)
		if len(credParts) != 2 {
			continue
		}

		tokenInfo := &TokenInfo{
			Username: strings.TrimSpace(credParts[0]),
			Password: strings.TrimSpace(credParts[1]),
			TenantID: tenantID,
		}

		m.tokenInfos = append(m.tokenInfos, tokenInfo)
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

		// Token expired — mark dead, try next
		if atomic.CompareAndSwapInt32(&info.dead, 0, 1) {
			atomic.AddInt32(&m.deadCount, 1)
		}
		continue
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

			// Nếu vượt threshold, đánh dấu exhausted và trigger delete user
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
			if atomic.CompareAndSwapInt32(&info.dead, 0, 1) {
				atomic.AddInt32(&m.deadCount, 1)
				if m.deadChan != nil && info.Username != "" {
					select {
					case m.deadChan <- info.Username:
					default:
					}
				}
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

// InitEmptyQueue initializes queue mode with an empty queue.
// Tokens are added later via AddToken().
func (m *Manager) InitEmptyQueue(bufferSize int) {
	m.tokenQueue = make(chan *TokenInfo, bufferSize)
	m.queueMode = true
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
	// Only return alive tokens
	if atomic.LoadInt32(&token.dead) == 0 {
		select {
		case m.tokenQueue <- token:
			// Token returned to queue
		default:
			// Queue full, token lost (shouldn't happen)
		}
	}
}

// MarkDeadAndRelease marks token as dead (don't return to queue)
// and notifies the dead channel for Redis cleanup.
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

	// Check if token is dead
	if atomic.LoadInt32(&token.dead) == 1 {
		return "", fmt.Errorf("token is dead")
	}

	// Check if token is valid
	if token.AccessToken != "" && time.Now().Before(token.ExpiresAt) {
		return token.AccessToken, nil
	}

	// Token expired — no refresh, caller gets new token from API
	return "", fmt.Errorf("token expired")
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

