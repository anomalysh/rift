package ingress

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/config"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadErrorPagesRenders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "502.html", "<h1>{{status}}</h1><p>{{code}}: {{message}}</p>")
	writeFile(t, dir, "error.html", "generic {{status}}")
	writeFile(t, dir, "notes.txt", "ignored") // non-template file is skipped

	ep := loadErrorPages(dir, discardLogger())
	if ep == nil {
		t.Fatal("expected templates to load")
	}

	// Exact status template wins and placeholders are substituted.
	body, ok := ep.render(502, "tunnel_unavailable", "down")
	if !ok {
		t.Fatal("502 template not found")
	}
	if body != "<h1>502</h1><p>tunnel_unavailable: down</p>" {
		t.Fatalf("unexpected render: %q", body)
	}

	// A status without its own file falls back to error.html.
	fb, ok := ep.render(404, "not_a_tunnel", "nope")
	if !ok || fb != "generic 404" {
		t.Fatalf("fallback render = %q ok=%v", fb, ok)
	}
}

func TestLoadErrorPagesEmptyOrMissing(t *testing.T) {
	if loadErrorPages("", discardLogger()) != nil {
		t.Fatal("an empty dir must disable the feature")
	}
	if loadErrorPages(filepath.Join(t.TempDir(), "does-not-exist"), discardLogger()) != nil {
		t.Fatal("a missing dir must disable the feature, not panic")
	}
	// A dir with no usable templates disables the feature too.
	empty := t.TempDir()
	writeFile(t, empty, "readme.md", "hi")
	if loadErrorPages(empty, discardLogger()) != nil {
		t.Fatal("a dir with no <status>.html/error.html must disable the feature")
	}
}

func TestWriteGatewayErrorServesBrandedPage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "403.html", "<html>blocked {{status}}</html>")
	i := &Ingress{
		cfg:        &config.Config{Tunnel: config.Tunnel{PublicScheme: "https"}},
		logger:     discardLogger(),
		errorPages: loadErrorPages(dir, discardLogger()),
	}

	// A browser (no JSON Accept) gets the branded HTML page.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test/x", nil)
	i.writeGatewayError(w, r, http.StatusForbidden, "ip_forbidden", "no")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "blocked 403") {
		t.Fatalf("branded body not served: %q", w.Body.String())
	}

	// A JSON client still gets JSON, not the HTML page.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "http://app.rift.test/x", nil)
	r.Header.Set("Accept", "application/json")
	i.writeGatewayError(w, r, http.StatusForbidden, "ip_forbidden", "no")
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("json client got content-type %q", ct)
	}

	// A status with no template falls back to the built-in plain-text body.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "http://app.rift.test/x", nil)
	i.writeGatewayError(w, r, http.StatusBadGateway, "tunnel_unavailable", "down")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("uncovered status content-type = %q, want text/plain", ct)
	}
}
