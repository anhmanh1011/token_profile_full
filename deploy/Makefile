# deploy/Makefile — wrapper mỏng quanh Ansible cho stack token-tool.
# Chạy mọi target từ thư mục deploy/ (ansible.cfg ở đây trỏ inventory + roles).
#
# Biến: TENANT (t1..t6, def all; cho status/restart/start/stop/logs),
#       LIMIT (host/group cho provision/deploy/verify), VAULT_PASS (def .vault-pass),
#       EXTRA (cờ ansible thêm, vd EXTRA="--check --diff").
SHELL := /bin/bash

VAULT_PASS ?= .vault-pass
TENANT     ?= all
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
	$(ANSIBLE_PLAYBOOK) playbooks/site.yml --tags tenant $(LIMIT_FLAG) $(EXTRA)

status:                ## systemctl is-active + curl /status mọi tenant (TENANT=t1 để lọc)
	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "action=status tenant=$(TENANT)" $(LIMIT_FLAG) $(EXTRA)

verify:                ## Cổng kiểm tra sau deploy (units active, /status, source-IP, metrics)
	$(ANSIBLE_PLAYBOOK) playbooks/verify.yml $(LIMIT_FLAG) $(EXTRA)

restart: require-tenant ## Restart cặp unit của TENANT=<id>
	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "action=restart tenant=$(TENANT)" $(EXTRA)

start: require-tenant   ## Start cặp unit của TENANT=<id>
	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "action=start tenant=$(TENANT)" $(EXTRA)

stop: require-tenant    ## Stop cặp unit của TENANT=<id>
	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "action=stop tenant=$(TENANT)" $(EXTRA)

logs: require-tenant    ## In journal gần nhất của TENANT=<id>
	$(ANSIBLE_PLAYBOOK) playbooks/control.yml -e "action=logs tenant=$(TENANT)" $(EXTRA)

monitoring-up:         ## (Plan 3) Dựng Grafana+Loki+Prometheus trên ops node
	$(ANSIBLE_PLAYBOOK) playbooks/monitoring.yml --limit ops $(EXTRA)

ping:                  ## Kiểm tra SSH tới host (mọi host, hoặc LIMIT=...)
	$(ANSIBLE) all -m ping $(LIMIT_FLAG)

graph:                 ## In cây inventory
	ansible-inventory $(VAULT_FLAG) --graph

vault-view:            ## Xem secret đã giải mã (không ghi ra đĩa)
	$(ANSIBLE_VAULT) view group_vars/all/vault.yml

require-tenant:
	@if [ "$(TENANT)" = "all" ] || [ -z "$(TENANT)" ]; then \
	  echo "ERROR: target này cần TENANT=<id> (vd: make restart TENANT=t1)"; \
	  exit 2; \
	fi
