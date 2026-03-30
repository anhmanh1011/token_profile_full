#!/usr/bin/env bash
# Parse config.yaml and export variables for other scripts
# Usage: source scripts/parse_config.sh [config_path]

CONFIG_FILE="${1:-$(dirname "$0")/../config.yaml}"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "Error: Config file not found: $CONFIG_FILE"
    exit 1
fi

# Simple YAML parser using grep/sed (no yq dependency)
_yaml_val() {
    grep -E "^\s*${1}:" "$CONFIG_FILE" | head -1 | sed 's/.*:\s*"\?\([^"]*\)"\?.*/\1/' | xargs
}

_yaml_tenant_val() {
    # Extract value for tenant at index $1, field $2
    local idx=$1 field=$2
    awk -v idx="$idx" -v field="$field" '
    BEGIN { count=-1; in_tenant=0; in_worker=0 }
    /^  - id:/ { count++; in_tenant=(count==idx); in_worker=0 }
    in_tenant && /worker:/ { in_worker=1; next }
    in_tenant && in_worker && $1 == field":" { gsub(/.*: *"?/, ""); gsub(/".*/, ""); print; exit }
    in_tenant && !in_worker && $1 == field":" { gsub(/.*: *"?/, ""); gsub(/".*/, ""); print; exit }
    ' "$CONFIG_FILE"
}

# Central config
CENTRAL_REDIS=$(_yaml_val "redis")
CENTRAL_API_PORT=$(_yaml_val "token_api_port")
VN_TAKK_THREADS=$(_yaml_val "vn_takk_threads")
CRON_INTERVAL=$(_yaml_val "cron_interval_hours")

# Count tenants
TENANT_COUNT=$(grep -c "^  - id:" "$CONFIG_FILE")

# Parse each tenant
declare -a TENANT_IDS TENANT_NAMES TENANT_ACCOUNTS
declare -a WORKER_HOSTS WORKER_USERS WORKER_DIRS WORKER_CPMS WORKER_COUNTS

for i in $(seq 0 $((TENANT_COUNT - 1))); do
    TENANT_IDS[$i]=$(_yaml_tenant_val $i "id")
    TENANT_NAMES[$i]=$(_yaml_tenant_val $i "name")
    TENANT_ACCOUNTS[$i]=$(_yaml_tenant_val $i "accounts_file")
    WORKER_HOSTS[$i]=$(_yaml_tenant_val $i "host")
    WORKER_USERS[$i]=$(_yaml_tenant_val $i "user")
    WORKER_DIRS[$i]=$(_yaml_tenant_val $i "dir")
    WORKER_CPMS[$i]=$(_yaml_tenant_val $i "max_cpm")
    WORKER_COUNTS[$i]=$(_yaml_tenant_val $i "workers")
done

# Export for use by other scripts
export CENTRAL_REDIS CENTRAL_API_PORT VN_TAKK_THREADS CRON_INTERVAL TENANT_COUNT
export TENANT_IDS TENANT_NAMES TENANT_ACCOUNTS
export WORKER_HOSTS WORKER_USERS WORKER_DIRS WORKER_CPMS WORKER_COUNTS
