# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Manage_User setup & run
cd Manage_User && pip install -r requirements.txt
cd Manage_User && python main.py

# Get_Profile build & run
cd Get_Profile && go build -o get_profile.exe .
cd Get_Profile && ./get_profile.exe -queue 1

# Verify Redis connection
python -c "import redis; r = redis.Redis(); print(r.ping())"

# Syntax check Python modules
cd Manage_User && python -m py_compile redis_pusher.py && python -m py_compile deleter.py && python -m py_compile creator.py && python -m py_compile token_getter.py && python -m py_compile main.py

# Build & vet Go modules
cd Get_Profile && go build . && go vet ./...
```

Note: On the developer's Windows machine, use `py` instead of `python`.

## Architecture

Two-stage pipeline: **Manage_User** (Python) provisions users and tokens into Redis, **Get_Profile** (Go) consumes tokens to fetch LinkedIn profiles.

```
admin_token.json
       │
       ▼
  [Manage_User Pipeline]  (Python)
  FastBulkDeleter → BulkUserCreator → RedisPusher → BulkTokenGetter → RedisPusher
       │
       ├── redis-users-{N}   (Hash, TTL 20h)
       └── redis-tokens-{N}  (Hash, TTL 20h)
       │
       ▼
  [Get_Profile]  (Go)
  RedisLoader → TokenManager → WorkerPool(550) → Loki API → result.txt
       │
       └── HDEL dead tokens from redis-tokens-{N} (real-time cleanup)
```

### Manage_User module contracts

| Module | Class | Input | Output |
|--------|-------|-------|--------|
| `deleter.py` | `FastBulkDeleter` | admin dict | `{deleted, failed, skipped_admins, new_refresh_token}` |
| `creator.py` | `BulkUserCreator` | admin dict, count | `{created_users: [{email, password}], failed, licensed, new_refresh_token}` |
| `token_getter.py` | `BulkTokenGetter` | `[{email, password}]` | `{tokens: [{email, refresh_token, tenant_id}], failed}` |
| `redis_pusher.py` | `RedisPusher` | queue_name, data list | pushes to Redis Hash (HSET) with 20h TTL |

`main.py` orchestrates these — it is the only entry point.

### Get_Profile module structure

| Package | File | Responsibility |
|---------|------|----------------|
| `token` | `redis.go` | Redis HGETALL load, HDEL dead token cleanup goroutine |
| `token` | `manager.go` | OAuth refresh, token queue, dead notification via channel |
| `api` | `client.go` | Loki API client, connection pooling, gzip |
| `worker` | `pool.go` | Worker pool, rate limiter (20K CPM), token queue mode |
| `reader` | `file.go` | Streaming email reader from file |
| `writer` | `result.go` | Async buffered result writer |

`main.go` wires everything — it is the only entry point. Required flag: `-queue`.

### Key design decisions

- **Browser flow for tokens**: `token_getter.py` uses `curl_cffi` to impersonate a browser for Microsoft Teams OAuth. Do not replace with ROPC or Graph API.
- **1 admin = 1 Redis queue**: `redis_queue` field in `admin_token.json` is the suffix (e.g. `"1"` → `redis-users-1`, `redis-tokens-1`).
- **Redis Hash (HSET)**: key = email, value = password (users) or `email|refresh_token|tenant_id` (tokens). TTL 20 hours.
- **Backup before Redis**: Files are always written to `backup/` before Redis push, as a recovery mechanism.
- **Graph Batch API**: Both deleter and creator use `/$batch` endpoint with `BATCH_SIZE=20` (Graph API max per batch).
- **Threading model (Python)**: deleter (2 workers), creator (2 workers), token_getter (30 workers). Each uses `threading.Lock` for stats.
- **Concurrency model (Go)**: 550 worker goroutines, rate limiter 20K CPM, token queue mode with per-token lock.
- **Dead token cleanup**: Real-time HDEL via background goroutine + buffered channel (batch 50 or 5s flush).
- **No refresh token rotation in Get_Profile**: Tokens are consumed from Redis and not saved back.

### Legacy files

`delete_fast.py`, `create_user.py`, `get_refresh_token_user.py` are the original standalone scripts. The refactored versions are `deleter.py`, `creator.py`, `token_getter.py`. Legacy files are kept for reference only.

## Environments

- **Test**: Windows, Memurai (Redis-compatible), `localhost:6379`
- **Production**: Ubuntu Linux VPS, `redis-server`, `localhost:6379`

Code must be cross-platform. No `ctypes.windll`, no `os.system("clear")`, use `pathlib.Path` for file paths (Python), forward slashes (Go).
