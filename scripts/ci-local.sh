#!/usr/bin/env bash
# ci-local.sh — Run all CI checks locally (test, lint, helm-check)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

pass=0
fail=0
skip=0

ok()   { echo "  [PASS] $1"; pass=$((pass + 1)); }
err()  { echo "  [FAIL] $1"; fail=$((fail + 1)); }
skip() { echo "  [SKIP] $1"; skip=$((skip + 1)); }

echo "=== draino local CI ==="
echo ""

# --- test ---
echo ">> test (./scripts/test.sh)"
if (cd "$PROJECT_ROOT" && bash ./scripts/test.sh > /dev/null 2>&1); then
    # test.sh outputs to output/coverage.txt
    ok "go tests"
else
    err "go tests"
fi

# --- lint ---
echo ">> lint (golangci-lint)"
LINT_BIN=""
if command -v golangci-lint &>/dev/null; then
    LINT_BIN="golangci-lint"
elif [ -x "$BIN_DIR/golangci-lint" ]; then
    LINT_BIN="$BIN_DIR/golangci-lint"
fi

if [ -z "$LINT_BIN" ]; then
    skip "golangci-lint not found (install: scripts/install-tools.sh)"
else
    if (cd "$PROJECT_ROOT" && GOGC=10 "$LINT_BIN" run --timeout 5m ./... > /dev/null 2>&1); then
        ok "golangci-lint"
    else
        err "golangci-lint"
    fi
fi

# --- helm-check ---
echo ">> helm-check (helm template + kubeconform)"
if ! command -v helm &>/dev/null; then
    skip "helm not found"
elif ! command -v kubeconform &>/dev/null && [ ! -x "$BIN_DIR/kubeconform" ]; then
    skip "kubeconform not found (install: scripts/install-tools.sh)"
else
    KUBECONFORM_BIN="kubeconform"
    [ ! -x "$BIN_DIR/kubeconform" ] || KUBECONFORM_BIN="$BIN_DIR/kubeconform"

    if (cd "$PROJECT_ROOT" && helm template ./helm/draino/ --set 'conditions[0]=MemoryPressure' --set 'image.tag=sha-test' 2>/dev/null | "$KUBECONFORM_BIN" --strict > /dev/null 2>&1); then
        ok "helm check"
    else
        err "helm check"
    fi

    if (cd "$PROJECT_ROOT" && helm template ./helm/draino/ --set dryRun=true --set 'image.tag=sha-test' 2>/dev/null | "$KUBECONFORM_BIN" --strict > /dev/null 2>&1); then
        ok "helm check (dry-run bypass)"
    else
        err "helm check (dry-run bypass)"
    fi
fi

# --- build ---
echo ">> build (go build)"
if (cd "$PROJECT_ROOT" && go build -o /dev/null ./cmd/draino > /dev/null 2>&1); then
    ok "go build"
else
    err "go build"
fi

# --- summary ---
echo ""
echo "=== Results: $pass passed, $fail failed, $skip skipped ==="
[ "$fail" -eq 0 ] || exit 1
