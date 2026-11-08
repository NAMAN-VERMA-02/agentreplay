// demoagent is a small but real tool-using agent: it starts an MCP server from a
// config file, lists its tools, and loops between an LLM (Anthropic Messages or
// OpenAI Chat Completions) and tools/call until the model answers.
//
// Its flags plant the harmless changes the benchmark expects agentreplay to ignore.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var (
	provider      = flag.String("provider", "anthropic", "anthropic or openai")
	model         = flag.String("model", "fake/baseline", "model name")
	mcpConfig     = flag.String("mcp-config", os.Getenv("AGENTREPLAY_MCP_CONFIG"), "MCP config file ({\"mcpServers\": ...})")
	era           = flag.String("era", "legacy", "MCP era: legacy (initialize) or modern (2026-07-28)")
	prompt        = flag.String("prompt", "Find the quarterly report for user u-1 and check whether it has a backup.", "task")
	reverseKeys   = flag.Bool("reverse-arg-keys", false, "harmless: write tool arguments in reverse key order")
	idStyle       = flag.String("rpc-ids", "int", "harmless: JSON-RPC id style, int or string")
	metaTimestamp = flag.Bool("meta-timestamps", false, "harmless: add a timestamp to each tools/call _meta")
	clientVersion = flag.String("client-version", "1.0.0", "harmless: client version sent to the server")
	stream        = flag.Bool("stream", false, "request a streaming response (unsupported by agentreplay v1)")
)

func main() {
	flag.Parse()
	answer, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demoagent:", err)
		os.Exit(1)
	}
	fmt.Println(answer)
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func run() (string, error) {
	c, err := startMCP()
	if err != nil {
		return "", err
	}
	defer c.close()
	if err := c.open(); err != nil {
		return "", err
	}
	var tools []tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.request("tools/list", params)
		if err != nil {
			return "", err
		}
		var page struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		json.Unmarshal(res, &page)
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if *provider == "openai" {
		return loopOpenAI(c, tools)
	}
	return loopAnthropic(c, tools)
}

// ---- MCP client ----

type mcpClient struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	next int
}

func startMCP() (*mcpClient, error) {
	if *mcpConfig == "" {
		return nil, errors.New("no MCP config (use --mcp-config)")
	}
	raw, err := os.ReadFile(*mcpConfig)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.MCPServers) == 0 {
		return nil, fmt.Errorf("bad MCP config %s", *mcpConfig)
	}
	var names []string
	for n := range cfg.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	s := cfg.MCPServers[names[0]]
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &mcpClient{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}, nil
}

func (c *mcpClient) close() {
	c.in.Close()
	done := make(chan struct{})
	go func() { c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		c.cmd.Process.Kill()
	}
}

func (c *mcpClient) meta(params map[string]any, call bool) {
	m := map[string]any{}
	if *era == "modern" {
		m["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"
		m["io.modelcontextprotocol/clientCapabilities"] = map[string]any{}
		m["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "demoagent", "version": *clientVersion}
	}
	if call && *metaTimestamp {
		m["io.agentreplay.demo/sentAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if len(m) > 0 {
		params["_meta"] = m
	}
}

func (c *mcpClient) open() error {
	if *era == "modern" {
		_, err := c.request("server/discover", map[string]any{})
		return err
	}
	_, err := c.request("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "demoagent", "version": *clientVersion}})
	if err != nil {
		return err
	}
	return c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (c *mcpClient) write(v any) error {
	b, _ := json.Marshal(v)
	_, err := c.in.Write(append(b, '\n'))
	return err
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

func (c *mcpClient) request(method string, params map[string]any) (json.RawMessage, error) {
	c.next++
	var id any = c.next
	if *idStyle == "string" {
		id = fmt.Sprintf("req-%d", c.next)
	}
	c.meta(params, method == "tools/call")
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	want, _ := json.Marshal(id)
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("MCP server closed: %w", err)
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Method != "" || !bytes.Equal(msg.ID, want) {
			continue // notifications and unrelated lines
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

// callTool returns the result text and whether it is an error.
func (c *mcpClient) callTool(name string, args json.RawMessage) (string, bool) {
	if *reverseKeys {
		args = reversed(args)
	}
	res, err := c.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "ERROR: " + err.Error(), true
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	json.Unmarshal(res, &r)
	var parts []string
	for _, ct := range r.Content {
		if ct.Type == "text" {
			parts = append(parts, ct.Text)
		}
	}
	return strings.Join(parts, "\n"), r.IsError
}

// reversed rewrites a JSON object with its keys in reverse order.
func reversed(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(m[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// ---- LLM loops ----

func post(url string, headers map[string]string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LLM returned %d: %s", resp.StatusCode, rb)
	}
	return json.Unmarshal(rb, out)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const system = "You are a careful file assistant. Answer as:\nFILE: <path>\nBACKUP: <ok|missing>"

func loopAnthropic(c *mcpClient, tools []tool) (string, error) {
	url := strings.TrimSuffix(env("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/") + "/v1/messages"
	headers := map[string]string{"x-api-key": os.Getenv("ANTHROPIC_API_KEY"), "anthropic-version": "2023-06-01"}
	var defs []any
	for _, t := range tools {
		defs = append(defs, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.InputSchema})
	}
	messages := []any{map[string]any{"role": "user", "content": *prompt}}
	for range 12 {
		body := map[string]any{"model": *model, "max_tokens": 1024, "system": system, "messages": messages, "tools": defs}
		if *stream {
			body["stream"] = true
		}
		var resp struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := post(url, headers, body, &resp); err != nil {
			return "", err
		}
		var texts []string
		var said, blocks []any
		for _, b := range resp.Content {
			switch b.Type {
			case "tool_use":
				said = append(said, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Input})
				out, isErr := c.callTool(b.Name, b.Input)
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": b.ID, "content": out, "is_error": isErr})
			case "text":
				said = append(said, map[string]any{"type": "text", "text": b.Text})
				texts = append(texts, b.Text)
			}
		}
		if len(blocks) == 0 {
			return strings.Join(texts, "\n"), nil
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": said},
			map[string]any{"role": "user", "content": blocks})
	}
	return "", errors.New("no answer after 12 turns")
}

func loopOpenAI(c *mcpClient, tools []tool) (string, error) {
	url := strings.TrimSuffix(env("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/") + "/chat/completions"
	headers := map[string]string{"Authorization": "Bearer " + os.Getenv("OPENAI_API_KEY")}
	var defs []any
	for _, t := range tools {
		defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
	}
	messages := []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": *prompt}}
	for range 12 {
		body := map[string]any{"model": *model, "messages": messages, "tools": defs}
		if *stream {
			body["stream"] = true
		}
		var resp struct {
			Choices []struct {
				Message struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := post(url, headers, body, &resp); err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("LLM returned no choices")
		}
		msg := resp.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			if msg.Content == nil {
				return "", nil
			}
			return *msg.Content, nil
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": msg.Content, "tool_calls": msg.ToolCalls})
		for _, tc := range msg.ToolCalls {
			out, _ := c.callTool(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": tc.ID, "content": out})
		}
	}
	return "", errors.New("no answer after 12 turns")
}
