# Multi-VPS — Observability Plan (Plan 3/3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.
>
> **Language skills (CLAUDE.md):** `docker-patterns`, `deployment-patterns`, `security-review`.

**Goal:** Dựng stack giám sát tập trung (Grafana + Grafana Loki + Prometheus + json-exporter) trên ops node, và agent quan sát (node_exporter + Grafana Alloy) trên 3 app VPS, để gom log + metrics + dashboard cho cả 6 tenant.

**Architecture:** ops node chạy docker-compose (Prometheus pull node_exporter `:9100` của mỗi app VPS lấy metric `getprofile_*` từ textfile; json-exporter fetch `/status` mỗi tenant → metric `manageuser_*`; Grafana Loki nhận log do Alloy đẩy từ journald với label `{vps,tenant,app,unit}`). Mọi cổng ops bind `127.0.0.1` (SSH tunnel). app VPS hạn chế cổng bằng ufw chỉ cho ops.

**Tech Stack:** docker-compose (Grafana 11.4 / Grafana Loki 3.3 / Prometheus 2.55 / json-exporter 0.6), prometheus-node-exporter (textfile collector), Grafana Alloy, ufw, ansible (community.docker).

**Prereq:** Plan 1 (Get_Profile `--metrics-file`) + Plan 2 (inventory, `tenants.yml`, vault, role `common` tạo textfile dir, role `tenant` đặt `--metrics-file=/var/lib/node_exporter/textfile/getprofile_<id>.prom`).

> **Thuật ngữ:** "Grafana Loki" (hệ log) KHÁC endpoint "Loki" của app (`eur.loki.delve.office.com`).

---

## ⚠️ Quyết định hợp nhất (authoritative — đọc TRƯỚC)

Plan gồm 2 Part: A (stack giám sát trên ops) + B (agent trên app VPS + dashboard + e2e). Quyết định khi divergence:

1. **Inventory** = `deploy/inventory/hosts.yml`. Lệnh ghi `-i inventory` → dùng `-i inventory/hosts.yml`.
2. **requirements.yml** đã tạo ở Plan 2 Part A — Part A (monitoring) chỉ đảm bảo có `community.docker (>=3.10)`; hợp nhất version cao hơn vào file Plan 2.
3. **Playbook monitoring** canonical = `deploy/playbooks/monitoring.yml` (khớp Makefile `monitoring-up`). Khi Part A ghi `deploy/monitoring.yml` → tạo tại `deploy/playbooks/monitoring.yml`; sửa `monitoring_src` trong `roles/monitoring/defaults/main.yml` thành `"{{ playbook_dir }}/../monitoring"`.
4. **json-exporter** sở hữu bởi ops node (container, Part A). app VPS mở `manageuser_port` chỉ cho `ops_ip`.
5. **Firewall app VPS** sở hữu bởi role `observability` (Part B `firewall.yml`): bỏ rule `7979`; thêm rule mở mỗi `manageuser_port` của tenant trên host CHỈ từ `ops_ip`. **Bỏ** 2 task ufw `delegate_to` app VPS trong role `monitoring` (Part A). Đánh đổi: `/tokens/next` reachable từ ops (chấp nhận; hardening: json-exporter local / reverse-proxy chỉ `/status`).
6. **node_exporter** listen `0.0.0.0:9100` + ufw-from-ops, do role `observability` sở hữu (khớp Reconciliation #5 của Plan 2). Khớp Prometheus target `<vps_ip>:9100`.
7. **Prometheus job names** = `prometheus`, `linkedin_nodes`, `manageuser_status` (Part A). Script verify e2e (Part B) đã được vá để dùng `linkedin_nodes`/`manageuser_status`.
8. **vault:** thêm `vault_grafana_admin_password` vào `group_vars/all/vault.yml` (+ mục mẫu trong `vault.yml.example`) trước khi chạy `monitoring.yml`.
9. **Hai dashboard** (`token-tool-overview.json` ở Part A + `linkedin-fleet.json` ở Part B) đều được provisioned — giữ cả hai.

---

## Phần A — Stack giám sát trên ops node

Phần này triển khai stack giám sát chạy bằng `docker compose` **chỉ trên ops node** (VPS thứ 4, always-on, KHÔNG chạy tenant). Tất cả service bind `127.0.0.1` (truy cập qua SSH tunnel, không phơi ra Internet). Prometheus trên ops scrape `node_exporter` (textfile collector → metric `getprofile_*`) ở mỗi app VPS, và scrape `/status` của từng tenant qua `json-exporter`. Grafana Loki (hệ log) dùng để gom log; lưu ý "Grafana Loki" KHÁC endpoint "Loki" (`eur.loki.delve.office.com`) của app.

Giả định inventory (do Plan 2 dựng) có group `app_vps` với host tên `vps1`/`vps2`/`vps3` và group `ops` với host `ops`, cùng các biến `app_vps`, `ops_host`, `tenants` trong `deploy/group_vars/all/tenants.yml`, và secret `vault_grafana_admin_password` trong `deploy/group_vars/all/vault.yml`. Việc cài + cấu hình `node_exporter` trên app VPS thuộc **role `observability` (Plan 3, xem Reconciliation #6)** — KHÔNG phải role `app`; role `common` (Plan 2) chỉ cài package + tạo thư mục textfile. Phần ops này chỉ cấu hình scrape + mở firewall tới ops.

---

### Task 1: Bootstrap thư mục `deploy/monitoring` và `docker-compose.yml`

**Files:**
- Create `deploy/monitoring/docker-compose.yml`
- Create `deploy/monitoring/.gitignore`

- [ ] Tạo cây thư mục source-of-truth cho stack:
  ```bash
  mkdir -p deploy/monitoring/prometheus \
           deploy/monitoring/loki \
           deploy/monitoring/json-exporter \
           deploy/monitoring/grafana/provisioning/datasources \
           deploy/monitoring/grafana/provisioning/dashboards \
           deploy/monitoring/grafana/dashboards
  ```
- [ ] Tạo `deploy/monitoring/.gitignore` (không bao giờ commit secret `.env` được render trên ops host):
  ```gitignore
  # Rendered on the ops host by Ansible (contains the Grafana admin password)
  .env
  ```
- [ ] Tạo `deploy/monitoring/docker-compose.yml` với NỘI DUNG ĐẦY ĐỦ. Mọi image pin tag cố định; mọi port bind `127.0.0.1` (chỉ vào được qua SSH tunnel); `restart: unless-stopped`; data nằm trên named volume; config mount read-only.
  ```yaml
  name: token-tool-monitoring

  # SECURITY: every published port is bound to 127.0.0.1 on the ops host, so
  # Grafana/Prometheus/Loki/json-exporter are NOT reachable from the Internet.
  # Operators reach them via an SSH tunnel, e.g. from a laptop:
  #   ssh -N -L 3000:127.0.0.1:3000 -L 9090:127.0.0.1:9090 <user>@<ops_ip>
  # then browse http://localhost:3000 (Grafana) / http://localhost:9090 (Prometheus).
  # json-exporter is reached by Prometheus over the internal compose network
  # (service DNS name "json-exporter"); the 127.0.0.1:7979 publish is for manual probing only.

  networks:
    monitoring:
      driver: bridge

  volumes:
    prometheus-data:
    grafana-data:
    loki-data:

  services:
    prometheus:
      image: prom/prometheus:v2.55.1
      container_name: monitoring-prometheus
      restart: unless-stopped
      command:
        - --config.file=/etc/prometheus/prometheus.yml
        - --storage.tsdb.path=/prometheus
        - --storage.tsdb.retention.time=15d
        - --web.enable-lifecycle
        - --web.listen-address=0.0.0.0:9090
      volumes:
        - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
        - prometheus-data:/prometheus
      ports:
        - "127.0.0.1:9090:9090"
      networks:
        - monitoring

    loki:
      image: grafana/loki:3.3.0
      container_name: monitoring-loki
      restart: unless-stopped
      command: -config.file=/etc/loki/local-config.yaml
      volumes:
        - ./loki/loki-config.yml:/etc/loki/local-config.yaml:ro
        - loki-data:/loki
      ports:
        - "127.0.0.1:3100:3100"
      networks:
        - monitoring

    json-exporter:
      # Mirrored on Docker Hub as prometheuscommunity/json-exporter:v0.6.0
      image: quay.io/prometheuscommunity/json-exporter:v0.6.0
      container_name: monitoring-json-exporter
      restart: unless-stopped
      command:
        - --config.file=/config.yml
      volumes:
        - ./json-exporter/config.yml:/config.yml:ro
      ports:
        - "127.0.0.1:7979:7979"
      networks:
        - monitoring

    grafana:
      image: grafana/grafana:11.4.0
      container_name: monitoring-grafana
      restart: unless-stopped
      depends_on:
        - prometheus
        - loki
      environment:
        GF_SECURITY_ADMIN_USER: admin
        GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_ADMIN_PASSWORD:?GRAFANA_ADMIN_PASSWORD must be set in .env}
        GF_USERS_ALLOW_SIGN_UP: "false"
        GF_AUTH_ANONYMOUS_ENABLED: "false"
        GF_SERVER_ROOT_URL: http://127.0.0.1:3000
        GF_ANALYTICS_REPORTING_ENABLED: "false"
        GF_ANALYTICS_CHECK_FOR_UPDATES: "false"
      volumes:
        - grafana-data:/var/lib/grafana
        - ./grafana/provisioning:/etc/grafana/provisioning:ro
        - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
      ports:
        - "127.0.0.1:3000:3000"
      networks:
        - monitoring
  ```
- [ ] Verify cú pháp compose cục bộ (truyền biến giả vì `:?` bắt buộc biến tồn tại):
  ```bash
  GRAFANA_ADMIN_PASSWORD=dummy docker compose -f deploy/monitoring/docker-compose.yml config -q && echo "compose OK"
  ```
  Expected: in ra `compose OK`, không có lỗi YAML/interpolation.
- [ ] Commit:
  ```bash
  git add deploy/monitoring/docker-compose.yml deploy/monitoring/.gitignore
  git commit -m "feat(monitoring): docker-compose stack (grafana+loki+prometheus+json-exporter), loopback-bound"
  ```

---

### Task 2: Cấu hình Grafana Loki (`loki-config.yml`)

**Files:**
- Create `deploy/monitoring/loki/loki-config.yml`

- [ ] Tạo `deploy/monitoring/loki/loki-config.yml` — chế độ single-binary, lưu trên filesystem (`/loki` = named volume `loki-data`), bật compactor + retention 7 ngày (`168h`), tắt telemetry:
  ```yaml
  # Grafana Loki — single-binary, filesystem storage, 7-day retention.
  # NOTE: "Grafana Loki" (log aggregation) is unrelated to the app's "Loki"
  # endpoint (eur.loki.delve.office.com). This config is for logs only.
  auth_enabled: false

  server:
    http_listen_address: 0.0.0.0
    http_listen_port: 3100
    grpc_listen_port: 9096
    log_level: info

  common:
    instance_addr: 127.0.0.1
    path_prefix: /loki
    storage:
      filesystem:
        chunks_directory: /loki/chunks
        rules_directory: /loki/rules
    replication_factor: 1
    ring:
      kvstore:
        store: inmemory

  schema_config:
    configs:
      - from: 2024-01-01
        store: tsdb
        object_store: filesystem
        schema: v13
        index:
          prefix: index_
          period: 24h

  limits_config:
    retention_period: 168h
    reject_old_samples: true
    reject_old_samples_max_age: 168h
    max_query_lookback: 168h
    ingestion_rate_mb: 16
    ingestion_burst_size_mb: 32
    volume_enabled: true

  compactor:
    working_directory: /loki/compactor
    delete_request_store: filesystem
    retention_enabled: true
    retention_delete_delay: 2h
    compaction_interval: 10m

  ruler:
    storage:
      type: local
      local:
        directory: /loki/rules
    rule_path: /loki/rules-temp
    enable_api: true

  analytics:
    reporting_enabled: false
  ```
- [ ] Verify YAML hợp lệ cục bộ:
  ```bash
  python3 -c "import yaml,sys; yaml.safe_load(open('deploy/monitoring/loki/loki-config.yml')); print('loki yaml OK')"
  ```
  Expected: `loki yaml OK`.
- [ ] Commit:
  ```bash
  git add deploy/monitoring/loki/loki-config.yml
  git commit -m "feat(monitoring): Grafana Loki single-binary config, 7d retention"
  ```

---

### Task 3: Cấu hình Prometheus (`prometheus.yml`, template render từ tenants.yml)

**Files:**
- Create `deploy/monitoring/prometheus/prometheus.yml`

- [ ] Tạo `deploy/monitoring/prometheus/prometheus.yml`. File này là **Jinja2 template** (role sẽ render bằng `ansible.builtin.template` từ `app_vps` + `tenants`, KHÔNG copy thô). Có 3 scrape job: self-scrape, `linkedin_nodes` (node_exporter mỗi app VPS, mang theo metric textfile `getprofile_*`), và `manageuser_status` (gọi `/status` từng tenant qua json-exporter, gắn label `tenant`):
  ```yaml
  # RENDERED by Ansible (deploy/roles/monitoring) from group_vars/all/tenants.yml.
  # This file contains Jinja2 — it is NOT valid plain YAML until rendered.
  # Do not edit the rendered copy on the ops host; edit this template in git.
  global:
    scrape_interval: 30s
    scrape_timeout: 10s
    evaluation_interval: 30s
    external_labels:
      monitor: token-tool-ops

  scrape_configs:
    # --- Prometheus self-monitoring -----------------------------------------
    - job_name: prometheus
      static_configs:
        - targets:
            - 127.0.0.1:9090

    # --- node_exporter on each app VPS --------------------------------------
    # The node_exporter textfile collector on every app VPS exposes the
    # Get_Profile counters written to /var/lib/node_exporter/textfile/getprofile_<id>.prom
    # (metric names getprofile_*, label tenant="<id>"). Prometheus picks them up here.
    - job_name: linkedin_nodes
      static_configs:
  {% for vps_name, vps in app_vps.items() %}
        - targets:
            - {{ vps.ansible_host }}:9100
          labels:
            vps: {{ vps_name }}
  {% endfor %}

    # --- Manage_User /status per tenant via json-exporter -------------------
    # json-exporter (same compose network) fetches http://<vps_ip>:<port>/status
    # and maps queue_size / total_* JSON fields to gauges. We pass the full URL
    # as __param_target and route the actual scrape to the json-exporter container.
    - job_name: manageuser_status
      metrics_path: /probe
      params:
        module:
          - manageuser_status
      static_configs:
  {% for t in tenants %}
        - targets:
            - http://{{ app_vps[t.vps].ansible_host }}:{{ t.manageuser_port }}/status
          labels:
            tenant: {{ t.id }}
            vps: {{ t.vps }}
  {% endfor %}
      relabel_configs:
        - source_labels: [__address__]
          target_label: __param_target
        - source_labels: [__param_target]
          target_label: instance
        - target_label: __address__
          replacement: json-exporter:7979
  ```
- [ ] Verify logic render cục bộ (render thử với dữ liệu mẫu rồi `promtool check` nếu có; nếu không có `promtool` thì chỉ kiểm tra Jinja render ra YAML hợp lệ):
  ```bash
  python3 - <<'PY'
  from jinja2 import Template
  import yaml
  src = open("deploy/monitoring/prometheus/prometheus.yml").read()
  ctx = {
      "app_vps": {"vps1": {"ansible_host": "10.0.0.1"},
                  "vps2": {"ansible_host": "10.0.0.2"},
                  "vps3": {"ansible_host": "10.0.0.3"}},
      "tenants": [
          {"id": "t1", "vps": "vps1", "manageuser_port": 5000},
          {"id": "t2", "vps": "vps1", "manageuser_port": 5001},
          {"id": "t3", "vps": "vps2", "manageuser_port": 5000},
      ],
  }
  rendered = Template(src).render(**ctx)
  yaml.safe_load(rendered)
  print("prometheus template renders to valid YAML")
  PY
  ```
  Expected: `prometheus template renders to valid YAML`.
- [ ] (Tùy chọn) Nếu có `promtool` cài sẵn: ghi `rendered` ra file tạm và chạy `promtool check config /tmp/prometheus.rendered.yml` → expected `SUCCESS: ... config ... is valid`.
- [ ] Commit:
  ```bash
  git add deploy/monitoring/prometheus/prometheus.yml
  git commit -m "feat(monitoring): Prometheus scrape template (linkedin_nodes + manageuser_status)"
  ```

---

### Task 4: Cấu hình json-exporter (module `manageuser_status`)

**Files:**
- Create `deploy/monitoring/json-exporter/config.yml`

- [ ] Tạo `deploy/monitoring/json-exporter/config.yml`. Module `manageuser_status` map 5 field JSON của `/status` (`queue_size`, `total_created`, `total_tokens`, `total_deleted`, `total_failed_delete`) thành 5 gauge. File này là STATIC (định nghĩa CÁCH scrape; danh sách target nằm trong `prometheus.yml`):
  ```yaml
  # prometheus-community/json_exporter module.
  # Prometheus probes /probe?module=manageuser_status&target=http://<vps_ip>:<port>/status
  # Manage_User GET /status returns a flat JSON object:
  #   {"queue_size":N,"total_created":N,"total_tokens":N,"total_deleted":N,"total_failed_delete":N}
  ---
  modules:
    manageuser_status:
      metrics:
        - name: manageuser_queue_size
          path: '{.queue_size}'
          help: Tokens currently buffered in the Manage_User producer queue
          valuetype: gauge
        - name: manageuser_total_created
          path: '{.total_created}'
          help: Total M365 users created since service start
          valuetype: gauge
        - name: manageuser_total_tokens
          path: '{.total_tokens}'
          help: Total refresh tokens minted since service start
          valuetype: gauge
        - name: manageuser_total_deleted
          path: '{.total_deleted}'
          help: Total users deleted since service start
          valuetype: gauge
        - name: manageuser_total_failed_delete
          path: '{.total_failed_delete}'
          help: Total failed user deletions since service start
          valuetype: gauge
  ```
- [ ] Verify YAML hợp lệ cục bộ:
  ```bash
  python3 -c "import yaml; d=yaml.safe_load(open('deploy/monitoring/json-exporter/config.yml')); assert 'manageuser_status' in d['modules']; print('json-exporter yaml OK')"
  ```
  Expected: `json-exporter yaml OK`.
- [ ] Commit:
  ```bash
  git add deploy/monitoring/json-exporter/config.yml
  git commit -m "feat(monitoring): json-exporter manageuser_status module (5 gauges)"
  ```

---

### Task 5: Grafana provisioning (datasources + dashboards provider + dashboard khởi tạo)

**Files:**
- Create `deploy/monitoring/grafana/provisioning/datasources/datasources.yml`
- Create `deploy/monitoring/grafana/provisioning/dashboards/dashboards.yml`
- Create `deploy/monitoring/grafana/dashboards/token-tool-overview.json`

- [ ] Tạo `deploy/monitoring/grafana/provisioning/datasources/datasources.yml` (Prometheus là default + Loki; URL dùng service DNS nội bộ compose):
  ```yaml
  apiVersion: 1

  datasources:
    - name: Prometheus
      uid: prometheus
      type: prometheus
      access: proxy
      url: http://prometheus:9090
      isDefault: true
      editable: false
      jsonData:
        httpMethod: POST
        timeInterval: 30s

    - name: Loki
      uid: loki
      type: loki
      access: proxy
      url: http://loki:3100
      editable: false
      jsonData:
        maxLines: 1000
  ```
- [ ] Tạo `deploy/monitoring/grafana/provisioning/dashboards/dashboards.yml` (file provider trỏ `/var/lib/grafana/dashboards`):
  ```yaml
  apiVersion: 1

  providers:
    - name: token-tool
      orgId: 1
      folder: token-tool
      type: file
      disableDeletion: false
      updateIntervalSeconds: 30
      allowUiUpdates: false
      options:
        path: /var/lib/grafana/dashboards
        foldersFromFilesStructure: false
  ```
- [ ] Tạo `deploy/monitoring/grafana/dashboards/token-tool-overview.json` — dashboard khởi tạo có sẵn (queue size + tokens theo tenant, app VPS up, và bảng catch-all `getprofile_*` để không phụ thuộc tên metric chính xác của Get_Profile):
  ```json
  {
    "uid": "token-tool-overview",
    "title": "Token Tool — Overview",
    "tags": ["token-tool"],
    "timezone": "browser",
    "schemaVersion": 39,
    "version": 1,
    "refresh": "30s",
    "time": { "from": "now-6h", "to": "now" },
    "templating": { "list": [] },
    "annotations": { "list": [] },
    "panels": [
      {
        "id": 1,
        "type": "timeseries",
        "title": "Manage_User queue size (per tenant)",
        "datasource": { "type": "prometheus", "uid": "prometheus" },
        "gridPos": { "h": 8, "w": 12, "x": 0, "y": 0 },
        "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
        "targets": [
          {
            "refId": "A",
            "datasource": { "type": "prometheus", "uid": "prometheus" },
            "expr": "manageuser_queue_size",
            "legendFormat": "{{tenant}}"
          }
        ]
      },
      {
        "id": 2,
        "type": "timeseries",
        "title": "Refresh tokens minted (total, per tenant)",
        "datasource": { "type": "prometheus", "uid": "prometheus" },
        "gridPos": { "h": 8, "w": 12, "x": 12, "y": 0 },
        "fieldConfig": { "defaults": { "unit": "short" }, "overrides": [] },
        "targets": [
          {
            "refId": "A",
            "datasource": { "type": "prometheus", "uid": "prometheus" },
            "expr": "manageuser_total_tokens",
            "legendFormat": "{{tenant}}"
          }
        ]
      },
      {
        "id": 3,
        "type": "stat",
        "title": "App VPS node_exporter up",
        "datasource": { "type": "prometheus", "uid": "prometheus" },
        "gridPos": { "h": 8, "w": 8, "x": 0, "y": 8 },
        "fieldConfig": {
          "defaults": {
            "mappings": [
              { "type": "value", "options": { "0": { "text": "DOWN", "color": "red" }, "1": { "text": "UP", "color": "green" } } }
            ],
            "thresholds": { "mode": "absolute", "steps": [ { "color": "red", "value": null }, { "color": "green", "value": 1 } ] }
          },
          "overrides": []
        },
        "options": { "reduceOptions": { "calcs": ["lastNotNull"] }, "colorMode": "background", "graphMode": "none" },
        "targets": [
          {
            "refId": "A",
            "datasource": { "type": "prometheus", "uid": "prometheus" },
            "expr": "up{job=\"linkedin_nodes\"}",
            "legendFormat": "{{vps}}"
          }
        ]
      },
      {
        "id": 4,
        "type": "table",
        "title": "Get_Profile metrics (getprofile_*)",
        "datasource": { "type": "prometheus", "uid": "prometheus" },
        "gridPos": { "h": 8, "w": 16, "x": 8, "y": 8 },
        "fieldConfig": { "defaults": {}, "overrides": [] },
        "options": { "showHeader": true },
        "targets": [
          {
            "refId": "A",
            "datasource": { "type": "prometheus", "uid": "prometheus" },
            "expr": "{__name__=~\"getprofile_.*\"}",
            "format": "table",
            "instant": true
          }
        ]
      }
    ]
  }
  ```
- [ ] Verify YAML + JSON hợp lệ cục bộ:
  ```bash
  python3 -c "import yaml; yaml.safe_load(open('deploy/monitoring/grafana/provisioning/datasources/datasources.yml')); yaml.safe_load(open('deploy/monitoring/grafana/provisioning/dashboards/dashboards.yml')); print('grafana provisioning yaml OK')"
  python3 -c "import json; json.load(open('deploy/monitoring/grafana/dashboards/token-tool-overview.json')); print('dashboard json OK')"
  ```
  Expected: `grafana provisioning yaml OK` và `dashboard json OK`.
- [ ] Commit:
  ```bash
  git add deploy/monitoring/grafana
  git commit -m "feat(monitoring): Grafana provisioning (datasources + dashboard provider + overview)"
  ```

---

### Task 6: Ansible role `monitoring` + playbook deploy stack

> ⚠️ **Reconciliation #2/#3:** `requirements.yml` đã tạo ở Plan 2 Part A — chỉ đảm bảo có `community.docker (>=3.10)`. Playbook canonical là **`deploy/playbooks/monitoring.yml`** (không phải `deploy/monitoring.yml`); khi tạo nó trong `playbooks/`, sửa `monitoring_src` trong `roles/monitoring/defaults/main.yml` thành `"{{ playbook_dir }}/../monitoring"`. Cần thêm `vault_grafana_admin_password` vào vault (Reconciliation #8).

**Files:**
- Create `deploy/requirements.yml`
- Create `deploy/roles/monitoring/defaults/main.yml`
- Create `deploy/roles/monitoring/handlers/main.yml`
- Create `deploy/roles/monitoring/tasks/main.yml`
- Create `deploy/monitoring.yml`

- [ ] Tạo `deploy/requirements.yml` (collections cần thiết):
  ```yaml
  ---
  collections:
    - name: community.docker
      version: ">=3.10.0"
    - name: community.general
      version: ">=9.0.0"
    - name: ansible.posix
      version: ">=1.5.0"
  ```
  Cài: `ansible-galaxy collection install -r deploy/requirements.yml` (expected: 3 collection installed/already present).
- [ ] Tạo `deploy/roles/monitoring/defaults/main.yml`:
  ```yaml
  ---
  monitoring_dir: /opt/monitoring
  # Source-of-truth files live next to the playbook (deploy/monitoring/*).
  monitoring_src: "{{ playbook_dir }}/monitoring"

  # IP the ops node connects FROM when scraping app VPSes. Override with the
  # private-network IP if app VPS and ops share one.
  monitoring_ops_ip: "{{ ops_host.ops.ansible_host }}"

  # When true, this role opens ufw on each app VPS for 9100 + each tenant's
  # Manage_User port, restricted to monitoring_ops_ip only. Set false if you
  # manage app-side firewalls elsewhere or use a private network.
  monitoring_manage_app_firewall: true

  docker_apt_arch_map:
    x86_64: amd64
    aarch64: arm64
  ```
- [ ] Tạo `deploy/roles/monitoring/handlers/main.yml`. Prometheus hot-reload qua `/-/reload` (đã bật `--web.enable-lifecycle`), có retry chờ container sẵn sàng; các service khác restart theo service:
  ```yaml
  ---
  - name: reload prometheus
    ansible.builtin.uri:
      url: http://127.0.0.1:9090/-/reload
      method: POST
      status_code: 200
    register: prom_reload
    until: prom_reload.status == 200
    retries: 10
    delay: 3
    become: true

  - name: restart loki
    community.docker.docker_compose_v2:
      project_src: "{{ monitoring_dir }}"
      services:
        - loki
      state: restarted
    become: true

  - name: restart json-exporter
    community.docker.docker_compose_v2:
      project_src: "{{ monitoring_dir }}"
      services:
        - json-exporter
      state: restarted
    become: true

  - name: restart grafana
    community.docker.docker_compose_v2:
      project_src: "{{ monitoring_dir }}"
      services:
        - grafana
      state: restarted
    become: true
  ```
- [ ] Tạo `deploy/roles/monitoring/tasks/main.yml` (cài Docker + compose plugin; sync config; render Prometheus; ufw ops chỉ SSH; mở 9100 + port `/status` trên app VPS chỉ cho ops; `docker compose up -d`). Secret `.env` dùng `no_log: true`:
  ```yaml
  ---
  - name: Install Docker apt prerequisites
    ansible.builtin.apt:
      name:
        - ca-certificates
        - curl
        - gnupg
      state: present
      update_cache: true
      cache_valid_time: 3600
    become: true

  - name: Create Docker apt keyring directory
    ansible.builtin.file:
      path: /etc/apt/keyrings
      state: directory
      mode: "0755"
    become: true

  - name: Download Docker apt GPG key
    ansible.builtin.get_url:
      url: https://download.docker.com/linux/ubuntu/gpg
      dest: /etc/apt/keyrings/docker.asc
      mode: "0644"
    become: true

  - name: Add Docker apt repository
    ansible.builtin.apt_repository:
      repo: >-
        deb [arch={{ docker_apt_arch_map[ansible_architecture] | default('amd64') }}
        signed-by=/etc/apt/keyrings/docker.asc]
        https://download.docker.com/linux/ubuntu {{ ansible_distribution_release }} stable
      filename: docker
      state: present
    become: true

  - name: Install Docker Engine and Compose plugin
    ansible.builtin.apt:
      name:
        - docker-ce
        - docker-ce-cli
        - containerd.io
        - docker-buildx-plugin
        - docker-compose-plugin
      state: present
      update_cache: true
    become: true

  - name: Ensure Docker service is enabled and running
    ansible.builtin.systemd:
      name: docker
      enabled: true
      state: started
    become: true

  - name: Create monitoring directory tree on the ops host
    ansible.builtin.file:
      path: "{{ monitoring_dir }}/{{ item }}"
      state: directory
      owner: root
      group: root
      mode: "0755"
    loop:
      - ""
      - prometheus
      - loki
      - json-exporter
      - grafana/provisioning/datasources
      - grafana/provisioning/dashboards
      - grafana/dashboards
    become: true

  - name: Render Grafana admin credentials (.env)
    ansible.builtin.copy:
      dest: "{{ monitoring_dir }}/.env"
      owner: root
      group: root
      mode: "0600"
      content: |
        GRAFANA_ADMIN_PASSWORD={{ vault_grafana_admin_password }}
    no_log: true
    become: true
    notify: restart grafana

  - name: Deploy docker-compose.yml
    ansible.builtin.copy:
      src: "{{ monitoring_src }}/docker-compose.yml"
      dest: "{{ monitoring_dir }}/docker-compose.yml"
      owner: root
      group: root
      mode: "0644"
    become: true

  - name: Deploy Grafana Loki config
    ansible.builtin.copy:
      src: "{{ monitoring_src }}/loki/loki-config.yml"
      dest: "{{ monitoring_dir }}/loki/loki-config.yml"
      owner: root
      group: root
      mode: "0644"
    become: true
    notify: restart loki

  - name: Deploy json-exporter config
    ansible.builtin.copy:
      src: "{{ monitoring_src }}/json-exporter/config.yml"
      dest: "{{ monitoring_dir }}/json-exporter/config.yml"
      owner: root
      group: root
      mode: "0644"
    become: true
    notify: restart json-exporter

  - name: Deploy Grafana provisioning
    ansible.builtin.copy:
      src: "{{ monitoring_src }}/grafana/provisioning/"
      dest: "{{ monitoring_dir }}/grafana/provisioning/"
      owner: root
      group: root
      mode: "0644"
      directory_mode: "0755"
    become: true
    notify: restart grafana

  - name: Deploy Grafana dashboards
    ansible.builtin.copy:
      src: "{{ monitoring_src }}/grafana/dashboards/"
      dest: "{{ monitoring_dir }}/grafana/dashboards/"
      owner: root
      group: root
      mode: "0644"
      directory_mode: "0755"
    become: true
    notify: restart grafana

  - name: Render Prometheus config from tenants.yml
    ansible.builtin.template:
      src: "{{ monitoring_src }}/prometheus/prometheus.yml"
      dest: "{{ monitoring_dir }}/prometheus/prometheus.yml"
      owner: root
      group: root
      mode: "0644"
    become: true
    notify: reload prometheus

  - name: Allow OpenSSH through ufw on the ops host
    community.general.ufw:
      rule: allow
      name: OpenSSH
    become: true

  - name: Set ufw default-deny incoming on the ops host
    community.general.ufw:
      direction: incoming
      policy: deny
    become: true

  - name: Enable ufw on the ops host
    community.general.ufw:
      state: enabled
    become: true

  - name: Open node_exporter (9100) to the ops host on each app VPS
    community.general.ufw:
      rule: allow
      direction: in
      proto: tcp
      port: "9100"
      from_ip: "{{ monitoring_ops_ip }}"
    delegate_to: "{{ item }}"
    loop: "{{ groups['app_vps'] }}"
    when: monitoring_manage_app_firewall | default(true)
    become: true

  - name: Open Manage_User /status port to the ops host on the tenant's VPS
    community.general.ufw:
      rule: allow
      direction: in
      proto: tcp
      port: "{{ item.manageuser_port | string }}"
      from_ip: "{{ monitoring_ops_ip }}"
    delegate_to: "{{ item.vps }}"
    loop: "{{ tenants }}"
    loop_control:
      label: "{{ item.id }} -> {{ item.vps }}:{{ item.manageuser_port }}"
    when: monitoring_manage_app_firewall | default(true)
    become: true

  - name: Pull and start the monitoring stack
    community.docker.docker_compose_v2:
      project_src: "{{ monitoring_dir }}"
      state: present
      pull: missing
    become: true
  ```
- [ ] Tạo `deploy/monitoring.yml` (playbook chạy role trên group `ops`; các task ufw delegate sang app VPS dùng inventory sẵn có):
  ```yaml
  ---
  - name: Deploy the monitoring stack on the ops node
    hosts: ops
    gather_facts: true
    become: true
    roles:
      - monitoring
  ```
- [ ] Syntax-check + lint:
  ```bash
  ansible-playbook -i deploy/inventory/hosts.yml deploy/monitoring.yml --syntax-check
  ansible-lint deploy/monitoring.yml deploy/roles/monitoring
  ```
  Expected: `--syntax-check` báo `playbook: deploy/monitoring.yml` không lỗi; `ansible-lint` 0 lỗi (warning về `command`/`risky-file-permissions` không có vì đã set `mode`).
- [ ] (a) Dry-run `--check --diff`:
  ```bash
  ansible-playbook -i deploy/inventory/hosts.yml deploy/monitoring.yml \
    --ask-vault-pass --check --diff
  ```
  Expected: diff hiển thị nội dung `.env` bị ẩn (do `no_log`), các file config/compose/provisioning sẽ được tạo; task `docker_compose_v2` báo sẽ tạo container. Lưu ý: nếu Docker chưa cài trên ops, task `Install Docker Engine` trong check-mode có thể báo `would install`; chạy thật ở bước sau sẽ hiện thực hóa.
- [ ] (b) Chạy thật:
  ```bash
  ansible-playbook -i deploy/inventory/hosts.yml deploy/monitoring.yml --ask-vault-pass
  ```
  Expected: `PLAY RECAP` cho host `ops` có `failed=0`; các handler `reload prometheus` / `restart grafana` chạy ở cuối.
- [ ] (c) Chạy LẠI để chứng minh idempotent:
  ```bash
  ansible-playbook -i deploy/inventory/hosts.yml deploy/monitoring.yml --ask-vault-pass
  ```
  Expected: `PLAY RECAP` → `changed=0` (và `0` handler chạy), chứng minh hội tụ.
- [ ] (d) Verify chức năng trên ops host (chạy trực tiếp trên ops hoặc qua `ansible ops -m shell`):
  ```bash
  docker compose -f /opt/monitoring/docker-compose.yml ps
  ```
  Expected: 4 service `monitoring-prometheus`, `monitoring-loki`, `monitoring-json-exporter`, `monitoring-grafana` đều `running`.
  ```bash
  curl -s http://127.0.0.1:9090/-/ready
  ```
  Expected: `Prometheus Server is Ready.`
  ```bash
  curl -s http://127.0.0.1:3100/ready
  ```
  Expected: `ready` (sau ~30s khởi động Loki).
  ```bash
  curl -s http://127.0.0.1:3000/api/health
  ```
  Expected JSON: `"database": "ok"`.
  ```bash
  curl -s http://127.0.0.1:9090/api/v1/targets | \
    jq -r '.data.activeTargets[] | "\(.labels.job) \(.labels.instance) \(.health)"'
  ```
  Expected (mọi dòng kết thúc `up` sau khi app VPS đã chạy + ufw đã mở):
  ```
  prometheus 127.0.0.1:9090 up
  linkedin_nodes <vps1_ip>:9100 up
  linkedin_nodes <vps2_ip>:9100 up
  linkedin_nodes <vps3_ip>:9100 up
  manageuser_status http://<vps1_ip>:5000/status up
  manageuser_status http://<vps1_ip>:5001/status up
  ...
  ```
  Probe trực tiếp json-exporter cho một tenant:
  ```bash
  curl -s 'http://127.0.0.1:7979/probe?module=manageuser_status&target=http://<vps1_ip>:5000/status'
  ```
  Expected: xuất hiện các dòng `manageuser_queue_size ...`, `manageuser_total_created ...`, `manageuser_total_tokens ...`, `manageuser_total_deleted ...`, `manageuser_total_failed_delete ...`.
  Truy cập Grafana an toàn từ laptop (không phơi port ra ngoài):
  ```bash
  ssh -N -L 3000:127.0.0.1:3000 <user>@<ops_ip>
  # rồi mở http://localhost:3000 (đăng nhập admin / mật khẩu trong vault),
  # dashboard "Token Tool — Overview" trong folder "token-tool".
  ```
- [ ] (e) Commit:
  ```bash
  git add deploy/requirements.yml deploy/monitoring.yml deploy/roles/monitoring
  git commit -m "feat(monitoring): Ansible role + playbook to deploy ops monitoring stack"
  ```

---

---

## Phần B — Agent quan sát trên app VPS + dashboard + e2e

### Task 7: Role `observability`: node_exporter + textfile collector (app VPS)

Role `observability` chạy trên group `app_vps` (3 VPS giống hệt nhau). Task này dựng khung role và cài `prometheus-node-exporter` (gói Ubuntu universe), trỏ textfile collector vào đúng thư mục mà `getprofile@<id>.service` ghi file `.prom` (cross-ref bắt buộc với Plan 2). Prometheus trên ops node sẽ scrape cổng `9100`.

**Cross-ref (BẮT BUỘC khớp Plan 2):** `getprofile@<id>.service` chạy với `--metrics-file=/var/lib/node_exporter/textfile/getprofile_<id>.prom`. Giá trị này phải bằng `{{ node_exporter_textfile_dir }}/getprofile_<id>.prom`. Nếu lệch, textfile collector không đọc được metric của tenant.

**Files:**
- Create: `deploy/roles/observability/defaults/main.yml`
- Create: `deploy/roles/observability/handlers/main.yml`
- Create: `deploy/roles/observability/tasks/main.yml`
- Create: `deploy/roles/observability/tasks/node_exporter.yml`
- Create: `deploy/roles/observability/templates/prometheus-node-exporter.default.j2`
- Create: `deploy/playbooks/observability.yml`

- [ ] **Step 1: Tạo `deploy/roles/observability/defaults/main.yml`**

```yaml
---
# deploy/roles/observability/defaults/main.yml
#
# Observability agents installed on every app VPS (inventory group: app_vps):
#   - prometheus-node-exporter  → host + textfile metrics; Prometheus (ops) scrapes :9100
#   - grafana-alloy             → ships journald logs to Grafana Loki on the ops node
#
# CROSS-REF (Plan 2 tenant role / getprofile@<id>.service):
#   Get_Profile is launched with
#     --metrics-file=/var/lib/node_exporter/textfile/getprofile_<id>.prom
#   which MUST equal {{ node_exporter_textfile_dir }}/getprofile_<id>.prom so the
#   node_exporter textfile collector exposes the per-tenant metrics.

# --- node_exporter ---------------------------------------------------------
node_exporter_port: 9100
node_exporter_listen_address: "0.0.0.0:{{ node_exporter_port }}"
node_exporter_textfile_dir: /var/lib/node_exporter/textfile

# --- Grafana Alloy / Grafana Loki -----------------------------------------
# ops node host (inventory group 'ops', single host) — exposes the Grafana Loki
# push API on :3100. NOTE: "Grafana Loki" (log aggregation) is NOT the app Loki
# (eur.loki.delve.office.com).
ops_ip: "{{ hostvars[groups['ops'][0]].ansible_host }}"
loki_push_url: "http://{{ ops_ip }}:3100/loki/api/v1/push"
alloy_journal_max_age: "12h"

# --- firewall --------------------------------------------------------------
# Manage_User /status is scraped by a json_exporter that runs LOCALLY on the app
# VPS (probes 127.0.0.1:<manageuser_port>/status) and is itself scraped by
# Prometheus on ops over this port. The token API (/tokens/next, which returns
# refresh_tokens) therefore never leaves loopback. The json_exporter itself is
# deployed by the ops monitoring part (cross-ref); here we only open its port.
json_exporter_port: 7979
```

- [ ] **Step 2: Tạo `deploy/roles/observability/handlers/main.yml`**

```yaml
---
# deploy/roles/observability/handlers/main.yml
- name: Restart node_exporter
  ansible.builtin.systemd:
    name: prometheus-node-exporter
    state: restarted
    daemon_reload: true

- name: Restart alloy
  ansible.builtin.systemd:
    name: alloy
    state: restarted
    daemon_reload: true
```

- [ ] **Step 3: Tạo `deploy/roles/observability/tasks/main.yml`** (chỉ include node_exporter ở task này; Task sau sẽ Modify để thêm alloy + firewall)

```yaml
---
# deploy/roles/observability/tasks/main.yml
# Observability agent for each app VPS: node_exporter (host + Get_Profile
# textfile metrics) + Grafana Alloy (journald → Grafana Loki on ops) + ufw.
- name: node_exporter + textfile collector
  ansible.builtin.import_tasks: node_exporter.yml
```

- [ ] **Step 4: Tạo `deploy/roles/observability/tasks/node_exporter.yml`**

```yaml
---
# deploy/roles/observability/tasks/node_exporter.yml
- name: Install prometheus-node-exporter
  ansible.builtin.apt:
    name: prometheus-node-exporter
    state: present
    update_cache: true
    cache_valid_time: 3600

- name: Ensure node_exporter textfile directory exists (Get_Profile writes here)
  ansible.builtin.file:
    path: "{{ node_exporter_textfile_dir }}"
    state: directory
    owner: tokentool
    group: tokentool
    mode: "0755"

- name: Configure node_exporter ARGS (textfile collector + listen address)
  ansible.builtin.template:
    src: prometheus-node-exporter.default.j2
    dest: /etc/default/prometheus-node-exporter
    owner: root
    group: root
    mode: "0644"
  notify: Restart node_exporter

- name: Enable and start node_exporter
  ansible.builtin.systemd:
    name: prometheus-node-exporter
    enabled: true
    state: started
```

- [ ] **Step 5: Tạo `deploy/roles/observability/templates/prometheus-node-exporter.default.j2`**

```jinja
# {{ ansible_managed }}
# Managed by Ansible (role: observability). Do not edit by hand.
# The textfile collector reads *.prom files written by Get_Profile
# (getprofile_<id>.prom). 9100 is restricted to the ops node by ufw.
ARGS="--web.listen-address={{ node_exporter_listen_address }} --collector.textfile.directory={{ node_exporter_textfile_dir }}"
```

- [ ] **Step 6: Tạo `deploy/playbooks/observability.yml`**

```yaml
---
# deploy/playbooks/observability.yml
# Installs node_exporter + Grafana Alloy + ufw rules on every app VPS.
# Run from the ops node (or laptop): ansible-playbook playbooks/observability.yml
- name: Observability agents (node_exporter + Grafana Alloy) on app VPS
  hosts: app_vps
  become: true
  gather_facts: true
  roles:
    - observability
```

- [ ] **Step 7: Dry-run `--check --diff`**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --check --diff --limit app_vps
```
Expected: hiển thị diff cho `prometheus-node-exporter.default.j2`, task install gói, file dir; `PLAY RECAP` mỗi VPS `changed>=1, failed=0`. (Task install có thể báo `changed` ở lần làm tươi cache apt đầu.)

- [ ] **Step 8: Chạy thật**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --limit app_vps
```
Expected: `PLAY RECAP` mỗi VPS `failed=0`, `unreachable=0`.

- [ ] **Step 9: Chạy lại để chứng minh idempotent**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --limit app_vps
```
Expected: mỗi VPS `changed=0`.

- [ ] **Step 10: Verify chức năng (trên 1 app VPS)**

```bash
systemctl is-active prometheus-node-exporter
curl -fsS http://127.0.0.1:9100/metrics | grep -E '^node_textfile_scrape_error|node_exporter_build_info' | head -3
```
Expected: `active`; có dòng `node_exporter_build_info{...} 1` và `node_textfile_scrape_error 0`. (Khi tenant đã chạy, `curl ... | grep getprofile_processed_total` sẽ thấy `getprofile_processed_total{tenant="t1"} <n>`.)

- [ ] **Step 11: Commit**

```bash
git add deploy/roles/observability/defaults/main.yml \
        deploy/roles/observability/handlers/main.yml \
        deploy/roles/observability/tasks/main.yml \
        deploy/roles/observability/tasks/node_exporter.yml \
        deploy/roles/observability/templates/prometheus-node-exporter.default.j2 \
        deploy/playbooks/observability.yml
git commit -m "feat(observability): node_exporter + textfile collector role for app VPS"
```

---

### Task 8: Role `observability`: Grafana Alloy → Grafana Loki (journald shipping)

Cài Grafana Alloy từ apt repo chính thức của Grafana và render config đọc journald, gán nhãn `{vps, tenant, app, unit}`, đẩy về Grafana Loki trên ops node. **Chỉ ship journald app logs** — app không bao giờ log token, nên không có secret nào rời VPS qua đường này.

**Files:**
- Modify: `deploy/roles/observability/tasks/main.yml`
- Create: `deploy/roles/observability/tasks/alloy.yml`
- Create: `deploy/roles/observability/templates/alloy-config.alloy.j2`

- [ ] **Step 1: Modify `deploy/roles/observability/tasks/main.yml`** — thêm import alloy ngay sau node_exporter

```yaml
- name: node_exporter + textfile collector
  ansible.builtin.import_tasks: node_exporter.yml
```
thành
```yaml
- name: node_exporter + textfile collector
  ansible.builtin.import_tasks: node_exporter.yml

- name: Grafana Alloy journald shipping
  ansible.builtin.import_tasks: alloy.yml
```

- [ ] **Step 2: Tạo `deploy/roles/observability/tasks/alloy.yml`**

```yaml
---
# deploy/roles/observability/tasks/alloy.yml
- name: Install apt prerequisites for the Grafana repository
  ansible.builtin.apt:
    name:
      - gpg
      - apt-transport-https
      - ca-certificates
    state: present
    update_cache: true
    cache_valid_time: 3600

- name: Ensure apt keyrings directory exists
  ansible.builtin.file:
    path: /etc/apt/keyrings
    state: directory
    owner: root
    group: root
    mode: "0755"

- name: Add the Grafana APT signing key
  ansible.builtin.get_url:
    url: https://apt.grafana.com/gpg.key
    dest: /etc/apt/keyrings/grafana.asc
    owner: root
    group: root
    mode: "0644"

- name: Add the Grafana APT repository
  ansible.builtin.apt_repository:
    repo: "deb [signed-by=/etc/apt/keyrings/grafana.asc] https://apt.grafana.com stable main"
    filename: grafana
    state: present

- name: Install Grafana Alloy
  ansible.builtin.apt:
    name: alloy
    state: present
    update_cache: true

- name: Allow the alloy user to read the systemd journal
  ansible.builtin.user:
    name: alloy
    groups: systemd-journal
    append: true
  notify: Restart alloy

- name: Render the Alloy configuration
  ansible.builtin.template:
    src: alloy-config.alloy.j2
    dest: /etc/alloy/config.alloy
    owner: root
    group: root
    mode: "0644"
    validate: "alloy fmt %s"
  notify: Restart alloy

- name: Enable and start Alloy
  ansible.builtin.systemd:
    name: alloy
    enabled: true
    state: started
```

- [ ] **Step 3: Tạo `deploy/roles/observability/templates/alloy-config.alloy.j2`**

```jinja
// {{ ansible_managed }}
// Grafana Alloy config (role: observability) — ships systemd journald logs to
// Grafana Loki on the ops node. SECURITY: only journald app logs are shipped;
// Manage_User/Get_Profile never log secrets/tokens, so no secret leaves the VPS
// through this path. "Grafana Loki" here is the log store, NOT the app Loki
// (eur.loki.delve.office.com).

logging {
  level  = "warn"
  format = "logfmt"
}

// Relabel rules applied to every journal entry before it is forwarded.
loki.relabel "journal" {
  forward_to = []

  // Keep the full systemd unit name (e.g. getprofile@t1.service).
  rule {
    source_labels = ["__journal__systemd_unit"]
    target_label  = "unit"
  }

  // app = "manageuser" or "getprofile" (the template-unit prefix before '@').
  rule {
    source_labels = ["__journal__systemd_unit"]
    regex         = `(manageuser|getprofile)@.+\.service`
    target_label  = "app"
    replacement   = "$1"
  }

  // tenant = the instance id between '@' and '.service' (e.g. t1).
  rule {
    source_labels = ["__journal__systemd_unit"]
    regex         = `(?:manageuser|getprofile)@(.+)\.service`
    target_label  = "tenant"
    replacement   = "$1"
  }

  // Forward the journald-reported priority for level-based filtering.
  rule {
    source_labels = ["__journal_priority_keyword"]
    target_label  = "level"
  }
}

// Read the system journal and forward to the Grafana Loki writer.
loki.source.journal "journald" {
  max_age       = "{{ alloy_journal_max_age }}"
  relabel_rules = loki.relabel.journal.rules
  forward_to    = [loki.write.ops.receiver]
  labels        = {
    job = "systemd-journal",
    vps = "{{ inventory_hostname }}",
  }
}

// Push to Grafana Loki on the ops node.
loki.write "ops" {
  endpoint {
    url = "{{ loki_push_url }}"
  }
}
```

- [ ] **Step 4: Cài collection ufw/general (nếu chưa có)** — Task sau dùng `community.general.ufw`; cài sẵn để các lần chạy đồng nhất

```bash
ansible-galaxy collection install community.general
```
Expected: `community.general ... was installed successfully` (hoặc đã có sẵn).

- [ ] **Step 5: Dry-run `--check --diff`**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --check --diff --limit app_vps
```
Expected: diff cho `/etc/alloy/config.alloy`. LƯU Ý: ở lần `--check` ĐẦU TIÊN trên host chưa có Grafana repo, task `Install Grafana Alloy` có thể báo `No package matching 'alloy'` (giới hạn cố hữu của `--check` với repo+package mới). Cách xử lý: chạy thật một lần (Step 6) rồi `--check` lại sẽ sạch.

- [ ] **Step 6: Chạy thật**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --limit app_vps
```
Expected: `failed=0`; handler `Restart alloy` chạy.

- [ ] **Step 7: Chạy lại idempotent**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --limit app_vps
```
Expected: mỗi VPS `changed=0`.

- [ ] **Step 8: Verify chức năng (trên 1 app VPS)**

```bash
alloy fmt /etc/alloy/config.alloy >/dev/null && echo "alloy config OK"
systemctl is-active alloy
journalctl -u alloy -n 20 --no-pager | grep -iE 'loki|journal' | tail -5
```
Expected: `alloy config OK`; `active`; log Alloy không có lỗi push tới `{{ ops_ip }}:3100`. (Xác nhận log đến Grafana Loki ở Task verify cuối.)

- [ ] **Step 9: Commit**

```bash
git add deploy/roles/observability/tasks/main.yml \
        deploy/roles/observability/tasks/alloy.yml \
        deploy/roles/observability/templates/alloy-config.alloy.j2
git commit -m "feat(observability): Grafana Alloy journald shipping to Grafana Loki"
```

---

### Task 9: Role `observability`: ufw firewall + security hardening

> ⚠️ **Reconciliation #4/#5:** json-exporter chạy TẬP TRUNG trên ops node (Part monitoring), KHÔNG chạy local trên app VPS. Trong `firewall.yml`: **bỏ** rule mở `7979`; **thêm** rule mở mỗi `manageuser_port` của tenant trên host này CHỈ từ `ops_ip` (để json-exporter ở ops fetch `http://<vps_ip>:<port>/status`). Đồng thời **bỏ** 2 task ufw `delegate_to` app VPS trong role `monitoring` (Plan 3 Part monitoring) — firewall app VPS do role `observability` sở hữu. Đánh đổi: mở `manageuser_port` cho ops khiến `/tokens/next` reachable từ ops (chấp nhận vì chỉ ops; hardening: json-exporter local probe loopback hoặc reverse-proxy chỉ lộ `/status`).

Áp dụng security-review: mặc định deny incoming / allow outgoing; chỉ mở `9100` (node_exporter) và `{{ json_exporter_port }}` (json_exporter local probe) cho **đúng ops node**; SSH luôn mở trước khi bật ufw. **CỐ Ý KHÔNG** mở các cổng `manageuser_port` (5000/5001…) ra ngoài — endpoint `/tokens/next` trả refresh_token nên phải ở loopback; json_exporter probe `127.0.0.1:<port>/status` rồi Prometheus scrape qua `{{ json_exporter_port }}`. Outbound được phép nên Alloy đẩy được tới Grafana Loki `:3100`. Cổng Grafana `:3000` và Prometheus `:9090` nằm trên ops node (do ops monitoring part bind `127.0.0.1` + truy cập qua SSH tunnel — cross-ref), không liên quan firewall app VPS.

**Files:**
- Modify: `deploy/roles/observability/tasks/main.yml`
- Create: `deploy/roles/observability/tasks/firewall.yml`

- [ ] **Step 1: Modify `deploy/roles/observability/tasks/main.yml`** — thêm import firewall cuối cùng

```yaml
- name: Grafana Alloy journald shipping
  ansible.builtin.import_tasks: alloy.yml
```
thành
```yaml
- name: Grafana Alloy journald shipping
  ansible.builtin.import_tasks: alloy.yml

- name: Firewall (ufw) for observability ports
  ansible.builtin.import_tasks: firewall.yml
```

- [ ] **Step 2: Tạo `deploy/roles/observability/tasks/firewall.yml`**

```yaml
---
# deploy/roles/observability/tasks/firewall.yml
# Restrict inbound observability ports to the ops node only. Outbound is left
# open (default allow) so Alloy can push to Grafana Loki :3100 on ops.
#
# SECURITY: the Manage_User token API (manageuser_port) is deliberately NOT
# opened. /tokens/next returns refresh_tokens; it must stay on loopback. The
# json_exporter runs locally on the app VPS, probes 127.0.0.1:<port>/status, and
# is the only Manage_User-derived surface exposed (to ops only).
- name: Ensure ufw is installed
  ansible.builtin.apt:
    name: ufw
    state: present
    update_cache: true
    cache_valid_time: 3600

- name: Allow inbound SSH (must precede enabling ufw to avoid lockout)
  community.general.ufw:
    rule: allow
    name: OpenSSH

- name: Allow node_exporter scrape ({{ node_exporter_port }}) from the ops node only
  community.general.ufw:
    rule: allow
    direction: in
    proto: tcp
    from_ip: "{{ ops_ip }}"
    to_port: "{{ node_exporter_port }}"
    comment: "node_exporter scrape from ops"

- name: Allow json_exporter scrape ({{ json_exporter_port }}) from the ops node only
  community.general.ufw:
    rule: allow
    direction: in
    proto: tcp
    from_ip: "{{ ops_ip }}"
    to_port: "{{ json_exporter_port }}"
    comment: "json_exporter (Manage_User /status local probe) scrape from ops"

- name: Set default ufw policy (deny incoming, allow outgoing) and enable
  community.general.ufw:
    state: enabled
    direction: "{{ item.direction }}"
    policy: "{{ item.policy }}"
  loop:
    - { direction: incoming, policy: deny }
    - { direction: outgoing, policy: allow }
  loop_control:
    label: "{{ item.direction }}"
```

- [ ] **Step 3: Dry-run `--check --diff`**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --check --diff --limit app_vps
```
Expected: diff các rule ufw (allow 9100/7979 from `{{ ops_ip }}`, default deny incoming). `failed=0`.

- [ ] **Step 4: Chạy thật**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --limit app_vps
```
Expected: `failed=0`; SSH session hiện tại không bị ngắt (rule OpenSSH áp dụng trước khi enable).

- [ ] **Step 5: Chạy lại idempotent**

```bash
cd deploy && ansible-playbook -i inventory playbooks/observability.yml --limit app_vps
```
Expected: mỗi VPS `changed=0`.

- [ ] **Step 6: Verify chức năng (trên 1 app VPS)**

```bash
ufw status verbose | grep -E 'Default:|9100|7979'
```
Expected:
```
Default: deny (incoming), allow (outgoing), disabled (routed)
9100/tcp                   ALLOW IN    <ops_ip>
7979/tcp                   ALLOW IN    <ops_ip>
```

- [ ] **Step 7: Verify cổng token API KHÔNG mở ra ngoài** — từ ops node (không phải localhost):

```bash
# Run on the ops node; expect connection refused/filtered (NOT a JSON response).
curl -m 5 -s -o /dev/null -w '%{http_code}\n' http://<app_vps_ip>:5000/status || echo "blocked (expected)"
```
Expected: `blocked (expected)` hoặc `000` — token API không truy cập được từ ngoài loopback.

- [ ] **Step 8: Commit**

```bash
git add deploy/roles/observability/tasks/main.yml \
        deploy/roles/observability/tasks/firewall.yml
git commit -m "feat(observability): ufw rules restricting metrics ports to ops node"
```

---

### Task 10: Grafana dashboard `linkedin-fleet.json` (provisioned, per-tenant)

Dashboard provisioned (1 dashboard, lọc theo template variable `$tenant`). Datasource tham chiếu theo uid cố định: Prometheus uid `prometheus`, Grafana Loki uid `loki`.

**Cross-ref datasource/metric (ops monitoring part phải khớp):**
- Provisioned datasources phải đặt `uid: prometheus` và `uid: loki`.
- Metric Get_Profile (node_exporter textfile, nhãn `tenant`): `getprofile_processed_total`, `getprofile_successful_total`, `getprofile_failed_total`, `getprofile_tokens_alive`, `getprofile_tokens_dead`, `getprofile_tokens_exhausted`, `getprofile_lines_done`, `getprofile_lines_total`.
- Metric Manage_User (json_exporter, nhãn `tenant` do Prometheus relabel theo target per-tenant): `manageuser_queue_size`, `manageuser_total_created`, `manageuser_total_deleted`, `manageuser_total_failed_delete`, `manageuser_total_tokens`.
- File JSON này phải được ops monitoring part mount vào thư mục Grafana provisioning dashboards (vd `/etc/grafana/provisioning/dashboards/`).

**Files:**
- Create: `deploy/monitoring/grafana/dashboards/linkedin-fleet.json`

- [ ] **Step 1: Tạo `deploy/monitoring/grafana/dashboards/linkedin-fleet.json`**

```json
{
  "annotations": {
    "list": [
      {
        "builtIn": 1,
        "datasource": { "type": "grafana", "uid": "-- Grafana --" },
        "enable": true,
        "hide": true,
        "iconColor": "rgba(0, 211, 255, 1)",
        "name": "Annotations & Alerts",
        "type": "dashboard"
      }
    ]
  },
  "editable": true,
  "fiscalYearStartMonth": 0,
  "graphTooltip": 1,
  "id": null,
  "links": [],
  "liveNow": false,
  "panels": [
    {
      "id": 2,
      "type": "stat",
      "title": "Token queue depth",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 6, "w": 6, "x": 0, "y": 0 },
      "fieldConfig": {
        "defaults": { "unit": "short", "color": { "mode": "thresholds" },
          "thresholds": { "mode": "absolute", "steps": [
            { "color": "red", "value": null },
            { "color": "yellow", "value": 50 },
            { "color": "green", "value": 100 } ] } },
        "overrides": []
      },
      "options": {
        "reduceOptions": { "calcs": ["lastNotNull"], "fields": "", "values": false },
        "orientation": "auto", "textMode": "auto", "colorMode": "value",
        "graphMode": "area", "justifyMode": "auto"
      },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "manageuser_queue_size{tenant=~\"$tenant\"}",
          "legendFormat": "queue {{tenant}}",
          "range": true
        }
      ]
    },
    {
      "id": 3,
      "type": "timeseries",
      "title": "Users created / deleted / failed-delete",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 6, "w": 9, "x": 6, "y": 0 },
      "fieldConfig": {
        "defaults": { "custom": { "drawStyle": "line", "fillOpacity": 10, "lineWidth": 1 }, "unit": "short" },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        { "refId": "A", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "manageuser_total_created{tenant=~\"$tenant\"}", "legendFormat": "created {{tenant}}", "range": true },
        { "refId": "B", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "manageuser_total_deleted{tenant=~\"$tenant\"}", "legendFormat": "deleted {{tenant}}", "range": true },
        { "refId": "C", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "manageuser_total_failed_delete{tenant=~\"$tenant\"}", "legendFormat": "failed_delete {{tenant}}", "range": true }
      ]
    },
    {
      "id": 4,
      "type": "timeseries",
      "title": "Tokens alive / dead / exhausted",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 6, "w": 9, "x": 15, "y": 0 },
      "fieldConfig": {
        "defaults": { "custom": { "drawStyle": "line", "fillOpacity": 10, "lineWidth": 1 }, "unit": "short" },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        { "refId": "A", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "getprofile_tokens_alive{tenant=~\"$tenant\"}", "legendFormat": "alive {{tenant}}", "range": true },
        { "refId": "B", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "getprofile_tokens_dead{tenant=~\"$tenant\"}", "legendFormat": "dead {{tenant}}", "range": true },
        { "refId": "C", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "getprofile_tokens_exhausted{tenant=~\"$tenant\"}", "legendFormat": "exhausted {{tenant}}", "range": true }
      ]
    },
    {
      "id": 5,
      "type": "timeseries",
      "title": "Profiles processed / sec (rate 5m)",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 6 },
      "fieldConfig": {
        "defaults": { "custom": { "drawStyle": "line", "fillOpacity": 10, "lineWidth": 1 }, "unit": "ops" },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "bottom", "calcs": ["mean", "max"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        { "refId": "A", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "rate(getprofile_processed_total{tenant=~\"$tenant\"}[5m])", "legendFormat": "processed {{tenant}}", "range": true },
        { "refId": "B", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "rate(getprofile_successful_total{tenant=~\"$tenant\"}[5m])", "legendFormat": "successful {{tenant}}", "range": true },
        { "refId": "C", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "rate(getprofile_failed_total{tenant=~\"$tenant\"}[5m])", "legendFormat": "failed {{tenant}}", "range": true }
      ]
    },
    {
      "id": 6,
      "type": "timeseries",
      "title": "Successful vs failed (cumulative)",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 8, "w": 6, "x": 12, "y": 6 },
      "fieldConfig": {
        "defaults": { "custom": { "drawStyle": "line", "fillOpacity": 10, "lineWidth": 1 }, "unit": "short" },
        "overrides": []
      },
      "options": {
        "legend": { "displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull"] },
        "tooltip": { "mode": "multi", "sort": "desc" }
      },
      "targets": [
        { "refId": "A", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "getprofile_successful_total{tenant=~\"$tenant\"}", "legendFormat": "successful {{tenant}}", "range": true },
        { "refId": "B", "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "getprofile_failed_total{tenant=~\"$tenant\"}", "legendFormat": "failed {{tenant}}", "range": true }
      ]
    },
    {
      "id": 7,
      "type": "gauge",
      "title": "Progress % (lines_done / lines_total)",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "gridPos": { "h": 8, "w": 6, "x": 18, "y": 6 },
      "fieldConfig": {
        "defaults": {
          "min": 0, "max": 100, "unit": "percent",
          "thresholds": { "mode": "absolute", "steps": [
            { "color": "red", "value": null },
            { "color": "yellow", "value": 50 },
            { "color": "green", "value": 90 } ] }
        },
        "overrides": []
      },
      "options": {
        "reduceOptions": { "calcs": ["lastNotNull"], "fields": "", "values": false },
        "orientation": "auto", "showThresholdLabels": false, "showThresholdMarkers": true
      },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "prometheus", "uid": "prometheus" },
          "expr": "100 * getprofile_lines_done{tenant=~\"$tenant\"} / clamp_min(getprofile_lines_total{tenant=~\"$tenant\"}, 1)",
          "legendFormat": "{{tenant}}",
          "range": true
        }
      ]
    },
    {
      "id": 8,
      "type": "logs",
      "title": "Logs (Grafana Loki) — tenant=$tenant",
      "datasource": { "type": "loki", "uid": "loki" },
      "gridPos": { "h": 10, "w": 24, "x": 0, "y": 14 },
      "options": {
        "showTime": true, "showLabels": false, "showCommonLabels": false,
        "wrapLogMessage": true, "prettifyLogMessage": false, "enableLogDetails": true,
        "dedupStrategy": "none", "sortOrder": "Descending"
      },
      "targets": [
        {
          "refId": "A",
          "datasource": { "type": "loki", "uid": "loki" },
          "expr": "{tenant=~\"$tenant\"}",
          "queryType": "range"
        }
      ]
    }
  ],
  "refresh": "30s",
  "schemaVersion": 39,
  "tags": ["linkedin", "fleet", "token-tool"],
  "templating": {
    "list": [
      {
        "name": "tenant",
        "label": "Tenant",
        "type": "query",
        "datasource": { "type": "prometheus", "uid": "prometheus" },
        "definition": "label_values(getprofile_processed_total, tenant)",
        "query": { "qryType": 1, "query": "label_values(getprofile_processed_total, tenant)", "refId": "PrometheusVariableQueryEditor-VariableQuery" },
        "refresh": 2,
        "regex": "",
        "includeAll": true,
        "allValue": ".*",
        "multi": true,
        "current": { "text": "All", "value": "$__all", "selected": true },
        "sort": 1,
        "hide": 0
      }
    ]
  },
  "time": { "from": "now-6h", "to": "now" },
  "timepicker": {},
  "timezone": "",
  "title": "LinkedIn Fleet — per tenant",
  "uid": "linkedin-fleet",
  "version": 1,
  "weekStart": ""
}
```

- [ ] **Step 2: Validate JSON (dry-run)**

```bash
python3 -m json.tool deploy/monitoring/grafana/dashboards/linkedin-fleet.json > /dev/null && echo "JSON OK"
python3 -c "import json;d=json.load(open('deploy/monitoring/grafana/dashboards/linkedin-fleet.json'));print('uid=',d['uid'],'panels=',len(d['panels']),'vars=',[v['name'] for v in d['templating']['list']])"
```
Expected: `JSON OK`; `uid= linkedin-fleet panels= 7 vars= ['tenant']`.

- [ ] **Step 3: Apply (provisioning trên ops node)** — reload Grafana để nạp dashboard (provider do ops monitoring part cấu hình)

```bash
# On the ops node, after the file is synced into the Grafana provisioning dir:
docker compose -f deploy/monitoring/docker-compose.yml restart grafana
```
Expected: container `grafana` restart `Started`. Provisioning idempotent — Grafana đọc lại file mỗi lần khởi động, không tạo bản trùng.

- [ ] **Step 4: Verify dashboard load (trên ops node, qua loopback)**

```bash
curl -fsS -H "Authorization: Bearer $GRAFANA_TOKEN" \
  http://127.0.0.1:3000/api/dashboards/uid/linkedin-fleet | jq -r '.dashboard.title, (.dashboard.panels | length)'
```
Expected:
```
LinkedIn Fleet — per tenant
7
```

- [ ] **Step 5: Commit**

```bash
git add deploy/monitoring/grafana/dashboards/linkedin-fleet.json
git commit -m "feat(observability): provisioned Grafana fleet dashboard (per-tenant)"
```

---

### Task 11: End-to-end verification (Prometheus, Grafana Loki, Grafana)

Script verify e2e chạy trên ops node: Prometheus targets `node` + `json_exporter` UP, có `getprofile_processed_total` cho tenant, log xuất hiện trong Grafana Loki theo `{tenant="..."}`, dashboard `linkedin-fleet` đã nạp. Đây là cổng kiểm tra cuối cùng của Plan 3 cho phần observability.

**Cross-ref (ops monitoring part):** Prometheus scrape job phải đặt tên `job="linkedin_nodes"` (node_exporter) và `job="manageuser_status"` (json-exporter); Grafana service-account token đặt ở biến môi trường `GRAFANA_TOKEN`.

**Files:**
- Create: `deploy/scripts/verify_observability.sh`

- [ ] **Step 1: Tạo `deploy/scripts/verify_observability.sh`**

```bash
#!/usr/bin/env bash
# deploy/scripts/verify_observability.sh
# End-to-end observability checks for the multi-VPS fleet. Run on the ops node
# (Prometheus :9090, Grafana :3000, Grafana Loki :3100 reachable on localhost).
# Exits non-zero on the first failed check.
#
# Usage:  GRAFANA_TOKEN=<sa-token> ./verify_observability.sh [tenant]
set -euo pipefail

PROM_ADDR="${PROM_ADDR:-http://127.0.0.1:9090}"
LOKI_ADDR="${LOKI_ADDR:-http://127.0.0.1:3100}"
GRAFANA_ADDR="${GRAFANA_ADDR:-http://127.0.0.1:3000}"
GRAFANA_TOKEN="${GRAFANA_TOKEN:?set GRAFANA_TOKEN to a Grafana service-account token}"
DASHBOARD_UID="${DASHBOARD_UID:-linkedin-fleet}"
TENANT="${1:-t1}"

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "== 1. Prometheus targets UP (linkedin_nodes + manageuser_status) =="
targets="$(curl -fsS "${PROM_ADDR}/api/v1/targets")"
for job in linkedin_nodes manageuser_status; do
  health="$(echo "$targets" | jq -r --arg j "$job" \
    '[.data.activeTargets[] | select(.labels.job==$j) | .health] | unique | join(",")')"
  echo "  job=$job health=${health:-<none>}"
  [ "$health" = "up" ] || fail "job '$job' not all up (got: ${health:-none})"
done

echo "== 2. Prometheus has getprofile_processed_total{tenant=\"$TENANT\"} =="
val="$(curl -fsS --get "${PROM_ADDR}/api/v1/query" \
  --data-urlencode "query=getprofile_processed_total{tenant=\"$TENANT\"}" \
  | jq -r '.data.result[0].value[1] // empty')"
echo "  value=${val:-<none>}"
[ -n "$val" ] || fail "no getprofile_processed_total for tenant=$TENANT"

echo "== 3. Grafana Loki has logs for {tenant=\"$TENANT\"} (last 15m) =="
end="$(date +%s)000000000"
start="$(( $(date +%s) - 900 ))000000000"
count="$(curl -fsS --get "${LOKI_ADDR}/loki/api/v1/query_range" \
  --data-urlencode "query={tenant=\"$TENANT\"}" \
  --data-urlencode "start=$start" --data-urlencode "end=$end" --data-urlencode "limit=5" \
  | jq '[.data.result[].values[]] | length')"
echo "  log lines returned=${count:-0}"
[ "${count:-0}" -gt 0 ] || fail "no Grafana Loki logs for tenant=$TENANT in last 15m"

echo "== 4. Grafana dashboard '$DASHBOARD_UID' is provisioned =="
title="$(curl -fsS -H "Authorization: Bearer $GRAFANA_TOKEN" \
  "${GRAFANA_ADDR}/api/dashboards/uid/${DASHBOARD_UID}" | jq -r '.dashboard.title // empty')"
echo "  title=${title:-<none>}"
[ -n "$title" ] || fail "dashboard $DASHBOARD_UID not loaded in Grafana"

echo "ALL CHECKS PASSED for tenant=$TENANT"
```

- [ ] **Step 2: Đặt quyền thực thi**

```bash
chmod +x deploy/scripts/verify_observability.sh
```

- [ ] **Step 3: Smoke-check cú pháp bash (dry-run)**

```bash
bash -n deploy/scripts/verify_observability.sh && echo "syntax OK"
```
Expected: `syntax OK`.

- [ ] **Step 4: Chạy thật trên ops node (tenant t1)**

```bash
GRAFANA_TOKEN="<grafana-sa-token>" deploy/scripts/verify_observability.sh t1
```
Expected:
```
== 1. Prometheus targets UP (linkedin_nodes + manageuser_status) ==
  job=linkedin_nodes health=up
  job=manageuser_status health=up
== 2. Prometheus has getprofile_processed_total{tenant="t1"} ==
  value=<number >= 0>
== 3. Grafana Loki has logs for {tenant="t1"} (last 15m) ==
  log lines returned=<number > 0>
== 4. Grafana dashboard 'linkedin-fleet' is provisioned ==
  title=LinkedIn Fleet — per tenant
ALL CHECKS PASSED for tenant=t1
```

- [ ] **Step 5: Chạy lại (chứng minh ổn định, không phụ thuộc trạng thái)** — lặp cho mọi tenant trên fleet

```bash
for t in t1 t2 t3 t4 t5 t6; do
  GRAFANA_TOKEN="<grafana-sa-token>" deploy/scripts/verify_observability.sh "$t" || echo "tenant $t: SKIP/absent"
done
```
Expected: mỗi tenant đang chạy in `ALL CHECKS PASSED for tenant=<t>`; tenant chưa cấu hình in `SKIP/absent`.

- [ ] **Step 6: Verify security posture (secret không bị ship)** — Alloy chỉ ship journald; xác nhận không có refresh_token rò rỉ vào Grafana Loki:

```bash
end="$(date +%s)000000000"; start="$(( $(date +%s) - 900 ))000000000"
curl -fsS --get http://127.0.0.1:3100/loki/api/v1/query_range \
  --data-urlencode 'query={app=~"manageuser|getprofile"} |~ "(?i)refresh_token|0\\.A[A-Za-z0-9]"' \
  --data-urlencode "start=$start" --data-urlencode "end=$end" --data-urlencode "limit=1" \
  | jq '[.data.result[].values[]] | length'
```
Expected: `0` — không có dòng log nào chứa pattern giống token (app không log token).

- [ ] **Step 7: Commit**

```bash
git add deploy/scripts/verify_observability.sh
git commit -m "feat(observability): e2e verification script (Prometheus + Grafana Loki + Grafana)"
```
---

## Self-Review (Plan 3)

**Spec coverage:**
- Log aggregation journald → Alloy → Grafana Loki (label `{vps,tenant,app,unit}`) → Part B. ✅
- Metrics: Manage_User `/status` qua json-exporter (`manageuser_*`); Get_Profile textfile (`getprofile_*`) qua node_exporter → Part A + Part B. ✅
- Dashboard per-tenant (`$tenant`) → Part B `linkedin-fleet.json` + overview Part A. ✅
- Bind 127.0.0.1 + SSH tunnel; ufw hạn chế cổng chỉ cho ops; secret không bị ship → Part A + Part B. ✅
- e2e verify (targets up, metric tồn tại, log có, dashboard load, secret-leak = 0) → Part B. ✅

**Consistency (đã chốt ở mục Quyết định hợp nhất):** inventory `hosts.yml`; playbook `playbooks/monitoring.yml`; json-exporter tập trung trên ops; firewall app VPS do `observability`; job names `linkedin_nodes`/`manageuser_status`; vault grafana password.

**Lưu ý:** metric `getprofile_*` chỉ xuất hiện khi tenant đã chạy + node_exporter (Part B) đã cấu hình; verify Part B chạy sau khi cả Plan 2 + Part A đã hội tụ.
