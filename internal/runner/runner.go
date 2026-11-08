// Package runner wires the proxies around an agent process for one run.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/diff"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/llmproxy"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
)

// Options for one run.
type Options struct {
	Mode       string // record | replay | test
	Baseline   string // the cassette replayed (replay) or compared against (test)
	Out        string // where the run's cassette goes; required for record and test
	Policy     *diff.Policy
	RedactPath string // record only; replay and test reuse the baseline's rules
	MCPConfig  string
	Upstream   map[string]string
	ShimCmd    []string // how the agent should start the shim: [exe, "mcp-shim"]
	Agent      []string
	Key        []byte
	Timeout    time.Duration
	AgentOut   io.Writer // the agent's stdout and stderr
}

// MCPConfig is the common {"mcpServers": {...}} file format.
type MCPConfig struct {
	MCPServers map[string]MCPServer `json:"mcpServers"`
}

// MCPServer is one stdio server entry.
type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Run executes the agent once in the given mode and returns the verdict.
func Run(o Options) diff.Report {
	r, err := run(o)
	if err != nil {
		return diff.Fail(o.Mode, err.Error())
	}
	return r
}

func run(o Options) (diff.Report, error) {
	if len(o.Agent) == 0 {
		return diff.Report{}, errors.New("no agent command given after --")
	}
	if len(o.Key) == 0 {
		return diff.Report{}, errors.New("no HMAC key: set " + redact.KeyEnv + " (the same value is needed to replay)")
	}
	if o.Mode != "replay" && o.Out == "" {
		return diff.Report{}, errors.New("--out is required")
	}
	if o.Out != "" {
		if _, err := os.Stat(o.Out); err == nil {
			return diff.Report{}, fmt.Errorf("refusing to overwrite %s; pick a new --out", o.Out)
		}
	}

	var rules redact.Rules
	var base []*cassette.Event
	if o.Mode == "record" {
		if o.RedactPath != "" {
			b, err := os.ReadFile(o.RedactPath)
			if err != nil {
				return diff.Report{}, err
			}
			dec := yaml.NewDecoder(strings.NewReader(string(b)))
			dec.KnownFields(true)
			if err := dec.Decode(&rules); err != nil {
				return diff.Report{}, fmt.Errorf("redaction config: %w", err)
			}
		}
		rules = rules.WithDefaults()
	} else {
		var err error
		if base, err = cassette.ReadFile(o.Baseline); err != nil {
			return diff.Report{}, fmt.Errorf("baseline: %w", err)
		}
		h, err := cassette.FindHeader(base)
		if err != nil {
			return diff.Report{}, err
		}
		if h.KeyCheck != redact.KeyCheck(o.Key) {
			return diff.Report{}, fmt.Errorf("%s does not match the key this cassette was recorded with; fingerprints would not match, so nothing was run", redact.KeyEnv)
		}
		rules = h.Rules
	}
	pipe, err := cassette.NewPipeline(o.Key, rules)
	if err != nil {
		return diff.Report{}, err
	}

	tmp, err := os.MkdirTemp("", "agentreplay-")
	if err != nil {
		return diff.Report{}, err
	}
	defer os.RemoveAll(tmp)
	runID := newID()

	llmPart := filepath.Join(tmp, "llm.jsonl")
	lw, err := cassette.Create(llmPart, true)
	if err != nil {
		return diff.Report{}, err
	}
	proxy := llmproxy.New(o.Mode, runID, pipe, lw, o.Upstream, base)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		lw.Close()
		return diff.Report{}, err
	}
	srv := &http.Server{Handler: proxy}
	go srv.Serve(ln)
	addr := "http://" + ln.Addr().String()

	mcpPath, parts, err := writeShimConfig(o, tmp, runID, rules)
	if err != nil {
		srv.Close()
		lw.Close()
		return diff.Report{}, err
	}

	argv := make([]string, len(o.Agent))
	for i, a := range o.Agent {
		argv[i] = strings.ReplaceAll(a, "{mcp_config}", mcpPath)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+addr, "OPENAI_BASE_URL="+addr+"/v1", "AGENTREPLAY_MCP_CONFIG="+mcpPath)
	cmd.Stdout, cmd.Stderr = o.AgentOut, o.AgentOut
	start := time.Now()
	if err := cmd.Start(); err != nil {
		srv.Close()
		lw.Close()
		return diff.Report{}, fmt.Errorf("could not start the agent: %w", err)
	}
	var extra []*cassette.Event
	exitCode := waitAgent(cmd, o.Timeout, func() {
		extra = append(extra, cassette.NewIssue(runID, "agent", "agent_timeout", cassette.ClassInconclusive, "the agent was killed after "+o.Timeout.String()))
	})
	elapsed := time.Since(start)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Shutdown(ctx)
	cancel()
	lw.Close()

	events := []*cassette.Event{{SchemaVersion: cassette.SchemaVersion, RunID: runID, EventType: cassette.Header,
		Meta: toMeta(cassette.HeaderMeta{Mode: o.Mode, KeyCheck: redact.KeyCheck(o.Key), Rules: rules})}}
	llmEvents, err := cassette.ReadFile(llmPart)
	if err != nil {
		return diff.Report{}, err
	}
	events = append(events, llmEvents...)
	spawned := 0
	for _, p := range parts {
		evs := waitForPart(p, 5*time.Second)
		found := false
		for _, e := range evs {
			if e.EventType == cassette.ShimSummary {
				found = true
				if n, ok := e.Meta["servers_spawned"].(float64); ok {
					spawned += int(n)
				}
			}
		}
		if !found {
			extra = append(extra, cassette.NewIssue(runID, filepath.Base(p), "shim_incomplete", cassette.ClassInconclusive,
				"an MCP connection never finished; the agent may have exited without closing its server"))
		}
		events = append(events, evs...)
	}
	if o.Mode == "replay" {
		if unused := proxy.Unused(); len(unused) > 0 {
			extra = append(extra, cassette.NewIssue(runID, unused[0].ConnectionID, "unused_exchanges", cassette.ClassInconclusive,
				fmt.Sprintf("run ended early: %d recorded LLM call(s) never requested, first %s", len(unused), unused[0].ExchangeID)))
		}
		if proxy.Outbound() > 0 || spawned > 0 {
			extra = append(extra, cassette.NewIssue(runID, "run", "network_in_replay", cassette.ClassInconclusive,
				fmt.Sprintf("replay made %d upstream call(s) and started %d server(s)", proxy.Outbound(), spawned)))
		}
	}
	events = append(events, extra...)
	events = append(events, &cassette.Event{SchemaVersion: cassette.SchemaVersion, RunID: runID, EventType: cassette.AgentExit,
		Meta: map[string]any{"code": exitCode}})
	cassette.SortForOutput(events)
	// round-trip through JSON so meta numbers look the same as when read from disk
	events = normalizeEvents(events)

	if o.Out != "" {
		w, err := cassette.Create(o.Out, true)
		if err != nil {
			return diff.Report{}, err
		}
		for _, e := range events {
			w.Write(e)
		}
		if err := w.Close(); err != nil {
			return diff.Report{}, err
		}
	}

	var rep diff.Report
	switch o.Mode {
	case "record":
		rep = diff.Report{Mode: "record", Verdict: diff.Recorded, Issues: diff.CollectIssues(events, "run")}
		if len(rep.Issues) > 0 {
			rep.Verdict = diff.Inconclusive
			rep.Reason = "the recording has problems that would make it an unreliable baseline"
		}
		rep.ExitCode = diff.ExitCode(rep.Verdict)
	case "replay":
		rep = diff.Compare("replay", base, events, nil)
	default:
		rep = diff.Compare("test", base, events, o.Policy)
	}
	if rep.Metrics == nil {
		rep.Metrics = map[string]any{}
	}
	llmCalls, tokIn, tokOut := 0, int64(0), int64(0)
	for _, e := range events {
		if e.EventType == cassette.LLMExchange && e.Response != nil {
			llmCalls++
			i, out := llmproxy.Usage(e)
			tokIn, tokOut = tokIn+i, tokOut+out
		}
	}
	rep.Metrics["llm_calls"] = llmCalls
	rep.Metrics["llm_tokens_in"] = tokIn
	rep.Metrics["llm_tokens_out"] = tokOut
	rep.Metrics["upstream_llm_calls"] = proxy.Outbound()
	rep.Metrics["mcp_servers_started"] = spawned
	rep.Metrics["wall_ms"] = elapsed.Milliseconds()
	rep.Metrics["agent_exit_code"] = exitCode
	return rep, nil
}

func writeShimConfig(o Options, tmp, runID string, rules redact.Rules) (string, []string, error) {
	if o.MCPConfig == "" {
		return "", nil, nil
	}
	raw, err := os.ReadFile(o.MCPConfig)
	if err != nil {
		return "", nil, err
	}
	var cfg MCPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", nil, fmt.Errorf("MCP config %s: %w", o.MCPConfig, err)
	}
	keyFile := filepath.Join(tmp, "key")
	if err := os.WriteFile(keyFile, o.Key, 0o600); err != nil {
		return "", nil, err
	}
	ruleFile := filepath.Join(tmp, "rules.json")
	rb, _ := json.Marshal(rules)
	if err := os.WriteFile(ruleFile, rb, 0o600); err != nil {
		return "", nil, err
	}
	out := MCPConfig{MCPServers: map[string]MCPServer{}}
	var parts []string
	var names []string
	for n := range cfg.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s := cfg.MCPServers[name]
		part := filepath.Join(tmp, "mcp-"+name+".jsonl")
		parts = append(parts, part)
		args := append(append([]string{}, o.ShimCmd[1:]...), "--mode", o.Mode, "--conn", "mcp-"+name, "--run-id", runID,
			"--part", part, "--rules", ruleFile, "--key-file", keyFile)
		if o.Baseline != "" {
			abs, _ := filepath.Abs(o.Baseline)
			args = append(args, "--baseline", abs)
		}
		args = append(append(args, "--", s.Command), s.Args...)
		out.MCPServers[name] = MCPServer{Command: o.ShimCmd[0], Args: args, Env: s.Env}
	}
	path := filepath.Join(tmp, "mcp.json")
	b, _ := json.MarshalIndent(out, "", "  ")
	return path, parts, os.WriteFile(path, b, 0o600)
}

func waitAgent(cmd *exec.Cmd, timeout time.Duration, onTimeout func()) int {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		err = <-done
		onTimeout()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// waitForPart reads a shim's part file, waiting briefly for it to finish: agents
// do not always wait for their MCP servers to exit.
func waitForPart(path string, limit time.Duration) []*cassette.Event {
	deadline := time.Now().Add(limit)
	for {
		evs, err := cassette.ReadFile(path)
		if err == nil {
			for _, e := range evs {
				if e.EventType == cassette.ShimSummary {
					return evs
				}
			}
		}
		if time.Now().After(deadline) {
			return evs
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func toMeta(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func normalizeEvents(evs []*cassette.Event) []*cassette.Event {
	out := make([]*cassette.Event, 0, len(evs))
	for _, e := range evs {
		b, _ := json.Marshal(e)
		var n cassette.Event
		json.Unmarshal(b, &n)
		out = append(out, &n)
	}
	return out
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
