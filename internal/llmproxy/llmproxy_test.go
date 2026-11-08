package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
)

const apiKey = "sk-ant-PLANTEDAPIKEY000000000000"

func setup(t *testing.T) (*cassette.Pipeline, string) {
	t.Helper()
	pipe, err := cassette.NewPipeline([]byte("k"), redact.Rules{}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	return pipe, t.TempDir()
}

func upstream(hits *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), `"second"`) {
			io.WriteString(w, `{"content":[{"type":"text","text":"two"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
			return
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"one"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func record(t *testing.T, pipe *cassette.Pipeline, dir string, up string) string {
	t.Helper()
	path := filepath.Join(dir, "base.jsonl")
	w, _ := cassette.Create(path, true)
	p := New("record", "r1", pipe, w, map[string]string{Anthropic: up}, nil)
	srv := httptest.NewServer(p)
	post(t, srv.URL, `{"model":"m","messages":[{"role":"user","content":"first"}]}`)
	post(t, srv.URL, `{"model":"m","messages":[{"role":"user","content":"second"}]}`)
	srv.Close()
	w.Close()
	return path
}

func TestRecordThenReplayOffline(t *testing.T) {
	pipe, dir := setup(t)
	var hits atomic.Int64
	up := upstream(&hits)
	defer up.Close()
	path := record(t, pipe, dir, up.URL)
	if hits.Load() != 2 {
		t.Fatalf("record sent %d upstream calls, want 2", hits.Load())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "PLANTEDAPIKEY") {
		t.Fatal("API key reached the cassette")
	}

	base, _ := cassette.ReadFile(path)
	hits.Store(0)
	w, _ := cassette.Create(filepath.Join(dir, "replay.jsonl"), true)
	defer w.Close()
	p := New("replay", "r2", pipe, w, map[string]string{Anthropic: up.URL}, base)
	srv := httptest.NewServer(p)
	defer srv.Close()
	_, b1 := post(t, srv.URL, `{"messages":[{"content":"first","role":"user"}],"model":"m"}`)
	_, b2 := post(t, srv.URL, `{"model":"m","messages":[{"role":"user","content":"second"}]}`)
	if !strings.Contains(b1, `"one"`) || !strings.Contains(b2, `"two"`) {
		t.Fatalf("replay served wrong bodies: %s / %s", b1, b2)
	}
	if hits.Load() != 0 || p.Outbound() != 0 {
		t.Fatalf("replay reached the network: upstream hits %d, outbound %d", hits.Load(), p.Outbound())
	}
	if len(p.Unused()) != 0 {
		t.Fatal("all recorded calls were used but some are reported unused")
	}
}

func TestReplayDivergenceIsReported(t *testing.T) {
	pipe, dir := setup(t)
	var hits atomic.Int64
	up := upstream(&hits)
	defer up.Close()
	base, _ := cassette.ReadFile(record(t, pipe, dir, up.URL))

	outPath := filepath.Join(dir, "replay.jsonl")
	w, _ := cassette.Create(outPath, true)
	p := New("replay", "r2", pipe, w, nil, base)
	srv := httptest.NewServer(p)
	status, _ := post(t, srv.URL, `{"model":"other-model","messages":[{"role":"user","content":"first"}]}`)
	srv.Close()
	w.Close()
	if status != http.StatusConflict {
		t.Fatalf("divergent call got %d, want 409", status)
	}
	evs, _ := cassette.ReadFile(outPath)
	if !hasIssue(evs, "divergence") {
		t.Fatal("no divergence issue written")
	}
}

func TestStreamingAndResponsesAPIAreRefused(t *testing.T) {
	pipe, dir := setup(t)
	outPath := filepath.Join(dir, "out.jsonl")
	w, _ := cassette.Create(outPath, true)
	p := New("record", "r", pipe, w, map[string]string{Anthropic: "http://127.0.0.1:1"}, nil)
	srv := httptest.NewServer(p)
	status, _ := post(t, srv.URL, `{"model":"m","stream":true,"messages":[]}`)
	resp, _ := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
	srv.Close()
	w.Close()
	if status != http.StatusNotImplemented || resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("got %d and %d, want 501 for both", status, resp.StatusCode)
	}
	evs, _ := cassette.ReadFile(outPath)
	if !hasIssue(evs, "unsupported_streaming") || !hasIssue(evs, "unsupported_endpoint") {
		t.Fatal("unsupported features were not recorded as issues")
	}
	if p.Outbound() != 0 {
		t.Fatal("refused requests were forwarded")
	}
}

func hasIssue(evs []*cassette.Event, kind string) bool {
	for _, e := range evs {
		if e.EventType == cassette.Issue && e.MetaString("kind") == kind {
			return true
		}
	}
	return false
}
