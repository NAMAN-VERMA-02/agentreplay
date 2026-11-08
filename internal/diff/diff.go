// Package diff compares a candidate run with a baseline and turns the result into
// a verdict. SAME means "no regression detected by the configured assertions", not
// that two runs are equivalent in general.
package diff

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"
	"github.com/NAMAN-VERMA-02/agentreplay/internal/llmproxy"
)

// Verdicts and their exit codes.
const (
	Same         = "SAME"
	Changed      = "CHANGED"
	Inconclusive = "INCONCLUSIVE"
	CouldNotRun  = "COULD_NOT_RUN"
	Recorded     = "RECORDED"
)

// ExitCode maps a verdict to the process exit code.
func ExitCode(v string) int {
	switch v {
	case Same, Recorded:
		return 0
	case Changed:
		return 1
	case Inconclusive:
		return 2
	}
	return 3
}

// StructuralOnly is printed when the policy asserts nothing about behaviour.
const StructuralOnly = "structural checks only — semantic correctness not tested"

// Policy is the agentreplay.yaml file.
type Policy struct {
	Required  []Requirement `yaml:"required" json:"required,omitempty"`
	Forbidden []string      `yaml:"forbidden" json:"forbidden,omitempty"`
	Output    Output        `yaml:"output" json:"output"`
}

// Requirement: at least one call to Tool whose arguments meet every constraint.
// A constraint is a plain value (equality) or a map with prefix, regex or equals.
type Requirement struct {
	Tool      string         `yaml:"tool" json:"tool"`
	Arguments map[string]any `yaml:"arguments" json:"arguments,omitempty"`
}

// Output checks the agent's final answer.
type Output struct {
	Mode     string   `yaml:"mode" json:"mode"` // ignore | exact | contains | regex
	Expected []string `yaml:"expected" json:"expected,omitempty"`
}

// LoadPolicy reads and validates a policy file.
func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	return &p, p.validate()
}

func (p *Policy) validate() error {
	switch p.Output.Mode {
	case "", "ignore", "exact":
	case "contains", "regex":
		if len(p.Output.Expected) == 0 {
			return fmt.Errorf("output mode %q needs expected values", p.Output.Mode)
		}
		if p.Output.Mode == "regex" {
			for _, e := range p.Output.Expected {
				if _, err := regexp.Compile(e); err != nil {
					return fmt.Errorf("output regex %q: %w", e, err)
				}
			}
		}
	default:
		return fmt.Errorf("unknown output mode %q (want ignore, exact, contains or regex)", p.Output.Mode)
	}
	for _, r := range p.Required {
		if r.Tool == "" {
			return fmt.Errorf("a required entry has no tool")
		}
		for name, c := range r.Arguments {
			if m, ok := c.(map[string]any); ok {
				for k, v := range m {
					switch k {
					case "prefix", "equals":
					case "regex":
						if _, err := regexp.Compile(fmt.Sprint(v)); err != nil {
							return fmt.Errorf("%s.%s regex: %w", r.Tool, name, err)
						}
					default:
						return fmt.Errorf("%s.%s: unknown constraint %q (want prefix, regex or equals)", r.Tool, name, k)
					}
				}
			}
		}
	}
	return nil
}

func (p *Policy) asserts() bool {
	return p != nil && (len(p.Required) > 0 || (p.Output.Mode != "" && p.Output.Mode != "ignore"))
}

// ToolCall is one tools/call seen on an MCP connection.
type ToolCall struct {
	Conn string
	Seq  int
	Name string
	Args map[string]any
	Key  string // name + canonical arguments
}

// ToolCalls extracts tools/call requests in order (per connection, by seq).
func ToolCalls(evs []*cassette.Event) []ToolCall {
	var out []ToolCall
	for _, e := range evs {
		if e.EventType != cassette.MCPExchange {
			continue
		}
		v, err := cassette.Decode(e.Request)
		msg, _ := v.(map[string]any)
		if err != nil || msg["method"] != "tools/call" {
			continue
		}
		params, _ := msg["params"].(map[string]any)
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		out = append(out, ToolCall{Conn: e.ConnectionID, Seq: e.Seq, Name: name, Args: args,
			Key: name + string(cassette.Canonical(params["arguments"]))})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Conn != out[j].Conn {
			return out[i].Conn < out[j].Conn
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// FinalAnswer is the text of the last LLM response.
func FinalAnswer(evs []*cassette.Event) string {
	var last *cassette.Event
	for _, e := range evs {
		if e.EventType == cassette.LLMExchange && e.Response != nil && (last == nil || e.Seq >= last.Seq) {
			last = e
		}
	}
	if last == nil {
		return ""
	}
	return llmproxy.FinalText(last)
}

// Check is one assertion's outcome.
type Check struct {
	Level  string `json:"level"` // structural | contract | output
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Issue is an issue event as reported.
type Issue struct {
	Source string `json:"source"` // baseline | candidate | run
	Conn   string `json:"connection,omitempty"`
	Kind   string `json:"kind"`
	Class  string `json:"class"`
	Detail string `json:"detail,omitempty"`
}

// Report is what every command prints and writes.
type Report struct {
	Verdict  string         `json:"verdict"`
	ExitCode int            `json:"exit_code"`
	Mode     string         `json:"mode"`
	Scope    string         `json:"scope,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Checks   []Check        `json:"checks,omitempty"`
	Issues   []Issue        `json:"issues,omitempty"`
	Metrics  map[string]any `json:"metrics,omitempty"`
}

// Fail builds a COULD_NOT_RUN report.
func Fail(mode, reason string) Report {
	return Report{Verdict: CouldNotRun, ExitCode: 3, Mode: mode, Reason: reason}
}

// CollectIssues turns issue and agent_exit events into report issues.
func CollectIssues(evs []*cassette.Event, source string) []Issue {
	var out []Issue
	for _, e := range evs {
		switch e.EventType {
		case cassette.Issue:
			out = append(out, Issue{Source: source, Conn: e.ConnectionID, Kind: e.MetaString("kind"),
				Class: e.MetaString("class"), Detail: e.MetaString("detail")})
		case cassette.AgentExit:
			if code, _ := e.Meta["code"].(float64); code != 0 {
				out = append(out, Issue{Source: source, Kind: "agent_exit_nonzero", Class: cassette.ClassInconclusive,
					Detail: fmt.Sprintf("the agent exited with code %v", code)})
			}
		}
	}
	return out
}

// Compare diffs a candidate against a baseline under a policy (which may be nil).
func Compare(mode string, base, cand []*cassette.Event, pol *Policy) Report {
	r := Report{Mode: mode, Metrics: map[string]any{}}
	for _, is := range CollectIssues(base, "baseline") {
		is.Class = cassette.ClassInconclusive // a flawed baseline cannot prove anything
		r.Issues = append(r.Issues, is)
	}
	r.Issues = append(r.Issues, CollectIssues(cand, "candidate")...)

	bc, cc := ToolCalls(base), ToolCalls(cand)
	r.Metrics["baseline_tool_calls"] = len(bc)
	r.Metrics["candidate_tool_calls"] = len(cc)
	r.Checks = append(r.Checks, structural(bc, cc)...)

	hard := false // facts about what the candidate did, valid even if the run was otherwise compromised
	if pol != nil {
		for _, req := range pol.Required {
			r.Checks = append(r.Checks, required(req, cc))
		}
		for _, f := range pol.Forbidden {
			c := Check{Level: "contract", Name: "forbidden " + f, Pass: true}
			for _, call := range cc {
				if call.Name == f {
					c.Pass, c.Detail, hard = false, fmt.Sprintf("called %s(%s)", f, argsText(call.Args)), true
					break
				}
			}
			r.Checks = append(r.Checks, c)
		}
		if c, ok := output(pol.Output, FinalAnswer(base), FinalAnswer(cand)); ok {
			r.Checks = append(r.Checks, c)
		}
	}
	r.Scope = "structural, contract and output assertions"
	if !pol.asserts() {
		r.Scope = StructuralOnly
	}

	inconclusive := false
	for _, is := range r.Issues {
		switch is.Class {
		case cassette.ClassChanged:
			hard = true
		case cassette.ClassInconclusive:
			inconclusive = true
		}
	}
	failed := false
	for _, c := range r.Checks {
		failed = failed || !c.Pass
	}
	switch {
	case hard:
		r.Verdict = Changed
	case inconclusive:
		r.Verdict = Inconclusive
	case failed:
		r.Verdict = Changed
	default:
		r.Verdict = Same
	}
	r.ExitCode = ExitCode(r.Verdict)
	return r
}

func argsText(a map[string]any) string { return string(cassette.Canonical(a)) }

func structural(bc, cc []ToolCall) []Check {
	conns := map[string]bool{}
	byConn := func(calls []ToolCall) map[string][]string {
		m := map[string][]string{}
		for _, c := range calls {
			conns[c.Conn] = true
			m[c.Conn] = append(m[c.Conn], c.Key)
		}
		return m
	}
	b, c := byConn(bc), byConn(cc)
	var names []string
	for k := range conns {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []Check
	for _, conn := range names {
		ch := Check{Level: "structural", Name: "tool sequence on " + conn, Pass: true,
			Detail: fmt.Sprintf("%d call(s) identical in name, arguments and order", len(b[conn]))}
		if lines := lcsDiff(b[conn], c[conn]); len(lines) > 0 {
			ch.Pass, ch.Detail = false, strings.Join(lines, "; ")
		}
		out = append(out, ch)
	}
	if len(names) == 0 {
		out = append(out, Check{Level: "structural", Name: "tool sequence", Pass: true, Detail: "no tool calls in either run"})
	}
	return out
}

// lcsDiff lists removed (-) and added (+) calls; empty means identical.
func lcsDiff(a, b []string) []string {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			i, j = i+1, j+1
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			out = append(out, "+ "+b[j])
			j++
		default:
			out = append(out, "- "+a[i])
			i++
		}
	}
	return out
}

func required(req Requirement, calls []ToolCall) Check {
	c := Check{Level: "contract", Name: "required " + req.Tool}
	seen := 0
	for _, call := range calls {
		if call.Name != req.Tool {
			continue
		}
		seen++
		if why := argsMeet(call.Args, req.Arguments); why == "" {
			c.Pass, c.Detail = true, "called with "+argsText(call.Args)
			return c
		} else {
			c.Detail = why
		}
	}
	if seen == 0 {
		c.Detail = "never called"
	}
	return c
}

func argsMeet(args map[string]any, cons map[string]any) string {
	names := make([]string, 0, len(cons))
	for k := range cons {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		got, ok := args[name]
		if !ok {
			return "argument " + name + " missing"
		}
		gotS := fmt.Sprint(got)
		if s, isStr := got.(string); isStr {
			gotS = s
		}
		con := cons[name]
		m, isMap := con.(map[string]any)
		if !isMap {
			m = map[string]any{"equals": con}
		}
		for k, v := range m {
			want := fmt.Sprint(v)
			switch k {
			case "equals":
				if gotS != want {
					return fmt.Sprintf("%s = %q, want %q", name, gotS, want)
				}
			case "prefix":
				if !strings.HasPrefix(gotS, want) {
					return fmt.Sprintf("%s = %q, want prefix %q", name, gotS, want)
				}
			case "regex":
				if !regexp.MustCompile(want).MatchString(gotS) {
					return fmt.Sprintf("%s = %q does not match %q", name, gotS, want)
				}
			}
		}
	}
	return ""
}

func output(o Output, base, cand string) (Check, bool) {
	c := Check{Level: "output", Name: "final answer (" + o.Mode + ")", Pass: true}
	switch o.Mode {
	case "", "ignore":
		return c, false
	case "exact":
		want := strings.TrimSpace(base)
		if len(o.Expected) > 0 {
			want = o.Expected[0]
		}
		if strings.TrimSpace(cand) != want {
			c.Pass, c.Detail = false, fmt.Sprintf("got %q, want %q", cand, want)
		}
	case "contains":
		for _, e := range o.Expected {
			if !strings.Contains(cand, e) {
				c.Pass, c.Detail = false, fmt.Sprintf("missing %q in %q", e, cand)
				break
			}
		}
	case "regex":
		for _, e := range o.Expected {
			if !regexp.MustCompile(e).MatchString(cand) {
				c.Pass, c.Detail = false, fmt.Sprintf("%q does not match %q", cand, e)
				break
			}
		}
	}
	return c, true
}

// Render is the human summary.
func Render(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (exit %d) — %s\n", r.Verdict, r.ExitCode, r.Mode)
	if r.Reason != "" {
		fmt.Fprintf(&b, "  reason: %s\n", r.Reason)
	}
	if r.Scope != "" {
		fmt.Fprintf(&b, "  scope: %s\n", r.Scope)
	}
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %-10s %s: %s\n", mark, c.Level, c.Name, c.Detail)
	}
	for _, is := range r.Issues {
		fmt.Fprintf(&b, "  issue (%s, %s) %s %s: %s\n", is.Source, is.Class, is.Conn, is.Kind, is.Detail)
	}
	if len(r.Metrics) > 0 {
		m, _ := json.Marshal(r.Metrics)
		fmt.Fprintf(&b, "  metrics: %s\n", m)
	}
	return b.String()
}
