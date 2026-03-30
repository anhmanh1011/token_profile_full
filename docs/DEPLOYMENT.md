# Huong dan trien khai

## Yeu cau

- VPS trung tam: Ubuntu/Debian, Redis, Python 3.8+, Go 1.21+
- Worker VPS: Ubuntu/Debian, Go 1.21+
- SSH key hoac password giua cac VPS

## Buoc 0: Dien config.yaml

```yaml
tenants:
  - id: "tenant-id-tu-microsoft"
    name: "tenant_a"
    accounts_file: "/opt/vn-takk/accounts_tenant_a.txt"
    worker:
      host: "worker-vps-ip"
      user: "root"
      dir: "/opt/check-connection"
      max_cpm: 20000
      workers: 500
```

## Buoc 1: Deploy VPS trung tam

```bash
# Copy files len VPS
scp -r token-api/ root@central-ip:/opt/token-api/
scp -r VN-TAKK/ root@central-ip:/opt/vn-takk/
scp -r scripts/ root@central-ip:/opt/scripts/
scp config.yaml root@central-ip:/opt/config.yaml

# SSH vao va chay deploy
ssh root@central-ip
bash /opt/scripts/deploy_central.sh

# Kiem tra
curl http://localhost:8080/stats
```

## Buoc 2: Tao file accounts

Tach account.txt thanh 3 file theo tenant, moi file chua email:password:

```
/opt/vn-takk/accounts_tenant_a.txt
/opt/vn-takk/accounts_tenant_b.txt
/opt/vn-takk/accounts_tenant_c.txt
```

## Buoc 3: Chay VN-TAKK lan dau

```bash
# Chay truc tiep hoac dung cron script
bash /opt/scripts/cron_vn_takk.sh

# Kiem tra token da duoc generate
curl http://localhost:8080/stats
# Ket qua: {"fresh": 38000, "in_use": 0, "used": 0} cho moi tenant
```

## Buoc 4: Deploy worker VPS

```bash
# Copy CHECK_CONNECTION len workers
for host in worker1-ip worker2-ip worker3-ip; do
    scp -r CHECK_CONNECTION/ root@${host}:/opt/check-connection/
done

# Deploy systemd service
bash /opt/scripts/deploy_worker.sh <central-vps-ip>
```

## Buoc 5: Upload emails

```bash
# Upload file emails.txt.gz len tung worker
scp emails_1.txt.gz root@worker1-ip:/opt/check-connection/
ssh root@worker1-ip "cd /opt/check-connection && gunzip -f emails_1.txt.gz && mv emails_1.txt emails.txt"

# Lap lai cho worker 2, 3
```

## Buoc 6: Start workers

```bash
ssh root@worker1-ip "systemctl start check-connection"
ssh root@worker2-ip "systemctl start check-connection"
ssh root@worker3-ip "systemctl start check-connection"
```

## Monitoring

```bash
# Token stats
curl http://central-ip:8080/stats

# Worker logs
ssh root@worker-ip "journalctl -u check-connection -f"

# Ket qua
ssh root@worker-ip "wc -l /opt/check-connection/result_*.txt"
```

## Van hanh hang ngay

- Cron tu dong chay VN-TAKK moi 6h de bo sung token
- Workers chay lien tuc, tu lay token tu API
- Khi muon doi file emails: upload file moi + restart worker

## Troubleshooting

| Van de | Giai phap |
|--------|-----------|
| Token API khong tra ve token | Kiem tra Redis: `redis-cli LLEN tokens:{tenant}:fresh` |
| Worker bao "no tokens" | Chay lai VN-TAKK: `bash /opt/scripts/cron_vn_takk.sh` |
| Build loi tren VPS | Kiem tra Go version: `go version` (can >= 1.21) |
| Worker dung dot ngot | Kiem tra log: `journalctl -u check-connection --since "1 hour ago"` |

## Hien trang trien khai

- [x] Token API (Go) — hoan thanh + code review fixes
- [x] VN-TAKK Redis integration (Python) — hoan thanh + code review fixes
- [x] CHECK_CONNECTION API mode (Go) — hoan thanh + code review fixes
- [x] Deploy scripts — hoan thanh
- [x] Config tap trung (config.yaml) — hoan thanh
- [ ] Trien khai len VPS — cho thong tin VPS tu user

## Code Review Fixes (2026-03-30)

### Token API
- Redis PopFreshTokens: LPOP+RPUSH → LMOVE (atomic, 1 round-trip/token)
- Redis MarkExhausted: Go loop → Lua script (atomic, tranh duplicate)
- Error handling: khong leak err.Error() ra client, log internal
- Server timeouts: Read 10s, Write 30s, Idle 120s
- Graceful shutdown: server.Close() → server.Shutdown(10s context)
- Count parameter: gioi han toi da 500

### CHECK_CONNECTION
- Race fix: exhaustedAt time.Time → int64 atomic
- Race fix: exhaustedCount decrement dung CAS guard
- Buffer: double-close panic → sync.Once
- Buffer: time.Sleep → select interruptible by stopCh
- Buffer: Start() hold activeMu khi set active
- api_client: fmt.Sprintf → json.Marshal (tranh JSON injection)
- api_client: response body limited 1MB
- worker: ReportExhausted truyen context cho cancellation
- worker: ShutdownWithReason chi CloseQueue khi file mode
- main: repeatString → strings.Repeat

### VN-TAKK
- Thread-safe: them _write_lock + safe_write() cho file writes
- Resource: tat ca open().write() → context manager (with)
- Exception: bare except → except (IndexError, KeyError)
- Crash fix: login_common kiem tra header truoc khi regex
- Imports: consolidate PEP 8, xoa duplicates va unused
- Type hints: them cho public methods
- Deps: pin tat ca versions trong requirements.txt
