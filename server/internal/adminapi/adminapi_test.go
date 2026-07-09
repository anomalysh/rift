package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anomaly-sh/rift/server/internal/config"
	"github.com/anomaly-sh/rift/server/internal/core"
)

const adminToken = "rift_admin_super_secret_value"

// --- in-memory fakes ----------------------------------------------------

type fakeTokenStore struct {
	mu   sync.Mutex
	byID map[string]*core.Token
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{byID: make(map[string]*core.Token)}
}

func (s *fakeTokenStore) Create(_ context.Context, t *core.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.byID[t.ID] = &cp
	return nil
}

func (s *fakeTokenStore) FindByID(_ context.Context, id string) (*core.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.byID[id]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, core.ErrNotFound
}

func (s *fakeTokenStore) FindByHash(_ context.Context, hash string) (*core.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.TokenHash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *fakeTokenStore) List(context.Context) ([]core.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Token, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, *t)
	}
	return out, nil
}

func (s *fakeTokenStore) Revoke(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return core.ErrNotFound
	}
	t.RevokedAt = &at
	return nil
}

func (s *fakeTokenStore) TouchLastUsed(context.Context, string, time.Time) error { return nil }

type fakeReservationStore struct {
	mu    sync.Mutex
	bySub map[string]*core.Reservation
}

func newFakeReservationStore() *fakeReservationStore {
	return &fakeReservationStore{bySub: make(map[string]*core.Reservation)}
}

func (s *fakeReservationStore) Get(_ context.Context, sub string) (*core.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.bySub[sub]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, core.ErrNotFound
}

func (s *fakeReservationStore) Create(_ context.Context, r *core.Reservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bySub[r.Subdomain]; ok {
		return core.ErrConflict
	}
	cp := *r
	s.bySub[r.Subdomain] = &cp
	return nil
}

func (s *fakeReservationStore) List(context.Context) ([]core.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Reservation, 0, len(s.bySub))
	for _, r := range s.bySub {
		out = append(out, *r)
	}
	return out, nil
}

func (s *fakeReservationStore) Delete(_ context.Context, sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bySub[sub]; !ok {
		return core.ErrNotFound
	}
	delete(s.bySub, sub)
	return nil
}

type fakeTunnelStore struct {
	active []core.Tunnel
}

func (s *fakeTunnelStore) Claim(context.Context, *core.Tunnel) error          { return nil }
func (s *fakeTunnelStore) Release(context.Context, string) error              { return nil }
func (s *fakeTunnelStore) Heartbeat(context.Context, string, time.Time) error { return nil }
func (s *fakeTunnelStore) GetBySubdomain(context.Context, string) (*core.Tunnel, error) {
	return nil, core.ErrNotFound
}
func (s *fakeTunnelStore) CountByToken(context.Context, string) (int, error) { return 0, nil }
func (s *fakeTunnelStore) ListActive(context.Context) ([]core.Tunnel, error) {
	return s.active, nil
}
func (s *fakeTunnelStore) DeleteStale(context.Context, time.Time) ([]core.Tunnel, error) {
	return nil, nil
}
func (s *fakeTunnelStore) DeleteByNode(context.Context, string) (int, error) { return 0, nil }

// --- harness ------------------------------------------------------------

type harness struct {
	handler      http.Handler
	tokens       *fakeTokenStore
	reservations *fakeReservationStore
	tunnels      *fakeTunnelStore
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rules, err := core.NewSubdomainRules(3, 20, `^[a-z0-9-]+$`, []string{"admin", "www"}, core.GeneratorRandom, 8, "abcdefghijklmnopqrstuvwxyz")
	if err != nil {
		t.Fatalf("build subdomain rules: %v", err)
	}
	cfg := &config.Config{
		Admin:          config.Admin{Enabled: true, Addr: ":0", Token: adminToken},
		Tunnel:         config.Tunnel{MaxTunnelsPerToken: 5},
		SubdomainRules: rules,
	}
	h := &harness{
		tokens:       newFakeTokenStore(),
		reservations: newFakeReservationStore(),
		tunnels:      &fakeTunnelStore{},
	}
	h.handler = New(cfg, h.tokens, h.reservations, h.tunnels, nil)
	return h
}

func (h *harness) do(t *testing.T, method, target, body string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if authorized {
		req.Header.Set(config.HeaderAuthorization, config.BearerPrefix+adminToken)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

// --- tests --------------------------------------------------------------

func TestUnauthorizedWithoutBearer(t *testing.T) {
	h := newHarness(t)
	for _, target := range []string{config.RouteAdminTokens, config.RouteAdminReservations, config.RouteAdminTunnels} {
		rr := h.do(t, http.MethodGet, target, "", false)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s without bearer: status = %d, want 401", target, rr.Code)
		}
		if rr.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s: missing WWW-Authenticate challenge", target)
		}
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodGet, config.RouteHealth, "", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("health without bearer: status = %d, want 200", rr.Code)
	}
}

func TestCreateTokenReturnsPlaintextOnceAndListNeverDoes(t *testing.T) {
	h := newHarness(t)

	rr := h.do(t, http.MethodPost, config.RouteAdminTokens, `{"name":"ci"}`, true)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create token: status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created tokenCreatedView
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(created.Token, "rift_") {
		t.Fatalf("create response token %q lacks rift_ prefix", created.Token)
	}
	if created.ID == "" || created.Name != "ci" {
		t.Fatalf("unexpected create response: %+v", created)
	}
	if created.MaxTunnels != 5 {
		t.Fatalf("max_tunnels = %d, want default 5", created.MaxTunnels)
	}

	// The stored record must hold the hash, never the plaintext.
	stored, err := h.tokens.FindByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("token not persisted: %v", err)
	}
	if stored.TokenHash == created.Token || stored.TokenHash == "" {
		t.Fatal("stored TokenHash must be a hash, not the plaintext")
	}

	// The list must expose neither the plaintext nor the hash.
	listRR := h.do(t, http.MethodGet, config.RouteAdminTokens, "", true)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list tokens: status = %d, want 200", listRR.Code)
	}
	body := listRR.Body.String()
	if strings.Contains(body, created.Token) {
		t.Fatal("list response leaked the plaintext token")
	}
	if strings.Contains(body, stored.TokenHash) {
		t.Fatal("list response leaked the token hash")
	}
	if strings.Contains(body, `"token"`) {
		t.Fatal("list response contains a token field")
	}
	if !strings.Contains(body, created.ID) {
		t.Fatal("list response omitted the created token id")
	}
}

func TestCreateTokenHonoursExpiryAndMaxTunnels(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodPost, config.RouteAdminTokens,
		`{"name":"scoped","max_tunnels":2,"expires_at":"2030-01-02T03:04:05Z"}`, true)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created tokenCreatedView
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.MaxTunnels != 2 {
		t.Fatalf("max_tunnels = %d, want 2", created.MaxTunnels)
	}
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("expires_at = %v, want 2030-01-02T03:04:05Z", created.ExpiresAt)
	}
}

func TestReservationInvalidSubdomainIs400(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodPost, config.RouteAdminReservations,
		`{"subdomain":"no_good!","token_id":"whatever"}`, true)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidSubdomain)
}

func TestReservationBlocklistedSubdomainIs409(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodPost, config.RouteAdminReservations,
		`{"subdomain":"admin","token_id":"whatever"}`, true)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeConflict)
}

func TestReservationUnknownTokenIs404(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodPost, config.RouteAdminReservations,
		`{"subdomain":"myapp","token_id":"does-not-exist"}`, true)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeNotFound)
}

func TestReservationSucceedsForKnownToken(t *testing.T) {
	h := newHarness(t)
	if err := h.tokens.Create(context.Background(), &core.Token{ID: "tok-1", Name: "owner", TokenHash: "hash"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	rr := h.do(t, http.MethodPost, config.RouteAdminReservations,
		`{"subdomain":"MyApp","token_id":"tok-1","note":"the app"}`, true)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var view reservationView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Subdomain != "myapp" {
		t.Fatalf("subdomain = %q, want normalized 'myapp'", view.Subdomain)
	}

	// A second reservation of the same subdomain conflicts.
	dup := h.do(t, http.MethodPost, config.RouteAdminReservations,
		`{"subdomain":"myapp","token_id":"tok-1"}`, true)
	if dup.Code != http.StatusConflict {
		t.Fatalf("duplicate reservation: status = %d, want 409", dup.Code)
	}
}

func TestMethodNotAllowedIncludesAllow(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodPut, config.RouteAdminTokens, "", true)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	allow := rr.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodPost) {
		t.Fatalf("Allow header = %q, want it to list GET and POST", allow)
	}
	assertErrorCode(t, rr, codeMethodNotAllowed)
}

func TestUnknownSubPathIs404(t *testing.T) {
	h := newHarness(t)
	// Trailing slash with an empty id must not act on an empty identifier.
	rr := h.do(t, http.MethodDelete, config.RouteAdminTokens+"/", "", true)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty token id: status = %d, want 404", rr.Code)
	}
	// A nested path under the sub-resource is also unknown.
	nested := h.do(t, http.MethodDelete, config.RouteAdminTokens+"/a/b", "", true)
	if nested.Code != http.StatusNotFound {
		t.Fatalf("nested path: status = %d, want 404", nested.Code)
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	h := newHarness(t)
	big := `{"name":"` + strings.Repeat("x", 128*1024) + `"}`
	rr := h.do(t, http.MethodPost, config.RouteAdminTokens, big, true)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codePayloadTooLarge)
}

func TestUnknownJSONFieldIs400(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, http.MethodPost, config.RouteAdminTokens, `{"name":"x","bogus":true}`, true)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestListTunnelsProjectsActive(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.tunnels.active = []core.Tunnel{{
		ID: "t1", Subdomain: "app", TokenID: "tok", Protocol: core.ProtocolHTTP,
		LocalPort: 3000, NodeID: "n1", ClientAddr: "1.2.3.4", ConnectedAt: now, LastSeenAt: now,
	}}
	rr := h.do(t, http.MethodGet, config.RouteAdminTunnels, "", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Tunnels []tunnelView `json:"tunnels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Tunnels) != 1 || out.Tunnels[0].ID != "t1" || out.Tunnels[0].Protocol != "http" {
		t.Fatalf("unexpected tunnels payload: %+v", out.Tunnels)
	}
}

func assertErrorCode(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Error.Code != want {
		t.Fatalf("error code = %q, want %q", env.Error.Code, want)
	}
	if env.Error.Message == "" {
		t.Fatal("error message is empty")
	}
}
