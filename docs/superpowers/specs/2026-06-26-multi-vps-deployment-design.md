# Thiết kế: Triển khai đa-VPS (3 app VPS + 1 ops node)

- **Ngày:** 2026-06-26
- **Trạng thái:** Đã duyệt (brainstorming)
- **Phạm vi:** Tối ưu cách deploy ứng dụng (Manage_User + Get_Profile + email-gen) lên 3 VPS giống
  hệt nhau, dễ cấu hình input, dễ quản lý, có quan sát (observability) tập trung.

## 1. Mục tiêu & bối cảnh

Hiện tại 3 app chạy thủ công qua CLI (`python app.py …`, `./get_profile …`), có tài liệu
`DEPLOY_DUAL_TENANT.md` cho mô hình 2 tenant/1 VPS. Cần nâng lên:

- **Deploy dễ** lên 3 VPS giống hệt nhau, bootstrap 1 lệnh, cập nhật 1 lệnh.
- **Cấu hình input dễ:** một nguồn sự thật duy nhất cho toàn bộ tenant.
- **Quản lý dễ:** start/stop/restart/status/logs tập trung.
- **Quan sát:** log aggregation + metrics + dashboard cho cả 6 tenant.

### Topology đã chốt
- **3 app VPS giống hệt nhau.** Mỗi VPS chạy nhiều tenant (mô hình dual-tenant nhân rộng).
  Mỗi tenant = 1 callout IP riêng + 1 admin token riêng + 1 file emails riêng + 1 cặp process
  `Manage_User` + `Get_Profile`. Tối đa ~6 tenant trên 3 VPS.
- **1 ops node always-on** (khuyến nghị VPS thứ 4 nhỏ, KHÔNG chạy tenant — không cần giống 3 app
  VPS). Chứa: Ansible repo, `tenants.yml`, vault secrets, và stack giám sát
  (Grafana + Grafana Loki + Prometheus) qua docker-compose. Lý do tách: log aggregation chỉ thu
  thập khi host giám sát còn sống — không đặt trên laptop. Có thể chạy `ansible-playbook` từ laptop
  trỏ vào ops node.

> Lưu ý thuật ngữ: "Loki" của ứng dụng (`eur.loki.delve.office.com`, endpoint Microsoft/LinkedIn)
> KHÁC hoàn toàn **Grafana Loki** (hệ log aggregation). Trong spec, hệ log luôn ghi rõ "Grafana Loki".

## 2. Kiến trúc

```
                ┌─────────────────────── ops node (always-on) ───────────────────────┐
   laptop ─────▶│  Ansible (tenants.yml, vault)   docker-compose:                     │
   (ansible run)│                                  Grafana + Grafana Loki + Prometheus │
                └───────▲────────────────────────────────▲───────────────────────────┘
                        │ logs (Alloy→Loki)               │ metrics (Prometheus scrape)
        ┌───────────────┴───────┐   ┌───────────────┐   ┌─┴─────────────┐
        │ app VPS1              │   │ app VPS2       │   │ app VPS3       │  (giống hệt nhau)
        │  manageuser@t1,@t2    │   │  @t3,@t4       │   │  @t5,@t6       │
        │  getprofile@t1,@t2    │   │  ...           │   │  ...           │
        │  Grafana Alloy agent  │   │  ...           │   │  ...           │
        └───────────────────────┘   └───────────────┘   └────────────────┘
```

### Build strategy
Build **trên từng VPS** qua Ansible (cài Go + Rust toolchain + Python venv). VPS giống hệt nhau nên
reproducible; Ansible chỉ rebuild khi source đổi. Tránh phụ thuộc kiến trúc/OS của control node
(laptop có thể là macOS).

## 3. Nguồn cấu hình duy nhất — `tenants.yml`

Đặt tại `deploy/group_vars/all/tenants.yml`. Thêm/sửa tenant = sửa file này + chạy playbook.

```yaml
app_vps:
  vps1: { ansible_host: <ip-or-dns> }
  vps2: { ansible_host: <ip-or-dns> }
  vps3: { ansible_host: <ip-or-dns> }

tenants:
  - id: t1
    vps: vps1
    local_ip: 203.0.113.10            # callout IP (IPv4/IPv6)
    manageuser_port: 5000
    admin:
      tenant_id: "..."
      username: admin@tenant1.example
      domain: tenant1.example
      refresh_token: "{{ vault_t1_refresh }}"   # lấy từ vault.yml
    license_sku: a1-students          # alias → STANDARDWOFFPACK_STUDENT (mặc định); hoặc GUID; hoặc "auto"
    usage_location: US
    emailgen:
      domains: [tenant1.example]      # danh sách nhỏ, commit vào repo
      usernames_file: usernames/common.txt
    workers: 400
    max_cpm: 20000
  # - id: t2 … (tenant thứ 2 của vps1, IP thứ 2) … tối đa 6 tenant
```

### Secrets
- `refresh_token` + `tenant_id` đặt trong `deploy/group_vars/all/vault.yml`, mã hóa
  **ansible-vault** (commit an toàn dạng đã mã hóa).
- Render trên VPS thành `admin_token.<tenant>.json`, mode `0600`, owner `tokentool` (non-root).
- Áp dụng **security-review skill** khi hiện thực phần xử lý secret.

## 4. Bố cục trên VPS & systemd templates

```
/opt/token-tool/                            code + binary đã build (get_profile, email-gen, venv)
/etc/token-tool/<tenant>.env                tham số per-tenant (port, ip, license, đường dẫn)
/etc/token-tool/admin_token.<tenant>.json   secret, 0600
/var/lib/token-tool/<tenant>/               emails.txt, *.ckpt, result.txt, metrics textfile
```

Hai **template unit** systemd, instantiate theo tenant qua `@`:

- `manageuser@<tenant>.service`
  `ExecStart=python app.py --port <port> --config <admin_file> --local-ip <ip> --license-sku <sku> --usage-location <loc>`
- `getprofile@<tenant>.service`
  `ExecStart=get_profile --api http://localhost:<port> --local-ip <ip> --emails … --checkpoint … --result … --metrics-file …`
  - `After=`/`Wants=` unit `manageuser@<tenant>` tương ứng.
  - `Restart=on-failure` + start-limit. Bitmap checkpoint (ở `/var/lib`) cho resume an toàn qua
    restart/redeploy.
- Tham số per-tenant nạp qua `EnvironmentFile=/etc/token-tool/<tenant>.env` (Ansible render); unit
  giữ generic, không hardcode.

## 5. Quan sát (observability)

- **Logs:** systemd → journald → **Grafana Alloy** (agent trên app VPS) → **Grafana Loki** (ops node).
  Label `{vps, tenant, app}`. Không đổi code app.
- **Metrics:**
  - *Manage_User:* đã có `/status` (JSON) → Prometheus **json_exporter** scrape (chỉ cấu hình, không đổi code).
  - *Get_Profile:* không có HTTP server → thêm `--metrics-file` ghi định kỳ theo định dạng
    **node_exporter textfile** (processed, success, http_403, dead, cpm, progress %) từ counter sẵn có
    trong worker pool → node_exporter textfile collector. (Thay đổi Go nhỏ.)
- **Dashboard:** 1 Grafana dashboard provisioned, mỗi tenant 1 hàng: token-queue depth, users
  created/deleted, profiles/sec, success/403/dead, progress %.

## 6. Thay đổi code (nhỏ, trong phạm vi)

1. **`Manage_User/creator.py`** — thay logic "first available seat" bằng thứ tự ưu tiên:
   (a) SKU chỉ định rõ → (b) **Office 365 A1 for students** (khớp `skuPartNumber == STANDARDWOFFPACK_STUDENT`,
   chỉ khi còn seat `enabled − consumed > 0`) → (c) fallback first-available + `logger.warning`.
   Log skuId đã chọn. Thêm alias map `{"a1-students": "STANDARDWOFFPACK_STUDENT"}`. Biến
   `usage_location` thành tham số (bỏ hardcode `"US"`).
2. **`Manage_User/producer.py`** — forward `license_sku` + `usage_location` vào `BulkUserCreator`.
3. **`Manage_User/app.py`** — thêm flag `--license-sku` / `--usage-location` (mặc định a1-students),
   truyền vào `TokenProducer`.
4. **`Get_Profile` (`main.go` + `worker`/`progress`)** — thêm flag `--metrics-file` + writer textfile định kỳ.

Theo CLAUDE.md: áp dụng python-patterns / python-testing (Python), bộ Go skills (Go), và
security-review (secret/token).

## 7. Quản lý — Makefile bọc Ansible

```
make provision            # site.yml — bootstrap VPS mới (deps, users, build, units)
make deploy               # deploy.yml — sync code, rebuild phần đổi, rolling restart (serial:1)
make add-tenant           # sửa tenants.yml rồi provision (idempotent)
make status               # tổng hợp: systemctl is-active + curl /status mọi tenant
make restart TENANT=t1    # restart cặp unit của tenant
make logs TENANT=t1       # journalctl -f (hoặc dùng Grafana)
make verify               # units active + /status ok + source-IP đúng (ss) + Grafana targets up
```

### Bố cục repo mới — `deploy/`
```
deploy/
  ansible.cfg
  inventory/                       # 3 app VPS + ops node
  playbooks/{site,deploy,control}.yml
  roles/
    common/                        # os deps, user tokentool, dirs, toolchains
    app/                           # sync code, build Go + Rust, tạo venv
    tenant/                        # env file, admin token, systemd units, email-gen, enable/start
    observability/                 # Grafana Alloy agent trên app VPS
  monitoring/                      # docker-compose: grafana, loki, prometheus + provisioning + json_exporter
  group_vars/all/tenants.yml
  group_vars/all/vault.yml         # ansible-vault encrypted
  Makefile
```

## 8. Rollout, an toàn, kiểm thử

- **Rolling deploy** `serial: 1` — cập nhật từng VPS một, không downtime toàn cục.
- **An toàn:** user `tokentool` non-root; secret `0600`; giữ binding source-IP per-tenant; systemd
  `Restart=on-failure` + start-limit.
- **Kiểm thử:**
  - Ansible `--check` / `--diff` dry-run; staging trên 1 VPS trước.
  - Python: test table-driven cho logic chọn license (`creator.py`).
  - Go: test cho metrics writer.
  - `make verify` là cổng kiểm tra sau deploy.

## 9. Ngoài phạm vi (YAGNI)

- Container hóa các app (giữ native systemd để binding source-IP sạch).
- Auto-scaling / orchestration (k8s).
- Molecule test cho Ansible roles.
- Alerting rules nâng cao (có thể thêm sau khi dashboard ổn).
