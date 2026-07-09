// Package adminapi serves the token, reservation and tunnel management API.
// It is authenticated by a single shared admin bearer token (auth.AdminGuard)
// and speaks only to the core store ports; it never imports a store adapter.
package adminapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anomalysh/rift/server/internal/auth"
	"github.com/anomalysh/rift/server/internal/config"
	"github.com/anomalysh/rift/server/internal/core"
)

// maxBodyBytes caps admin request bodies. Admin payloads are a few small JSON
// fields; anything larger is a client error or an attack, so we reject early.
const maxBodyBytes = 64 << 10 // 64 KiB

// Machine-readable error codes returned in the JSON error envelope. Clients
// branch on these, not on the human-readable message.
const (
	codeBadRequest       = "bad_request"
	codeUnauthorized     = "unauthorized"
	codeNotFound         = "not_found"
	codeConflict         = "conflict"
	codeInvalidSubdomain = "invalid_subdomain"
	codeMethodNotAllowed = "method_not_allowed"
	codePayloadTooLarge  = "payload_too_large"
	codeInternal         = "internal_error"
)

type server struct {
	cfg          *config.Config
	tokens       core.TokenStore
	reservations core.ReservationStore
	tunnels      core.TunnelStore
	logger       *slog.Logger
}

// New returns the admin API handler. Every route except RouteHealth sits behind
// auth.AdminGuard, so the liveness probe stays reachable without credentials.
func New(cfg *config.Config, tokens core.TokenStore, reservations core.ReservationStore, tunnels core.TunnelStore, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{
		cfg:          cfg,
		tokens:       tokens,
		reservations: reservations,
		tunnels:      tunnels,
		logger:       logger,
	}

	// Go 1.21's ServeMux has no method or wildcard patterns: each handler
	// switches on the method itself, and sub-resource routes are registered as
	// subtree patterns (trailing slash) whose prefix the handler strips.
	admin := http.NewServeMux()
	admin.HandleFunc(config.RouteAdminTokens, s.handleTokens)
	admin.HandleFunc(config.RouteAdminTokens+"/", s.handleTokenByID)
	admin.HandleFunc(config.RouteAdminReservations, s.handleReservations)
	admin.HandleFunc(config.RouteAdminReservations+"/", s.handleReservationBySubdomain)
	admin.HandleFunc(config.RouteAdminTunnels, s.handleTunnels)

	root := http.NewServeMux()
	root.HandleFunc(config.RouteHealth, s.handleHealth)
	root.Handle("/", auth.AdminGuard(cfg.Admin.Token, admin))
	return root
}

// --- tokens -------------------------------------------------------------

func (s *server) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createToken(w, r)
	case http.MethodGet:
		s.listTokens(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// tokenCreatedView is the create-token response. This is the ONLY place the
// plaintext token is ever returned to a client; it is never persisted, never
// logged, and never appears in any list or get response.
type tokenCreatedView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Token      string     `json:"token"`
	MaxTunnels int        `json:"max_tunnels"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func (s *server) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string     `json:"name"`
		MaxTunnels *int       `json:"max_tunnels"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "name is required")
		return
	}
	maxTunnels := s.cfg.Tunnel.MaxTunnelsPerToken
	if req.MaxTunnels != nil {
		if *req.MaxTunnels < 0 {
			writeError(w, http.StatusBadRequest, codeBadRequest, "max_tunnels must not be negative")
			return
		}
		maxTunnels = *req.MaxTunnels
	}

	now := time.Now().UTC()
	id, err := core.NewID(now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	plaintext, hash, err := auth.Mint()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		e := req.ExpiresAt.UTC()
		expiresAt = &e
	}
	tok := &core.Token{
		ID:         id,
		Name:       name,
		TokenHash:  hash,
		MaxTunnels: maxTunnels,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
	}
	if err := s.tokens.Create(r.Context(), tok); err != nil {
		s.fail(w, r, err)
		return
	}
	s.logger.InfoContext(r.Context(), "admin created token", "token_id", id, "name", name)
	writeJSON(w, http.StatusCreated, tokenCreatedView{
		ID:         id,
		Name:       name,
		Token:      plaintext,
		MaxTunnels: maxTunnels,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
	})
}

// tokenView is the list/get projection: no hash, no plaintext, ever.
type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	MaxTunnels int        `json:"max_tunnels"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func (s *server) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.tokens.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := make([]tokenView, 0, len(tokens))
	for i := range tokens {
		t := tokens[i]
		views = append(views, tokenView{
			ID:         t.ID,
			Name:       t.Name,
			MaxTunnels: t.MaxTunnels,
			CreatedAt:  t.CreatedAt,
			LastUsedAt: t.LastUsedAt,
			RevokedAt:  t.RevokedAt,
			ExpiresAt:  t.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": views})
}

func (s *server) handleTokenByID(w http.ResponseWriter, r *http.Request) {
	id, ok := subPath(r.URL.Path, config.RouteAdminTokens)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found")
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	if err := s.tokens.Revoke(r.Context(), id, time.Now().UTC()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.logger.InfoContext(r.Context(), "admin revoked token", "token_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// --- reservations -------------------------------------------------------

func (s *server) handleReservations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createReservation(w, r)
	case http.MethodGet:
		s.listReservations(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

type reservationView struct {
	Subdomain string    `json:"subdomain"`
	TokenID   string    `json:"token_id"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *server) createReservation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subdomain string `json:"subdomain"`
		TokenID   string `json:"token_id"`
		Note      string `json:"note"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	subdomain := core.NormalizeSubdomain(req.Subdomain)
	// Validate shape first: ErrSubdomainInvalid -> 400, ErrSubdomainReserved
	// (blocklisted) -> 409, before we ever touch a store.
	if err := s.cfg.SubdomainRules.Validate(subdomain); err != nil {
		s.fail(w, r, err)
		return
	}
	if strings.TrimSpace(req.TokenID) == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "token_id is required")
		return
	}
	if _, err := s.tokens.FindByID(r.Context(), req.TokenID); err != nil {
		s.fail(w, r, err) // ErrNotFound -> 404
		return
	}

	now := time.Now().UTC()
	res := &core.Reservation{
		Subdomain: subdomain,
		TokenID:   req.TokenID,
		Note:      strings.TrimSpace(req.Note),
		CreatedAt: now,
	}
	if err := s.reservations.Create(r.Context(), res); err != nil {
		s.fail(w, r, err) // already reserved -> 409
		return
	}
	s.logger.InfoContext(r.Context(), "admin created reservation", "subdomain", subdomain, "token_id", req.TokenID)
	writeJSON(w, http.StatusCreated, reservationView{
		Subdomain: res.Subdomain,
		TokenID:   res.TokenID,
		Note:      res.Note,
		CreatedAt: res.CreatedAt,
	})
}

func (s *server) listReservations(w http.ResponseWriter, r *http.Request) {
	list, err := s.reservations.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := make([]reservationView, 0, len(list))
	for i := range list {
		res := list[i]
		views = append(views, reservationView{
			Subdomain: res.Subdomain,
			TokenID:   res.TokenID,
			Note:      res.Note,
			CreatedAt: res.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reservations": views})
}

func (s *server) handleReservationBySubdomain(w http.ResponseWriter, r *http.Request) {
	raw, ok := subPath(r.URL.Path, config.RouteAdminReservations)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found")
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	subdomain := core.NormalizeSubdomain(raw)
	if err := s.reservations.Delete(r.Context(), subdomain); err != nil {
		s.fail(w, r, err)
		return
	}
	s.logger.InfoContext(r.Context(), "admin deleted reservation", "subdomain", subdomain)
	w.WriteHeader(http.StatusNoContent)
}

// --- tunnels ------------------------------------------------------------

type tunnelView struct {
	ID          string    `json:"id"`
	Subdomain   string    `json:"subdomain"`
	TokenID     string    `json:"token_id"`
	Protocol    string    `json:"protocol"`
	LocalPort   int       `json:"local_port"`
	NodeID      string    `json:"node_id"`
	ClientAddr  string    `json:"client_addr"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

func (s *server) handleTunnels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	list, err := s.tunnels.ListActive(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := make([]tunnelView, 0, len(list))
	for i := range list {
		tn := list[i]
		views = append(views, tunnelView{
			ID:          tn.ID,
			Subdomain:   tn.Subdomain,
			TokenID:     tn.TokenID,
			Protocol:    tn.Protocol.String(),
			LocalPort:   tn.LocalPort,
			NodeID:      tn.NodeID,
			ClientAddr:  tn.ClientAddr,
			ConnectedAt: tn.ConnectedAt,
			LastSeenAt:  tn.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnels": views})
}

// --- health -------------------------------------------------------------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- helpers ------------------------------------------------------------

// subPath returns the single path segment after base+"/". It reports false for
// an empty segment (a trailing slash with no id) or a nested path, so those
// map to 404 rather than acting on an empty or ambiguous identifier.
func subPath(urlPath, base string) (string, bool) {
	rest := strings.TrimPrefix(urlPath, base+"/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// decodeBody reads a single JSON object into dst under a size cap, rejecting
// unknown fields. It writes the appropriate error response and returns false on
// failure so the caller can simply return.
func (s *server) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "invalid request body")
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, codeBadRequest, "request body must contain a single JSON object")
		return false
	}
	return true
}

// fail maps a core sentinel error to its HTTP status and writes the JSON error
// envelope. A 500 logs the underlying error and returns a generic message, so
// internal detail never reaches the client.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := statusForError(err)
	if status == http.StatusInternalServerError {
		s.logger.ErrorContext(r.Context(), "admin api request failed",
			"method", r.Method, "path", r.URL.Path, "error", err.Error())
	}
	writeError(w, status, code, msg)
}

func statusForError(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, codeNotFound, "resource not found"
	case errors.Is(err, core.ErrUnauthorized):
		return http.StatusUnauthorized, codeUnauthorized, "unauthorized"
	case errors.Is(err, core.ErrSubdomainInvalid):
		return http.StatusBadRequest, codeInvalidSubdomain, "subdomain is invalid"
	case errors.Is(err, core.ErrSubdomainReserved),
		errors.Is(err, core.ErrSubdomainTaken),
		errors.Is(err, core.ErrConflict):
		return http.StatusConflict, codeConflict, "subdomain is not available"
	default:
		return http.StatusInternalServerError, codeInternal, "internal server error"
	}
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
