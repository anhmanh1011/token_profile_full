# Get_Profile Redis Token Integration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace file-based token loading with Redis (`redis-tokens-{N}`) and add real-time dead token cleanup via HDEL.

**Architecture:** Minimal Redis Swap — only change token loading layer. New `token/redis.go` handles Redis I/O. Existing `token/manager.go` drops refresh-token-save logic, adds dead-token notification channel. `main.go` wires Redis in and removes file-based token flags.

**Tech Stack:** Go, `github.com/redis/go-redis/v9`, Redis Hash (HSET/HGETALL/HDEL)

**Spec:** `docs/superpowers/specs/2026-04-14-get-profile-redis-integration-design.md`

---

## File Map

| File | Action | Responsibility |
|------|--------|----------------|
| `Get_Profile/go.mod` | **Create** | Module definition + dependencies |
| `Get_Profile/token/redis.go` | **Create** | Redis client: connect, load tokens, batch HDEL cleanup goroutine |
| `Get_Profile/token/manager.go` | **Modify** | Remove refresh-save logic, add `deadChan` support |
| `Get_Profile/config/config.go` | **Modify** | Add `RedisAddr`, `RedisQueue`; remove `TokensFile` |
| `Get_Profile/main.go` | **Modify** | Wire Redis, new flags, remove file-token logic |

---

## Task 1: Initialize go.mod and add Redis dependency

**Files:**
- Create: `Get_Profile/go.mod`

- [ ] **Step 1: Create go.mod**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go mod init linkedin_fetcher
```

- [ ] **Step 2: Add dependencies**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go get github.com/redis/go-redis/v9
go get golang.org/x/time
go mod tidy
```

- [ ] **Step 3: Verify go.mod**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
cat go.mod
```

Expected: `module linkedin_fetcher` with `github.com/redis/go-redis/v9` and `golang.org/x/time` in require block.

- [ ] **Step 4: Commit**

```bash
cd "d:/vs code/token_profile_full/token_profile_full"
git add Get_Profile/go.mod Get_Profile/go.sum
git commit -m "feat(get-profile): init go.mod with redis dependency"
```

---

## Task 2: Create `token/redis.go` — RedisLoader

**Files:**
- Create: `Get_Profile/token/redis.go`

- [ ] **Step 1: Write `token/redis.go`**

```go
package token

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLoader handles loading tokens from Redis and cleaning up dead tokens.
type RedisLoader struct {
	client *redis.Client
}

// NewRedisLoader connects to Redis and verifies the connection with a ping.
// Returns error if Redis is unreachable.
func NewRedisLoader(addr string) (*RedisLoader, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		PoolSize:     10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("cannot connect to Redis at %s: %w", addr, err)
	}

	return &RedisLoader{client: client}, nil
}

// LoadTokens loads all tokens from a Redis Hash using HGETALL.
// Queue name format: "redis-tokens-{queueSuffix}" (e.g. suffix "1" -> "redis-tokens-1").
// Each hash entry: key = email, value = "email|refresh_token|tenant_id".
// Returns parsed TokenInfo slice. Skips entries with invalid format (logs warning).
func (r *RedisLoader) LoadTokens(queueSuffix string) ([]*TokenInfo, error) {
	queueName := fmt.Sprintf("redis-tokens-%s", queueSuffix)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := r.client.HGetAll(ctx, queueName).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s failed: %w", queueName, err)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no tokens found in %s", queueName)
	}

	tokens := make([]*TokenInfo, 0, len(result))
	skipped := 0

	for _, value := range result {
		parts := strings.SplitN(value, "|", 3)
		if len(parts) != 3 {
			skipped++
			continue
		}

		email := strings.TrimSpace(parts[0])
		refreshToken := strings.TrimSpace(parts[1])
		tenantID := strings.TrimSpace(parts[2])

		if email == "" || refreshToken == "" || tenantID == "" {
			skipped++
			continue
		}

		tokens = append(tokens, &TokenInfo{
			Username:     email,
			RefreshToken: refreshToken,
			TenantID:     tenantID,
		})
	}

	if skipped > 0 {
		log.Printf("[REDIS] Warning: skipped %d entries with invalid format in %s", skipped, queueName)
	}

	if len(tokens) == 0 {
		return nil, fmt.Errorf("no valid tokens found in %s (all %d entries had invalid format)", queueName, skipped)
	}

	log.Printf("[REDIS] Loaded %d tokens from %s", len(tokens), queueName)
	return tokens, nil
}

// DeleteTokens removes dead token entries from Redis Hash.
// Best-effort: logs errors but does not return them.
func (r *RedisLoader) DeleteTokens(queueSuffix string, emails []string) {
	if len(emails) == 0 {
		return
	}

	queueName := fmt.Sprintf("redis-tokens-%s", queueSuffix)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Convert []string to []interface{} for HDEL
	fields := make([]string, len(emails))
	copy(fields, emails)

	deleted, err := r.client.HDel(ctx, queueName, fields...).Result()
	if err != nil {
		log.Printf("[REDIS] Error deleting %d dead tokens from %s: %v", len(emails), queueName, err)
		return
	}

	log.Printf("[REDIS] Deleted %d dead tokens from %s", deleted, queueName)
}

// StartCleanupWorker runs a background goroutine that batches dead token emails
// from deadChan and deletes them from Redis.
// Flushes when: 50 emails collected OR 5 seconds since last flush.
// Stops when ctx is cancelled; drains remaining items on exit.
func (r *RedisLoader) StartCleanupWorker(ctx context.Context, queueSuffix string, deadChan <-chan string) {
	go func() {
		const batchSize = 50
		const flushInterval = 5 * time.Second

		batch := make([]string, 0, batchSize)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()

		flush := func() {
			if len(batch) > 0 {
				r.DeleteTokens(queueSuffix, batch)
				batch = batch[:0]
			}
		}

		for {
			select {
			case email, ok := <-deadChan:
				if !ok {
					// Channel closed, flush remaining
					flush()
					return
				}
				batch = append(batch, email)
				if len(batch) >= batchSize {
					flush()
				}

			case <-ticker.C:
				flush()

			case <-ctx.Done():
				// Drain remaining items from channel
				for email := range deadChan {
					batch = append(batch, email)
					if len(batch) >= batchSize {
						flush()
					}
				}
				flush()
				return
			}
		}
	}()
}

// Close closes the Redis connection.
func (r *RedisLoader) Close() error {
	return r.client.Close()
}
```

- [ ] **Step 2: Verify compilation**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go build ./token/
```

Expected: No errors.

- [ ] **Step 3: Commit**

```bash
cd "d:/vs code/token_profile_full/token_profile_full"
git add Get_Profile/token/redis.go
git commit -m "feat(get-profile): add RedisLoader for token loading and dead token cleanup"
```

---

## Task 3: Modify `config/config.go` — add Redis fields, remove TokensFile

**Files:**
- Modify: `Get_Profile/config/config.go`

- [ ] **Step 1: Update config struct and defaults**

Replace the entire file content with:

```go
package config

type Config struct {
	// File paths
	EmailsFile  string
	ResultsFile string

	// Redis settings
	RedisAddr  string
	RedisQueue string // Queue suffix, e.g. "1" -> redis-tokens-1

	// Worker settings
	NumWorkers int
	MaxWorkers int

	// API settings
	APITimeout int // seconds

	// Rate limiting
	MaxCPM int // Max requests per minute (0 = unlimited)

	// Buffer settings
	EmailBufferSize  int
	ResultBufferSize int
	FileBufferSize   int
}

func NewConfig() *Config {
	numWorkers := 1000
	maxWorkers := 3000

	return &Config{
		// File paths
		EmailsFile:  "emails.txt",
		ResultsFile: "result.txt",

		// Redis settings
		RedisAddr:  "localhost:6379",
		RedisQueue: "",

		// Worker settings
		NumWorkers: numWorkers,
		MaxWorkers: maxWorkers,

		// API settings
		APITimeout: 30,

		// Rate limiting (default 35K CPM, overridden in main.go to 20K)
		MaxCPM: 35000,

		// Buffer settings
		EmailBufferSize:  100000,
		ResultBufferSize: 10000,
		FileBufferSize:   4 * 1024 * 1024,
	}
}
```

- [ ] **Step 2: Verify compilation**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go build ./config/
```

Expected: No errors.

- [ ] **Step 3: Commit**

```bash
cd "d:/vs code/token_profile_full/token_profile_full"
git add Get_Profile/config/config.go
git commit -m "refactor(get-profile): replace TokensFile with RedisAddr/RedisQueue in config"
```

---

## Task 4: Modify `token/manager.go` — remove refresh-save, add deadChan

**Files:**
- Modify: `Get_Profile/token/manager.go:56-76` (Manager struct)
- Modify: `Get_Profile/token/manager.go:78-94` (NewManager)
- Modify: `Get_Profile/token/manager.go:245-249` (refreshAccessToken — remove queueRefreshedToken)
- Remove: `Get_Profile/token/manager.go:527-630` (entire refresh-save section)
- Add: `LoadFromSlice()` and `SetDeadChan()` methods

- [ ] **Step 1: Update Manager struct — remove refresh-save fields, add deadChan**

In `token/manager.go`, replace the Manager struct (lines 56-76) with:

```go
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

	// Dead token notification channel (for Redis cleanup)
	deadChan chan<- string
}
```

- [ ] **Step 2: Update NewManager — remove refresh-save initialization**

No change needed to `NewManager()` — the removed fields are initialized elsewhere (`StartRefreshTokenSaver`), not in the constructor.

- [ ] **Step 3: Add `SetDeadChan` method**

Add after `NewManager()` (after line 94):

```go
// SetDeadChan sets the channel for dead token email notifications.
// When a token is marked dead, its email is sent to this channel
// for Redis cleanup by the RedisLoader cleanup worker.
func (m *Manager) SetDeadChan(ch chan<- string) {
	m.deadChan = ch
}
```

- [ ] **Step 4: Add `LoadFromSlice` method**

Add after `SetDeadChan`:

```go
// LoadFromSlice loads tokens from a pre-parsed slice (from Redis).
func (m *Manager) LoadFromSlice(tokens []*TokenInfo) error {
	if len(tokens) == 0 {
		return ErrNoTokensAvailable
	}

	m.tokenInfos = tokens
	m.totalTokens = int32(len(tokens))
	return nil
}
```

- [ ] **Step 5: Remove `queueRefreshedToken` call from `refreshAccessToken`**

In `refreshAccessToken()` (lines 245-249), replace:

```go
		if tokenResp.RefreshToken != "" {
			info.RefreshToken = tokenResp.RefreshToken
			// Queue token for saving to file
			m.queueRefreshedToken(info)
		}
```

with:

```go
		if tokenResp.RefreshToken != "" {
			info.RefreshToken = tokenResp.RefreshToken
		}
```

- [ ] **Step 6: Update `MarkDeadAndRelease` to notify deadChan**

Replace `MarkDeadAndRelease` (lines 467-475) with:

```go
// MarkDeadAndRelease marks token as dead (don't return to queue)
// and notifies the dead channel for Redis cleanup.
func (m *Manager) MarkDeadAndRelease(token *TokenInfo) {
	if token == nil {
		return
	}
	if atomic.CompareAndSwapInt32(&token.dead, 0, 1) {
		atomic.AddInt32(&m.deadCount, 1)
		// Notify Redis cleanup goroutine
		if m.deadChan != nil && token.Username != "" {
			select {
			case m.deadChan <- token.Username:
			default:
				// Channel full, skip (best-effort)
			}
		}
	}
	// Don't return to queue - token is dead
}
```

- [ ] **Step 7: Also update `MarkDead` to notify deadChan**

Replace `MarkDead` (lines 368-381) with:

```go
// MarkDead marks a token as dead (lock-free)
func (m *Manager) MarkDead(accessToken string) {
	total := int(atomic.LoadInt32(&m.totalTokens))

	for idx := 0; idx < total; idx++ {
		info := m.tokenInfos[idx]
		if info.AccessToken == accessToken {
			// Atomic CAS - only increment deadCount once
			if atomic.CompareAndSwapInt32(&info.dead, 0, 1) {
				atomic.AddInt32(&m.deadCount, 1)
				// Notify Redis cleanup goroutine
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
```

- [ ] **Step 8: Delete the entire refresh-save section**

Remove lines 527-630 (all of these functions):
- `StartRefreshTokenSaver()`
- `saveRefreshedTokensLoop()`
- `StopRefreshTokenSaver()`
- `queueRefreshedToken()`
- `SaveAliveTokens()`

- [ ] **Step 9: Clean up unused imports**

Remove these imports that are no longer used after deleting the save functions:
- `"bufio"` (was used by `saveRefreshedTokensLoop` and `SaveAliveTokens`)
- `"os"` (was used by `LoadFromFile`, `saveRefreshedTokensLoop`, `SaveAliveTokens`)

Keep `"os"` only if `LoadFromFile` is kept for backward compatibility. Since we are keeping `LoadFromFile` for now (the plan only adds `LoadFromSlice`, doesn't delete `LoadFromFile`), keep `"os"` and `"bufio"`.

Actually, `LoadFromFile` still uses `"os"` and `"bufio"`, so keep them. No import changes needed.

- [ ] **Step 10: Verify compilation**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go build ./token/
```

Expected: No errors.

- [ ] **Step 11: Commit**

```bash
cd "d:/vs code/token_profile_full/token_profile_full"
git add Get_Profile/token/manager.go
git commit -m "refactor(get-profile): remove refresh-save logic, add deadChan and LoadFromSlice"
```

---

## Task 5: Modify `main.go` — wire Redis, new flags, remove file-token logic

**Files:**
- Modify: `Get_Profile/main.go`

- [ ] **Step 1: Replace the entire `main.go`**

```go
//go:build !hot
// +build !hot

package main

import (
	"flag"
	"fmt"
	"io"
	"linkedin_fetcher/api"
	"linkedin_fetcher/config"
	"linkedin_fetcher/reader"
	"linkedin_fetcher/token"
	"linkedin_fetcher/worker"
	"linkedin_fetcher/writer"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	// Generate timestamp for this run
	startTimestamp := time.Now().Format("2006-01-02_15-04-05")

	// Parse command line arguments
	emailsFile := flag.String("emails", "", "Path to emails file (default: emails.txt)")
	resultFile := flag.String("result", "", "Path to result file (will use timestamp if not specified)")
	queueSuffix := flag.String("queue", "", "Redis queue suffix (required, e.g. \"1\" -> redis-tokens-1)")
	redisAddr := flag.String("redis", "localhost:6379", "Redis address")
	numWorkers := flag.Int("workers", 350, "Number of workers")
	instanceID := flag.String("id", "", "Instance ID for logging (optional)")
	maxCPM := flag.Int("max-cpm", 0, "Max requests per minute (0 = use default 20000)")
	flag.Parse()

	// Validate required flags
	if *queueSuffix == "" {
		fmt.Fprintln(os.Stderr, "Required flag: -queue (e.g. -queue 1)")
		flag.Usage()
		os.Exit(1)
	}

	// Generate timestamped filenames
	logFileName := fmt.Sprintf("output_%s.log", startTimestamp)
	resultFileName := fmt.Sprintf("result_%s.txt", startTimestamp)

	// If result file specified, use it instead
	if *resultFile != "" {
		resultFileName = *resultFile
	}

	// Set up logging to file
	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// Log to both stdout and file
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if *instanceID != "" {
		log.SetPrefix(fmt.Sprintf("[%s] ", *instanceID))
	}
	log.Println("=== LinkedIn Profile Fetcher ===")
	log.Printf("Run timestamp: %s", startTimestamp)
	log.Printf("Log file: %s", logFileName)
	log.Printf("Result file: %s", resultFileName)
	log.Println("Starting application...")

	// Load configuration
	cfg := config.NewConfig()

	// Apply overrides
	cfg.NumWorkers = *numWorkers
	cfg.MaxCPM = 20000 // Hard limit: 20K CPM
	cfg.RedisAddr = *redisAddr
	cfg.RedisQueue = *queueSuffix

	if *emailsFile != "" {
		cfg.EmailsFile = *emailsFile
	}
	cfg.ResultsFile = resultFileName

	if *maxCPM > 0 {
		cfg.MaxCPM = *maxCPM
	}

	log.Printf("[CONFIG] Workers: %d | APITimeout: %ds | EmailBuffer: %d | MaxCPM: %d",
		cfg.NumWorkers, cfg.APITimeout, cfg.EmailBufferSize, cfg.MaxCPM)
	log.Printf("[CONFIG] Redis: %s | Queue: redis-tokens-%s", cfg.RedisAddr, cfg.RedisQueue)
	log.Printf("[FILES] Emails: %s | Result: %s", cfg.EmailsFile, cfg.ResultsFile)

	// Check if emails file exists
	if _, err := os.Stat(cfg.EmailsFile); os.IsNotExist(err) {
		log.Fatalf("[ERROR] Emails file not found: %s", cfg.EmailsFile)
	}

	// Connect to Redis
	redisLoader, err := token.NewRedisLoader(cfg.RedisAddr)
	if err != nil {
		log.Fatalf("[ERROR] %v", err)
	}
	defer redisLoader.Close()
	log.Printf("[REDIS] Connected to %s", cfg.RedisAddr)

	// Load tokens from Redis
	tokenSlice, err := redisLoader.LoadTokens(cfg.RedisQueue)
	if err != nil {
		log.Fatalf("[ERROR] %v", err)
	}

	// Initialize token manager
	tokenManager := token.NewManager()
	if err := tokenManager.LoadFromSlice(tokenSlice); err != nil {
		log.Fatalf("[ERROR] Failed to load tokens: %v", err)
	}
	total, alive, _ := tokenManager.Stats()
	log.Printf("[TOKEN] Loaded %d tokens (%d alive)", total, alive)

	// Create dead token channel and set on manager
	deadChan := make(chan string, 1000)
	tokenManager.SetDeadChan(deadChan)

	// Initialize token queue mode
	tokenManager.InitQueue()
	log.Printf("[TOKEN] Queue mode enabled, %d tokens in queue", tokenManager.QueueLen())

	// Start Redis dead token cleanup worker
	ctx, cancel := context.WithCancel(context.Background())
	redisLoader.StartCleanupWorker(ctx, cfg.RedisQueue, deadChan)
	log.Printf("[REDIS] Dead token cleanup worker started")

	// Initialize email reader
	emailReader := reader.NewEmailReader(cfg.EmailsFile, cfg.FileBufferSize)
	totalEmails := 0
	log.Printf("[READER] Skipping email count for faster startup")

	// Initialize result writer
	resultWriter, err := writer.NewResultWriter(cfg.ResultsFile)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create result writer: %v", err)
	}
	defer resultWriter.Close()
	log.Printf("[WRITER] Output file: %s", cfg.ResultsFile)

	// Initialize API client
	apiClient := api.NewClient(cfg.APITimeout)
	log.Println("[API] Client initialized")

	// Initialize worker pool
	pool := worker.NewPool(cfg.NumWorkers, apiClient, tokenManager, resultWriter, cfg.EmailBufferSize, cfg.MaxCPM)

	// Start progress reporter
	pool.StartProgressReporter(5*time.Second, totalEmails)

	// Start workers
	pool.Start()

	// Set up graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start reading emails
	startTime := time.Now()
	emailChan, errChan := emailReader.ReadEmails()

	// Done channel to signal completion
	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() {
		closeOnce.Do(func() {
			close(done)
		})
	}

	// Process emails from channel
	go func() {
		for email := range emailChan {
			pool.Submit(email)
		}
		if err := <-errChan; err != nil {
			log.Printf("[ERROR] Failed to read emails: %v", err)
		}
		pool.Close()
		closeDone()
	}()

	// Wait for completion or interrupt
	go func() {
		<-sigChan
		log.Println("\n[SHUTDOWN] Received interrupt signal, shutting down...")
		pool.Shutdown()
		closeDone()
	}()

	// Wait for processing to complete
	<-done
	time.Sleep(500 * time.Millisecond)

	// Shutdown cleanup
	cancel()         // Signal cleanup worker to stop
	close(deadChan)  // Close dead channel, cleanup worker drains remaining
	time.Sleep(1 * time.Second) // Give cleanup worker time to flush

	// Print final statistics
	elapsed := time.Since(startTime)
	processed, successful, failed, exactMatch := pool.Stats()

	fmt.Println(repeatString("=", 60))
	log.Println("=== FINAL STATISTICS ===")

	if pool.StoppedEarly() {
		log.Printf("Stop Reason:     %s", pool.StopReason())
	} else {
		log.Printf("Stop Reason:     completed (all emails processed)")
	}

	log.Printf("Total Emails:    %d", totalEmails)
	log.Printf("Processed:       %d", processed)
	log.Printf("Successful:      %d", successful)
	log.Printf("Failed:          %d", failed)
	log.Printf("ExactMatch:      %d", exactMatch)
	log.Printf("Results Written: %d", resultWriter.Count())
	log.Printf("Elapsed Time:    %s", elapsed.Round(time.Second))
	if elapsed.Seconds() > 0 {
		log.Printf("Rate:            %.1f emails/second", float64(processed)/elapsed.Seconds())
	}

	tTotal, tAlive, tDead := tokenManager.Stats()
	log.Printf("Tokens:          %d total, %d alive, %d dead", tTotal, tAlive, tDead)

	fmt.Println(repeatString("=", 60))
	log.Printf("Results saved to: %s", cfg.ResultsFile)

	if pool.StoppedEarly() {
		log.Printf("Application stopped early: %s", pool.StopReason())
	} else {
		log.Println("Application finished successfully.")
	}
}

// repeatString returns a string repeated n times
func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
```

- [ ] **Step 2: Verify compilation**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go build .
```

Expected: No errors. If `context` import missing, add `"context"` to imports.

- [ ] **Step 3: Fix import — add `context`**

The `main.go` uses `context.WithCancel` but the import block above is missing it. Add `"context"` to the import block:

```go
import (
	"context"
	"flag"
	"fmt"
	// ...rest unchanged
)
```

- [ ] **Step 4: Verify build again**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go build .
```

Expected: Clean build, no errors.

- [ ] **Step 5: Commit**

```bash
cd "d:/vs code/token_profile_full/token_profile_full"
git add Get_Profile/main.go
git commit -m "feat(get-profile): wire Redis token loading, remove file-based token logic"
```

---

## Task 6: Integration test — verify with local Redis

**Files:** None (manual testing)

- [ ] **Step 1: Verify Redis is running**

```bash
redis-cli ping
```

Expected: `PONG`

- [ ] **Step 2: Seed test data into Redis**

```bash
redis-cli HSET redis-tokens-1 "testuser@domain.com" "testuser@domain.com|fake_refresh_token_here|fake-tenant-id-here"
redis-cli HLEN redis-tokens-1
```

Expected: Shows count of tokens in queue.

- [ ] **Step 3: Build and run with minimal test**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go build -o get_profile.exe .
echo "test@example.com" > test_emails.txt
./get_profile.exe -queue 1 -emails test_emails.txt -workers 5 -max-cpm 100
```

Expected: Application starts, connects to Redis, loads tokens, processes emails. Tokens will fail (fake) but the pipeline should run without crashes.

- [ ] **Step 4: Verify dead tokens are cleaned from Redis**

After the test run (tokens should die since they're fake):

```bash
redis-cli HLEN redis-tokens-1
```

Expected: Count should be lower than before (dead tokens were HDEL'd).

- [ ] **Step 5: Clean up test data**

```bash
redis-cli DEL redis-tokens-1
rm -f test_emails.txt get_profile.exe
```

- [ ] **Step 6: Full build verification**

```bash
cd "d:/vs code/token_profile_full/token_profile_full/Get_Profile"
go vet ./...
go build .
```

Expected: No warnings, clean build.

- [ ] **Step 7: Commit all final changes**

```bash
cd "d:/vs code/token_profile_full/token_profile_full"
git add Get_Profile/
git commit -m "feat(get-profile): complete Redis token integration with dead token cleanup"
```

---

## Summary of changes

| File | Lines changed | What |
|------|-------------|------|
| `go.mod` + `go.sum` | New | Module init + redis dependency |
| `token/redis.go` | ~160 lines new | RedisLoader: connect, HGETALL, HDEL cleanup goroutine |
| `token/manager.go` | ~30 lines modified, ~100 lines removed | Remove refresh-save, add deadChan + LoadFromSlice |
| `config/config.go` | ~10 lines modified | RedisAddr/RedisQueue replace TokensFile |
| `main.go` | ~80 lines modified | Redis wiring, new flags, remove file-token logic |

**Total:** ~160 lines new, ~120 lines modified, ~100 lines removed.
