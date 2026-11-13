package appserver

// The journal is intentionally small and append-like: every outbound turn is
// recorded before it reaches the provider.  In particular, a turn recorded as
// starting is never automatically replayed after a daemon crash, because the
// provider may already have received it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	SupervisorIdle         = "idle"
	SupervisorInterrupting = "interrupting"
	SupervisorPaused       = "paused"
	SupervisorBlocked      = "blocked"
	SupervisorCompleted    = "completed"
	SupervisorTerminated   = "terminated"
	SupervisorStopped      = "stopped"
)

// Limits are immutable for a journal once it has started. This prevents a
// reconnect or daemon restart from silently extending an executing goal.
type Limits struct {
	MaxAutonomousTurns int           `json:"max_autonomous_turns"`
	TurnTimeout        time.Duration `json:"turn_timeout_ns"`
	TotalTimeout       time.Duration `json:"total_timeout_ns"`
	MaxRetries         int           `json:"max_retries"`
	RetryBackoff       time.Duration `json:"retry_backoff_ns"`
}

type Execution struct {
	StartedAt           time.Time `json:"started_at,omitempty"`
	AutonomousTurns     int       `json:"autonomous_turns"`
	RetryAttempts       int       `json:"retry_attempts"`
	LimitReason         string    `json:"limit_reason,omitempty"`
	InterruptInProgress bool      `json:"interrupt_in_progress,omitempty"`
	ActiveTurnID        string    `json:"active_turn_id,omitempty"`
}

type Instruction struct {
	ID         uint64    `json:"id"`
	Text       string    `json:"text"`
	State      string    `json:"state"` // queued, starting, interrupting, interrupted, timed_out, checkpoint, completed, blocked, failed, uncertain, reconciled, cancelled
	Outcome    string    `json:"outcome,omitempty"`
	Output     string    `json:"output,omitempty"`
	Autonomous bool      `json:"autonomous,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type PendingApproval struct {
	ID        json.RawMessage `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	CreatedAt time.Time       `json:"created_at"`
}

type JournalEvent struct {
	Sequence uint64          `json:"sequence"`
	Type     string          `json:"type"`
	Message  string          `json:"message,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	At       time.Time       `json:"at"`
}

type Journal struct {
	Version          int               `json:"version"`
	ThreadID         string            `json:"thread_id"`
	State            string            `json:"state"`
	Checkpoint       string            `json:"checkpoint,omitempty"`
	NextInstruction  uint64            `json:"next_instruction"`
	NextEvent        uint64            `json:"next_event"`
	Instructions     []Instruction     `json:"instructions"`
	PendingApprovals []PendingApproval `json:"pending_approvals,omitempty"`
	Events           []JournalEvent    `json:"events,omitempty"`
	Limits           Limits            `json:"limits"`
	Execution        Execution         `json:"execution"`
}

type JournalStore struct{ Path string }

func DefaultJournalPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "app-supervisor-state.json")
}

func (s JournalStore) Load(threadID string) (Journal, error) {
	if s.Path == "" {
		return Journal{}, errors.New("journal path is empty")
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Journal{Version: 1, ThreadID: threadID, State: SupervisorIdle}, nil
	}
	if err != nil {
		return Journal{}, err
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return Journal{}, fmt.Errorf("decode supervisor journal: %w", err)
	}
	if j.Version != 1 || j.ThreadID == "" {
		return Journal{}, errors.New("invalid supervisor journal")
	}
	if threadID != "" && j.ThreadID != threadID {
		return Journal{}, fmt.Errorf("journal is bound to thread %s, not %s", j.ThreadID, threadID)
	}
	if j.State == "" {
		j.State = SupervisorIdle
	}
	return j, nil
}

func (s JournalStore) Save(j Journal) error {
	if s.Path == "" || j.ThreadID == "" {
		return errors.New("incomplete supervisor journal")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + fmt.Sprintf(".tmp.%d", os.Getpid())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (j *Journal) addEvent(typ, message string, data json.RawMessage) {
	j.NextEvent++
	j.Events = append(j.Events, JournalEvent{Sequence: j.NextEvent, Type: typ, Message: message, Data: data, At: time.Now().UTC()})
	// A bounded event tail makes polling safe even if a terminal remains away
	// for a long time.  Durable state (queue/checkpoint/approvals) is separate.
	if len(j.Events) > 256 {
		j.Events = append([]JournalEvent(nil), j.Events[len(j.Events)-256:]...)
	}
}

func (j Journal) active() bool {
	for _, instruction := range j.Instructions {
		if instruction.State == "starting" || instruction.State == "interrupting" {
			return true
		}
	}
	return false
}

func (j Journal) nextQueued() int {
	for i := range j.Instructions {
		if j.Instructions[i].State == "queued" {
			return i
		}
	}
	return -1
}

// Counts distinguishes provider turns from instructions waiting in the local
// queue.  A queued instruction has never reached Codex and must not inflate a
// session's executed-turn count.
func (j Journal) Counts() (executed, queued, active int) {
	for _, instruction := range j.Instructions {
		switch instruction.State {
		case "queued":
			queued++
		case "starting", "interrupting":
			executed++
			active++
		case "cancelled":
			// Cancelled queue entries never became provider turns.
		default:
			executed++
		}
	}
	return executed, queued, active
}
