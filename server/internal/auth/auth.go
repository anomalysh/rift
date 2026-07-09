// Package auth mints, hashes and verifies rift API tokens, and guards the
// admin API with a shared bearer token. It depends only on core and config;
// it never imports a store implementation.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anomaly-sh/rift/server/internal/config"
	"github.com/anomaly-sh/rift/server/internal/core"
)

const (
	// TokenPrefix marks a string as a rift token for provenance and lets us
	// reject obviously-malformed credentials before any store lookup.
	TokenPrefix = "rift_"
	// TokenEntropyBytes is the randomness behind each token. 32 bytes is a
	// 256-bit secret: not brute-forceable, which is what makes HashToken safe.
	TokenEntropyBytes = 32
)

// Mint returns a fresh plaintext token and its storage hash. The plaintext is
// shown to the operator exactly once at creation; only the hash is persisted.
func Mint() (plaintext, hash string, err error) {
	plaintext, err = core.NewSecret(TokenPrefix, TokenEntropyBytes)
	if err != nil {
		return "", "", fmt.Errorf("auth: mint token: %w", err)
	}
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the hex-encoded SHA-256 of a plaintext token.
//
// A single unsalted SHA-256 is deliberate, not an oversight. Tokens are 256
// bits of cryptographic randomness (see TokenEntropyBytes), not user-chosen
// passwords. The slow-KDF defenses (bcrypt/argon2 work factors, per-row salts)
// exist to blunt brute-force and rainbow-table attacks on low-entropy human
// secrets; against a uniformly random 256-bit keyspace they buy nothing, since
// an attacker cannot enumerate it regardless of hash speed. A fast hash also
// keeps per-request authentication cheap.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves a plaintext token to its stored record.
//
// Every failure mode collapses to a wrapped core.ErrUnauthorized so a caller
// cannot distinguish "no such token" from "wrong token" from "expired token";
// only a store/transport fault surfaces as a distinct error.
func Authenticate(ctx context.Context, store core.TokenStore, plaintext string, now time.Time) (*core.Token, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return nil, fmt.Errorf("auth: token missing %q prefix: %w", TokenPrefix, core.ErrUnauthorized)
	}
	tok, err := store.FindByHash(ctx, HashToken(plaintext))
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, fmt.Errorf("auth: token not recognized: %w", core.ErrUnauthorized)
		}
		return nil, fmt.Errorf("auth: look up token: %w", err)
	}
	if !tok.Active(now) {
		return nil, fmt.Errorf("auth: token is inactive: %w", core.ErrUnauthorized)
	}
	return tok, nil
}

// BearerFromHeader extracts the credential from an Authorization header value,
// matching the scheme case-insensitively per RFC 7235. It reports false when
// the scheme is absent, is not Bearer, or the credential is empty.
func BearerFromHeader(headerValue string) (string, bool) {
	if len(headerValue) < len(config.BearerPrefix) {
		return "", false
	}
	if !strings.EqualFold(headerValue[:len(config.BearerPrefix)], config.BearerPrefix) {
		return "", false
	}
	token := headerValue[len(config.BearerPrefix):]
	if token == "" {
		return "", false
	}
	return token, true
}

// EqualConstantTime reports whether a and b are equal without leaking, through
// timing, where they first differ.
func EqualConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AdminGuard rejects any request whose bearer token does not equal expected,
// comparing in constant time so the response timing reveals nothing about the
// secret. A 401 carries a WWW-Authenticate challenge and never echoes expected.
func AdminGuard(expected string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := BearerFromHeader(r.Header.Get(config.HeaderAuthorization))
		if !ok || !EqualConstantTime(presented, expected) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="rift admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
