# Security

## What a cassette can contain

A cassette is a full trace of an agent run: prompts, model answers, tool arguments and
tool results. That can include personal data, file contents and business data. Treat
cassettes like logs from production.

What agentreplay does about it:

- HTTP headers are never written, so `Authorization`, `x-api-key` and similar never
  reach a cassette.
- Before anything is written, every string is scanned for common credential formats
  (`sk-…`, GitHub and Slack tokens, AWS key ids, bearer tokens, PEM private key headers),
  and each match is replaced with `hmac:<fingerprint>`.
- `--redact rules.yaml` adds your own regexes (`patterns`) and JSON paths (`paths`,
  `*` matches any key or index). A path rule replaces the whole value.
- A fingerprint is HMAC-SHA256 with the key in `AGENTREPLAY_HMAC_KEY`. Two requests
  that differ only in a secret still hash differently, but the secret is not stored.
  The cassette holds a key check value, never the key. Replaying with a missing or
  different key stops before anything runs (COULD_NOT_RUN); it never falls back to
  unkeyed hashing.
- Cassettes are written with mode 0600 and `*.jsonl` is in `.gitignore`.
- The benchmark plants an API key and a secret inside tool output, then searches every
  cassette it wrote for them. CI fails if either is found.

What it does **not** do:

- Pattern matching cannot find every secret. A password with no recognisable format,
  in a field you did not list, is stored as is. Add `paths` for fields you know hold
  secrets, and read a cassette before sharing it.
- An agent replaying a cassette receives fingerprints where secrets were. That is
  intended; replay is not meant to hand secrets back.
- During a run the HMAC key is written to a temporary file (mode 0600) so MCP server
  processes started by the agent can read it. The file is deleted when the run ends.

## Reporting a problem

Open a private security advisory on the GitHub repository.
