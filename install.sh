#!/usr/bin/env bash
# Install mvs from a published release archive or a local build.
#
# Usage:
#   ./install.sh                      # build from source (needs Go) and install to ~/.local/bin
#   PREFIX=/usr/local ./install.sh    # install to /usr/local/bin instead
#
# The script is intentionally minimal: it does not curl from the internet.
# Pair it with a GoReleaser archive later.

set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"

if ! command -v go >/dev/null 2>&1; then
    echo "error: Go is required to build mvs from source." >&2
    echo "       install Go (mise use go@latest) or download a prebuilt binary" >&2
    exit 1
fi

echo "building mvs..."
make build

mkdir -p "$BIN_DIR"
install -m 0755 mvs "$BIN_DIR/mvs"
echo "installed: $BIN_DIR/mvs"

case ":$PATH:" in
    *":$BIN_DIR:"*)
        ;;
    *)
        cat <<EOF >&2

warning: $BIN_DIR is not on your \$PATH.
add this to your shell profile (~/.zshrc, ~/.bashrc, etc.):

    export PATH="\$HOME/.local/bin:\$PATH"

EOF
        ;;
esac

echo "run \`mvs doctor\` to verify which agents were detected."
