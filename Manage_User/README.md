# Manage_User Module

Pipeline tự động quản lý lifecycle của normal users trên Microsoft 365 tenant.

## Pipeline Flow

```
Delete old users → Create 10K users → Push to Redis → Get tokens → Push tokens to Redis
```

Mỗi admin trong `admin_token.json` chạy tuần tự qua toàn bộ pipeline.

## Cấu trúc

```
Manage_User/
├── main.py              # Orchestrator — entry point
├── deleter.py           # Xoá bulk normal users (bảo vệ admin)
├── creator.py           # Tạo bulk users qua Graph Batch API
├── token_getter.py      # Lấy refresh_token qua browser flow
├── redis_pusher.py      # Push data lên Redis Hash + backup
├── admin_token.json     # Config admin credentials
├── requirements.txt     # Python dependencies
└── backup/              # Auto-generated backup files
```

## Yêu cầu

- Python 3.10+
- Redis server (localhost:6379)

## Cài đặt

```bash
cd Manage_User
pip install -r requirements.txt
```

### Redis

**Windows (test):**
- [Memurai](https://www.memurai.com/) — Redis-compatible cho Windows

**Linux (production):**
```bash
sudo apt install redis-server
sudo systemctl enable redis-server
sudo systemctl start redis-server
```

## Chạy

```bash
python main.py
```

Pipeline sẽ tự động:
1. Xoá toàn bộ normal users (bảo vệ admin users)
2. Tạo mới users (mặc định 10.000)
3. Push credentials vào Redis `redis-users-{N}`
4. Lấy refresh token cho từng user qua browser flow
5. Push tokens vào Redis `redis-tokens-{N}`

## Cấu hình

### admin_token.json

```json
[
    {
        "refresh_token": "...",
        "tenant_id": "35921bf1-...",
        "domain": "example.onmicrosoft.com",
        "username": "admin@example.onmicrosoft.com",
        "redis_queue": "1"
    }
]
```

| Field | Mô tả |
|-------|--------|
| `refresh_token` | Admin refresh token (tự động cập nhật sau mỗi lần chạy) |
| `tenant_id` | Azure AD tenant ID |
| `domain` | Domain của tenant |
| `username` | Email admin |
| `redis_queue` | Suffix cho Redis queue (e.g. `"1"` → `redis-users-1`, `redis-tokens-1`) |

### Tuỳ chỉnh

Trong `main.py`:
- `DEFAULT_USER_COUNT` — Số user tạo mỗi admin (mặc định: 10000)

Trong `token_getter.py`:
- `DEFAULT_WORKERS` — Số threads lấy token đồng thời (mặc định: 30)

## Redis Data Format

### redis-users-{N} (Hash)

```
HGETALL redis-users-1
> user1@domain.com → P@ssw0rd123
> user2@domain.com → Xyz!456abc
```

### redis-tokens-{N} (Hash)

```
HGETALL redis-tokens-1
> user1@domain.com → user1@domain.com|refresh_token_abc...|tenant-id-123
> user2@domain.com → user2@domain.com|refresh_token_def...|tenant-id-123
```

## Backup

Mỗi lần chạy tự động ghi backup vào `backup/`:
- `backup/YYYY-MM-DD_redis-users-{N}.txt`
- `backup/YYYY-MM-DD_redis-tokens-{N}.txt`

Dùng để recovery thủ công nếu Redis gặp sự cố.

## Logging

Log ghi ra console và file `pipeline.log`. Các mức:
- **INFO** — Tiến trình mỗi bước
- **WARNING** — Partial failures
- **ERROR** — Bước fail hoàn toàn

## Error Handling

| Bước | Nếu fail | Hành vi |
|------|----------|---------|
| Delete | Partial | Log warning, tiếp tục |
| Create | < target | Tiếp tục với số users đã tạo |
| Push users Redis | Redis down | Dừng push, backup đã có, tiếp tục get token |
| Get token | Một số fail | Tiếp tục với tokens đã lấy |
| Push tokens Redis | Redis down | Log error, backup đã có |
