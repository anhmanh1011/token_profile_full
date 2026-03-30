# Centralized Token Management System — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Centralize token generation (VN-TAKK) on 1 VPS with a Token API, while 3 worker VPS run only CHECK_CONNECTION and fetch tokens via HTTP.

**Architecture:** VPS trung tam chay VN-TAKK (Python, cron/6h) + Redis (local) + Token API (Go, :8080). 3 Worker VPS chay CHECK_CONNECTION (Go) lay token qua HTTP API. Script `split_deploy.sh` tu dong chia emails va rsync den workers.

**Tech Stack:** Go 1.25 (Token API + CHECK_CONNECTION), Python 3 (VN-TAKK), Redis, Bash

---

## File Structure

### New Project: Token API (Go) — `token-api/`

| File | Responsibility |
|------|---------------|
| `token-api/main.go` | HTTP server entrypoint, routes |
| `token-api/handler.go` | HTTP handlers: GET /tokens, POST exhausted, GET /stats |
| `token-api/redis.go` | Redis client wrapper, token CRUD operations |
| `token-api/models.go` | Shared data structures (TokenData, API responses) |
| `token-api/go.mod` | Module definition |

### Modified: VN-TAKK (Python) — `VN-TAKK/`

| File | Change |
|------|--------|
| `VN-TAKK/main.py` | Replace file output with Redis push, add Redis input for account selection |
| `VN-TAKK/redis_client.py` | New: Redis helper functions |
| `VN-TAKK/requirements.txt` | Add `redis` package |

### Modified: CHECK_CONNECTION (Go) — `CHECK_CONNECTION/`

| File | Change |
|------|--------|
| `CHECK_CONNECTION/token/api_client.go` | New: HTTP client for Token API |
| `CHECK_CONNECTION/token/buffer.go` | New: 2-layer token buffer (active + prefetch) |
| `CHECK_CONNECTION/token/manager.go` | Add API-based token source mode |
| `CHECK_CONNECTION/config/config.go` | Add API URL, tenant ID, batch size fields |
| `CHECK_CONNECTION/main.go` | Add -api, -tenant, -batch flags; init API mode |
| `CHECK_CONNECTION/worker/pool.go` | Use buffer instead of token queue in API mode |

### New: Deploy Script

| File | Responsibility |
|------|---------------|
| `scripts/split_deploy.sh` | Split emails.txt + rsync to 3 worker VPS |

---

## Task 1: Token API — Redis Client

**Files:**
- Create: `token-api/go.mod`
- Create: `token-api/models.go`
- Create: `token-api/redis.go`

- [ ] **Step 1: Initialize Go module**

```bash
mkdir -p token-api && cd token-api
go mod init token-api
go get github.com/redis/go-redis/v9
```

- [ ] **Step 2: Create models.go**

```go
package main

import "time"

// TokenData represents a token stored in Redis
type TokenData struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	RefreshToken string `json:"refresh_token"`
	TenantID     string `json:"tenant_id"`
	CreatedAt    int64  `json:"created_at"`
	ExhaustedAt  int64  `json:"exhausted_at,omitempty"`
}

// TokenResponse is the API response for GET /tokens/{tenant_id}
type TokenResponse struct {
	Tokens []TokenData `json:"tokens"`
	Count  int         `json:"count"`
}

// ExhaustedRequest is the request body for POST /tokens/{tenant_id}/exhausted
type ExhaustedRequest struct {
	Email string `json:"email"`
}

// StatsResponse is the API response for GET /stats/{tenant_id}
type StatsResponse struct {
	TenantID string `json:"tenant_id,omitempty"`
	Fresh    int64  `json:"fresh"`
	InUse    int64  `json:"in_use"`
	Used     int64  `json:"used"`
}

// OKResponse is a simple success response
type OKResponse struct {
	OK bool `json:"ok"`
}

// TimeNow is a helper for testing
var TimeNow = time.Now
```

- [ ] **Step 3: Create redis.go**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore handles all Redis operations for token management
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore creates a new Redis store
func NewRedisStore(addr string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DB:           0,
		PoolSize:     20,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connect failed: %w", err)
	}

	return &RedisStore{client: client}, nil
}

func freshKey(tenantID string) string   { return fmt.Sprintf("tokens:%s:fresh", tenantID) }
func inUseKey(tenantID string) string   { return fmt.Sprintf("tokens:%s:in_use", tenantID) }
func usedKey(tenantID string) string    { return fmt.Sprintf("tokens:%s:used", tenantID) }

// PopFreshTokens pops up to `count` tokens from fresh queue and moves them to in_use
func (r *RedisStore) PopFreshTokens(ctx context.Context, tenantID string, count int) ([]TokenData, error) {
	tokens := make([]TokenData, 0, count)

	for i := 0; i < count; i++ {
		// LPOP from fresh
		val, err := r.client.LPop(ctx, freshKey(tenantID)).Result()
		if err == redis.Nil {
			break // No more tokens
		}
		if err != nil {
			return tokens, fmt.Errorf("lpop failed: %w", err)
		}

		var token TokenData
		if err := json.Unmarshal([]byte(val), &token); err != nil {
			log.Printf("[WARN] invalid token JSON in fresh queue: %v", err)
			continue
		}

		// Push to in_use
		if err := r.client.RPush(ctx, inUseKey(tenantID), val).Err(); err != nil {
			log.Printf("[WARN] failed to push to in_use: %v", err)
		}

		tokens = append(tokens, token)
	}

	return tokens, nil
}

// MarkExhausted moves a token from in_use to used queue with exhausted_at timestamp
func (r *RedisStore) MarkExhausted(ctx context.Context, tenantID string, email string) error {
	// Scan in_use list to find the token by email
	vals, err := r.client.LRange(ctx, inUseKey(tenantID), 0, -1).Result()
	if err != nil {
		return fmt.Errorf("lrange failed: %w", err)
	}

	for _, val := range vals {
		var token TokenData
		if err := json.Unmarshal([]byte(val), &token); err != nil {
			continue
		}

		if token.Email == email {
			// Remove from in_use
			r.client.LRem(ctx, inUseKey(tenantID), 1, val)

			// Set exhausted_at and push to used
			token.ExhaustedAt = TimeNow().Unix()
			updatedJSON, err := json.Marshal(token)
			if err != nil {
				return fmt.Errorf("marshal failed: %w", err)
			}
			r.client.RPush(ctx, usedKey(tenantID), string(updatedJSON))
			return nil
		}
	}

	return fmt.Errorf("token not found for email: %s", email)
}

// GetStats returns queue lengths for a tenant
func (r *RedisStore) GetStats(ctx context.Context, tenantID string) (StatsResponse, error) {
	fresh, err := r.client.LLen(ctx, freshKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, err
	}
	inUse, err := r.client.LLen(ctx, inUseKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, err
	}
	used, err := r.client.LLen(ctx, usedKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, err
	}

	return StatsResponse{
		TenantID: tenantID,
		Fresh:    fresh,
		InUse:    inUse,
		Used:     used,
	}, nil
}

// GetAllTenants scans Redis for all tenant keys and returns unique tenant IDs
func (r *RedisStore) GetAllTenants(ctx context.Context) ([]string, error) {
	tenants := make(map[string]bool)

	iter := r.client.Scan(ctx, 0, "tokens:*:fresh", 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		// Extract tenant ID from "tokens:{tenant_id}:fresh"
		// Find between first and last ":"
		start := len("tokens:")
		end := len(key) - len(":fresh")
		if start < end {
			tenants[key[start:end]] = true
		}
	}

	result := make([]string, 0, len(tenants))
	for t := range tenants {
		result = append(result, t)
	}
	return result, nil
}

// Close closes the Redis connection
func (r *RedisStore) Close() error {
	return r.client.Close()
}
```

- [ ] **Step 4: Run `go mod tidy`**

```bash
cd token-api && go mod tidy
```

Expected: go.mod and go.sum updated with redis dependency.

- [ ] **Step 5: Commit**

```bash
git add token-api/go.mod token-api/go.sum token-api/models.go token-api/redis.go
git commit -m "feat(token-api): add Redis store and data models"
```

---

## Task 2: Token API — HTTP Handlers

**Files:**
- Create: `token-api/handler.go`

- [ ] **Step 1: Create handler.go**

```go
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Handler holds dependencies for HTTP handlers
type Handler struct {
	store *RedisStore
}

// NewHandler creates a new handler
func NewHandler(store *RedisStore) *Handler {
	return &Handler{store: store}
}

// ServeHTTP routes requests to the appropriate handler
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")

	// GET /stats
	if path == "/stats" && r.Method == http.MethodGet {
		h.handleStatsAll(w, r)
		return
	}

	// GET /stats/{tenant_id}
	if strings.HasPrefix(path, "/stats/") && r.Method == http.MethodGet {
		tenantID := strings.TrimPrefix(path, "/stats/")
		h.handleStats(w, r, tenantID)
		return
	}

	// POST /tokens/{tenant_id}/exhausted
	if strings.HasSuffix(path, "/exhausted") && r.Method == http.MethodPost {
		parts := strings.Split(path, "/")
		// /tokens/{tenant_id}/exhausted → ["", "tokens", "{tenant_id}", "exhausted"]
		if len(parts) == 4 && parts[1] == "tokens" {
			h.handleExhausted(w, r, parts[2])
			return
		}
	}

	// GET /tokens/{tenant_id}
	if strings.HasPrefix(path, "/tokens/") && r.Method == http.MethodGet {
		tenantID := strings.TrimPrefix(path, "/tokens/")
		h.handleGetTokens(w, r, tenantID)
		return
	}

	http.NotFound(w, r)
}

// handleGetTokens handles GET /tokens/{tenant_id}?count=100
func (h *Handler) handleGetTokens(w http.ResponseWriter, r *http.Request, tenantID string) {
	count := 100 // default
	if c := r.URL.Query().Get("count"); c != "" {
		if parsed, err := strconv.Atoi(c); err == nil && parsed > 0 {
			count = parsed
		}
	}

	tokens, err := h.store.PopFreshTokens(r.Context(), tenantID, count)
	if err != nil {
		log.Printf("[ERROR] PopFreshTokens: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, TokenResponse{
		Tokens: tokens,
		Count:  len(tokens),
	})
}

// handleExhausted handles POST /tokens/{tenant_id}/exhausted
func (h *Handler) handleExhausted(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req ExhaustedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email required"})
		return
	}

	if err := h.store.MarkExhausted(r.Context(), tenantID, req.Email); err != nil {
		log.Printf("[WARN] MarkExhausted: %v", err)
		// Still return OK — token might have been cleaned up already
	}

	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

// handleStats handles GET /stats/{tenant_id}
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request, tenantID string) {
	stats, err := h.store.GetStats(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleStatsAll handles GET /stats
func (h *Handler) handleStatsAll(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.store.GetAllTenants(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	allStats := make([]StatsResponse, 0, len(tenants))
	for _, t := range tenants {
		stats, err := h.store.GetStats(r.Context(), t)
		if err != nil {
			continue
		}
		allStats = append(allStats, stats)
	}

	writeJSON(w, http.StatusOK, allStats)
}

// writeJSON writes a JSON response
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
```

- [ ] **Step 2: Commit**

```bash
git add token-api/handler.go
git commit -m "feat(token-api): add HTTP handlers for token distribution"
```

---

## Task 3: Token API — Main Entrypoint

**Files:**
- Create: `token-api/main.go`

- [ ] **Step 1: Create main.go**

```go
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	redisAddr := flag.String("redis", "127.0.0.1:6379", "Redis address")
	listenAddr := flag.String("listen", ":8080", "HTTP listen address")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("=== Token API Server ===")

	// Connect to Redis
	store, err := NewRedisStore(*redisAddr)
	if err != nil {
		log.Fatalf("[FATAL] Redis connection failed: %v", err)
	}
	defer store.Close()
	log.Printf("[REDIS] Connected to %s", *redisAddr)

	// Create handler
	handler := NewHandler(store)

	// Start HTTP server
	server := &http.Server{
		Addr:    *listenAddr,
		Handler: handler,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("[SHUTDOWN] Shutting down...")
		server.Close()
	}()

	log.Printf("[HTTP] Listening on %s", *listenAddr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("[FATAL] HTTP server error: %v", err)
	}
	log.Println("[SHUTDOWN] Server stopped")
}
```

- [ ] **Step 2: Build and verify**

```bash
cd token-api && go build -o token-api .
```

Expected: Binary compiles without errors.

- [ ] **Step 3: Commit**

```bash
git add token-api/main.go
git commit -m "feat(token-api): add main entrypoint with graceful shutdown"
```

---

## Task 4: VN-TAKK — Redis Integration

**Files:**
- Create: `VN-TAKK/redis_client.py`
- Modify: `VN-TAKK/main.py`
- Modify: `VN-TAKK/requirements.txt`

- [ ] **Step 1: Create redis_client.py**

```python
import json
import time
import redis
import logging

logger = logging.getLogger(__name__)

class TokenRedis:
    def __init__(self, host='127.0.0.1', port=6379, db=0):
        self.r = redis.Redis(host=host, port=port, db=db, decode_responses=True)
        self.r.ping()
        logger.info(f"Redis connected: {host}:{port}")

    def push_token(self, email, password, refresh_token, tenant_id):
        """Push a fresh token to Redis"""
        token_data = json.dumps({
            "email": email,
            "password": password,
            "refresh_token": refresh_token,
            "tenant_id": tenant_id,
            "created_at": int(time.time()),
            "exhausted_at": 0
        })
        key = f"tokens:{tenant_id}:fresh"
        self.r.rpush(key, token_data)

    def get_accounts_to_login(self, tenant_id, all_accounts):
        """Determine which accounts need to be logged in.

        Returns list of 'email:password' strings that need login:
        - Accounts in 'used' queue with exhausted_at + 24h < now
        - Accounts not present in any queue (fresh/in_use/used)
        """
        accounts_in_redis = set()
        cooldown_ready = []

        # Scan used queue for cooldown-ready accounts
        used_key = f"tokens:{tenant_id}:used"
        used_tokens = self.r.lrange(used_key, 0, -1)
        now = int(time.time())

        for val in used_tokens:
            token = json.loads(val)
            accounts_in_redis.add(token["email"])
            if token.get("exhausted_at", 0) > 0 and (now - token["exhausted_at"]) >= 86400:
                # Remove from used queue
                self.r.lrem(used_key, 1, val)
                cooldown_ready.append(f"{token['email']}:{token['password']}")

        # Collect emails in fresh and in_use queues
        for suffix in ["fresh", "in_use"]:
            key = f"tokens:{tenant_id}:{suffix}"
            items = self.r.lrange(key, 0, -1)
            for val in items:
                token = json.loads(val)
                accounts_in_redis.add(token["email"])

        # Find accounts not in Redis at all
        new_accounts = []
        for account in all_accounts:
            email = account.split(":")[0]
            if email not in accounts_in_redis:
                new_accounts.append(account)

        result = cooldown_ready + new_accounts
        logger.info(
            f"[{tenant_id}] Accounts to login: {len(result)} "
            f"(cooldown_ready={len(cooldown_ready)}, new={len(new_accounts)}, "
            f"in_redis={len(accounts_in_redis)})"
        )
        return result

    def stats(self, tenant_id):
        """Return queue lengths"""
        return {
            "fresh": self.r.llen(f"tokens:{tenant_id}:fresh"),
            "in_use": self.r.llen(f"tokens:{tenant_id}:in_use"),
            "used": self.r.llen(f"tokens:{tenant_id}:used"),
        }
```

- [ ] **Step 2: Modify main.py — replace file output with Redis push**

In `main.py`, the `GetAccessToken` method currently writes to file on line 449:

```python
# OLD (line 449):
open(f'{FOLDERCRETE}\\get_token_success.txt','a',encoding='utf-8').write(f'{self.mail}:{self.pwd}|{rf}|{self.tenant_id}\n')
```

Replace the class constructor and `GetAccessToken` method. The key changes:

1. Add `redis_client` parameter to `TeamOutLook.__init__`
2. In `GetAccessToken`, replace file write with `redis_client.push_token()`
3. Replace main block to read accounts from Redis

Modified `__init__` — add redis_client:
```python
def __init__(self, account: str, proxy: str = None, redis_client=None):
    self.mail, self.pwd = account.split(":")
    self.newpwd = self.pwd + "1"
    self.session = requests.Session(impersonate="firefox135", timeout=60)
    self.data_proxy = None
    self.tenant_id = ""
    self.redis_client = redis_client
    # ... rest of proxy setup unchanged
```

Modified `GetAccessToken` — replace file write:
```python
def GetAccessToken(self, code):
    # ... existing code to get rf and access_token unchanged ...

    if self.redis_client:
        self.redis_client.push_token(self.mail, self.pwd, rf, self.tenant_id)
    else:
        open(f'{FOLDERCRETE}/get_token_success.txt', 'a', encoding='utf-8').write(
            f'{self.mail}:{self.pwd}|{rf}|{self.tenant_id}\n')

    logger.info(f'{self.mail}     ------      Get token success - Tenant: {self.tenant_id}')
    return True
```

Modified main block — add Redis mode:
```python
import argparse

def main():
    global FOLDERCRETE

    parser = argparse.ArgumentParser()
    parser.add_argument('--redis', default=None, help='Redis host:port (e.g. 127.0.0.1:6379)')
    parser.add_argument('--tenant', default=None, help='Tenant ID to process')
    parser.add_argument('--accounts', default='account.txt', help='Accounts file')
    parser.add_argument('--threads', type=int, default=100, help='Number of threads')
    args = parser.parse_args()

    print_red_centered_art()
    start_time = time.time()
    FOLDERCRETE = create_timestamped_folder()

    redis_client = None
    if args.redis:
        from redis_client import TokenRedis
        host, port = args.redis.split(':')
        redis_client = TokenRedis(host=host, port=int(port))

    # Load all accounts from file
    listmail = open(args.accounts, 'r', encoding='utf-8').read().splitlines()

    # If Redis mode + tenant specified, filter to accounts that need login
    if redis_client and args.tenant:
        listmail = redis_client.get_accounts_to_login(args.tenant, listmail)
        logger.info(f"Filtered to {len(listmail)} accounts needing login")

    if not listmail:
        logger.info("No accounts to process")
        return

    task_queue = queue.Queue()
    for mail in listmail:
        task_queue.put(mail)

    stop_event = threading.Event()
    hits = 0
    fails = 0
    lock = threading.Lock()

    def worker(thread_id):
        nonlocal hits, fails
        while not stop_event.is_set():
            try:
                mail = task_queue.get(timeout=1)
            except queue.Empty:
                return

            try:
                obj = TeamOutLook(mail, redis_client=redis_client)
                if obj.DoTask():
                    with lock:
                        hits += 1
                else:
                    open(f'{FOLDERCRETE}/failed_get.txt', 'a', encoding='utf-8').write(f'{mail}\n')
                    with lock:
                        fails += 1
            except Exception as e:
                logger.error(f"[ERROR] {mail}: {e}")
                with lock:
                    fails += 1
            finally:
                task_queue.task_done()

    threads = []
    for i in range(args.threads):
        t = threading.Thread(target=worker, args=(i,), daemon=True)
        threads.append(t)
        t.start()

    task_queue.join()

    end_time = time.time()
    logger.info(f"Completed {len(listmail)} accounts in {(end_time - start_time)/60:.2f} min | Hits: {hits} | Fails: {fails}")

    if redis_client and args.tenant:
        stats = redis_client.stats(args.tenant)
        logger.info(f"Redis stats: {stats}")

if __name__ == "__main__":
    main()
```

- [ ] **Step 3: Update requirements.txt**

Add `redis` to `VN-TAKK/requirements.txt`:

```
colorama == 0.4.6
curl_cffi == 0.10.0
requests
redis
```

Remove `python-telegram-bot` and `aiohttp` (bot no longer needed).

- [ ] **Step 4: Test locally (requires Redis running)**

```bash
cd VN-TAKK
pip install redis
python main.py --redis 127.0.0.1:6379 --tenant test-tenant --accounts account.txt --threads 5
```

Expected: Tokens pushed to Redis instead of file.

- [ ] **Step 5: Commit**

```bash
git add VN-TAKK/redis_client.py VN-TAKK/main.py VN-TAKK/requirements.txt
git commit -m "feat(vn-takk): add Redis integration for centralized token storage"
```

---

## Task 5: CHECK_CONNECTION — API Token Client

**Files:**
- Create: `CHECK_CONNECTION/token/api_client.go`

- [ ] **Step 1: Create api_client.go**

```go
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
```

- [ ] **Step 2: Commit**

```bash
git add CHECK_CONNECTION/token/api_client.go
git commit -m "feat(check-connection): add HTTP client for Token API"
```

---

## Task 6: CHECK_CONNECTION — 2-Layer Token Buffer

**Files:**
- Create: `CHECK_CONNECTION/token/buffer.go`

- [ ] **Step 1: Create buffer.go**

```go
package token

import (
	"log"
	"sync"
	"time"
)

// TokenBuffer implements a 2-layer buffer for continuous token supply
type TokenBuffer struct {
	apiClient *APIClient
	batchSize int

	// Active pool — workers take from here
	active []*TokenInfo
	activeMu sync.Mutex

	// Prefetch pool — swapped in when active runs low
	prefetch []*TokenInfo
	prefetchMu sync.Mutex
	prefetchReady chan struct{} // signals prefetch is filled

	// Control
	threshold int // swap when active pool falls below this
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewTokenBuffer creates a new 2-layer token buffer
func NewTokenBuffer(apiClient *APIClient, batchSize int) *TokenBuffer {
	threshold := batchSize / 5 // 20% of batch size
	if threshold < 5 {
		threshold = 5
	}

	return &TokenBuffer{
		apiClient:     apiClient,
		batchSize:     batchSize,
		active:        make([]*TokenInfo, 0, batchSize),
		prefetch:      make([]*TokenInfo, 0, batchSize),
		prefetchReady: make(chan struct{}, 1),
		threshold:     threshold,
		stopCh:        make(chan struct{}),
	}
}

// Start initializes the buffer with initial tokens and starts prefetch goroutine
func (b *TokenBuffer) Start() error {
	// Fetch initial active pool
	tokens, err := b.fetchBatch()
	if err != nil {
		return err
	}
	b.active = tokens
	log.Printf("[BUFFER] Loaded %d tokens into active pool", len(tokens))

	// Start prefetch goroutine
	b.wg.Add(1)
	go b.prefetchLoop()

	// Trigger initial prefetch
	b.triggerPrefetch()

	return nil
}

// AcquireToken gets a token from the active pool
// Returns nil if no tokens available
func (b *TokenBuffer) AcquireToken() *TokenInfo {
	b.activeMu.Lock()

	if len(b.active) == 0 {
		b.activeMu.Unlock()
		// Try swap immediately
		b.trySwap()
		b.activeMu.Lock()
		if len(b.active) == 0 {
			b.activeMu.Unlock()
			return nil
		}
	}

	// Pop from active pool
	token := b.active[len(b.active)-1]
	b.active = b.active[:len(b.active)-1]
	remaining := len(b.active)
	b.activeMu.Unlock()

	// Check if we need to swap
	if remaining < b.threshold {
		b.trySwap()
	}

	return token
}

// ReportExhausted reports a token as exhausted to the API (non-blocking)
func (b *TokenBuffer) ReportExhausted(email string) {
	go func() {
		if err := b.apiClient.ReportExhausted(email); err != nil {
			log.Printf("[BUFFER] Report exhausted failed for %s: %v", email, err)
		}
	}()
}

// trySwap swaps prefetch buffer into active if prefetch is ready
func (b *TokenBuffer) trySwap() {
	b.prefetchMu.Lock()
	if len(b.prefetch) == 0 {
		b.prefetchMu.Unlock()
		return
	}
	newActive := b.prefetch
	b.prefetch = make([]*TokenInfo, 0, b.batchSize)
	b.prefetchMu.Unlock()

	b.activeMu.Lock()
	// Prepend remaining active tokens to new active
	b.active = append(newActive, b.active...)
	b.activeMu.Unlock()

	log.Printf("[BUFFER] Swapped in %d prefetched tokens", len(newActive))

	// Trigger new prefetch
	b.triggerPrefetch()
}

// triggerPrefetch signals the prefetch goroutine to fetch more tokens
func (b *TokenBuffer) triggerPrefetch() {
	select {
	case b.prefetchReady <- struct{}{}:
	default:
	}
}

// prefetchLoop continuously prefetches tokens in the background
func (b *TokenBuffer) prefetchLoop() {
	defer b.wg.Done()

	for {
		select {
		case <-b.stopCh:
			return
		case <-b.prefetchReady:
			b.prefetchMu.Lock()
			needFetch := len(b.prefetch) == 0
			b.prefetchMu.Unlock()

			if !needFetch {
				continue
			}

			tokens, err := b.fetchBatch()
			if err != nil {
				log.Printf("[BUFFER] Prefetch failed: %v, retrying in 5s", err)
				time.Sleep(5 * time.Second)
				b.triggerPrefetch() // retry
				continue
			}

			if len(tokens) == 0 {
				log.Printf("[BUFFER] No fresh tokens available, retrying in 10s")
				time.Sleep(10 * time.Second)
				b.triggerPrefetch() // retry
				continue
			}

			b.prefetchMu.Lock()
			b.prefetch = tokens
			b.prefetchMu.Unlock()
			log.Printf("[BUFFER] Prefetched %d tokens", len(tokens))
		}
	}
}

// fetchBatch fetches a batch of tokens from the API and converts to TokenInfo
func (b *TokenBuffer) fetchBatch() ([]*TokenInfo, error) {
	apiTokens, err := b.apiClient.FetchTokens(b.batchSize)
	if err != nil {
		return nil, err
	}

	tokens := make([]*TokenInfo, 0, len(apiTokens))
	for _, t := range apiTokens {
		tokens = append(tokens, &TokenInfo{
			Username:     t.Email,
			Password:     t.Password,
			RefreshToken: t.RefreshToken,
			TenantID:     t.TenantID,
		})
	}

	return tokens, nil
}

// Stop stops the prefetch goroutine
func (b *TokenBuffer) Stop() {
	close(b.stopCh)
	b.wg.Wait()
}

// Stats returns buffer statistics
func (b *TokenBuffer) Stats() (active, prefetch int) {
	b.activeMu.Lock()
	active = len(b.active)
	b.activeMu.Unlock()

	b.prefetchMu.Lock()
	prefetch = len(b.prefetch)
	b.prefetchMu.Unlock()

	return
}
```

- [ ] **Step 2: Commit**

```bash
git add CHECK_CONNECTION/token/buffer.go
git commit -m "feat(check-connection): add 2-layer token buffer with prefetch"
```

---

## Task 7: CHECK_CONNECTION — Integrate API Mode

**Files:**
- Modify: `CHECK_CONNECTION/config/config.go`
- Modify: `CHECK_CONNECTION/main.go`
- Modify: `CHECK_CONNECTION/worker/pool.go`

- [ ] **Step 1: Update config/config.go — add API fields**

Add to the `Config` struct after the existing fields:

```go
type Config struct {
	// ... existing fields ...

	// Token API settings (centralized mode)
	TokenAPIURL string // Central Token API URL (e.g. http://central:8080)
	TenantID    string // Tenant ID for this worker
	BatchSize   int    // Tokens per API fetch (default 100)
}
```

Add defaults in `NewConfig()`:

```go
func NewConfig() *Config {
	numWorkers := 1000
	maxWorkers := 3000

	return &Config{
		// ... existing fields ...

		// Token API defaults
		TokenAPIURL: "",
		TenantID:    "",
		BatchSize:   100,
	}
}
```

- [ ] **Step 2: Update main.go — add API mode flags and initialization**

Add flags after existing flags (line ~34):

```go
apiURL := flag.String("api", "", "Central Token API URL (enables API mode)")
tenantID := flag.String("tenant", "", "Tenant ID for this worker")
batchSize := flag.Int("batch", 100, "Tokens per API fetch")
```

Replace the token loading block (lines ~102-117) with:

```go
// Initialize token manager
tokenManager := token.NewManager()

var tokenBuffer *token.TokenBuffer

if *apiURL != "" && *tenantID != "" {
	// API mode — fetch tokens from central Token API
	log.Printf("[TOKEN] API mode: %s (tenant: %s, batch: %d)", *apiURL, *tenantID, *batchSize)
	apiClient := token.NewAPIClient(*apiURL, *tenantID)
	tokenBuffer = token.NewTokenBuffer(apiClient, *batchSize)
	if err := tokenBuffer.Start(); err != nil {
		log.Fatalf("[ERROR] Failed to start token buffer: %v", err)
	}
	defer tokenBuffer.Stop()
	log.Printf("[BUFFER] Token buffer started")
} else {
	// File mode — load tokens from local file (original behavior)
	if err := tokenManager.LoadFromFile(cfg.TokensFile); err != nil {
		log.Fatalf("[ERROR] Failed to load tokens: %v", err)
	}
	total, alive, _ := tokenManager.Stats()
	log.Printf("[TOKEN] Loaded %d tokens (%d alive)", total, alive)
	tokenManager.InitQueue()
	log.Printf("[TOKEN] Queue mode enabled, %d tokens in queue", tokenManager.QueueLen())
	tokenManager.StartRefreshTokenSaver(*refreshedTokensFile)
	log.Printf("[TOKEN] Refreshed tokens will be saved to: %s", *refreshedTokensFile)
}
```

Update pool initialization to pass tokenBuffer:

```go
// Initialize worker pool
pool := worker.NewPool(cfg.NumWorkers, apiClient, tokenManager, tokenBuffer, resultWriter, cfg.EmailBufferSize, cfg.MaxCPM)
```

- [ ] **Step 3: Update worker/pool.go — support token buffer**

Add `tokenBuffer` field to Pool struct:

```go
type Pool struct {
	// ... existing fields ...
	tokenBuffer *token.TokenBuffer // nil if using file-based tokens
}
```

Update `NewPool` signature:

```go
func NewPool(numWorkers int, apiClient *api.Client, tokenManager *token.Manager, tokenBuffer *token.TokenBuffer, resultWriter *writer.ResultWriter, bufferSize int, maxCPM int) *Pool {
	// ... existing code ...
	return &Pool{
		// ... existing fields ...
		tokenBuffer: tokenBuffer,
	}
}
```

Add a new worker mode in `worker()` method:

```go
func (p *Pool) worker() {
	defer p.wg.Done()

	if p.tokenBuffer != nil {
		p.workerWithAPIBuffer()
	} else if p.tokenManager.IsQueueMode() {
		p.workerWithTokenQueue()
	} else {
		for email := range p.jobsChan {
			p.processEmail(email)
		}
	}
}
```

Add `workerWithAPIBuffer` method:

```go
// workerWithAPIBuffer — worker using centralized Token API buffer
func (p *Pool) workerWithAPIBuffer() {
	for {
		// 1. Acquire token from buffer
		tkn := p.tokenBuffer.AcquireToken()
		if tkn == nil {
			log.Printf("[WORKER] No tokens available from buffer, waiting 5s...")
			time.Sleep(5 * time.Second)
			select {
			case <-p.ctx.Done():
				return
			default:
				continue
			}
		}

		// 2. Refresh token
		accessToken, err := p.tokenManager.GetAccessToken(tkn)
		if err != nil {
			p.tokenBuffer.ReportExhausted(tkn.Username)
			log.Printf("[TOKEN] Token refresh failed for %s, getting new token", tkn.Username)
			continue
		}

		// 3. Process emails with this token
		tokenDead := false
		for email := range p.jobsChan {
			if tokenDead {
				p.safeRequeue(email)
				break
			}

			atomic.AddUint64(&p.processed, 1)

			// Check if access token needs refresh
			if tkn.ExpiresAt.Before(time.Now().Add(30 * time.Second)) {
				newAccessToken, err := p.tokenManager.GetAccessToken(tkn)
				if err != nil {
					tokenDead = true
					p.tokenBuffer.ReportExhausted(tkn.Username)
					p.safeRequeue(email)
					break
				}
				accessToken = newAccessToken
			}

			// Wait for rate limit
			if err := p.waitForRateLimit(); err != nil {
				p.safeRequeue(email)
				return
			}

			// Make API call
			result, statusCode, err := p.apiClient.FetchProfile(email, accessToken)

			// Handle token death (401/424)
			if statusCode == 401 || statusCode == 424 {
				tokenDead = true
				p.tokenBuffer.ReportExhausted(tkn.Username)
				log.Printf("[TOKEN] Token dead (status %d) for %s", statusCode, tkn.Username)
				p.safeRequeue(email)
				break
			}

			// Handle 403 - rate limit
			if statusCode == 403 {
				p.trackFailReason("status_403")
				atomic.AddUint64(&p.failed, 1)
				time.Sleep(500 * time.Millisecond)
				continue
			}

			// Handle 5xx
			if statusCode >= 500 {
				p.trackFailReason(fmt.Sprintf("status_%d", statusCode))
				atomic.AddUint64(&p.failed, 1)

				if statusCode == 500 {
					newCount := atomic.AddInt32(&tkn.failCount, 1)
					if newCount >= 5 {
						tokenDead = true
						p.tokenBuffer.ReportExhausted(tkn.Username)
						log.Printf("[TOKEN] Token exhausted (5x 500) for %s", tkn.Username)
						p.safeRequeue(email)
						break
					}
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}

			// Success
			if err == nil {
				atomic.StoreInt32(&tkn.failCount, 0)
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

		// Get new token for next loop
		select {
		case <-p.ctx.Done():
			return
		default:
			if tokenDead {
				continue
			}
			return // jobsChan closed
		}
	}
}
```

- [ ] **Step 4: Build and verify**

```bash
cd CHECK_CONNECTION && go build -o check_connection .
```

Expected: Compiles without errors. Running with `-api http://central:8080 -tenant tenant-abc` enables API mode.

- [ ] **Step 5: Commit**

```bash
git add CHECK_CONNECTION/config/config.go CHECK_CONNECTION/main.go CHECK_CONNECTION/worker/pool.go
git commit -m "feat(check-connection): integrate Token API mode with 2-layer buffer"
```

---

## Task 8: Deploy Script — split_deploy.sh

**Files:**
- Create: `scripts/split_deploy.sh`

- [ ] **Step 1: Create split_deploy.sh**

```bash
#!/usr/bin/env bash
set -euo pipefail

# Configuration — edit these for your environment
WORKER1_HOST="worker1-ip"
WORKER1_USER="root"
WORKER1_DIR="/opt/check-connection"

WORKER2_HOST="worker2-ip"
WORKER2_USER="root"
WORKER2_DIR="/opt/check-connection"

WORKER3_HOST="worker3-ip"
WORKER3_USER="root"
WORKER3_DIR="/opt/check-connection"

# Usage check
if [ $# -lt 1 ]; then
    echo "Usage: $0 <emails.txt>"
    echo "Splits the file into 3 equal parts and deploys to worker VPS"
    exit 1
fi

INPUT_FILE="$1"
if [ ! -f "$INPUT_FILE" ]; then
    echo "Error: File not found: $INPUT_FILE"
    exit 1
fi

TOTAL_LINES=$(wc -l < "$INPUT_FILE")
LINES_PER_PART=$(( (TOTAL_LINES + 2) / 3 ))

echo "=== Split & Deploy ==="
echo "Input: $INPUT_FILE ($TOTAL_LINES lines)"
echo "Lines per worker: ~$LINES_PER_PART"

# Create temp dir for split files
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# Split file
split -l "$LINES_PER_PART" "$INPUT_FILE" "$TMPDIR/part_"

# Get split file names
PARTS=("$TMPDIR"/part_*)
echo "Split into ${#PARTS[@]} parts"

# Deploy to workers
echo ""
echo "Deploying to Worker 1 ($WORKER1_HOST)..."
rsync -az --progress "${PARTS[0]}" "${WORKER1_USER}@${WORKER1_HOST}:${WORKER1_DIR}/emails.txt"

echo "Deploying to Worker 2 ($WORKER2_HOST)..."
rsync -az --progress "${PARTS[1]}" "${WORKER2_USER}@${WORKER2_HOST}:${WORKER2_DIR}/emails.txt"

if [ ${#PARTS[@]} -ge 3 ]; then
    echo "Deploying to Worker 3 ($WORKER3_HOST)..."
    rsync -az --progress "${PARTS[2]}" "${WORKER3_USER}@${WORKER3_HOST}:${WORKER3_DIR}/emails.txt"
fi

echo ""
echo "=== Deploy Complete ==="
echo "Worker 1: $(wc -l < "${PARTS[0]}") emails"
echo "Worker 2: $(wc -l < "${PARTS[1]}") emails"
[ ${#PARTS[@]} -ge 3 ] && echo "Worker 3: $(wc -l < "${PARTS[2]}") emails"
```

- [ ] **Step 2: Make executable**

```bash
chmod +x scripts/split_deploy.sh
```

- [ ] **Step 3: Commit**

```bash
git add scripts/split_deploy.sh
git commit -m "feat(scripts): add split_deploy.sh for email distribution"
```

---

## Task 9: Cron Setup for VN-TAKK

**Files:**
- Create: `scripts/cron_vn_takk.sh`

- [ ] **Step 1: Create cron wrapper script**

```bash
#!/usr/bin/env bash
set -euo pipefail

LOG_DIR="/opt/vn-takk/logs"
mkdir -p "$LOG_DIR"
TIMESTAMP=$(date +%Y-%m-%d_%H-%M-%S)
LOG_FILE="$LOG_DIR/vn-takk_$TIMESTAMP.log"

# Configuration — one entry per tenant
# Format: tenant_id:accounts_file
TENANTS=(
    "TENANT_A_ID:/opt/vn-takk/accounts_tenant_a.txt"
    "TENANT_B_ID:/opt/vn-takk/accounts_tenant_b.txt"
    "TENANT_C_ID:/opt/vn-takk/accounts_tenant_c.txt"
)

REDIS_ADDR="127.0.0.1:6379"
THREADS=100

echo "=== VN-TAKK Cron Run: $TIMESTAMP ===" >> "$LOG_FILE"

for entry in "${TENANTS[@]}"; do
    IFS=':' read -r tenant_id accounts_file <<< "$entry"
    echo "[$(date)] Processing tenant: $tenant_id" >> "$LOG_FILE"

    python3 /opt/vn-takk/main.py \
        --redis "$REDIS_ADDR" \
        --tenant "$tenant_id" \
        --accounts "$accounts_file" \
        --threads "$THREADS" \
        >> "$LOG_FILE" 2>&1

    echo "[$(date)] Finished tenant: $tenant_id" >> "$LOG_FILE"
done

echo "=== VN-TAKK Cron Complete ===" >> "$LOG_FILE"

# Clean up logs older than 7 days
find "$LOG_DIR" -name "vn-takk_*.log" -mtime +7 -delete
```

- [ ] **Step 2: Make executable**

```bash
chmod +x scripts/cron_vn_takk.sh
```

- [ ] **Step 3: Add crontab entry (manual step)**

```bash
# Add to crontab on VPS trung tam:
# crontab -e
0 */6 * * * /opt/scripts/cron_vn_takk.sh
```

- [ ] **Step 4: Commit**

```bash
git add scripts/cron_vn_takk.sh
git commit -m "feat(scripts): add cron wrapper for VN-TAKK multi-tenant"
```

---

## Task 10: Integration Test & Deployment Guide

**Files:**
- Create: `scripts/deploy_central.sh`

- [ ] **Step 1: Create central VPS deploy script**

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "=== Central VPS Deployment ==="

# 1. Install Redis if not present
if ! command -v redis-server &> /dev/null; then
    echo "Installing Redis..."
    apt-get update && apt-get install -y redis-server
fi

# Ensure Redis binds to localhost only
grep -q "^bind 127.0.0.1" /etc/redis/redis.conf || {
    sed -i 's/^bind .*/bind 127.0.0.1/' /etc/redis/redis.conf
    systemctl restart redis-server
}

echo "Redis: $(redis-cli ping)"

# 2. Build Token API
echo "Building Token API..."
cd /opt/token-api
go build -o token-api .
echo "Token API built"

# 3. Install Python deps for VN-TAKK
echo "Installing VN-TAKK dependencies..."
cd /opt/vn-takk
pip3 install -r requirements.txt

# 4. Create systemd service for Token API
cat > /etc/systemd/system/token-api.service << 'EOF'
[Unit]
Description=Token API Server
After=redis-server.service
Requires=redis-server.service

[Service]
Type=simple
ExecStart=/opt/token-api/token-api -redis 127.0.0.1:6379 -listen :8080
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable token-api
systemctl start token-api

echo "Token API status: $(systemctl is-active token-api)"

# 5. Set up cron
echo "Setting up cron..."
cp /opt/scripts/cron_vn_takk.sh /opt/scripts/
chmod +x /opt/scripts/cron_vn_takk.sh

# Add cron if not exists
(crontab -l 2>/dev/null | grep -q "cron_vn_takk" ) || {
    (crontab -l 2>/dev/null; echo "0 */6 * * * /opt/scripts/cron_vn_takk.sh") | crontab -
    echo "Cron added"
}

echo ""
echo "=== Deployment Complete ==="
echo "Token API: http://0.0.0.0:8080"
echo "Test: curl http://localhost:8080/stats"
```

- [ ] **Step 2: Create worker VPS deploy script**

Create `scripts/deploy_worker.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

# Configuration — set these per worker
CENTRAL_API="${CENTRAL_API:-http://central-ip:8080}"
TENANT_ID="${TENANT_ID:-}"
MAX_CPM="${MAX_CPM:-20000}"
WORKERS="${WORKERS:-500}"

if [ -z "$TENANT_ID" ]; then
    echo "Error: TENANT_ID not set"
    echo "Usage: TENANT_ID=xxx CENTRAL_API=http://ip:8080 ./deploy_worker.sh"
    exit 1
fi

echo "=== Worker VPS Deployment ==="
echo "API: $CENTRAL_API"
echo "Tenant: $TENANT_ID"
echo "CPM: $MAX_CPM"

# 1. Build CHECK_CONNECTION
echo "Building CHECK_CONNECTION..."
cd /opt/check-connection
go build -o check_connection .
echo "Built successfully"

# 2. Create systemd service
cat > /etc/systemd/system/check-connection.service << EOF
[Unit]
Description=CHECK_CONNECTION Worker ($TENANT_ID)

[Service]
Type=simple
WorkingDirectory=/opt/check-connection
ExecStart=/opt/check-connection/check_connection \
    -api $CENTRAL_API \
    -tenant $TENANT_ID \
    -emails emails.txt \
    -max-cpm $MAX_CPM \
    -workers $WORKERS
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable check-connection

echo ""
echo "=== Worker Deployment Complete ==="
echo "Start: systemctl start check-connection"
echo "Logs:  journalctl -u check-connection -f"
```

- [ ] **Step 3: Make scripts executable**

```bash
chmod +x scripts/deploy_central.sh scripts/deploy_worker.sh
```

- [ ] **Step 4: Commit**

```bash
git add scripts/deploy_central.sh scripts/deploy_worker.sh
git commit -m "feat(scripts): add deployment scripts for central and worker VPS"
```

---

## Self-Review Checklist

- [x] **Spec coverage:** All 4 components covered (Token API, VN-TAKK mod, CHECK_CONNECTION mod, split_deploy.sh). Token lifecycle, buffer 2 lop, cron setup, deployment — all addressed.
- [x] **Placeholder scan:** No TBD/TODO found. All code blocks are complete.
- [x] **Type consistency:** `TokenData` (API) matches `APITokenData` (client). `TokenInfo` reused from existing codebase. `ExhaustedRequest.Email` matches across API and client.
- [x] **Scope check:** Each task is self-contained and produces a working commit.
