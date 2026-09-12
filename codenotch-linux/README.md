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

## Install (end users)

No build tools needed — download and run the AppImage:

1. Grab the latest `MeshNotch_*_amd64.AppImage` from the
   [Releases page](https://github.com/NatanBack77/agentmesh/releases).
2. Make it executable and run it:
   ```sh
   chmod +x MeshNotch_*_amd64.AppImage
   ./MeshNotch_*_amd64.AppImage
   ```
3. The notch appears on the right edge of your primary monitor, and a tray
   icon is added (Settings, Refresh, Autostart, Quit).

If it fails to start with an error mentioning `libfuse.so.2`, your distro
dropped FUSE2 by default (Ubuntu 24.04+, Fedora 36+). Either install it —
`sudo apt install libfuse2t64` (Ubuntu) or `sudo dnf install fuse-libs`
(Fedora) — or run the AppImage without FUSE:
```sh
./MeshNotch_*_amd64.AppImage --appimage-extract-and-run
```

**Updating**: open Settings → Atualizações → *Verificar atualização*. MeshNotch
checks the latest GitHub Release, and if a newer signed build is available it
downloads and installs it in place — no need to re-download the AppImage by
hand.

**Autostart**: enable it from the tray menu or Settings to have MeshNotch
launch automatically on login (writes
`~/.config/autostart/meshnotch.desktop`).

**Uninstalling**: quit the app, delete the AppImage file, and remove
`~/.config/meshnotch/` and `~/.config/autostart/meshnotch.desktop` if you
enabled autostart. The application menu entry and icon are installed under
`~/.local/share/applications/meshnotch.desktop` and
`~/.local/share/icons/hicolor/32x32/apps/meshnotch.png`; remove those too if
you want a complete uninstall.

**Application menu**: on first launch, MeshNotch adds itself to the desktop
application launcher. For an AppImage, the launcher points to the stable
`.AppImage` path rather than its temporary mounted executable. If you move or
rename the AppImage, open Settings and use **Add to application menu** to
refresh the launcher path.

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

If a signing key or its password has been exposed, rotate the pair before the
next signed release. Anyone holding the old private key must securely discard
it; replace both GitHub Actions secrets with the new private key and its new
password before publishing another signed release. The public key in
`app/tauri.conf.json` must match that new pair.

For this rotation, the new private key and its password are stored locally at
`~/.tauri/meshnotch.key` and `~/.tauri/meshnotch.key.password` respectively.
Update the Actions secrets directly from those files (without printing their
contents) or paste them into GitHub's secret editor, then securely delete any
obsolete private-key copies.

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
- Startup diagnostics (including GTK/WebKit stdout/stderr) are tee'd to
  `~/.config/meshnotch/app.log` and the original output streams. Wayland
  launches disable WebKit compositing and force software GL before GTK starts;
  KDE/Wayland filters the known
  incompatible appmenu/window-decoration modules from `GTK_MODULES` while
  retaining other modules. Other desktop sessions retain their configuration.
  The tray is initialized before either WebView is created, so a recoverable
  window creation failure still leaves tray controls available.
