# agentreplay

Did your agent change behaviour when you changed its prompt, its model or its code?

agentreplay records what a tool-using agent sends to its LLM and its MCP servers,
replays the run offline, and tests a changed agent against the recorded tool results.
It compares what the agent *did* (which tools, with which arguments, in which order,
and what it answered) and gives a verdict with an exit code CI can use.

## Try it in one minute

You need Go. No API key and no real model are involved: a scripted model and a fake
MCP server stand in for both.

```bash
go build -o bin/ ./cmd/agentreplay ./examples/...
export AGENTREPLAY_HMAC_KEY=pick-a-long-random-key
./bin/fakemodel -addr 127.0.0.1:8787 &
echo '{"mcpServers":{"files":{"command":"./bin/fakemcp"}}}' > mcp.json

# 1. record a baseline
./bin/agentreplay record --out baseline.jsonl --mcp-config mcp.json \
  --anthropic-upstream http://127.0.0.1:8787 -- ./bin/demoagent --mcp-config {mcp_config}

# 2. replay it offline: no model, no MCP server        -> SAME
./bin/agentreplay replay --cassette baseline.jsonl --mcp-config mcp.json \
  -- ./bin/demoagent --mcp-config {mcp_config}

# 3. test a changed agent against the recorded tools   -> CHANGED
./bin/agentreplay test --baseline baseline.jsonl --out candidate.jsonl \
  --policy examples/agentreplay.yaml --mcp-config mcp.json \
  --anthropic-upstream http://127.0.0.1:8787 \
  -- ./bin/demoagent --mcp-config {mcp_config} --model fake/skip-step
```

The last command prints:

```
CHANGED (exit 1) — test
  scope: structural, contract and output assertions
  [FAIL] structural tool sequence on mcp-files: - read_file{"path":"/allowed-root/report.pdf","user_id":"u-1"}
  [ok  ] contract   required search_files: called with {"query":"quarterly report","root":"/allowed-root"}
  [FAIL] contract   required read_file: never called
  ...
```

## Three modes

| Mode | LLM | MCP tools | Use it to |
|---|---|---|---|
| `record` | live | live | capture a baseline cassette |
| `replay` | from the cassette | from the cassette | reproduce a run exactly, offline; any difference is INCONCLUSIVE |
| `test` | live (the candidate) | from the cassette | run a changed prompt, model or agent against the recorded tools, then diff |

`test` is the one that finds regressions. The model really runs, so a changed prompt
can change what it does, while the tools answer from the baseline, so nothing real is
touched. A tool call the baseline never made gets an error back and counts as CHANGED.

`record` and `test` refuse to overwrite an existing `--out`, so a baseline cannot be
replaced by accident.

The agent finds the proxies through `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` and
`AGENTREPLAY_MCP_CONFIG`. In the agent command, `{mcp_config}` is replaced with the
path of a generated MCP config that starts each server through agentreplay.

## Verdicts

| Verdict | Meaning | Exit code |
|---|---|---|
| `SAME` | No regression detected **by the configured assertions** | 0 |
| `CHANGED` | A demonstrated difference: different tool calls, a broken contract or a wrong answer | 1 |
| `INCONCLUSIVE` | The run can't show either: a missing or unused cassette event, an unsupported feature, the wrong MCP era, a flawed baseline | 2 |
| `COULD_NOT_RUN` | Setup failed before a valid test started, for example a wrong HMAC key | 3 |

`SAME` does not mean two runs are equivalent. With no `required` rule and no output
check, the report says `structural checks only — semantic correctness not tested`.

A forbidden tool call or a tool call the baseline never made is CHANGED even if the
run was also compromised, because it is a fact about what the agent did. Every other
difference in a compromised run is INCONCLUSIVE, so a broken setup never reads as a
regression or as a pass.

## Policy

```yaml
required:                      # at least one call per entry must meet every constraint
  - tool: search_files
    arguments:
      root: { prefix: /allowed-root }
  - tool: read_file
    arguments:
      user_id: u-1             # a plain value means equals
      path: { regex: "^/allowed-root/.+" }
forbidden: [delete_file, execute_shell]
output:                        # the agent's final answer
  mode: contains               # ignore | exact | contains | regex
  expected: ["FILE: /allowed-root/report.pdf", "BACKUP: missing"]
```

Checks run at three levels: **structural** (tool sequence, arguments and order against
the baseline), **contract** (required and forbidden tools) and **output**. The default
output mode is `ignore`, because a reworded answer is usually not a regression; put
the meaning you care about in `contains` or `regex`.

## What v1 supports

| Integration | v1 |
|---|---|
| Anthropic Messages, non-streaming | yes |
| OpenAI Chat Completions, non-streaming | yes |
| MCP over stdio, legacy era (≤ 2025-11-25): `initialize`, `notifications/initialized`, `ping`, paginated `tools/list`, `tools/call`, `notifications/tools/list_changed` | yes |
| MCP over stdio, modern era (2026-07-28): `server/discover`, per-request `_meta` protocol version and client capabilities, paginated `tools/list`, `tools/call` | yes |
| Tool results with images, audio or embedded resources | stored and replayed as raw JSON-RPC, never flattened to text |
| Streaming responses | refused with HTTP 501 → INCONCLUSIVE |
| `notifications/cancelled`, progress, `subscriptions/listen` | recorded; INCONCLUSIVE on replay |
| Resources, prompts, sampling, server-to-client requests | the client gets -32601 → INCONCLUSIVE, never dropped silently |
| OpenAI Responses API, other providers, MCP over HTTP | not yet |
| Testing a changed MCP server (`test --mcp-live`) | not yet |
| Any agent framework | only through the integrations above |

**Not tested yet:** real providers, and real agents such as Claude Code. Everything
measured so far uses the demo agent, the scripted model and the fake MCP server.

## How matching works

Each request goes through one pipeline, the same when recording and when matching:
parse, drop volatile fields, redact, canonical JSON (sorted keys), SHA-256.

- **Volatile fields** are the JSON-RPC id, the client identity (`clientInfo`) and
  progress tokens. Everything else stays in the hash: model, messages, tools,
  generation parameters, user ids, metadata, and in the modern era the per-request
  protocol version and client capabilities. Add `ignore` paths in `--redact` for
  anything else, such as timestamps.
- **LLM calls** replay in order on each provider lane. A call that doesn't match the
  next recorded one is answered with HTTP 409 and reported as a divergence.
- **MCP requests** match per connection. Each request gets the earliest unused
  recorded exchange with the same hash, so the Nth identical call gets the Nth
  recorded answer. Order is the order requests reach the proxy. The recorded response
  is sent back with the *current* request's id, so responses can arrive in any order.
- If identical requests overlapped while recording **and** got different answers,
  replay can't tell which caller got which. The run is INCONCLUSIVE rather than
  claimed deterministic.
- In `test` mode tools match on tool name and canonical arguments. Per-request
  `_meta` and ids don't count.
- The MCP era is detected from each connection's first message. Replaying a cassette
  in the other era is INCONCLUSIVE.

## Secrets

Headers are never written. Common credential formats and any JSON paths you list are
replaced with HMAC fingerprints keyed by `AGENTREPLAY_HMAC_KEY`. Replay with the wrong
key stops before running. Cassettes can still hold personal data and file contents:
see [SECURITY.md](SECURITY.md).

## Benchmark

`go run ./bench` records the demo agent and runs every case below in both MCP eras.
CI runs it on every push and fails on any wrong verdict, any network call during
replay, or any planted secret found in a cassette.

- 10 planted regressions, expected CHANGED: required tool removed, wrong tool, wrong
  path, wrong user ID, swapped arguments, skipped step, forbidden tool, error treated
  as success, tool result ignored, broken output format.
- 5 harmless changes, expected SAME: JSON key order, request ids, timestamps,
  client metadata, reworded answer.
- Guards: streaming, replaying a changed agent, wrong era, overwriting a baseline,
  wrong key.

Latest run: [bench/RESULTS.md](bench/RESULTS.md).

Read the numbers for what they are. The regressions were written by the same person
who wrote the checks, on one agent and one task. They show the suite can tell these
cases apart; they do not estimate how many real regressions it would catch.

## Layout

```
cmd/agentreplay      CLI: record, replay, test, diff, and the mcp-shim the agent starts
internal/cassette    event format, the hashing pipeline, ordered matching
internal/redact      HMAC redaction
internal/llmproxy    Anthropic and OpenAI adapters
internal/mcpproxy    one MCP stdio connection: era detection, id remapping
internal/diff        structural, contract and output checks; verdicts
internal/runner      starts the proxies and the agent for one run
internal/fake        the scripted model and the fake MCP server
examples/            demo agent, fake model and fake MCP binaries, example policy
bench/               the mutation benchmark
```

## License

Apache 2.0.
