# Token Profile Full

Hệ thống tự động quản lý user Microsoft 365 và thu thập LinkedIn profile.

## Modules

### [Manage_User](Manage_User/) (Python)

Pipeline tự động quản lý lifecycle normal users:
- Xoá toàn bộ normal users cũ (bảo vệ admin)
- Tạo mới 10K users qua Graph Batch API
- Lấy refresh token qua browser flow (curl_cffi)
- Push credentials và tokens lên Redis queue (TTL 20h)

### [Get_Profile](Get_Profile/) (Go)

High-performance LinkedIn profile fetcher:
- Load tokens từ Redis (`redis-tokens-{N}`)
- 550 worker goroutines, rate limit 20K CPM
- Call Loki API (Delve Office) lấy LinkedIn profile
- Real-time dead token cleanup (HDEL khỏi Redis)
- Output: `result_TIMESTAMP.txt`

## Kiến trúc tổng quan

```
admin_token.json (input)
       │
       ▼
  [Manage_User Pipeline]  ← Python
       │
       ├── redis-users-{N}   (Hash, email → password, TTL 20h)
       └── redis-tokens-{N}  (Hash, email → email|token|tenant, TTL 20h)
       │
       ▼
  [Get_Profile]  ← Go
       │
       ├── emails.txt (input: emails cần check)
       └── result_TIMESTAMP.txt (output: LinkedIn profiles)
```

## Yêu cầu

- Python 3.10+ (Manage_User)
- Go 1.21+ (Get_Profile)
- Redis server (Memurai on Windows / redis-server on Linux)

## Quick Start

```bash
# 1. Manage_User: tạo users và push tokens lên Redis
cd Manage_User
pip install -r requirements.txt
python main.py

# 2. Get_Profile: lấy LinkedIn profiles
cd Get_Profile
go build -o get_profile.exe .
./get_profile.exe -queue 1
```

### Get_Profile flags

| Flag | Default | Description |
|------|---------|-------------|
| `-queue` | (required) | Redis queue suffix, e.g. `1` → `redis-tokens-1` |
| `-redis` | `localhost:6379` | Redis address |
| `-workers` | `550` | Number of worker goroutines |
| `-max-cpm` | `20000` | Max requests per minute |
| `-emails` | `emails.txt` | Path to emails file |
| `-result` | `result_TIMESTAMP.txt` | Path to result file |
| `-id` | | Instance ID for logging |

## Môi trường

| Env | OS | Redis |
|-----|-----|-------|
| Test | Windows | Memurai |
| Production | Ubuntu Linux VPS | redis-server |
