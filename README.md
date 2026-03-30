# Centralized Token Management System

He thong tap trung quan ly token va fetch LinkedIn profile tu Microsoft Delve API.

## Kien truc

```
VPS Trung Tam
├── VN-TAKK (Python, cron/6h) — login accounts, generate refresh token
├── Redis (127.0.0.1) — luu token theo tenant lifecycle
├── Token API (Go, :8080) — serve token cho workers
└── config.yaml — cau hinh tap trung

Worker VPS 1 (Tenant A, IP rieng, 20K CPM)
Worker VPS 2 (Tenant B, IP rieng, 20K CPM)
Worker VPS 3 (Tenant C, IP rieng, 20K CPM)
└── CHECK_CONNECTION (Go) — fetch LinkedIn profile tu Delve API
```

## Cau truc thu muc

```
token-api/          # Token API server (Go)
├── main.go         # HTTP server entrypoint
├── handler.go      # API endpoints
├── redis.go        # Redis CRUD operations
└── models.go       # Data structures

VN-TAKK/            # Token generator (Python)
├── main.py         # OAuth2 login, generate refresh token
├── redis_client.py # Redis helper
└── requirements.txt

CHECK_CONNECTION/    # LinkedIn profile fetcher (Go)
├── main.go         # Entrypoint (ho tro -api mode)
├── token/
│   ├── manager.go     # Token refresh, OAuth2
│   ├── api_client.go  # HTTP client goi Token API
│   └── buffer.go      # Buffer 2 lop (active + prefetch)
├── worker/
│   └── pool.go        # Worker pool (ho tro API buffer mode)
├── api/client.go      # Delve API client
├── config/config.go   # Configuration
├── models/profile.go  # Data models
├── reader/file.go     # Email file reader
└── writer/result.go   # Result writer

scripts/             # Deploy & operations
├── parse_config.sh    # Doc config.yaml
├── split_deploy.sh    # Chia emails + rsync
├── cron_vn_takk.sh    # Cron wrapper multi-tenant
├── deploy_central.sh  # Deploy VPS trung tam
└── deploy_worker.sh   # Deploy worker VPS

config.yaml          # CAU HINH TAP TRUNG - dien VPS IPs, tenant IDs
```

## Token Lifecycle

```
VN-TAKK login account ──> fresh ──> Worker lay ──> in_use ──> exhausted ──> used
                                                                             │
    VN-TAKK (lan chay tiep):                                                 │
    - Account trong "used" da qua 24h cooldown ──> login lai ──> fresh       │
    - Account chua co trong Redis ──> login ──> fresh                        │
    - Account "used" chua qua 24h ──> bo qua                                │
```

## API Endpoints (Token API)

| Method | Path | Mo ta |
|--------|------|-------|
| GET | `/tokens/{tenant_id}?count=100` | Lay batch token tu fresh queue |
| POST | `/tokens/{tenant_id}/exhausted` | Report token da het quota |
| GET | `/stats/{tenant_id}` | Stats 1 tenant |
| GET | `/stats` | Stats tat ca tenants |

## Cau hinh

Tat ca cau hinh nam trong `config.yaml`. Dien 1 lan, tat ca scripts tu doc tu do.

## Trien khai

Xem chi tiet: [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)

## API Luu y

- `count` toi da 500 moi request (server tu clamp)
- Redis operations su dung `LMOVE` (atomic) va Lua script de tranh race condition
- Token API co server timeouts: Read 10s, Write 30s, Idle 120s
- Graceful shutdown voi 10s drain timeout

## Thong so

| Metric | Value |
|--------|-------|
| Accounts | 120K (40K x 3 tenant) |
| Emails/ngay | 30-50M |
| Rate limit | 20K CPM x 3 VPS = 60K CPM |
| Token/ngay | ~10-15K |
| Refresh token TTL | 24h |
| Token cooldown | 24h sau khi exhausted |

## Code Quality

### Token API (Go)
- Redis atomic operations (LMOVE, Lua script cho MarkExhausted)
- Proper error handling (khong leak internal errors ra client)
- HTTP server timeouts + graceful shutdown
- Count parameter gioi han toi da 500

### CHECK_CONNECTION (Go)
- Race-free token management (atomic exhaustedAt, CAS guards)
- Buffer 2 lop voi interruptible sleep, double-close protection
- JSON marshal thay vi fmt.Sprintf (tranh injection)
- Context propagation cho goroutine cancellation

### VN-TAKK (Python)
- Thread-safe file writes voi threading.Lock
- File handles dong dung cach (context manager)
- Specific exception handling (khong dung bare except)
- Type hints tren cac public methods
- PEP 8 imports, pinned dependencies
