package appserver

// These tests exercise the protocol boundary used by the persistent goal
// worker.  The fake server deliberately speaks JSON-RPC rather than returning
// canned values from a Go interface: this catches regressions in event
// ordering, resume, and approval handling without touching a real Codex
// process or a production session.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeRPC struct {
	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter
	mu   sync.Mutex
}

func newFakeRPC(t *testing.T, handler func(Message, *fakeRPC)) (*Client, *fakeRPC) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f := &fakeRPC{inR: inR, inW: inW, outR: outR, outW: outW}
	go func() {
		s := bufio.NewScanner(inR)
		for s.Scan() {
			var m Message
			if json.Unmarshal(s.Bytes(), &m) == nil {
				handler(m, f)
			}
		}
	}()
	c := New(inW, outR)
	t.Cleanup(func() { _ = c.Close(); _ = f.outW.Close(); _ = f.inR.Close() })
	return c, f
}

func (f *fakeRPC) send(m Message) {
	b, _ := json.Marshal(m)
	_, _ = f.outW.Write(append(b, '\n'))
}

func reply(f *fakeRPC, m Message, result string) {
	f.send(Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(result)})
}

func TestPersistentCheckpointThenCompletionSameThread(t *testing.T) {
	turns := 0
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		switch m.Method {
		case "initialize":
			reply(f, m, `{}`)
		case "thread/start":
			reply(f, m, `{"threadId":"goal-thread"}`)
		case "turn/start":
			turns++
			reply(f, m, `{"turnId":"turn"}`)
			state := "checkpoint"
			if turns == 2 {
				state = "completed"
			}
			f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"goal-thread","status":"` + state + `"}`)})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Initialize(ctx, InitializeParams{ClientInfo: ClientInfo{Name: "test", Version: "e2e"}}); err != nil {
		t.Fatal(err)
	}
	id, err := c.StartThread(ctx, ThreadStartParams{})
	if err != nil || id != "goal-thread" {
		t.Fatalf("start thread: %s %v", id, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.StartTurn(ctx, TurnStartParams{ThreadID: id, Input: []Input{{Type: "text", Text: "continue"}}}); err != nil {
			t.Fatal(err)
		}
		ev := <-c.Events
		if ev.Method != "turn/completed" {
			t.Fatalf("event %q", ev.Method)
		}
	}
	if turns != 2 {
		t.Fatalf("expected two serialized turns, got %d", turns)
	}
}

func TestApprovalRequestIsVisibleAndDisconnectDoesNotApprove(t *testing.T) {
	c, f := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method == "turn/start" {
			reply(f, m, `{"turnId":"approval-turn"}`)
			f.send(Message{JSONRPC: "2.0", ID: json.RawMessage("99"), Method: "commandExecutionApproval", Params: json.RawMessage(`{"command":"rm -rf /"}`)})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.StartTurn(ctx, TurnStartParams{ThreadID: "goal-thread", Input: []Input{{Type: "text", Text: "unsafe"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-c.Requests:
		if req.Method != "commandExecutionApproval" || string(req.ID) != "99" {
			t.Fatalf("unexpected approval request: %+v", req)
		}
	case <-ctx.Done():
		t.Fatal("approval request was not surfaced")
	}
	// Closing the client is the disconnected-client policy: no Reply is sent,
	// so the server must leave the turn waiting rather than auto-approving it.
	_ = c.Close()
	_ = f.outW.Close()
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("client did not disconnect")
	}
}

func TestThreadResumeAfterClientRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c1, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method == "thread/start" {
			reply(f, m, `{"threadId":"durable-thread"}`)
		}
	})
	id, err := c1.StartThread(ctx, ThreadStartParams{})
	if err != nil || id != "durable-thread" {
		t.Fatalf("start: %s %v", id, err)
	}
	_ = c1.Close()
	c2, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method == "thread/resume" {
			var p ThreadRef
			_ = json.Unmarshal(m.Params, &p)
			if p.ThreadID != "durable-thread" {
				t.Errorf("resume used wrong thread %q", p.ThreadID)
			}
			reply(f, m, `{"threadId":"durable-thread"}`)
		}
	})
	got, err := c2.ResumeThread(ctx, id)
	if err != nil || got != id {
		t.Fatalf("resume: %s %v", got, err)
	}
}
