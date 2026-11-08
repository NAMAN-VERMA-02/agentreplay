// Package mcpproxy records and replays one MCP stdio connection.
package mcpproxy

import "github.com/NAMAN-VERMA-02/agentreplay/internal/cassette"

// Era is the MCP protocol generation spoken on a connection.
type Era string

// Eras. Legacy is everything up to 2025-11-25 (initialize handshake); Modern is
// 2026-07-28 (no handshake, server/discover, required per-request _meta).
const (
	Legacy  Era = "legacy"
	Modern  Era = "modern"
	Unknown Era = "unknown"
)

// ModernVersion is the first stateless protocol revision.
const ModernVersion = "2026-07-28"

const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaClient  = "io.modelcontextprotocol/clientInfo"
)

// DetectEra decides the era from the first client message on a connection.
func DetectEra(msg map[string]any) Era {
	switch msg["method"] {
	case "initialize":
		return Legacy
	case "server/discover":
		return Modern
	}
	if v, _ := metaOf(msg)[metaVersion].(string); v >= ModernVersion {
		return Modern
	}
	return Unknown
}

// The v1 surface. Anything outside these tables is recorded but cannot be
// replayed: the client gets -32601 and the run is INCONCLUSIVE.
var (
	requests = map[Era]map[string]bool{
		Legacy: {"initialize": true, "ping": true, "tools/list": true, "tools/call": true},
		Modern: {"server/discover": true, "tools/list": true, "tools/call": true},
	}
	clientNotifications = map[Era]map[string]bool{
		Legacy: {"notifications/initialized": true},
	}
	serverNotifications = map[Era]map[string]bool{
		Legacy: {"notifications/tools/list_changed": true},
	}
)

func metaOf(msg map[string]any) map[string]any {
	params, _ := msg["params"].(map[string]any)
	meta, _ := params["_meta"].(map[string]any)
	return meta
}

// Normalize drops fields that change between runs without changing meaning: the
// JSON-RPC id, client identity, and progress tokens. Protocol version and client
// capabilities stay, because they change what the server does.
func Normalize(v any) any {
	msg, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(msg))
	for k, val := range msg {
		if k != "id" {
			out[k] = val
		}
	}
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return out
	}
	np := make(map[string]any, len(params))
	for k, val := range params {
		np[k] = val
	}
	if msg["method"] == "initialize" {
		delete(np, "clientInfo") // legacy handshake identity
	}
	if meta, ok := params["_meta"].(map[string]any); ok {
		nm := make(map[string]any, len(meta))
		for k, val := range meta {
			if k != metaClient && k != "progressToken" {
				nm[k] = val
			}
		}
		np["_meta"] = nm
	}
	out["params"] = np
	return out
}

// toolKey identifies a request in test mode: method, and for tools/call the tool
// name and canonical arguments. Per-request _meta is deliberately not part of it.
func toolKey(redacted any) string {
	msg, _ := redacted.(map[string]any)
	method, _ := msg["method"].(string)
	if method != "tools/call" {
		return method
	}
	params, _ := msg["params"].(map[string]any)
	name, _ := params["name"].(string)
	return method + "|" + name + "|" + string(cassette.Canonical(params["arguments"]))
}
