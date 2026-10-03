// Package mcp is a minimal Model Context Protocol client for stdio servers.
// Each server's tools are exposed to the model as mcp__<server>__<tool>.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/eodomo/nextpage/internal/config"
	"github.com/eodomo/nextpage/internal/tools"
)

const protocolVersion = "2025-06-18"

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type Client struct {
	Name string
	cmd  *exec.Cmd
	in   io.WriteCloser

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcMessage
	closed  chan struct{}
}

type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

func Start(ctx context.Context, name string, cfg config.MCPServer) (*Client, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = logWriter{name}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	c := &Client{Name: name, cmd: cmd, in: stdin, pending: map[int64]chan rpcMessage{}, closed: make(chan struct{})}
	go c.readLoop(stdout)

	var initResult json.RawMessage
	err = c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "nextpage", "version": "0.1.0"},
	}, &initResult)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize %s: %w", name, err)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type logWriter struct{ name string }

func (w logWriter) Write(p []byte) (int, error) {
	log.Printf("mcp[%s] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.closed)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			log.Printf("mcp[%s] bad message: %v", c.Name, err)
			continue
		}
		if msg.ID == nil || msg.Method != "" {
			continue // notifications and server→client requests are ignored
		}
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
}

func (c *Client) send(msg rpcMessage) error {
	msg.JSONRPC = "2.0"
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.in.Write(append(b, '\n'))
	return err
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcMessage{Method: method, Params: params})
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(rpcMessage{ID: &id, Method: method, Params: params}); err != nil {
		return err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("%s: %s (code %d)", method, msg.Error.Message, msg.Error.Code)
		}
		return json.Unmarshal(msg.Result, result)
	case <-c.closed:
		return fmt.Errorf("server %s exited", c.Name)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		_ = c.notify("notifications/cancelled", map[string]any{"requestId": id})
		return ctx.Err()
	}
}

func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", true, err
	}
	var parts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[%s content (%s) omitted]", c.Type, c.MimeType))
		}
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}

func (c *Client) Close() error {
	c.in.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}

// Tool adapts an MCP tool to tools.Tool.
type Tool struct {
	client *Client
	info   ToolInfo
}

func (t *Tool) Name() string { return "mcp__" + t.client.Name + "__" + t.info.Name }
func (t *Tool) Description() string {
	return t.info.Description
}
func (t *Tool) Schema() json.RawMessage {
	if len(t.info.InputSchema) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.info.InputSchema
}
func (t *Tool) Kind() tools.Kind {
	if t.info.Annotations.ReadOnlyHint {
		return tools.KindRead
	}
	return tools.KindExecute
}
func (t *Tool) Run(ctx context.Context, env *tools.Env, in json.RawMessage) tools.Result {
	out, isErr, err := t.client.CallTool(ctx, t.info.Name, in)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Result{Output: out, IsError: isErr}
}

// Manager owns the running MCP servers.
type Manager struct {
	Clients []*Client
	Errors  map[string]error
	Tools   map[string][]string
}

// StartAll launches every configured server and registers its tools.
// Servers that fail to start are recorded in Errors rather than aborting.
func StartAll(ctx context.Context, servers map[string]config.MCPServer, reg *tools.Registry) *Manager {
	m := &Manager{Errors: map[string]error{}, Tools: map[string][]string{}}
	for name, cfg := range servers {
		c, err := Start(ctx, name, cfg)
		if err != nil {
			m.Errors[name] = err
			log.Printf("mcp: %v", err)
			continue
		}
		infos, err := c.ListTools(ctx)
		if err != nil {
			m.Errors[name] = err
			c.Close()
			continue
		}
		m.Clients = append(m.Clients, c)
		for _, info := range infos {
			t := &Tool{client: c, info: info}
			reg.Add(t)
			m.Tools[name] = append(m.Tools[name], t.Name())
		}
	}
	return m
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, c := range m.Clients {
		c.Close()
	}
}
