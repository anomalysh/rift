package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anomalysh/rift/server/internal/config"
	"github.com/anomalysh/rift/server/internal/core"
)

// fakeTokenStore is an in-memory core.TokenStore keyed by hash. Only the
// methods Authenticate exercises are meaningful; the rest satisfy the port.
type fakeTokenStore struct {
	byHash map[string]*core.Token
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{byHash: make(map[string]*core.Token)}
}

func (s *fakeTokenStore) add(t *core.Token) { s.byHash[t.TokenHash] = t }

func (s *fakeTokenStore) FindByHash(_ context.Context, hash string) (*core.Token, error) {
	if t, ok := s.byHash[hash]; ok {
		return t, nil
	}
	return nil, core.ErrNotFound
}

func (s *fakeTokenStore) FindByID(context.Context, string) (*core.Token, error) {
	return nil, core.ErrNotFound
}
func (s *fakeTokenStore) Create(context.Context, *core.Token) error       { return nil }
func (s *fakeTokenStore) List(context.Context) ([]core.Token, error)      { return nil, nil }
func (s *fakeTokenStore) Revoke(context.Context, string, time.Time) error { return nil }
func (s *fakeTokenStore) TouchLastUsed(context.Context, string, time.Time) error {
	return nil
}

func TestHashTokenIsStableHexSHA256(t *testing.T) {
	h := HashToken("rift_abc")
	if len(h) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(h))
	}
	if h != HashToken("rift_abc") {
		t.Fatal("hash is not deterministic")
	}
	if h == HashToken("rift_abd") {
		t.Fatal("distinct inputs produced the same hash")
	}
}

func TestMintRoundTrips(t *testing.T) {
	plaintext, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(plaintext) <= len(TokenPrefix) || plaintext[:len(TokenPrefix)] != TokenPrefix {
		t.Fatalf("plaintext %q lacks prefix %q", plaintext, TokenPrefix)
	}
	if hash != HashToken(plaintext) {
		t.Fatal("returned hash does not match HashToken(plaintext)")
	}
}

func TestAuthenticate(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	valid, validHash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	revoked, revokedHash, _ := Mint()
	expired, expiredHash, _ := Mint()

	store := newFakeTokenStore()
	store.add(&core.Token{ID: "1", Name: "valid", TokenHash: validHash, CreatedAt: past})
	store.add(&core.Token{ID: "2", Name: "revoked", TokenHash: revokedHash, CreatedAt: past, RevokedAt: &past})
	store.add(&core.Token{ID: "3", Name: "expired", TokenHash: expiredHash, CreatedAt: past, ExpiresAt: &past})

	cases := []struct {
		name      string
		plaintext string
		wantErr   bool
		wantID    string
	}{
		{"missing prefix", "nope_" + valid[len(TokenPrefix):], true, ""},
		{"unknown token", "rift_deadbeef", true, ""},
		{"revoked token", revoked, true, ""},
		{"expired token", expired, true, ""},
		{"valid token", valid, false, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := Authenticate(context.Background(), store, tc.plaintext, now)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !errors.Is(err, core.ErrUnauthorized) {
					t.Fatalf("error %v does not wrap core.ErrUnauthorized", err)
				}
				if tok != nil {
					t.Fatalf("expected nil token on failure, got %+v", tok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok.ID != tc.wantID {
				t.Fatalf("token id = %q, want %q", tok.ID, tc.wantID)
			}
		})
	}

	// A token that is valid only until 'future' authenticates now but not later.
	limited, limitedHash, _ := Mint()
	store.add(&core.Token{ID: "4", TokenHash: limitedHash, CreatedAt: past, ExpiresAt: &future})
	if _, err := Authenticate(context.Background(), store, limited, now); err != nil {
		t.Fatalf("token within validity window rejected: %v", err)
	}
	if _, err := Authenticate(context.Background(), store, limited, future); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("token at expiry not rejected as unauthorized: %v", err)
	}
}

func TestBearerFromHeader(t *testing.T) {
	cases := []struct {
		in      string
		wantTok string
		wantOK  bool
	}{
		{"Bearer x", "x", true},
		{"bearer x", "x", true}, // scheme is case-insensitive
		{"BEARER secret-token", "secret-token", true},
		{"Bearer", "", false},  // no space, no credential
		{"", "", false},        // empty header
		{"Basic x", "", false}, // wrong scheme
		{"Bearer ", "", false}, // empty credential
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			tok, ok := BearerFromHeader(tc.in)
			if ok != tc.wantOK || tok != tc.wantTok {
				t.Fatalf("BearerFromHeader(%q) = (%q, %v), want (%q, %v)", tc.in, tok, ok, tc.wantTok, tc.wantOK)
			}
		})
	}
}

func TestEqualConstantTime(t *testing.T) {
	if !EqualConstantTime("abc", "abc") {
		t.Fatal("equal strings reported unequal")
	}
	if EqualConstantTime("abc", "abd") {
		t.Fatal("different strings reported equal")
	}
	if EqualConstantTime("abc", "abcd") {
		t.Fatal("different-length strings reported equal")
	}
}

func TestAdminGuard(t *testing.T) {
	const expected = "rift_admin_secret"
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	guard := AdminGuard(expected, next)

	t.Run("wrong token is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tokens", nil)
		req.Header.Set(config.HeaderAuthorization, config.BearerPrefix+"wrong")
		rr := httptest.NewRecorder()
		guard.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
		if rr.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing WWW-Authenticate challenge")
		}
	})

	t.Run("missing header is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tokens", nil)
		rr := httptest.NewRecorder()
		guard.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("correct token passes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tokens", nil)
		req.Header.Set(config.HeaderAuthorization, config.BearerPrefix+expected)
		rr := httptest.NewRecorder()
		guard.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (guard should have passed through)", rr.Code)
		}
	})
}
