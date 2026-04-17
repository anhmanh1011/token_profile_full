# Token Profile Full

Hệ thống tự động quản lý user Microsoft 365 và thu thập LinkedIn profile.

## Modules

### [Manage_User](Manage_User/) (Python) — API Service

Long-running HTTP service quản lý lifecycle users:
- **Startup cleanup**: Xóa toàn bộ `bot_` users từ lần chạy trước
- **Background producer**: Tự động tạo users + lấy access tokens, giữ ≥500 tokens trong queue
- **API endpoints**: Serve tokens cho Go app, nhận yêu cầu xóa users

### [Get_Profile](Get_Profile/) (Go) — Batch Job

High-performance LinkedIn profile fetcher:
- Lấy access tokens từ Python API (`GET /tokens/next?count=500`)
- 550 worker goroutines, rate limit 20K CPM
- Call Loki API (Delve Office) lấy LinkedIn profile
- Dead/exhausted token → batch delete users (`POST /users/delete`)
- Output: `result_TIMESTAMP.txt`

### [email-gen](email-gen/) (Rust) — CLI Utility

Standalone CLI sinh `emails.txt` cho Get_Profile bằng cross-product `domains.txt × usernames.txt`:
- Input: 1M domains + 200 usernames → 200M emails (~10 GB)
- Target: < 60 s trên 8 core NVMe, RAM < 500 MB
- Parallel (rayon) + mmap (memmap2) + dedicated writer thread (crossbeam channel)
- Hỗ trợ split, gzip, csv/json format, dedup domains, shuffle chunks

Chạy tay trước khi start Get_Profile:
```bash
cd email-gen && cargo build --release
./target/release/email-gen -d domains.txt -u usernames.txt -o ../Get_Profile/emails.txt
```

## Kiến trúc tổng quan

```
admin_token.json (input)
       │
       ▼
  [Manage_User API Service]  ← Python (localhost:5000)
       │
       ├── GET  /tokens/next      (access_token)
       ├── POST /users/delete     (batch delete)
       └── GET  /status           (monitoring)
       │
       ▼
  [Get_Profile]  ← Go (batch job)
       │
       ├── emails.txt (input: emails cần check)
       └── result_TIMESTAMP.txt (output: LinkedIn profiles)
```

## API Endpoints

| Method | Path | Response | Mô tả |
|--------|------|----------|-------|
| `GET` | `/tokens/next?count=N` | `{"tokens": [...], "count": N}` hoặc 202 | Lấy batch tokens (default 100, max 500) |
| `POST` | `/users/delete` | `{"deleted": N, "failed": N}` | Batch delete users |
| `GET` | `/status` | `{"queue_size", "total_created", ...}` | Monitoring |

## Yêu cầu

- Python 3.10+ (Manage_User)
- Go 1.21+ (Get_Profile)
- Rust 1.74+ / Cargo (email-gen) — [rustup.rs](https://rustup.rs) nếu chưa cài
- **Không cần Redis** — giao tiếp qua HTTP localhost

## Quick Start

```bash
# 1. Start Python API service
cd Manage_User
pip install -r requirements.txt
python app.py --port 5000

# 2. Đợi producer tạo tokens (check /status)
curl http://localhost:5000/status

# 3. Run Go app
cd Get_Profile
go build -o get_profile.exe .
./get_profile.exe --api http://localhost:5000 --emails emails.txt
```

### Get_Profile flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api` | `http://localhost:5000` | Python API service address |
| `--workers` | `550` | Number of worker goroutines |
| `--max-cpm` | `20000` | Max requests per minute |
| `--emails` | `emails.txt` | Path to emails file |
| `--result` | `result_TIMESTAMP.txt` | Path to result file |
| `--id` | | Instance ID for logging |

### Manage_User flags

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `0.0.0.0` | Bind host |
| `--port` | `5000` | Bind port |

## Môi trường

| Env | OS | Communication |
|-----|-----|---------------|
| Test | Windows | HTTP localhost:5000 |
| Production | Ubuntu Linux VPS | HTTP localhost:5000 |
