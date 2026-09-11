# UI Brief

Pixel owns the complete frontend polish for `app/ui/notch.html` and
`app/ui/settings.html`.

Hard requirements:

- macOS Sonoma/Sequoia-inspired glassmorphism.
- Keep the prepared light/dark theme toggle from `../codenotch-linux-ui-base`
  and its `localStorage` key, `codenotch.theme`.
- Dense desktop utility feel, not a marketing page.
- Translucent panels with fallbacks for reduced/unsupported transparency.
- Provider rings must remain readable on light and dark wallpapers.
- No card nesting.
- Stable dimensions: hover, loading and long labels must not resize the notch.

Backend contract:

- `get_state()` returns:
  - `providers`: array of provider cards
  - `sessions`: activity sessions
  - `theme`: `"system" | "light" | "dark"`
  - `notch_visible`, `tray_visible`, `scale`
- Events actually emitted by the backend today:
  - `state`: full state snapshot (providers + sessions + theme + scale + notch/tray visibility)
  - `theme`: theme mode update
  - `scale`: scale update
  - `activity`: session/activity changed (payload-less signal; read fresh state from `state`)
  - `usage` and `pointer_left` are NOT emitted by the backend yet — don't rely on them.
- Commands (exact names registered in `invoke_handler!`):
  - `get_state`
  - `refresh_all_cmd`
  - `open_data_dir`
  - `open_provider_page`
  - `set_theme`
  - `set_scale`
  - `set_autostart`
  - `autostart_status`
  - `linux_probe`
  - `set_hot`
  - `drag_begin`
  - `open_settings` (shows/focuses the settings window; use for a settings affordance in the notch)
