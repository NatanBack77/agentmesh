# MeshNotch Linux

Linux-first MeshNotch notch for coding-assistant usage and activity.

This workspace is the Rust/Tauri Linux application:

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
cargo run -p meshnotch
```

## Current MVP Scope

- Right-edge notch window on the primary monitor.
- Tray menu: Settings, Refresh, Reset position, Autostart, Open data dir, Quit.
- XDG autostart via `~/.config/autostart/meshnotch.desktop`.
- Claude usage from `~/.claude/.credentials.json` or `~/.claude/credentials.json`.
- Codex usage from `~/.codex/auth.json` and fallback rollout snapshots.
- Cursor usage from `~/.config/Cursor/User/globalStorage/state.vscdb`.
- Claude activity hooks through the `hook` binary and local HTTP server.
- Linux platform layer for `xdg-open`, `/proc` parent PID and desktop-session
  detection.

## Signed updates

The updater uses Tauri's signed AppImage updater and the static
`latest.json` published on the GitHub Release. The public key is committed in
`app/tauri.conf.json`; the private key must never be committed, logged or
shared.

Generate or rotate the signing pair outside the repository:

```sh
cargo tauri signer generate -w ~/.tauri/meshnotch.key
```

Store the contents of `~/.tauri/meshnotch.key` in the GitHub Actions secret
`TAURI_SIGNING_PRIVATE_KEY`, and store the key password in
`TAURI_SIGNING_PRIVATE_KEY_PASSWORD`. Configure both secrets manually in the
repository's GitHub Settings → Secrets and variables → Actions. Do not paste
the private key into source files, workflow YAML, issues or chat logs.

To publish a release, update the version in `app/Cargo.toml` and
`app/tauri.conf.json`, commit the change, then push a tag:

```sh
git tag meshnotch-v0.1.1
git push origin meshnotch-v0.1.1
```

The release workflow signs the AppImage, creates the `.sig` file and
`latest.json`, and attaches all three assets to the GitHub Release. Existing
installations then find the new release through the configured updater
endpoint.

## Known Linux Constraints

- X11 is the reliable MVP target for always-on-top notch behavior.
- Wayland may fall back to tray/settings if the compositor does not permit the
  notch behavior. A later milestone should add layer-shell support.
- GUI-launched Linux apps often have a smaller `$PATH`; provider CLI discovery
  should not rely on shell dotfiles.
