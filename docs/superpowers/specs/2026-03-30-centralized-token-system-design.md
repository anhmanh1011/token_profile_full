# Centralized Token Management System — Design Spec

**Date:** 2026-03-30
**Status:** Draft

---

## 1. Background

### Current State
- 3 VPS, moi VPS chay ca 2 project: VN-TAKK (Python, generate refresh token) va CHECK_CONNECTION (Go, fetch LinkedIn profile tu Delve API)
- 3 tenant, moi tenant chay tren IP/VPS rieng (bat buoc)
- Moi tenant: 40K accounts
- Rate limit: 20K CPM/tenant/VPS
- Refresh token song 24h, sau khi su dung can 24h cooldown moi dung lai

### Problem
- Moi VPS phai chay ca 2 project → phuc tap, kho quan ly
- Chia file emails thu cong cho 3 VPS → mat thoi gian
- Token management phan tan, khong co lifecycle tracking tap trung

### Goal
- Tap trung VN-TAKK tai 1 VPS trung tam
- Worker VPS chi chay CHECK_CONNECTION, lay token tu API
- Tu dong hoa viec chia va deploy emails

---

## 2. Architecture Overview

```
+------------------------------------------------+
|              VPS TRUNG TAM                     |
|                                                |
|  VN-TAKK (cron/6h) --> Redis <-- Token API     |
|                        127.0.0.1    Go :8080   |
|                                       |        |
|  split_deploy.sh                      | HTTP   |
|  (chia emails + rsync)                |        |
+---------------------------------------+--------+
                                        |
              +-------------------------+------------------------+
              |                         |                        |
        Worker 1                  Worker 2                 Worker 3
        Tenant A                  Tenant B                 Tenant C
        IP rieng                  IP rieng                 IP rieng
        20K CPM                   20K CPM                  20K CPM
        emails.txt (local)        emails.txt               emails.txt
```

**Communication:** HTTP (no auth, no TLS) giua worker va central VPS.

---

## 3. Components

### 3.1 Token API (Go) — New

HTTP server chay tren VPS trung tam, port 8080.

**Endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/tokens/{tenant_id}?count=100` | Pop tokens tu fresh queue, chuyen sang in_use, tra ve cho worker |
| `POST` | `/tokens/{tenant_id}/exhausted` | Body: `{"email":"user@domain.com"}` — chuyen token tu in_use sang used, ghi exhausted_at |
| `GET` | `/stats/{tenant_id}` | Tra ve so luong token: fresh, in_use, used |
| `GET` | `/stats` | Tong hop tat ca tenant |

**Response format:**

```json
// GET /tokens/{tenant_id}?count=100
{
  "tokens": [
    {
      "email": "user@domain.com",
      "password": "xxx",
      "refresh_token": "0.AVY...",
      "tenant_id": "abc-123"
    }
  ],
  "count": 100
}

// POST /tokens/{tenant_id}/exhausted
{ "ok": true }

// GET /stats/{tenant_id}
{ "fresh": 35000, "in_use": 200, "used": 4800 }
```

### 3.2 Redis Data Structure

Redis bind 127.0.0.1 (local only). Moi tenant co 3 queue:

```
tokens:{tenant_id}:fresh      # List — token cho dung
tokens:{tenant_id}:in_use     # List — worker dang dung
tokens:{tenant_id}:used       # List — da exhausted, cho 24h cooldown
```

Moi token la 1 JSON string:
```json
{
  "email": "user@domain.com",
  "password": "xxx",
  "refresh_token": "0.AVY...",
  "tenant_id": "abc-123",
  "created_at": 1711756800,
  "exhausted_at": 0
}
```

### 3.3 Token Lifecycle

```
VN-TAKK generate --> fresh --> Worker lay --> in_use --> exhausted --> used
                                                                       |
                       VN-TAKK (lan chay tiep theo):                   |
                       - Doc "used" queue                              |
                       - Loc account da qua 24h cooldown               |
                       - Login lai --> day vao fresh                   |
                       - Account chua qua 24h --> bo qua               |
```

VN-TAKK moi lan chay (6h/lan) se login:
1. Accounts trong "used" queue da qua 24h cooldown (exhausted_at + 24h < now)
2. Accounts chua co trong Redis (chua fresh, chua in_use, chua used)
3. Bo qua accounts "used" chua qua 24h

### 3.4 VN-TAKK (Python) — Modified

**Thay doi:**
- Output: ghi file `get_token_success.txt` → RPUSH vao Redis `tokens:{tenant}:fresh`
- Input: doc Redis de biet accounts nao can login (used qua 24h + accounts chua co trong Redis)
- Cron schedule: moi 6h (`0 */6 * * *`)

**Giu nguyen:**
- Class TeamOutLook trong main.py — logic OAuth2 login
- Thread pool worker
- Logic xu ly password change

**Them:**
- Redis client (redis-py)
- Logic doc Redis de xac dinh accounts can login

### 3.5 CHECK_CONNECTION (Go) — Modified

**Thay doi:**
- Bo: doc file tokens.txt
- Them: `token_client.go` — HTTP client goi Token API
- Them: `token_buffer.go` — buffer 2 lop (active + prefetch)
- Sua: `token/manager.go` — thay TokenQueue bang buffer moi

**Token Buffer 2 lop:**
```
Active Pool (100 tokens)     Prefetch Buffer (100 tokens)
     |                              |
     | Khi active < 20 tokens       |
     | ---------> swap <----------  |
     |                              |
     |                    Background goroutine
     |                    fetch 100 tokens moi tu API
     |
   1000 Workers (goroutine)
```

Logic:
1. Khoi dong: fetch 200 tokens (100 active + 100 prefetch)
2. Workers lay token tu Active Pool
3. Active Pool con < 20 tokens → swap Prefetch vao lam Active
4. Background goroutine fetch 100 tokens moi cho Prefetch
5. Workers khong bao gio cho — luon co token san

Khi worker detect token exhausted (5x 500 lien tiep):
- Goi `POST /tokens/{tenant}/exhausted`
- Lay token moi tu buffer
- Tiep tuc xu ly, khong ngat quang

**Config flags moi:**
```
-api        string   Central API URL (default: http://central:8080)
-tenant     string   Tenant ID
-batch      int      So token moi lan fetch (default: 100)
```

**Giu nguyen:**
- Logic goi Delve API (fetch_profile.go)
- Email reader, result writer
- Worker pool, rate limiter
- Config flags: -emails, -result, -max-cpm, -workers

### 3.6 split_deploy.sh (Bash) — New

Script tren VPS trung tam:
1. Nhan file emails.txt (1 file duy nhat)
2. Chia deu thanh 3 phan
3. rsync moi phan den worker VPS tuong ung

```bash
# Usage: ./split_deploy.sh emails.txt
# Chia file → rsync den 3 VPS
```

Worker giu nguyen doc file emails.txt local — khong thay doi logic email.

---

## 4. Deployment

### VPS Trung Tam
- Redis (bind 127.0.0.1)
- Token API (Go binary, port 8080)
- VN-TAKK (Python, cron 6h)
- split_deploy.sh
- File emails.txt

### Moi Worker VPS
- CHECK_CONNECTION (Go binary, sua doi)
- File emails.txt (nhan tu rsync)
- Config: CENTRAL_API url + TENANT_ID

---

## 5. Data Flow — Full Cycle

```
1. Upload emails.txt len VPS trung tam
2. Chay split_deploy.sh → chia + rsync den 3 workers
3. VN-TAKK chay (cron hoac manual):
   - Doc Redis → xac dinh accounts can login
   - Login OAuth2 → lay refresh_token
   - RPUSH vao tokens:{tenant}:fresh
4. Workers khoi dong:
   - Fetch token batch tu Token API
   - Doc emails.txt local
   - Goi Delve API → lay LinkedIn profile
   - Token exhausted → report ve API → lay token moi
5. Ket qua ghi vao result.txt tren moi worker VPS
```

---

## 6. Capacity Planning

| Metric | Value |
|--------|-------|
| Total accounts | 120K (40K x 3 tenant) |
| Emails/day | 30-50M |
| Emails/token | 3-5K |
| Tokens needed/day | ~10-15K |
| Rate limit | 20K CPM x 3 VPS = 60K CPM total |
| Token API load | ~150 requests/day/worker (token fetch) |
| Redis memory | Minimal (~50MB for 120K token JSON strings) |

---

## 7. Failure Scenarios

| Scenario | Impact | Recovery |
|----------|--------|----------|
| Token API down | Workers dung khi het buffer (~200 tokens) | Workers retry moi 5s, tu phuc hoi khi API len lai |
| Redis down | Token API va VN-TAKK khong hoat dong | Restart Redis, VN-TAKK chay lai de repopulate |
| VN-TAKK fail | Khong co token moi, workers dung token con lai trong fresh queue | Chay lai VN-TAKK manual |
| Worker crash | Emails chua xu ly xong | Restart worker, lay tiep emails tu file (co the trung 1 so emails — chap nhan duoc) |
| Central VPS down | Tat ca workers mat token source | Workers chay het buffer roi dung. Khoi phuc VPS → chay lai |
