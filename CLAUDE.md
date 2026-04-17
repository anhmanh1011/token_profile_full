# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Manage_User: start API service
cd Manage_User && pip install -r requirements.txt
cd Manage_User && python app.py --port 5000

# Get_Profile: build & run
cd Get_Profile && go build -o get_profile.exe .
cd Get_Profile && ./get_profile.exe --api http://localhost:5000

# Syntax check Python modules
cd Manage_User && python -m py_compile app.py && python -m py_compile cleanup.py && python -m py_compile producer.py && python -m py_compile creator.py && python -m py_compile deleter.py && python -m py_compile token_getter.py

# Build & vet Go modules
cd Get_Profile && go build . && go vet ./...

# email-gen: build, test, bench (Rust)
cd email-gen && cargo build --release
cd email-gen && cargo test --release
cd email-gen && cargo clippy --release -- -D warnings
cd email-gen && cargo bench
```

Note: On the developer's Windows machine, use `py` instead of `python`.

## Architecture

Three independent apps on the same VPS:
- **Manage_User** (Python) — long-running HTTP API service tạo users và phát access tokens.
- **Get_Profile** (Go) — batch job fetch LinkedIn profiles, consume tokens qua HTTP.
- **email-gen** (Rust) — CLI utility standalone sinh `emails.txt` (input cho Get_Profile) bằng cross-product domains × usernames.

**No Redis required.** Manage_User ↔ Get_Profile giao tiếp qua HTTP localhost. email-gen độc lập, user chạy tay sinh file.

```
admin_token.json
       │
       ▼
  [Manage_User API Service]  (Python, localhost:5000)
  StartupCleaner → TokenProducer (background thread)
       │
       ├── GET  /tokens/next    → access_token
       ├── POST /users/delete   → batch delete users
       └── GET  /status         → monitoring
       │
       ▼
  [Get_Profile]  (Go, batch job)
  APIClient → TokenManager → WorkerPool(550) → Loki API → result.txt
       │
       └── POST /users/delete (batch 20, dead token cleanup)
```

### Manage_User module contracts

| Module | Class | Input | Output |
|--------|-------|-------|--------|
| `admin_token_manager.py` | `AdminTokenManager` | admin dict | Shared OAuth token manager (thread-safe, auto-refresh, rotation tracking) |
| `cleanup.py` | `StartupCleaner` | `AdminTokenManager` | `{found, deleted, failed, purged}` — soft-delete + permanently purge `bot_` users |
| `creator.py` | `BulkUserCreator` | `AdminTokenManager`, count | `{created_users: [{email, password}], failed, licensed}` |
| `token_getter.py` | `BulkTokenGetter` | `[{email, password}]` | `{tokens: [{email, access_token, tenant_id}], failed}` |
| `producer.py` | `TokenProducer` | `AdminTokenManager`, queue.Queue | Background thread, keeps ≥500 tokens in queue |
| `deleter.py` | `FastBulkDeleter` | `AdminTokenManager` | `_process_batch(emails)` → `[{email, success, [error]}]` per email |
| `app.py` | Flask app | `--host`, `--port` | HTTP API service (entry point) |

`app.py` is the entry point. All modules share a single `AdminTokenManager` instance for admin OAuth.

### Get_Profile module structure

| Package | File | Responsibility |
|---------|------|----------------|
| `token` | `api.go` | HTTP client for Python API (`/tokens/next`, `/users/delete`) |
| `token` | `manager.go` | Token queue, dead notification, access token validation |
| `api` | `client.go` | Loki API client, connection pooling, gzip |
| `worker` | `pool.go` | Worker pool, rate limiter (20K CPM), token queue mode |
| `reader` | `file.go` | Streaming email reader from file |
| `writer` | `result.go` | Async buffered result writer |

`main.go` wires everything — it is the only entry point. Required: `--api` flag (default `http://localhost:5000`).

### email-gen module structure

| File | Trách nhiệm |
|------|-------------|
| `src/main.rs` | Entry point, wiring |
| `src/lib.rs` | Module re-exports |
| `src/config.rs` | Cli (clap derive) + Config validation + OutputFormat |
| `src/reader.rs` | Mmap + memchr line scan + dedup domains + load_usernames |
| `src/generator.rs` | Rayon cross-product + ByteBudget backpressure |
| `src/writer.rs` | Writer thread (crossbeam channel consumer) |
| `src/splitter.rs` | File rotation + gzip wrapping (SingleFile, Splitter) |
| `src/stats.rs` | Stats + print_vi + ram_peak (Linux /proc, Windows psapi) |
| `src/error.rs` | thiserror enums: Config/Reader/Gen/WriterError |

Default chunk-size = 2000 domains (không phải 20000 như spec gốc), để giữ RAM < 500 MB với 200 usernames. User override bằng `-c`.

### Key design decisions

- **Browser flow for tokens**: `token_getter.py` uses `curl_cffi` to impersonate a browser for Microsoft Teams OAuth. Do not replace with ROPC or Graph API.
- **1 VPS = 1 admin**: First admin from `admin_token.json` is used.
- **User prefix `bot_`**: All app-created users have `bot_` prefix in userPrincipalName. Startup cleanup lists and deletes all `bot_` users for crash recovery.
- **Shared AdminTokenManager**: Single `AdminTokenManager` instance handles admin OAuth for all modules. Thread-safe, auto-refresh, tracks refresh_token rotation, saves to `admin_token.json`.
- **Access tokens (not refresh tokens)**: Python gets loki-scoped access_token from browser flow. Go uses it directly (~50 min TTL). No refresh logic in Go — expired/dead/exhausted tokens trigger user deletion and new token fetch.
- **Permanently delete users**: All user deletions (cleanup + runtime) are permanent — soft-delete via Graph API followed by `DELETE /directory/deletedItems/{id}` to purge from recycle bin. Prevents Directory_QuotaExceeded.
- **Graph Batch API**: Both deleter and creator use `/$batch` endpoint with `BATCH_SIZE=20` (Graph API max per batch).
- **Threading model (Python)**: creator (2 workers), token_getter (30 workers), producer (1 background thread).
- **Concurrency model (Go)**: 550 worker goroutines, rate limiter 20K CPM, token queue mode.
- **Token producer auto-refill**: Background thread keeps ≥500 tokens in `queue.Queue(maxsize=1000)`. Creates batch of 100 users at a time. Flask waits for queue to fill before accepting requests.
- **Batch token API**: `GET /tokens/next?count=500` returns up to 500 tokens per call. Go pre-fetches 500 tokens at startup, background goroutine refills 500 when pool < 100.
- **Batch user deletion**: Go batches dead+exhausted token emails into groups of 20, flushes every 5s or when full → `POST /users/delete` → soft-delete + permanently purge.

## Environments

- **Test**: Windows, `localhost:5000` (Flask dev server)
- **Production**: Ubuntu Linux VPS, `localhost:5000` (consider gunicorn for production)

Code must be cross-platform. No `ctypes.windll`, no `os.system("clear")`, use `pathlib.Path` for file paths (Python), forward slashes (Go).
