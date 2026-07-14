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

echo "[3/3] Building Go binaries (cross-compile)..."
cd "$SCRIPT_DIR"
mkdir -p dist

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS="-s -w -X good-review-master/version.Version=${VERSION} -X good-review-master/version.Commit=${COMMIT} -X good-review-master/version.BuildTime=${BUILD_TIME}"

GOOS=windows GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/good-review-master-windows-amd64.exe .
GOOS=linux   GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/good-review-master-linux-amd64 .
GOOS=darwin  GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o dist/good-review-master-darwin-amd64 .
GOOS=darwin  GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o dist/good-review-master-darwin-arm64 .

echo "Build success:"
ls -lh dist/
