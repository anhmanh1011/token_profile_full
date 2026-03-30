package worker_hot

import (
	"context"
	"fmt"
	"linkedin_fetcher/api"
	"linkedin_fetcher/models"
	"linkedin_fetcher/token_hot"
	"linkedin_fetcher/writer"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Pool manages a pool of workers for concurrent API calls
type Pool struct {
	numWorkers   int
	apiClient    *api.Client
	tokenManager *token_hot.Manager
	resultWriter *writer.ResultWriter

	// Channels - use larger buffer
	jobsChan chan string

	// Rate limiter
	limiter *rate.Limiter

	// Statistics - use uint64 for better atomic perf on 64-bit
	processed  uint64
	successful uint64
	failed     uint64
	exactMatch uint64

	// Fail reason tracking
	failReasons   map[string]*uint64
	failReasonsMu sync.RWMutex

	// Control
	wg           sync.WaitGroup
	ctx          context.Context
	cancel       context.CancelFunc
	closed       int32
	closeOnce    sync.Once
	stopReason   string
	stoppedEarly int32 // Flag: stopped due to no tokens
}

// NewPool creates a new worker pool
func NewPool(numWorkers int, apiClient *api.Client, tokenManager *token_hot.Manager, resultWriter *writer.ResultWriter, bufferSize int, maxCPM int) *Pool {
	ctx, cancel := context.WithCancel(context.Background())

	// Use larger buffer for better throughput
	if bufferSize < 50000 {
		bufferSize = 50000
	}

	// Create rate limiter if maxCPM > 0
	var limiter *rate.Limiter
	if maxCPM > 0 {
		// Convert CPM to requests per second
		rps := float64(maxCPM) / 60.0
		// Burst size = 1 second worth of requests
		burst := int(rps)
		if burst < 1 {
			burst = 1
		}
		limiter = rate.NewLimiter(rate.Limit(rps), burst)
		log.Printf("[RATE_LIMIT] Enabled: %d CPM (%.1f req/s, burst: %d)", maxCPM, rps, burst)
	}

	return &Pool{
		numWorkers:   numWorkers,
		apiClient:    apiClient,
		tokenManager: tokenManager,
		resultWriter: resultWriter,
		jobsChan:     make(chan string, bufferSize),
		limiter:      limiter,
		failReasons:  make(map[string]*uint64),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// waitForRateLimit waits for rate limiter if enabled
func (p *Pool) waitForRateLimit() error {
	if p.limiter != nil {
		return p.limiter.Wait(p.ctx)
	}
	return nil
}

// trackFailReason tracks the reason for failure
func (p *Pool) trackFailReason(reason string) {
	p.failReasonsMu.Lock()
	if p.failReasons[reason] == nil {
		var zero uint64
		p.failReasons[reason] = &zero
	}
	atomic.AddUint64(p.failReasons[reason], 1)
	p.failReasonsMu.Unlock()
}

// GetFailReasons returns failure reason statistics
func (p *Pool) GetFailReasons() map[string]uint64 {
	p.failReasonsMu.RLock()
	defer p.failReasonsMu.RUnlock()

	result := make(map[string]uint64)
	for k, v := range p.failReasons {
		result[k] = atomic.LoadUint64(v)
	}
	return result
}

// Start starts all workers
func (p *Pool) Start() {
	for i := 0; i < p.numWorkers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	log.Printf("[POOL] Started %d workers", p.numWorkers)
}

// Submit submits an email for processing
func (p *Pool) Submit(email string) bool {
	if atomic.LoadInt32(&p.closed) == 1 {
		return false
	}

	select {
	case p.jobsChan <- email:
		return true
	case <-p.ctx.Done():
		return false
	}
}

// Close closes the jobs channel and waits for workers to finish
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		atomic.StoreInt32(&p.closed, 1)
		close(p.jobsChan)
	})
	p.wg.Wait()
}

// Shutdown gracefully shuts down the pool
func (p *Pool) Shutdown() {
	p.cancel()
	p.Close()
}

// ShutdownWithReason gracefully shuts down with a reason
func (p *Pool) ShutdownWithReason(reason string) {
	p.stopReason = reason
	atomic.StoreInt32(&p.stoppedEarly, 1)
	p.cancel()
	// Close token queue to unblock workers waiting at AcquireToken()
	p.tokenManager.CloseQueue()
	p.Close()
}

// StoppedEarly returns true if pool was stopped early (not due to completing all emails)
func (p *Pool) StoppedEarly() bool {
	return atomic.LoadInt32(&p.stoppedEarly) == 1
}

// StopReason returns the reason for stopping
func (p *Pool) StopReason() string {
	return p.stopReason
}

// safeRequeue safely re-queues an email, avoiding send on closed channel
func (p *Pool) safeRequeue(email string) {
	if atomic.LoadInt32(&p.closed) == 1 {
		return // Channel closed, drop the email
	}
	select {
	case p.jobsChan <- email:
		// Successfully re-queued
	default:
		// Channel full or closed, drop the email
	}
}

// worker is the worker goroutine - optimized for throughput
// Queue mode: each worker holds one token and processes multiple emails
func (p *Pool) worker() {
	defer p.wg.Done()

	if p.tokenManager.IsQueueMode() {
		p.workerWithTokenQueue()
	} else {
		// Legacy mode: round-robin tokens
		for email := range p.jobsChan {
			p.processEmail(email)
		}
	}
}

// workerWithTokenQueue - worker that holds a token from queue
func (p *Pool) workerWithTokenQueue() {
	for {
		// 1. Acquire token from queue (blocks until available or closed)
		token := p.tokenManager.AcquireToken()
		if token == nil {
			// Queue closed, no more tokens
			return
		}

		// 2. Refresh token before use
		accessToken, err := p.tokenManager.GetAccessToken(token)
		if err != nil {
			// Token dead on refresh, get another
			p.tokenManager.MarkDeadAndRelease(token)
			log.Printf("[TOKEN_REFRESH_FAIL] %s | Error: %v", token.Username, err)
			continue
		}

		// 3. Process emails with this token until it dies
		tokenDead := false
		for email := range p.jobsChan {
			if tokenDead {
				// Re-queue email and break to get new token
				p.safeRequeue(email)
				break
			}

			atomic.AddUint64(&p.processed, 1)

			// Check if access token expired, refresh
			if token.ExpiresAt.Before(time.Now().Add(30 * time.Second)) {
				newAccessToken, err := p.tokenManager.GetAccessToken(token)
				if err != nil {
					tokenDead = true
					p.tokenManager.MarkDeadAndRelease(token)
					log.Printf("[TOKEN_REFRESH_FAIL] %s | Mid-process refresh error: %v", token.Username, err)
					// Re-queue email
					p.safeRequeue(email)
					break
				}
				accessToken = newAccessToken
			}

			// Wait for rate limit
			if err := p.waitForRateLimit(); err != nil {
				// Context cancelled
				p.safeRequeue(email)
				return
			}

			// Make API call with retry for 401/424
			var result *models.Result
			var statusCode int
			var err error
			maxRetries := 5

			for retry := 0; retry < maxRetries; retry++ {
				result, statusCode, err = p.apiClient.FetchProfile(email, accessToken)

				// Success or non-retryable error
				if statusCode == 200 || (statusCode >= 400 && statusCode != 401 && statusCode != 424) {
					break
				}

				// 401/424 - retry with backoff
				if statusCode == 401 || statusCode == 424 {
					if retry < maxRetries-1 {
						backoff := time.Duration(500*(retry+1)) * time.Millisecond
						time.Sleep(backoff)
						continue
					}
				}
			}

			// Handle 4xx/5xx after all retries - mark token dead
			if statusCode >= 400 {
				p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
				tokenDead = true
				p.tokenManager.MarkDeadAndRelease(token)
				total, alive, dead := p.tokenManager.Stats()
				log.Printf("[TOKEN] Token dead (status %d, retried %d times)! Total: %d, Alive: %d, Dead: %d", statusCode, maxRetries, total, alive, dead)
				p.safeRequeue(email)
				break
			}

			// Success
			if err == nil {
				// Reset fail count khi request thành công
				p.tokenManager.ResetFailCount(accessToken)

				atomic.AddUint64(&p.successful, 1)
				if result != nil {
					atomic.AddUint64(&p.exactMatch, 1)
					if writeErr := p.resultWriter.WriteResult(result); writeErr != nil {
						log.Printf("[WRITER] Failed to write result: %v", writeErr)
					}
				}
				continue
			}

			// Other errors
			if statusCode > 0 {
				p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
			} else {
				p.trackFailReason("connection_error")
			}
			atomic.AddUint64(&p.failed, 1)
		}

		// Token was dead or jobsChan closed, return token if still alive
		if !tokenDead {
			p.tokenManager.ReleaseToken(token)
		}

		// Check if jobsChan is closed
		select {
		case <-p.ctx.Done():
			return
		default:
			if tokenDead {
				continue // Get new token
			}
			return // jobsChan closed normally
		}
	}
}

// processEmail processes an email (retry on 401/424/403 with new token)
func (p *Pool) processEmail(email string) {
	atomic.AddUint64(&p.processed, 1)

	maxRetries := 3
	retryCount := 0

	for {
		// Fast check for alive tokens
		if !p.tokenManager.HasAliveTokens() {
			p.trackFailReason("no_alive_tokens")
			atomic.AddUint64(&p.failed, 1)
			return
		}

		// Get token
		tkn, err := p.tokenManager.GetToken()
		if err != nil {
			p.trackFailReason("get_token_error")
			atomic.AddUint64(&p.failed, 1)
			return
		}

		// Wait for rate limit
		if err := p.waitForRateLimit(); err != nil {
			// Context cancelled
			return
		}

		// Make API call
		result, statusCode, err := p.apiClient.FetchProfile(email, tkn)

		// Handle 401/424 - token exhausted, mark dead and retry
		if statusCode == 401 || statusCode == 424 {
			p.tokenManager.MarkDead(tkn)
			total, alive, dead := p.tokenManager.Stats()
			log.Printf("[TOKEN] Token dead (status %d)! Total: %d, Alive: %d, Dead: %d", statusCode, total, alive, dead)
			continue // Always retry with new token, no limit
		}

		// Handle 403 - might be rate limit, retry with different token
		if statusCode == 403 {
			retryCount++
			if retryCount > maxRetries {
				p.trackFailReason("status_403")
				atomic.AddUint64(&p.failed, 1)
				return
			}
			time.Sleep(500 * time.Millisecond) // Backoff khi bị rate limit
			continue
		}

		// Handle 5xx server errors - retry
		if statusCode >= 500 {
			// Track quota exhaustion cho status 500
			if statusCode == 500 {
				exhausted := p.tokenManager.MarkQuotaExhausted(tkn)
				if exhausted {
					total, alive, dead, exhaustedCount := p.tokenManager.FullStats()
					log.Printf("[TOKEN] Token quota exhausted! Total: %d, Alive: %d, Dead: %d, Exhausted: %d", total, alive, dead, exhaustedCount)
				}
			}
			retryCount++
			if retryCount > maxRetries {
				p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
				atomic.AddUint64(&p.failed, 1)
				return
			}
			time.Sleep(200 * time.Millisecond) // Backoff khi server error
			continue
		}

		// Success
		if err == nil {
			// Reset fail count on successful API response
			p.tokenManager.ResetFailCount(tkn)

			atomic.AddUint64(&p.successful, 1)

			if result != nil {
				atomic.AddUint64(&p.exactMatch, 1)
				if writeErr := p.resultWriter.WriteResult(result); writeErr != nil {
					log.Printf("[WRITER] Failed to write result: %v", writeErr)
				}
			}
			return
		}

		// Other errors (4xx except 401/403/424, or connection errors)
		if statusCode > 0 {
			p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
		} else {
			p.trackFailReason("connection_error")
		}

		failCount := atomic.AddUint64(&p.failed, 1)
		if failCount <= 10 {
			log.Printf("[ERROR] API call failed for %s (status: %d): %v", email, statusCode, err)
		}
		return
	}
}

// Stats returns worker pool statistics
func (p *Pool) Stats() (processed, successful, failed, exactMatch int64) {
	return int64(atomic.LoadUint64(&p.processed)),
		int64(atomic.LoadUint64(&p.successful)),
		int64(atomic.LoadUint64(&p.failed)),
		int64(atomic.LoadUint64(&p.exactMatch))
}

// FullStats returns all statistics
func (p *Pool) FullStats() (processed, successful, failed, exactMatch int64) {
	return p.Stats()
}

// StartProgressReporter starts a goroutine to periodically report progress
func (p *Pool) StartProgressReporter(interval time.Duration, totalEmails int) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		startTime := time.Now()
		var lastSuccessful int64 = 0
		var lastTime = startTime

		// Report fail reasons every 30 seconds
		failReportTicker := time.NewTicker(30 * time.Second)
		defer failReportTicker.Stop()

		// Check for no tokens every 2 seconds
		tokenCheckTicker := time.NewTicker(2 * time.Second)
		defer tokenCheckTicker.Stop()

		for {
			select {
			case <-ticker.C:
				processed, successful, failed, exactMatch := p.FullStats()
				elapsed := time.Since(startTime)
				now := time.Now()

				// CPM calculation
				successCPM := float64(successful) / elapsed.Seconds() * 60

				// Real-time CPM
				intervalSuccessful := successful - lastSuccessful
				intervalDuration := now.Sub(lastTime).Seconds()
				realtimeCPM := float64(intervalSuccessful) / intervalDuration * 60

				lastSuccessful = successful
				lastTime = now

				// Progress
				progress := float64(0)
				if totalEmails > 0 {
					progress = float64(processed) / float64(totalEmails) * 100
				}

				successPct := float64(0)
				if processed > 0 {
					successPct = float64(successful) / float64(processed) * 100
				}

				// Token stats
				totalTokens, aliveTokens, deadTokens, exhaustedTokens := p.tokenManager.FullStats()

				log.Printf("[PROGRESS] %.1f%% | Processed: %d | Success: %d (%.1f%%) | Failed: %d | HIT: %d | CPM: %.0f | RealCPM: %.0f | Tokens[A:%d/D:%d/E:%d/%d]",
					progress, processed, successful, successPct, failed, exactMatch, successCPM, realtimeCPM,
					aliveTokens, deadTokens, exhaustedTokens, totalTokens)

			case <-tokenCheckTicker.C:
				// Check if all tokens are dead - stop early
				if !p.tokenManager.HasAliveTokens() {
					log.Printf("[STOP] All tokens are dead! Stopping application...")
					p.ShutdownWithReason("all_tokens_dead")
					return
				}

			case <-failReportTicker.C:
				// Report fail reasons
				reasons := p.GetFailReasons()
				if len(reasons) > 0 {
					log.Printf("[FAIL_STATS] Failure reasons: %v", reasons)
				}

			case <-p.ctx.Done():
				// Final fail report
				reasons := p.GetFailReasons()
				if len(reasons) > 0 {
					log.Printf("[FINAL_FAIL_STATS] Failure reasons: %v", reasons)
				}
				return
			}
		}
	}()
}
