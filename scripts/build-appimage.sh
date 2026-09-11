#!/usr/bin/env bash
# Builds the MeshNotch AppImage inside an Ubuntu 22.04 container so the
# result runs on the widest range of Linux distros (Ubuntu, Debian, Fedora,
# Arch, ...) instead of being tied to this machine's (newer) glibc.
#
# First run builds the docker image (rustup + tauri-cli compiled once,
# cached); later runs reuse it and are much faster.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_TAG="meshnotch-appimage-builder"
CARGO_CACHE_VOLUME="meshnotch-cargo-cache"

ENGINE="${CONTAINER_ENGINE:-docker}"
if ! command -v "$ENGINE" >/dev/null 2>&1; then
    if command -v podman >/dev/null 2>&1; then
        ENGINE="podman"
    else
        echo "Nem docker nem podman encontrados no PATH." >&2
        exit 1
    fi
fi

echo "==> Usando engine: $ENGINE"
echo "==> Buildando imagem builder (Ubuntu 22.04 + Rust + tauri-cli)..."
"$ENGINE" build -t "$IMAGE_TAG" -f "$REPO_ROOT/codenotch-linux/docker/ubuntu-appimage.Dockerfile" "$REPO_ROOT/codenotch-linux/docker"

echo "==> Compilando AppImage dentro do container..."
"$ENGINE" run --rm \
    -v "$REPO_ROOT:/work:z" \
    -v "$CARGO_CACHE_VOLUME:/root/.cargo/registry:z" \
    -w /work/codenotch-linux/app \
    "$IMAGE_TAG" \
    cargo tauri build --bundles appimage

OUT_DIR="$REPO_ROOT/codenotch-linux/app/target/release/bundle/appimage"
echo
echo "==> Pronto. AppImage em:"
find "$OUT_DIR" -iname "*.AppImage" 2>/dev/null || echo "    (não encontrado em $OUT_DIR — confira o log acima)"
