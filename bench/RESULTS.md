# agentreplay mutation benchmark

Demo agent + scripted model + fake MCP server, both MCP eras (legacy 2025-11-25, modern 2026-07-28). This is an initial demonstration on one agent and one task, not a general benchmark.

## Confusion matrix (20 regressions, 10 harmless changes)

| | verdict CHANGED | verdict SAME | other |
|---|---|---|---|
| planted regression | 20 (caught) | 0 (missed) | 0 |
| harmless change | 0 (false alarm) | 10 | |

- Regressions caught: 20/20
- False alarms: 0/10
- Missed regressions: 0
- Replay wall time over 40 runs: p50 150 ms, p95 219 ms, including start-up of the agent, proxy and shim processes
- Each replay serves 4 LLM calls (2250 input / 51 output tokens) from the cassette. At an illustrative $3 / $15 per million tokens that is $0.0075 not spent per run; the fake model's token counts are approximate.

## Every case

| group | era | case | expected | got | |
|---|---|---|---|---|---|
| replay | legacy | baseline replay offline | SAME | SAME | ok |
| replay | legacy | model requests during 20 replays | 0 | 0 | ok |
| regression | legacy | required tool removed | CHANGED | CHANGED | ok |
| regression | legacy | wrong tool selected | CHANGED | CHANGED | ok |
| regression | legacy | wrong file path | CHANGED | CHANGED | ok |
| regression | legacy | wrong user ID | CHANGED | CHANGED | ok |
| regression | legacy | tool arguments swapped | CHANGED | CHANGED | ok |
| regression | legacy | required step skipped | CHANGED | CHANGED | ok |
| regression | legacy | forbidden tool invoked | CHANGED | CHANGED | ok |
| regression | legacy | error treated as success | CHANGED | CHANGED | ok |
| regression | legacy | tool result ignored | CHANGED | CHANGED | ok |
| regression | legacy | invalid final output contract | CHANGED | CHANGED | ok |
| harmless | legacy | JSON key order | SAME | SAME | ok |
| harmless | legacy | request IDs | SAME | SAME | ok |
| harmless | legacy | timestamps | SAME | SAME | ok |
| harmless | legacy | non-semantic metadata | SAME | SAME | ok |
| harmless | legacy | reworded final answer | SAME | SAME | ok |
| guard | legacy | streaming request | INCONCLUSIVE | INCONCLUSIVE | ok |
| guard | legacy | replaying a changed agent | INCONCLUSIVE | INCONCLUSIVE | ok |
| replay | modern | baseline replay offline | SAME | SAME | ok |
| replay | modern | model requests during 20 replays | 0 | 0 | ok |
| regression | modern | required tool removed | CHANGED | CHANGED | ok |
| regression | modern | wrong tool selected | CHANGED | CHANGED | ok |
| regression | modern | wrong file path | CHANGED | CHANGED | ok |
| regression | modern | wrong user ID | CHANGED | CHANGED | ok |
| regression | modern | tool arguments swapped | CHANGED | CHANGED | ok |
| regression | modern | required step skipped | CHANGED | CHANGED | ok |
| regression | modern | forbidden tool invoked | CHANGED | CHANGED | ok |
| regression | modern | error treated as success | CHANGED | CHANGED | ok |
| regression | modern | tool result ignored | CHANGED | CHANGED | ok |
| regression | modern | invalid final output contract | CHANGED | CHANGED | ok |
| harmless | modern | JSON key order | SAME | SAME | ok |
| harmless | modern | request IDs | SAME | SAME | ok |
| harmless | modern | timestamps | SAME | SAME | ok |
| harmless | modern | non-semantic metadata | SAME | SAME | ok |
| harmless | modern | reworded final answer | SAME | SAME | ok |
| guard | modern | streaming request | INCONCLUSIVE | INCONCLUSIVE | ok |
| guard | modern | replaying a changed agent | INCONCLUSIVE | INCONCLUSIVE | ok |
| guard | modern | cassette replayed in the wrong era | INCONCLUSIVE | INCONCLUSIVE | ok |
| guard | legacy | baseline overwrite refused | COULD_NOT_RUN | COULD_NOT_RUN | ok |
| guard | legacy | wrong HMAC key | COULD_NOT_RUN | COULD_NOT_RUN | ok |
| openai | legacy | baseline replay offline | SAME | SAME | ok |
| openai | legacy | wrong user ID | CHANGED | CHANGED | ok |
| openai | legacy | reworded final answer | SAME | SAME | ok |
| secrets | both | planted secrets found in 37 written cassettes | 0 | 0 | ok |

## Why each regression was caught

- legacy / required tool removed: structural: tool sequence on mcp-files; contract: required search_files
- legacy / wrong tool selected: structural: tool sequence on mcp-files; contract: required search_files; unexpected_tool_call
- legacy / wrong file path: structural: tool sequence on mcp-files; contract: required search_files; unexpected_tool_call
- legacy / wrong user ID: structural: tool sequence on mcp-files; contract: required read_file; unexpected_tool_call
- legacy / tool arguments swapped: structural: tool sequence on mcp-files; contract: required read_file; unexpected_tool_call
- legacy / required step skipped: structural: tool sequence on mcp-files; contract: required read_file
- legacy / forbidden tool invoked: structural: tool sequence on mcp-files; contract: forbidden delete_file; unexpected_tool_call
- legacy / error treated as success: output: final answer (contains)
- legacy / tool result ignored: output: final answer (contains)
- legacy / invalid final output contract: output: final answer (contains)
- legacy / streaming request: structural: tool sequence on mcp-files; contract: required search_files; contract: required read_file; output: final answer (contains); agent_exit_nonzero; unsupported_streaming
- legacy / replaying a changed agent: structural: tool sequence on mcp-files; agent_exit_nonzero; divergence; unused_exchanges; unused_exchanges
- modern / required tool removed: structural: tool sequence on mcp-files; contract: required search_files
- modern / wrong tool selected: structural: tool sequence on mcp-files; contract: required search_files; unexpected_tool_call
- modern / wrong file path: structural: tool sequence on mcp-files; contract: required search_files; unexpected_tool_call
- modern / wrong user ID: structural: tool sequence on mcp-files; contract: required read_file; unexpected_tool_call
- modern / tool arguments swapped: structural: tool sequence on mcp-files; contract: required read_file; unexpected_tool_call
- modern / required step skipped: structural: tool sequence on mcp-files; contract: required read_file
- modern / forbidden tool invoked: structural: tool sequence on mcp-files; contract: forbidden delete_file; unexpected_tool_call
- modern / error treated as success: output: final answer (contains)
- modern / tool result ignored: output: final answer (contains)
- modern / invalid final output contract: output: final answer (contains)
- modern / streaming request: structural: tool sequence on mcp-files; contract: required search_files; contract: required read_file; output: final answer (contains); agent_exit_nonzero; unsupported_streaming
- modern / replaying a changed agent: structural: tool sequence on mcp-files; agent_exit_nonzero; divergence; unused_exchanges; unused_exchanges
- modern / cassette replayed in the wrong era: structural: tool sequence on mcp-files; agent_exit_nonzero; unused_exchanges; era_mismatch; unused_exchanges
- legacy / baseline overwrite refused: refusing to overwrite C:\Users\naman\AppData\Local\Temp\agentreplay-bench-1341685502\base-legacy.jsonl; pick a new --out
- legacy / wrong HMAC key: AGENTREPLAY_HMAC_KEY does not match the key this cassette was recorded with; fingerprints would not match, so nothing was run
