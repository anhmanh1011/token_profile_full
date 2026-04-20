package worker

import (
	"context"
	"fmt"
	"linkedin_fetcher/api"
	"linkedin_fetcher/progress"
	"linkedin_fetcher/reader"
	"linkedin_fetcher/token"
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
	tokenManager *token.Manager
	resultWriter *writer.ResultWriter
	bitmap       *progress.Bitmap

	// Channels - use larger buffer
	jobsChan chan reader.EmailJob

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

// NewPool creates a new worker pool. bitmap may be nil to disable progress
// tracking (all emails are processed without skip/set).
func NewPool(numWorkers int, apiClient *api.Client, tokenManager *token.Manager, resultWriter *writer.ResultWriter, bufferSize int, maxCPM int, bitmap *progress.Bitmap) *Pool {
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
		bitmap:       bitmap,
		jobsChan:     make(chan reader.EmailJob, bufferSize),
		limiter:      limiter,
		failReasons:  make(map[string]*uint64),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// markDone sets the bitmap bit for a terminal outcome.
func (p *Pool) markDone(job reader.EmailJob) {
	if p.bitmap != nil {
		p.bitmap.Set(job.LineIdx)
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

// Submit submits an email job for processing
func (p *Pool) Submit(job reader.EmailJob) bool {
	if atomic.LoadInt32(&p.closed) == 1 {
		return false
	}

	select {
	case p.jobsChan <- job:
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
	p.tokenManager.CloseQueue()
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

// safeRequeue safely re-queues a job, avoiding send on closed channel
func (p *Pool) safeRequeue(job reader.EmailJob) {
	if atomic.LoadInt32(&p.closed) == 1 {
		return // Channel closed, drop the job
	}
	select {
	case p.jobsChan <- job:
		// Successfully re-queued
	default:
		// Channel full or closed, drop the job
	}
}

// worker is the worker goroutine — each worker holds one token from the queue
// and processes emails with it until the token dies.
func (p *Pool) worker() {
	defer p.wg.Done()

	for {
		// 1. Acquire token from queue (blocks until available or closed)
		tok := p.tokenManager.AcquireToken()
		if tok == nil {
			// Queue closed, no more tokens
			return
		}

		// 2. Process emails with this token until it dies.
		//    GetAccessToken lazily exchanges refresh_token → access_token and
		//    re-exchanges when the cached access_token nears expiry.
		tokenDead := false
		for job := range p.jobsChan {
			if tokenDead {
				// Re-queue job and break to get new token
				p.safeRequeue(job)
				break
			}

			atomic.AddUint64(&p.processed, 1)

			accessToken, err := p.tokenManager.GetAccessToken(tok)
			if err != nil {
				tokenDead = true
				p.tokenManager.MarkDeadAndRelease(tok)
				log.Printf("[TOKEN] Exchange failed: %v — getting new token...", err)
				p.safeRequeue(job)
				break
			}

			// Wait for rate limit
			if err := p.waitForRateLimit(); err != nil {
				// Context cancelled
				p.safeRequeue(job)
				return
			}

			// Make API call
			result, statusCode, err := p.apiClient.FetchProfile(job.Email, accessToken)

			// Handle token death (401/424)
			if statusCode == 401 || statusCode == 424 {
				tokenDead = true
				p.tokenManager.MarkDeadAndRelease(tok)
				total, alive, dead := p.tokenManager.Stats()
				log.Printf("[TOKEN] Token dead (status %d)! Total: %d, Alive: %d, Dead: %d", statusCode, total, alive, dead)
				// Re-queue email to be processed with new token
				p.safeRequeue(job)
				break
			}

			// Handle 403 - rate limit, backoff before continuing
			if statusCode == 403 {
				p.trackFailReason("status_403")
				atomic.AddUint64(&p.failed, 1)
				p.markDone(job)
				time.Sleep(500 * time.Millisecond) // Backoff khi bị rate limit
				continue
			}

			// Handle 500 - token quota exhausted: mark exhausted + rotate token.
			// Sau threshold fail liên tiếp, token được gửi vào deadChan để delete user.
			if statusCode == 500 {
				p.trackFailReason("status_500")
				atomic.AddUint64(&p.failed, 1)
				if p.tokenManager.MarkQuotaExhausted(tok) {
					tokenDead = true
					_, alive, _, exhausted := p.tokenManager.FullStats()
					log.Printf("[TOKEN] Token exhausted (consecutive 500s)! Alive: %d, Exhausted: %d", alive, exhausted)
					p.safeRequeue(job)
					break
				}
				// Below threshold: email consumed a request with this token; mark terminal.
				p.markDone(job)
				time.Sleep(200 * time.Millisecond)
				continue
			}

			// Handle other 5xx - transient server error: backoff + continue with same token.
			if statusCode >= 500 {
				p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
				atomic.AddUint64(&p.failed, 1)
				p.markDone(job)
				time.Sleep(200 * time.Millisecond)
				continue
			}

			// Success
			if err == nil {
				// Reset fail count khi request thành công
				p.tokenManager.ResetFailCount(tok)

				atomic.AddUint64(&p.successful, 1)
				if result != nil {
					atomic.AddUint64(&p.exactMatch, 1)
					if writeErr := p.resultWriter.WriteResult(result); writeErr != nil {
						log.Printf("[WRITER] Failed to write result: %v", writeErr)
					}
				}
				p.markDone(job)
				continue
			}

			// Other errors
			if statusCode > 0 {
				p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
			} else {
				p.trackFailReason("connection_error")
			}
			atomic.AddUint64(&p.failed, 1)
			p.markDone(job)
		}

		// Token was dead or jobsChan closed, return token if still alive
		if !tokenDead {
			p.tokenManager.ReleaseToken(tok)
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

				// Progress: prefer bitmap-backed total if available.
				progress := float64(0)
				var doneLabel string
				if p.bitmap != nil && p.bitmap.TotalLines() > 0 {
					done := p.bitmap.Done()
					progress = float64(done) / float64(p.bitmap.TotalLines()) * 100
					doneLabel = fmt.Sprintf("Done: %d/%d", done, p.bitmap.TotalLines())
				} else if totalEmails > 0 {
					progress = float64(processed) / float64(totalEmails) * 100
					doneLabel = fmt.Sprintf("Processed: %d/%d", processed, totalEmails)
				} else {
					doneLabel = fmt.Sprintf("Processed: %d", processed)
				}

				successPct := float64(0)
				if processed > 0 {
					successPct = float64(successful) / float64(processed) * 100
				}

				// Token stats
				totalTokens, aliveTokens, deadTokens, exhaustedTokens := p.tokenManager.FullStats()

				log.Printf("[PROGRESS] %.2f%% | %s | Success: %d (%.1f%%) | Failed: %d | HIT: %d | CPM: %.0f | RealCPM: %.0f | Tokens[A:%d/D:%d/E:%d/%d]",
					progress, doneLabel, successful, successPct, failed, exactMatch, successCPM, realtimeCPM,
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
