package mesh

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A browser can't hand a web page the real OS path behind its native
// folder picker (File System Access API deliberately hides it — same
// reason Google Drive/VS Code Web can't either). But this dashboard only
// ever talks to 127.0.0.1, and the Go process behind it already has full
// filesystem access (it's the same trust boundary /spawn already assumes
// for --cwd), so a small server-side directory listing gets the same
// practical result: click through real folders, get back a real absolute
// path — no typing, no guessing at spelling.
type fsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type fsListResponse struct {
	Path    string    `json:"path"`
	Parent  string    `json:"parent,omitempty"`
	Entries []fsEntry `json:"entries"`
	Error   string    `json:"error,omitempty"`
}

// handleFSBrowse lists the subdirectories of ?path= (defaults to $HOME).
// Read-only, directories only, hidden (dot) entries skipped. Unreadable or
// missing paths fall back to $HOME rather than erroring, so a stale/typed
// path in the picker never dead-ends the UI.
func (e *Engine) handleFSBrowse(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, fsListResponse{Error: err.Error()})
		return
	}

	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		reqPath = home
	}
	abs, err := filepath.Abs(expandHome(reqPath))
	if err != nil {
		abs = home
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		abs = home
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, http.StatusOK, fsListResponse{Path: abs, Error: "sem permissão de leitura: " + err.Error()})
		return
	}

	entries := make([]fsEntry, 0, len(dirEntries))
	for _, en := range dirEntries {
		name := en.Name()
		if strings.HasPrefix(name, ".") || !en.IsDir() {
			continue
		}
		entries = append(entries, fsEntry{Name: name, Path: filepath.Join(abs, name)})
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name) })

	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	writeJSON(w, http.StatusOK, fsListResponse{Path: abs, Parent: parent, Entries: entries})
}
