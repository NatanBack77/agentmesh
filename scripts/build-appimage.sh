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
# app/ is a workspace member, so cargo puts
# target/ at the workspace root, not under app/.
"$ENGINE" run --rm \
    -v "$REPO_ROOT:/work:z" \
    -v "$CARGO_CACHE_VOLUME:/root/.cargo/registry:z" \
    -w /work/codenotch-linux/app \
    "$IMAGE_TAG" \
    bash -c 'set -euo pipefail
        cargo tauri build --bundles appimage
        out_dir=/work/codenotch-linux/target/release/bundle/appimage
        appimage=$(find "$out_dir" -iname "*.AppImage" | head -n1)
        if [ -z "$appimage" ]; then
            echo "AppImage não encontrado em $out_dir" >&2
            exit 1
        fi

        # libwayland-client/cursor/egl/server are protocol/Mesa-coupled to
        # the host compositor and driver: bundling a version from the
        # (older) Ubuntu 22.04 builder causes EGL_BAD_PARAMETER aborts in
        # WebKitGTK on newer host Mesa (confirmed via LD_PRELOAD A/B test).
        # The official AppImage excludelist agrees these must come from the
        # host, so strip them here and re-pack instead of trusting them in
        # the bundle.
        echo "==> Removendo libwayland-* empacotadas e reempacotando..."
        work_dir=$(mktemp -d)
        cd "$work_dir"
        "$appimage" --appimage-extract >/dev/null
        rm -f squashfs-root/usr/lib/libwayland-client.so* \
              squashfs-root/usr/lib/libwayland-cursor.so* \
              squashfs-root/usr/lib/libwayland-egl.so* \
              squashfs-root/usr/lib/libwayland-server.so*
        rm -f "$appimage"
        ARCH=x86_64 appimagetool squashfs-root "$appimage"
        cd /
        rm -rf "$work_dir"
    '

OUT_DIR="$REPO_ROOT/codenotch-linux/target/release/bundle/appimage"
echo
echo "==> Pronto. AppImage em:"
find "$OUT_DIR" -iname "*.AppImage" 2>/dev/null || echo "    (não encontrado em $OUT_DIR — confira o log acima)"
