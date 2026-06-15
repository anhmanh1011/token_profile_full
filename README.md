# Token Profile Full

He thong gom 3 app doc lap:
- `Manage_User` (Python): HTTP service tao bot users, lay refresh tokens, va xoa user het token.
- `Get_Profile` (Go): batch job fetch LinkedIn profiles, consume token tu Manage_User.
- `email-gen` (Rust): CLI sinh file email dau vao.

## Global Config

`Manage_User` va `Get_Profile` chi doc `admin_token_config_global.json` o root project. Khong con doc `Manage_User/admin_token.json`.

Moi entry la mot pool doc lap:

```json
[
  {
    "refresh_token": "...",
    "tenant_id": "...",
    "username": "admin1",
    "domain": "example.com",
    "proxy": "host:port:user:pass",
    "email_file": "email1.txt",
    "bot_prefix": "bot_p01_"
  }
]
```

`pool_id` mac dinh duoc suy ra tu `bot_prefix` bang cach bo `_` cuoi, vi du `bot_p01_` -> `bot_p01`. Co the them field `pool_id` neu muon ten ro rang hon.

## Architecture

```text
admin_token_config_global.json
       |
       v
Manage_User (localhost:5000)
  one pool per config entry
  AdminTokenManager + StartupCleaner + TokenProducer per pool
       |
       | GET  /pools/<pool_id>/tokens/next?count=N
       | POST /pools/<pool_id>/users/delete
       | GET  /pools/<pool_id>/proxy
       v
Get_Profile --pool <pool_id>
  Loki + token-exchange use that pool proxy
  localhost Manage_User API calls stay direct
  checkpoint defaults to <email_file>.<pool_id>.ckpt
```

## Run

```bash
# Start API service from repo root or Manage_User
cd Manage_User
pip install -r requirements.txt
python app.py --port 5000 --config ../admin_token_config_global.json

# Run one Get_Profile instance
cd ../Get_Profile
go build -o get_profile.exe .
./get_profile.exe --api http://localhost:5000 --pool bot_p01

# Run more instances in other shells
./get_profile.exe --api http://localhost:5000 --pool bot_p02
./get_profile.exe --api http://localhost:5000 --pool bot_p03
```

## Manage_User API

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/pools` | List configured pools and safe status fields. |
| `GET` | `/status` | Aggregate status across all pools. |
| `GET` | `/pools/<pool_id>/status` | Status for one pool. |
| `GET` | `/pools/<pool_id>/tokens/next?count=N` | Pop up to 500 refresh tokens from that pool. |
| `POST` | `/pools/<pool_id>/users/delete` | Delete up to 20 users; emails must match that pool domain and `bot_prefix`. |
| `GET` | `/pools/<pool_id>/proxy` | Return that pool SOCKS5 URL. |

Legacy endpoints `/tokens/next`, `/users/delete`, and `/proxy` still work only when `pool_id` is supplied or exactly one pool is configured.

## Get_Profile Flags

| Flag | Default | Notes |
| --- | --- | --- |
| `--config` | auto-detect root/parent `admin_token_config_global.json` | Global config path. |
| `--pool` | required when config has multiple pools | Pool id, usually `bot_prefix` without trailing `_`. |
| `--instance` | alias for `--pool` | Kept for command readability. |
| `--api` | `http://localhost:5000` | Manage_User API address. |
| `--emails` | selected `email_file` | Override email input file. |
| `--checkpoint` | `<emails>.<pool_id>.ckpt` | Bitmap checkpoint; default is pool-isolated. |
| `--result` | `result_<pool_id>_<timestamp>.txt` | Output file. |
| `--workers` | `400` or config `workers` | Worker goroutines. |
| `--max-cpm` | `20000` or config `max_cpm` | Rate limit. |
| `--proxy` | fetched from `/pools/<pool_id>/proxy` | Manual SOCKS5 override. |
| `--id` | pool id | Log prefix. |

## Verification

```bash
cd Manage_User
py -m unittest discover -s . -p "test_*.py"
py -m py_compile app.py admin_token_manager.py pool_config.py cleanup.py producer.py creator.py deleter.py token_getter.py proxy_config.py

cd ../Get_Profile
go test ./...
go vet ./...
go build .
```
