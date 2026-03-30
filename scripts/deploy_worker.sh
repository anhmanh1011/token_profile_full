#!/usr/bin/env bash
set -euo pipefail

CENTRAL_API="${CENTRAL_API:-http://central-ip:8080}"
TENANT_ID="${TENANT_ID:-}"
MAX_CPM="${MAX_CPM:-20000}"
WORKERS="${WORKERS:-500}"

if [ -z "$TENANT_ID" ]; then
    echo "Error: TENANT_ID not set"
    echo "Usage: TENANT_ID=xxx CENTRAL_API=http://ip:8080 ./deploy_worker.sh"
    exit 1
fi

echo "=== Worker VPS Deployment ==="
echo "API: $CENTRAL_API"
echo "Tenant: $TENANT_ID"
echo "CPM: $MAX_CPM"

echo "Building CHECK_CONNECTION..."
cd /opt/check-connection
go build -o check_connection .
echo "Built successfully"

cat > /etc/systemd/system/check-connection.service << EOF
[Unit]
Description=CHECK_CONNECTION Worker ($TENANT_ID)

[Service]
Type=simple
WorkingDirectory=/opt/check-connection
ExecStart=/opt/check-connection/check_connection \
    -api $CENTRAL_API \
    -tenant $TENANT_ID \
    -emails emails.txt \
    -max-cpm $MAX_CPM \
    -workers $WORKERS
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable check-connection

echo ""
echo "=== Worker Deployment Complete ==="
echo "Start: systemctl start check-connection"
echo "Logs:  journalctl -u check-connection -f"
