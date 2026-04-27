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
admin_token.json (refresh_token + tenant_id + proxy)
       │
       ▼
  [Manage_User API Service]  (Python, localhost:5000)
  StartupCleaner → TokenProducer (background thread)
  Admin OAuth + token_getter → SOCKS5 (socks5h://, DNS qua proxy)
       │
       ├── GET  /tokens/next    → refresh_token (Go tự exchange sang access_token)
       ├── POST /users/delete   → batch delete users
       ├── GET  /proxy          → expose SOCKS5 URL cho Go startup
       └── GET  /status         → monitoring
       │
       ▼
  [Get_Profile]  (Go, batch job)
  APIClient → TokenManager → WorkerPool(400) → Loki API → result.txt
  Loki + token-exchange clients → SOCKS5 (qua x/net/proxy);
  /tokens/next & /users/delete → direct (localhost)
       │
       └── POST /users/delete (batch 20, dead token cleanup)
```

### Manage_User module contracts

| Module | Class | Input | Output |
|--------|-------|-------|--------|
| `admin_token_manager.py` | `AdminTokenManager` | admin dict | Shared OAuth token manager (thread-safe, auto-refresh, rotation tracking) |
| `cleanup.py` | `StartupCleaner` | `AdminTokenManager` | `{found, deleted, failed, purged}` — soft-delete + permanently purge `bot_` users |
| `creator.py` | `BulkUserCreator` | `AdminTokenManager`, count | `{created_users: [{email, password}], failed, licensed}` |
| `token_getter.py` | `BulkTokenGetter` | `[{email, password}]` | `{tokens: [{email, refresh_token, tenant_id}], failed}` |
| `producer.py` | `TokenProducer` | `AdminTokenManager`, queue.Queue | Background thread, keeps ≥100 tokens in queue; passes admin proxy to BulkTokenGetter; backfills missing tenant_id with admin tenant |
| `deleter.py` | `FastBulkDeleter` | `AdminTokenManager` | `_process_batch(emails)` → `[{email, success, [error]}]` per email |
| `proxy_config.py` | `parse_proxy`, `proxies_dict` | `host:port[:user:pass]` hoặc URL | Helpers normalize to `socks5h://` URL + build `requests`/`curl_cffi` proxies dict |
| `app.py` | Flask app | `--host`, `--port` | HTTP API service (entry point); exposes `/proxy` |

`app.py` is the entry point. All modules share a single `AdminTokenManager` instance for admin OAuth.

### Get_Profile module structure

| Package | File | Responsibility |
|---------|------|----------------|
| `token` | `api.go` | HTTP client for Python API (`/tokens/next`, `/users/delete`, `/proxy`) |
| `token` | `exchange.go` | `ExchangeRefreshToken` — swap refresh_token → Loki access_token (2 retries on transient) |
| `token` | `manager.go` | Token queue, dead notification, lazy access_token cache per TokenInfo; `NewManagerWithProxy` builds proxied HTTP client for token exchange |
| `api` | `client.go` | Loki API client, connection pooling, gzip; `NewClientWithProxy` routes via SOCKS5 dialer |
| `proxy` | `proxy.go` | `Parse` (legacy `host:port[:user:pass]` → `socks5h://` URL) + `SOCKS5DialContext` (x/net/proxy, hostname resolution on proxy side, no DNS leak) |
| `progress` | `bitmap.go` | Line-indexed bitmap checkpoint (LoadOrCreate, atomic Set, SaveAtomic via tmp+rename, 10s auto-save) |
| `worker` | `pool.go` | Worker pool, rate limiter (20K CPM), token queue mode, bitmap terminal-marking |
| `reader` | `file.go` | Streaming email reader, per-line counter, bitmap skip; pushes via `pool.Submit` callback |
| `writer` | `result.go` | Async buffered result writer |

`main.go` wires everything — it is the only entry point. Flags: `--api` (default `http://localhost:5000`), `--emails`, `--result`, `--checkpoint` (default `<emails>.ckpt`), `--workers`, `--max-cpm`, `--proxy` (override; empty = fetch once from Python `/proxy`).

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
- **Refresh tokens in queue, Go exchanges lazily**: Python browser flow returns the raw refresh_token (~24h TTL) — no Loki rescope on the Python side. Queue contains refresh_token + tenant_id; Go's `token.Manager.GetAccessToken` lazily exchanges to a Loki access_token (~1h TTL) on first use and re-exchanges when the cached access_token nears expiry, so one refresh_token yields many access_tokens over its 24h window. Exchange failures retry 2× with 1s backoff; permanent failures (invalid_grant) or 401/424/500-exhausted responses still trigger user deletion as before.
- **Permanently delete users**: All user deletions (cleanup + runtime) are permanent — soft-delete via Graph API followed by `DELETE /directory/deletedItems/{id}` to purge from recycle bin. Prevents Directory_QuotaExceeded.
- **Graph Batch API**: Both deleter and creator use `/$batch` endpoint with `BATCH_SIZE=20` (Graph API max per batch).
- **Threading model (Python)**: creator (2 workers), token_getter (30 workers), producer (1 background thread).
- **Concurrency model (Go)**: 400 worker goroutines, rate limiter 20K CPM, token queue mode.
- **Token producer auto-refill**: Background thread keeps ≥100 tokens in `queue.Queue(maxsize=1000)`. Creates batch of 100 users at a time. Flask waits for queue to fill before accepting requests.
- **Batch token API**: `GET /tokens/next?count=300` returns up to 300 tokens per call (cap server-side is 500). Go pre-fetches 300 tokens at startup, background goroutine refills 300 when pool < 100.
- **Batch user deletion**: Go batches dead+exhausted token emails into groups of 20, flushes every 5s or when full → `POST /users/delete` → soft-delete + permanently purge.
- **Resume-safe bitmap checkpoint**: `progress.Bitmap` tracks per-line terminal processing (20 MB for 160M lines). Reader assigns `LineIdx` = physical line number (every line, including invalid — option A); on restart, lines with bit set are skipped. Workers set bit **only** on terminal outcomes (success, 403, 500-below-threshold, other 5xx, other errors) — **not** on requeue (401/424, exchange-fail, 500-exhausted). Auto-save every 10s via `os.Rename(tmp, final)` → crash-safe. Fingerprint = SHA-256 of (size ‖ first 64KB ‖ last 64KB); mismatch invalidates bitmap. First run counts lines once (~10s for 5 GB); subsequent runs read totalLines from header. Reader pushes jobs directly to `pool.jobsChan` via `pool.Submit` callback (single channel, no intermediate pump goroutine).
- **SOCKS5 proxy (single source of truth)**: `proxy` field of `admin_token.json` carries the SOCKS5 (legacy `host:port[:user:pass]` or `socks5h://...` URL). `proxy_config.parse_proxy` normalises to `socks5h://` so DNS resolves on the proxy (no leak, geolocation consistent for MS login). Manage_User: AdminTokenManager session + token_getter (`curl_cffi`) both route through it. Get_Profile: at startup `--proxy` overrides; otherwise calls `GET /proxy` **once** to fetch the URL — no runtime reload. The same dialer is shared by Loki client (`api.NewClientWithProxy`) and refresh-token exchange (`token.NewManagerWithProxy`). Localhost API calls (`/tokens/next`, `/users/delete`) always go direct. Changing the proxy in `Manage_User/admin_token.json` requires **restarting Get_Profile** (Manage_User picks it up next admin-OAuth refresh).

## Environments

- **Test**: Windows, `localhost:5000` (Flask dev server)
- **Production**: Ubuntu Linux VPS, `localhost:5000` (consider gunicorn for production)

Code must be cross-platform. No `ctypes.windll`, no `os.system("clear")`, use `pathlib.Path` for file paths (Python), forward slashes (Go).
