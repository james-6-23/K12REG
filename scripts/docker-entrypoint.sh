#!/bin/sh
set -e

DATA_DIR="${K12_DATA_DIR:-/data}"
PORT="${PORT:-8000}"
STATIC="${K12_STATIC_DIR:-/app/frontend/dist}"

# Always ensure data volume exists and is writable (no host-side mkdir required).
mkdir -p "$DATA_DIR"
chmod 755 "$DATA_DIR" 2>/dev/null || true

exec k12reg serve -addr ":${PORT}" -data "$DATA_DIR" -static "$STATIC"
