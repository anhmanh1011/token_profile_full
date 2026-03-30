#!/usr/bin/env bash
set -euo pipefail

LOG_DIR="/opt/vn-takk/logs"
mkdir -p "$LOG_DIR"
TIMESTAMP=$(date +%Y-%m-%d_%H-%M-%S)
LOG_FILE="$LOG_DIR/vn-takk_$TIMESTAMP.log"

TENANTS=(
    "TENANT_A_ID:/opt/vn-takk/accounts_tenant_a.txt"
    "TENANT_B_ID:/opt/vn-takk/accounts_tenant_b.txt"
    "TENANT_C_ID:/opt/vn-takk/accounts_tenant_c.txt"
)

REDIS_ADDR="127.0.0.1:6379"
THREADS=100

echo "=== VN-TAKK Cron Run: $TIMESTAMP ===" >> "$LOG_FILE"

for entry in "${TENANTS[@]}"; do
    IFS=':' read -r tenant_id accounts_file <<< "$entry"
    echo "[$(date)] Processing tenant: $tenant_id" >> "$LOG_FILE"

    python3 /opt/vn-takk/main.py \
        --redis "$REDIS_ADDR" \
        --tenant "$tenant_id" \
        --accounts "$accounts_file" \
        --threads "$THREADS" \
        >> "$LOG_FILE" 2>&1

    echo "[$(date)] Finished tenant: $tenant_id" >> "$LOG_FILE"
done

echo "=== VN-TAKK Cron Complete ===" >> "$LOG_FILE"

find "$LOG_DIR" -name "vn-takk_*.log" -mtime +7 -delete
