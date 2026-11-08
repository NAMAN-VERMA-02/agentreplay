// Package cassette defines the JSONL trace format and the pipeline that turns a raw
// request into a stored, redacted event with a deterministic hash.
package cassette

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
)

// SchemaVersion is bumped on any incompatible change to Event.
const SchemaVersion = 1

// Event types.
const (
	Header          = "header"
	LLMExchange     = "llm_exchange"
	MCPExchange     = "mcp_exchange"
	MCPNotification = "mcp_notification"
	MCPOther        = "mcp_other"
	Issue           = "issue"
	AgentExit       = "agent_exit"
	ShimSummary     = "shim_summary"
)

// Issue classes decide the verdict an issue forces.
const (
	ClassInconclusive = "inconclusive"
	ClassChanged      = "changed"
)

// Event is one line of a cassette.
type Event struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	ConnectionID  string          `json:"connection_id"`
	ExchangeID    string          `json:"exchange_id,omitempty"`
	Seq           int             `json:"seq"`
	EventType     string          `json:"event_type"`
	Protocol      string          `json:"protocol,omitempty"`
	RequestHash   string          `json:"request_hash,omitempty"`
	Request       json.RawMessage `json:"request,omitempty"`
	Response      json.RawMessage `json:"response,omitempty"`
	Meta          map[string]any  `json:"meta,omitempty"`
}

// MetaString reads a string meta field.
func (e *Event) MetaString(k string) string {
	s, _ := e.Meta[k].(string)
	return s
}

// NewIssue builds an issue event.
func NewIssue(runID, conn, kind, class, detail string) *Event {
	return &Event{SchemaVersion: SchemaVersion, RunID: runID, ConnectionID: conn, EventType: Issue,
		Meta: map[string]any{"kind": kind, "class": class, "detail": detail}}
}

// HeaderMeta is the run-level information in the first event.
type HeaderMeta struct {
	Mode     string       `json:"mode"`
	KeyCheck string       `json:"key_check"`
	Rules    redact.Rules `json:"rules"`
}

// Writer appends events to a file; safe for concurrent use.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// Create opens path for writing. With exclusive, an existing file is an error,
// which is how a baseline is protected from being overwritten.
func Create(path string, exclusive bool) (*Writer, error) {
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if exclusive {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &Writer{f: f, enc: enc}, nil
}

// Write appends one event.
func (w *Writer) Write(e *Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e.SchemaVersion == 0 {
		e.SchemaVersion = SchemaVersion
	}
	return w.enc.Encode(e)
}

// Close flushes and closes the file.
func (w *Writer) Close() error { return w.f.Close() }

// ReadFile loads every event of a cassette.
func ReadFile(path string) ([]*Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses JSONL events and checks the schema version.
func Read(r io.Reader) ([]*Event, error) {
	var out []*Event
	br := bufio.NewReader(r)
	line := 0
	for {
		b, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			line++
			var e Event
			if jerr := json.Unmarshal(b, &e); jerr != nil {
				return nil, fmt.Errorf("cassette line %d: %w", line, jerr)
			}
			if e.SchemaVersion != SchemaVersion {
				return nil, fmt.Errorf("cassette line %d: schema_version %d, this build reads %d", line, e.SchemaVersion, SchemaVersion)
			}
			out = append(out, &e)
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// FindHeader returns the header metadata of a cassette.
func FindHeader(events []*Event) (HeaderMeta, error) {
	for _, e := range events {
		if e.EventType != Header {
			continue
		}
		b, _ := json.Marshal(e.Meta)
		var h HeaderMeta
		if err := json.Unmarshal(b, &h); err != nil {
			return h, err
		}
		return h, nil
	}
	return HeaderMeta{}, errors.New("cassette has no header event")
}

// SortForOutput orders events: header first, then by connection and seq, issues last.
func SortForOutput(events []*Event) {
	rank := func(e *Event) int {
		switch e.EventType {
		case Header:
			return 0
		case Issue, AgentExit, ShimSummary:
			return 2
		}
		return 1
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		if a.ConnectionID != b.ConnectionID {
			return a.ConnectionID < b.ConnectionID
		}
		return a.Seq < b.Seq
	})
}

// Decode parses JSON keeping numbers exact.
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

// Canonical encodes v with sorted keys and no HTML escaping.
func Canonical(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // values come from Decode, so they always encode
	return bytes.TrimRight(b.Bytes(), "\n")
}

// Hash is the request hash of canonical bytes.
func Hash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}
