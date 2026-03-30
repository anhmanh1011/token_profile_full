#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/parse_config.sh"

CENTRAL_IP="${1:-}"
if [ -z "$CENTRAL_IP" ]; then
    echo "Usage: $0 <central-vps-ip> [tenant-index]"
    echo ""
    echo "Tenants:"
    for i in $(seq 0 $((TENANT_COUNT - 1))); do
        echo "  $i: ${TENANT_NAMES[$i]} (${TENANT_IDS[$i]}) -> ${WORKER_HOSTS[$i]}"
    done
    echo ""
    echo "Deploy all:  $0 <central-ip>"
    echo "Deploy one:  $0 <central-ip> 0"
    exit 1
fi

CENTRAL_API="http://${CENTRAL_IP}:${CENTRAL_API_PORT}"

deploy_worker() {
    local idx=$1
    local tenant_id="${TENANT_IDS[$idx]}"
    local name="${TENANT_NAMES[$idx]}"
    local host="${WORKER_HOSTS[$idx]}"
    local user="${WORKER_USERS[$idx]}"
    local dir="${WORKER_DIRS[$idx]}"
    local max_cpm="${WORKER_CPMS[$idx]}"
    local workers="${WORKER_COUNTS[$idx]}"

    echo "=== Deploying $name to $host ==="
    echo "  Tenant: $tenant_id"
    echo "  API:    $CENTRAL_API"
    echo "  CPM:    $max_cpm"

    ssh "${user}@${host}" bash -s <<REMOTE
set -euo pipefail

cd "$dir"
go build -o check_connection .

cat > /etc/systemd/system/check-connection.service << 'UNIT'
[Unit]
Description=CHECK_CONNECTION Worker ($name)

[Service]
Type=simple
WorkingDirectory=$dir
ExecStart=$dir/check_connection \\
    -api $CENTRAL_API \\
    -tenant $tenant_id \\
    -emails emails.txt \\
    -max-cpm $max_cpm \\
    -workers $workers
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable check-connection
echo "Done: systemctl start check-connection"
REMOTE

    echo "=== $name deployed ==="
    echo ""
}

if [ -n "${2:-}" ]; then
    deploy_worker "$2"
else
    for i in $(seq 0 $((TENANT_COUNT - 1))); do
        deploy_worker "$i"
    done
fi
