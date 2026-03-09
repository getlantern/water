#!/usr/bin/env bash
set -euo pipefail

# setup-remote.sh — Provision a Digital Ocean droplet for remote WATER benchmarks.
#
# Prerequisites:
#   - doctl (DO CLI) authenticated: doctl auth init
#   - Go installed locally (to cross-compile echo server)
#   - ss-tunnel installed locally (homebrew: shadowsocks-libev)
#
# Usage:
#   ./scripts/setup-remote.sh up      # Create droplet and start services
#   ./scripts/setup-remote.sh down    # Destroy droplet
#   ./scripts/setup-remote.sh status  # Show droplet IP and status
#   ./scripts/setup-remote.sh ssh     # SSH into the droplet

DROPLET_NAME="water-bench"
REGION="sfo3"
SIZE="s-1vcpu-1gb"
IMAGE="ubuntu-24-04-x64"
SSH_KEY_NAME="water-bench-key"
TAG="water-bench"

SS_PASSWORD="8JCsPssfgS8tiRwiMlhARg=="
SS_METHOD="chacha20-ietf-poly1305"
SS_PORT=8388
ECHO_PORT=8080

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

red()   { printf '\033[1;31m%s\033[0m\n' "$*" >&2; }
green() { printf '\033[1;32m%s\033[0m\n' "$*" >&2; }
blue()  { printf '\033[1;34m%s\033[0m\n' "$*" >&2; }

get_droplet_ip() {
    doctl compute droplet list --tag-name "$TAG" --format PublicIPv4 --no-header 2>/dev/null | head -1
}

get_droplet_id() {
    doctl compute droplet list --tag-name "$TAG" --format ID --no-header 2>/dev/null | head -1
}

ensure_ssh_key() {
    # Check if key already exists in DO
    local key_id
    key_id=$(doctl compute ssh-key list --format ID,Name --no-header 2>/dev/null | grep "$SSH_KEY_NAME" | awk '{print $1}')
    if [ -n "$key_id" ]; then
        echo "$key_id"
        return
    fi

    # Generate a temporary key pair if needed
    local key_path="$HOME/.ssh/${SSH_KEY_NAME}"
    if [ ! -f "$key_path" ]; then
        blue "Generating SSH key pair at $key_path"
        ssh-keygen -t ed25519 -f "$key_path" -N "" -C "$SSH_KEY_NAME"
    fi

    # Upload to DO
    blue "Uploading SSH key to DigitalOcean"
    key_id=$(doctl compute ssh-key import "$SSH_KEY_NAME" --public-key-file "${key_path}.pub" --format ID --no-header)
    echo "$key_id"
}

cmd_up() {
    # Check for existing droplet
    local existing_ip
    existing_ip=$(get_droplet_ip)
    if [ -n "$existing_ip" ]; then
        green "Droplet already exists at $existing_ip"
        echo "Run '$0 down' first to destroy it, or '$0 ssh' to connect."
        return
    fi

    # Ensure doctl is available
    if ! command -v doctl &>/dev/null; then
        red "doctl not found. Install with: brew install doctl"
        exit 1
    fi

    # Ensure SSH key is in DO
    local key_id
    key_id=$(ensure_ssh_key)
    blue "Using SSH key ID: $key_id"

    # Cross-compile echo server for linux/amd64
    blue "Building echo server for linux/amd64..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/echoserver "$PROJECT_DIR/cmd/echoserver"

    # Create droplet
    blue "Creating droplet ($SIZE in $REGION)..."
    doctl compute droplet create "$DROPLET_NAME" \
        --region "$REGION" \
        --size "$SIZE" \
        --image "$IMAGE" \
        --ssh-keys "$key_id" \
        --tag-name "$TAG" \
        --wait

    # Wait for IP
    blue "Waiting for droplet IP..."
    local ip=""
    for i in $(seq 1 30); do
        ip=$(get_droplet_ip)
        if [ -n "$ip" ]; then
            break
        fi
        sleep 2
    done

    if [ -z "$ip" ]; then
        red "Timed out waiting for droplet IP"
        exit 1
    fi
    green "Droplet IP: $ip"

    # Wait for SSH
    blue "Waiting for SSH..."
    local ssh_key="$HOME/.ssh/${SSH_KEY_NAME}"
    local ssh_opts="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -i $ssh_key"
    for i in $(seq 1 60); do
        if ssh $ssh_opts "root@$ip" true 2>/dev/null; then
            break
        fi
        sleep 3
    done

    # Install shadowsocks-libev and copy echo server
    blue "Installing shadowsocks-libev..."
    ssh $ssh_opts "root@$ip" bash <<'SETUP'
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq shadowsocks-libev
        systemctl stop shadowsocks-libev 2>/dev/null || true
        systemctl disable shadowsocks-libev 2>/dev/null || true
SETUP

    blue "Copying echo server..."
    scp $ssh_opts /tmp/echoserver "root@$ip:/usr/local/bin/echoserver"
    ssh $ssh_opts "root@$ip" chmod +x /usr/local/bin/echoserver

    # Write ss-server config
    blue "Configuring ss-server..."
    ssh $ssh_opts "root@$ip" "mkdir -p /etc/shadowsocks-libev && cat > /etc/shadowsocks-libev/bench.json" <<SSCONF
{
    "server": "0.0.0.0",
    "server_port": $SS_PORT,
    "password": "$SS_PASSWORD",
    "method": "$SS_METHOD",
    "timeout": 300
}
SSCONF

    # Create systemd units
    blue "Creating systemd services..."
    ssh $ssh_opts "root@$ip" bash <<'UNITS'
        cat > /etc/systemd/system/ss-bench.service <<EOF
[Unit]
Description=Shadowsocks server for WATER benchmarks
After=network.target

[Service]
ExecStart=/usr/bin/ss-server -c /etc/shadowsocks-libev/bench.json
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

        cat > /etc/systemd/system/echoserver.service <<EOF
[Unit]
Description=Echo server for WATER benchmarks
After=network.target

[Service]
ExecStart=/usr/local/bin/echoserver -port 8080
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

        systemctl daemon-reload
        systemctl enable --now ss-bench echoserver
UNITS

    # Verify services are running
    sleep 2
    blue "Verifying services..."
    ssh $ssh_opts "root@$ip" "systemctl is-active ss-bench echoserver"

    # Open firewall (DO droplets have no firewall by default, but just in case)
    ssh $ssh_opts "root@$ip" "ufw allow $SS_PORT/tcp 2>/dev/null; ufw allow $ECHO_PORT/tcp 2>/dev/null; true"

    echo ""
    green "=== Remote benchmark environment ready ==="
    echo ""
    echo "  Droplet IP:   $ip"
    echo "  SS server:    $ip:$SS_PORT"
    echo "  Echo server:  $ip:$ECHO_PORT"
    echo ""
    echo "  Run benchmarks:"
    echo "    REMOTE_HOST=$ip go test -tags=remote -bench=. -benchmem -benchtime=3s -count=1"
    echo ""
    echo "  Tear down:"
    echo "    $0 down"
    echo ""
}

cmd_down() {
    local droplet_id
    droplet_id=$(get_droplet_id)
    if [ -z "$droplet_id" ]; then
        echo "No droplet found with tag '$TAG'"
        return
    fi

    blue "Destroying droplet $droplet_id..."
    doctl compute droplet delete "$droplet_id" --force
    green "Droplet destroyed."
}

cmd_status() {
    local ip
    ip=$(get_droplet_ip)
    if [ -z "$ip" ]; then
        echo "No droplet found with tag '$TAG'"
        return
    fi
    echo "Droplet IP: $ip"
    doctl compute droplet list --tag-name "$TAG" --format ID,Name,PublicIPv4,Status,Region,Memory
}

cmd_ssh() {
    local ip
    ip=$(get_droplet_ip)
    if [ -z "$ip" ]; then
        red "No droplet found with tag '$TAG'"
        exit 1
    fi
    local ssh_key="$HOME/.ssh/${SSH_KEY_NAME}"
    exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$ssh_key" "root@$ip"
}

case "${1:-help}" in
    up)     cmd_up ;;
    down)   cmd_down ;;
    status) cmd_status ;;
    ssh)    cmd_ssh ;;
    *)
        echo "Usage: $0 {up|down|status|ssh}"
        echo ""
        echo "  up      Create droplet and start ss-server + echo server"
        echo "  down    Destroy the droplet"
        echo "  status  Show droplet IP and status"
        echo "  ssh     SSH into the droplet"
        exit 1
        ;;
esac
