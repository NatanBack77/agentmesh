# Codenotch Linux MVP

Linux-first Codenotch-inspired notch for coding-assistant usage and activity.

This workspace follows the upstream Codenotch architecture, using the Windows
Rust/Tauri port as the closest base:

- `app/`: Tauri 2 desktop app with tray, edge notch window, settings window,
  provider polling, XDG autostart and local hook server.
- `hook/`: tiny Claude Code hook client that forwards JSON hook events to the
  app over `127.0.0.1`.

The first target is X11-friendly Linux. Wayland support is best-effort in this
MVP because absolute positioning, always-on-top, focus stealing and click-through
depend on compositor policy.

## Build

Install Rust and the Tauri Linux dependencies for your distro, including
WebKitGTK 4.1, GTK 3, Ayatana/AppIndicator and xdotool/libxdo development
packages.

```sh
cd codenotch-linux
cargo build
cargo run -p codenotch-linux
```

## Current MVP Scope

- Right-edge notch window on the primary monitor.
- Tray menu: Settings, Refresh, Reset position, Autostart, Open data dir, Quit.
- XDG autostart via `~/.config/autostart/codenotch-linux.desktop`.
- Claude usage from `~/.claude/.credentials.json` or `~/.claude/credentials.json`.
- Codex usage from `~/.codex/auth.json` and fallback rollout snapshots.
- Cursor usage from `~/.config/Cursor/User/globalStorage/state.vscdb`.
- Claude activity hooks through the `hook` binary and local HTTP server.
- Linux platform layer for `xdg-open`, `/proc` parent PID and desktop-session
  detection.

## Known Linux Constraints

- X11 is the reliable MVP target for always-on-top notch behavior.
- Wayland may fall back to tray/settings if the compositor does not permit the
  notch behavior. A later milestone should add layer-shell support.
- GUI-launched Linux apps often have a smaller `$PATH`; provider CLI discovery
  should not rely on shell dotfiles.
