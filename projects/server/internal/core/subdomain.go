package core

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Subdomain generation strategies.
const (
	// GeneratorWords produces adjective-noun-number, e.g. "swift-otter-42".
	GeneratorWords = "words"
	// GeneratorRandom produces a random string over GeneratedAlphabet.
	GeneratorRandom = "random"
)

// generatedNumberCeiling bounds the numeric suffix of a word-based subdomain.
// Two to four digits keeps the label readable while adding enough spread that
// a first-try collision on adjective+noun is rare.
const generatedNumberCeiling = 10000

// SubdomainRules constrain which labels an agent may claim. The concrete
// values come from configuration; core only enforces them.
type SubdomainRules struct {
	MinLength int
	MaxLength int
	// Pattern must match the whole label.
	Pattern *regexp.Regexp
	// Blocked is the set of labels no token may claim (lowercased).
	Blocked map[string]struct{}
	// Generator selects the shape of a generated subdomain.
	Generator string
	// GeneratedLength is the label length used by the random generator.
	GeneratedLength int
	// GeneratedAlphabet is the character set used by the random generator.
	GeneratedAlphabet string
}

// NewSubdomainRules validates the rule set itself, so a misconfigured server
// fails at boot rather than at the first handshake.
func NewSubdomainRules(minLen, maxLen int, pattern string, blocked []string, generator string, genLen int, genAlphabet string) (*SubdomainRules, error) {
	switch generator {
	case GeneratorWords, GeneratorRandom:
	default:
		return nil, fmt.Errorf("core: subdomain generator must be %q or %q, got %q", GeneratorWords, GeneratorRandom, generator)
	}
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
		Generator:         generator,
		GeneratedLength:   genLen,
		GeneratedAlphabet: genAlphabet,
	}

	// A generated subdomain must itself be claimable, otherwise the server
	// would hand out labels it then rejects. Probe whichever strategy is in
	// use against the real validator.
	probe, err := rules.GenerateSubdomain()
	if err != nil {
		return nil, fmt.Errorf("core: generator produced no valid subdomain: %w", err)
	}
	if err := rules.Validate(probe); err != nil {
		return nil, fmt.Errorf("core: generated subdomain %q does not satisfy the rules: %w", probe, err)
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

// generateAttempts bounds retries so a rule set the generator cannot satisfy
// (a word pair too long for MaxLength, say) fails loudly instead of spinning.
const generateAttempts = 20

// GenerateSubdomain returns a fresh label using the configured strategy. The
// caller still retries on a store collision; the store's unique index is the
// real arbiter. Generation only guarantees the label satisfies the rules.
func (r *SubdomainRules) GenerateSubdomain() (string, error) {
	for attempt := 0; attempt < generateAttempts; attempt++ {
		var (
			s   string
			err error
		)
		if r.Generator == GeneratorWords {
			s, err = r.generateWords()
		} else {
			s, err = r.generateRandom()
		}
		if err != nil {
			return "", err
		}
		if len(s) >= r.MinLength && len(s) <= r.MaxLength && r.Pattern.MatchString(s) && !r.IsBlocked(s) {
			return s, nil
		}
	}
	return "", fmt.Errorf("core: could not generate a valid subdomain in %d attempts (generator %q vs length [%d,%d])",
		generateAttempts, r.Generator, r.MinLength, r.MaxLength)
}

func (r *SubdomainRules) generateRandom() (string, error) {
	n := big.NewInt(int64(len(r.GeneratedAlphabet)))
	out := make([]byte, r.GeneratedLength)
	for i := range out {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("core: generate subdomain: %w", err)
		}
		out[i] = r.GeneratedAlphabet[idx.Int64()]
	}
	return string(out), nil
}

func (r *SubdomainRules) generateWords() (string, error) {
	adj, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	noun, err := pick(nouns)
	if err != nil {
		return "", err
	}
	num, err := rand.Int(rand.Reader, big.NewInt(generatedNumberCeiling))
	if err != nil {
		return "", fmt.Errorf("core: generate subdomain number: %w", err)
	}
	return fmt.Sprintf("%s-%s-%d", adj, noun, num.Int64()), nil
}

func pick(words []string) (string, error) {
	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", fmt.Errorf("core: pick word: %w", err)
	}
	return words[idx.Int64()], nil
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
