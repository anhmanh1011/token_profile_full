#!/usr/bin/env bash
set -euo pipefail

WORKER1_HOST="worker1-ip"
WORKER1_USER="root"
WORKER1_DIR="/opt/check-connection"

WORKER2_HOST="worker2-ip"
WORKER2_USER="root"
WORKER2_DIR="/opt/check-connection"

WORKER3_HOST="worker3-ip"
WORKER3_USER="root"
WORKER3_DIR="/opt/check-connection"

if [ $# -lt 1 ]; then
    echo "Usage: $0 <emails.txt>"
    echo "Splits the file into 3 equal parts and deploys to worker VPS"
    exit 1
fi

INPUT_FILE="$1"
if [ ! -f "$INPUT_FILE" ]; then
    echo "Error: File not found: $INPUT_FILE"
    exit 1
fi

TOTAL_LINES=$(wc -l < "$INPUT_FILE")
LINES_PER_PART=$(( (TOTAL_LINES + 2) / 3 ))

echo "=== Split & Deploy ==="
echo "Input: $INPUT_FILE ($TOTAL_LINES lines)"
echo "Lines per worker: ~$LINES_PER_PART"

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

split -l "$LINES_PER_PART" "$INPUT_FILE" "$TMPDIR/part_"

PARTS=("$TMPDIR"/part_*)
echo "Split into ${#PARTS[@]} parts"

echo ""
echo "Deploying to Worker 1 ($WORKER1_HOST)..."
rsync -az --progress "${PARTS[0]}" "${WORKER1_USER}@${WORKER1_HOST}:${WORKER1_DIR}/emails.txt"

echo "Deploying to Worker 2 ($WORKER2_HOST)..."
rsync -az --progress "${PARTS[1]}" "${WORKER2_USER}@${WORKER2_HOST}:${WORKER2_DIR}/emails.txt"

if [ ${#PARTS[@]} -ge 3 ]; then
    echo "Deploying to Worker 3 ($WORKER3_HOST)..."
    rsync -az --progress "${PARTS[2]}" "${WORKER3_USER}@${WORKER3_HOST}:${WORKER3_DIR}/emails.txt"
fi

echo ""
echo "=== Deploy Complete ==="
echo "Worker 1: $(wc -l < "${PARTS[0]}") emails"
echo "Worker 2: $(wc -l < "${PARTS[1]}") emails"
[ ${#PARTS[@]} -ge 3 ] && echo "Worker 3: $(wc -l < "${PARTS[2]}") emails"
