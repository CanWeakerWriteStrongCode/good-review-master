#!/bin/bash
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "[1/3] Building frontend..."
cd "$SCRIPT_DIR/web/frontend"
npm install && npm run build:h5

echo "[2/3] Copying to embed directory..."
rm -rf "$SCRIPT_DIR/web/server/static/frontend"
mkdir -p "$SCRIPT_DIR/web/server/static"
cp -r dist/build/h5 "$SCRIPT_DIR/web/server/static/frontend"

echo "[3/3] Building Go binary..."
cd "$SCRIPT_DIR"
go build -o good-review-master .
echo "Build success: good-review-master"
