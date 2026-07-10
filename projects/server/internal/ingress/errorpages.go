package ingress

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// errorPages holds branded gateway error templates loaded once at startup (T4).
// A page is chosen by exact status ("502.html"), falling back to "error.html".
// Templates are read from disk once, not per request, and the only paths opened
// are ones this code constructs -- a visitor cannot steer the lookup, so there
// is no path-traversal surface.
type errorPages struct {
	byStatus map[int]string
	fallback string // "error.html" contents, or "" if absent
}

// errorPageName matches the per-status template files we load from the dir.
var errorPageName = regexp.MustCompile(`^([1-5][0-9]{2})\.html$`)

// loadErrorPages reads the templates in dir. An empty dir disables the feature
// (returns nil). Unreadable files are logged and skipped rather than failing
// boot: a missing brand page should degrade to the built-in body, not take the
// server down.
func loadErrorPages(dir string, logger *slog.Logger) *errorPages {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn("error page dir unreadable; using built-in error bodies",
			slog.String("dir", dir), slog.Any("error", err))
		return nil
	}
	ep := &errorPages{byStatus: make(map[int]string)}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			logger.Warn("skipping unreadable error page",
				slog.String("file", name), slog.Any("error", err))
			continue
		}
		if name == "error.html" {
			ep.fallback = string(body)
			continue
		}
		if m := errorPageName.FindStringSubmatch(name); m != nil {
			status, _ := strconv.Atoi(m[1])
			ep.byStatus[status] = string(body)
		}
	}
	if len(ep.byStatus) == 0 && ep.fallback == "" {
		logger.Warn("error page dir has no usable templates (<status>.html or error.html)",
			slog.String("dir", dir))
		return nil
	}
	logger.Info("loaded branded error pages",
		slog.String("dir", dir), slog.Int("count", len(ep.byStatus)))
	return ep
}

// render returns the HTML body for a status with the placeholders substituted,
// or ("", false) when no template covers it (the caller then serves the
// built-in body). Placeholders: {{status}}, {{code}}, {{message}}.
func (e *errorPages) render(status int, code, message string) (string, bool) {
	tmpl, ok := e.byStatus[status]
	if !ok {
		if e.fallback == "" {
			return "", false
		}
		tmpl = e.fallback
	}
	r := strings.NewReplacer(
		"{{status}}", strconv.Itoa(status),
		"{{code}}", code,
		"{{message}}", message,
	)
	return r.Replace(tmpl), true
}
