#!/usr/bin/env bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COV_DIR="$PROJECT_ROOT/output"
COV_FILE="$COV_DIR/coverage.txt"

mkdir -p "$COV_DIR"
: > "$COV_FILE"

for d in $(cd "$PROJECT_ROOT" && go list ./... | grep -v "vendor/"); do
    (cd "$PROJECT_ROOT" && go test -race -coverprofile="$COV_DIR/c.out" "$d")
    if [ -f "$COV_DIR/c.out" ]; then
        # Skip header line from all but the first package
        if [ -s "$COV_FILE" ]; then
            tail -n +2 "$COV_DIR/c.out" >> "$COV_FILE"
        else
            cat "$COV_DIR/c.out" >> "$COV_FILE"
        fi
        rm "$COV_DIR/c.out"
    fi
done
