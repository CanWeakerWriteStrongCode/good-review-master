#!/bin/bash
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "[1/2] Building frontend..."
cd "$SCRIPT_DIR/web/frontend"
npm run build:h5

echo "[2/2] Building Go binary..."
cd "$SCRIPT_DIR"
go build -o good-review-master .
echo "Build success: good-review-master"
