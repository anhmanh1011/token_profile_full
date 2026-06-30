# deploy/ — Ansible cho stack token-tool (multi-VPS)

Nguồn cấu hình + tự động hóa triển khai 3 app VPS giống hệt nhau (mỗi VPS chạy
tối đa 2 tenant) + 1 ops node (Grafana + Grafana Loki + Prometheus).

Mọi lệnh chạy với CWD = thư mục `deploy/` này (Ansible tự nạp `ansible.cfg`).
Dùng `make help` để xem các thao tác.

> **Lưu ý thực thi:** các bước `--check`/chạy thật/verify trong 2 plan
> (`docs/superpowers/plans/2026-06-26-multi-vps-*.md`) cần SSH tới VPS thật +
> secret trong vault. Các file ở đây đã được validate **tĩnh** (ansible
> syntax-check, ansible-lint, inventory graph, jinja/yaml parse, vault round-trip)
> nhưng chưa chạy lên host nào.

## Bản đồ file

| Đường dẫn | Plan | Mô tả |
|-----------|------|-------|
| `ansible.cfg` | 2 | Cấu hình Ansible (inventory, roles_path, vault, callback) |
| `requirements.yml` | 2 | Collection cần cài (community.general/docker, ansible.posix) |
| `inventory/hosts.yml` | 2 | Inventory tĩnh: nhóm `app_vps` (vps1..3) + `ops` |
| `group_vars/all/tenants.yml` | 2 | NGUỒN CẤU HÌNH DUY NHẤT: app_vps, ops_host, danh sách tenant |
| `group_vars/all/vault.yml.example` | 2 | MẪU plaintext của secret (commit được) |
| `group_vars/all/vault.yml` | 2 | Secret thật, ansible-vault (KHÔNG commit — gitignore) |
| `files/usernames/common.txt` | 2 | Username dùng chung cho tenant trỏ `usernames_file` |
| `Makefile` | 2 | Bọc ansible-playbook/ansible cho mọi thao tác |
| `playbooks/site.yml` | 2 | Provision: deps, user, build, units (idempotent) |
| `playbooks/deploy.yml` | 2 | Sync code + rebuild phần đổi + rolling restart (serial:1) |
| `playbooks/control.yml` | 2 | start/stop/restart/status/logs per-tenant |
| `playbooks/verify.yml` | 2 | Cổng kiểm tra sau deploy (unit active, /status, source-IP, metrics) |
| `playbooks/monitoring.yml` | 3 | Dựng stack giám sát trên ops node |
| `roles/common/` | 2 | OS deps, user `tokentool`, thư mục, toolchain (Go/Rust/venv) |
| `roles/app/` | 2 | Sync code → `/opt/token-tool`, build Go + Rust, tạo venv |
| `roles/tenant/` | 2 | env file, admin_token.json, systemd template units, email-gen, enable/start |
| `roles/observability/` | 3 | node_exporter (textfile) + Grafana Alloy agent trên app VPS |
| `monitoring/` | 3 | docker-compose grafana+loki+prometheus+json-exporter + provisioning |

## Quy ước đường dẫn TRÊN VPS (do role `tenant`/`common`/`observability` render)

| Đường dẫn | Quyền | Nội dung |
|-----------|-------|----------|
| service user/group | — | `tokentool` (system user, no-login shell, non-root) |
| `/opt/token-tool/` | owner `tokentool` | code + binary: `Manage_User/`, `Get_Profile/get_profile`, `email-gen/target/release/email-gen`, venv `Manage_User/.venv` |
| `/etc/token-tool/admin_token.<id>.json` | `0600 tokentool:tokentool` | secret admin OAuth của tenant |
| `/etc/token-tool/<id>.env` | `0640 root:tokentool` | `EnvironmentFile` cho systemd unit |
| `/var/lib/token-tool/<id>/` | `0750 tokentool` | `emails.txt`, `<id>.ckpt`, `result.txt`, `inputs/{domains,usernames}.txt`, `service.log`, `output_*.log` |
| `/var/lib/node_exporter/textfile/getprofile_<id>.prom` | `0644` | metrics textfile cho node_exporter |

Unit template: `manageuser@<id>.service`, `getprofile@<id>.service`,
`emailgen@<id>.service` (Type=oneshot). Port: `tenant.manageuser_port`
(5000, 5001, ...). Get_Profile gọi `--api http://127.0.0.1:<port>`.

## Quy trình

1. Cài: `ansible-galaxy collection install -r requirements.yml`.
2. Điền IP thật vào `group_vars/all/tenants.yml` (`app_vps`, `ops_host`).
3. Tạo secret: xem hướng dẫn trong `group_vars/all/vault.yml.example`.
4. `make provision` (bootstrap) → `make deploy` (cập nhật) → `make verify`.
5. (Plan 3) `make monitoring-up` để dựng giám sát trên ops node.
