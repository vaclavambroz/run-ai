package appserver

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newLimitSupervisor(t *testing.T, c *Client, thread string) (*Supervisor, *JournalStore) {
	t.Helper()
	dir := t.TempDir()
	store := &JournalStore{Path: filepath.Join(dir, "journal.json")}
	s := &Supervisor{Client: c, ThreadID: thread, Store: *store, ctx: context.Background()}
	s.mu.Lock()
	err := s.setup()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	go s.consumeProvider(s.ctx)
	return s, store
}

func completedEvent(thread string) Message {
	return Message{JSONRPC: "2.0", Method: "turn/completed", Params: json.RawMessage(`{"threadId":"` + thread + `","turn":{"status":"completed"}}`)}
}

func markerEvent(thread, marker string) Message {
	return Message{JSONRPC: "2.0", Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"` + thread + `","delta":"RUN_AI_GOAL_STATE: ` + marker + `"}`)}
}

func TestAutonomousTurnLimitPausesDurably(t *testing.T) {
	t.Setenv("RUN_AI_APP_MAX_AUTONOMOUS_TURNS", "1")
	t.Setenv("RUN_AI_APP_TURN_TIMEOUT", "1h")
	t.Setenv("RUN_AI_APP_TOTAL_TIMEOUT", "1h")
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
	s, store := newLimitSupervisor(t, c, "limit-thread")
	if _, err := s.command(IPCCommand{Op: "prompt", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 1 })
	f.send(markerEvent("limit-thread", "checkpoint"))
	f.send(completedEvent("limit-thread"))
	eventually(t, "first autonomous turn did not start", func() bool { mu.Lock(); defer mu.Unlock(); return turns == 2 })
	f.send(markerEvent("limit-thread", "checkpoint"))
	f.send(completedEvent("limit-thread"))
	eventually(t, "autonomous limit did not pause", func() bool {
		j, err := store.Load("limit-thread")
		return err == nil && j.State == SupervisorPaused && j.Execution.AutonomousTurns == 1 && j.Execution.LimitReason == "max_autonomous_turns_reached"
	})
	mu.Lock()
	defer mu.Unlock()
	if turns != 2 {
		t.Fatalf("autonomous turn limit started %d turns", turns)
	}
}

func TestTurnTimeoutRetriesOnlyInterrupt(t *testing.T) {
	t.Setenv("RUN_AI_APP_TURN_TIMEOUT", "20ms")
	t.Setenv("RUN_AI_APP_TOTAL_TIMEOUT", "1h")
	t.Setenv("RUN_AI_APP_MAX_RETRIES", "1")
	t.Setenv("RUN_AI_APP_RETRY_BACKOFF", "1ms")
	var mu sync.Mutex
	starts, interrupts := 0, 0
	c, _ := newFakeRPC(t, func(m Message, f *fakeRPC) {
		switch m.Method {
		case "turn/start":
			mu.Lock()
			starts++
			mu.Unlock()
			reply(f, m, `{"turnId":"slow"}`)
		case "turn/interrupt":
			mu.Lock()
			interrupts++
			n := interrupts
			mu.Unlock()
			if n == 1 {
				f.send(Message{JSONRPC: "2.0", ID: m.ID, Error: json.RawMessage(`{"code":-1,"message":"temporary"}`)})
				return
			}
			reply(f, m, `{}`)
		}
	})
	s, store := newLimitSupervisor(t, c, "timeout-thread")
	if _, err := s.command(IPCCommand{Op: "prompt", Text: "slow"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "turn timeout was not persisted", func() bool {
		j, err := store.Load("timeout-thread")
		return err == nil && j.State == SupervisorInterrupting && j.Execution.LimitReason == "turn_timeout_reached" && j.Execution.RetryAttempts == 1
	})
	eventually(t, "interrupt was not retried", func() bool { mu.Lock(); defer mu.Unlock(); return interrupts == 2 })
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("turn/start was retried %d times", starts)
	}
}

func TestTotalTimeoutPersistsAcrossRestart(t *testing.T) {
	t.Setenv("RUN_AI_APP_MAX_AUTONOMOUS_TURNS", "99")
	store := JournalStore{Path: filepath.Join(t.TempDir(), "journal.json")}
	started := time.Now().UTC().Add(-time.Hour)
	if err := store.Save(Journal{Version: 1, ThreadID: "restart-limit", State: SupervisorIdle, Limits: Limits{MaxAutonomousTurns: 1, TurnTimeout: time.Hour, TotalTimeout: 30 * time.Minute, MaxRetries: 2, RetryBackoff: time.Second}, Execution: Execution{StartedAt: started, AutonomousTurns: 1}}); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{ThreadID: "restart-limit", Store: store, ctx: context.Background()}
	s.mu.Lock()
	err := s.setup()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if s.j.Limits.MaxAutonomousTurns != 1 || !s.j.Execution.StartedAt.Equal(started) {
		t.Fatalf("restart reset durable limits: %+v", s.j)
	}
	s.enforceTotalTimeout(started)
	j, err := store.Load("restart-limit")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != SupervisorPaused || j.Execution.LimitReason != "total_timeout_reached" {
		t.Fatalf("total deadline was not restored: %+v", j)
	}
}

func TestRestartReconcilesInterruptingTurn(t *testing.T) {
	store := JournalStore{Path: filepath.Join(t.TempDir(), "journal.json")}
	now := time.Now().UTC()
	if err := store.Save(Journal{Version: 1, ThreadID: "interrupting-restart", State: SupervisorPaused, Instructions: []Instruction{{ID: 1, State: "interrupting", CreatedAt: now, UpdatedAt: now}}, Limits: defaultLimits()}); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{ThreadID: "interrupting-restart", Store: store}
	s.mu.Lock()
	err := s.setup()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if s.j.State != SupervisorPaused || s.j.Instructions[0].State != "uncertain" || s.j.Checkpoint != "restart_reconciliation_required:instruction-1" {
		t.Fatalf("interrupting turn was not reconciled safely: %+v", s.j)
	}
}

func TestShutdownMarksActiveTurnUncertainAndStopsDaemon(t *testing.T) {
	store := JournalStore{Path: filepath.Join(t.TempDir(), "journal.json")}
	if err := store.Save(Journal{Version: 1, ThreadID: "shutdown-thread", State: SupervisorIdle}); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	var once sync.Once
	s := &Supervisor{ThreadID: "shutdown-thread", Store: store, Stop: func() { once.Do(func() { close(stopped) }) }}
	s.mu.Lock()
	if err := s.setup(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.j.Instructions = append(s.j.Instructions, Instruction{ID: 1, State: "starting"})
	s.mu.Unlock()
	server, client := net.Pipe()
	defer client.Close()
	go s.handle(server)
	if err := json.NewEncoder(client).Encode(IPCCommand{Op: "shutdown"}); err != nil {
		t.Fatal(err)
	}
	var reply Reply
	if err := json.NewDecoder(client).Decode(&reply); err != nil || reply.State != SupervisorStopped {
		t.Fatalf("shutdown reply: %+v %v", reply, err)
	}
	s.mu.Lock()
	state, checkpoint := s.j.Instructions[0].State, s.j.Checkpoint
	s.mu.Unlock()
	if state != "uncertain" || checkpoint != "daemon_shutdown_reconciliation_required:instruction-1" {
		t.Fatalf("unsafe shutdown state=%s checkpoint=%s", state, checkpoint)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not stop daemon")
	}
}
