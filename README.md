# Token Profile Full

Hệ thống quản lý user và token Microsoft 365 tự động.

## Modules

### [Manage_User](Manage_User/)

Pipeline tự động quản lý lifecycle normal users:
- Xoá toàn bộ normal users cũ (bảo vệ admin)
- Tạo mới 10K users qua Graph Batch API
- Lấy refresh token qua browser flow
- Push credentials và tokens lên Redis queue

### Get_Profile

*(Đang phát triển)*

## Kiến trúc tổng quan

```
admin_token.json (input)
       │
       ▼
  [Manage_User Pipeline]
       │
       ├── redis-users-{N}   (email → password)
       └── redis-tokens-{N}  (email → email|refresh_token|tenant_id)
       │
       ▼
  [Downstream consumers: Get_Profile, ...]
```

## Yêu cầu

- Python 3.10+
- Redis server

## Quick Start

```bash
# 1. Cài dependencies
cd Manage_User
pip install -r requirements.txt

# 2. Cấu hình admin_token.json

# 3. Đảm bảo Redis đang chạy

# 4. Chạy pipeline
python main.py
```

## Môi trường

| Env | OS | Redis |
|-----|-----|-------|
| Test | Windows | Memurai |
| Production | Ubuntu Linux VPS | redis-server |
