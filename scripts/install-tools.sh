#!/usr/bin/env bash
# install-tools.sh — Install CI tools locally (golangci-lint, kubeconform)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

mkdir -p "$BIN_DIR"

# --- golangci-lint ---
echo ">> Installing golangci-lint v2.11.4 ..."
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b "$BIN_DIR" v2.11.4
echo "  Installed: $("$BIN_DIR/golangci-lint" version)"

# --- kubeconform ---
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
esac

echo ">> Installing kubeconform ($OS/$ARCH) ..."
KUBECONFORM_URL="https://github.com/yannh/kubeconform/releases/latest/download/kubeconform-${OS}-${ARCH}.tar.gz"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT
curl -sSfL "$KUBECONFORM_URL" | tar xz -C "$TMPDIR" kubeconform
mv "$TMPDIR/kubeconform" "$BIN_DIR/kubeconform"
chmod +x "$BIN_DIR/kubeconform"
echo "  Installed: $("$BIN_DIR/kubeconform" -version)"

echo ""
echo "Tools installed to $BIN_DIR"
echo "  $BIN_DIR/golangci-lint"
echo "  $BIN_DIR/kubeconform"
