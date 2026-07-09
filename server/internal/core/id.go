package core

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// idEncoding is Crockford-adjacent base32 without padding: URL-safe, case
// insensitive, and lexicographically ordered so time-prefixed IDs sort by age.
var idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewID returns a 26-character lexicographically sortable identifier: 48 bits
// of millisecond timestamp followed by 80 bits of randomness (the ULID layout).
// Sortable IDs keep Postgres primary-key inserts append-only.
func NewID(now time.Time) (string, error) {
	var raw [16]byte
	ms := uint64(now.UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	binary.BigEndian.PutUint32(raw[2:6], uint32(ms))
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("core: generate id: %w", err)
	}
	return idEncoding.EncodeToString(raw[:]), nil
}

// MustNewID panics on entropy failure. Only for process start-up paths where
// there is nothing sensible to do but die.
func MustNewID(now time.Time) string {
	id, err := NewID(now)
	if err != nil {
		panic(err)
	}
	return id
}

// NewSecret returns n bytes of randomness encoded as an unpadded base32
// string, prefixed for provenance (e.g. "tunl_").
func NewSecret(prefix string, n int) (string, error) {
	if n < 16 {
		return "", fmt.Errorf("core: secret needs at least 16 bytes of entropy, got %d", n)
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("core: generate secret: %w", err)
	}
	return prefix + strings.ToLower(idEncoding.EncodeToString(buf)), nil
}
