package mcpproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
)

// Config describes one proxied connection.
type Config struct {
	Mode     string // record | replay | test
	RunID    string
	Conn     string
	Pipe     *cassette.Pipeline
	Out      *cassette.Writer
	Baseline []*cassette.Event // this connection's events (replay and test)
}

// Upstream is the real server's stdio, used only when recording.
type Upstream struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
}

type pending struct {
	seq     int
	method  string
	proc    cassette.Processed
	overlap bool
}

// Shim sits between an MCP client and server on one stdio connection.
type Shim struct {
	cfg Config

	outMu     sync.Mutex
	clientOut io.Writer

	mu           sync.Mutex
	seq          int
	era          Era
	eraSeen      bool
	pending      map[string]*pending
	inflight     map[string][]*pending
	lastExchange string

	baseEra    Era
	pool       *cassette.Pool
	notesAfter map[string][]*cassette.Event
	checked    map[string]bool
}

// Run proxies until the client closes its side. up must be set in record mode and
// nil otherwise: replay and test never start the real server.
func Run(cfg Config, clientIn io.Reader, clientOut io.Writer, up *Upstream) error {
	s := &Shim{cfg: cfg, clientOut: clientOut, pending: map[string]*pending{},
		inflight: map[string][]*pending{}, notesAfter: map[string][]*cassette.Event{}, checked: map[string]bool{}}
	spawned := 0
	var err error
	if cfg.Mode == "record" {
		if up == nil {
			return fmt.Errorf("record mode needs the real server")
		}
		spawned = 1
		err = s.record(clientIn, up)
	} else {
		s.loadBaseline()
		err = s.serve(clientIn)
	}
	s.write(&cassette.Event{ConnectionID: cfg.Conn, EventType: cassette.ShimSummary,
		Meta: map[string]any{"mode": cfg.Mode, "era": string(s.era), "servers_spawned": spawned}})
	return err
}

func (s *Shim) write(e *cassette.Event) {
	e.SchemaVersion = cassette.SchemaVersion
	e.RunID = s.cfg.RunID
	_ = s.cfg.Out.Write(e)
}

func (s *Shim) issue(kind, class, detail string) {
	s.write(cassette.NewIssue(s.cfg.RunID, s.cfg.Conn, kind, class, detail))
}

func (s *Shim) nextSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

func (s *Shim) exchangeID(seq int) string { return fmt.Sprintf("%s-%d", s.cfg.Conn, seq) }

func (s *Shim) send(line []byte) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.clientOut.Write(append(bytes.TrimRight(line, "\r\n"), '\n'))
}

type kind int

const (
	kRequest kind = iota
	kNotification
	kResponse
)

func classify(msg map[string]any) kind {
	_, hasID := msg["id"]
	_, hasMethod := msg["method"]
	switch {
	case hasMethod && hasID:
		return kRequest
	case hasMethod:
		return kNotification
	}
	return kResponse
}

func readLines(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parse(line []byte) (map[string]any, bool) {
	v, err := cassette.Decode(bytes.TrimSpace(line))
	m, ok := v.(map[string]any)
	return m, err == nil && ok
}

func (s *Shim) noteEra(msg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.eraSeen {
		s.eraSeen = true
		s.era = DetectEra(msg)
	}
}

func idKey(msg map[string]any) string { return string(cassette.Canonical(msg["id"])) }

// ---- record ----

func (s *Shim) record(clientIn io.Reader, up *Upstream) error {
	done := make(chan struct{})
	go func() {
		readLines(up.Stdout, s.fromServer)
		close(done)
	}()
	err := readLines(clientIn, func(line []byte) {
		s.fromClient(line)
		up.Stdin.Write(append(bytes.TrimRight(line, "\r\n"), '\n'))
	})
	up.Stdin.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // a server that ignores EOF must not hang the run
	}
	s.mu.Lock()
	left := len(s.pending)
	s.mu.Unlock()
	if left > 0 {
		s.issue("no_response", cassette.ClassInconclusive, fmt.Sprintf("%d request(s) never got a response", left))
	}
	return err
}

func (s *Shim) fromClient(line []byte) {
	msg, ok := parse(line)
	if !ok {
		s.issue("invalid_json", cassette.ClassInconclusive, "client sent a line that is not a JSON object")
		return
	}
	s.noteEra(msg)
	method, _ := msg["method"].(string)
	switch classify(msg) {
	case kRequest:
		p := &pending{seq: s.nextSeq(), method: method, proc: s.cfg.Pipe.ProcessValue(msg, Normalize)}
		s.mu.Lock()
		// an identical request already in flight: which caller gets which
		// response depends on scheduling, so mark both
		if others := s.inflight[p.proc.Hash]; len(others) > 0 {
			p.overlap = true
			for _, o := range others {
				o.overlap = true
			}
		}
		s.inflight[p.proc.Hash] = append(s.inflight[p.proc.Hash], p)
		s.pending[idKey(msg)] = p
		era := s.era
		s.mu.Unlock()
		if !requests[era][method] {
			s.issue("unsupported_method", cassette.ClassInconclusive, fmt.Sprintf("%s (%s era) is recorded but cannot be replayed in v1", method, era))
		}
	case kNotification:
		s.recordNote(msg, "c2s")
		if !clientNotifications[s.era][method] {
			s.issue("unsupported_notification", cassette.ClassInconclusive, method+" from the client cannot be replayed in v1")
		}
	default:
		s.recordOther(msg, "c2s")
	}
}

func (s *Shim) fromServer(line []byte) {
	defer s.send(line)
	msg, ok := parse(line)
	if !ok {
		s.issue("invalid_json", cassette.ClassInconclusive, "server sent a line that is not a JSON object")
		return
	}
	method, _ := msg["method"].(string)
	switch classify(msg) {
	case kResponse:
		s.mu.Lock()
		p := s.pending[idKey(msg)]
		delete(s.pending, idKey(msg))
		if p != nil {
			list := s.inflight[p.proc.Hash]
			for i, o := range list {
				if o == p {
					s.inflight[p.proc.Hash] = append(list[:i], list[i+1:]...)
					break
				}
			}
		}
		era := s.era
		s.mu.Unlock()
		if p == nil {
			s.issue("unmatched_response", cassette.ClassInconclusive, "server answered an id the client never sent")
			return
		}
		resp := cassette.Canonical(s.cfg.Pipe.Red.Apply(msg))
		meta := map[string]any{"era": string(era), "method": p.method}
		if p.overlap {
			meta["overlap"] = true
		}
		id := s.exchangeID(p.seq)
		s.write(&cassette.Event{ConnectionID: s.cfg.Conn, ExchangeID: id, Seq: p.seq, EventType: cassette.MCPExchange,
			Protocol: "mcp", RequestHash: p.proc.Hash, Request: cassette.Canonical(p.proc.Stored), Response: resp, Meta: meta})
		s.mu.Lock()
		s.lastExchange = id
		s.mu.Unlock()
	case kNotification:
		s.recordNote(msg, "s2c")
		s.mu.Lock()
		era := s.era
		s.mu.Unlock()
		if !serverNotifications[era][method] {
			s.issue("unsupported_notification", cassette.ClassInconclusive, method+" from the server cannot be replayed in v1")
		}
	default:
		s.recordOther(msg, "s2c")
	}
}

func (s *Shim) recordNote(msg map[string]any, dir string) {
	s.mu.Lock()
	after := s.lastExchange
	s.mu.Unlock()
	s.write(&cassette.Event{ConnectionID: s.cfg.Conn, Seq: s.nextSeq(), EventType: cassette.MCPNotification, Protocol: "mcp",
		Request: cassette.Canonical(s.cfg.Pipe.Red.Apply(msg)), Meta: map[string]any{"direction": dir, "after_exchange": after}})
}

func (s *Shim) recordOther(msg map[string]any, dir string) {
	s.write(&cassette.Event{ConnectionID: s.cfg.Conn, Seq: s.nextSeq(), EventType: cassette.MCPOther, Protocol: "mcp",
		Request: cassette.Canonical(s.cfg.Pipe.Red.Apply(msg)), Meta: map[string]any{"direction": dir}})
	m, _ := msg["method"].(string)
	s.issue("unsupported_message", cassette.ClassInconclusive, fmt.Sprintf("%s message %q (server requests and client replies) cannot be replayed in v1", dir, m))
}

// ---- replay and test ----

func (s *Shim) loadBaseline() {
	var exchanges []*cassette.Event
	s.baseEra = Unknown
	for _, e := range s.cfg.Baseline {
		switch e.EventType {
		case cassette.MCPExchange:
			if e.Response == nil {
				continue
			}
			exchanges = append(exchanges, e)
			if era := e.MetaString("era"); era != "" {
				s.baseEra = Era(era)
			}
		case cassette.MCPNotification:
			if e.MetaString("direction") == "s2c" {
				k := e.MetaString("after_exchange")
				s.notesAfter[k] = append(s.notesAfter[k], e)
			}
		}
	}
	key := func(e *cassette.Event) string { return e.RequestHash }
	if s.cfg.Mode == "test" {
		key = func(e *cassette.Event) string {
			v, _ := cassette.Decode(e.Request)
			return toolKey(v)
		}
	}
	s.pool = cassette.NewPool(exchanges, key)
}

func (s *Shim) reply(id any, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	return b
}

func (s *Shim) serve(clientIn io.Reader) error {
	first := true
	err := readLines(clientIn, func(line []byte) {
		msg, ok := parse(line)
		if !ok {
			s.issue("invalid_json", cassette.ClassInconclusive, "client sent a line that is not a JSON object")
			return
		}
		s.noteEra(msg)
		if first {
			first = false
			if s.era != s.baseEra {
				s.issue("era_mismatch", cassette.ClassInconclusive,
					fmt.Sprintf("the cassette was recorded in the %s era but the client speaks %s", s.baseEra, s.era))
			}
		}
		method, _ := msg["method"].(string)
		switch classify(msg) {
		case kRequest:
			s.serveRequest(msg, method)
		case kNotification:
			s.write(&cassette.Event{ConnectionID: s.cfg.Conn, Seq: s.nextSeq(), EventType: cassette.MCPNotification, Protocol: "mcp",
				Request: cassette.Canonical(s.cfg.Pipe.Red.Apply(msg)), Meta: map[string]any{"direction": "c2s"}})
			if s.era != s.baseEra || !clientNotifications[s.era][method] {
				s.issue("unsupported_notification", cassette.ClassInconclusive, method+" cannot be honoured during "+s.cfg.Mode)
			}
		default:
			s.issue("unsupported_message", cassette.ClassInconclusive, "client replies to server requests cannot be replayed in v1")
		}
	})
	if s.cfg.Mode == "replay" {
		if unused := s.pool.Unused(); len(unused) > 0 {
			s.issue("unused_exchanges", cassette.ClassInconclusive,
				fmt.Sprintf("run ended early: %d recorded exchange(s) never requested, first %s", len(unused), unused[0].ExchangeID))
		}
	}
	return err
}

func (s *Shim) serveRequest(msg map[string]any, method string) {
	seq := s.nextSeq()
	proc := s.cfg.Pipe.ProcessValue(msg, Normalize)
	ev := &cassette.Event{ConnectionID: s.cfg.Conn, ExchangeID: s.exchangeID(seq), Seq: seq, EventType: cassette.MCPExchange,
		Protocol: "mcp", RequestHash: proc.Hash, Request: cassette.Canonical(proc.Stored), Meta: map[string]any{"era": string(s.era), "method": method}}

	if s.era != s.baseEra || !requests[s.era][method] {
		ev.Response = s.reply(msg["id"], -32601, fmt.Sprintf("agentreplay: %s is not supported for %s in the %s era", method, s.cfg.Mode, s.era))
		s.write(ev)
		s.send(ev.Response)
		if s.era == s.baseEra {
			s.issue("unsupported_method", cassette.ClassInconclusive, method+" is outside the v1 replay surface")
		}
		return
	}

	var rec *cassette.Event
	var ok bool
	if s.cfg.Mode == "replay" {
		rec, ok = s.pool.Take(proc.Hash)
	} else {
		k := toolKey(proc.Stored)
		rec, ok = s.pool.Take(k)
		if !ok && method != "tools/call" {
			rec, ok = s.pool.Last(k) // discovery may be repeated; reuse the recorded answer
		}
	}
	if !ok {
		ev.Response = s.reply(msg["id"], -32000, "agentreplay: no recorded response for this request")
		s.write(ev)
		s.send(ev.Response)
		if s.cfg.Mode == "test" && method == "tools/call" {
			s.issue("unexpected_tool_call", cassette.ClassChanged, "no baseline fixture for "+toolKey(proc.Stored))
		} else {
			s.issue("divergence", cassette.ClassInconclusive, fmt.Sprintf("%s request %d (hash %s) has no matching recorded exchange", method, seq, proc.Hash))
		}
		return
	}
	ev.Response = remapID(rec.Response, msg["id"])
	ev.Meta["replayed_from"] = rec.ExchangeID
	s.write(ev)
	s.send(ev.Response)
	if rec.Meta["overlap"] == true {
		s.checkAmbiguity(rec)
	}
	for _, n := range s.notesAfter[rec.ExchangeID] {
		s.write(&cassette.Event{ConnectionID: s.cfg.Conn, Seq: s.nextSeq(), EventType: cassette.MCPNotification, Protocol: "mcp",
			Request: n.Request, Meta: map[string]any{"direction": "s2c", "after_exchange": ev.ExchangeID}})
		s.send(n.Request)
	}
}

// checkAmbiguity: identical requests overlapped while recording. If their recorded
// responses differ, which caller got which depends on scheduling, so equivalence
// cannot be claimed.
func (s *Shim) checkAmbiguity(rec *cassette.Event) {
	if s.checked[rec.RequestHash] {
		return
	}
	s.checked[rec.RequestHash] = true
	var first []byte
	for _, e := range s.cfg.Baseline {
		if e.EventType != cassette.MCPExchange || e.RequestHash != rec.RequestHash || e.Meta["overlap"] != true {
			continue
		}
		body := withoutID(e.Response)
		if first == nil {
			first = body
		} else if !bytes.Equal(first, body) {
			s.issue("ambiguous_concurrent_identical", cassette.ClassInconclusive,
				"identical requests overlapped while recording and got different responses; replay cannot tell which caller got which")
			return
		}
	}
}

func withoutID(raw []byte) []byte {
	v, err := cassette.Decode(raw)
	if m, ok := v.(map[string]any); err == nil && ok {
		delete(m, "id")
		return cassette.Canonical(m)
	}
	return raw
}

// remapID gives a recorded response the id of the request being answered now.
func remapID(raw []byte, id any) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	m["id"] = cassette.Canonical(id)
	b, _ := json.Marshal(m)
	return b
}
