package appserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// This exercises the durable scheduling state even on platforms where the
// test sandbox forbids AF_UNIX. The companion E2E test covers the same flow
// through a real socket when that capability is available.
func TestJournalledQueueSerializesAndPersistsWithoutSocket(t *testing.T) {
	var mu sync.Mutex
	turns := 0
	c, f := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method == "turn/start" {
			mu.Lock()
			turns++
			mu.Unlock()
			reply(f, m, `{"turnId":"turn"}`)
		}
	})
	dir := t.TempDir()
	s := &Supervisor{Client: c, ThreadID: "journal-thread", Socket: filepath.Join(dir, "unused.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}, ctx: context.Background()}
	s.mu.Lock()
	if err := s.setup(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	go s.consumeProvider(s.ctx)
	if reply, err := s.command(IPCCommand{Op: "prompt", Text: "first"}); err != nil || reply.InstructionID != 1 {
		t.Fatalf("first: %+v %v", reply, err)
	}
	eventually(t, "first journalled turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 1 })
	if reply, err := s.command(IPCCommand{Op: "prompt", Text: "second"}); err != nil || reply.InstructionID != 2 {
		t.Fatalf("second: %+v %v", reply, err)
	}
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	if turns != 1 {
		mu.Unlock()
		t.Fatalf("overlap: %d", turns)
	}
	mu.Unlock()
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"journal-thread","turn":{"status":"completed"}}`)})
	eventually(t, "second journalled turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 2 })
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"journal-thread","turn":{"status":"completed"}}`)})
	eventually(t, "completed turns were not persisted", func() bool {
		j, err := s.Store.Load("journal-thread")
		return err == nil && len(j.Instructions) == 2 && j.Instructions[0].State == "completed" && j.Instructions[1].State == "completed"
	})
}

func TestJournalRestartNeverReplaysUncertainInstruction(t *testing.T) {
	dir := t.TempDir()
	store := JournalStore{Path: filepath.Join(dir, "journal.json")}
	now := time.Now().UTC()
	if err := store.Save(Journal{Version: 1, ThreadID: "restart", State: SupervisorIdle, NextInstruction: 1, Instructions: []Instruction{{ID: 1, Text: "unsafe original", State: "starting", CreatedAt: now, UpdatedAt: now}}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method == "turn/start" {
			started <- struct{}{}
			reply(f, m, `{"turnId":"new"}`)
		}
	})
	s := &Supervisor{Client: c, ThreadID: "restart", Socket: filepath.Join(dir, "unused.sock"), Store: store, ctx: context.Background()}
	s.mu.Lock()
	if err := s.setup(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	if s.j.State != SupervisorPaused || s.j.Instructions[0].State != "uncertain" {
		t.Fatalf("restart reconciliation: %+v", s.j)
	}
	if _, err := s.command(IPCCommand{Op: "resume"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("continuation did not start")
	}
	// Wait for the asynchronous turn/start reply to be durably recorded before
	// the temp journal is cleaned up (and before asserting its restart state).
	eventually(t, "continuation turn ID was not persisted", func() bool {
		j, loadErr := store.Load("restart")
		return loadErr == nil && j.Execution.ActiveTurnID == "new"
	})
	j, err := store.Load("restart")
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Instructions) != 2 || j.Instructions[0].State != "reconciled" || j.Instructions[1].Text == "unsafe original" {
		t.Fatalf("unsafe restart journal: %+v", j.Instructions)
	}
}

func TestJournalCountsSeparateExecutedTurnsFromQueuedInstructions(t *testing.T) {
	j := Journal{Instructions: []Instruction{
		{State: "completed"}, {State: "interrupting"}, {State: "queued"},
		{State: "queued"}, {State: "cancelled"}, {State: "uncertain"},
	}}
	executed, queued, active := j.Counts()
	if executed != 3 || queued != 2 || active != 1 {
		t.Fatalf("counts = executed=%d queued=%d active=%d, want 3/2/1", executed, queued, active)
	}
}
