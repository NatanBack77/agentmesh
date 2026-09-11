const THEME_KEY = "codenotch.theme";
const themeQuery = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;

function tauriApi() {
  return window.__TAURI__ || null;
}

function invoke(command, payload) {
  const core = tauriApi()?.core;
  if (!core?.invoke) return Promise.resolve(null);
  return core.invoke(command, payload);
}

function listen(event, handler) {
  const eventApi = tauriApi()?.event;
  if (!eventApi?.listen) return Promise.resolve(() => {});
  return eventApi.listen(event, handler);
}

function explicitTheme() {
  const theme = document.documentElement.dataset.theme;
  return theme === "light" || theme === "dark" ? theme : "";
}

function effectiveTheme() {
  return explicitTheme() || (themeQuery && themeQuery.matches ? "dark" : "light");
}

function applyTheme(theme, persist = true) {
  if (theme === "system") {
    document.documentElement.removeAttribute("data-theme");
    try {
      if (persist) localStorage.removeItem(THEME_KEY);
    } catch (_) {}
    updateThemeToggle();
    return;
  }
  if (theme !== "light" && theme !== "dark") return;

  document.documentElement.setAttribute("data-theme-switching", "");
  document.documentElement.dataset.theme = theme;
  try {
    if (persist) localStorage.setItem(THEME_KEY, theme);
  } catch (_) {}
  updateThemeToggle();
  window.setTimeout(() => document.documentElement.removeAttribute("data-theme-switching"), 80);
}

function updateThemeToggle() {
  const button = document.getElementById("theme-toggle");
  const icon = document.getElementById("theme-icon");
  const label = document.getElementById("theme-label");
  if (!button || !icon || !label) return;

  const dark = effectiveTheme() === "dark";
  button.setAttribute("aria-pressed", String(dark));
  button.setAttribute("aria-label", `Tema atual: ${dark ? "escuro" : "claro"}. Alternar tema.`);
  icon.textContent = dark ? "◐" : "○";
  label.textContent = dark ? "Escuro" : "Claro";

  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = dark ? "#101014" : "#f5f5f7";
}

function setTheme(theme, persist = true) {
  if (theme !== "light" && theme !== "dark" && theme !== "system") return Promise.resolve();
  applyTheme(theme, persist);
  return invoke("set_theme", { theme }).catch((error) => {
    console.warn("MeshNotch: backend theme sync failed", error);
  });
}

async function syncState(state) {
  if (!state || typeof state !== "object") return;
  if (state.theme) applyTheme(state.theme, state.theme !== "system");
  document.dispatchEvent(new CustomEvent("codenotch:state", { detail: state }));
}

async function bootThemeToggle() {
  document.getElementById("theme-toggle")?.addEventListener("click", () => {
    setTheme(effectiveTheme() === "dark" ? "light" : "dark");
  });

  if (themeQuery) {
    themeQuery.addEventListener("change", () => {
      if (!explicitTheme()) updateThemeToggle();
    });
  }

  updateThemeToggle();

  const listeners = ["state", "usage", "activity", "theme", "scale", "pointer_left"];
  listeners.forEach((event) => {
    listen(event, (payload) => {
      const value = payload?.payload ?? payload;
      if (event === "theme" && typeof value === "string") applyTheme(value, value !== "system");
      if (event === "state") syncState(value);
      document.dispatchEvent(new CustomEvent(`codenotch:${event}`, { detail: value }));
    });
  });

  try {
    const state = await invoke("get_state");
    await syncState(state);
  } catch (error) {
    console.warn("MeshNotch: state bootstrap failed", error);
  }
}

window.MeshNotchTheme = {
  effectiveTheme,
  explicitTheme,
  applyTheme,
  invoke,
  listen,
  setTheme,
  syncState,
  updateThemeToggle,
};

bootThemeToggle();
