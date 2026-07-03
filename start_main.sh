#!/bin/bash
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "[1/2] Building frontend..."
cd "$SCRIPT_DIR/web/frontend"
npm run build:h5

echo "[2/2] Starting server..."
cd "$SCRIPT_DIR"
go run main.go
