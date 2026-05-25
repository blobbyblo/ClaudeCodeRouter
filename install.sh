#!/usr/bin/env bash
set -euo pipefail

REPO="blobbyblo/ClaudeCodeRouter"
BINARY_NAME="cc-router"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/cc-router"
CONFIG_FILE="$CONFIG_DIR/config.toml"

# ---------------------------------------------------------------
# Detect OS and arch
# ---------------------------------------------------------------
OS="$(uname -s)"
case "$OS" in
    Linux*)  PLATFORM="Linux"  ;;
    Darwin*) PLATFORM="Darwin" ;;
    *)       echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)         ARCH_NAME="x86_64" ;;
    aarch64|arm64)  ARCH_NAME="arm64"  ;;
    *)              echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

ASSET_NAME="${BINARY_NAME}_${PLATFORM}_${ARCH_NAME}.tar.gz"

# ---------------------------------------------------------------
# Install directory
# ---------------------------------------------------------------
if [ "$PLATFORM" = "Darwin" ]; then
    INSTALL_DIR="/usr/local/bin"
else
    INSTALL_DIR="$HOME/.local/bin"
fi

# ---------------------------------------------------------------
# Download latest release
# ---------------------------------------------------------------
echo ""
echo "==> Fetching latest release of cc-router..."

DOWNLOAD_URL="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | \
    grep '"browser_download_url"' | \
    grep "${ASSET_NAME}" | \
    sed 's/.*"browser_download_url": *"\([^"]*\)".*/\1/')"

if [ -z "$DOWNLOAD_URL" ]; then
    echo "Error: Could not find asset '$ASSET_NAME' in latest release." >&2
    exit 1
fi

echo "==> Downloading $ASSET_NAME..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

curl -fsSL "$DOWNLOAD_URL" -o "$TMP_DIR/cc-router.tar.gz"
tar -xzf "$TMP_DIR/cc-router.tar.gz" -C "$TMP_DIR"

EXTRACTED_BINARY="$(find "$TMP_DIR" -name "$BINARY_NAME" -not -name "*.tar.gz" | head -1)"
if [ -z "$EXTRACTED_BINARY" ]; then
    echo "Error: Binary '$BINARY_NAME' not found after extraction." >&2
    exit 1
fi

# ---------------------------------------------------------------
# Install binary
# ---------------------------------------------------------------
echo "==> Installing binary to $INSTALL_DIR..."
mkdir -p "$INSTALL_DIR"

if [ ! -w "$INSTALL_DIR" ]; then
    echo "   (requires sudo for $INSTALL_DIR)"
    sudo install -m 755 "$EXTRACTED_BINARY" "$INSTALL_DIR/$BINARY_NAME"
else
    install -m 755 "$EXTRACTED_BINARY" "$INSTALL_DIR/$BINARY_NAME"
fi

# ---------------------------------------------------------------
# Ensure install dir is on PATH
# ---------------------------------------------------------------
_add_path_to_rc() {
    local rc_file="$1"
    local dir="$2"
    if [ -f "$rc_file" ] && ! grep -qF "$dir" "$rc_file" 2>/dev/null; then
        printf '\nexport PATH="%s:$PATH"\n' "$dir" >> "$rc_file"
        echo "   Added $dir to PATH in $rc_file"
    fi
}

if ! echo "$PATH" | grep -qF "$INSTALL_DIR"; then
    echo "==> Adding $INSTALL_DIR to PATH..."
    _add_path_to_rc "$HOME/.bashrc" "$INSTALL_DIR"
    _add_path_to_rc "$HOME/.zshrc" "$INSTALL_DIR"
    [ "$PLATFORM" = "Darwin" ] && _add_path_to_rc "$HOME/.bash_profile" "$INSTALL_DIR"
    echo "   Restart your shell or run: export PATH=\"$INSTALL_DIR:\$PATH\""
else
    echo "==> $INSTALL_DIR is already on PATH."
fi

# ---------------------------------------------------------------
# Create sample config
# ---------------------------------------------------------------
echo "==> Creating sample config..."
mkdir -p "$CONFIG_DIR"
if [ ! -f "$CONFIG_FILE" ]; then
    cat > "$CONFIG_FILE" <<'TOML'
[server]
host = "127.0.0.1"
client_port = 8080
admin_port = 8081
log_level = "info"

# Add providers as needed
# [[provider]]
# name = "anthropic"
# api_key = "your-api-key-here"
# enabled = true
TOML
    echo "   Created: $CONFIG_FILE"
else
    echo "   Config already exists: $CONFIG_FILE"
fi

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
echo ""
echo "========================================"
echo "  cc-router installed!"
echo "========================================"
echo ""
echo "  Binary:  $INSTALL_DIR/$BINARY_NAME"
echo "  Config:  $CONFIG_FILE"
echo ""
echo "Usage:"
echo "  cc-router -config \"$CONFIG_FILE\""
echo ""

# ---------------------------------------------------------------
# Optional: install as a background service
# ---------------------------------------------------------------
printf "Install cc-router as a background service? (y/N) "
INSTALL_SERVICE="n"
read -r INSTALL_SERVICE </dev/tty 2>/dev/null || true

if [[ ! "${INSTALL_SERVICE:-n}" =~ ^[Yy]$ ]]; then
    echo "Skipping service installation."
    exit 0
fi

if [ "$PLATFORM" = "Linux" ]; then
    # systemd user service
    SYSTEMD_DIR="$HOME/.config/systemd/user"
    mkdir -p "$SYSTEMD_DIR"
    cat > "$SYSTEMD_DIR/cc-router.service" <<EOF
[Unit]
Description=Claude Code Router
After=network.target

[Service]
ExecStart=$INSTALL_DIR/$BINARY_NAME -config $CONFIG_FILE
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
    echo "==> Enabling and starting systemd user service..."
    systemctl --user daemon-reload
    systemctl --user enable cc-router
    systemctl --user start cc-router
    echo ""
    if systemctl --user is-active --quiet cc-router; then
        echo "Service is running."
    else
        echo "Service may have failed to start. Check: journalctl --user -u cc-router" >&2
        exit 1
    fi

elif [ "$PLATFORM" = "Darwin" ]; then
    # launchd plist
    PLIST_DIR="$HOME/Library/LaunchAgents"
    PLIST_FILE="$PLIST_DIR/com.blobbyblo.cc-router.plist"
    LOG_DIR="$HOME/Library/Logs/cc-router"
    mkdir -p "$PLIST_DIR" "$LOG_DIR"
    cat > "$PLIST_FILE" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.blobbyblo.cc-router</string>
    <key>ProgramArguments</key>
    <array>
        <string>$INSTALL_DIR/$BINARY_NAME</string>
        <string>-config</string>
        <string>$CONFIG_FILE</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$LOG_DIR/stdout.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR/stderr.log</string>
</dict>
</plist>
EOF
    echo "==> Loading launchd service..."
    launchctl load "$PLIST_FILE"
    echo ""
    echo "Service loaded."
    echo "Check status: launchctl list | grep cc-router"
    echo "Logs: $LOG_DIR"
fi

echo ""
echo "========================================"
echo "  Service installed!"
echo "========================================"
