# Manage_User Module — Design Spec

## Context

Module `Manage_User` quản lý lifecycle hàng ngày của normal users trên Microsoft 365 tenant:
1. Xoá toàn bộ normal users (non-admin) cũ
2. Tạo mới 10K normal users
3. Lấy refresh_token cho từng user qua browser flow
4. Đẩy tất cả lên Redis queue để downstream consumers sử dụng

Hiện tại có 3 file standalone scripts chưa liên kết với nhau, chưa tích hợp Redis, và có code Windows-only. Cần refactor thành pipeline orchestrated, cross-platform, production-ready.

**Production**: Ubuntu Linux VPS  
**Test**: Windows PC  

## Decisions

- **Giữ browser flow** cho get_refresh_token (curl_cffi) — không đổi sang ROPC/Graph API
- **Mỗi admin map 1:1 với Redis queue riêng** (admin_token.json chứa `redis_queue` field)
- **Redis Hash** cho data structure (HSET) — key=email, value=data
- **1 file main.py orchestrator** gọi tuần tự các module
- **Scheduling tính sau** — script chạy manual trước

## Architecture

### File Structure

```
Manage_User/
├── admin_token.json        # config admin (existing)
├── main.py                 # orchestrator entry point (new)
├── deleter.py              # refactor from delete_fast.py
├── creator.py              # refactor from create_user.py
├── token_getter.py         # refactor from get_refresh_token_user.py
├── redis_pusher.py         # new: Redis integration
├── requirements.txt        # new
└── backup/                 # auto-created: backup files
```

### Pipeline Flow (per admin)

```
admin_token.json
       |
       v
   [Delete all normal users]
       |
       v
   [Create 10K users] --> List[{email, password}]
       |                        |
       |                        v
       |                 [Push redis-users-N]
       v                   (HSET email -> password)
   [Get refresh tokens] --> List[{email, refresh_token, tenant_id}]
                                |
                                v
                         [Push redis-tokens-N]
                           (HSET email -> "email|rf_token|tenant_id")
```

Mỗi admin chạy tuần tự qua cả pipeline. Nhiều admin xử lý lần lượt.

## Module Details

### main.py — Orchestrator

```python
def run_pipeline(admin: dict, count: int = 10000):
    queue_suffix = admin["redis_queue"]  # "redis-1" -> suffix "1"

    # Step 1: Delete all normal users
    deleter = FastBulkDeleter(admin, auto_confirm=True)
    delete_result = deleter.run()

    # Step 2: Create new users
    creator = BulkUserCreator(admin, count)
    create_result = creator.run()
    created_users = create_result["created_users"]

    # Step 3: Push users to Redis
    redis = RedisPusher()
    redis.clear_queue(f"redis-users-{queue_suffix}")
    redis.push_users(f"redis-users-{queue_suffix}", created_users)

    # Step 4: Get refresh tokens via browser flow
    getter = BulkTokenGetter(created_users)
    token_result = getter.run()

    # Step 5: Push tokens to Redis
    redis.clear_queue(f"redis-tokens-{queue_suffix}")
    redis.push_tokens(f"redis-tokens-{queue_suffix}", token_result["tokens"])

    # Step 6: Update admin_token.json if refresh_token changed
    update_admin_token_if_changed(admin, create_result)

def main():
    admins = load_admin_token("admin_token.json")
    for admin in admins:
        run_pipeline(admin)
```

### deleter.py — Refactor from delete_fast.py

**Class**: `FastBulkDeleter`  
**Input**: admin dict, auto_confirm flag  
**Output**: `{"deleted": int, "failed": int, "skipped_admins": int}`

Changes from current delete_fast.py:
- Remove `input()` confirmation -> `auto_confirm=True` param for pipeline
- Remove colorama dependency -> plain logging
- Remove per-run log files -> return results to orchestrator
- Accept admin dict directly instead of reading file
- Keep: Graph Batch API logic, admin protection, retry/throttle handling

### creator.py — Refactor from create_user.py

**Class**: `BulkUserCreator`  
**Input**: admin dict, count (default 10000)  
**Output**: `{"created_users": [{"email": str, "password": str}, ...], "failed": int, "new_refresh_token": str | None}`

Changes from current create_user.py:
- Remove file output (`created_users.txt`) -> return list in memory
- Remove argparse -> orchestrator controls params
- Keep: Graph Batch API logic, token refresh, license auto-detect, threading

### token_getter.py — Refactor from get_refresh_token_user.py

**Class**: `BulkTokenGetter`  
**Input**: `List[{"email": str, "password": str}]`  
**Output**: `{"tokens": [{"email": str, "refresh_token": str, "tenant_id": str}, ...], "failed": int}`

Changes from current get_refresh_token_user.py (most changes):
- Remove `ctypes.windll.kernel32.SetConsoleTitleW` -> cross-platform or remove
- Remove `os.system("clear")` -> remove
- Remove `account.txt` file read -> accept list from orchestrator
- Remove file writes -> return results in memory
- Remove global variables (`Hits`, `Failed`) -> instance variables
- Fix backslash paths -> `os.path.join()` or pathlib
- Reduce default threads from 100 -> 30 (stability)
- Keep: TeamOutLook class, browser flow logic, curl_cffi, change password flow

### redis_pusher.py — New module

**Class**: `RedisPusher`  
**Config**: localhost:6379, no password

```python
class RedisPusher:
    def __init__(self, host="localhost", port=6379, db=0):
        self.client = redis.Redis(host=host, port=port, db=db)

    def clear_queue(self, queue_name: str):
        """DEL the hash before pushing new data (daily reset)"""
        self.client.delete(queue_name)

    def push_users(self, queue_name: str, users: list):
        """HSET queue_name email password"""
        pipe = self.client.pipeline()
        for user in users:
            pipe.hset(queue_name, user["email"], user["password"])
        pipe.execute()

    def push_tokens(self, queue_name: str, tokens: list):
        """HSET queue_name email 'email|refresh_token|tenant_id'"""
        pipe = self.client.pipeline()
        for token in tokens:
            value = f"{token['email']}|{token['refresh_token']}|{token['tenant_id']}"
            pipe.hset(queue_name, token["email"], value)
        pipe.execute()
```

Uses Redis pipeline for bulk insert performance.

## Redis Data Format

### redis-users-{N} (Hash)

| Key (field) | Value |
|-------------|-------|
| `user1@domain.com` | `P@ssw0rd123` |
| `user2@domain.com` | `Xyz!456abc` |

### redis-tokens-{N} (Hash)

| Key (field) | Value |
|-------------|-------|
| `user1@domain.com` | `user1@domain.com\|refresh_token_abc...\|tenant-id-123` |
| `user2@domain.com` | `user2@domain.com\|refresh_token_def...\|tenant-id-123` |

## Error Handling

| Step | If fails | Behavior |
|------|----------|----------|
| Delete | Partial delete | Log warning, continue — leftover old users don't block creation |
| Create | Created < 10K | Continue with successfully created users |
| Push users to Redis | Redis down | Stop pipeline for this admin, log error, move to next admin |
| Get token | Some users fail | Continue with successfully obtained tokens |
| Push tokens to Redis | Redis down | Log error, write backup file for manual recovery |

### Backup Files

On each run, write backup to `backup/` directory:
- `backup/YYYY-MM-DD_redis-users-{N}.txt` — format: `email:password` per line
- `backup/YYYY-MM-DD_redis-tokens-{N}.txt` — format: `email|refresh_token|tenant_id` per line

Backup is written regardless of Redis success — serves as recovery mechanism.

### Logging

Python `logging` module, output to console + `pipeline.log`:
- INFO: progress per step
- WARNING: partial failures
- ERROR: step completely failed

## Dependencies

### New
- `redis` — Redis client

### Existing (keep)
- `curl_cffi` — browser impersonation for token_getter
- `requests` — HTTP client for Graph API

### Removed
- `colorama` — replaced by plain logging (cross-platform)

## Cross-platform Compatibility

| Issue | Current (Windows-only) | Fix |
|-------|----------------------|-----|
| `ctypes.windll.kernel32.SetConsoleTitleW` | Crashes on Linux | Remove or guard with `platform.system() == "Windows"` |
| `os.system("clear")` | Unnecessary | Remove |
| Backslash paths `\\` | Fails on Linux | Use `os.path.join()` or `pathlib.Path` |

## admin_token.json Format (unchanged)

```json
[
    {
        "refresh_token": "...",
        "tenant_id": "...",
        "domain": "example.onmicrosoft.com",
        "username": "admin@example.onmicrosoft.com",
        "redis_queue": "1"
    }
]
```

`redis_queue` field value is used as suffix. Example: if `redis_queue: "1"` then queues are `redis-users-1` and `redis-tokens-1`. If `redis_queue: "redis-1"` (current value in file), queues would be `redis-users-redis-1` and `redis-tokens-redis-1`. Recommend changing the value to just `"1"` for cleaner naming.
