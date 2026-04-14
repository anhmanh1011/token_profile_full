# Get_Profile Module -- Redis Token Integration Design

## Overview

Refactor `Get_Profile` module to load tokens from Redis (`redis-tokens-{N}`) instead of file (`tokens.txt`). Remove refresh token rotation. Add real-time dead token cleanup via HDEL.

**Approach:** Minimal Redis Swap (Approach A) -- thay token loading layer, giữ nguyên 95% code hiện tại.

## Decisions

| Decision | Choice |
|----------|--------|
| Token source | Redis Hash `redis-tokens-{N}` via HGETALL |
| Email source | File `emails.txt` (unchanged) |
| Queue selection | 1 queue via `-queue` flag |
| Output | File `result_TIMESTAMP.txt` (unchanged) |
| Token refresh/rotation | Removed entirely |
| Dead token cleanup | Real-time HDEL via background goroutine + channel |
| Max CPM | 20,000 (hard limit, unchanged) |
| NumWorkers | Configurable via `-workers` flag (default 350) |

## Architecture

### Data Flow

```
redis-tokens-{N} ──→ [RedisLoader] ──→ [TokenManager] ──→ workers ──→ result_TIMESTAMP.txt
     (HGETALL)            │                  │                ↑
                          │                  │    emails.txt ─┘
                          │                  ↓
                          │           deadChan (buffer 1K)
                          │                  ↓
                          │         [cleanup goroutine]
                          │                  ↓
                          └──── redis-tokens-{N} (HDEL dead tokens)
```

### Goroutine Model

| Goroutine | Count | Purpose |
|-----------|-------|---------|
| Email reader | 1 | Stream emails to channel |
| Workers | configurable (default 350) | Process emails with tokens |
| Progress reporter | 1 | Stats every 5 seconds |
| Dead token cleanup | 1 | Batch HDEL from deadChan |
| Result writer flusher | 1 | Background flush to file |

## File Changes

### New: `token/redis.go`

`RedisLoader` struct with:

- `NewRedisLoader(addr string) (*RedisLoader, error)` -- connect + ping
- `LoadTokens(queueName string) ([]TokenInfo, error)` -- HGETALL, parse values
- `DeleteTokens(queueName string, emails []string) error` -- HDEL batch
- `StartCleanupWorker(ctx context.Context, queueName string, deadChan <-chan string)` -- background goroutine: gom batch 50 emails hoac timeout 5s, goi HDEL
- `Close() error` -- dong connection

**Token format parse:**

```
Redis value:  "user@domain.com|eyJ0eXAi...|tenant-uuid"
                    ↓ split by "|"
TokenInfo {
    Email:        "user@domain.com"
    RefreshToken: "eyJ0eXAi..."
    TenantID:     "tenant-uuid"
}
```

**Validation:** Value must contain exactly 2 `|` separators (3 fields). Skip entries that don't match, log warning.

### Modified: `token/manager.go`

**Remove:**
- `saveRefreshedToken()` function
- `refreshedTokens` channel (10K buffer)
- Background token saver goroutine
- `SaveAliveTokens()` function
- Logic to store new refresh_token after OAuth refresh

**Modify:**
- `refreshAccessToken(info)`: after OAuth call, save only `access_token` and `expires_at`. Ignore `new_refresh_token` in response.
- `MarkDeadAndRelease(token)`: additionally push email to `deadChan`

**Keep unchanged:**
- Token queue mode (acquire/release via channel)
- Atomic flags: dead, exhausted, failCount
- Per-token mutex for refresh atomicity
- Quota exhaustion detection (5 consecutive 500s, daily reset)
- `refreshAccessToken()` OAuth call to get access_token

### Modified: `main.go`

**Flags:**

```
REMOVED:
  -tokens     (file path, replaced by Redis)
  -refreshed  (refreshed tokens output, no longer needed)

ADDED:
  -queue      string  Redis queue suffix (required, e.g. "1" -> redis-tokens-1)
  -redis      string  Redis address (default "localhost:6379")
  -workers    int     Number of workers (default 350, tunable for performance)

KEPT:
  -emails     string  Path to emails file
  -result     string  Path to result file
  -id         string  Instance ID
  -max-cpm    int     Max CPM (default 20000)
```

**Startup flow:**

1. Parse flags, validate `-queue` required
2. Connect Redis, ping check -- fatal on failure
3. `HGETALL redis-tokens-{queue}` -- load all tokens, fatal if empty
4. Load emails from file
5. Create `deadChan` (buffered 1K)
6. Start dead token cleanup goroutine
7. Start workers + progress reporter
8. Wait for completion

**Shutdown flow:**

1. Cancel context (Ctrl+C or all emails processed)
2. Wait all workers done
3. Flush result writer
4. Close `deadChan`, drain remaining -- final HDEL batch
5. Close Redis connection
6. Print final stats

**Removed from shutdown:**
- Save `tokens_alive.txt`
- Save `tokens_refreshed.txt`

### Modified: `config/config.go`

```
ADDED:
  RedisAddr   string  // "localhost:6379"
  RedisQueue  string  // "1"

REMOVED:
  TokensFile  string  // no longer needed
```

### Modified: `go.mod`

Add dependency: `github.com/redis/go-redis/v9`

### Unchanged

- `api/client.go` -- Loki API client, connection pooling, status handling
- `reader/file.go` -- email reader, streaming, validation
- `writer/result.go` -- result writer, buffering, flushing
- `models/profile.go` -- data structures
- `worker/pool.go` -- worker pool, rate limiter (20K CPM), token queue mode

## Dead Token Cleanup (Real-time)

### Flow

```
Worker: token 401/424 → mark dead → push email to deadChan
                                           ↓
Cleanup goroutine: collect batch (50 emails OR 5s timeout)
                                           ↓
                                    HDEL redis-tokens-{N} email1 email2 ...
```

### Cleanup goroutine behavior

- Buffer: accumulate emails from `deadChan`
- Flush when: 50 emails collected OR 5 seconds since last flush (whichever first)
- On context cancel: drain channel, flush remaining batch
- HDEL error: log error, continue (best-effort, no retry)

## Error Handling

| Situation | Action |
|-----------|--------|
| Redis down at startup | Fatal exit: `"Cannot connect to Redis at %s"` |
| Redis queue empty | Fatal exit: `"No tokens in redis-tokens-%s"` |
| Redis down mid-run (HDEL fail) | Log error, continue. Cleanup next run |
| All tokens dead | Workers auto-shutdown (existing logic) |
| Token 401/424 | Mark dead, push deadChan, HDEL, worker gets new token |
| Token 403 (rate limit) | Backoff 500ms, retry same token |
| Token 5xx (quota) | 5 consecutive -> mark exhausted, daily reset |
| Max CPM exceeded | Rate limiter blocks workers (existing) |
| emails.txt not found | Fatal exit (existing) |
| `-queue` flag missing | Fatal exit: `"Required flag: -queue"` |
| Token parse error | Log warning, skip entry, continue loading |

## Rate Limiting

Hard limit: **20,000 CPM** (requests per minute).

Implementation unchanged: `golang.org/x/time/rate.Limiter` with rate = 20000/60 = 333 req/s, burst = 333. Every worker calls `limiter.Wait(ctx)` before each API call.

`-max-cpm` flag allows override but defaults to 20000. This is the absolute ceiling for the Loki API.

## Environments

| Env | OS | Redis |
|-----|-----|-------|
| Test | Windows | Memurai (`localhost:6379`) |
| Production | Ubuntu Linux VPS | redis-server (`localhost:6379`) |

Code must remain cross-platform. No OS-specific imports.
