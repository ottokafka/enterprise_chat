#!/usr/bin/env bash
set -euo pipefail

# Simple ClickHouse installation script for Debian/Ubuntu
# 2024–2025 style (using current official repo method)

echo ""
echo "======================================="
echo "  ClickHouse install + basic hardening"
echo "  (Debian/Ubuntu only - simple version)"
echo "======================================="
echo ""

# ────────────────────────────────────────────────
echo "[1/10] Updating package index ..."
apt-get update -qq >/dev/null

# ────────────────────────────────────────────────
echo "[2/10] Installing https transport + gpg + curl ..."
apt-get install -y -qq --no-install-recommends \
    apt-transport-https \
    ca-certificates \
    curl \
    gnupg \
    lsb-release

# ────────────────────────────────────────────────
echo "[3/10] Adding ClickHouse repository key ..."
mkdir -p /usr/share/keyrings

curl -fsSL https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key \
    | gpg --dearmor --yes -o /usr/share/keyrings/clickhouse-keyring.gpg

# ────────────────────────────────────────────────
echo "[4/10] Adding ClickHouse repository ..."
echo "deb [signed-by=/usr/share/keyrings/clickhouse-keyring.gpg] https://packages.clickhouse.com/deb stable main" \
    > /etc/apt/sources.list.d/clickhouse.list

# ────────────────────────────────────────────────
echo "[5/10] Updating package index again (with ClickHouse repo) ..."
apt-get update -qq

# ────────────────────────────────────────────────
echo "[6/10] Installing clickhouse-server + clickhouse-client ..."
DEBIAN_FRONTEND=noninteractive \
    apt-get install -y -qq clickhouse-server clickhouse-client

# ────────────────────────────────────────────────
echo "[7/10] Stopping service before configuration ..."
systemctl stop clickhouse-server 2>/dev/null || true

# ────────────────────────────────────────────────
echo "[8/10] Fixing ownership (very important) ..."
useradd -r -s /bin/false clickhouse 2>/dev/null || true

for dir in /var/lib/clickhouse /var/log/clickhouse-server /etc/clickhouse-server; do
    if [[ -d "$dir" ]]; then
        chown -R clickhouse:clickhouse "$dir"
    fi
done

mkdir -p /run/clickhouse-server
chown clickhouse:clickhouse /run/clickhouse-server

# ────────────────────────────────────────────────
echo "[9/10] Allowing connections from anywhere (listen 0.0.0.0) ..."

# We modify config.xml - very naive replace (works on fresh install)
CONFIG_FILE="/etc/clickhouse-server/config.xml"

if grep -q "<listen_host>127.0.0.1</listen_host>" "$CONFIG_FILE" 2>/dev/null; then
    sed -i 's/<listen_host>127.0.0.1<\/listen_host>/<listen_host>0.0.0.0<\/listen_host>/' "$CONFIG_FILE"
    echo "    → Changed listen_host → 0.0.0.0"
else
    # If the line is not there, we add it near the top (naive approach)
    sed -i '/<clickhouse>/a \    <listen_host>0.0.0.0</listen_host>' "$CONFIG_FILE"
    echo "    → Added <listen_host>0.0.0.0</listen_host>"
fi

# Also allow IPv6 in most cases (optional but common)
if ! grep -q "<listen_host>::</listen_host>" "$CONFIG_FILE"; then
    sed -i '/<listen_host>0.0.0.0<\/listen_host>/a \    <listen_host>::</listen_host>' "$CONFIG_FILE"
    echo "    → Added IPv6 listen <listen_host>::</listen_host>"
fi

# ────────────────────────────────────────────────
echo "[10/10] Creating admin user with password 'admin' ..."

USERS_DIR="/etc/clickhouse-server/users.d"
ADMIN_FILE="$USERS_DIR/admin.xml"

mkdir -p "$USERS_DIR"
chown clickhouse:clickhouse "$USERS_DIR"

cat > "$ADMIN_FILE" << 'EOF'
<clickhouse>
    <users>
        <admin>
            <password>admin</password>
            <networks>
                <ip>::/0</ip>
            </networks>
            <profile>default</profile>
            <quota>default</quota>
            <access_management>1</access_management>
        </admin>
    </users>
</clickhouse>
EOF

chown clickhouse:clickhouse "$ADMIN_FILE"
chmod 640 "$ADMIN_FILE"

echo "    → Admin user created (password = admin)"

# ────────────────────────────────────────────────
echo ""
echo "[OK] Enabling and starting ClickHouse ..."

systemctl daemon-reload
systemctl enable --now clickhouse-server

# Give it a moment to start
sleep 3

if systemctl is-active --quiet clickhouse-server; then
    echo ""
    echo "╔════════════════════════════════════════════╗"
    echo "║          ClickHouse seems to be running    ║"
    echo "╚════════════════════════════════════════════╝"
    echo ""
    echo "You can connect with:"
    echo "    clickhouse-client --user admin --password admin"
    echo ""
    echo "Warning:"
    echo "  • Listening on 0.0.0.0 (not secure for internet!)"
    echo "  • Password 'admin' is very weak – change it!"
    echo ""
else
    echo ""
    echo "!!! ClickHouse failed to start !!!"
    echo ""
    journalctl -u clickhouse-server -n 40 --no-pager
    echo ""
    exit 1
fi