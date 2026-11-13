package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Terminal is the deliberately small, reconnectable text client for app-server.
// It keeps all provider decisions in the server and never approves requests on
// its own. A prompt is submitted only when the previous turn is complete.
type Terminal struct {
	Client       *Client
	ThreadID     string
	In           io.Reader
	Out          io.Writer
	Err          io.Writer
	Turns        int
	Active       string
	ActiveTurnID string
	// OnPrompt durably records user input. OnTurnCompleted returns an
	// autonomous prompt only when the supervisor explicitly classified the
	// turn as a checkpoint; an empty prompt means stop.
	OnPrompt        func(string) error
	OnTurnCompleted func(string) (string, error)
}

// Run reads one short prompt per non-empty line. The production terminal adds
// explicit /paste and /prompt-file support; this fallback deliberately never
// treats a blank line as a prompt boundary. /status, /interrupt, /quit and
// /approve ID are local controls. This is a
// transport-safe fallback UI, not an attempt to emulate undocumented TUI key
// bindings.
func (t *Terminal) Run(ctx context.Context) error {
	if t.Client == nil {
		return fmt.Errorf("app-server client is nil")
	}
	if t.In == nil {
		t.In = strings.NewReader("")
	}
	if t.Out == nil {
		t.Out = io.Discard
	}
	r := bufio.NewReader(t.In)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := r.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "/quit" {
			return nil
		}
		if strings.HasPrefix(line, "/approve ") {
			if e := t.approve(strings.TrimSpace(strings.TrimPrefix(line, "/approve "))); e != nil {
				return e
			}
			continue
		}
		if strings.HasPrefix(line, "/deny ") {
			if e := t.decide(strings.TrimSpace(strings.TrimPrefix(line, "/deny ")), false); e != nil {
				return e
			}
			continue
		}
		if line == "/interrupt" {
			if e := t.Client.InterruptTurn(ctx, t.ThreadID, t.ActiveTurnID); e != nil {
				fmt.Fprintf(t.errorWriter(), "[command error: %v]\n", e)
			}
			continue
		}
		if line == "/status" {
			fmt.Fprintf(t.Out, "goal thread=%s turns=%d active=%s\n", t.ThreadID, t.Turns, t.Active)
			continue
		}
		if line != "" {
			if e := t.submit(ctx, line); e != nil {
				return e
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (t *Terminal) submit(ctx context.Context, text string) error {
	if t.OnPrompt != nil {
		if err := t.OnPrompt(text); err != nil {
			return err
		}
	}
	t.Active = "turn"
	t.Turns++
	defer func() { t.Active = "" }()
	reply, err := t.Client.StartTurn(ctx, TurnStartParams{ThreadID: t.ThreadID, Input: []Input{{Type: "text", Text: text}}})
	if err != nil {
		return err
	}
	t.ActiveTurnID = TurnID(reply)
	if t.ActiveTurnID == "" {
		return fmt.Errorf("app-server did not return an active turn ID")
	}
	defer func() { t.ActiveTurnID = "" }()
	var message strings.Builder
	for {
		select {
		case ev, ok := <-t.Client.Events:
			if !ok {
				return io.EOF
			}
			if ev.Method == "item/agentMessage/delta" || ev.Method == "agentMessage/delta" {
				var p struct {
					Delta string `json:"delta"`
					Text  string `json:"text"`
				}
				_ = json.Unmarshal(ev.Params, &p)
				if p.Delta != "" {
					fmt.Fprint(t.Out, p.Delta)
					message.WriteString(p.Delta)
				} else if p.Text != "" {
					fmt.Fprint(t.Out, p.Text)
					message.WriteString(p.Text)
				}
			}
			if ev.Method == "turn/completed" {
				fmt.Fprintln(t.Out)
				if t.OnTurnCompleted != nil {
					next, err := t.OnTurnCompleted(message.String())
					if err != nil {
						return err
					}
					if next != "" {
						return t.submit(ctx, next)
					}
				}
				return nil
			}
		case req, ok := <-t.Client.Requests:
			if ok {
				fmt.Fprintf(t.Out, "\n[approval required: %s id=%s; no automatic approval]\n", req.Method, string(req.ID))
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *Terminal) errorWriter() io.Writer {
	if t.Err != nil {
		return t.Err
	}
	return t.Out
}

func (t *Terminal) approve(id string) error {
	return t.decide(id, true)
}

func (t *Terminal) decide(id string, approved bool) error {
	if id == "" {
		return fmt.Errorf("approval id is required")
	}
	// Approval request IDs are opaque JSON values. The user must provide an
	// explicit decision; this helper is intentionally not called automatically.
	b, _ := json.Marshal(map[string]any{"approved": approved})
	return t.Client.Reply(json.RawMessage(id), json.RawMessage(b))
}
