// Package mcpserver is a small file-search MCP server used as the test oracle. It
// speaks both the legacy (initialize) and modern (2026-07-28, server/discover) eras.
package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Secret is planted in read_file output so tests can prove redaction works.
const Secret = "sk-test-PLANTEDSECRET0000000000"

const pageSize = 3

var tools = []map[string]any{
	tool("search_files", "Search for files under a root directory.", "root", "query"),
	tool("read_file", "Read a file as a given user.", "user_id", "path"),
	tool("stat_backup", "Check whether a file has a backup.", "path"),
	tool("list_dir", "List a directory.", "path"),
	tool("delete_file", "Delete a file.", "path"),
	tool("next_ticket", "Return the next ticket number."),
	tool("slow_echo", "Echo text after delay_ms milliseconds.", "text", "delay_ms"),
	tool("slow_ticket", "Return the next ticket number after delay_ms milliseconds.", "delay_ms"),
	tool("get_chart", "Return a chart image."),
}

func tool(name, desc string, args ...string) map[string]any {
	props := map[string]any{}
	for _, a := range args {
		props[a] = map[string]any{"type": "string"}
	}
	return map[string]any{"name": name, "description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": props, "required": args}}
}

type server struct {
	outMu  sync.Mutex
	out    io.Writer
	legacy atomic.Bool
	ticket atomic.Int64
	wg     sync.WaitGroup
}

// Serve answers newline-delimited JSON-RPC on in/out until in closes.
func Serve(in io.Reader, out io.Writer) error {
	s := &server{out: out}
	br := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handle(line)
		}
		if err != nil {
			s.wg.Wait()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *server) send(v any) {
	b, _ := json.Marshal(v)
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *server) result(id any, r any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": r})
}
func (s *server) fail(id any, code int, msg string) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *server) handle(line []byte) {
	var msg struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if json.Unmarshal(line, &msg) != nil {
		s.fail(nil, -32700, "parse error")
		return
	}
	if msg.ID == nil { // notifications need no answer
		return
	}
	if msg.Method == "initialize" {
		s.legacy.Store(true)
		s.result(msg.ID, map[string]any{"protocolVersion": "2025-11-25",
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":   map[string]any{"name": "fakemcp", "version": "1.0.0"}})
		return
	}
	if !s.legacy.Load() { // modern: every request carries its version and capabilities
		meta, _ := msg.Params["_meta"].(map[string]any)
		if meta["io.modelcontextprotocol/protocolVersion"] == nil || meta["io.modelcontextprotocol/clientCapabilities"] == nil {
			s.fail(msg.ID, -32602, "missing required _meta protocolVersion or clientCapabilities")
			return
		}
	}
	switch msg.Method {
	case "server/discover":
		s.result(msg.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"},
			"capabilities": map[string]any{"tools": map[string]any{}},
			"_meta":        map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "fakemcp", "version": "1.0.0"}}})
	case "ping":
		if !s.legacy.Load() {
			s.fail(msg.ID, -32601, "ping was removed in 2026-07-28")
			return
		}
		s.result(msg.ID, map[string]any{})
	case "tools/list":
		start := 0
		if c, _ := msg.Params["cursor"].(string); c != "" {
			fmt.Sscanf(c, "page-%d", &start)
		}
		end := min(start+pageSize, len(tools))
		r := map[string]any{"tools": tools[start:end]}
		if end < len(tools) {
			r["nextCursor"] = fmt.Sprintf("page-%d", end)
		}
		s.result(msg.ID, r)
	case "tools/call":
		name, _ := msg.Params["name"].(string)
		args, _ := msg.Params["arguments"].(map[string]any)
		if name == "slow_echo" || name == "slow_ticket" { // answered out of order on purpose
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				var ms int
				fmt.Sscanf(str(args, "delay_ms"), "%d", &ms)
				t := str(args, "text")
				if name == "slow_ticket" {
					t = fmt.Sprintf("ticket-%d", s.ticket.Add(1))
				}
				time.Sleep(time.Duration(ms) * time.Millisecond)
				s.result(msg.ID, text(t, false))
			}()
			return
		}
		s.call(msg.ID, name, args)
	default:
		s.fail(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func str(args map[string]any, k string) string {
	v, _ := args[k].(string)
	return v
}

func text(t string, isErr bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": t}}, "isError": isErr}
}

func (s *server) call(id any, name string, args map[string]any) {
	switch name {
	case "search_files":
		if str(args, "root") == "/allowed-root" && strings.Contains(str(args, "query"), "report") {
			s.result(id, text("/allowed-root/report.pdf", false))
			return
		}
		s.result(id, text("no matches", false))
	case "read_file":
		if str(args, "user_id") == "u-1" && str(args, "path") == "/allowed-root/report.pdf" {
			s.result(id, text("Q3 revenue: 4.2M. deploy token "+Secret, false))
			return
		}
		s.result(id, text("ERROR: access denied", true))
	case "stat_backup":
		s.result(id, text("ERROR: no backup found for "+str(args, "path"), true))
	case "list_dir":
		s.result(id, text("report.pdf\nnotes.txt", false))
	case "delete_file":
		s.result(id, text("deleted "+str(args, "path"), false))
		if s.legacy.Load() {
			s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		}
	case "next_ticket":
		s.result(id, text(fmt.Sprintf("ticket-%d", s.ticket.Add(1)), false))
	case "get_chart":
		s.result(id, map[string]any{"content": []any{
			map[string]any{"type": "image", "data": "iVBORw0KGgo=", "mimeType": "image/png"},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///chart.csv", "mimeType": "text/csv", "text": "q,rev\n3,4.2"}},
		}})
	default:
		s.result(id, text("ERROR: unknown tool "+name, true))
	}
}
