// Package redact replaces secrets with keyed fingerprints before anything is written.
//
// A fingerprint is HMAC-SHA256(key, value), so two requests that differ only in a
// secret still hash differently, but the secret itself never reaches the cassette.
package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// KeyEnv names the environment variable that holds the HMAC key.
const KeyEnv = "AGENTREPLAY_HMAC_KEY"

// DefaultPatterns catch common credential formats in any string value.
var DefaultPatterns = []string{
	`sk-[A-Za-z0-9_\-]{16,}`,             // OpenAI and Anthropic style keys
	`gh[pousr]_[A-Za-z0-9]{20,}`,         // GitHub tokens
	`xox[abprs]-[A-Za-z0-9\-]{10,}`,      // Slack tokens
	`AKIA[0-9A-Z]{16}`,                   // AWS access key ids
	`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`,  // bearer tokens pasted into text
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`, // PEM private key headers
}

// Rules are stored in the cassette header so replay applies exactly the same ones.
type Rules struct {
	// Patterns are regexes; every match inside any string is replaced.
	Patterns []string `json:"patterns,omitempty" yaml:"patterns"`
	// Paths are dotted JSON paths ("*" matches any key or index); the whole value is replaced.
	Paths []string `json:"paths,omitempty" yaml:"paths"`
	// Ignore are dotted JSON paths dropped before hashing (volatile fields such as timestamps).
	Ignore []string `json:"ignore,omitempty" yaml:"ignore"`
}

// WithDefaults returns r plus the default patterns, without duplicates.
func (r Rules) WithDefaults() Rules {
	seen := map[string]bool{}
	var pats []string
	for _, p := range append(append([]string{}, DefaultPatterns...), r.Patterns...) {
		if !seen[p] {
			seen[p] = true
			pats = append(pats, p)
		}
	}
	r.Patterns = pats
	return r
}

// Redactor applies Rules with a key.
type Redactor struct {
	key      []byte
	patterns []*regexp.Regexp
	paths    [][]string
}

// New compiles rules. An empty key is refused: there is no unkeyed fallback.
func New(key []byte, r Rules) (*Redactor, error) {
	if len(key) == 0 {
		return nil, errors.New("no HMAC key: set " + KeyEnv)
	}
	red := &Redactor{key: key}
	for _, p := range r.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redaction pattern %q: %w", p, err)
		}
		red.patterns = append(red.patterns, re)
	}
	for _, p := range r.Paths {
		red.paths = append(red.paths, SplitPath(p))
	}
	return red, nil
}

// KeyCheck lets replay prove it holds the same key without storing the key.
func KeyCheck(key []byte) string { return mac(key, "agentreplay-keycheck") }

// Fingerprint is the replacement for one secret value.
func (r *Redactor) Fingerprint(s string) string { return "hmac:" + mac(r.key, s)[:32] }

func mac(key []byte, s string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// Apply returns a redacted copy of a decoded JSON value.
func (r *Redactor) Apply(v any) any { return r.walk(v, nil) }

// fingerprintRe matches values that are already fingerprints. In replay the agent
// echoes fingerprints it received from replayed responses; redacting them again
// would change the hash and break matching.
var fingerprintRe = regexp.MustCompile(`^hmac:[0-9a-f]{32}$`)

func (r *Redactor) walk(v any, path []string) any {
	if s, ok := v.(string); ok && fingerprintRe.MatchString(s) {
		return s
	}
	if r.pathMatches(path) {
		if s, ok := v.(string); ok {
			return r.Fingerprint(s)
		}
		b, _ := json.Marshal(v)
		return r.Fingerprint(string(b))
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = r.walk(val, append(path, k))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = r.walk(val, append(path, strconv.Itoa(i)))
		}
		return out
	case string:
		for _, re := range r.patterns {
			t = re.ReplaceAllStringFunc(t, r.Fingerprint)
		}
		return t
	default:
		return v
	}
}

func (r *Redactor) pathMatches(path []string) bool {
	for _, p := range r.paths {
		if MatchPath(p, path) {
			return true
		}
	}
	return false
}

// SplitPath turns "a.b.*.c" into its segments.
func SplitPath(p string) []string { return strings.Split(p, ".") }

// MatchPath reports whether a concrete path matches a pattern with "*" segments.
func MatchPath(pattern, path []string) bool {
	if len(pattern) != len(path) || len(path) == 0 {
		return false
	}
	for i := range pattern {
		if pattern[i] != "*" && pattern[i] != path[i] {
			return false
		}
	}
	return true
}
