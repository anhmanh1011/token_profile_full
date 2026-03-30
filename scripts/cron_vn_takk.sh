#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/parse_config.sh"

LOG_DIR="/opt/vn-takk/logs"
mkdir -p "$LOG_DIR"
TIMESTAMP=$(date +%Y-%m-%d_%H-%M-%S)
LOG_FILE="$LOG_DIR/vn-takk_$TIMESTAMP.log"

echo "=== VN-TAKK Cron Run: $TIMESTAMP ===" >> "$LOG_FILE"

for i in $(seq 0 $((TENANT_COUNT - 1))); do
    tenant_id="${TENANT_IDS[$i]}"
    accounts_file="${TENANT_ACCOUNTS[$i]}"
    name="${TENANT_NAMES[$i]}"

    echo "[$(date)] Processing $name (tenant: $tenant_id)" >> "$LOG_FILE"

    python3 /opt/vn-takk/main.py \
        --redis "$CENTRAL_REDIS" \
        --tenant "$tenant_id" \
        --accounts "$accounts_file" \
        --threads "$VN_TAKK_THREADS" \
        >> "$LOG_FILE" 2>&1

    echo "[$(date)] Finished $name" >> "$LOG_FILE"
done

echo "=== VN-TAKK Cron Complete ===" >> "$LOG_FILE"

find "$LOG_DIR" -name "vn-takk_*.log" -mtime +7 -delete
