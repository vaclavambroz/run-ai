package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

type rw struct {
	io.Reader
	io.Writer
}

func (rw) Close() error { return nil }
func TestClientRPCAndEvents(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := New(inW, outR)
	defer c.Close()
	go func() {
		s := bufio.NewScanner(inR)
		for s.Scan() {
			var m Message
			_ = json.Unmarshal(s.Bytes(), &m)
			if m.Method == "initialize" {
				b, _ := json.Marshal(Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{}`)})
				outW.Write(append(b, '\n'))
			}
			if m.Method == "thread/start" {
				b, _ := json.Marshal(Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{"threadId":"t1"}`)})
				outW.Write(append(b, '\n'))
				b, _ = json.Marshal(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"t1"}`)})
				outW.Write(append(b, '\n'))
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := c.Initialize(ctx, InitializeParams{ClientInfo: ClientInfo{Name: "test", Version: "1"}}); e != nil {
		t.Fatal(e)
	}
	id, e := c.StartThread(ctx, ThreadStartParams{})
	if e != nil || id != "t1" {
		t.Fatalf("%s %v", id, e)
	}
	select {
	case ev := <-c.Events:
		if ev.Method != "turn/completed" {
			t.Fatal(ev.Method)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func TestServerRequestIsNotResponse(t *testing.T) {
	_, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := New(inW, outR)
	defer c.Close()
	go func() {
		b, _ := json.Marshal(Message{JSONRPC: "2.0", ID: json.RawMessage("9"), Method: "execCommandApproval", Params: json.RawMessage(`{"command":"rm"}`)})
		outW.Write(append(b, '\n'))
	}()
	select {
	case r := <-c.Requests:
		if r.Method != "execCommandApproval" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("no request")
	}
}

func TestInterruptTurnSendsThreadAndActiveTurnIDs(t *testing.T) {
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method != "turn/interrupt" {
			return
		}
		var params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			t.Errorf("decode interrupt params: %v", err)
		}
		if params.ThreadID != "thread-42" || params.TurnID != "turn-99" {
			t.Errorf("interrupt params = %+v, want thread-42/turn-99", params)
		}
		reply(f, m, `{}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.InterruptTurn(ctx, "thread-42", "turn-99"); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
}
