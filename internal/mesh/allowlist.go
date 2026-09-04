package mesh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AllowlistEnvVar names the environment variable that, when set (even to a
// single entry), overrides the on-disk allowlist file entirely —
// colon-separated absolute (or ~-relative) paths.
const AllowlistEnvVar = "AGENTMESH_ALLOWED_DIRS"

// AllowlistPath returns ~/.agentmesh/allowlist, the on-disk allowlist file
// consulted by Spawn when AllowlistEnvVar is unset. One path per line,
// blank lines and lines starting with "#" ignored, "~" expanded.
func AllowlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentmesh", "allowlist"), nil
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// loadAllowlist returns the configured allowed directories (made absolute),
// and whether an allowlist is configured at all. No allowlist configured
// (env unset AND file missing/empty/comments-only) means unrestricted —
// Spawn keeps its historical behavior of accepting any --cwd, so installs
// that predate this feature don't suddenly start rejecting spawns.
func loadAllowlist() (dirs []string, configured bool, err error) {
	if v := os.Getenv(AllowlistEnvVar); v != "" {
		for _, p := range strings.Split(v, ":") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			abs, err := filepath.Abs(expandHome(p))
			if err != nil {
				return nil, true, fmt.Errorf("%s: caminho inválido %q: %w", AllowlistEnvVar, p, err)
			}
			dirs = append(dirs, abs)
		}
		return dirs, true, nil
	}

	path, err := AllowlistPath()
	if err != nil {
		// Can't resolve $HOME — treat as unconfigured rather than fatal.
		return nil, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		abs, err := filepath.Abs(expandHome(line))
		if err != nil {
			continue
		}
		dirs = append(dirs, abs)
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return dirs, len(dirs) > 0, nil
}

// withinAny reports whether target (already absolute) is one of dirs or a
// descendant of one of them.
func withinAny(target string, dirs []string) bool {
	for _, d := range dirs {
		rel, err := filepath.Rel(d, target)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// checkAllowed validates cwd against the configured allowlist (if any).
// Symlinks are resolved on both sides first, so a symlink sitting inside an
// allowed directory can't be used to point a spawn outside of it. Returns
// nil when no allowlist is configured (see loadAllowlist).
func checkAllowed(cwd string) error {
	dirs, configured, err := loadAllowlist()
	if err != nil {
		return fmt.Errorf("lendo allowlist de diretórios: %w", err)
	}
	if !configured {
		return nil
	}

	resolved := cwd
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = real
	}
	resolvedDirs := make([]string, len(dirs))
	for i, d := range dirs {
		resolvedDirs[i] = d
		if real, err := filepath.EvalSymlinks(d); err == nil {
			resolvedDirs[i] = real
		}
	}

	if withinAny(resolved, resolvedDirs) {
		return nil
	}
	return fmt.Errorf(
		"%q está fora da allowlist de diretórios (%s) — adicione com `agentmesh allowlist add %s`, "+
			"ou libere geral removendo/esvaziando %s (ou a var %s)",
		cwd, strings.Join(dirs, ", "), cwd, mustAllowlistPath(), AllowlistEnvVar,
	)
}

func mustAllowlistPath() string {
	p, err := AllowlistPath()
	if err != nil {
		return "~/.agentmesh/allowlist"
	}
	return p
}
