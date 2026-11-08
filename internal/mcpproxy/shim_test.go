package mcpproxy

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/fake/mcpserver"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
)

type harness struct {
	t    *testing.T
	era  Era
	in   *io.PipeWriter
	out  chan map[string]any
	done chan error
	path string
}

func start(t *testing.T, mode string, era Era, base []*cassette.Event) *harness {
	t.Helper()
	pipe, err := cassette.NewPipeline([]byte("k"), redact.Rules{}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, era: era, out: make(chan map[string]any, 100), done: make(chan error, 1),
		path: filepath.Join(t.TempDir(), mode+".jsonl")}
	w, err := cassette.Create(h.path, true)
	if err != nil {
		t.Fatal(err)
	}
	cinR, cinW := io.Pipe()
	coutR, coutW := io.Pipe()
	h.in = cinW
	var up *Upstream
	if mode == "record" {
		sinR, sinW := io.Pipe()
		soutR, soutW := io.Pipe()
		go func() { mcpserver.Serve(sinR, soutW); soutW.Close() }()
		up = &Upstream{Stdin: sinW, Stdout: soutR}
	}
	var conn []*cassette.Event
	for _, e := range base {
		if e.ConnectionID == "mcp-files" {
			conn = append(conn, e)
		}
	}
	cfg := Config{Mode: mode, RunID: "r", Conn: "mcp-files", Pipe: pipe, Out: w, Baseline: conn}
	go func() {
		err := Run(cfg, cinR, coutW, up)
		coutW.Close()
		w.Close()
		h.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(coutR)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			h.out <- m
		}
		close(h.out)
	}()
	return h
}

func (h *harness) send(id any, method string, params map[string]any) {
	if params == nil {
		params = map[string]any{}
	}
	if h.era == Modern {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    ModernVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "t", "version": "1"},
		}
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		msg["id"] = id
	}
	b, _ := json.Marshal(msg)
	h.in.Write(append(b, '\n'))
}

func (h *harness) recv() map[string]any {
	h.t.Helper()
	select {
	case m, ok := <-h.out:
		if !ok {
			h.t.Fatal("connection closed")
		}
		return m
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for a message")
	}
	return nil
}

func (h *harness) call(id any, method string, params map[string]any) map[string]any {
	h.t.Helper()
	h.send(id, method, params)
	return h.recv()
}

func (h *harness) open(ids func(int) any) {
	if h.era == Legacy {
		h.call(ids(0), "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "t", "version": "1"}})
		h.send(nil, "notifications/initialized", nil)
	} else {
		h.call(ids(0), "server/discover", nil)
	}
}

func (h *harness) finish() []*cassette.Event {
	h.t.Helper()
	h.in.Close()
	if err := <-h.done; err != nil {
		h.t.Fatal(err)
	}
	for range h.out {
	}
	evs, err := cassette.ReadFile(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

func callTool(name string, args map[string]any) map[string]any {
	return map[string]any{"name": name, "arguments": args}
}

func textOf(m map[string]any) string {
	r, _ := m["result"].(map[string]any)
	c, _ := r["content"].([]any)
	if len(c) == 0 {
		return ""
	}
	first, _ := c[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func issues(evs []*cassette.Event) map[string]string {
	out := map[string]string{}
	for _, e := range evs {
		if e.EventType == cassette.Issue {
			out[e.MetaString("kind")] = e.MetaString("class")
		}
	}
	return out
}

// script runs the same session against record and replay. ids lets replay use a
// different id scheme, which must not matter.
func script(t *testing.T, h *harness, ids func(int) any) {
	t.Helper()
	h.open(ids)
	cursor := ""
	pages := 0
	for {
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		r := h.call(ids(1+pages), "tools/list", p)["result"].(map[string]any)
		pages++
		next, _ := r["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if pages < 2 {
		t.Fatalf("expected paginated tools/list, got %d page", pages)
	}
	if got := textOf(h.call(ids(10), "tools/call", callTool("next_ticket", map[string]any{}))); got != "ticket-1" {
		t.Fatalf("first ticket %q", got)
	}
	if got := textOf(h.call(ids(11), "tools/call", callTool("next_ticket", map[string]any{}))); got != "ticket-2" {
		t.Fatalf("second identical call got %q, want ticket-2", got)
	}
	// two requests in flight; the server answers the second one first while recording
	h.send(ids(12), "tools/call", callTool("slow_echo", map[string]any{"text": "slow", "delay_ms": "200"}))
	h.send(ids(13), "tools/call", callTool("slow_echo", map[string]any{"text": "fast", "delay_ms": "0"}))
	got := map[string]string{}
	for range 2 {
		m := h.recv()
		got[string(cassette.Canonical(m["id"]))] = textOf(m)
	}
	if got[string(cassette.Canonical(ids(12)))] != "slow" || got[string(cassette.Canonical(ids(13)))] != "fast" {
		t.Fatalf("responses matched to the wrong requests: %v", got)
	}
	chart := h.call(ids(14), "tools/call", callTool("get_chart", map[string]any{}))
	content := chart["result"].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["type"] != "image" || content[1].(map[string]any)["type"] != "resource" {
		t.Fatalf("non-text content not preserved: %v", content)
	}
	if h.era == Legacy {
		h.call(ids(15), "ping", nil)
		h.call(ids(16), "tools/call", callTool("delete_file", map[string]any{"path": "/tmp/x"}))
		if n := h.recv(); n["method"] != "notifications/tools/list_changed" {
			t.Fatalf("expected list_changed after delete, got %v", n)
		}
	}
}

func intIDs(i int) any    { return i + 1 }
func stringIDs(i int) any { return "req-" + string(rune('a'+i)) }

func TestRecordThenReplayBothEras(t *testing.T) {
	for _, era := range []Era{Legacy, Modern} {
		t.Run(string(era), func(t *testing.T) {
			rec := start(t, "record", era, nil)
			script(t, rec, intIDs)
			base := rec.finish()
			if is := issues(base); len(is) != 0 {
				t.Fatalf("record raised issues: %v", is)
			}
			rep := start(t, "replay", era, base)
			script(t, rep, stringIDs)
			out := rep.finish()
			if is := issues(out); len(is) != 0 {
				t.Fatalf("replay raised issues: %v", is)
			}
			for _, e := range out {
				if e.EventType == cassette.ShimSummary && e.Meta["servers_spawned"] != float64(0) {
					t.Fatal("replay started the real server")
				}
			}
		})
	}
}

func recordLegacy(t *testing.T) []*cassette.Event {
	rec := start(t, "record", Legacy, nil)
	rec.open(intIDs)
	rec.call(2, "tools/call", callTool("read_file", map[string]any{"user_id": "u-1", "path": "/allowed-root/report.pdf"}))
	rec.call(3, "tools/call", callTool("next_ticket", map[string]any{}))
	return rec.finish()
}

func TestSecretInToolOutputIsRedacted(t *testing.T) {
	rec := start(t, "record", Legacy, nil)
	rec.open(intIDs)
	rec.call(2, "tools/call", callTool("read_file", map[string]any{"user_id": "u-1", "path": "/allowed-root/report.pdf"}))
	rec.finish()
	raw, _ := os.ReadFile(rec.path)
	if strings.Contains(string(raw), "PLANTEDSECRET") {
		t.Fatal("secret from tool output reached the cassette")
	}
}

func TestReplayEndingEarlyIsInconclusive(t *testing.T) {
	base := recordLegacy(t)
	rep := start(t, "replay", Legacy, base)
	rep.open(intIDs)
	rep.call(2, "tools/call", callTool("read_file", map[string]any{"user_id": "u-1", "path": "/allowed-root/report.pdf"}))
	if issues(rep.finish())["unused_exchanges"] != cassette.ClassInconclusive {
		t.Fatal("unused recorded exchange not reported")
	}
}

func TestReplayDivergenceGetsErrorNotAGuess(t *testing.T) {
	base := recordLegacy(t)
	rep := start(t, "replay", Legacy, base)
	rep.open(intIDs)
	m := rep.call(2, "tools/call", callTool("read_file", map[string]any{"user_id": "u-2", "path": "/allowed-root/report.pdf"}))
	if m["error"] == nil {
		t.Fatalf("divergent request got a recorded answer: %v", m)
	}
	if issues(rep.finish())["divergence"] != cassette.ClassInconclusive {
		t.Fatal("divergence not reported")
	}
}

func TestWrongEraIsInconclusive(t *testing.T) {
	base := recordLegacy(t)
	rep := start(t, "replay", Modern, base)
	m := rep.call(1, "server/discover", nil)
	if code := m["error"].(map[string]any)["code"]; code != float64(-32601) {
		t.Fatalf("got %v, want -32601", m)
	}
	if issues(rep.finish())["era_mismatch"] != cassette.ClassInconclusive {
		t.Fatal("era mismatch not reported")
	}
}

func TestUnsupportedMethodIsNeverDropped(t *testing.T) {
	base := recordLegacy(t)
	rep := start(t, "replay", Legacy, base)
	rep.open(intIDs)
	m := rep.call(2, "resources/list", nil)
	if code := m["error"].(map[string]any)["code"]; code != float64(-32601) {
		t.Fatalf("got %v, want -32601", m)
	}
	rep.send(nil, "notifications/cancelled", map[string]any{"requestId": 2})
	is := issues(rep.finish())
	if is["unsupported_method"] != cassette.ClassInconclusive || is["unsupported_notification"] != cassette.ClassInconclusive {
		t.Fatalf("unsupported traffic not reported: %v", is)
	}
}

func TestTestModeMatchesByToolAndArguments(t *testing.T) {
	base := recordLegacy(t)
	tst := start(t, "test", Legacy, base)
	// different client identity and id scheme: matching ignores both
	tst.call("a", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "other", "version": "9"}})
	m := tst.call("b", "tools/call", map[string]any{"name": "read_file",
		"arguments": map[string]any{"path": "/allowed-root/report.pdf", "user_id": "u-1"},
		"_meta":     map[string]any{"sentAt": "2030-01-01"}})
	if !strings.Contains(textOf(m), "Q3 revenue") {
		t.Fatalf("fixture not served: %v", m)
	}
	if m := tst.call("c", "tools/call", callTool("delete_file", map[string]any{"path": "/x"})); m["error"] == nil {
		t.Fatal("unexpected tool call got an answer")
	}
	if issues(tst.finish())["unexpected_tool_call"] != cassette.ClassChanged {
		t.Fatal("unexpected tool call not reported as a change")
	}
}

func TestOverlappingIdenticalCalls(t *testing.T) {
	run := func(tool string) map[string]string {
		rec := start(t, "record", Legacy, nil)
		rec.open(intIDs)
		args := map[string]any{"text": "same", "delay_ms": "150"}
		rec.send(2, "tools/call", callTool(tool, args))
		rec.send(3, "tools/call", callTool(tool, args))
		rec.recv()
		rec.recv()
		base := rec.finish()
		rep := start(t, "replay", Legacy, base)
		rep.open(intIDs)
		rep.call(2, "tools/call", callTool(tool, args))
		rep.call(3, "tools/call", callTool(tool, args))
		return issues(rep.finish())
	}
	if is := run("slow_echo"); len(is) != 0 {
		t.Fatalf("identical overlapping calls with identical answers flagged: %v", is)
	}
	if is := run("slow_ticket"); is["ambiguous_concurrent_identical"] != cassette.ClassInconclusive {
		t.Fatalf("overlapping identical calls with different answers not flagged: %v", is)
	}
}
