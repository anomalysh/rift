package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// loader reads typed values from the environment, accumulating every problem
// so a misconfigured deployment reports all its errors in one pass instead of
// one restart per mistake.
type loader struct {
	errs []string
	// warns are legal but noteworthy settings. They are surfaced by the caller
	// after the logger exists, so they never gag a boot.
	warns []string
}

func (l *loader) fail(key string, err error) {
	l.errs = append(l.errs, fmt.Sprintf("%s: %v", key, err))
}

func (l *loader) warn(key, msg string) {
	l.warns = append(l.warns, fmt.Sprintf("%s: %s", key, msg))
}

func (l *loader) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: %d problem(s):\n  - %s", len(l.errs), strings.Join(l.errs, "\n  - "))
}

func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

// str returns the env value or def.
func (l *loader) str(key, def string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return def
}

// requiredStr returns the env value, recording an error when unset. There is
// deliberately no default: these are values no sane default exists for.
func (l *loader) requiredStr(key string) string {
	v, ok := lookup(key)
	if !ok {
		l.fail(key, fmt.Errorf("is required but not set"))
		return ""
	}
	return v
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, fmt.Errorf("expected a boolean, got %q", v))
		return def
	}
	return b
}

func (l *loader) integer(key string, def int) int {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, fmt.Errorf("expected an integer, got %q", v))
		return def
	}
	return n
}

func (l *loader) integer64(key string, def int64) int64 {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		l.fail(key, fmt.Errorf("expected an integer, got %q", v))
		return def
	}
	return n
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, fmt.Errorf("expected a duration such as 15s or 2m, got %q", v))
		return def
	}
	if d <= 0 {
		l.fail(key, fmt.Errorf("expected a positive duration, got %q", v))
		return def
	}
	return d
}

// csv splits a comma-separated list, trimming and dropping empties. An unset
// key yields def.
func (l *loader) csv(key string, def []string) []string {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	return splitCSV(v)
}

// csvAppend returns base plus whatever the env var adds, deduplicated.
//
// Used for lists where the built-in entries are a safety floor rather than a
// suggestion. The subdomain blocklist is the motivating case: an operator who
// adds one label of their own must not thereby unblock `gateway`, `api` and
// `www`, which would let an agent claim a hostname that looks official — or,
// for `gateway`, one that Caddy routes to the agent endpoint.
func (l *loader) csvAppend(key string, base []string) []string {
	extra, ok := lookup(key)
	if !ok {
		return base
	}

	seen := make(map[string]struct{}, len(base))
	out := make([]string, 0, len(base))
	for _, v := range base {
		if _, dup := seen[v]; !dup {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range splitCSV(extra) {
		if _, dup := seen[v]; !dup {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// atLeast records an error when n is below minimum.
func (l *loader) atLeast(key string, n, minimum int) int {
	if n < minimum {
		l.fail(key, fmt.Errorf("must be >= %d, got %d", minimum, n))
	}
	return n
}
