# Builds the MeshNotch AppImage against Ubuntu 22.04's glibc/webkitgtk so the
# resulting AppImage runs on the widest range of Linux distros (glibc >= 2.35
# covers Ubuntu 22.04+, Debian 12+, Fedora 36+, and everything newer). Building
# natively on a newer host (e.g. Fedora 44) links a newer glibc that older
# distros' AppImage runtime can't satisfy — this container sidesteps that.
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ENV CARGO_HOME=/root/.cargo
ENV PATH=/root/.cargo/bin:$PATH

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates build-essential pkg-config file git \
    libgtk-3-dev libwebkit2gtk-4.1-dev librsvg2-dev \
    libayatana-appindicator3-dev \
    libfuse2 patchelf \
    && rm -rf /var/lib/apt/lists/*

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable
RUN cargo install tauri-cli --version "^2.0" --locked

# ureq's native-tls feature needs openssl-sys, which needs libssl-dev at build
# time (separate RUN so a future edit here doesn't invalidate the tauri-cli
# compile cached above).
RUN apt-get update && apt-get install -y --no-install-recommends libssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /work
