#!/usr/bin/env bash
set -euo pipefail

echo "=== Central VPS Deployment ==="

if ! command -v redis-server &> /dev/null; then
    echo "Installing Redis..."
    apt-get update && apt-get install -y redis-server
fi

grep -q "^bind 127.0.0.1" /etc/redis/redis.conf || {
    sed -i 's/^bind .*/bind 127.0.0.1/' /etc/redis/redis.conf
    systemctl restart redis-server
}

echo "Redis: $(redis-cli ping)"

echo "Building Token API..."
cd /opt/token-api
go build -o token-api .
echo "Token API built"

echo "Installing VN-TAKK dependencies..."
cd /opt/vn-takk
pip3 install -r requirements.txt

cat > /etc/systemd/system/token-api.service << 'EOF'
[Unit]
Description=Token API Server
After=redis-server.service
Requires=redis-server.service

[Service]
Type=simple
ExecStart=/opt/token-api/token-api -redis 127.0.0.1:6379 -listen :8080
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable token-api
systemctl start token-api

echo "Token API status: $(systemctl is-active token-api)"

echo "Setting up cron..."
cp /opt/scripts/cron_vn_takk.sh /opt/scripts/
chmod +x /opt/scripts/cron_vn_takk.sh

(crontab -l 2>/dev/null | grep -q "cron_vn_takk" ) || {
    (crontab -l 2>/dev/null; echo "0 */6 * * * /opt/scripts/cron_vn_takk.sh") | crontab -
    echo "Cron added"
}

echo ""
echo "=== Deployment Complete ==="
echo "Token API: http://0.0.0.0:8080"
echo "Test: curl http://localhost:8080/stats"
