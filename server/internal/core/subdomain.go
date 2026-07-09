package core

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// SubdomainRules constrain which labels an agent may claim. The concrete
// values come from configuration; core only enforces them.
type SubdomainRules struct {
	MinLength int
	MaxLength int
	// Pattern must match the whole label.
	Pattern *regexp.Regexp
	// Blocked is the set of labels no token may claim (lowercased).
	Blocked map[string]struct{}
	// GeneratedLength is the label length used by GenerateSubdomain.
	GeneratedLength int
	// GeneratedAlphabet is the character set used by GenerateSubdomain.
	GeneratedAlphabet string
}

// NewSubdomainRules validates the rule set itself, so a misconfigured server
// fails at boot rather than at the first handshake.
func NewSubdomainRules(minLen, maxLen int, pattern string, blocked []string, genLen int, genAlphabet string) (*SubdomainRules, error) {
	if minLen < 1 {
		return nil, fmt.Errorf("core: subdomain min length must be >= 1, got %d", minLen)
	}
	if maxLen < minLen {
		return nil, fmt.Errorf("core: subdomain max length %d < min length %d", maxLen, minLen)
	}
	// A DNS label cannot exceed 63 octets (RFC 1035 §2.3.4).
	const maxDNSLabel = 63
	if maxLen > maxDNSLabel {
		return nil, fmt.Errorf("core: subdomain max length %d exceeds DNS label limit %d", maxLen, maxDNSLabel)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("core: compile subdomain pattern %q: %w", pattern, err)
	}
	if genLen < minLen || genLen > maxLen {
		return nil, fmt.Errorf("core: generated subdomain length %d outside [%d,%d]", genLen, minLen, maxLen)
	}
	if len(genAlphabet) < 2 {
		return nil, fmt.Errorf("core: generated subdomain alphabet needs >= 2 characters")
	}

	set := make(map[string]struct{}, len(blocked))
	for _, b := range blocked {
		b = strings.ToLower(strings.TrimSpace(b))
		if b != "" {
			set[b] = struct{}{}
		}
	}

	rules := &SubdomainRules{
		MinLength:         minLen,
		MaxLength:         maxLen,
		Pattern:           re,
		Blocked:           set,
		GeneratedLength:   genLen,
		GeneratedAlphabet: genAlphabet,
	}

	// A generated subdomain must itself be claimable, otherwise the server
	// would hand out labels it then rejects.
	probe := strings.Repeat(string(genAlphabet[0]), genLen)
	if !re.MatchString(probe) {
		return nil, fmt.Errorf("core: generated subdomain %q does not satisfy pattern %q", probe, pattern)
	}
	return rules, nil
}

// NormalizeSubdomain lowercases and trims a requested label.
func NormalizeSubdomain(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Blocked reports whether the (already normalized) label is on the blocklist.
func (r *SubdomainRules) IsBlocked(s string) bool {
	_, ok := r.Blocked[s]
	return ok
}

// Validate checks a normalized label against length, pattern and blocklist.
// It returns ErrSubdomainInvalid or ErrSubdomainReserved.
func (r *SubdomainRules) Validate(s string) error {
	if len(s) < r.MinLength || len(s) > r.MaxLength {
		return fmt.Errorf("%w: length must be between %d and %d", ErrSubdomainInvalid, r.MinLength, r.MaxLength)
	}
	if !r.Pattern.MatchString(s) {
		return fmt.Errorf("%w: must match %s", ErrSubdomainInvalid, r.Pattern.String())
	}
	if r.IsBlocked(s) {
		return fmt.Errorf("%w: %q is not available", ErrSubdomainReserved, s)
	}
	return nil
}

// GenerateSubdomain returns a cryptographically random label. The caller
// retries on collision; the store's unique index is the real arbiter.
func (r *SubdomainRules) GenerateSubdomain() (string, error) {
	n := big.NewInt(int64(len(r.GeneratedAlphabet)))
	out := make([]byte, r.GeneratedLength)
	for i := range out {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("core: generate subdomain: %w", err)
		}
		out[i] = r.GeneratedAlphabet[idx.Int64()]
	}
	s := string(out)
	// Extremely unlikely, but never hand back a blocked label.
	if r.IsBlocked(s) {
		return r.GenerateSubdomain()
	}
	return s, nil
}

// Hostname joins a label with the configured base domain.
func Hostname(subdomain, baseDomain string) string {
	return subdomain + "." + baseDomain
}

// SubdomainFromHost extracts the tunnel label from a request Host header.
// It strips any port, matches the base domain suffix case-insensitively, and
// rejects multi-label prefixes such as "a.b.base.tld".
func SubdomainFromHost(host, baseDomain string) (string, bool) {
	if i := strings.LastIndexByte(host, ':'); i != -1 {
		// Guard against IPv6 literals like "[::1]:8080" having no label.
		if !strings.Contains(host[i:], "]") {
			host = host[:i]
		}
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(baseDomain), "."))

	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := host[:len(host)-len(suffix)]
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}
