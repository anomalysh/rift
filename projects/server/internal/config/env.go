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

// parse is the one path every typed setting takes: unset yields def, and a
// value conv rejects is recorded as "expected <want>" and also yields def, so
// Load keeps going and reports every bad key in one pass.
func parse[T any](l *loader, key string, def T, want string, conv func(string) (T, error)) T {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	out, err := conv(v)
	if err != nil {
		l.fail(key, fmt.Errorf("expected %s, got %q", want, v))
		return def
	}
	return out
}

func (l *loader) boolean(key string, def bool) bool {
	return parse(l, key, def, "a boolean", strconv.ParseBool)
}

func (l *loader) integer(key string, def int) int {
	return parse(l, key, def, "an integer", strconv.Atoi)
}

func (l *loader) integer64(key string, def int64) int64 {
	return parse(l, key, def, "an integer", func(v string) (int64, error) { return strconv.ParseInt(v, 10, 64) })
}

// duration reads a strictly positive duration: a zero timeout or interval
// would disable what it bounds, or spin a ticker.
func (l *loader) duration(key string, def time.Duration) time.Duration {
	return parse(l, key, def, "a positive duration such as 15s or 2m", durationFrom(1))
}

// optionalDuration reads a duration for which zero legitimately means "no
// deadline" (the ingress write timeout), so only a negative one is refused.
func (l *loader) optionalDuration(key string, def time.Duration) time.Duration {
	return parse(l, key, def, "a duration such as 30s, or 0 for none", durationFrom(0))
}

// durationFrom parses a duration and rejects one below floor.
func durationFrom(floor time.Duration) func(string) (time.Duration, error) {
	return func(v string) (time.Duration, error) {
		d, err := time.ParseDuration(v)
		if err == nil && d < floor {
			err = fmt.Errorf("below %s", floor)
		}
		return d, err
	}
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
