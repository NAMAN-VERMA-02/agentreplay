package diff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/llmproxy"
)

type call struct {
	name string
	args map[string]any
}

func run(answer string, calls ...call) []*cassette.Event {
	var evs []*cassette.Event
	for i, c := range calls {
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i, "method": "tools/call",
			"params": map[string]any{"name": c.name, "arguments": c.args}})
		evs = append(evs, &cassette.Event{ConnectionID: "mcp-files", Seq: i + 1, EventType: cassette.MCPExchange, Request: req, Response: []byte(`{}`)})
	}
	resp, _ := json.Marshal(map[string]any{"status": 200, "body": map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}}})
	evs = append(evs, &cassette.Event{ConnectionID: llmproxy.ConnAnthropic, Seq: 1, EventType: cassette.LLMExchange,
		Protocol: llmproxy.Anthropic, Response: resp})
	return evs
}

var (
	search = call{"search_files", map[string]any{"root": "/allowed-root", "query": "report"}}
	read   = call{"read_file", map[string]any{"user_id": "u-1", "path": "/allowed-root/report.pdf"}}
	policy = &Policy{
		Required:  []Requirement{{Tool: "search_files", Arguments: map[string]any{"root": map[string]any{"prefix": "/allowed-root"}}}},
		Forbidden: []string{"delete_file"},
		Output:    Output{Mode: "contains", Expected: []string{"FILE: /allowed-root/report.pdf"}},
	}
)

func TestSameWhenOnlyWordingChanges(t *testing.T) {
	base := run("FILE: /allowed-root/report.pdf", search, read)
	cand := run("Done. FILE: /allowed-root/report.pdf — anything else?", search, read)
	r := Compare("test", base, cand, policy)
	if r.Verdict != Same || r.ExitCode != 0 {
		t.Fatalf("got %s\n%s", r.Verdict, Render(r))
	}
}

func TestStructuralChangeIsChanged(t *testing.T) {
	base := run("FILE: /allowed-root/report.pdf", search, read)
	cand := run("FILE: /allowed-root/report.pdf", search)
	r := Compare("test", base, cand, policy)
	if r.Verdict != Changed || !strings.Contains(Render(r), "- read_file") {
		t.Fatalf("got %s\n%s", r.Verdict, Render(r))
	}
}

func TestContractAndOutputFailures(t *testing.T) {
	base := run("FILE: /allowed-root/report.pdf", search, read)
	wrongRoot := call{"search_files", map[string]any{"root": "/etc", "query": "report"}}
	r := Compare("test", base, run("FILE: /allowed-root/report.pdf", wrongRoot, read), policy)
	if r.Verdict != Changed || !strings.Contains(Render(r), `want prefix "/allowed-root"`) {
		t.Fatalf("prefix rule not enforced\n%s", Render(r))
	}
	r = Compare("test", base, run("I could not find anything.", search, read), policy)
	if r.Verdict != Changed {
		t.Fatalf("wrong answer passed\n%s", Render(r))
	}
}

func TestForbiddenCallBeatsInconclusive(t *testing.T) {
	base := run("FILE: /allowed-root/report.pdf", search, read)
	cand := run("FILE: /allowed-root/report.pdf", search, read, call{"delete_file", map[string]any{"path": "/x"}})
	cand = append(cand, cassette.NewIssue("r", "llm", "unsupported_streaming", cassette.ClassInconclusive, ""))
	if r := Compare("test", base, cand, policy); r.Verdict != Changed {
		t.Fatalf("an observed forbidden call must be CHANGED even in a compromised run, got %s", r.Verdict)
	}
}

func TestInconclusiveBeatsSoftDifferences(t *testing.T) {
	base := run("FILE: /allowed-root/report.pdf", search, read)
	cand := append(run("", search), cassette.NewIssue("r", "llm", "unsupported_streaming", cassette.ClassInconclusive, ""))
	if r := Compare("test", base, cand, policy); r.Verdict != Inconclusive || r.ExitCode != 2 {
		t.Fatalf("a run broken by an unsupported feature must not read as a regression, got %s", r.Verdict)
	}
	flawed := append(run("FILE: /allowed-root/report.pdf", search, read), cassette.NewIssue("r", "mcp-files", "no_response", cassette.ClassInconclusive, ""))
	if r := Compare("test", flawed, run("FILE: /allowed-root/report.pdf", search, read), policy); r.Verdict != Inconclusive {
		t.Fatalf("a flawed baseline must make the result INCONCLUSIVE, got %s", r.Verdict)
	}
}

func TestStructuralOnlyScopeIsAnnounced(t *testing.T) {
	base := run("found it", search)
	r := Compare("test", base, run("nothing found", search), &Policy{})
	if r.Verdict != Same || r.Scope != StructuralOnly {
		t.Fatalf("got %s / %q", r.Verdict, r.Scope)
	}
}

func TestPolicyFileValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "p.yaml")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	good := write("required:\n  - tool: search_files\n    arguments: {root: {prefix: /allowed-root}}\nforbidden: [delete_file]\noutput: {mode: contains, expected: [\"FILE:\"]}\n")
	if _, err := LoadPolicy(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"output: {mode: fuzzy}\n",
		"output: {mode: contains}\n",
		"required:\n  - tool: x\n    arguments: {a: {startswith: b}}\n",
		"requird: []\n",
	} {
		if _, err := LoadPolicy(write(bad)); err == nil {
			t.Errorf("accepted bad policy: %s", bad)
		}
	}
}
