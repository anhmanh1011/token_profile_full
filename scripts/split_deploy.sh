#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/parse_config.sh"

if [ $# -lt 1 ]; then
    echo "Usage: $0 <emails.txt>"
    echo "Splits the file into ${TENANT_COUNT} parts and deploys to worker VPS"
    exit 1
fi

INPUT_FILE="$1"
if [ ! -f "$INPUT_FILE" ]; then
    echo "Error: File not found: $INPUT_FILE"
    exit 1
fi

TOTAL_LINES=$(wc -l < "$INPUT_FILE")
LINES_PER_PART=$(( (TOTAL_LINES + TENANT_COUNT - 1) / TENANT_COUNT ))

echo "=== Split & Deploy ==="
echo "Input: $INPUT_FILE ($TOTAL_LINES lines)"
echo "Workers: $TENANT_COUNT"
echo "Lines per worker: ~$LINES_PER_PART"

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

split -l "$LINES_PER_PART" "$INPUT_FILE" "$TMPDIR/part_"
PARTS=("$TMPDIR"/part_*)

echo ""
for i in $(seq 0 $((TENANT_COUNT - 1))); do
    host="${WORKER_HOSTS[$i]}"
    user="${WORKER_USERS[$i]}"
    dir="${WORKER_DIRS[$i]}"
    name="${TENANT_NAMES[$i]}"

    if [ $i -ge ${#PARTS[@]} ]; then
        echo "[WARN] No part for worker $name ($host), skipping"
        continue
    fi

    count=$(wc -l < "${PARTS[$i]}")
    echo "Deploying to $name ($host) — $count emails..."
    rsync -az --progress "${PARTS[$i]}" "${user}@${host}:${dir}/emails.txt"
done

echo ""
echo "=== Deploy Complete ==="
