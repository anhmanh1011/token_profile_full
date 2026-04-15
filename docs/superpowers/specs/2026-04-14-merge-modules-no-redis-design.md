# Design: Gộp 2 module, bỏ Redis — Python API Service + Go App

**Date:** 2026-04-14
**Status:** Draft
**Scope:** Refactor kiến trúc từ Redis-based pipeline sang HTTP API localhost

## Context

Hiện tại 2 module chạy qua Redis broker:
- Manage_User (Python): tạo 10K users → lấy tokens → push Redis
- Get_Profile (Go): đọc Redis → 550 workers → Loki API

**Vấn đề:**
- Redis là dependency thừa khi cả 2 module chạy cùng 1 VPS
- Pipeline sequential: phải đợi ~40 phút (tạo 10K users + lấy tokens) trước khi Go bắt đầu
- Tạo 10K tokens upfront có thể lãng phí nếu không dùng hết
- Tốn thêm RAM cho Redis (~100MB)

**Giải pháp:** Python chạy như HTTP API service trên localhost, Go gọi API trực tiếp. Streaming pipeline — tokens sẵn sàng trong ~25s thay vì ~40 phút.

## Kiến trúc

### Tổng quan

```
┌─────────────────────────────────────────────────┐
│                   VPS (1 admin)                  │
│                                                  │
│  Python API Service (luôn chạy, localhost:5000)  │
│  ┌─────────────────────────────────────────┐     │
│  │  Background Producer Thread             │     │
│  │  - Giữ >= 500 tokens trên queue         │     │
│  │  - Tạo batch 100 users → lấy tokens     │     │
│  │  - Queue < 500 → tạo thêm              │     │
│  │  - Queue >= 500 → sleep 30s            │     │
│  │                                         │     │
│  │  HTTP Endpoints:                        │     │
│  │  GET  /tokens/next    (lấy 1 token)     │     │
│  │  POST /users/delete   (batch delete)    │     │
│  │  GET  /status         (monitoring)      │     │
│  │                                         │     │
│  │  Startup cleanup:                       │     │
│  │  List users prefix "bot_" → delete all  │     │
│  └───────────────┬─────────────────────────┘     │
│                  │ HTTP localhost:5000            │
│  ┌───────────────▼─────────────────────────┐     │
│  │  Go App (chạy khi cần, exit khi xong)   │     │
│  │  ./get_profile --api http://localhost:5000│    │
│  │                                         │     │
│  │  1. GET /tokens/next khi cần token mới  │     │
│  │  2. 550 workers → Loki API → result.txt │     │
│  │  3. Token chết → batch 20 → DELETE      │     │
│  │  4. Hết emails → flush deletes → exit   │     │
│  └─────────────────────────────────────────┘     │
│                                                  │
│  emails.txt (input) → result.txt (output)        │
└─────────────────────────────────────────────────┘
```

### 2 app chạy độc lập

- **Python API Service**: long-running, luôn chạy, tự duy trì pool tokens
- **Go App**: batch job, chạy khi có emails cần check, xong thì exit
- Không phụ thuộc nhau để start/stop
- Go có thể chạy nhiều lần trong ngày

## Python API Service

### Endpoints

| Method | Path | Request | Response | Mô tả |
|--------|------|---------|----------|-------|
| `GET` | `/tokens/next` | — | `{"email", "refresh_token", "tenant_id"}` hoặc 202 `{"waiting": true}` | Lấy 1 token từ queue |
| `POST` | `/users/delete` | `{"emails": [...]}` | `{"deleted": N, "failed": N}` | Batch delete (max 20) |
| `GET` | `/status` | — | `{"queue_size", "total_created", "total_deleted"}` | Monitoring |

### Token Queue

- `queue.Queue(maxsize=1000)` — bounded, thread-safe, in-memory
- Producer thread đẩy vào, HTTP endpoint lấy ra
- Không cần Redis, không cần SQLite, không cần persistence

### Background Producer

```
Producer Thread:
  while running:
    if queue.qsize() < 500:
      batch_size = min(500 - queue.qsize(), 100)
      users = create_users(batch_size)       # Graph Batch API
      tokens = get_tokens(users)             # 30 workers browser flow
      for token in tokens:
        queue.put(token)
    else:
      sleep(30)
```

- Batch 100 users/lần → tạo ~15s, lấy token ~10s → ~25s/batch
- Queue refill liên tục, không đợi hết mới tạo

### User Naming Pattern

- **Pattern:** `bot_{random6}@domain` — ví dụ `bot_a3kx9m@domain.com`
- Prefix `bot_` để nhận diện users do app tạo
- Startup cleanup: Graph API list users `startsWith('bot_')` → batch delete tất cả
- Không cần database, không cần Redis — prefix là source of truth

### Startup Flow

1. Load admin config (`admin_token.json`)
2. Cleanup: Graph API filter users prefix `bot_` → batch delete
3. Start Flask server trên `localhost:5000`
4. Start producer thread → fill queue đến 500
5. Sẵn sàng nhận requests

### Framework

Flask — lightweight, đủ cho internal localhost API. Không cần FastAPI/async vì producer chạy trên thread riêng.

## Go App — Thay đổi

### Bỏ

- Redis dependency (`go-redis` package)
- `token/redis.go` (RedisLoader, cleanup goroutine)
- Flags: `-redis`, `-queue`

### Thêm

- Flag: `--api` (default `http://localhost:5000`)
- `token/api_loader.go` — gọi `GET /tokens/next`
- `token/api_deleter.go` — gom batch 20 emails → `POST /users/delete`

### Token Flow mới

```
Worker cần token (pool hết hoặc token chết)
  → TokenManager.AcquireToken()
  → Pool hết alive tokens → gọi GET /tokens/next
  → Nhận {email, refresh_token, tenant_id}
  → Tạo TokenInfo, refresh access_token
  → Giao cho worker

Token chết
  → MarkDeadAndRelease()
  → Email → deleteChan (buffered channel)
  → Background goroutine: gom 20 emails hoặc timeout 5s
  → POST /users/delete {"emails": [...]}
```

### TokenManager thay đổi

- Bỏ `LoadFromSlice()` (load tất cả 1 lần)
- Thêm lazy loading: pool bắt đầu rỗng, gọi API khi cần
- Pre-fetch: khi pool < 50 alive tokens → background goroutine gọi API lấy thêm

### CLI Flags

| Flag | Default | Mô tả |
|------|---------|-------|
| `--api` | `http://localhost:5000` | Python API address |
| `--workers` | `550` | Số workers |
| `--emails` | `emails.txt` | File emails input |
| `--max-cpm` | `20000` | Rate limit |

### Giữ nguyên

- Worker pool, rate limiter (550 workers, 20K CPM)
- Loki API client (connection pooling, gzip)
- Result writer (async buffered)
- Reader (streaming email file)

## Shutdown & Error Handling

### Shutdown Scenarios

| Scenario | Python | Go |
|----------|--------|----|
| Go xong emails | Vẫn chạy | Flush delete buffer → exit |
| Go Ctrl+C | Vẫn chạy | Flush delete buffer → exit |
| Python Ctrl+C | Dừng producer, đợi pending | Retry 3 lần → exit |
| Python crash + restart | Cleanup prefix `bot_` → producer lại | Retry, tự recover |

### Go → Python Error Handling

| Response | Hành vi Go |
|----------|-----------|
| 200 (GET /tokens/next) | Dùng token |
| 202 (queue rỗng) | Sleep 2s, retry |
| Connection refused | Retry 3 lần, interval 5s |
| Retry hết | Log error, worker đợi |
| POST /users/delete fail | Log warning, giữ emails retry sau |

## So sánh trước/sau

| Metric | Trước (Redis) | Sau (API) |
|--------|--------------|-----------|
| Thời gian chờ trước khi Go chạy | ~40 phút | ~25 giây |
| Memory | Python + Redis + Go | Python + Go |
| Dependencies | Python, Go, Redis | Python, Go |
| Recovery sau crash | Cần Redis data | Tự cleanup prefix |
| Tokens lãng phí | 10K upfront | On-demand |
| Complexity | 3 processes | 2 processes |
