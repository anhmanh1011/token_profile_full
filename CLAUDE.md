# CLAUDE.md

This file provides guidance for coding agents working in this repository.

## Commands

```bash
# Manage_User: start API service
cd Manage_User && pip install -r requirements.txt
cd Manage_User && python app.py --port 5000 --config ../admin_token_config_global.json

# Get_Profile: build and run one pool
cd Get_Profile && go build -o get_profile.exe .
cd Get_Profile && ./get_profile.exe --api http://localhost:5000 --pool bot_p01

# Python checks
cd Manage_User && python -m unittest discover -s . -p "test_*.py"
cd Manage_User && python -m py_compile app.py admin_token_manager.py pool_config.py cleanup.py producer.py creator.py deleter.py token_getter.py proxy_config.py

# Go checks
cd Get_Profile && go test ./...
cd Get_Profile && go vet ./...
cd Get_Profile && go build .

# email-gen
cd email-gen && cargo build --release
cd email-gen && cargo test --release
cd email-gen && cargo clippy --release -- -D warnings
cd email-gen && cargo bench
```

On the developer's Windows machine, use `py` instead of `python`.

## Architecture

Three independent apps run on the same VPS:
- `Manage_User` (Python): long-running HTTP API service.
- `Get_Profile` (Go): batch job fetches LinkedIn profiles and consumes tokens over HTTP.
- `email-gen` (Rust): standalone CLI that generates email input files.

`Manage_User` and `Get_Profile` read only `admin_token_config_global.json` from the project root. Do not reintroduce `Manage_User/admin_token.json`.

```text
admin_token_config_global.json
       |
       v
Manage_User API Service (localhost:5000)
  one pool per enabled config entry
  per pool: AdminTokenManager, StartupCleaner, TokenProducer, Queue, delete stats
       |
       | GET  /pools/<pool_id>/tokens/next
       | POST /pools/<pool_id>/users/delete
       | GET  /pools/<pool_id>/proxy
       v
Get_Profile --pool <pool_id>
  APIClient scopes localhost calls to /pools/<pool_id>/...
  Loki + refresh-token exchange use the pool SOCKS5 proxy
  checkpoint defaults to <email_file>.<pool_id>.ckpt
```

## Global Config Shape

Each enabled entry is one independent pool:

```json
{
  "refresh_token": "...",
  "tenant_id": "...",
  "username": "admin1",
  "domain": "example.com",
  "proxy": "host:port:user:pass",
  "email_file": "email1.txt",
  "bot_prefix": "bot_p01_",
  "pool_id": "bot_p01",
  "checkpoint_file": "optional.ckpt",
  "result_file": "optional.txt",
  "workers": 400,
  "max_cpm": 20000,
  "enabled": true
}
```

`pool_id` is optional. If omitted, it is derived from `bot_prefix` by trimming trailing underscores.

## Manage_User Contracts

| Module | Responsibility |
| --- | --- |
| `pool_config.py` | Load and validate enabled pools from `admin_token_config_global.json`. |
| `admin_token_manager.py` | Thread-safe admin OAuth manager; saves rotated refresh tokens back to the matching global config entry. |
| `cleanup.py` | Deletes and purges users matching that pool's `bot_prefix`. |
| `creator.py` | Creates users as `<bot_prefix><random>@<domain>`. |
| `producer.py` | Keeps one pool queue above low watermark 400 and refills toward 800. |
| `token_getter.py` | Browser-flow refresh token acquisition through the pool proxy. |
| `deleter.py` | Graph batch soft-delete; caller enforces pool scope and purge. |
| `app.py` | Flask entry point; owns pool registry and API endpoints. |

Pool-scoped endpoints:
- `GET /pools`
- `GET /status`
- `GET /pools/<pool_id>/status`
- `GET /pools/<pool_id>/tokens/next?count=N`
- `POST /pools/<pool_id>/users/delete`
- `GET /pools/<pool_id>/proxy`

Legacy endpoints `/tokens/next`, `/users/delete`, and `/proxy` are compatibility paths only. They require `pool_id` when more than one pool is configured.

## Get_Profile Contracts

| Package | File | Responsibility |
| --- | --- | --- |
| `config` | `global.go` | Load/select global config pool and derive isolated checkpoint/result defaults. |
| `token` | `api.go` | HTTP client for Manage_User; `SetPoolID` scopes paths to `/pools/<pool_id>`. |
| `token` | `exchange.go` | Exchange refresh token to Loki access token. |
| `token` | `manager.go` | Token queue, lazy access-token cache, dead-token notifications. |
| `api` | `client.go` | Loki API client with optional SOCKS5 dialer. |
| `progress` | `bitmap.go` | Resume-safe line-indexed bitmap checkpoint. |
| `worker` | `pool.go` | Worker pool, rate limiter, terminal bitmap marking. |
| `reader` | `file.go` | Streaming email reader with bitmap skip. |
| `writer` | `result.go` | Async buffered result writer. |

`main.go` flags of interest:
- `--config`: global config path; auto-detects root or parent.
- `--pool`: selected pool id; required when multiple pools are enabled.
- `--instance`: alias for `--pool`.
- `--emails`: override selected pool `email_file`.
- `--checkpoint`: override default `<emails>.<pool_id>.ckpt`.
- `--result`: override default `result_<pool_id>_<timestamp>.txt`.
- `--proxy`: override pool proxy.

## Key Decisions

- No Redis. Manage_User and Get_Profile communicate through localhost HTTP.
- One global config can contain many admin/proxy/email pools.
- Refresh tokens remain in the Python queue; Go exchanges lazily into Loki access tokens.
- User deletion is pool-scoped. Runtime delete rejects emails outside the selected pool domain or `bot_prefix`.
- Startup cleanup only touches the configured pool `bot_prefix`.
- Changing a pool proxy requires restarting Get_Profile for that pool.
- Checkpoints must remain pool-isolated unless the caller explicitly overrides `--checkpoint`.

## Environments

- Test: Windows, localhost:5000.
- Production: Ubuntu Linux VPS, localhost:5000.
- Code must stay cross-platform. Avoid OS-specific shell assumptions in app logic.
