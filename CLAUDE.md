# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Setup
cd Manage_User && pip install -r requirements.txt

# Run pipeline
cd Manage_User && python main.py

# Verify Redis connection
python -c "import redis; r = redis.Redis(); print(r.ping())"

# Syntax check all modules
python -m py_compile redis_pusher.py && python -m py_compile deleter.py && python -m py_compile creator.py && python -m py_compile token_getter.py && python -m py_compile main.py
```

Note: On the developer's Windows machine, use `py` instead of `python`.

## Architecture

This is a Microsoft 365 user management pipeline. Each admin in `admin_token.json` runs sequentially through:

```
FastBulkDeleter → BulkUserCreator → RedisPusher → BulkTokenGetter → RedisPusher
```

### Module contracts

| Module | Class | Input | Output |
|--------|-------|-------|--------|
| `deleter.py` | `FastBulkDeleter` | admin dict | `{deleted, failed, skipped_admins, new_refresh_token}` |
| `creator.py` | `BulkUserCreator` | admin dict, count | `{created_users: [{email, password}], failed, licensed, new_refresh_token}` |
| `token_getter.py` | `BulkTokenGetter` | `[{email, password}]` | `{tokens: [{email, refresh_token, tenant_id}], failed}` |
| `redis_pusher.py` | `RedisPusher` | queue_name, data list | pushes to Redis Hash (HSET) |

`main.py` orchestrates these — it is the only entry point.

### Key design decisions

- **Browser flow for tokens**: `token_getter.py` uses `curl_cffi` to impersonate a browser for Microsoft Teams OAuth. This is intentional — do not replace with ROPC or Graph API.
- **1 admin = 1 Redis queue**: `redis_queue` field in `admin_token.json` is the suffix (e.g. `"1"` → `redis-users-1`, `redis-tokens-1`).
- **Redis Hash (HSET)**: key = email, value = password (users) or `email|refresh_token|tenant_id` (tokens).
- **Backup before Redis**: Files are always written to `backup/` before Redis push, as a recovery mechanism.
- **Graph Batch API**: Both deleter and creator use `/$batch` endpoint with `BATCH_SIZE=20` (Graph API max).
- **Threading model**: deleter (3 workers), creator (4 workers), token_getter (30 workers). Each uses `threading.Lock` for stats.

### Legacy files

`delete_fast.py`, `create_user.py`, `get_refresh_token_user.py` are the original standalone scripts. The refactored versions are `deleter.py`, `creator.py`, `token_getter.py`. Legacy files are kept for reference only.

## Environments

- **Test**: Windows, Memurai (Redis-compatible), `localhost:6379`
- **Production**: Ubuntu Linux VPS, `redis-server`, `localhost:6379`

Code must be cross-platform. No `ctypes.windll`, no `os.system("clear")`, use `pathlib.Path` for file paths.
