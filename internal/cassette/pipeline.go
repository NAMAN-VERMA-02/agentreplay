package cassette

import (
	"strconv"
	"sync"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
)

// Pipeline runs the fixed order used both when recording and when matching:
// parse -> normalize volatile fields -> redact -> canonical JSON -> hash.
type Pipeline struct {
	Red    *redact.Redactor
	ignore [][]string
}

// NewPipeline builds a pipeline from a key and rules.
func NewPipeline(key []byte, rules redact.Rules) (*Pipeline, error) {
	red, err := redact.New(key, rules)
	if err != nil {
		return nil, err
	}
	p := &Pipeline{Red: red}
	for _, s := range rules.Ignore {
		p.ignore = append(p.ignore, redact.SplitPath(s))
	}
	return p, nil
}

// Normalizer removes protocol-specific volatile fields from a decoded value.
type Normalizer func(v any) any

// Processed is the result of running the pipeline on one message.
type Processed struct {
	Stored    any    // redacted, not normalized: what is written to the cassette
	Canonical []byte // normalized + redacted canonical JSON
	Hash      string
}

// Process runs the pipeline on raw JSON.
func (p *Pipeline) Process(raw []byte, norm Normalizer) (Processed, error) {
	v, err := Decode(raw)
	if err != nil {
		return Processed{}, err
	}
	return p.ProcessValue(v, norm), nil
}

// ProcessValue runs the pipeline on an already decoded value (it is not modified).
func (p *Pipeline) ProcessValue(v any, norm Normalizer) Processed {
	stored := p.Red.Apply(v)
	n := v
	if norm != nil {
		n = norm(v)
	}
	n = dropPaths(n, p.ignore, nil)
	canon := Canonical(p.Red.Apply(n))
	return Processed{Stored: stored, Canonical: canon, Hash: Hash(canon)}
}

// Redact returns a redacted copy of raw JSON (used for responses).
func (p *Pipeline) Redact(raw []byte) ([]byte, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	return Canonical(p.Red.Apply(v)), nil
}

// dropPaths returns a copy of v without the values at the given paths.
func dropPaths(v any, paths [][]string, at []string) any {
	if len(paths) == 0 {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			p := append(append([]string{}, at...), k)
			if anyMatch(paths, p) {
				continue
			}
			out[k] = dropPaths(val, paths, p)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for i, val := range t {
			p := append(append([]string{}, at...), strconv.Itoa(i))
			if anyMatch(paths, p) {
				continue
			}
			out = append(out, dropPaths(val, paths, p))
		}
		return out
	}
	return v
}

func anyMatch(paths [][]string, p []string) bool {
	for _, pat := range paths {
		if redact.MatchPath(pat, p) {
			return true
		}
	}
	return false
}

// Pool hands out recorded events by key, earliest unused first. The Nth request
// with a key gets the Nth recorded event with that key.
type Pool struct {
	mu    sync.Mutex
	byKey map[string][]*Event
	used  map[*Event]bool
	all   []*Event
}

// NewPool indexes events (already in recorded order) by key.
func NewPool(events []*Event, key func(*Event) string) *Pool {
	p := &Pool{byKey: map[string][]*Event{}, used: map[*Event]bool{}, all: events}
	for _, e := range events {
		k := key(e)
		p.byKey[k] = append(p.byKey[k], e)
	}
	return p
}

// Take returns the earliest unused event with key.
func (p *Pool) Take(key string) (*Event, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.byKey[key] {
		if !p.used[e] {
			p.used[e] = true
			return e, true
		}
	}
	return nil, false
}

// Last returns the last event with key, used or not (fixture reuse in test mode).
func (p *Pool) Last(key string) (*Event, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	evs := p.byKey[key]
	if len(evs) == 0 {
		return nil, false
	}
	return evs[len(evs)-1], true
}

// Group returns every event with key.
func (p *Pool) Group(key string) []*Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Event{}, p.byKey[key]...)
}

// Unused returns events never taken, in recorded order.
func (p *Pool) Unused() []*Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Event
	for _, e := range p.all {
		if !p.used[e] {
			out = append(out, e)
		}
	}
	return out
}
