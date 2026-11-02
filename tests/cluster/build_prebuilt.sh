#!/usr/bin/env bash
# Builds the web UI and a static binary on the host, then a runtime image from it.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"' EXIT
(cd "$ROOT/apps/web" && npm run build >/dev/null)
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/intrabin/intrabin/internal/node.Version=${VERSION:-dev}" -o "$CTX/intrabin" ./cmd/intrabin)
mkdir -p "$CTX/data"
cp "$ROOT/tests/cluster/Dockerfile.prebuilt" "$CTX/Dockerfile"
docker build -t "${INTRABIN_IMAGE:-intrabin/intrabin:latest}" "$CTX"
