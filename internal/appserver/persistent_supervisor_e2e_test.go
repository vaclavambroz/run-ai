package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func startPersistentSupervisor(t *testing.T, s *Supervisor) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { errs <- s.Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.Dial("unix", s.Socket)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case serveErr := <-errs:
			cancel()
			if errors.Is(serveErr, syscall.EPERM) {
				t.Skipf("Unix-domain sockets are unavailable in this sandbox: %v", serveErr)
			}
			t.Fatalf("supervisor start: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("supervisor socket not ready: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("supervisor stopped with %v", err)
			}
		case <-time.After(time.Second):
			t.Error("supervisor did not stop")
		}
		if s.Client != nil {
			_ = s.Client.Close()
		}
		if _, err := os.Lstat(s.Socket); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("supervisor socket was not removed: %v", err)
		}
	})
	return cancel
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}

// This is an end-to-end Unix-socket test, not a direct method test. Each Send
// call opens and closes a fresh terminal connection.  The first disconnect
// happens while its turn is active; the reconnect queues a second turn and it
// is run only after the first provider completion arrives.
func TestPersistentSupervisorDisconnectReconnectMultiTurnE2E(t *testing.T) {
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
	s := &Supervisor{Client: c, ThreadID: "durable-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "first turn"})
	if err != nil || first.InstructionID != 1 {
		t.Fatalf("first prompt: %+v %v", first, err)
	}
	eventually(t, "first turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 1 })
	// Send has now disconnected the first terminal. A new terminal remains able
	// to interact with the daemon and the instruction is journalled, not lost.
	second, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "second turn"})
	if err != nil || second.InstructionID != 2 {
		t.Fatalf("reconnected prompt: %+v %v", second, err)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if turns != 1 {
		mu.Unlock()
		t.Fatalf("overlapping provider turns: %d", turns)
	}
	mu.Unlock()
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"durable-thread","turn":{"status":"completed"}}`)})
	eventually(t, "second queued turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 2 })
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"durable-thread","turn":{"status":"completed"}}`)})
	eventually(t, "journal did not record two completed turns", func() bool {
		j, err := s.Store.Load("durable-thread")
		return err == nil && len(j.Instructions) == 2 && j.Instructions[0].State == "completed" && j.Instructions[1].State == "completed"
	})
}

// A provider goal marker completes one turn, not the durable conversational
// session.  A later reconnect must be able to submit the next independent
// instruction instead of leaving it permanently queued behind `completed`.
func TestPersistentSupervisorAcceptsPromptAfterCompletedMarker(t *testing.T) {
	var mu sync.Mutex
	turns := 0
	c, f := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method != "turn/start" {
			return
		}
		mu.Lock()
		turns++
		mu.Unlock()
		reply(f, m, `{"turnId":"turn"}`)
	})
	dir := t.TempDir()
	s := &Supervisor{Client: c, ThreadID: "completed-reconnect-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 1 })
	f.send(Message{JSONRPC: "2.0", Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"completed-reconnect-thread","delta":"RUN_AI_GOAL_STATE: completed"}`)})
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"completed-reconnect-thread","turn":{"status":"completed"}}`)})
	eventually(t, "first marked turn did not complete", func() bool {
		status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return err == nil && status.State == SupervisorCompleted && !status.Active
	})
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "second"}); err != nil {
		t.Fatalf("prompt after completed marker: %v", err)
	}
	eventually(t, "second prompt remained queued after completed marker", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 2 })
}

func TestSupervisorPersistsApprovalAndControls(t *testing.T) {
	approvalReply := make(chan bool, 1)
	c, f := newFakeRPC(t, func(m Message, f *fakeRPC) {
		switch m.Method {
		case "turn/start":
			reply(f, m, `{"turnId":"approval-turn"}`)
			f.send(Message{JSONRPC: "2.0", ID: json.RawMessage("42"), Method: "commandExecutionApproval", Params: json.RawMessage(`{"command":"touch x"}`)})
		case "turn/interrupt":
			reply(f, m, `{}`)
		case "":
			if string(m.ID) == "42" {
				var r struct {
					Approved bool `json:"approved"`
				}
				_ = json.Unmarshal(m.Result, &r)
				approvalReply <- r.Approved
			}
		}
	})
	dir := t.TempDir()
	s := &Supervisor{Client: c, ThreadID: "approval-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "requires approval"}); err != nil {
		t.Fatal(err)
	}
	var status Reply
	eventually(t, "approval was not journalled", func() bool {
		status, _ = Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return len(status.PendingApprovals) == 1
	})
	approved := true
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "approve", ID: json.RawMessage("42"), Approved: &approved}); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-approvalReply:
		if !ok {
			t.Fatal("approval reply denied")
		}
	case <-ctx.Done():
		t.Fatal("approval reply did not reach provider")
	}
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "pause"}); err != nil {
		t.Fatal(err)
	}
	status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
	if err != nil || status.State != SupervisorInterrupting || !status.Active {
		t.Fatalf("pause must remain interrupting before provider completion: %+v %v", status, err)
	}
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"approval-thread","turn":{"status":"interrupted"}}`)})
	eventually(t, "pause did not settle after provider completion", func() bool {
		status, err = Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return err == nil && status.State == SupervisorPaused && !status.Active
	})
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "terminate"}); err != nil {
		t.Fatal(err)
	}
	status, err = Send(ctx, s.Socket, IPCCommand{Op: "status"})
	if err != nil || status.State != SupervisorTerminated {
		t.Fatalf("terminate status: %+v %v", status, err)
	}
}

// An interrupt acknowledgement only proves that app-server accepted the
// request.  The original turn can still be running, so reconnecting clients
// must keep seeing an active interrupting state until turn/completed arrives.
func TestSupervisorPauseWaitsForProviderInterruptCompletion(t *testing.T) {
	interrupt := make(chan struct{}, 1)
	c, f := newFakeRPC(t, func(m Message, f *fakeRPC) {
		switch m.Method {
		case "turn/start":
			reply(f, m, `{"turnId":"active-turn"}`)
		case "turn/interrupt":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
			}
			if err := json.Unmarshal(m.Params, &params); err != nil {
				t.Errorf("decode interrupt params: %v", err)
			}
			if params.ThreadID != "pause-thread" || params.TurnID != "active-turn" {
				t.Errorf("interrupt params = %+v, want pause-thread/active-turn", params)
			}
			reply(f, m, `{}`)
			interrupt <- struct{}{}
		}
	})
	dir := t.TempDir()
	s := &Supervisor{Client: c, ThreadID: "pause-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "long running"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "turn did not become active", func() bool {
		status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return err == nil && status.Active && status.Execution.ActiveTurnID == "active-turn"
	})
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "pause"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interrupt:
	case <-ctx.Done():
		t.Fatal("pause did not send interrupt")
	}
	// This is a reconnect, not a shared command connection.  It must not
	// advertise a completed interruption merely because the RPC replied.
	status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
	if err != nil || status.State != SupervisorInterrupting || !status.Active {
		t.Fatalf("status before provider completion: %+v %v", status, err)
	}
	f.send(Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"pause-thread","turn":{"id":"active-turn","status":"interrupted"}}`)})
	eventually(t, "interrupted turn did not settle", func() bool {
		status, err = Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return err == nil && status.State == SupervisorPaused && !status.Active
	})
}

// A pending approval belongs to the persistent supervisor, rather than to the
// client connection that happened to receive it.  An approval from a new
// terminal must be accepted while the original turn remains active.
func TestSupervisorApprovalSurvivesActiveTurnReconnect(t *testing.T) {
	approvalReply := make(chan bool, 1)
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		switch m.Method {
		case "turn/start":
			reply(f, m, `{"turnId":"approval-active"}`)
			f.send(Message{JSONRPC: "2.0", ID: json.RawMessage("77"), Method: "commandExecutionApproval", Params: json.RawMessage(`{"command":"id"}`)})
		case "":
			if string(m.ID) == "77" {
				var result struct {
					Approved bool `json:"approved"`
				}
				if err := json.Unmarshal(m.Result, &result); err != nil {
					t.Errorf("decode approval result: %v", err)
				}
				approvalReply <- result.Approved
			}
		}
	})
	dir := t.TempDir()
	s := &Supervisor{Client: c, ThreadID: "approval-reconnect-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "needs approval"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "active approval was not persisted", func() bool {
		status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return err == nil && status.Active && len(status.PendingApprovals) == 1
	})
	approved := true
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "approve", ID: json.RawMessage("77"), Approved: &approved}); err != nil {
		t.Fatalf("approval from reconnected terminal: %v", err)
	}
	select {
	case got := <-approvalReply:
		if !got {
			t.Fatal("reconnected approval was denied")
		}
	case <-ctx.Done():
		t.Fatal("approval did not reach provider")
	}
	status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
	if err != nil || !status.Active || len(status.PendingApprovals) != 0 {
		t.Fatalf("approval changed active turn unexpectedly: %+v %v", status, err)
	}
}

// A rejected interrupt is an operation error, not a daemon or terminal
// failure.  The next reconnect must still be able to inspect the live turn.
func TestSupervisorRejectedInterruptKeepsServingReconnects(t *testing.T) {
	attempted := make(chan struct{}, 1)
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		switch m.Method {
		case "turn/start":
			reply(f, m, `{"turnId":"still-active"}`)
		case "turn/interrupt":
			f.send(Message{JSONRPC: "2.0", ID: m.ID, Error: json.RawMessage(`{"code":-32602,"message":"Invalid request"}`)})
			attempted <- struct{}{}
		}
	})
	dir := t.TempDir()
	s := &Supervisor{Client: c, ThreadID: "error-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: JournalStore{Path: filepath.Join(dir, "journal.json")}}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "prompt", Text: "remains active"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "turn did not receive an ID", func() bool {
		status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
		return err == nil && status.Execution.ActiveTurnID == "still-active"
	})
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "interrupt"}); err != nil {
		t.Fatalf("interrupt intent: %v", err)
	}
	select {
	case <-attempted:
	case <-ctx.Done():
		t.Fatal("interrupt request never reached provider")
	}
	status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
	if err != nil || !status.Active || status.Execution.ActiveTurnID != "still-active" {
		t.Fatalf("supervisor stopped after interrupt error: %+v %v", status, err)
	}
}

func TestSupervisorRestartRequiresReconciledContinuation(t *testing.T) {
	dir := t.TempDir()
	store := JournalStore{Path: filepath.Join(dir, "journal.json")}
	now := time.Now().UTC()
	if err := store.Save(Journal{Version: 1, ThreadID: "restart-thread", State: SupervisorIdle, NextInstruction: 1, Instructions: []Instruction{{ID: 1, Text: "possibly accepted", State: "starting", CreatedAt: now, UpdatedAt: now}}}); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		if m.Method == "turn/start" {
			var p TurnStartParams
			_ = json.Unmarshal(m.Params, &p)
			started <- p.Input[0].Text
			reply(f, m, `{"turnId":"continued"}`)
		}
	})
	s := &Supervisor{Client: c, ThreadID: "restart-thread", Socket: filepath.Join(dir, "supervisor.sock"), Store: store}
	startPersistentSupervisor(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, err := Send(ctx, s.Socket, IPCCommand{Op: "status"})
	if err != nil || status.State != SupervisorPaused || status.Active {
		t.Fatalf("reconciled status: %+v %v", status, err)
	}
	if _, err := Send(ctx, s.Socket, IPCCommand{Op: "resume"}); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-started:
		if text == "possibly accepted" || text == "" {
			t.Fatalf("unsafe replay: %q", text)
		}
	case <-ctx.Done():
		t.Fatal("continuation did not start")
	}
}
