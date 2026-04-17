#!/usr/bin/env bash
# Linux launcher cho email-gen. Tự cài rustup nếu thiếu, build release,
# rồi chạy với inputs/output tùy biến qua env vars hoặc flags.
#
# Usage:
#   ./run.sh                                  # dùng defaults trong testdata/
#   DOMAINS=a.txt USERNAMES=u.txt ./run.sh    # override qua env
#   ./run.sh --gzip --split 1                 # forward thêm flags cho binary
#
# Env vars (optional):
#   DOMAINS     path tới domains file (default: testdata/domains.txt)
#   USERNAMES   path tới usernames file (default: testdata/usernames.txt)
#   OUTPUT      path output (default: ../Get_Profile/emails.txt)
#   CHUNK_SIZE  domains per chunk (default: 2000)
#   THREADS     worker threads (default: 0 = num_cpus)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

log() { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
err() { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; }

# 1. Đảm bảo Rust toolchain có sẵn
if ! command -v cargo >/dev/null 2>&1; then
    if [ -f "$HOME/.cargo/env" ]; then
        # shellcheck disable=SC1091
        source "$HOME/.cargo/env"
    fi
fi

if ! command -v cargo >/dev/null 2>&1; then
    log "Rust chưa cài, đang cài rustup (stable)..."
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
        | sh -s -- -y --default-toolchain stable --profile minimal
    # shellcheck disable=SC1091
    source "$HOME/.cargo/env"
fi

# 2. Build nếu binary chưa có hoặc source mới hơn
BIN="target/release/email-gen"
needs_build=0
if [ ! -x "$BIN" ]; then
    needs_build=1
else
    # Check source newer than binary (find exits 0 but prints files)
    if [ -n "$(find src Cargo.toml Cargo.lock -newer "$BIN" 2>/dev/null | head -1)" ]; then
        needs_build=1
    fi
fi

if [ "$needs_build" -eq 1 ]; then
    log "Build release binary..."
    cargo build --release
else
    log "Dùng binary có sẵn: $BIN"
fi

# 3. Validate inputs
DOMAINS="${DOMAINS:-testdata/domains.txt}"
USERNAMES="${USERNAMES:-testdata/usernames.txt}"
OUTPUT="${OUTPUT:-../Get_Profile/emails.txt}"
CHUNK_SIZE="${CHUNK_SIZE:-2000}"
THREADS="${THREADS:-0}"

for f in "$DOMAINS" "$USERNAMES"; do
    if [ ! -f "$f" ]; then
        err "input file không tồn tại: $f"
        err "Hint: đặt file vào testdata/ hoặc set env DOMAINS=... USERNAMES=..."
        exit 1
    fi
done

# 4. Đảm bảo thư mục output tồn tại
output_dir="$(dirname "$OUTPUT")"
if [ ! -d "$output_dir" ]; then
    log "Tạo thư mục output: $output_dir"
    mkdir -p "$output_dir"
fi

# 5. Run
log "Chạy email-gen:"
echo "    domains    : $DOMAINS"
echo "    usernames  : $USERNAMES"
echo "    output     : $OUTPUT"
echo "    chunk_size : $CHUNK_SIZE"
echo "    threads    : $THREADS (0 = auto)"
echo

exec "$BIN" \
    -d "$DOMAINS" \
    -u "$USERNAMES" \
    -o "$OUTPUT" \
    -c "$CHUNK_SIZE" \
    -t "$THREADS" \
    --progress \
    "$@"
