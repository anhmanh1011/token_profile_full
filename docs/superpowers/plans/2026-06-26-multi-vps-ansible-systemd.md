# Multi-VPS — Ansible + systemd Deployment Plan (Plan 2/3)

> 🟡 **TRẠNG THÁI: SCAFFOLD + CODE-PREREQ ĐÃ THỰC THI; BƯỚC CHẠY-HOST DEFERRED (2026-06-30).**
> - ✅ **2 code-prereq Python** (TDD, đã test): `AdminTokenManager.config_path` (token xoay ghi về `--config` per-tenant) + `MANAGE_USER_LOG_FILE`/`_resolve_log_file()`.
> - ✅ **Toàn bộ `deploy/`** đã tạo + **validate tĩnh**: `ansible-playbook --syntax-check` (4 playbook OK), `ansible-lint` (pass profile `min`; còn `var-naming` style do biến `token_tool_*` chia sẻ xuyên role — cố ý), `ansible-inventory --graph`, jinja2 parse (7 template), vault round-trip, Makefile TAB/guard.
> - ⏸️ **DEFERRED (cần hạ tầng thật):** `--check`/chạy thật/idempotence-on-host/verify (`ss` source-IP, `curl /status`, metrics freshness) — cần SSH tới 3 VPS + secret trong vault.
> - Quyết định hợp nhất đã áp dụng: inventory `hosts.yml`; group `ops`/host `ops1` tách tên; một Makefile (`action`/`tenant`); node_exporter do role `observability` (Plan 3) sở hữu, `common` chỉ cài package + tạo textfile dir; `site.yml` phạm vi Plan 2 (common/app/tenant), Plan 3 sẽ nối observability + play ops.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.
>
> **Language skills (CLAUDE.md, mandatory):** Python tasks → `python-patterns`, `python-testing`, `security-review`. Ansible/systemd/secrets → `security-review`, `deployment-patterns`, `docker-patterns` (Plan 3).

**Goal:** Dựng hạ tầng Ansible tại `deploy/` để bootstrap + cập nhật 3 app VPS giống hệt nhau (mỗi VPS chạy nhiều tenant qua systemd template units), với `tenants.yml` là nguồn cấu hình duy nhất, secret qua `ansible-vault`, email-gen sinh trên VPS, và quản lý qua `make`.

**Architecture:** 1 ops node + 3 app VPS. Mỗi tenant = cặp `manageuser@<id>` + `getprofile@<id>` (+ `emailgen@<id>` oneshot), nạp tham số từ `EnvironmentFile=/etc/token-tool/<id>.env`, chạy non-root `tokentool`, giữ binding source-IP per-tenant. Phụ thuộc Plan 1 (flag `--license-sku`/`--usage-location`/`--metrics-file`).

**Tech Stack:** ansible-core ≥2.16 (community.general, community.docker, ansible.posix), systemd template units, Python venv, Go 1.25, Rust (rustup), ufw, ansible-vault.

**Prereq:** Plan 1 (code changes) nên hoàn tất trước (flag CLI + pytest tooling).

---

## ⚠️ Quyết định hợp nhất (authoritative — đọc TRƯỚC, giải quyết mọi divergence giữa các Part)

Bản plan gồm 3 Part (A scaffold, B roles common/app, C role tenant + playbooks + verify). Khi các Part mô tả khác nhau về cùng một file, theo các quyết định sau (đã vá inline ở nơi liên quan):

1. **Inventory** = `deploy/inventory/hosts.yml` (YAML, Part A). Mọi lệnh ghi `inventory/hosts.ini` → đọc là `inventory/hosts.yml`.
2. **Makefile** = MỘT file `deploy/Makefile` (bản Part A: `help/provision/deploy/status/verify/restart/start/stop/logs/monitoring-up/ping/graph/vault-view`). Bỏ qua việc tạo Makefile lần hai ở Part C (Part C chỉ tạo `playbooks/verify.yml`). Trong Makefile Part A: các target `status/restart/start/stop` truyền `-e action=<...> -e tenant=<...>`, `verify` gọi `playbooks/verify.yml`, `logs` chạy `ansible <host> -b -a "journalctl -u manageuser@<TENANT> -u getprofile@<TENANT> -n 200 --no-pager"`. (Thay `control_action`/`control_tenant` → `action`/`tenant` để khớp `control.yml`.)
3. **control.yml** canonical = bản Part C (biến `action` ∈ start|stop|restart|status; `tenant` = id hoặc `all`).
4. **Code-prereq log isolation** thực hiện MỘT LẦN ở Part C (`_resolve_log_file()` — sạch, xử lý whitespace). Tại Part B "Task A", chỉ tạo `requirements-dev.txt` + `tests/conftest.py`; bỏ qua sửa `app.py`. (Plan 1 Task 0 có thể đã tạo `requirements-dev.txt` + `tests/__init__.py`.)
5. **node_exporter** sở hữu bởi role `observability` (Plan 3): cài + ARGS/listen `0.0.0.0:9100` + enable + ufw. Role `common` (Part B) CHỈ cài package + tạo `/var/lib/node_exporter/textfile`. (Prometheus ở ops pull `:9100` qua mạng nên không thể bind 127.0.0.1.)
6. **Code-prereq AdminTokenManager** (task ngay dưới) bắt buộc cho đa-tenant: token xoay phải ghi về `--config` per-tenant, không phải file cố định.

---

## Code prerequisites (Python) — làm TRƯỚC khi role `app` sync code

Hai thay đổi code nhỏ là tiền đề cho đa-tenant. **Reconciliation #4:** thay đổi log
(`MANAGE_USER_LOG_FILE`) được trình bày trong Part C (`_resolve_log_file()`) — thực hiện
MỘT LẦN ở đó. Task dưới đây là code-prereq thứ hai (bắt buộc, riêng biệt).

### Task 1: Code-prereq (TDD): `AdminTokenManager` lưu refresh_token xoay về file `--config` per-tenant

Hiện `AdminTokenManager.save_if_updated()` ghi refresh_token đã xoay vào hằng
`CONFIG_FILE = Path(__file__).parent / "admin_token.json"` (cố định theo code dir),
KHÔNG ghi vào file `--config` của tenant (`/etc/token-tool/admin_token.<id>.json`).
Đa-tenant dùng chung `/opt/token-tool/Manage_User` ⇒ các tenant ghi đè lẫn nhau và file
config per-tenant không bao giờ nhận token mới ⇒ admin OAuth hỏng sau restart khi Microsoft
xoay token. Sửa: cho `AdminTokenManager` nhận `config_path` và lưu về đúng đường dẫn đó.

**Files:**
- Create: `Manage_User/tests/test_admin_config_path.py`
- Modify: `Manage_User/admin_token_manager.py`
- Modify: `Manage_User/app.py`

Steps:

- [ ] **Step 1 (RED): Viết test** `Manage_User/tests/test_admin_config_path.py`:

```python
"""save_if_updated() must persist the rotated refresh_token to the per-tenant
--config file, not the module-level admin_token.json."""
import json
from pathlib import Path

import admin_token_manager as atm
from admin_token_manager import AdminTokenManager


def _write_cfg(path: Path, token: str) -> None:
    path.write_text(json.dumps([{
        "tenant_id": "tid", "refresh_token": token,
        "username": "admin@t1.example", "domain": "t1.example",
    }]), encoding="utf-8")


def test_save_writes_to_config_path(tmp_path):
    cfg = tmp_path / "admin_token.t1.json"
    _write_cfg(cfg, "OLD")
    mgr = AdminTokenManager(json.loads(cfg.read_text())[0], config_path=cfg)
    # Simulate Microsoft rotating the refresh_token.
    mgr.refresh_token = "NEW"
    mgr._refresh_token_updated = True
    mgr.save_if_updated()
    assert json.loads(cfg.read_text())[0]["refresh_token"] == "NEW"
    # The module-level default file is never touched.
    assert atm.CONFIG_FILE != cfg


def test_config_path_defaults_to_module_file(tmp_path):
    mgr = AdminTokenManager({"tenant_id": "t", "refresh_token": "r"})
    assert mgr.config_path == atm.CONFIG_FILE
```

- [ ] **Step 2 (RED): Chạy test** — `cd Manage_User && python -m pytest tests/test_admin_config_path.py -v`
  Expected: FAIL — `AdminTokenManager.__init__() got an unexpected keyword argument 'config_path'`.

- [ ] **Step 3 (GREEN): Sửa `Manage_User/admin_token_manager.py`** — thêm tham số `config_path`.
  Đổi chữ ký + đầu `__init__` (dòng ~28-33):

```python
    def __init__(self, admin: dict):
        self.tenant_id = admin["tenant_id"]
        self.refresh_token = admin["refresh_token"]
        self.domain = admin.get("domain", "")
        # Per-tenant outbound source IP (callout IP). None = OS default route.
        self.local_ip = parse_local_ip(admin.get("local_ip"))
```

  thành:

```python
    def __init__(self, admin: dict, config_path: Optional[Path] = None):
        self.tenant_id = admin["tenant_id"]
        self.refresh_token = admin["refresh_token"]
        self.domain = admin.get("domain", "")
        # Per-tenant outbound source IP (callout IP). None = OS default route.
        self.local_ip = parse_local_ip(admin.get("local_ip"))
        # Persist rotated refresh_token back to THIS tenant's --config file
        # (e.g. /etc/token-tool/admin_token.<id>.json), not the shared default.
        self.config_path = Path(config_path) if config_path else CONFIG_FILE
```

- [ ] **Step 4 (GREEN): Sửa `save_if_updated()`** trong cùng file — thay 3 chỗ `CONFIG_FILE` → `self.config_path`:

```python
    def save_if_updated(self) -> None:
        """Save updated refresh_token to this tenant's config file if rotated."""
        if not self._refresh_token_updated:
            return

        try:
            with open(self.config_path, "r", encoding="utf-8") as f:
                admins = json.load(f)

            # Update first admin's refresh_token
            if admins:
                admins[0]["refresh_token"] = self.refresh_token
                with open(self.config_path, "w", encoding="utf-8") as f:
                    json.dump(admins, f, indent=4, ensure_ascii=False)
                logger.info("Saved updated refresh_token to %s", self.config_path.name)
        except Exception as e:
            logger.error("Failed to save refresh_token: %s", e)
```

  Bảo đảm `from pathlib import Path` và `from typing import Optional` đã có ở đầu file
  (CONFIG_FILE dùng `Path` nên `Path` đã import; thêm `Optional` nếu thiếu).

- [ ] **Step 5 (GREEN): Sửa `Manage_User/app.py`** — truyền `--config` path vào manager.
  Trong `start_service`, đổi:

```python
        _token_mgr = AdminTokenManager(_admin_config)
```

  thành:

```python
        _token_mgr = AdminTokenManager(_admin_config, config_path=config_file)
```

- [ ] **Step 6 (GREEN): Chạy test + compile** — `cd Manage_User && python -m pytest tests/test_admin_config_path.py -v && python -m py_compile admin_token_manager.py app.py`
  Expected: `2 passed`; py_compile không output.

- [ ] **Step 7: Commit**

```bash
git add Manage_User/admin_token_manager.py Manage_User/app.py Manage_User/tests/test_admin_config_path.py
git commit -m "fix(manage_user): persist rotated admin refresh_token to per-tenant --config file"
```

---

## Phần A — Scaffold `deploy/` + nguồn cấu hình + Makefile + vault

Phần này dựng bộ khung Ansible tại `deploy/` (chạy từ ops node hoặc laptop), khai báo nguồn cấu hình duy nhất (`tenants.yml`), cơ chế secret (`ansible-vault`), và Makefile bọc mọi thao tác. Các playbook/role/monitoring được điền ở các phần Plan 2/3 kế tiếp; phần này chỉ tạo khung + file cấu hình tĩnh nên **không TDD** mà verify bằng các lệnh cấu hình Ansible (`ansible-config dump`, `ansible-inventory --graph`, vault round-trip).

**Tiền đề kỹ thuật (engineer chạy 1 lần trên control node):**
- Cài `ansible-core >= 2.16` (`pipx install ansible-core` hoặc `apt install ansible`).
- Mọi lệnh trong phần này chạy với **CWD = `deploy/`** của repo (để Ansible tự nạp `deploy/ansible.cfg`). Trong plan, đường dẫn file ghi tương đối repo (`deploy/...`); đường dẫn trong lệnh ghi tương đối `deploy/`.
- An toàn (security-review): secret chỉ nằm trong `vault.yml` mã hóa; file mật khẩu `.vault-pass` và `vault.yml` đều gitignore; tenant chạy non-root `tokentool`; không in token ra stdout (các task render token ở phần role dùng `no_log: true`).

---

### Task 2: Scaffold cây thư mục `deploy/` + README overview

Tạo bộ khung thư mục và một README liệt kê **mọi file** sẽ tạo trong Plan 2 + Plan 3, để engineer biết file nào thuộc phần nào.

**Files:**
- Create `deploy/README.md`
- Create `deploy/playbooks/.gitkeep`
- Create `deploy/roles/.gitkeep`
- Create `deploy/monitoring/.gitkeep`
- Create `deploy/files/.gitkeep`

Steps:

- [ ] Tạo cây thư mục khung:
  ```bash
  mkdir -p deploy/inventory deploy/group_vars/all deploy/playbooks \
           deploy/roles deploy/monitoring deploy/files/usernames
  touch deploy/playbooks/.gitkeep deploy/roles/.gitkeep \
        deploy/monitoring/.gitkeep deploy/files/.gitkeep
  ```

- [ ] Tạo `deploy/README.md` với nội dung đầy đủ sau (bản đồ file + quy ước đường dẫn trên VPS):
  ````markdown
  # deploy/ — Ansible cho stack token-tool (multi-VPS)

  Nguồn cấu hình + tự động hóa triển khai 3 app VPS giống hệt nhau (mỗi VPS chạy
  tối đa 2 tenant) + 1 ops node (Grafana + Grafana Loki + Prometheus).

  Mọi lệnh chạy với CWD = thư mục `deploy/` này (Ansible tự nạp `ansible.cfg`).
  Dùng `make help` để xem các thao tác.

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
  | `playbooks/control.yml` | 2 | status/restart/start/stop/logs/verify per-tenant |
  | `playbooks/monitoring.yml` | 3 | Dựng stack giám sát trên ops node |
  | `roles/common/` | 2 | OS deps, user `tokentool`, thư mục, toolchain (Go/Rust/venv) |
  | `roles/app/` | 2 | Sync code → `/opt/token-tool`, build Go + Rust, tạo venv |
  | `roles/tenant/` | 2 | env file, admin_token.json, systemd template units, email-gen, enable/start |
  | `roles/observability/` | 3 | Grafana Alloy agent trên app VPS |
  | `monitoring/docker-compose.yml` | 3 | grafana + loki + prometheus + json_exporter |
  | `monitoring/prometheus/` | 3 | prometheus.yml + json_exporter config |
  | `monitoring/loki/` | 3 | loki-config.yml |
  | `monitoring/grafana/provisioning/` | 3 | datasources + dashboards provisioning |
  | `monitoring/grafana/dashboards/` | 3 | dashboard JSON (1 hàng/tenant) |

  ## Quy ước đường dẫn TRÊN VPS (do role `tenant`/`common` render)

  | Đường dẫn | Quyền | Nội dung |
  |-----------|-------|----------|
  | service user/group | — | `tokentool` (system user, no-login shell, non-root) |
  | `/opt/token-tool/` | owner `tokentool` | code + binary: `Manage_User/`, `Get_Profile/get_profile`, `email-gen/target/release/email-gen`, venv `Manage_User/.venv` |
  | `/etc/token-tool/admin_token.<id>.json` | `0600 tokentool:tokentool` | secret admin OAuth của tenant |
  | `/etc/token-tool/<id>.env` | `0640 root:tokentool` | `EnvironmentFile` cho systemd unit |
  | `/var/lib/token-tool/<id>/` | `0750 tokentool` | `emails.txt`, `<id>.ckpt`, `result.txt`, `inputs/{domains,usernames}.txt`, `service.log`, `output_*.log` |
  | `/var/lib/node_exporter/textfile/getprofile_<id>.prom` | — | metrics textfile cho node_exporter |

  Unit template: `manageuser@<id>.service`, `getprofile@<id>.service`,
  `emailgen@<id>.service` (Type=oneshot). Port: `tenant.manageuser_port`
  (5000, 5001, ...). Get_Profile gọi `--api http://127.0.0.1:<port>`.

  ## Quy trình
  1. Điền IP thật vào `group_vars/all/tenants.yml` (`app_vps`, `ops_host`).
  2. Tạo secret: `make` không có target cho việc này — xem `vault.yml.example`.
  3. `make provision` (bootstrap) → `make deploy` (cập nhật) → `make verify`.
  ````

- [ ] Xác minh khung tồn tại:
  ```bash
  find deploy -maxdepth 2 -type d | sort
  ```
  Expected (thứ tự có thể khác):
  ```
  deploy
  deploy/files
  deploy/files/usernames
  deploy/group_vars
  deploy/group_vars/all
  deploy/inventory
  deploy/monitoring
  deploy/playbooks
  deploy/roles
  ```

- [ ] Commit:
  ```bash
  git add deploy/README.md deploy/playbooks/.gitkeep deploy/roles/.gitkeep \
          deploy/monitoring/.gitkeep deploy/files/.gitkeep
  git commit -m "deploy: scaffold deploy/ skeleton + file-map README"
  ```

---

### Task 3: `deploy/ansible.cfg` + `deploy/requirements.yml`

Cấu hình Ansible trung tâm. `stdout_callback = yaml` cần collection `community.general`, nên kèm `requirements.yml`.

**Files:**
- Create `deploy/ansible.cfg`
- Create `deploy/requirements.yml`

Steps:

- [ ] Tạo `deploy/ansible.cfg`:
  ```ini
  # deploy/ansible.cfg — cấu hình Ansible cho stack token-tool.
  # Tự nạp khi chạy lệnh ansible với CWD = deploy/.
  [defaults]
  inventory            = inventory/hosts.yml
  roles_path           = roles
  host_key_checking    = False
  retry_files_enabled  = False
  stdout_callback      = yaml
  interpreter_python   = auto_silent
  forks                = 10
  # vault_password_file: TÙY CHỌN. Bỏ comment để mọi lệnh tự đọc mật khẩu vault
  # từ file (đã gitignore). Makefile cũng tự truyền --vault-password-file khi
  # file .vault-pass tồn tại, nên có thể để nguyên comment.
  # vault_password_file = .vault-pass

  [ssh_connection]
  pipelining = True
  # Tái dùng kết nối SSH cho nhanh (an toàn với pipelining vì không dùng sudo password).
  ssh_args   = -o ControlMaster=auto -o ControlPersist=60s -o PreferredAuthentications=publickey

  [privilege_escalation]
  # become bật ở cấp play (provision/deploy cần root); để mặc định tắt ở đây.
  become        = False
  become_method = sudo
  ```

- [ ] Tạo `deploy/requirements.yml`:
  ```yaml
  ---
  # deploy/requirements.yml — collection Ansible cần cài trên control node.
  # Cài: ansible-galaxy collection install -r requirements.yml
  collections:
    - name: community.general    # stdout_callback=yaml + nhiều module tiện ích
      version: ">=8.0.0"
    - name: community.docker     # (Plan 3) quản lý docker compose stack giám sát trên ops node
      version: ">=3.4.0"
    - name: ansible.posix        # sysctl, firewalld, mount... dùng ở role common/observability
      version: ">=1.5.0"
  ```

- [ ] Cài collection (cần cho `stdout_callback=yaml`):
  ```bash
  ansible-galaxy collection install -r requirements.yml
  ```
  Expected: dòng `community.general ... was installed successfully` (hoặc `Nothing to do` nếu đã có).

- [ ] Verify ansible.cfg được áp dụng:
  ```bash
  ansible-config dump --only-changed | grep -E 'INVENTORY|ROLES_PATH|HOST_KEY_CHECKING|STDOUT_CALLBACK|RETRY_FILES'
  ```
  Expected (giá trị, đường dẫn tuyệt đối có thể khác máy):
  ```
  DEFAULT_HOST_LIST(.../deploy/ansible.cfg) = ['.../deploy/inventory/hosts.yml']
  DEFAULT_ROLES_PATH(.../deploy/ansible.cfg) = ['.../deploy/roles']
  DEFAULT_STDOUT_CALLBACK(.../deploy/ansible.cfg) = yaml
  HOST_KEY_CHECKING(.../deploy/ansible.cfg) = False
  RETRY_FILES_ENABLED(.../deploy/ansible.cfg) = False
  ```
  Và kiểm tra pipelining:
  ```bash
  ansible-config dump | grep -i 'PIPELINING'
  ```
  Expected: chứa `ANSIBLE_PIPELINING(.../deploy/ansible.cfg) = True`.

- [ ] Commit:
  ```bash
  git add deploy/ansible.cfg deploy/requirements.yml
  git commit -m "deploy: add ansible.cfg (yaml callback, no retry, pipelining) + galaxy requirements"
  ```

---

### Task 4: `deploy/inventory/hosts.yml`

Inventory tĩnh khai báo nhóm `app_vps` (vps1/vps2/vps3) và `ops`. `ansible_host` được **template từ `app_vps`/`ops_host`** (định nghĩa trong `tenants.yml`) nên IP chỉ khai báo một nơi duy nhất; có `default` để không lỗi khi `tenants.yml` chưa nạp.

**Map tenant → host:** inventory KHÔNG ánh xạ tenant. Mỗi tenant trong `tenants.yml` có field `vps` (vps1|vps2|vps3). Playbook lặp danh sách `tenants` và lọc `when: item.vps == inventory_hostname`, nên khi chạy trên `vps1` chỉ các tenant `vps: vps1` được cấu hình.

**Files:**
- Create `deploy/inventory/hosts.yml`

Steps:

- [ ] Tạo `deploy/inventory/hosts.yml`:
  ```yaml
  ---
  # deploy/inventory/hosts.yml — inventory tĩnh: 3 app VPS + 1 ops node.
  #
  # ansible_host được template từ app_vps/ops_host trong group_vars/all/tenants.yml
  # (nguồn IP duy nhất). Nếu tenants.yml chưa nạp, fallback về tên host (không lỗi).
  #
  # Map tenant->host KHÔNG ở đây: mỗi tenant có field `vps`; playbook lọc
  # `when: item.vps == inventory_hostname`.
  all:
    vars:
      # SSH user có sudo (production Ubuntu thường là 'ubuntu'). Provision dùng become.
      ansible_user: ubuntu
      ansible_python_interpreter: /usr/bin/python3
      ansible_host: >-
        {{ (app_vps | default({}) | combine(ops_host | default({})))
           .get(inventory_hostname, {})
           .get('ansible_host', inventory_hostname) }}
    children:
      app_vps:
        hosts:
          vps1: {}
          vps2: {}
          vps3: {}
      ops:
        hosts:
          ops: {}
  ```

- [ ] Verify cấu trúc inventory (offline, không cần SSH):
  ```bash
  ansible-inventory --graph
  ```
  Expected:
  ```
  @all:
    |--@app_vps:
    |  |--vps1
    |  |--vps2
    |  |--vps3
    |--@ops:
    |  |--ops
    |--@ungrouped:
  ```

- [ ] Verify fallback `ansible_host` không lỗi khi `tenants.yml` chưa có:
  ```bash
  ansible-inventory --host vps1 | grep ansible_host
  ```
  Expected (lúc này chưa có `app_vps`, fallback về tên host):
  ```
      "ansible_host": "vps1",
  ```
  (Sau khi thêm `tenants.yml` ở task sau, giá trị này tự đổi thành IP thật.)

- [ ] Commit:
  ```bash
  git add deploy/inventory/hosts.yml
  git commit -m "deploy: add static inventory (app_vps/ops) with templated ansible_host"
  ```

---

### Task 5: `deploy/group_vars/all/tenants.yml` (nguồn cấu hình duy nhất)

Khai báo `app_vps`, `ops_host`, và danh sách `tenants` đầy đủ cho 6 tenant (2/VPS). Secret (`refresh_token`, `tenant_id`) tham chiếu biến vault `{{ vault_<id>_refresh }}` / `{{ vault_<id>_tenant_id }}`. Tạo luôn file username dùng chung mà `t3` trỏ tới.

**Files:**
- Create `deploy/group_vars/all/tenants.yml`
- Create `deploy/files/usernames/common.txt`

Steps:

- [ ] Tạo `deploy/files/usernames/common.txt` (sample nhỏ — tenant nào trỏ `usernames_file` sẽ dùng):
  ```text
  john.doe
  jane.smith
  michael.brown
  emily.davis
  david.wilson
  ```

- [ ] Tạo `deploy/group_vars/all/tenants.yml` với nội dung đầy đủ:
  ```yaml
  ---
  # deploy/group_vars/all/tenants.yml
  # ==========================================================================
  # NGUỒN CẤU HÌNH DUY NHẤT cho toàn bộ tenant trên 3 app VPS.
  # Thêm/sửa/bớt tenant = sửa file này rồi `make provision` (idempotent).
  #
  # - Secret (refresh_token, tenant_id) KHÔNG để ở đây: tham chiếu biến vault
  #   trong group_vars/all/vault.yml (ansible-vault). Xem vault.yml.example.
  # - Map tenant -> host qua field `vps` (vps1|vps2|vps3); playbook lọc
  #   `when: item.vps == inventory_hostname`.
  # - IP trong app_vps/ops_host là MẪU (dải tài liệu RFC 5737 / 2001:db8::/32);
  #   THAY bằng IP/DNS thật. local_ip mỗi tenant = callout IP egress riêng.
  # ==========================================================================

  # Registry host (nguồn IP duy nhất; inventory/hosts.yml template ansible_host từ đây).
  app_vps:
    vps1: { ansible_host: 203.0.113.10 }
    vps2: { ansible_host: 198.51.100.10 }
    vps3: { ansible_host: 192.0.2.10 }

  ops_host:
    ops: { ansible_host: 203.0.113.200 }

  # Mặc định toàn cục — mỗi tenant có thể override.
  tenant_defaults:
    license_sku: a1-students        # alias -> STANDARDWOFFPACK_STUDENT (Office 365 A1 for students)
    usage_location: US
    workers: 400
    max_cpm: 20000

  tenants:
    # ---- VPS1: t1 (IPv4) + t2 (IPv4) ---------------------------------------
    - id: t1
      vps: vps1
      local_ip: 203.0.113.10
      manageuser_port: 5000
      admin:
        tenant_id: "{{ vault_t1_tenant_id }}"
        username: admin@tenant1.example
        domain: tenant1.example
        refresh_token: "{{ vault_t1_refresh }}"
      license_sku: a1-students
      usage_location: US
      emailgen:
        domains: [tenant1.example, alumni.tenant1.example]
        usernames: [john.doe, jane.smith, alex.lee]
      workers: 400
      max_cpm: 20000

    - id: t2
      vps: vps1
      local_ip: 203.0.113.11
      manageuser_port: 5001
      admin:
        tenant_id: "{{ vault_t2_tenant_id }}"
        username: admin@tenant2.example
        domain: tenant2.example
        refresh_token: "{{ vault_t2_refresh }}"
      license_sku: a1-students
      usage_location: US
      emailgen:
        domains: [tenant2.example]
        usernames: [mary.jones, peter.pan, sara.kim, tom.ford]
      workers: 400
      max_cpm: 20000

    # ---- VPS2: t3 (IPv4, usernames_file) + t4 (IPv6, license GUID override) -
    - id: t3
      vps: vps2
      local_ip: 198.51.100.10
      manageuser_port: 5000
      admin:
        tenant_id: "{{ vault_t3_tenant_id }}"
        username: admin@tenant3.example
        domain: tenant3.example
        refresh_token: "{{ vault_t3_refresh }}"
      license_sku: a1-students
      usage_location: US
      emailgen:
        domains: [tenant3.example]
        # Dùng file commit trong repo thay vì inline (role tenant copy lên VPS).
        usernames_file: files/usernames/common.txt
      workers: 400
      max_cpm: 20000

    - id: t4
      vps: vps2
      local_ip: "2001:db8:51::10"      # IPv6 callout IP (chuỗi -> phải quote)
      manageuser_port: 5001
      admin:
        tenant_id: "{{ vault_t4_tenant_id }}"
        username: admin@tenant4.example
        domain: tenant4.example
        refresh_token: "{{ vault_t4_refresh }}"
      # Override license: dùng GUID skuId trực tiếp thay alias.
      license_sku: 314c4481-f395-4525-be8b-2ec4bb1e9d91
      usage_location: GB               # override usage_location ví dụ
      emailgen:
        domains: [tenant4.example, study.tenant4.example]
        usernames: [li.wei, chen.yu]
      workers: 400
      max_cpm: 20000

    # ---- VPS3: t5 (IPv4, workers/max_cpm override) + t6 (IPv6, partnumber sku)
    - id: t5
      vps: vps3
      local_ip: 192.0.2.10
      manageuser_port: 5000
      admin:
        tenant_id: "{{ vault_t5_tenant_id }}"
        username: admin@tenant5.example
        domain: tenant5.example
        refresh_token: "{{ vault_t5_refresh }}"
      license_sku: a1-students
      usage_location: US
      emailgen:
        domains: [tenant5.example]
        usernames: [omar.aziz, nadia.h]
      workers: 300                     # override: VPS yếu hơn
      max_cpm: 15000                   # override

    - id: t6
      vps: vps3
      local_ip: "2001:db8:2::10"       # IPv6
      manageuser_port: 5001
      admin:
        tenant_id: "{{ vault_t6_tenant_id }}"
        username: admin@tenant6.example
        domain: tenant6.example
        refresh_token: "{{ vault_t6_refresh }}"
      # Override license bằng skuPartNumber tường minh.
      license_sku: STANDARDWOFFPACK_STUDENT
      usage_location: US
      emailgen:
        domains: [tenant6.example]
        usernames: [ivan.petrov, olga.k, dmitry.s]
      workers: 400
      max_cpm: 20000
  ```

- [ ] Verify YAML hợp lệ + đúng số tenant/port (deterministic, không cần vault):
  ```bash
  python3 - <<'PY'
  import yaml
  d = yaml.safe_load(open("group_vars/all/tenants.yml"))
  ts = d["tenants"]
  assert len(ts) == 6, len(ts)
  assert [t["id"] for t in ts] == ["t1","t2","t3","t4","t5","t6"]
  # 2 tenant/VPS, port 5000/5001
  from collections import Counter
  by_vps = Counter(t["vps"] for t in ts)
  assert by_vps == Counter({"vps1":2,"vps2":2,"vps3":2}), by_vps
  ports = {t["vps"]: [] for t in ts}
  for t in ts: ports[t["vps"]].append(t["manageuser_port"])
  assert all(sorted(v) == [5000,5001] for v in ports.values()), ports
  # secret luôn là tham chiếu vault, không phải giá trị thật
  for t in ts:
      assert t["admin"]["refresh_token"].startswith("{{ vault_"), t["id"]
      assert t["admin"]["tenant_id"].startswith("{{ vault_"), t["id"]
  print("OK: 6 tenants, 2/VPS, ports 5000/5001, secrets via vault")
  PY
  ```
  Expected: `OK: 6 tenants, 2/VPS, ports 5000/5001, secrets via vault`

- [ ] Verify inventory đã lấy IP thật từ `app_vps` (cập nhật `--graph` không lỗi):
  ```bash
  ansible-inventory --graph >/dev/null && echo "inventory loads OK with tenants.yml"
  ```
  Expected: `inventory loads OK with tenants.yml`

- [ ] Commit:
  ```bash
  git add deploy/group_vars/all/tenants.yml deploy/files/usernames/common.txt
  git commit -m "deploy: add tenants.yml single source (6 tenants, 2/VPS) + shared usernames file"
  ```

---

### Task 6: Secrets: `vault.yml.example` + `.gitignore` + quy trình vault-pass

`tenants.yml` tham chiếu `{{ vault_<id>_refresh }}` và `{{ vault_<id>_tenant_id }}`. Cung cấp file mẫu plaintext (commit được), hướng dẫn tạo `vault.yml` mã hóa, và cập nhật `.gitignore` để KHÔNG bao giờ commit secret/mật khẩu.

**Files:**
- Create `deploy/group_vars/all/vault.yml.example`
- Modify `.gitignore` (root repo)

Steps:

- [ ] Tạo `deploy/group_vars/all/vault.yml.example` (MẪU — giá trị giả, đúng định dạng):
  ```yaml
  ---
  # deploy/group_vars/all/vault.yml.example
  # ==========================================================================
  # MẪU plaintext của secret per-tenant. KHÔNG dùng trực tiếp, KHÔNG chứa secret thật.
  # tenants.yml tham chiếu: admin.refresh_token={{ vault_<id>_refresh }},
  #                         admin.tenant_id   ={{ vault_<id>_tenant_id }}.
  # Mỗi tenant cần đúng 2 biến: vault_<id>_refresh, vault_<id>_tenant_id.
  #
  # TẠO file vault thật (mã hóa) — chọn 1 trong 2 cách:
  #
  #   Cách A (tạo trực tiếp, mở editor):
  #     ansible-vault create group_vars/all/vault.yml
  #     # dán nội dung theo mẫu dưới, thay GIÁ TRỊ THẬT, lưu & thoát.
  #
  #   Cách B (từ file này):
  #     cp group_vars/all/vault.yml.example /tmp/vault.plain.yml
  #     # sửa /tmp/vault.plain.yml với giá trị thật
  #     ansible-vault encrypt --output group_vars/all/vault.yml /tmp/vault.plain.yml
  #     rm -f /tmp/vault.plain.yml          # XÓA bản plaintext ngay
  #
  # Mật khẩu vault: lưu vào deploy/.vault-pass (chmod 600, ĐÃ gitignore) hoặc
  # nhập tay với --ask-vault-pass. Sửa secret sau này: ansible-vault edit group_vars/all/vault.yml
  # ==========================================================================

  # --- vps1 ---
  vault_t1_refresh:   "0.AXEAReplaceWithRealRefreshTokenForTenant1...AAA"
  vault_t1_tenant_id: "11111111-1111-1111-1111-111111111111"
  vault_t2_refresh:   "0.AXEAReplaceWithRealRefreshTokenForTenant2...AAA"
  vault_t2_tenant_id: "22222222-2222-2222-2222-222222222222"

  # --- vps2 ---
  vault_t3_refresh:   "0.AXEAReplaceWithRealRefreshTokenForTenant3...AAA"
  vault_t3_tenant_id: "33333333-3333-3333-3333-333333333333"
  vault_t4_refresh:   "0.AXEAReplaceWithRealRefreshTokenForTenant4...AAA"
  vault_t4_tenant_id: "44444444-4444-4444-4444-444444444444"

  # --- vps3 ---
  vault_t5_refresh:   "0.AXEAReplaceWithRealRefreshTokenForTenant5...AAA"
  vault_t5_tenant_id: "55555555-5555-5555-5555-555555555555"
  vault_t6_refresh:   "0.AXEAReplaceWithRealRefreshTokenForTenant6...AAA"
  vault_t6_tenant_id: "66666666-6666-6666-6666-666666666666"
  ```

- [ ] Cập nhật `.gitignore` (root repo) — thêm khối Ansible vào cuối file:
  ```gitignore
  # --- Ansible deploy (Plan 2/3) ---
  # Mật khẩu vault — KHÔNG commit
  .vault-pass
  deploy/.vault-pass
  # Vault đã mã hóa chứa secret thật — mặc định KHÔNG commit để tránh rò rỉ.
  # (Nếu team muốn commit bản ĐÃ MÃ HÓA, xóa dòng dưới và commit có chủ đích.)
  deploy/group_vars/all/vault.yml
  # Retry files
  *.retry
  deploy/*.retry
  ```

- [ ] Tạo file mật khẩu vault và một `vault.yml` mã hóa từ mẫu để verify round-trip (giá trị mẫu — sẽ thay bằng `ansible-vault edit` với secret thật):
  ```bash
  umask 077 && printf 'change-me-strong-vault-pass' > .vault-pass
  ansible-vault encrypt --vault-password-file .vault-pass \
    --output group_vars/all/vault.yml group_vars/all/vault.yml.example
  ```
  Expected: `Encryption successful`

- [ ] Verify đầu file `vault.yml` là header vault và view giải mã được:
  ```bash
  head -1 group_vars/all/vault.yml
  ansible-vault view --vault-password-file .vault-pass group_vars/all/vault.yml | head -3
  ```
  Expected:
  ```
  $ANSIBLE_VAULT;1.1;AES256
  ---
  # deploy/group_vars/all/vault.yml.example
  ```

- [ ] Verify gitignore thật sự chặn secret + mật khẩu (KHÔNG được trống):
  ```bash
  git -C "$(git rev-parse --show-toplevel)" check-ignore \
    deploy/.vault-pass deploy/group_vars/all/vault.yml
  ```
  Expected (cả 2 dòng được liệt kê = đang bị ignore):
  ```
  deploy/.vault-pass
  deploy/group_vars/all/vault.yml
  ```

- [ ] Verify `git status` KHÔNG thấy `vault.yml`/`.vault-pass` ở untracked:
  ```bash
  git status --porcelain deploy/ | grep -E 'vault\.yml$|\.vault-pass' || echo "clean: no secrets staged/untracked"
  ```
  Expected: `clean: no secrets staged/untracked`

- [ ] Commit (chỉ file an toàn — example + gitignore):
  ```bash
  git add deploy/group_vars/all/vault.yml.example
  git -C "$(git rev-parse --show-toplevel)" add .gitignore
  git commit -m "deploy: add vault.yml.example + gitignore vault secrets and .vault-pass"
  ```

---

### Task 7: `deploy/Makefile`

Bọc mọi thao tác Ansible. Tự thêm `--vault-password-file` khi `.vault-pass` tồn tại (qua `wildcard`), `--limit` khi có `LIMIT`. `deploy` rolling do `serial: 1` đặt trong `playbooks/deploy.yml` (phần Plan 2 kế tiếp). Các target gọi `playbooks/*.yml` / `playbooks/monitoring.yml` sẽ chạy được sau khi phần playbook/role/monitoring hoàn tất.

**Biến:**
- `TENANT` — id tenant (t1..t6) cho `restart/start/stop/logs` (bắt buộc với các target đó).
- `LIMIT` — giới hạn host/group cho `provision/deploy/status/verify` (vd `LIMIT=vps1`); trống = mọi host.
- `VAULT_PASS` — đường dẫn file mật khẩu vault (mặc định `.vault-pass`).
- `EXTRA` — cờ ansible bổ sung (vd `EXTRA="--check --diff"` để dry-run).

**Files:**
- Create `deploy/Makefile`

Steps:

- [ ] Tạo `deploy/Makefile` (chú ý: lệnh trong Makefile phải thụt bằng **TAB**):
  ```makefile
  # deploy/Makefile — wrapper mỏng quanh Ansible cho stack token-tool.
  # Chạy mọi target từ thư mục deploy/ (ansible.cfg ở đây trỏ inventory + roles).
  #
  # Biến: TENANT (t1..t6, cho restart/start/stop/logs), LIMIT (host/group),
  #       VAULT_PASS (file mật khẩu vault, def .vault-pass), EXTRA (cờ ansible thêm).
  SHELL := /bin/bash

  VAULT_PASS ?= .vault-pass
  TENANT     ?=
  LIMIT      ?=
  EXTRA      ?=

  # Chỉ thêm --vault-password-file khi file thật sự tồn tại (tránh lỗi khi chưa tạo vault).
  VAULT_FLAG := $(if $(wildcard $(VAULT_PASS)),--vault-password-file $(VAULT_PASS),)
  LIMIT_FLAG := $(if $(LIMIT),--limit $(LIMIT),)

  ANSIBLE          := ansible $(VAULT_FLAG)
  ANSIBLE_PLAYBOOK := ansible-playbook $(VAULT_FLAG)
  ANSIBLE_VAULT    := ansible-vault $(VAULT_FLAG)

  .DEFAULT_GOAL := help
  .PHONY: help provision deploy add-tenant status verify restart start stop logs \
          monitoring-up ping graph vault-view require-tenant

  help:                  ## Liệt kê target
  	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
  	  | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

  provision:             ## Bootstrap VPS (deps, user, build, units) — site.yml
  	$(ANSIBLE_PLAYBOOK) playbooks/site.yml $(LIMIT_FLAG) $(EXTRA)

  deploy:                ## Sync code + rebuild phần đổi + rolling restart (serial:1) — deploy.yml
  	$(ANSIBLE_PLAYBOOK) playbooks/deploy.yml $(LIMIT_FLAG) $(EXTRA)

  add-tenant:            ## Thêm/sửa tenant: sửa tenants.yml rồi chạy lại site.yml (idempotent)
  	$(ANSIBLE_PLAYBOOK) playbooks/site.yml $(LIMIT_FLAG) $(EXTRA)

  status:                ## systemctl is-active + curl /status mọi tenant
  	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "control_action=status" $(LIMIT_FLAG) $(EXTRA)

  verify:                ## Cổng kiểm tra sau deploy (units active, /status, source-IP, targets)
  	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "control_action=verify" $(LIMIT_FLAG) $(EXTRA)

  restart: require-tenant ## Restart cặp unit của TENANT=<id>
  	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "control_action=restart control_tenant=$(TENANT)" $(EXTRA)

  start: require-tenant   ## Start cặp unit của TENANT=<id>
  	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "control_action=start control_tenant=$(TENANT)" $(EXTRA)

  stop: require-tenant    ## Stop cặp unit của TENANT=<id>
  	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "control_action=stop control_tenant=$(TENANT)" $(EXTRA)

  logs: require-tenant    ## In journal gần nhất của TENANT=<id> (stream: dùng Grafana hoặc journalctl -f trên host)
  	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "control_action=logs control_tenant=$(TENANT)" $(EXTRA)

  monitoring-up:         ## (Plan 3) Dựng Grafana+Loki+Prometheus trên ops node — monitoring.yml
  	$(ANSIBLE_PLAYBOOK) playbooks/monitoring.yml --limit ops $(EXTRA)

  ping:                  ## Kiểm tra SSH tới host (mọi host, hoặc LIMIT=...)
  	$(ANSIBLE) all -m ping $(LIMIT_FLAG)

  graph:                 ## In cây inventory
  	ansible-inventory $(VAULT_FLAG) --graph

  vault-view:            ## Xem secret đã giải mã (không ghi ra đĩa)
  	$(ANSIBLE_VAULT) view group_vars/all/vault.yml

  require-tenant:
  	@if [ -z "$(TENANT)" ]; then \
  	  echo "ERROR: target này cần TENANT=<id> (vd: make restart TENANT=t1)"; \
  	  exit 2; \
  	fi
  ```

- [ ] Verify TAB-indent đúng (Makefile hỏng nếu dùng space):
  ```bash
  grep -nP '^\t' deploy/Makefile | head -1 && echo "TAB-indent OK"
  ```
  Expected: in một dòng recipe có TAB rồi `TAB-indent OK`.

- [ ] Verify `make help` liệt kê target:
  ```bash
  make -C deploy help
  ```
  Expected (màu ANSI có thể khác): danh sách gồm `provision`, `deploy`, `status`, `restart`, `start`, `stop`, `logs`, `verify`, `add-tenant`, `monitoring-up`, `ping`, `graph`, `vault-view`.

- [ ] Verify Makefile render đúng lệnh (dry-run, KHÔNG chạy — `make -n`):
  ```bash
  ( cd deploy && make -n provision )
  ```
  Expected (có `--vault-password-file` vì `.vault-pass` đã tồn tại):
  ```
  ansible-playbook --vault-password-file .vault-pass playbooks/site.yml
  ```

- [ ] Verify guard `TENANT`:
  ```bash
  ( cd deploy && make restart ); echo "exit=$?"
  ```
  Expected:
  ```
  ERROR: target này cần TENANT=<id> (vd: make restart TENANT=t1)
  exit=2
  ```

- [ ] Verify `make -C deploy graph` chạy (cần `--vault-password-file` vì đã có `vault.yml` mã hóa):
  ```bash
  make -C deploy graph
  ```
  Expected: cây inventory `@all → @app_vps(vps1,vps2,vps3) / @ops(ops)`.

- [ ] Commit:
  ```bash
  git add deploy/Makefile
  git commit -m "deploy: add Makefile wrapping ansible (provision/deploy/control/monitoring + vault/limit flags)"
  ```

---

### Task 8: Verify scaffold đầu-cuối & milestone

Kiểm tra liên kết toàn bộ scaffold (inventory ↔ tenants.yml ↔ vault) và đánh dấu cột mốc.

**Files:** (không tạo file; chỉ verify + tag)

Steps:

- [ ] Inventory + group_vars nạp đầy đủ (cần vault vì `vault.yml` mã hóa đã tồn tại):
  ```bash
  make -C deploy graph
  ```
  Expected: cây `@all → @app_vps(vps1/2/3) / @ops(ops)` không lỗi giải mã.

- [ ] `ansible_host` thật được template từ `app_vps` + secret giải mã được:
  ```bash
  make -C deploy vault-view | grep -E 'vault_t1_(refresh|tenant_id)'
  ansible $(test -f deploy/.vault-pass && echo --vault-password-file deploy/.vault-pass) \
    -i deploy/inventory/hosts.yml vps1 -m debug -a "var=ansible_host" -c local 2>/dev/null \
    || ansible-inventory -i deploy/inventory/hosts.yml --host vps1 | grep ansible_host
  ```
  Expected: liệt kê `vault_t1_refresh`/`vault_t1_tenant_id`, và `ansible_host` của `vps1` = `203.0.113.10` (lấy từ `app_vps` trong `tenants.yml`).

- [ ] SSH reachability (chỉ chạy khi đã điền IP/DNS thật + SSH key — với IP tài liệu sẽ UNREACHABLE, đó là dự kiến ở giai đoạn scaffold):
  ```bash
  make -C deploy ping
  ```
  Expected khi host sẵn sàng:
  ```
  vps1 | SUCCESS => { "changed": false, "ping": "pong" }
  vps2 | SUCCESS => { ... }
  vps3 | SUCCESS => { ... }
  ops  | SUCCESS => { ... }
  ```

- [ ] Xác nhận không có secret nào lọt vào git:
  ```bash
  git -C "$(git rev-parse --show-toplevel)" ls-files deploy/ | grep -E 'vault\.yml$|\.vault-pass' \
    && echo "FAIL: secret tracked" || echo "OK: no secret tracked"
  ```
  Expected: `OK: no secret tracked`

- [ ] Đánh dấu cột mốc scaffold:
  ```bash
  git tag -a deploy-scaffold -m "deploy/ scaffold: ansible.cfg, inventory, tenants.yml, vault, Makefile"
  git tag --list deploy-scaffold
  ```
  Expected: `deploy-scaffold`

---

---

## Roles `common` + `app` — code-prereq (log isolation), dependencies, service user, directories, build Go/Rust/venv, sync code

> **Bối cảnh kỹ thuật đã verify trong repo (đọc trước khi thực thi):**
> - `Get_Profile/go.mod` khai báo `module linkedin_fetcher` + `go 1.25.6`, `require golang.org/x/time v0.15.0` ⇒ cần Go toolchain **>= 1.25** (plan pin `1.25.6`). Build Linux: `go build -o get_profile .` trong `Get_Profile/` (tên `get_profile`, **không** `.exe`). `golang.org/x/time` fetch qua GOPROXY lúc build (app VPS có internet).
> - `email-gen/Cargo.toml`: Rust `edition = "2021"`, có `[profile.release]` (`lto = true`, `panic = "abort"`, `strip = true`). Build: `cargo build --release` ⇒ binary `email-gen/target/release/email-gen`. CLI `clap` derive với flags `-d/-u/-o/-c/-t/-b/--split/--gzip/--progress/--dedup/--format/-v` ⇒ `--help` exit 0 (an toàn để verify).
> - `Manage_User/requirements.txt` = `curl_cffi>=0.7.0`, `requests>=2.31.0`, `flask>=3.0.0`. Entry `python app.py`.
> - **Đã verify `Manage_User/app.py:31`**: `LOG_FILE = Path(__file__).parent / "service.log"` (cố định theo code dir, không `import os`). Vì mọi tenant `manageuser@<id>` chia sẻ `/opt/token-tool/Manage_User`, **tất cả** tenant trên một VPS sẽ ghi đè cùng `service.log` ⇒ log chen nhau. Đây là điều **Task A (code-prereq, TDD)** dưới đây sửa, trước khi sync code.
>
> **Quyết định công cụ (nêu rõ theo yêu cầu):**
> 1. **node_exporter**: cài qua apt package `prometheus-node-exporter` (KHÔNG tải tarball thủ công). Lý do: package có sẵn systemd unit (`prometheus-node-exporter.service`), chạy dưới system user riêng (least-privilege), apt tự cập nhật. Ta (a) override textfile-collector directory về `/var/lib/node_exporter/textfile` (đúng convention `Get_Profile --metrics-file`) và (b) **bind `--web.listen-address` về `127.0.0.1:9100` mặc định** để cổng metrics KHÔNG public ngay từ role này (đóng cửa sổ phơi nhiễm trước khi ufw của plan part security chạy). Ops node scrape qua SSH tunnel; nếu muốn scrape qua private network thì override `node_exporter_listen_addr` thành `<private_ip>:9100` và để plan part security thêm `ufw allow from <ops_ip>`.
> 2. **Rust**: cài qua `rustup` chính thức dưới user `tokentool`, gate bằng stat `~/.cargo/bin/cargo` (KHÔNG dùng `email-gen/run.sh` — nó `curl | sh` không idempotent và không pin toolchain). rustup pin `--default-toolchain stable --profile minimal`. Bootstrap `rustup-init.sh` là trust root không có checksum upstream ổn định ⇒ chấp nhận residual trust (rustup sau đó tự verify các component nó tải); installer được xóa sau khi cài.
>
> **Bí mật:** Plan part này **không render bí mật nào** (admin_token / refresh_token / tenant_id thuộc plan part config/tenant). Do đó yêu cầu `no_log`/token-render của brief **cố ý N/A** ở đây — không phải thiếu sót. Bí mật và `no_log: true` được áp ở plan part tenant.
>
> **Yêu cầu môi trường control node + connect user (preflight ở Task B):**
> - `ansible-core >= 2.16` (module `ansible.builtin.systemd_service`); collection `ansible.posix` (cho `synchronize`); `rsync` có trên control node **và** app VPS (app VPS cài qua role `common`).
> - Connect user (`ansible_user` trong inventory) có **passwordless sudo** (`NOPASSWD: ALL`). Điều này bao trùm cả `sudo rsync` (bắt buộc cho `synchronize --rsync-path="sudo rsync"`) lẫn `sudo systemctl`.

---

### Task 9: Code-prereq (TDD): `MANAGE_USER_LOG_FILE` override cho log isolation

> ⚠️ **Reconciliation #4 (đọc mục đầu plan):** phần sửa `app.py` đã được thực hiện ở **code-prereq Part C** bằng `_resolve_log_file()`. Ở Task này CHỈ đảm bảo `requirements-dev.txt` + `tests/conftest.py` tồn tại — **bỏ qua** bước sửa `app.py` và test trùng dưới đây.

> Bắt buộc theo brief: `app.py` phải đọc env `MANAGE_USER_LOG_FILE` (giữ default cũ khi env trống/không set), để mỗi tenant log riêng `/var/lib/token-tool/<id>/service.log` (env này do `<id>.env` của plan part tenant set). Làm theo TDD (pytest). Task này chạy **trước** mọi role Ansible để `app.py` được sync đã hỗ trợ env.

**Files:**
- Create: `Manage_User/requirements-dev.txt`
- Create: `Manage_User/tests/conftest.py`
- Create: `Manage_User/tests/test_log_file.py`
- Modify: `Manage_User/app.py`

Steps:

- [ ] Tạo `Manage_User/requirements-dev.txt` (nếu plan part code-changes/Plan 1 đã tạo file này thì chỉ cần đảm bảo có dòng `pytest`):

```text
pytest>=8.0.0
```

- [ ] Tạo `Manage_User/tests/conftest.py` (đảm bảo `import app` resolve được dù pytest gọi từ thư mục nào):

```python
"""Make the Manage_User package importable as top-level `app` for tests."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
```

- [ ] Tạo `Manage_User/tests/test_log_file.py` với **toàn bộ** nội dung sau (RED — sẽ fail vì `app.py` chưa đọc env):

```python
"""TDD contract: per-tenant log file via MANAGE_USER_LOG_FILE.

Each tenant unit (manageuser@<id>) sets MANAGE_USER_LOG_FILE to
/var/lib/token-tool/<id>/service.log so tenants sharing /opt/token-tool/Manage_User
never write the same service.log. These tests pin that contract.
"""
import importlib
from pathlib import Path

import app as app_module


def _reload():
    return importlib.reload(app_module)


def test_log_file_honours_env(monkeypatch, tmp_path):
    target = tmp_path / "tenant-t1" / "service.log"
    target.parent.mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", str(target))
    mod = _reload()
    assert mod.LOG_FILE == Path(str(target))


def test_log_file_defaults_when_unset(monkeypatch):
    monkeypatch.delenv("MANAGE_USER_LOG_FILE", raising=False)
    mod = _reload()
    assert mod.LOG_FILE == Path(mod.__file__).parent / "service.log"


def test_log_file_defaults_when_empty(monkeypatch):
    # Empty string must fall back to the historical default, not "" .
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "")
    mod = _reload()
    assert mod.LOG_FILE == Path(mod.__file__).parent / "service.log"
```

- [ ] Chạy test RED:
  `cd Manage_User && pip install -r requirements.txt -r requirements-dev.txt && python -m pytest tests/test_log_file.py -v`
  Expected: `test_log_file_honours_env` FAIL (`LOG_FILE` không phụ thuộc env), 2 test default PASS. Đây là trạng thái RED hợp lệ.

- [ ] Sửa `Manage_User/app.py` — thêm `import os` vào khối import (ngay sau `import logging`). Đổi:

```python
import json
import logging
import queue
import sys
```

  thành:

```python
import json
import logging
import os
import queue
import sys
```

- [ ] Sửa dòng `LOG_FILE` (dòng ~31). Đổi:

```python
CONFIG_FILE = Path(__file__).parent / "admin_token.json"
LOG_FILE = Path(__file__).parent / "service.log"
```

  thành:

```python
CONFIG_FILE = Path(__file__).parent / "admin_token.json"
# Per-tenant override: each manageuser@<id> sets MANAGE_USER_LOG_FILE to its own
# /var/lib/token-tool/<id>/service.log. Empty/unset → historical default (code dir).
LOG_FILE = Path(os.environ.get("MANAGE_USER_LOG_FILE") or (Path(__file__).parent / "service.log"))
```

- [ ] Chạy test GREEN + syntax check:
  `cd Manage_User && python -m pytest tests/test_log_file.py -v && python -m py_compile app.py`
  Expected: 3 PASSED; `py_compile` exit 0 không output.

- [ ] **Commit:**
  `git add Manage_User/app.py Manage_User/requirements-dev.txt Manage_User/tests/conftest.py Manage_User/tests/test_log_file.py && git commit -m "feat(manage_user): honor MANAGE_USER_LOG_FILE env for per-tenant log isolation"`

---

### Task 10: Preflight + wire roles `common` + `app` vào `site.yml`

**Files:**
- Modify: `deploy/playbooks/site.yml`

Steps:

- [ ] **Preflight control node** (chạy ở repo root):
  `ansible --version` → Expected: `core 2.16.x` trở lên. `command -v rsync` → Expected: in path rsync (vd `/usr/bin/rsync` hoặc `/opt/homebrew/bin/rsync`). Nếu thiếu rsync trên macOS: `brew install rsync`.

- [ ] Cài collection phụ thuộc (đã liệt kê trong `deploy/requirements.yml` ở plan part scaffold):
  `ansible-galaxy collection install -r deploy/requirements.yml`
  Expected: dòng `ansible.posix` được cài (hoặc `Nothing to do` nếu đã có).

- [ ] Bảo đảm play cho group `app_vps` trong `deploy/playbooks/site.yml` liệt kê đúng hai role + tags. Nếu plan part scaffold đã tạo `site.yml`, đảm bảo block dưới đây tồn tại (các play khác như `ops_host`/monitoring do plan part của chúng thêm). Nội dung play app_vps đầy đủ:

```yaml
---
- name: Provision app VPS (toolchains, application code, builds)
  hosts: app_vps
  become: true
  roles:
    - role: common
      tags: [common]
    - role: app
      tags: [app]
```

- [ ] Verify wiring (precondition cho các lệnh sau):
  `grep -nE 'hosts:\s*app_vps|role:\s*common|role:\s*app' deploy/playbooks/site.yml`
  Expected: thấy `hosts: app_vps`, `role: common`, `role: app`.

- [ ] **Commit:**
  `git add deploy/playbooks/site.yml deploy/requirements.yml && git commit -m "deploy(site): wire common+app roles to app_vps with tags"`

---

### Task 11: Role `common`: base packages, service user, directories, Go + Rust toolchains, node_exporter

> ⚠️ **Reconciliation #5:** `node_exporter` được sở hữu bởi role `observability` (Plan 3). Ở role `common` CHỈ giữ: cài package `prometheus-node-exporter` + tạo thư mục `/var/lib/node_exporter/textfile`. **Bỏ** task ghi `/etc/default/prometheus-node-exporter`, task enable/start node_exporter, handler `Restart node_exporter`, và các verify `127.0.0.1:9100`/sentinel (chúng do Plan 3 quản lý, listen `0.0.0.0:9100` + ufw-from-ops).

**Files:**
- Create: `deploy/roles/common/defaults/main.yml`
- Create: `deploy/roles/common/tasks/main.yml`
- Create: `deploy/roles/common/handlers/main.yml`

Steps:

- [ ] Tạo cây thư mục role: `mkdir -p deploy/roles/common/{defaults,tasks,handlers}` (từ repo root).

- [ ] Tạo `deploy/roles/common/defaults/main.yml` với **toàn bộ** nội dung sau. Đây là nguồn canonical cho path/identity dùng chung; role `app` chạy sau trong cùng play nên tham chiếu được (role defaults có hiệu lực toàn play).

```yaml
---
# ── Service identity ──────────────────────────────────────────────────────────
# Non-root system user/group that owns code, runs builds and all tenant units.
token_tool_user: tokentool
token_tool_group: tokentool
# HOME of the service user = code dir. Toolchain caches (Go: .cache/go-build,
# go/; Rust: .cargo, .rustup) live under HOME but OUTSIDE the three synced
# application subdirs, so the allowlist sync never touches them.
token_tool_home: /opt/token-tool

# ── Filesystem layout (canonical paths, reused by role `app`) ─────────────────
token_tool_code_dir: /opt/token-tool          # synced app trees + built binaries
token_tool_config_dir: /etc/token-tool        # admin_token.<id>.json, <id>.env
token_tool_state_dir: /var/lib/token-tool     # per-tenant <id>/ runtime state
# node_exporter textfile collector dir; Get_Profile writes getprofile_<id>.prom
node_exporter_textfile_dir: /var/lib/node_exporter/textfile

# ── node_exporter exposure (restricted by default) ────────────────────────────
# Default loopback-only: ops node scrapes via SSH tunnel. Override per-VPS to a
# private/mgmt IP (e.g. "10.0.0.5:9100") for private-network scraping; the
# security plan part then adds `ufw allow from <ops_ip> to any port 9100`.
node_exporter_listen_addr: "127.0.0.1:9100"

# ── Base apt packages ─────────────────────────────────────────────────────────
common_apt_packages:
  - python3
  - python3-venv
  - python3-pip
  - build-essential        # gcc/headers for native wheels (curl_cffi) + cgo
  - acl                    # setfacl: reliable become_user: tokentool (no world-readable tmp fallback)
  - curl
  - git
  - rsync                  # required by ansible.posix.synchronize on the target
  - ufw                    # firewall package; rules applied by the security plan part
  - jq
  - ca-certificates
  - prometheus-node-exporter

# ── Go toolchain (downloaded tarball → /usr/local/go) ─────────────────────────
# go.mod requires `go 1.25.6`. Override go_arch to arm64 on ARM VPS.
go_version: "1.25.6"
go_arch: amd64
go_install_root: /usr/local/go
go_tarball: "go{{ go_version }}.linux-{{ go_arch }}.tar.gz"
go_download_url: "https://go.dev/dl/{{ go_tarball }}"
go_download_dest: "/usr/local/src/{{ go_tarball }}"
# Pinned SHA-256 for reproducible/offline installs. Empty (default) → the
# official go{{version}}.linux-{{arch}}.tar.gz.sha256 is fetched on the TARGET
# host and format-validated before use. On a version bump, paste the published
# digest from https://go.dev/dl/ here to remove the runtime fetch entirely.
go_sha256: ""

# ── Rust toolchain (rustup, under the service user's HOME) ────────────────────
rust_toolchain: stable
rust_profile: minimal
cargo_home: "{{ token_tool_home }}/.cargo"
rustup_home: "{{ token_tool_home }}/.rustup"
rustup_init_url: "https://sh.rustup.rs"
rustup_init_dest: "{{ token_tool_home }}/rustup-init.sh"
```

- [ ] Tạo `deploy/roles/common/tasks/main.yml` với **toàn bộ** nội dung sau:

```yaml
---
# ── Base packages ─────────────────────────────────────────────────────────────
- name: Update apt cache (valid 1h to stay idempotent on immediate re-run)
  become: true
  ansible.builtin.apt:
    update_cache: true
    cache_valid_time: 3600

- name: Install base system packages (incl. acl for reliable become_user)
  become: true
  ansible.builtin.apt:
    name: "{{ common_apt_packages }}"
    state: present

# ── Service user & group ──────────────────────────────────────────────────────
- name: Ensure service group exists
  become: true
  ansible.builtin.group:
    name: "{{ token_tool_group }}"
    system: true

- name: Ensure service user exists (system, nologin, HOME = code dir)
  become: true
  ansible.builtin.user:
    name: "{{ token_tool_user }}"
    group: "{{ token_tool_group }}"
    system: true
    shell: /usr/sbin/nologin
    home: "{{ token_tool_home }}"
    create_home: true

# ── Directory layout ──────────────────────────────────────────────────────────
- name: Ensure code dir exists (owned by service user)
  become: true
  ansible.builtin.file:
    path: "{{ token_tool_code_dir }}"
    state: directory
    owner: "{{ token_tool_user }}"
    group: "{{ token_tool_group }}"
    mode: "0755"

- name: Ensure config dir exists (root-owned, group-readable by service user)
  become: true
  ansible.builtin.file:
    path: "{{ token_tool_config_dir }}"
    state: directory
    owner: root
    group: "{{ token_tool_group }}"
    mode: "0750"

- name: Ensure state dir exists (owned by service user)
  become: true
  ansible.builtin.file:
    path: "{{ token_tool_state_dir }}"
    state: directory
    owner: "{{ token_tool_user }}"
    group: "{{ token_tool_group }}"
    mode: "0750"

- name: Ensure node_exporter textfile parent dir exists
  become: true
  ansible.builtin.file:
    path: "{{ node_exporter_textfile_dir | dirname }}"
    state: directory
    owner: root
    group: root
    mode: "0755"

# Mode 0755: .prom files hold only non-secret operational counters
# (getprofile_* + tenant="<id>" label). World-read is acceptable and avoids a
# fragile group-membership task against the package's node_exporter user.
- name: Ensure node_exporter textfile dir exists (tokentool writes; node_exporter reads)
  become: true
  ansible.builtin.file:
    path: "{{ node_exporter_textfile_dir }}"
    state: directory
    owner: "{{ token_tool_user }}"
    group: "{{ token_tool_group }}"
    mode: "0755"

# ── Go toolchain ──────────────────────────────────────────────────────────────
- name: Detect installed Go version (read-only; runs in --check)
  become: true
  check_mode: false
  ansible.builtin.command: "{{ go_install_root }}/bin/go version"
  register: go_installed
  changed_when: false
  failed_when: false

- name: Decide whether Go needs (re)installing
  ansible.builtin.set_fact:
    go_needs_install: "{{ ('go' + go_version) not in (go_installed.stdout | default('')) }}"

- name: Ensure /usr/local/src exists for the Go tarball
  become: true
  ansible.builtin.file:
    path: /usr/local/src
    state: directory
    owner: root
    group: root
    mode: "0755"
  when: go_needs_install | bool

- name: Fetch official Go checksum file when no pinned digest is set
  become: true
  ansible.builtin.get_url:
    url: "{{ go_download_url }}.sha256"
    dest: "{{ go_download_dest }}.sha256"
    mode: "0644"
    force: true
  when:
    - go_needs_install | bool
    - go_sha256 | length == 0

- name: Read the fetched Go checksum file
  become: true
  ansible.builtin.slurp:
    src: "{{ go_download_dest }}.sha256"
  register: go_sha256_slurp
  when:
    - go_needs_install | bool
    - go_sha256 | length == 0

- name: Resolve the effective Go checksum (pinned var or fetched file)
  ansible.builtin.set_fact:
    go_checksum_effective: >-
      {{ go_sha256 if (go_sha256 | length > 0)
         else ((go_sha256_slurp.content | b64decode).split() | first) }}
  when: go_needs_install | bool

- name: Assert the resolved Go checksum is a 64-hex SHA-256
  ansible.builtin.assert:
    that:
      - go_checksum_effective is match('^[0-9a-f]{64}$')
    fail_msg: "Resolved Go checksum is not a 64-hex SHA-256 digest: '{{ go_checksum_effective }}'"
    quiet: true
  when: go_needs_install | bool

- name: Download Go tarball (SHA-256 verified)
  become: true
  ansible.builtin.get_url:
    url: "{{ go_download_url }}"
    dest: "{{ go_download_dest }}"
    checksum: "sha256:{{ go_checksum_effective }}"
    mode: "0644"
  when: go_needs_install | bool

- name: Remove previous Go install before re-extracting (version change only)
  become: true
  ansible.builtin.file:
    path: "{{ go_install_root }}"
    state: absent
  when: go_needs_install | bool

- name: Extract Go tarball into /usr/local
  become: true
  ansible.builtin.unarchive:
    src: "{{ go_download_dest }}"
    dest: /usr/local
    remote_src: true
    creates: "{{ go_install_root }}/bin/go"
  when: go_needs_install | bool

- name: Expose Go on the system PATH for interactive shells
  become: true
  ansible.builtin.copy:
    dest: /etc/profile.d/go.sh
    owner: root
    group: root
    mode: "0644"
    content: |
      export PATH="$PATH:{{ go_install_root }}/bin"

# ── Rust toolchain (rustup, under the service user) ───────────────────────────
- name: Detect installed Rust toolchain
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.stat:
    path: "{{ cargo_home }}/bin/cargo"
  register: cargo_bin

- name: Download rustup installer (residual trust accepted; rustup verifies components)
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.get_url:
    url: "{{ rustup_init_url }}"
    dest: "{{ rustup_init_dest }}"
    mode: "0755"
  when: not cargo_bin.stat.exists

- name: Install Rust toolchain via rustup (pinned stable/minimal)
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.command:
    cmd: "{{ rustup_init_dest }} -y --no-modify-path --profile {{ rust_profile }} --default-toolchain {{ rust_toolchain }}"
  environment:
    HOME: "{{ token_tool_home }}"
    CARGO_HOME: "{{ cargo_home }}"
    RUSTUP_HOME: "{{ rustup_home }}"
  register: rustup_install
  changed_when: rustup_install.rc == 0
  when: not cargo_bin.stat.exists

- name: Remove the rustup installer after toolchain install (cleanup HOME)
  become: true
  ansible.builtin.file:
    path: "{{ rustup_init_dest }}"
    state: absent
  when: not cargo_bin.stat.exists

- name: Expose cargo on the system PATH for interactive shells
  become: true
  ansible.builtin.copy:
    dest: /etc/profile.d/rust.sh
    owner: root
    group: root
    mode: "0644"
    content: |
      export PATH="$PATH:{{ cargo_home }}/bin"

# ── node_exporter: restrict listen addr + point textfile collector at our dir ─
- name: Configure node_exporter args (restricted listen addr + textfile dir)
  become: true
  ansible.builtin.copy:
    dest: /etc/default/prometheus-node-exporter
    owner: root
    group: root
    mode: "0644"
    content: |
      # Managed by Ansible (role: common). Do not edit by hand.
      # Bound to a restricted address so :9100 is never public from this role.
      # Override node_exporter_listen_addr per-VPS for private-network scraping;
      # the security plan part adds the matching ufw allow rule.
      ARGS="--web.listen-address={{ node_exporter_listen_addr }} --collector.textfile.directory={{ node_exporter_textfile_dir }}"
  notify: Restart node_exporter

- name: Ensure node_exporter is enabled and running
  become: true
  ansible.builtin.systemd_service:
    name: prometheus-node-exporter
    enabled: true
    state: started
```

- [ ] Tạo `deploy/roles/common/handlers/main.yml`:

```yaml
---
- name: Restart node_exporter
  become: true
  ansible.builtin.systemd_service:
    name: prometheus-node-exporter
    state: restarted
    daemon_reload: false
```

- [ ] **Dry-run (check + diff)** trên 1 host:
  `ansible-playbook -i deploy/inventory/hosts.yml deploy/playbooks/site.yml --tags common -l vps1 --check --diff`
  Expected: liệt kê thay đổi dự kiến (install packages gồm `acl`, tạo `tokentool`, các dir, `/etc/default/prometheus-node-exporter`); recap `failed=0`. Lưu ý đúng về Go: task detect chạy thật (`check_mode: false`) trả version thực; trên host **chưa** có Go, `go_needs_install=true` ⇒ các task download/remove/extract hiển thị **changed** (would change) chứ không skipped; trên host đã có Go đúng version, chúng hiển thị **skipped**. Tính idempotent được chứng minh bằng chạy thật + chạy lại, không phải bằng `--check`.

- [ ] **Chạy thật:**
  `ansible-playbook -i deploy/inventory/hosts.yml deploy/playbooks/site.yml --tags common -l vps1`
  Expected: recap `changed>0 failed=0 unreachable=0`.

- [ ] **Idempotence proof — chạy LẠI ngay (trong 1h để apt cache còn hạn):**
  `ansible-playbook -i deploy/inventory/hosts.yml deploy/playbooks/site.yml --tags common -l vps1`
  Expected: recap `changed=0 failed=0` (Go: `go_needs_install=false` ⇒ skip; Rust: `cargo` tồn tại ⇒ skip; node_exporter config không đổi ⇒ handler KHÔNG chạy).

- [ ] **Verify chức năng** (chạy trên vps1 qua SSH hoặc `ansible vps1 -m shell -a '...' -b`):
  - Go: `/usr/local/go/bin/go version` → `go version go1.25.6 linux/amd64`.
  - Rust: `sudo -u tokentool env HOME=/opt/token-tool /opt/token-tool/.cargo/bin/cargo --version` → in `cargo 1.<minor>.<patch> (...)`, exit 0.
  - rustup installer đã dọn: `test ! -e /opt/token-tool/rustup-init.sh && echo cleaned` → `cleaned`.
  - User: `getent passwd tokentool` → `...:/opt/token-tool:/usr/sbin/nologin`.
  - Dirs: `stat -c '%U %G %a %n' /opt/token-tool /etc/token-tool /var/lib/token-tool /var/lib/node_exporter/textfile` → `tokentool tokentool 755 /opt/token-tool`, `root tokentool 750 /etc/token-tool`, `tokentool tokentool 750 /var/lib/token-tool`, `tokentool tokentool 755 /var/lib/node_exporter/textfile`.
  - node_exporter active: `systemctl is-active prometheus-node-exporter` → `active`.
  - **Cổng bị hạn chế (chứng minh "cổng giám sát hạn chế")**: `ss -ltnp | grep -- ':9100'` → bound `127.0.0.1:9100` (KHÔNG phải `0.0.0.0:9100`). Từ một host khác: `curl -m 3 http://<vps1_public_ip>:9100/metrics` → connection refused/timeout.
  - **ARGS override thực sự áp dụng**: `pgrep -af prometheus-node-exporter | grep -- '--collector.textfile.directory=/var/lib/node_exporter/textfile'` → đúng 1 match.
  - **Textfile collector đọc được .prom (đường đúng + scrape không lỗi)**: thả sentinel rồi kiểm tra (cross-ref Plan 1: `Get_Profile --metrics-file` ghi `.prom` mode `0644` qua atomic tmp+rename để node_exporter — chạy dưới user khác — đọc được):
    ```
    sudo -u tokentool sh -c 'printf "sentinel_probe 1\n" > /var/lib/node_exporter/textfile/sentinel.prom; chmod 0644 /var/lib/node_exporter/textfile/sentinel.prom'
    curl -s http://127.0.0.1:9100/metrics | grep -E '^node_textfile_scrape_error 0$'
    curl -s http://127.0.0.1:9100/metrics | grep -E '^sentinel_probe 1$'
    sudo -u tokentool rm -f /var/lib/node_exporter/textfile/sentinel.prom
    ```
    Expected: cả `node_textfile_scrape_error 0` lẫn `sentinel_probe 1` đều in 1 dòng (đường thư mục đúng, quyền đọc OK).

- [ ] **Commit:**
  `git add deploy/roles/common && git commit -m "deploy(common): base packages, tokentool user, dirs, pinned Go+Rust toolchains, restricted node_exporter"`

---

### Task 12: Role `app`: sync code (allowlist), Python venv, Go + Rust builds, daemon-reload handler

**Files:**
- Create: `deploy/roles/app/defaults/main.yml`
- Create: `deploy/roles/app/tasks/main.yml`
- Create: `deploy/roles/app/handlers/main.yml`

Steps:

- [ ] Tạo cây thư mục role: `mkdir -p deploy/roles/app/{defaults,tasks,handlers}`.

- [ ] Tạo `deploy/roles/app/defaults/main.yml` với **toàn bộ** nội dung sau. Path identity (`token_tool_code_dir`, `token_tool_home`, `go_install_root`, `cargo_home`, ...) đến từ `common/defaults` (cùng play).

```yaml
---
# Source tree on the CONTROL node = repo root. Playbook lives at
# deploy/playbooks/site.yml ⇒ repo root = playbook_dir/../.. ; realpath makes it
# robust regardless of symlinks. A guard task asserts this is really the repo.
token_tool_src_dir: "{{ (playbook_dir + '/../..') | realpath }}"

# Only these three trees are ever transferred (allowlist sync). The chown task
# is scoped to exactly these, never the toolchain/cache dirs under HOME.
token_tool_synced_subdirs:
  - Manage_User
  - Get_Profile
  - email-gen

# Build sub-paths on the TARGET (derived from common's token_tool_code_dir).
manageuser_dir: "{{ token_tool_code_dir }}/Manage_User"
manageuser_venv: "{{ token_tool_code_dir }}/Manage_User/.venv"
getprofile_dir: "{{ token_tool_code_dir }}/Get_Profile"
getprofile_bin: "{{ token_tool_code_dir }}/Get_Profile/get_profile"
emailgen_dir: "{{ token_tool_code_dir }}/email-gen"
emailgen_bin: "{{ token_tool_code_dir }}/email-gen/target/release/email-gen"
```

- [ ] Tạo `deploy/roles/app/tasks/main.yml` với **toàn bộ** nội dung sau.
  Điểm mấu chốt idempotence/an toàn: (1) `synchronize` dùng **allowlist** (fail-closed) + `--no-owner --no-group` (không copy uid của control node ⇒ không churn ownership) + `--rsync-path="sudo rsync"` (ghi vào cây `tokentool`-owned mà KHÔNG dùng `become` trên module); (2) chown chỉ chạy khi `code_sync.changed`, scope đúng 3 subdir; (3) builds gate trên `code_sync.changed or not <bin>.stat.exists`.

```yaml
---
# ── Guard: confirm the control-node source root really is the repo root ────────
- name: Probe the resolved source root for a known repo file
  delegate_to: localhost
  become: false
  run_once: true
  ansible.builtin.stat:
    path: "{{ token_tool_src_dir }}/Get_Profile/go.mod"
  register: repo_root_probe

- name: Fail fast if the resolved source root is wrong
  run_once: true
  ansible.builtin.assert:
    that:
      - repo_root_probe.stat.exists
    fail_msg: >-
      token_tool_src_dir={{ token_tool_src_dir }} does not look like the repo
      root (Get_Profile/go.mod missing). Fix the playbook layout before syncing.
    quiet: true

# ── Sync repo → /opt/token-tool (ALLOWLIST: only the three app trees) ──────────
# archive:true gives -rlptgoD; --no-owner/--no-group strip -o/-g so the control
# node's uid is never propagated (kills the ownership churn that breaks
# idempotence). delete:false so toolchain caches (.cargo/.rustup/go/.cache) and
# the venv are never wiped. become:false (override the play) + --rsync-path so the
# REMOTE rsync runs as root via passwordless sudo and can write the tokentool dir.
- name: Sync application code to the app VPS (allowlist, clean trees only)
  become: false
  ansible.posix.synchronize:
    src: "{{ token_tool_src_dir }}/"
    dest: "{{ token_tool_code_dir }}/"
    archive: true
    delete: false
    rsync_opts:
      - "--rsync-path=sudo rsync"
      - "--no-owner"
      - "--no-group"
      # build artifacts / runtime / caches — listed BEFORE the allowlist so they win
      - "--exclude=/Manage_User/.venv"
      - "--exclude=/Manage_User/service.log"
      - "--exclude=/Manage_User/admin_token*.json"
      - "--exclude=/Get_Profile/get_profile"
      - "--exclude=/Get_Profile/*.exe"
      - "--exclude=/email-gen/target"
      - "--exclude=**/__pycache__/"
      - "--exclude=*.pyc"
      - "--exclude=*.log"
      - "--exclude=*.ckpt"
      - "--exclude=*.gz"
      # allowlist: only these three trees are transferred
      - "--include=/Manage_User/***"
      - "--include=/Get_Profile/***"
      - "--include=/email-gen/***"
      # everything else at the repo root (aa.txt, token-api/, CHECK_CONNECTION/,
      # deploy/, .git/, *.env, *.pem, …) is refused
      - "--exclude=*"
  register: code_sync
  notify: Reload systemd

- name: Normalise ownership of the synced application subdirs to the service user
  become: true
  ansible.builtin.file:
    path: "{{ token_tool_code_dir }}/{{ item }}"
    state: directory
    owner: "{{ token_tool_user }}"
    group: "{{ token_tool_group }}"
    recurse: true
  loop: "{{ token_tool_synced_subdirs }}"
  when: code_sync.changed

# ── Defence in depth: the allowlist is fail-closed, but assert no secret leaked ─
- name: Scan the synced tree for secret-shaped files
  become: true
  ansible.builtin.find:
    paths: "{{ token_tool_code_dir }}"
    patterns:
      - "admin_token*.json"
      - "*.env"
      - "*.pem"
      - "*.key"
      - "*.p12"
      - "id_rsa*"
      - "vault*"
    recurse: true
    file_type: file
  register: leaked_secrets

- name: Fail if any secret-shaped file is present under the code dir
  ansible.builtin.assert:
    that:
      - leaked_secrets.matched == 0
    fail_msg: >-
      Secret-shaped files found under {{ token_tool_code_dir }}:
      {{ leaked_secrets.files | map(attribute='path') | list }}.
      The allowlist sync must not transfer these — clean the control-node tree.
    quiet: true

# ── Manage_User: virtualenv + dependencies ────────────────────────────────────
- name: Create the Manage_User virtualenv
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.command:
    cmd: "python3 -m venv {{ manageuser_venv }}"
    creates: "{{ manageuser_venv }}/bin/python"
  register: venv_created

- name: Install Manage_User Python dependencies
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.pip:
    requirements: "{{ manageuser_dir }}/requirements.txt"
    virtualenv: "{{ manageuser_venv }}"
    state: present
  register: pip_install
  when: code_sync.changed or venv_created.changed
  notify: Reload systemd

# ── Get_Profile: Go build (only when source changed or binary missing) ────────
- name: Stat the Get_Profile binary
  ansible.builtin.stat:
    path: "{{ getprofile_bin }}"
  register: getprofile_bin_stat

- name: Build the Get_Profile Go binary
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.command:
    cmd: "{{ go_install_root }}/bin/go build -o get_profile ."
    chdir: "{{ getprofile_dir }}"
  environment:
    HOME: "{{ token_tool_home }}"
    PATH: "{{ go_install_root }}/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    GOCACHE: "{{ token_tool_home }}/.cache/go-build"
    GOPATH: "{{ token_tool_home }}/go"
  register: getprofile_build
  changed_when: getprofile_build.rc == 0
  when: code_sync.changed or not getprofile_bin_stat.stat.exists
  notify: Reload systemd

# ── email-gen: Rust release build (only when source changed or binary missing) ─
- name: Stat the email-gen binary
  ansible.builtin.stat:
    path: "{{ emailgen_bin }}"
  register: emailgen_bin_stat

- name: Build the email-gen Rust binary (release)
  become: true
  become_user: "{{ token_tool_user }}"
  ansible.builtin.command:
    cmd: "{{ cargo_home }}/bin/cargo build --release"
    chdir: "{{ emailgen_dir }}"
  environment:
    HOME: "{{ token_tool_home }}"
    PATH: "{{ cargo_home }}/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    CARGO_HOME: "{{ cargo_home }}"
    RUSTUP_HOME: "{{ rustup_home }}"
  register: emailgen_build
  changed_when: emailgen_build.rc == 0
  when: code_sync.changed or not emailgen_bin_stat.stat.exists
  notify: Reload systemd

# ── Export a play-scoped flag for the Part C tenant role's per-instance restart ─
# The tenant role (Plan 3) reads token_tool_code_changed and restarts ONLY this
# host's own manageuser@<id>/getprofile@<id> instances (per-tenant, no host-wide
# blast radius). emailgen@<id> is intentionally never auto-restarted.
- name: Record that application code or binaries changed (consumed by Part C tenant role)
  ansible.builtin.set_fact:
    token_tool_code_changed: true
  when: >-
    code_sync.changed
    or (venv_created is defined and venv_created.changed)
    or (pip_install is defined and pip_install.changed)
    or (getprofile_build is defined and getprofile_build.changed)
    or (emailgen_build is defined and emailgen_build.changed)
```

- [ ] Tạo `deploy/roles/app/handlers/main.yml`. Plan 2 KHÔNG cài unit tenant nào, nên handler này chỉ reload systemd manager config (vô hại). **Việc restart từng tenant thuộc Part C (role tenant)** (tenant role có handler restart riêng theo từng instance, lặp trên các tenant của chính host đó qua `tenants.yml`, dùng `token_tool_code_changed` ở trên) — KHÔNG dùng glob restart toàn host để tránh bounce nhầm tenant khác. `emailgen@<id>` (Type=oneshot) **cố ý không bao giờ auto-restart**: regenerate `emails.txt` đổi SHA-256 fingerprint ⇒ vô hiệu bitmap checkpoint của Get_Profile.

```yaml
---
# Reusable daemon-reload. Reused by the Part C tenant role after it installs the
# manageuser@/getprofile@/emailgen@ template units. No shell here → no dash
# pipefail footgun, no host-wide restart blast radius.
- name: Reload systemd
  become: true
  ansible.builtin.systemd_service:
    daemon_reload: true
```

- [ ] **Dry-run (check + diff)**:
  `ansible-playbook -i deploy/inventory/hosts.yml deploy/playbooks/site.yml --tags app -l vps1 --check --diff`
  Expected: guard assert pass; `synchronize` (chạy `--dry-run` trong check mode) liệt kê file sẽ transfer dưới `Manage_User/`, `Get_Profile/`, `email-gen/`; `failed=0`. Lưu ý: các task build/venv là `command` ⇒ **skipped** trong `--check` (command không chạy ở check mode); lần chạy thật sẽ thực thi. Đây là kỳ vọng đúng, không phải lỗi.

- [ ] **Chạy thật:**
  `ansible-playbook -i deploy/inventory/hosts.yml deploy/playbooks/site.yml --tags app -l vps1`
  Expected: `code_sync` changed; chown 3 subdir chạy; secret-scan assert pass (`matched == 0`); venv created; pip installed; hai build chạy (`changed_when: rc == 0`); recap `changed>0 failed=0`. Handler `Reload systemd` chạy 1 lần (daemon-reload, an toàn dù chưa có unit tenant).

- [ ] **Idempotence proof — chạy LẠI y hệt:**
  `ansible-playbook -i deploy/inventory/hosts.yml deploy/playbooks/site.yml --tags app -l vps1`
  Expected: recap `changed=0 failed=0`. `code_sync` không changed (allowlist + `--no-owner/--no-group` ⇒ không churn) ⇒ chown skip; pip skip (`venv_created`/`code_sync` đều không changed); hai build skip (binary đã tồn tại); handler KHÔNG chạy; `token_tool_code_changed` không set.

- [ ] **Verify chức năng** (trên vps1):
  - Go binary: `file /opt/token-tool/Get_Profile/get_profile` → `ELF 64-bit ... executable`; `stat -c '%U' /opt/token-tool/Get_Profile/get_profile` → `tokentool`.
  - Rust binary: `sudo -u tokentool env HOME=/opt/token-tool /opt/token-tool/email-gen/target/release/email-gen --help` → in usage (cờ `-d/-u/-o/-c/-t/-b/--split/--gzip/--progress/--dedup/--format/-v`), exit 0.
  - venv + deps: `/opt/token-tool/Manage_User/.venv/bin/python -c "import flask, requests, curl_cffi; print('deps-ok')"` → `deps-ok`.
  - **Allowlist sạch (không VCS, không Ansible, không stray)**: `for p in deploy .git aa.txt token-api CHECK_CONNECTION; do test ! -e /opt/token-tool/$p; done && echo allowlist-clean` → `allowlist-clean`.
  - **Toolchain caches còn nguyên (delete:false)**: `test -x /opt/token-tool/.cargo/bin/cargo && echo caches-preserved` → `caches-preserved`.
  - **Per-tenant log isolation (chứng minh Task A đã được sync vào VPS)**:
    ```
    sudo -u tokentool env HOME=/opt/token-tool MANAGE_USER_LOG_FILE=/var/lib/token-tool/probe.log \
      sh -c 'cd /opt/token-tool/Manage_User && .venv/bin/python -c "import app; print(app.LOG_FILE)"'
    # Expected: /var/lib/token-tool/probe.log
    sudo -u tokentool env HOME=/opt/token-tool \
      sh -c 'cd /opt/token-tool/Manage_User && .venv/bin/python -c "import app; print(app.LOG_FILE)"'
    # Expected: /opt/token-tool/Manage_User/service.log
    sudo -u tokentool rm -f /var/lib/token-tool/probe.log /opt/token-tool/Manage_User/service.log
    ```
    Hai lệnh in đúng hai đường khác nhau ⇒ mỗi tenant (qua `MANAGE_USER_LOG_FILE` trong `<id>.env`) sẽ log riêng `/var/lib/token-tool/<id>/service.log`, không chen vào file chung.

- [ ] **Commit:**
  `git add deploy/roles/app && git commit -m "deploy(app): allowlist sync, secret-scan, venv, Go+Rust builds, reusable Reload systemd handler"`

---

**Idempotence — tổng kết cơ chế (cho reviewer):**
- `common`: `apt` (state present) idempotent; `update_cache` có `cache_valid_time:3600` ⇒ re-run ngay = không update; `user`/`group`/`file` declarative; Go gate bằng fact `go_needs_install` (`'go'+version not in go version` thật, đo qua task `check_mode:false`) + `unarchive creates:`; Rust gate bằng `stat ~/.cargo/bin/cargo` (download/install/cleanup đều `when: not cargo_bin.stat.exists` ⇒ không re-download installer); node_exporter config qua `copy` content-compare (handler chỉ chạy khi nội dung đổi). Re-run ⇒ `changed=0`.
- `app`: `synchronize` allowlist + `--no-owner --no-group` ⇒ không transfer & không churn ownership khi code không đổi; `chown` scope 3 subdir, gate `code_sync.changed`; secret-scan `find` + `assert` luôn `ok` (read-only); `venv` qua `creates:`; `pip` gate `code_sync.changed or venv_created.changed`; hai build gate `code_sync.changed or not <bin>.stat.exists`. Re-run không đổi code ⇒ mọi chown/pip/build skip; handler không chạy ⇒ `changed=0`.

**Phụ thuộc liên-plan đã mã hóa rõ:**
- Connect user cần **passwordless sudo** (bao gồm `sudo rsync`, `sudo systemctl`) — preflight ở Task B; thiếu nó thì `synchronize --rsync-path="sudo rsync"` và mọi `become` fail ngay lần đầu.
- `token_tool_code_changed` (set ở `app`) là hook để **Part C tenant role** restart per-instance đúng tenant của host; `Reload systemd` handler được Plan 3 tái sử dụng sau khi cài template units `manageuser@/getprofile@/emailgen@`.
- Cross-ref Plan 1: `Get_Profile --metrics-file` phải ghi `.prom` mode `0644` (atomic tmp+rename) để node_exporter (chạy user khác) đọc được — đã verify ở Task C bằng sentinel + `node_textfile_scrape_error 0`.

---

## Phần Plan 2 — Code-prereq log env + Ansible role `tenant` + playbooks + verify

Phần này giả định các phần khác của Plan 2 đã tạo: inventory `deploy/inventory/hosts.yml` (group `app_vps` chứa các host **đặt tên đúng** `vps1`, `vps2`, `vps3` để `inventory_hostname` khớp `tenant.vps`; group `ops`), `deploy/group_vars/all/tenants.yml` (biến `app_vps`, `ops_host`, `tenants`), `deploy/group_vars/all/vault.yml` (mã hóa ansible-vault, chứa `vault_<id>_refresh` v.v.), file `deploy/.vault-pass`, và các role `common` + `app` (tạo user `tokentool`, dir `/opt/token-tool`, `/etc/token-tool`, `/var/lib/token-tool`, `/var/lib/node_exporter/textfile`, build venv + binary). Role `tenant` ở đây render config per-tenant và khởi động cặp process. Plan 1 đã thêm các flag `--license-sku`, `--usage-location` (app.py) và `--metrics-file` (Get_Profile) mà các unit dưới đây tham chiếu.

---

### Task 13: Code-prereq (TDD): `MANAGE_USER_LOG_FILE` env override trong `app.py`

Mỗi tenant chạy cùng code dir `/opt/token-tool/Manage_User` nhưng phải ghi log riêng. Hiện `app.py` cố định `LOG_FILE = Path(__file__).parent / "service.log"` ⇒ các tenant chen log vào cùng file. Ta tách `_resolve_log_file()` (đọc env `MANAGE_USER_LOG_FILE`, default giữ nguyên hành vi cũ nếu env trống) để test thuần bằng pytest theo TDD (RED → GREEN).

**Files:**
- Create `Manage_User/tests/test_log_file_env.py`
- Modify `Manage_User/app.py`

Steps:

- [ ] (RED) Tạo file test `Manage_User/tests/test_log_file_env.py` với nội dung đầy đủ:

```python
"""Tests for the MANAGE_USER_LOG_FILE env override in app._resolve_log_file.

The function is pure (reads os.environ at call time), so we can assert its
return value directly with monkeypatched env. Importing app.py runs a
module-level logging.basicConfig() that opens a FileHandler at import time;
we point that at a throwaway path *before* importing so the test never writes
a stray service.log into the source tree.
"""
import os
from pathlib import Path

# Must be set BEFORE `import app` so the import-time FileHandler is harmless.
_IMPORT_LOG = Path(__file__).resolve().parent / "_import_service.log"
os.environ["MANAGE_USER_LOG_FILE"] = str(_IMPORT_LOG)

import app  # noqa: E402  (env must be configured before import)


def test_resolve_uses_env_override(monkeypatch):
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "/var/lib/token-tool/t1/service.log")
    assert app._resolve_log_file() == Path("/var/lib/token-tool/t1/service.log")


def test_resolve_falls_back_when_unset(monkeypatch):
    monkeypatch.delenv("MANAGE_USER_LOG_FILE", raising=False)
    expected = Path(app.__file__).parent / "service.log"
    assert app._resolve_log_file() == expected


def test_resolve_falls_back_when_blank(monkeypatch):
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "   ")
    expected = Path(app.__file__).parent / "service.log"
    assert app._resolve_log_file() == expected


def test_resolve_strips_surrounding_whitespace(monkeypatch):
    monkeypatch.setenv("MANAGE_USER_LOG_FILE", "  /var/lib/token-tool/t2/service.log  ")
    assert app._resolve_log_file() == Path("/var/lib/token-tool/t2/service.log")
```

- [ ] (RED) Chạy test để xác nhận đỏ (hàm chưa tồn tại):

```bash
cd Manage_User && pip install -r requirements.txt pytest >/dev/null && python -m pytest tests/test_log_file_env.py -v
```

Expected: 4 test **fail** với `AttributeError: module 'app' has no attribute '_resolve_log_file'`.

- [ ] (GREEN) Thêm `import os` vào khối import của `Manage_User/app.py`. Đổi dòng:

```python
import json
import logging
import queue
```

thành:

```python
import json
import logging
import os
import queue
```

- [ ] (GREEN) Thay block định nghĩa `LOG_FILE` trong `Manage_User/app.py`. Đổi:

```python
CONFIG_FILE = Path(__file__).parent / "admin_token.json"
LOG_FILE = Path(__file__).parent / "service.log"
```

thành:

```python
CONFIG_FILE = Path(__file__).parent / "admin_token.json"


def _resolve_log_file() -> Path:
    """Resolve the service log file path.

    Honors the ``MANAGE_USER_LOG_FILE`` environment variable so each tenant
    process can log to its own state dir (e.g.
    ``/var/lib/token-tool/<id>/service.log``). Falls back to the module
    directory's ``service.log`` when the variable is unset or blank, preserving
    the previous single-tenant default.
    """
    override = os.environ.get("MANAGE_USER_LOG_FILE", "").strip()
    if override:
        return Path(override)
    return Path(__file__).parent / "service.log"


LOG_FILE = _resolve_log_file()
```

- [ ] (GREEN) Chạy lại test, xác nhận xanh:

```bash
cd Manage_User && python -m pytest tests/test_log_file_env.py -v
```

Expected: `4 passed`.

- [ ] (Regression) Smoke-check module vẫn compile sạch:

```bash
cd Manage_User && python -m py_compile app.py && echo OK
```

Expected: in ra `OK`, không lỗi.

- [ ] Commit:

```bash
git add Manage_User/app.py Manage_User/tests/test_log_file_env.py
git commit -m "feat(manage_user): MANAGE_USER_LOG_FILE env for per-tenant logging

Extract _resolve_log_file(): reads MANAGE_USER_LOG_FILE, defaults to
<app dir>/service.log when unset/blank. Enables one code dir to serve
multiple tenants writing to /var/lib/token-tool/<id>/service.log.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

> Risk note (ngoài phạm vi task này, cần file riêng): `Manage_User/admin_token_manager.py::save_if_updated()` ghi refresh_token đã xoay vào hằng `CONFIG_FILE = Path(__file__).parent / "admin_token.json"` (fixed code-dir), **không** ghi vào `--config` per-tenant. Đa tenant sẽ ghi đè lẫn nhau và file `/etc/token-tool/admin_token.<id>.json` không nhận token mới. Đề nghị mở một code-prereq riêng (cùng pattern: truyền `config_path` vào `AdminTokenManager`) — KHÔNG xử lý ở đây để giữ scope.

---

### Task 14: Role `tenant`: templates (admin_token, env, email-gen inputs, systemd units)

Tạo toàn bộ template Jinja2 của role. Các template dùng biến vòng lặp `item` (một phần tử của `tenants`). Token chỉ xuất hiện trong `admin_token.json.j2`; task render nó đặt `no_log: true`.

**Files:**
- Create `deploy/roles/tenant/templates/admin_token.json.j2`
- Create `deploy/roles/tenant/templates/tenant.env.j2`
- Create `deploy/roles/tenant/templates/domains.txt.j2`
- Create `deploy/roles/tenant/templates/usernames.txt.j2`
- Create `deploy/roles/tenant/templates/manageuser@.service.j2`
- Create `deploy/roles/tenant/templates/getprofile@.service.j2`
- Create `deploy/roles/tenant/templates/emailgen@.service.j2`

Steps:

- [ ] Tạo `deploy/roles/tenant/templates/admin_token.json.j2` (JSON mảng đúng 1 phần tử, khớp `admin_token.tenant1.example.json`; `refresh_token` từ `item.admin.refresh_token` — bản thân nó là `"{{ vault_<id>_refresh }}"` trong `tenants.yml` nên Ansible tự resolve lồng):

```jinja
[
  {
    "refresh_token": "{{ item.admin.refresh_token }}",
    "tenant_id": "{{ item.admin.tenant_id }}",
    "username": "{{ item.admin.username }}",
    "domain": "{{ item.admin.domain }}",
    "local_ip": "{{ item.local_ip }}"
  }
]
```

- [ ] Tạo `deploy/roles/tenant/templates/tenant.env.j2` (EnvironmentFile cho cả 2 unit; mọi giá trị không chứa khoảng trắng nên không cần quote):

```jinja
# Managed by Ansible (role: tenant) — do not edit by hand.
# Tenant: {{ item.id }}  (VPS: {{ item.vps }})
MANAGEUSER_PORT={{ item.manageuser_port }}
LOCAL_IP={{ item.local_ip }}
LICENSE_SKU={{ item.license_sku | default('a1-students') }}
USAGE_LOCATION={{ item.usage_location | default('US') }}
MANAGE_USER_LOG_FILE=/var/lib/token-tool/{{ item.id }}/service.log
EMAILS=/var/lib/token-tool/{{ item.id }}/emails.txt
RESULT=/var/lib/token-tool/{{ item.id }}/result.txt
CHECKPOINT=/var/lib/token-tool/{{ item.id }}/{{ item.id }}.ckpt
METRICS_FILE=/var/lib/node_exporter/textfile/getprofile_{{ item.id }}.prom
API_ADDR=http://127.0.0.1:{{ item.manageuser_port }}
WORKERS={{ item.workers | default(400) }}
MAX_CPM={{ item.max_cpm | default(20000) }}
WORKDIR=/var/lib/token-tool/{{ item.id }}
```

- [ ] Tạo `deploy/roles/tenant/templates/domains.txt.j2` (một domain mỗi dòng; dựa vào `trim_blocks=true` mặc định của Ansible template để không sinh dòng trống):

```jinja
{% for d in item.emailgen.domains %}
{{ d }}
{% endfor %}
```

- [ ] Tạo `deploy/roles/tenant/templates/usernames.txt.j2` (hỗ trợ cả `emailgen.usernames` dạng list lẫn `emailgen.usernames_file` dạng path trên control node):

```jinja
{% if item.emailgen.usernames is defined %}
{% for u in item.emailgen.usernames %}
{{ u }}
{% endfor %}
{% else %}
{{ lookup('file', item.emailgen.usernames_file) }}
{% endif %}
```

- [ ] Tạo `deploy/roles/tenant/templates/manageuser@.service.j2` (template unit; `%i` = tenant id; biến từ `EnvironmentFile`):

```ini
[Unit]
Description=Manage_User API service (tenant %i)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=tokentool
Group=tokentool
EnvironmentFile=/etc/token-tool/%i.env
WorkingDirectory=/var/lib/token-tool/%i
ExecStart=/opt/token-tool/Manage_User/.venv/bin/python /opt/token-tool/Manage_User/app.py \
  --host 0.0.0.0 \
  --port ${MANAGEUSER_PORT} \
  --config /etc/token-tool/admin_token.%i.json \
  --local-ip ${LOCAL_IP} \
  --license-sku ${LICENSE_SKU} \
  --usage-location ${USAGE_LOCATION}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

- [ ] Tạo `deploy/roles/tenant/templates/getprofile@.service.j2` (phụ thuộc cả `manageuser@%i` lẫn `emailgen@%i`; `--id %i` + `--metrics-file` từ Plan 1):

```ini
[Unit]
Description=Get_Profile batch fetcher (tenant %i)
After=network-online.target manageuser@%i.service emailgen@%i.service
Wants=network-online.target manageuser@%i.service
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=tokentool
Group=tokentool
EnvironmentFile=/etc/token-tool/%i.env
WorkingDirectory=/var/lib/token-tool/%i
ExecStart=/opt/token-tool/Get_Profile/get_profile \
  --api ${API_ADDR} \
  --local-ip ${LOCAL_IP} \
  --emails ${EMAILS} \
  --result ${RESULT} \
  --checkpoint ${CHECKPOINT} \
  --workers ${WORKERS} \
  --max-cpm ${MAX_CPM} \
  --id %i \
  --metrics-file ${METRICS_FILE}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

- [ ] Tạo `deploy/roles/tenant/templates/emailgen@.service.j2` (oneshot; `RemainAfterExit=yes` để `state: started` idempotent; chạy email-gen sinh `emails.txt`):

```ini
[Unit]
Description=email-gen generate emails.txt (tenant %i)
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
User=tokentool
Group=tokentool
EnvironmentFile=/etc/token-tool/%i.env
WorkingDirectory=/var/lib/token-tool/%i
ExecStart=/opt/token-tool/email-gen/target/release/email-gen \
  -d /var/lib/token-tool/%i/inputs/domains.txt \
  -u /var/lib/token-tool/%i/inputs/usernames.txt \
  -o /var/lib/token-tool/%i/emails.txt

[Install]
WantedBy=multi-user.target
```

- [ ] Verify cú pháp template (render thử bằng `template` lookup không cần host) — chỉ kiểm tra Jinja2 parse:

```bash
cd deploy && python -c "import jinja2,glob,sys; [jinja2.Environment().parse(open(f).read()) for f in glob.glob('roles/tenant/templates/*.j2')]; print('jinja2 parse OK')"
```

Expected: in ra `jinja2 parse OK` (không exception). Idempotence + functional thực sự được chứng minh ở task tasks/main.yml + handlers tiếp theo (vì template chưa được apply nếu không có tasks).

- [ ] Commit:

```bash
git add deploy/roles/tenant/templates/
git commit -m "feat(deploy): tenant role templates (admin/env/inputs/systemd units)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 15: Role `tenant`: tasks/main.yml + restart.yml + handler (render + enable/start, idempotent)

`tasks/main.yml` lặp đúng các tenant thuộc host hiện tại (`tenants | selectattr('vps','equalto', inventory_hostname)`), render config/inputs/unit, daemon-reload qua handler, rồi enable+start `emailgen@` → `manageuser@` → `getprofile@`. Restart per-tenant được điều khiển bằng "changed ids" (đăng ký kết quả render) để giữ tính idempotent: lần chạy lại không có gì đổi ⇒ `changed=0`. `restart.yml` dùng cho rolling update của `deploy.yml`.

**Files:**
- Create `deploy/roles/tenant/tasks/main.yml`
- Create `deploy/roles/tenant/tasks/restart.yml`
- Create `deploy/roles/tenant/handlers/main.yml`

Steps:

- [ ] Tạo `deploy/roles/tenant/handlers/main.yml`:

```yaml
---
# roles/tenant/handlers/main.yml
- name: Reload systemd
  ansible.builtin.systemd_service:
    daemon_reload: true
```

- [ ] Tạo `deploy/roles/tenant/tasks/main.yml` với nội dung đầy đủ:

```yaml
---
# roles/tenant/tasks/main.yml
# Render per-tenant config + units and start the tenant's process pair.
# Runs only the tenants whose `vps` matches this host (inventory_hostname),
# so naming inventory hosts vps1/vps2/vps3 to match tenants[].vps is required.

- name: Compute tenants assigned to this host
  ansible.builtin.set_fact:
    host_tenants: "{{ tenants | selectattr('vps', 'equalto', inventory_hostname) | list }}"

- name: Ensure per-tenant state directory exists
  ansible.builtin.file:
    path: "/var/lib/token-tool/{{ item.id }}"
    state: directory
    owner: tokentool
    group: tokentool
    mode: "0750"
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"

- name: Ensure per-tenant email-gen inputs directory exists
  ansible.builtin.file:
    path: "/var/lib/token-tool/{{ item.id }}/inputs"
    state: directory
    owner: tokentool
    group: tokentool
    mode: "0750"
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"

- name: Render admin_token.<id>.json (contains secrets)
  ansible.builtin.template:
    src: admin_token.json.j2
    dest: "/etc/token-tool/admin_token.{{ item.id }}.json"
    owner: tokentool
    group: tokentool
    mode: "0600"
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  no_log: true
  register: admin_render

- name: Render <id>.env
  ansible.builtin.template:
    src: tenant.env.j2
    dest: "/etc/token-tool/{{ item.id }}.env"
    owner: root
    group: tokentool
    mode: "0640"
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  register: env_render

- name: Render email-gen inputs/domains.txt
  ansible.builtin.template:
    src: domains.txt.j2
    dest: "/var/lib/token-tool/{{ item.id }}/inputs/domains.txt"
    owner: tokentool
    group: tokentool
    mode: "0640"
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  register: domains_render

- name: Render email-gen inputs/usernames.txt
  ansible.builtin.template:
    src: usernames.txt.j2
    dest: "/var/lib/token-tool/{{ item.id }}/inputs/usernames.txt"
    owner: tokentool
    group: tokentool
    mode: "0640"
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  register: usernames_render

- name: Install systemd unit templates
  ansible.builtin.template:
    src: "{{ item }}.j2"
    dest: "/etc/systemd/system/{{ item }}"
    owner: root
    group: root
    mode: "0644"
  loop:
    - manageuser@.service
    - getprofile@.service
    - emailgen@.service
  notify: Reload systemd
  register: unit_render

- name: Apply pending daemon-reload before touching units
  ansible.builtin.meta: flush_handlers

- name: Compute tenant ids whose email-gen inputs changed
  ansible.builtin.set_fact:
    emailgen_changed_ids: >-
      {{ (domains_render.results | selectattr('changed') | map(attribute='item.id') | list)
         + (usernames_render.results | selectattr('changed') | map(attribute='item.id') | list)
         | unique }}

- name: Compute tenant ids whose env changed
  ansible.builtin.set_fact:
    env_changed_ids: "{{ env_render.results | selectattr('changed') | map(attribute='item.id') | list }}"

- name: Compute tenant ids whose admin token changed
  ansible.builtin.set_fact:
    admin_changed_ids: "{{ admin_render.results | selectattr('changed') | map(attribute='item.id') | list }}"

- name: Compute restart sets
  ansible.builtin.set_fact:
    manageuser_restart_ids: "{{ (env_changed_ids + admin_changed_ids) | unique }}"
    getprofile_restart_ids: "{{ env_changed_ids }}"

- name: Enable + start emailgen@<id> (generate emails.txt)
  ansible.builtin.systemd_service:
    name: "emailgen@{{ item.id }}.service"
    enabled: true
    state: started
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"

- name: Regenerate emails.txt when email-gen inputs changed
  ansible.builtin.systemd_service:
    name: "emailgen@{{ item.id }}.service"
    state: restarted
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  when: item.id in emailgen_changed_ids

- name: Enable + start manageuser@<id>
  ansible.builtin.systemd_service:
    name: "manageuser@{{ item.id }}.service"
    enabled: true
    state: started
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"

- name: Restart manageuser@<id> when its env or admin token changed
  ansible.builtin.systemd_service:
    name: "manageuser@{{ item.id }}.service"
    state: restarted
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  when: item.id in manageuser_restart_ids

- name: Enable + start getprofile@<id>
  ansible.builtin.systemd_service:
    name: "getprofile@{{ item.id }}.service"
    enabled: true
    state: started
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"

- name: Restart getprofile@<id> when its env changed
  ansible.builtin.systemd_service:
    name: "getprofile@{{ item.id }}.service"
    state: restarted
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
  when: item.id in getprofile_restart_ids
```

- [ ] Tạo `deploy/roles/tenant/tasks/restart.yml` (dùng bởi `deploy.yml` sau khi build code mới):

```yaml
---
# roles/tenant/tasks/restart.yml
# Restart this host's tenant service pair after a code update (rolling deploy).
- name: Compute tenants assigned to this host
  ansible.builtin.set_fact:
    host_tenants: "{{ tenants | selectattr('vps', 'equalto', inventory_hostname) | list }}"

- name: Restart manageuser@<id> after code update
  ansible.builtin.systemd_service:
    name: "manageuser@{{ item.id }}.service"
    state: restarted
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"

- name: Restart getprofile@<id> after code update
  ansible.builtin.systemd_service:
    name: "getprofile@{{ item.id }}.service"
    state: restarted
  loop: "{{ host_tenants }}"
  loop_control:
    label: "{{ item.id }}"
```

- [ ] (Dry-run) Chạy `--check --diff` giới hạn role tenant (site.yml có tags ở task playbooks bên dưới):

```bash
cd deploy && ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/site.yml --tags tenant --check --diff
```

Expected: với fleet chưa có config, mỗi tenant hiện diff tạo `/etc/token-tool/admin_token.<id>.json` (nội dung bị ẩn do `no_log`), `<id>.env`, inputs, 3 unit; tổng kết `changed=N` (>0), `failed=0`.

- [ ] (Apply thật) Chạy thật:

```bash
cd deploy && ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/site.yml --tags tenant
```

Expected: `changed>0`, `failed=0`; các unit được enable + start.

- [ ] (Idempotent proof) Chạy LẠI ngay:

```bash
cd deploy && ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/site.yml --tags tenant
```

Expected dòng PLAY RECAP: `changed=0` cho mọi host (vd `vps1 : ok=18 changed=0 unreachable=0 failed=0`). Các task render → ok, "Reload systemd" handler không kích hoạt, restart-on-change skipped, `state: started` no-op vì unit đã active.

- [ ] (Functional) Trên một app VPS, kiểm cặp unit của tenant `t1` đang active và `/status` trả 200:

```bash
ansible vps1 -i deploy/inventory/hosts.yml -b -a "systemctl is-active manageuser@t1 getprofile@t1"
ansible vps1 -i deploy/inventory/hosts.yml -b -a "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:5000/status"
```

Expected: hai dòng `active` cho lệnh đầu; `200` cho lệnh sau (sau khi queue đầy ≥100 token).

- [ ] Commit:

```bash
git add deploy/roles/tenant/tasks/ deploy/roles/tenant/handlers/
git commit -m "feat(deploy): tenant role tasks (render + idempotent enable/start)

Loop per host's tenants; daemon-reload via handler+flush; restart only on
config change via changed-id sets. restart.yml for rolling deploy.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 16: Playbooks: `site.yml`, `deploy.yml`, `control.yml`

`site.yml` hội tụ toàn fleet. `deploy.yml` rolling update code từng VPS (`serial: 1`) rồi restart cặp process của VPS đó. `control.yml` start/stop/restart/status theo tenant qua `-e action=...`.

**Files:**
- Create `deploy/playbooks/site.yml`
- Create `deploy/playbooks/deploy.yml`
- Create `deploy/playbooks/control.yml`

Steps:

- [ ] Tạo `deploy/playbooks/site.yml` (roles `common`,`app`,`tenant`,`observability` cho app VPS; `monitoring` cho ops — các role không thuộc phần này do Plan 2/Plan 3 khác cấp; tags để chạy chọn lọc):

```yaml
---
# deploy/playbooks/site.yml — full convergence of the whole fleet.
- name: App VPS — base, build, per-tenant config + services, local observability
  hosts: app_vps
  become: true
  roles:
    - { role: common, tags: [common, base] }
    - { role: app, tags: [app, build] }
    - { role: tenant, tags: [tenant] }
    - { role: observability, tags: [observability] }   # Plan 3 (node_exporter textfile)

- name: Ops node — monitoring stack (Grafana + Grafana Loki + Prometheus)
  hosts: ops
  become: true
  roles:
    - { role: monitoring, tags: [monitoring] }          # Plan 3 (docker-compose)
```

- [ ] Tạo `deploy/playbooks/deploy.yml` (rolling code update, một VPS một lúc):

```yaml
---
# deploy/playbooks/deploy.yml — rolling code update, one app VPS at a time.
# Rebuild code via role `app`, then restart this VPS's tenant pairs.
- name: Rolling update of app code (one VPS at a time)
  hosts: app_vps
  become: true
  serial: 1
  any_errors_fatal: true
  max_fail_percentage: 0
  roles:
    - { role: app, tags: [app, build] }
  post_tasks:
    - name: Restart this VPS's tenant services after code update
      ansible.builtin.include_role:
        name: tenant
        tasks_from: restart.yml
```

- [ ] Tạo `deploy/playbooks/control.yml` (operate cặp process theo tenant; `action` ∈ start|stop|restart|status; `tenant=all` hoặc một id):

```yaml
---
# deploy/playbooks/control.yml — operate tenant service pairs.
# Usage:
#   ansible-playbook playbooks/control.yml -e action=start
#   ansible-playbook playbooks/control.yml -e "action=restart tenant=t1"
#   ansible-playbook playbooks/control.yml -e action=status
- name: Control tenant service pairs
  hosts: app_vps
  become: true
  gather_facts: false
  vars:
    action: status
    tenant: all
  tasks:
    - name: Validate action
      ansible.builtin.assert:
        that:
          - action in ['start', 'stop', 'restart', 'status']
        fail_msg: "action must be one of start|stop|restart|status (got '{{ action }}')"

    - name: Compute target tenants on this host
      ansible.builtin.set_fact:
        target_tenants: >-
          {{ (tenants | selectattr('vps', 'equalto', inventory_hostname)
                      | selectattr('id', 'equalto', tenant) | list)
             if tenant != 'all'
             else (tenants | selectattr('vps', 'equalto', inventory_hostname) | list) }}

    - name: "{{ action | capitalize }} manageuser@<id>"
      ansible.builtin.systemd_service:
        name: "manageuser@{{ item.id }}.service"
        state: "{{ {'start': 'started', 'stop': 'stopped', 'restart': 'restarted'}[action] }}"
      loop: "{{ target_tenants }}"
      loop_control:
        label: "{{ item.id }}"
      when: action in ['start', 'stop', 'restart']

    - name: "{{ action | capitalize }} getprofile@<id>"
      ansible.builtin.systemd_service:
        name: "getprofile@{{ item.id }}.service"
        state: "{{ {'start': 'started', 'stop': 'stopped', 'restart': 'restarted'}[action] }}"
      loop: "{{ target_tenants }}"
      loop_control:
        label: "{{ item.id }}"
      when: action in ['start', 'stop', 'restart']

    # --- status path ---
    - name: Query systemd is-active for each unit in the pair
      ansible.builtin.command:
        cmd: "systemctl is-active {{ item.0 }}@{{ item.1.id }}.service"
      loop: "{{ ['manageuser', 'getprofile'] | product(target_tenants) | list }}"
      loop_control:
        label: "{{ item.0 }}@{{ item.1.id }}"
      register: active_check
      changed_when: false
      failed_when: false
      when: action == 'status'

    - name: Query Manage_User /status HTTP endpoint
      ansible.builtin.uri:
        url: "http://127.0.0.1:{{ item.manageuser_port }}/status"
        method: GET
        return_content: true
        status_code: [200, 202]
      loop: "{{ target_tenants }}"
      loop_control:
        label: "{{ item.id }}"
      register: status_http
      failed_when: false
      when: action == 'status'

    - name: Print systemd status report
      ansible.builtin.debug:
        msg: "{{ item.cmd | join(' ') }} -> {{ item.stdout }}"
      loop: "{{ active_check.results | default([]) }}"
      loop_control:
        label: "{{ item.item.0 }}@{{ item.item.1.id }}"
      when: action == 'status'

    - name: Print HTTP /status report
      ansible.builtin.debug:
        msg: >-
          {{ item.item.id }} port={{ item.item.manageuser_port }}
          http={{ item.status | default('n/a') }}
          json={{ item.json | default('n/a') }}
      loop: "{{ status_http.results | default([]) }}"
      loop_control:
        label: "{{ item.item.id }}"
      when: action == 'status'
```

- [ ] (Dry-run cú pháp) Kiểm cả 3 playbook parse + syntax:

```bash
cd deploy && for p in site.yml deploy.yml control.yml; do \
  ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass playbooks/$p --syntax-check; done
```

Expected: mỗi playbook in `playbook: playbooks/<p>` không lỗi.

- [ ] (Functional) Chạy `control.yml action=status` đọc trạng thái (read-only, `changed=0`):

```bash
cd deploy && ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/control.yml -e action=status
```

Expected: các dòng debug `manageuser@t1.service -> active`, `getprofile@t1.service -> active`, và `t1 port=5000 http=200 json={...'queue_size':...}`; PLAY RECAP `changed=0`.

- [ ] (Functional) Test stop rồi start một tenank để xác nhận điều khiển đúng cặp:

```bash
cd deploy && ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/control.yml -e "action=stop tenant=t1" && \
  ansible vps1 -i inventory/hosts.yml -b -a "systemctl is-active manageuser@t1 getprofile@t1" ; \
  ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/control.yml -e "action=start tenant=t1"
```

Expected: sau `stop`, `is-active` in `inactive` (x2, exit khác 0 nhưng bước này chỉ minh hoạ); sau `start`, tenant `t1` chạy lại.

- [ ] Commit:

```bash
git add deploy/playbooks/site.yml deploy/playbooks/deploy.yml deploy/playbooks/control.yml
git commit -m "feat(deploy): site/deploy/control playbooks

site=full converge; deploy=serial rolling code update + restart; control=
start|stop|restart|status per tenant via -e action.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 17: Verify: `verify.yml` + `deploy/Makefile` (`make verify`)

`verify.yml` chạy trên mọi app VPS: assert cặp unit `active`, `/status` trả 200 đúng shape, **source IP** outbound của `get_profile` đúng `local_ip` (qua `ss -tnp`), và file metrics node_exporter tươi (<120s). `deploy/Makefile` gói các entry point operator (`make check|site|deploy|verify|start|stop|restart|status`).

**Files:**
- Create `deploy/playbooks/verify.yml`
- Create `deploy/Makefile`

Steps:

- [ ] Tạo `deploy/playbooks/verify.yml` (gather_facts true để có `ansible_date_time.epoch` so mtime):

```yaml
---
# deploy/playbooks/verify.yml — post-deploy functional verification.
- name: Verify tenant deployments on each app VPS
  hosts: app_vps
  become: true
  gather_facts: true
  tasks:
    - name: Compute target tenants on this host
      ansible.builtin.set_fact:
        host_tenants: "{{ tenants | selectattr('vps', 'equalto', inventory_hostname) | list }}"

    - name: manageuser@<id> is active
      ansible.builtin.command: "systemctl is-active manageuser@{{ item.id }}.service"
      register: mu_active
      changed_when: false
      loop: "{{ host_tenants }}"
      loop_control:
        label: "{{ item.id }}"

    - name: Assert manageuser active
      ansible.builtin.assert:
        that: "item.stdout == 'active'"
        fail_msg: "manageuser@{{ item.item.id }} is {{ item.stdout }}"
        success_msg: "manageuser@{{ item.item.id }} active"
      loop: "{{ mu_active.results }}"
      loop_control:
        label: "{{ item.item.id }}"

    - name: getprofile@<id> is active
      ansible.builtin.command: "systemctl is-active getprofile@{{ item.id }}.service"
      register: gp_active
      changed_when: false
      loop: "{{ host_tenants }}"
      loop_control:
        label: "{{ item.id }}"

    - name: Assert getprofile active
      ansible.builtin.assert:
        that: "item.stdout == 'active'"
        fail_msg: "getprofile@{{ item.item.id }} is {{ item.stdout }}"
        success_msg: "getprofile@{{ item.item.id }} active"
      loop: "{{ gp_active.results }}"
      loop_control:
        label: "{{ item.item.id }}"

    - name: GET /status returns 200 with expected keys
      ansible.builtin.uri:
        url: "http://127.0.0.1:{{ item.manageuser_port }}/status"
        method: GET
        return_content: true
        status_code: 200
      register: status_http
      loop: "{{ host_tenants }}"
      loop_control:
        label: "{{ item.id }}"

    - name: Assert /status payload shape
      ansible.builtin.assert:
        that:
          - "'queue_size' in item.json"
          - "'total_tokens' in item.json"
          - "'total_deleted' in item.json"
        fail_msg: "Bad /status payload for {{ item.item.id }}: {{ item.json }}"
        success_msg: "{{ item.item.id }} /status ok (queue_size={{ item.json.queue_size }})"
      loop: "{{ status_http.results }}"
      loop_control:
        label: "{{ item.item.id }}"

    - name: Capture get_profile outbound sockets (source IP check)
      ansible.builtin.shell: "ss -H -tnp 2>/dev/null | grep get_profile || true"
      register: ss_out
      changed_when: false

    - name: Assert each tenant's local_ip appears as a get_profile source address
      ansible.builtin.assert:
        that: "item.local_ip in ss_out.stdout"
        fail_msg: >-
          No get_profile socket bound to {{ item.local_ip }} for {{ item.id }}.
          ss snapshot:
          {{ ss_out.stdout }}
        success_msg: "{{ item.id }}: get_profile egress bound to {{ item.local_ip }}"
      loop: "{{ host_tenants }}"
      loop_control:
        label: "{{ item.id }}"

    - name: Stat node_exporter textfile metrics
      ansible.builtin.stat:
        path: "/var/lib/node_exporter/textfile/getprofile_{{ item.id }}.prom"
      register: metric_stat
      loop: "{{ host_tenants }}"
      loop_control:
        label: "{{ item.id }}"

    - name: Assert metrics file exists and is fresh (< 120s old)
      ansible.builtin.assert:
        that:
          - item.stat.exists
          - (ansible_date_time.epoch | int) - (item.stat.mtime | int) < 120
        fail_msg: >-
          Metrics file getprofile_{{ item.item.id }}.prom missing or stale
          (exists={{ item.stat.exists }}, age={{ (ansible_date_time.epoch | int) - (item.stat.mtime | int) }}s).
        success_msg: >-
          {{ item.item.id }} metrics fresh
          (age={{ (ansible_date_time.epoch | int) - (item.stat.mtime | int) }}s)
      loop: "{{ metric_stat.results }}"
      loop_control:
        label: "{{ item.item.id }}"
```

- [ ] Tạo `deploy/Makefile` (chạy từ thư mục `deploy/`):

```makefile
# deploy/Makefile — operator entry points for the token-tool fleet.
# Run all targets from the deploy/ directory.

ANSIBLE_PLAYBOOK ?= ansible-playbook
INVENTORY        ?= inventory/hosts.yml
VAULT_FLAG       ?= --vault-password-file .vault-pass

.PHONY: check site deploy verify start stop restart status

check:   ## Dry-run full convergence (--check --diff)
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/site.yml --check --diff

site:    ## Full convergence of the whole fleet
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/site.yml

deploy:  ## Rolling code update, one VPS at a time
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/deploy.yml

verify:  ## Post-deploy functional verification
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/verify.yml

start:   ## Start all tenant pairs (override: TENANT=t1)
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/control.yml -e action=start -e tenant=$(or $(TENANT),all)

stop:    ## Stop all tenant pairs (override: TENANT=t1)
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/control.yml -e action=stop -e tenant=$(or $(TENANT),all)

restart: ## Restart all tenant pairs (override: TENANT=t1)
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/control.yml -e action=restart -e tenant=$(or $(TENANT),all)

status:  ## Show systemd + HTTP /status for all tenant pairs
	$(ANSIBLE_PLAYBOOK) -i $(INVENTORY) $(VAULT_FLAG) playbooks/control.yml -e action=status
```

- [ ] (Syntax) Kiểm verify.yml parse:

```bash
cd deploy && ansible-playbook -i inventory/hosts.yml --vault-password-file .vault-pass \
  playbooks/verify.yml --syntax-check
```

Expected: in `playbook: playbooks/verify.yml`, không lỗi.

- [ ] (Functional) Chạy `make verify`:

```bash
cd deploy && make verify
```

Expected: mọi assert `ok` (vd `t1: get_profile egress bound to 203.0.113.10`, `t1 /status ok (queue_size=120)`, `t1 metrics fresh (age=8s)`); PLAY RECAP `changed=0 failed=0`. Nếu fail, message in rõ tenant + lý do (unit không active / sai source IP / metrics stale).

- [ ] (Functional bổ trợ) Kiểm thủ công source IP trên một VPS để đối chiếu assert `ss`:

```bash
ansible vps1 -i deploy/inventory/hosts.yml -b -a "ss -H -tnp | grep get_profile | head -5"
```

Expected output (mẫu — cột local address là `local_ip` của tenant):

```text
ESTAB 0 0 203.0.113.10:51234 13.107.42.12:443 users:(("get_profile",pid=8123,fd=45))
ESTAB 0 0 203.0.113.10:51250 13.107.42.12:443 users:(("get_profile",pid=8123,fd=46))
```

(Với tenant IPv6, local address sẽ là `[2001:db8::10]:…`.) Xác nhận source = `local_ip`, không phải IP mặc định của VPS ⇒ binding per-tenant đúng.

- [ ] Commit:

```bash
git add deploy/playbooks/verify.yml deploy/Makefile
git commit -m "feat(deploy): verify.yml + Makefile (make verify)

Assert unit active, /status shape, get_profile source IP == local_ip (ss),
node_exporter textfile freshness. Makefile wraps check/site/deploy/verify/
start/stop/restart/status.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---
---

## Self-Review (Plan 2)

**Spec coverage:**
- Nguồn cấu hình duy nhất `tenants.yml` + vault → Part A. ✅
- Build trên VPS (Go/Rust/venv) + sync code allowlist → Part B. ✅
- systemd template units (`manageuser@`/`getprofile@`/`emailgen@`) + EnvironmentFile + WorkingDirectory per-tenant + source-IP binding giữ nguyên → Part C. ✅
- email-gen sinh `emails.txt` trên VPS (oneshot) → Part C. ✅
- Quản lý Makefile (provision/deploy/status/restart/verify) + rolling `serial:1` → Part A + Part C. ✅
- Code-prereq log isolation + admin-token-path → Code prerequisites + Part C. ✅
- License/usage flags (Plan 1) tham chiếu trong unit → Part C `manageuser@.service`. ✅

**Consistency (đã chốt ở mục Quyết định hợp nhất):** inventory `hosts.yml`; một Makefile; `action`/`tenant`; log code-prereq một lần; node_exporter do Plan 3 sở hữu.

**Lưu ý cho người thực thi:** số dòng trong các bước sửa code là gần đúng — khớp theo nội dung hiển thị, không theo số dòng. Chạy `python -m pytest` sau mỗi code-prereq.
