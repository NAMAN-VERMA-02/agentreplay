// Command agentreplay records an agent's LLM and MCP traffic, replays it offline,
// and tests changed agents against the recorded tool fixtures.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/diff"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/llmproxy"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/mcpproxy"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/runner"
)

const usage = `agentreplay: record, replay and regression-test tool-using agents

  agentreplay record --out baseline.jsonl [--mcp-config mcp.json] -- <agent command>
  agentreplay replay --cassette baseline.jsonl [--mcp-config mcp.json] -- <agent command>
  agentreplay test   --baseline baseline.jsonl --out candidate.jsonl [--policy agentreplay.yaml] [--mcp-config mcp.json] -- <agent command>
  agentreplay diff   [--policy agentreplay.yaml] baseline.jsonl candidate.jsonl

The agent reaches the proxies through ANTHROPIC_BASE_URL, OPENAI_BASE_URL and
AGENTREPLAY_MCP_CONFIG; "{mcp_config}" in the agent command is replaced by the
generated MCP config path. ` + redact.KeyEnv + ` must be set for record, replay and test.

Verdicts: SAME 0, CHANGED 1, INCONCLUSIVE 2, COULD_NOT_RUN 3 (record: RECORDED 0).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(3)
	}
	switch os.Args[1] {
	case "record", "replay", "test":
		os.Exit(runMode(os.Args[1], os.Args[2:]))
	case "diff":
		os.Exit(runDiff(os.Args[2:]))
	case "mcp-shim":
		os.Exit(runShim(os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(3)
	}
}

func finish(rep diff.Report, jsonPath string) int {
	fmt.Print(diff.Render(rep))
	if jsonPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(jsonPath, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "writing report:", err)
		}
	}
	return rep.ExitCode
}

func runMode(mode string, args []string) int {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	out := fs.String("out", "", "cassette to write (never overwritten)")
	cassetteF := fs.String("cassette", "", "cassette to replay (replay)")
	baseline := fs.String("baseline", "", "baseline cassette (test)")
	policyF := fs.String("policy", "", "policy file (test)")
	redactF := fs.String("redact", "", "redaction rules file (record)")
	mcpConfig := fs.String("mcp-config", "", "the agent's MCP config ({\"mcpServers\": ...})")
	anthropic := fs.String("anthropic-upstream", "https://api.anthropic.com", "Anthropic base URL (record and test)")
	openai := fs.String("openai-upstream", "https://api.openai.com/v1", "OpenAI base URL (record and test)")
	jsonF := fs.String("json", "", "also write the report as JSON")
	timeout := fs.Duration("timeout", 10*time.Minute, "kill the agent after this long")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	o := runner.Options{Mode: mode, Out: *out, RedactPath: *redactF, MCPConfig: *mcpConfig,
		Upstream: map[string]string{llmproxy.Anthropic: *anthropic, llmproxy.OpenAI: *openai},
		Agent:    fs.Args(), Key: []byte(os.Getenv(redact.KeyEnv)), Timeout: *timeout, AgentOut: os.Stderr}
	switch mode {
	case "replay":
		o.Baseline = *cassetteF
		if o.Baseline == "" {
			return finish(diff.Fail(mode, "--cassette is required"), *jsonF)
		}
	case "test":
		o.Baseline = *baseline
		if o.Baseline == "" {
			return finish(diff.Fail(mode, "--baseline is required"), *jsonF)
		}
		if *policyF != "" {
			p, err := diff.LoadPolicy(*policyF)
			if err != nil {
				return finish(diff.Fail(mode, err.Error()), *jsonF)
			}
			o.Policy = p
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return finish(diff.Fail(mode, err.Error()), *jsonF)
	}
	o.ShimCmd = []string{exe, "mcp-shim"}
	return finish(runner.Run(o), *jsonF)
}

func runDiff(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	policyF := fs.String("policy", "", "policy file")
	jsonF := fs.String("json", "", "also write the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() != 2 {
		return finish(diff.Fail("diff", "want exactly two cassettes: baseline and candidate"), *jsonF)
	}
	var pol *diff.Policy
	if *policyF != "" {
		p, err := diff.LoadPolicy(*policyF)
		if err != nil {
			return finish(diff.Fail("diff", err.Error()), *jsonF)
		}
		pol = p
	}
	base, err := cassette.ReadFile(fs.Arg(0))
	if err != nil {
		return finish(diff.Fail("diff", err.Error()), *jsonF)
	}
	cand, err := cassette.ReadFile(fs.Arg(1))
	if err != nil {
		return finish(diff.Fail("diff", err.Error()), *jsonF)
	}
	return finish(diff.Compare("diff", base, cand, pol), *jsonF)
}

// runShim is started by the agent in place of each MCP server.
func runShim(args []string) int {
	fs := flag.NewFlagSet("mcp-shim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	mode := fs.String("mode", "", "record | replay | test")
	conn := fs.String("conn", "", "connection id")
	runID := fs.String("run-id", "", "run id")
	part := fs.String("part", "", "file this connection's events go to")
	rulesF := fs.String("rules", "", "redaction rules (JSON)")
	keyFile := fs.String("key-file", "", "file holding the HMAC key")
	baseline := fs.String("baseline", "", "baseline cassette (replay and test)")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "agentreplay mcp-shim:", err)
		return 3
	}
	key := []byte(os.Getenv(redact.KeyEnv))
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return fail(err)
		}
		key = b
	}
	var rules redact.Rules
	if *rulesF != "" {
		b, err := os.ReadFile(*rulesF)
		if err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(b, &rules); err != nil {
			return fail(err)
		}
	}
	pipe, err := cassette.NewPipeline(key, rules)
	if err != nil {
		return fail(err)
	}
	w, err := cassette.Create(*part, false)
	if err != nil {
		return fail(err)
	}
	defer w.Close()
	cfg := mcpproxy.Config{Mode: *mode, RunID: *runID, Conn: *conn, Pipe: pipe, Out: w}
	if *baseline != "" {
		evs, err := cassette.ReadFile(*baseline)
		if err != nil {
			return fail(err)
		}
		for _, e := range evs {
			if e.ConnectionID == *conn {
				cfg.Baseline = append(cfg.Baseline, e)
			}
		}
	}
	if *mode != "record" {
		if err := mcpproxy.Run(cfg, os.Stdin, os.Stdout, nil); err != nil {
			return fail(err)
		}
		return 0
	}
	server := fs.Args()
	if len(server) == 0 {
		return fail(fmt.Errorf("record mode needs the server command after --"))
	}
	cmd := exec.Command(server[0], server[1:]...)
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		w.Write(cassette.NewIssue(*runID, *conn, "server_start_failed", cassette.ClassInconclusive, "could not start "+strings.Join(server, " ")))
		w.Write(&cassette.Event{RunID: *runID, ConnectionID: *conn, EventType: cassette.ShimSummary, Meta: map[string]any{"mode": *mode, "servers_spawned": 0}})
		return fail(err) // exiting closes stdout, so the agent sees the server die instead of waiting forever
	}
	err = mcpproxy.Run(cfg, os.Stdin, os.Stdout, &mcpproxy.Upstream{Stdin: stdin, Stdout: stdout})
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
	}
	if err != nil {
		return fail(err)
	}
	return 0
}
