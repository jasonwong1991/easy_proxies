#!/bin/sh
# Auto-generate config and fix permissions, then start easy_proxies

CONFIG_DIR="/etc/easy_proxies"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
NODES_FILE="$CONFIG_DIR/nodes.txt"
EXAMPLE_CONFIG="/app/config.example.yaml"

# Get current user uid/gid for permission fix
CURRENT_UID=$(id -u 2>/dev/null || echo "10001")
CURRENT_GID=$(id -g 2>/dev/null || echo "10001")

# Auto-generate config.yaml if not exists
if [ ! -f "$CONFIG_FILE" ]; then
    cp "$EXAMPLE_CONFIG" "$CONFIG_FILE"
    echo "[easy_proxies] Generated default config from $EXAMPLE_CONFIG"
fi

# Auto-create nodes.txt if not exists
if [ ! -f "$NODES_FILE" ]; then
    touch "$NODES_FILE"
    echo "[easy_proxies] Created empty nodes.txt"
fi

# Fix ownership of mounted files so the current user can access them
chown -R "$CURRENT_UID:$CURRENT_GID" /etc/easy_proxies 2>/dev/null || true

exec /usr/local/bin/easy_proxies "$@"
