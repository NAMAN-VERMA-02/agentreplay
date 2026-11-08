// bench runs the mutation benchmark: it records the demo agent, replays it, and
// tests 10 planted regressions and 5 harmless changes in both MCP eras, plus guard
// cases that must come out INCONCLUSIVE or COULD_NOT_RUN. It exits 1 if any case
// gets the wrong verdict, so CI fails when the suite can no longer tell good from bad.
//
//	go run ./bench [-replays 20] [-md bench/RESULTS.md]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/diff"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/fake/mcpserver"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/fake/model"
)

var (
	replays = flag.Int("replays", 20, "replays per era for latency")
	mdOut   = flag.String("md", "", "also write the results as Markdown to this file")
)

const apiKey = "sk-ant-PLANTEDAPIKEY0000000000"

type mutation struct{ name, flags string }

var regressions = []mutation{
	{"required tool removed", "--model fake/no-search"},
	{"wrong tool selected", "--model fake/wrong-tool"},
	{"wrong file path", "--model fake/wrong-path"},
	{"wrong user ID", "--model fake/wrong-user"},
	{"tool arguments swapped", "--model fake/swapped-args"},
	{"required step skipped", "--model fake/skip-step"},
	{"forbidden tool invoked", "--model fake/forbidden"},
	{"error treated as success", "--model fake/error-as-success"},
	{"tool result ignored", "--model fake/ignore-result"},
	{"invalid final output contract", "--model fake/bad-format"},
}

var harmless = []mutation{
	{"JSON key order", "--reverse-arg-keys"},
	{"request IDs", "--rpc-ids string"},
	{"timestamps", "--meta-timestamps"},
	{"non-semantic metadata", "--client-version 9.9.9"},
	{"reworded final answer", "--model fake/reworded"},
}

type row struct {
	group, era, name, expect, got string
	detail                        string
}

func (r row) ok() bool { return r.expect == r.got }

type bench struct {
	dir, bin, mcp, policy, model string
	n                            int
	rows                         []row
	written                      []string
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(3)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "agentreplay-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	fmt.Fprintln(os.Stderr, "building binaries...")
	build := exec.Command("go", "build", "-o", dir+string(os.PathSeparator), "./cmd/agentreplay", "./examples/demoagent", "./examples/fakemcp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}
	var hits atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go http.Serve(ln, model.Handler(&hits))

	b := &bench{dir: dir, bin: filepath.Join(dir, "agentreplay"+exe), model: "http://" + ln.Addr().String()}
	b.mcp = filepath.Join(dir, "mcp.json")
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"files": map[string]any{"command": filepath.Join(dir, "fakemcp"+exe)}}})
	os.WriteFile(b.mcp, cfg, 0o600)
	b.policy, _ = filepath.Abs(filepath.Join("examples", "agentreplay.yaml"))
	os.Setenv("AGENTREPLAY_HMAC_KEY", "bench-key")
	os.Setenv("ANTHROPIC_API_KEY", apiKey)
	os.Setenv("OPENAI_API_KEY", apiKey)
	agent := filepath.Join(dir, "demoagent"+exe)

	var lat []float64
	var llmCalls, tokIn, tokOut float64
	for _, era := range []string{"legacy", "modern"} {
		base := b.path("base-" + era)
		ag := func(extra string) []string {
			return append([]string{agent, "--mcp-config", "{mcp_config}", "--era", era}, strings.Fields(extra)...)
		}
		rep := b.cli(append([]string{"record", "--out", base, "--mcp-config", b.mcp, "--anthropic-upstream", b.model, "--"}, ag("")...)...)
		if rep.Verdict != diff.Recorded {
			return fmt.Errorf("%s baseline: %s %s %v", era, rep.Verdict, rep.Reason, rep.Issues)
		}
		llmCalls, tokIn, tokOut = num(rep.Metrics["llm_calls"]), num(rep.Metrics["llm_tokens_in"]), num(rep.Metrics["llm_tokens_out"])

		before := hits.Load()
		for i := range *replays {
			rep := b.cli(append([]string{"replay", "--cassette", base, "--mcp-config", b.mcp, "--"}, ag("")...)...)
			lat = append(lat, num(rep.Metrics["wall_ms"]))
			if i == 0 {
				b.add("replay", era, "baseline replay offline", diff.Same, rep)
				if num(rep.Metrics["upstream_llm_calls"]) != 0 || num(rep.Metrics["mcp_servers_started"]) != 0 {
					b.rows = append(b.rows, row{"replay", era, "zero network during replay", "0", "nonzero", fmt.Sprint(rep.Metrics)})
				}
			}
		}
		netRow := row{"replay", era, fmt.Sprintf("model requests during %d replays", *replays), "0", fmt.Sprint(hits.Load() - before), ""}
		b.rows = append(b.rows, netRow)

		for _, m := range regressions {
			b.add("regression", era, m.name, diff.Changed, b.test(base, ag(m.flags)))
		}
		for _, m := range harmless {
			b.add("harmless", era, m.name, diff.Same, b.test(base, ag(m.flags)))
		}
		b.add("guard", era, "streaming request", diff.Inconclusive, b.test(base, ag("--stream")))
		b.add("guard", era, "replaying a changed agent", diff.Inconclusive,
			b.cli(append([]string{"replay", "--cassette", base, "--mcp-config", b.mcp, "--"}, ag("--model fake/skip-step")...)...))
	}
	legacyBase := filepath.Join(b.dir, "base-legacy.jsonl")
	b.add("guard", "modern", "cassette replayed in the wrong era", diff.Inconclusive,
		b.cli("replay", "--cassette", legacyBase, "--mcp-config", b.mcp, "--", agent, "--mcp-config", "{mcp_config}", "--era", "modern"))
	b.add("guard", "legacy", "baseline overwrite refused", diff.CouldNotRun,
		b.cli("record", "--out", legacyBase, "--mcp-config", b.mcp, "--anthropic-upstream", b.model, "--", agent, "--mcp-config", "{mcp_config}"))
	os.Setenv("AGENTREPLAY_HMAC_KEY", "wrong-key")
	b.add("guard", "legacy", "wrong HMAC key", diff.CouldNotRun,
		b.cli("replay", "--cassette", legacyBase, "--mcp-config", b.mcp, "--", agent, "--mcp-config", "{mcp_config}"))
	os.Setenv("AGENTREPLAY_HMAC_KEY", "bench-key")

	// OpenAI Chat Completions adapter
	oaBase := b.path("base-openai")
	oa := func(extra string) []string {
		return append([]string{agent, "--mcp-config", "{mcp_config}", "--provider", "openai"}, strings.Fields(extra)...)
	}
	rep := b.cli(append([]string{"record", "--out", oaBase, "--mcp-config", b.mcp, "--openai-upstream", b.model + "/v1", "--"}, oa("")...)...)
	if rep.Verdict != diff.Recorded {
		return fmt.Errorf("openai baseline: %s %v", rep.Verdict, rep.Issues)
	}
	b.add("openai", "legacy", "baseline replay offline", diff.Same, b.cli(append([]string{"replay", "--cassette", oaBase, "--mcp-config", b.mcp, "--"}, oa("")...)...))
	b.add("openai", "legacy", "wrong user ID", diff.Changed, b.testWith(oaBase, oa("--model fake/wrong-user"), "--openai-upstream", b.model+"/v1"))
	b.add("openai", "legacy", "reworded final answer", diff.Same, b.testWith(oaBase, oa("--model fake/reworded"), "--openai-upstream", b.model+"/v1"))

	leaks := 0
	for _, p := range b.written {
		raw, _ := os.ReadFile(p)
		for _, s := range []string{apiKey, mcpserver.Secret} {
			if strings.Contains(string(raw), s) {
				leaks++
			}
		}
	}
	b.rows = append(b.rows, row{"secrets", "both", fmt.Sprintf("planted secrets found in %d written cassettes", len(b.written)), "0", fmt.Sprint(leaks), ""})

	md := b.report(lat, llmCalls, tokIn, tokOut)
	fmt.Print(md)
	if *mdOut != "" {
		os.WriteFile(*mdOut, []byte(md), 0o644)
	}
	for _, r := range b.rows {
		if !r.ok() {
			os.Exit(1)
		}
	}
	return nil
}

func (b *bench) path(name string) string {
	b.n++
	p := filepath.Join(b.dir, fmt.Sprintf("%02d-%s.jsonl", b.n, name))
	if strings.HasPrefix(name, "base-") {
		p = filepath.Join(b.dir, name+".jsonl")
	}
	b.written = append(b.written, p)
	return p
}

func (b *bench) test(base string, agent []string) diff.Report {
	return b.testWith(base, agent, "--anthropic-upstream", b.model)
}

func (b *bench) testWith(base string, agent []string, upstream ...string) diff.Report {
	args := append([]string{"test", "--baseline", base, "--out", b.path("cand"), "--policy", b.policy, "--mcp-config", b.mcp}, upstream...)
	return b.cli(append(append(args, "--"), agent...)...)
}

func (b *bench) cli(args ...string) diff.Report {
	jsonPath := filepath.Join(b.dir, fmt.Sprintf("report-%d.json", time.Now().UnixNano()))
	full := append([]string{args[0], "--json", jsonPath, "--timeout", "60s"}, args[1:]...)
	cmd := exec.Command(b.bin, full...)
	cmd.Run()
	var rep diff.Report
	raw, err := os.ReadFile(jsonPath)
	if err != nil || json.Unmarshal(raw, &rep) != nil {
		return diff.Report{Verdict: "NO_REPORT", Metrics: map[string]any{}}
	}
	if rep.Metrics == nil {
		rep.Metrics = map[string]any{}
	}
	return rep
}

func (b *bench) add(group, era, name, expect string, rep diff.Report) {
	var why []string
	for _, c := range rep.Checks {
		if !c.Pass {
			why = append(why, c.Level+": "+c.Name)
		}
	}
	for _, is := range rep.Issues {
		why = append(why, is.Kind)
	}
	if rep.Reason != "" {
		why = append(why, rep.Reason)
	}
	b.rows = append(b.rows, row{group, era, name, expect, rep.Verdict, strings.Join(why, "; ")})
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64{}, xs...)
	sort.Float64s(s)
	i := int(p*float64(len(s)-1) + 0.5)
	return s[i]
}

func (b *bench) report(lat []float64, calls, tokIn, tokOut float64) string {
	var sb strings.Builder
	tp, fn, inc, tn, fp := 0, 0, 0, 0, 0
	for _, r := range b.rows {
		switch r.group {
		case "regression":
			switch r.got {
			case diff.Changed:
				tp++
			case diff.Same:
				fn++
			default:
				inc++
			}
		case "harmless":
			switch r.got {
			case diff.Same:
				tn++
			case diff.Changed:
				fp++
			default:
				inc++
			}
		}
	}
	fmt.Fprintf(&sb, "# agentreplay mutation benchmark\n\n")
	fmt.Fprintf(&sb, "Demo agent + scripted model + fake MCP server, both MCP eras (legacy 2025-11-25, modern 2026-07-28). ")
	fmt.Fprintf(&sb, "This is an initial demonstration on one agent and one task, not a general benchmark.\n\n")
	fmt.Fprintf(&sb, "## Confusion matrix (%d regressions, %d harmless changes)\n\n", tp+fn, tn+fp)
	fmt.Fprintf(&sb, "| | verdict CHANGED | verdict SAME | other |\n|---|---|---|---|\n")
	fmt.Fprintf(&sb, "| planted regression | %d (caught) | %d (missed) | %d |\n", tp, fn, inc)
	fmt.Fprintf(&sb, "| harmless change | %d (false alarm) | %d | |\n\n", fp, tn)
	if tp+fn > 0 && tn+fp > 0 {
		fmt.Fprintf(&sb, "- Regressions caught: %d/%d\n- False alarms: %d/%d\n- Missed regressions: %d\n", tp, tp+fn, fp, tn+fp, fn)
	}
	fmt.Fprintf(&sb, "- Replay wall time over %d runs: p50 %.0f ms, p95 %.0f ms, including start-up of the agent, proxy and shim processes\n", len(lat), pct(lat, 0.5), pct(lat, 0.95))
	cost := tokIn/1e6*3 + tokOut/1e6*15
	fmt.Fprintf(&sb, "- Each replay serves %.0f LLM calls (%.0f input / %.0f output tokens) from the cassette. At an illustrative $3 / $15 per million tokens that is $%.4f not spent per run; the fake model's token counts are approximate.\n\n", calls, tokIn, tokOut, cost)
	fmt.Fprintf(&sb, "## Every case\n\n| group | era | case | expected | got | |\n|---|---|---|---|---|---|\n")
	for _, r := range b.rows {
		mark := "ok"
		if !r.ok() {
			mark = "**WRONG**"
		}
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s |\n", r.group, r.era, r.name, r.expect, r.got, mark)
	}
	fmt.Fprintf(&sb, "\n## Why each regression was caught\n\n")
	for _, r := range b.rows {
		if (r.group == "regression" || r.group == "guard") && r.detail != "" {
			fmt.Fprintf(&sb, "- %s / %s: %s\n", r.era, r.name, r.detail)
		}
	}
	return sb.String()
}
