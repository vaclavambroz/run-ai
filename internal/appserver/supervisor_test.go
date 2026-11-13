package appserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSupervisorSocketSerializesTurns(t *testing.T) {
	// The public command contract rejects a second turn while active; transport
	// integration is covered by the JSON-RPC fake tests.
	s := &Supervisor{Socket: t.TempDir() + "/s.sock"}
	startPersistentSupervisor(t, s)
	conn, e := Connect(s.Socket)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	_ = json.NewEncoder(conn).Encode(IPCCommand{Op: "status"})
	var r Reply
	if e = json.NewDecoder(conn).Decode(&r); e != nil || !r.OK {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestSupervisorTurnWaitsForCompletionEvent(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method != "turn/start" {
			return
		}
		reply(f, m, `{"turnId":"one"}`)
		close(started)
		<-finish
		f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1"}`)})
	})
	s := &Supervisor{Client: c, ThreadID: "thread-1"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.turn(ctx, "continue") }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("turn did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("turn returned before completion: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("turn did not complete")
	}
}
