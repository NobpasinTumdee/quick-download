#!/usr/bin/env bash
#
# Registers the Quick Download engine as a Chrome Native Messaging host on
# Linux and macOS. Unlike Windows there is no registry: the manifest simply has
# to sit in a well-known per-user directory.
#
#   Usage: ./install-unix.sh <extension-id> [--skip-build]

set -euo pipefail

HOST_NAME="com.downloader.app"
EXT_ID="${1:-}"
SKIP_BUILD="${2:-}"

if [[ ! "$EXT_ID" =~ ^[a-p]{32}$ ]]; then
  echo "usage: $0 <32-char-extension-id> [--skip-build]" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$REPO_ROOT/bin"
EXE="$BIN_DIR/quick-download"
MANIFEST="$BIN_DIR/$HOST_NAME.json"

if [[ "$SKIP_BUILD" != "--skip-build" ]]; then
  echo "==> Building the Go engine"
  mkdir -p "$BIN_DIR"
  (cd "$REPO_ROOT/backend" && go build -trimpath -ldflags '-s -w' -o "$EXE" .)
  chmod +x "$EXE"
fi

[[ -x "$EXE" ]] || { echo "engine not found at $EXE" >&2; exit 1; }

echo "==> Writing the host manifest"
cat > "$MANIFEST" <<JSON
{
  "name": "$HOST_NAME",
  "description": "Quick Download local engine",
  "path": "$EXE",
  "type": "stdio",
  "allowed_origins": [
    "chrome-extension://$EXT_ID/"
  ]
}
JSON

# Every Chromium flavour reads the manifest from its own config directory.
if [[ "$OSTYPE" == darwin* ]]; then
  TARGETS=(
    "$HOME/Library/Application Support/Google/Chrome/NativeMessagingHosts"
    "$HOME/Library/Application Support/Microsoft Edge/NativeMessagingHosts"
    "$HOME/Library/Application Support/Chromium/NativeMessagingHosts"
    "$HOME/Library/Application Support/BraveSoftware/Brave-Browser/NativeMessagingHosts"
  )
else
  TARGETS=(
    "$HOME/.config/google-chrome/NativeMessagingHosts"
    "$HOME/.config/microsoft-edge/NativeMessagingHosts"
    "$HOME/.config/chromium/NativeMessagingHosts"
    "$HOME/.config/BraveSoftware/Brave-Browser/NativeMessagingHosts"
  )
fi

echo "==> Installing the manifest"
for dir in "${TARGETS[@]}"; do
  parent="$(dirname "$dir")"
  # Only install for browsers that are actually present on this machine.
  if [[ -d "$parent" ]]; then
    mkdir -p "$dir"
    cp "$MANIFEST" "$dir/$HOST_NAME.json"
    echo "    $dir/$HOST_NAME.json"
  fi
done

cat <<EOF

==> Done
    Engine    : $EXE
    Extension : $EXT_ID

    Restart Chrome completely, then open the popup.
    Dashboard: http://127.0.0.1:9090
    Logs:      \${XDG_CONFIG_HOME:-\$HOME/.config}/quick-download/
EOF
