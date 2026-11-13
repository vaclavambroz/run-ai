// Package appserver implements the small JSON-RPC transport used by Codex's
// app-server. It deliberately does not answer server approval requests: the
// caller must make that decision explicitly.
package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}
type Event struct {
	Method string
	Params json.RawMessage
}
type Request struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type Client struct {
	in       io.WriteCloser
	out      io.ReadCloser
	encMu    sync.Mutex
	mu       sync.Mutex
	next     uint64
	pending  map[string]chan Message
	Events   chan Event
	Requests chan Request
	done     chan struct{}
}

func New(in io.WriteCloser, out io.ReadCloser) *Client {
	// These channels carry state-changing provider notifications.  They must
	// apply backpressure instead of silently dropping a completion or approval.
	c := &Client{in: in, out: out, pending: map[string]chan Message{}, Events: make(chan Event, 256), Requests: make(chan Request, 64), done: make(chan struct{})}
	go c.readLoop()
	return c
}

// Command starts a local app-server over stdio.
func Command(ctx context.Context, bin string, args ...string) (*Client, *exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, nil, e
	}
	return New(in, out), cmd, nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	s := bufio.NewScanner(c.out)
	s.Buffer(make([]byte, 4096), 8<<20)
	for s.Scan() {
		var m Message
		if json.Unmarshal(s.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil {
			key := string(m.ID)
			c.mu.Lock()
			ch := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		if m.Method == "" {
			continue
		}
		if len(m.Params) > 0 {
			// JSON-RPC server requests have an id and require an explicit
			// client reply; notifications (including turn/item events) do not.
			if m.ID != nil {
				c.Requests <- Request{m.ID, m.Method, m.Params}
			}
			c.Events <- Event{m.Method, m.Params}
		}
	}
	close(c.Events)
	close(c.Requests)
}

func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	c.next++
	id := json.RawMessage(fmt.Sprintf("%d", c.next))
	ch := make(chan Message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	b, e := json.Marshal(Message{JSONRPC: "2.0", ID: id, Method: method, Params: mustJSON(params)})
	if e != nil {
		return e
	}
	c.encMu.Lock()
	e = writeLine(c.in, b)
	c.encMu.Unlock()
	if e != nil {
		return e
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return io.EOF
	case m := <-ch:
		if len(m.Error) > 0 {
			return errors.New(string(m.Error))
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	}
}

func (c *Client) Notify(method string, params any) error {
	b, e := json.Marshal(Message{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
	if e != nil {
		return e
	}
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return writeLine(c.in, b)
}
func (c *Client) Reply(id json.RawMessage, result any) error {
	b, e := json.Marshal(Message{JSONRPC: "2.0", ID: id, Result: mustJSON(result)})
	if e != nil {
		return e
	}
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return writeLine(c.in, b)
}
func (c *Client) Close() error {
	err := c.in.Close()
	// Closing the read side is required for stdio transports whose server
	// keeps stdout open while waiting for an approval.  It also makes
	// disconnect deterministic for reconnect/restart handling.
	if e := c.out.Close(); err == nil {
		err = e
	}
	return err
}
func writeLine(w io.Writer, b []byte) error {
	if _, e := w.Write(append(b, '\n')); e != nil {
		return e
	}
	return nil
}
func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

type InitializeParams struct {
	ClientInfo   ClientInfo              `json:"clientInfo"`
	Capabilities *InitializeCapabilities `json:"capabilities,omitempty"`
}
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type InitializeCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi,omitempty"`
}
type ThreadStartParams struct {
	Model          string `json:"model,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	ApprovalPolicy any    `json:"approvalPolicy,omitempty"`
	Sandbox        any    `json:"sandbox,omitempty"`
}
type ThreadRef struct {
	ThreadID string `json:"threadId"`
}

// TurnInterruptParams is deliberately distinct from ThreadRef. Codex's
// app-server requires the exact active turn as well as its thread; a thread
// identifier alone is not a valid or safe interrupt target.
type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}
type TurnStartParams struct {
	ThreadID string  `json:"threadId"`
	Input    []Input `json:"input"`
}
type TurnSteerParams struct {
	ThreadID string  `json:"threadId"`
	Input    []Input `json:"input"`
}
type Input struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (c *Client) Initialize(ctx context.Context, p InitializeParams) error {
	var out any
	if e := c.Call(ctx, "initialize", p, &out); e != nil {
		return e
	}
	return c.Notify("initialized", map[string]any{})
}
func (c *Client) StartThread(ctx context.Context, p ThreadStartParams) (string, error) {
	var r struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	e := c.Call(ctx, "thread/start", p, &r)
	if r.ThreadID == "" {
		r.ThreadID = r.Thread.ID
	}
	return r.ThreadID, e
}
func (c *Client) ResumeThread(ctx context.Context, id string) (string, error) {
	var r struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	e := c.Call(ctx, "thread/resume", ThreadRef{ThreadID: id}, &r)
	if r.ThreadID == "" {
		r.ThreadID = r.Thread.ID
	}
	return r.ThreadID, e
}
func (c *Client) StartTurn(ctx context.Context, p TurnStartParams) (json.RawMessage, error) {
	var r json.RawMessage
	e := c.Call(ctx, "turn/start", p, &r)
	return r, e
}
func (c *Client) SteerTurn(ctx context.Context, p TurnSteerParams) (json.RawMessage, error) {
	var r json.RawMessage
	e := c.Call(ctx, "turn/steer", p, &r)
	return r, e
}
func (c *Client) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	if threadID == "" || turnID == "" {
		return errors.New("active thread ID and turn ID are required for interrupt")
	}
	return c.Call(ctx, "turn/interrupt", TurnInterruptParams{ThreadID: threadID, TurnID: turnID}, nil)
}

// TurnID extracts the provider turn ID from the documented turn/start and
// turn/steer reply shapes.  Keeping this parsing at the protocol boundary
// prevents callers from inventing IDs or interrupting a later turn.
func TurnID(reply json.RawMessage) string {
	var value struct {
		TurnID string `json:"turnId"`
		Turn   struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(reply, &value) != nil {
		return ""
	}
	if value.TurnID != "" {
		return value.TurnID
	}
	return value.Turn.ID
}
