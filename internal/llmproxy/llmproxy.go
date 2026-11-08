// Package llmproxy records and replays LLM API calls at the HTTP layer.
//
// v1 supports non-streaming Anthropic Messages and OpenAI Chat Completions. Anything
// else (streaming, the Responses API, unknown paths) gets HTTP 501 and an issue that
// makes the run INCONCLUSIVE, so an SDK is never silently broken.
package llmproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
)

// Protocols.
const (
	Anthropic = "anthropic_messages"
	OpenAI    = "openai_chat"
)

// Connection ids: each protocol is one logical lane, matched in arrival order.
const (
	ConnAnthropic = "llm-anthropic"
	ConnOpenAI    = "llm-openai"
)

const maxBody = 64 << 20

type route struct {
	protocol, conn, suffix string
}

func routeFor(path string) (route, bool) {
	switch strings.TrimSuffix(path, "/") {
	case "/v1/messages":
		return route{Anthropic, ConnAnthropic, "/v1/messages"}, true
	case "/v1/chat/completions", "/chat/completions":
		return route{OpenAI, ConnOpenAI, "/chat/completions"}, true
	}
	return route{}, false
}

// Proxy is an http.Handler. Mode is "record", "replay" or "test".
type Proxy struct {
	Mode     string
	RunID    string
	Pipe     *cassette.Pipeline
	Out      *cassette.Writer
	Upstream map[string]string // protocol -> base URL; OpenAI's includes /v1
	Client   *http.Client

	mu       sync.Mutex
	seq      map[string]int
	lanes    map[string]*lane
	outbound atomic.Int64
}

type lane struct {
	events []*cassette.Event
	next   int
}

// New builds a proxy. baseline is only used in replay mode.
func New(mode, runID string, pipe *cassette.Pipeline, out *cassette.Writer, upstream map[string]string, baseline []*cassette.Event) *Proxy {
	p := &Proxy{Mode: mode, RunID: runID, Pipe: pipe, Out: out, Upstream: upstream,
		Client: &http.Client{Timeout: 10 * time.Minute},
		seq:    map[string]int{}, lanes: map[string]*lane{}}
	for _, e := range baseline {
		if e.EventType == cassette.LLMExchange && e.Response != nil {
			l := p.lanes[e.ConnectionID]
			if l == nil {
				l = &lane{}
				p.lanes[e.ConnectionID] = l
			}
			l.events = append(l.events, e)
		}
	}
	return p
}

// Outbound is the number of requests sent upstream. Replay must leave it at 0.
func (p *Proxy) Outbound() int64 { return p.outbound.Load() }

// Unused returns recorded LLM exchanges that replay never served.
func (p *Proxy) Unused() []*cassette.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*cassette.Event
	for _, l := range p.lanes {
		out = append(out, l.events[l.next:]...)
	}
	return out
}

func (p *Proxy) issue(conn, kind, class, detail string) {
	_ = p.Out.Write(cassette.NewIssue(p.RunID, conn, kind, class, detail))
}

func (p *Proxy) nextSeq(conn string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq[conn]++
	return p.seq[conn]
}

func refuse(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
	w.Write(b)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, ok := routeFor(r.URL.Path)
	if !ok || r.Method != http.MethodPost {
		p.issue("llm", "unsupported_endpoint", cassette.ClassInconclusive, r.Method+" "+r.URL.Path+" is not supported in v1")
		refuse(w, http.StatusNotImplemented, "agentreplay_unsupported", "agentreplay v1 does not support "+r.Method+" "+r.URL.Path)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		p.issue(rt.conn, "bad_request", cassette.ClassInconclusive, "reading request body failed")
		refuse(w, http.StatusBadRequest, "agentreplay_bad_request", "could not read body")
		return
	}
	v, err := cassette.Decode(body)
	if err != nil {
		p.issue(rt.conn, "invalid_json", cassette.ClassInconclusive, "request body is not JSON")
		refuse(w, http.StatusBadRequest, "agentreplay_bad_request", "body is not JSON")
		return
	}
	if m, ok := v.(map[string]any); ok && m["stream"] == true {
		p.issue(rt.conn, "unsupported_streaming", cassette.ClassInconclusive, "streaming responses are not supported in v1")
		refuse(w, http.StatusNotImplemented, "agentreplay_unsupported", "agentreplay v1 does not support streaming; set stream=false")
		return
	}
	proc := p.Pipe.ProcessValue(map[string]any{"path": rt.suffix, "body": v}, nil)
	seq := p.nextSeq(rt.conn)
	ev := &cassette.Event{SchemaVersion: cassette.SchemaVersion, RunID: p.RunID, ConnectionID: rt.conn,
		ExchangeID: fmt.Sprintf("%s-%d", rt.conn, seq), Seq: seq, EventType: cassette.LLMExchange,
		Protocol: rt.protocol, RequestHash: proc.Hash, Request: cassette.Canonical(proc.Stored)}

	if p.Mode == "replay" {
		p.replay(w, rt, ev)
		return
	}
	p.forward(w, r, rt, body, ev)
}

func (p *Proxy) replay(w http.ResponseWriter, rt route, ev *cassette.Event) {
	p.mu.Lock()
	l := p.lanes[rt.conn]
	var want *cassette.Event
	if l != nil && l.next < len(l.events) {
		want = l.events[l.next]
	}
	matched := want != nil && want.RequestHash == ev.RequestHash
	if matched {
		l.next++
	}
	p.mu.Unlock()

	if !matched {
		detail := fmt.Sprintf("call %d on %s has hash %s but the cassette has no more calls", ev.Seq, rt.conn, ev.RequestHash)
		if want != nil {
			detail = fmt.Sprintf("call %d on %s: expected %s (recorded %s), got %s", ev.Seq, rt.conn, want.RequestHash, want.ExchangeID, ev.RequestHash)
			ev.Meta = map[string]any{"expected_request": json.RawMessage(want.Request)}
		}
		p.issue(rt.conn, "divergence", cassette.ClassInconclusive, detail)
		_ = p.Out.Write(ev)
		refuse(w, http.StatusConflict, "agentreplay_divergence", detail)
		return
	}
	var stored struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(want.Response, &stored)
	ev.Response = want.Response
	ev.Meta = map[string]any{"replayed_from": want.ExchangeID}
	_ = p.Out.Write(ev)
	body := []byte(stored.Body)
	var s string
	if json.Unmarshal(stored.Body, &s) == nil { // upstream sent non-JSON; it was stored as a string
		body = []byte(s)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(stored.Status)
	w.Write(body)
}

// hop-by-hop and length headers are rebuilt by net/http; Accept-Encoding is
// dropped so the stored body is plain JSON.
var skipHeaders = map[string]bool{"Host": true, "Content-Length": true, "Accept-Encoding": true,
	"Connection": true, "Keep-Alive": true, "Transfer-Encoding": true, "Upgrade": true, "Te": true, "Trailer": true}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, rt route, body []byte, ev *cassette.Event) {
	base := strings.TrimSuffix(p.Upstream[rt.protocol], "/")
	if base == "" {
		p.issue(rt.conn, "no_upstream", cassette.ClassInconclusive, "no upstream configured for "+rt.protocol)
		refuse(w, http.StatusBadGateway, "agentreplay_no_upstream", "no upstream for "+rt.protocol)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+rt.suffix, bytes.NewReader(body))
	if err != nil {
		p.issue(rt.conn, "upstream_error", cassette.ClassInconclusive, "bad upstream URL")
		refuse(w, http.StatusBadGateway, "agentreplay_upstream", "bad upstream URL")
		return
	}
	for k, vs := range r.Header { // credentials are forwarded, never stored
		if !skipHeaders[http.CanonicalHeaderKey(k)] {
			req.Header[k] = vs
		}
	}
	p.outbound.Add(1)
	resp, err := p.Client.Do(req)
	if err != nil {
		p.issue(rt.conn, "upstream_error", cassette.ClassInconclusive, "upstream request failed")
		refuse(w, http.StatusBadGateway, "agentreplay_upstream", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		p.issue(rt.conn, "upstream_error", cassette.ClassInconclusive, "reading upstream response failed")
		refuse(w, http.StatusBadGateway, "agentreplay_upstream", "reading upstream response failed")
		return
	}
	var storedBody any = string(rb)
	if v, err := cassette.Decode(rb); err == nil {
		storedBody = v
	}
	ev.Response = cassette.Canonical(map[string]any{"status": resp.StatusCode, "body": p.Pipe.Red.Apply(storedBody)})
	_ = p.Out.Write(ev)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(rb)
}

// FinalText extracts the assistant text from a recorded LLM response.
func FinalText(e *cassette.Event) string {
	var stored struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(e.Response, &stored) != nil {
		return ""
	}
	switch e.Protocol {
	case Anthropic:
		var b struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		json.Unmarshal(stored.Body, &b)
		var parts []string
		for _, c := range b.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n")
	case OpenAI:
		var b struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		json.Unmarshal(stored.Body, &b)
		if len(b.Choices) > 0 {
			return b.Choices[0].Message.Content
		}
	}
	return ""
}

// Usage returns input and output token counts from a recorded response, if present.
func Usage(e *cassette.Event) (in, out int64) {
	var stored struct {
		Body struct {
			Usage struct {
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		} `json:"body"`
	}
	json.Unmarshal(e.Response, &stored)
	u := stored.Body.Usage
	return u.InputTokens + u.PromptTokens, u.OutputTokens + u.CompletionTokens
}
