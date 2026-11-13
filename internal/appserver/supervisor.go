package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Supervisor is the single-writer IPC boundary. Clients never receive the
// Codex transport and therefore cannot start a parallel turn.
type Supervisor struct {
	Socket   string
	Client   *Client
	ThreadID string
	Store    JournalStore
	Stop     context.CancelFunc

	ctx context.Context
	mu  sync.Mutex
	j   Journal
}

type IPCCommand struct {
	Op       string          `json:"op"`
	Text     string          `json:"text,omitempty"`
	ID       json.RawMessage `json:"id,omitempty"`
	Approved *bool           `json:"approved,omitempty"`
	After    uint64          `json:"after,omitempty"`
}
type Reply struct {
	OK                 bool              `json:"ok"`
	Error              string            `json:"error,omitempty"`
	State              string            `json:"state,omitempty"`
	ThreadID           string            `json:"thread_id,omitempty"`
	Active             bool              `json:"active,omitempty"`
	Checkpoint         string            `json:"checkpoint,omitempty"`
	InstructionID      uint64            `json:"instruction_id,omitempty"`
	ExecutedTurns      int               `json:"executed_turns"`
	QueuedInstructions int               `json:"queued_instructions"`
	ActiveInstructions int               `json:"active_instructions"`
	PendingApprovals   []PendingApproval `json:"pending_approvals,omitempty"`
	Limits             Limits            `json:"limits"`
	Execution          Execution         `json:"execution"`
	LimitReason        string            `json:"limit_reason,omitempty"`
	Events             []JournalEvent    `json:"events,omitempty"`
}

// RunDaemon owns the Codex process. The caller may disconnect freely; the
// app-server transport and supervisor remain alive until ctx is cancelled.
func RunDaemon(ctx context.Context, socket, threadID string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lock, err := acquireDaemonLock(socket)
	if err != nil {
		return err
	}
	defer lock.Close()
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Errorf("codex binary not found in PATH: %w", err)
	}
	c, cmd, err := Command(ctx, codexBin, "app-server", "--stdio")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(); _ = cmd.Process.Kill() }()
	if err := c.Initialize(ctx, InitializeParams{ClientInfo: ClientInfo{Name: "run_ai_supervisor", Version: "1"}}); err != nil {
		return err
	}
	if threadID == "" {
		threadID, err = c.StartThread(ctx, ThreadStartParams{CWD: appServerCWD(), Model: appServerModel(), ApprovalPolicy: "on-request"})
	} else {
		threadID, err = c.ResumeThread(ctx, threadID)
	}
	if err != nil {
		return err
	}
	s := &Supervisor{Socket: socket, Client: c, ThreadID: threadID, Stop: cancel}
	if root := appServerJournalState(); root != "" {
		s.Store.Path = filepath.Join(root, "app-supervisor-state.json")
	}
	return s.Serve(ctx)
}

// acquireDaemonLock prevents two launchers from both reaching thread/start
// before one loses the Unix-socket bind race.
func acquireDaemonLock(socket string) (*os.File, error) {
	if socket == "" || !filepath.IsAbs(socket) {
		return nil, errors.New("supervisor socket must be an absolute path")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	// Persistent sockets share one short private runtime directory, so a lock
	// must be scoped to the socket instead of serializing every session.
	path := socket + ".lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("persistent supervisor is already running: %w", err)
	}
	return file, nil
}

// appServerCWD supplies an explicit, absolute working directory for a new
// app-server thread.  This keeps opt-in test or operator sessions isolated
// from the controller's unrelated checkout.  A relative value is ignored so
// a detached supervisor cannot silently acquire a different directory.
func appServerCWD() string {
	if cwd := os.Getenv("RUN_AI_APP_CWD"); filepath.IsAbs(cwd) {
		return cwd
	}
	return ""
}

// appServerModel is set internally by the standard persistent launcher.  It
// remains optional for the low-level compatibility entry point, where Codex
// selects its configured model when no explicit value is supplied.
func appServerModel() string { return strings.TrimSpace(os.Getenv("RUN_AI_APP_MODEL")) }

func appServerJournalState() string {
	if root := os.Getenv("RUN_AI_APP_JOURNAL_STATE"); filepath.IsAbs(root) {
		return root
	}
	return ""
}

func DefaultSocket() string {
	if socket := os.Getenv("RUN_AI_APP_SOCKET"); filepath.IsAbs(socket) {
		return socket
	}
	r := os.Getenv("RUN_AI_GOAL_STATE")
	if r == "" {
		r = "/root/.local/state/run-ai/goals"
	}
	return filepath.Join(r, "app-supervisor.sock")
}

func defaultLimits() Limits {
	return Limits{
		MaxAutonomousTurns: envNonNegativeInt("RUN_AI_APP_MAX_AUTONOMOUS_TURNS", 12),
		TurnTimeout:        envPositiveDuration("RUN_AI_APP_TURN_TIMEOUT", 15*time.Minute),
		TotalTimeout:       envPositiveDuration("RUN_AI_APP_TOTAL_TIMEOUT", 3*time.Hour),
		MaxRetries:         envNonNegativeInt("RUN_AI_APP_MAX_RETRIES", 2),
		RetryBackoff:       envPositiveDuration("RUN_AI_APP_RETRY_BACKOFF", 5*time.Second),
	}
}

func envPositiveInt(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envNonNegativeInt(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

func envPositiveDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func (s *Supervisor) setup() error {
	if s.Socket == "" {
		s.Socket = DefaultSocket()
	}
	if s.Store.Path == "" {
		s.Store.Path = DefaultJournalPath(s.Socket)
	}
	j, err := s.Store.Load(s.ThreadID)
	if err != nil {
		return err
	}
	if s.ThreadID == "" {
		s.ThreadID = j.ThreadID
	}
	s.j = j
	changed := false
	if s.j.Limits.TurnTimeout <= 0 || s.j.Limits.TotalTimeout <= 0 || s.j.Limits.RetryBackoff <= 0 || s.j.Limits.MaxRetries < 0 {
		s.j.Limits = defaultLimits()
		changed = true
	}
	if err := s.reconcileLocked(); err != nil {
		return err
	}
	if changed && s.j.ThreadID != "" {
		return s.saveLocked()
	}
	return nil
}

// reconcileLocked deliberately does not replay a turn that might have reached
// the provider before the previous daemon died.  Resume creates a *new*,
// explicit continuation turn from the recorded checkpoint instead.
func (s *Supervisor) reconcileLocked() error {
	changed := false
	for i := range s.j.Instructions {
		if s.j.Instructions[i].State == "starting" || s.j.Instructions[i].State == "interrupting" {
			s.j.Instructions[i].State = "uncertain"
			s.j.Instructions[i].UpdatedAt = time.Now().UTC()
			s.j.State = SupervisorPaused
			s.j.Checkpoint = fmt.Sprintf("restart_reconciliation_required:instruction-%d", s.j.Instructions[i].ID)
			s.j.addEvent("restart_reconciliation_required", s.j.Checkpoint, nil)
			changed = true
		}
	}
	if changed {
		return s.Store.Save(s.j)
	}
	return nil
}

func (s *Supervisor) saveLocked() error { return s.Store.Save(s.j) }

func (s *Supervisor) Serve(ctx context.Context) error {
	s.ctx = ctx
	s.mu.Lock()
	err := s.setup()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0700); err != nil {
		return err
	}
	_ = os.Remove(s.Socket)
	l, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close(); _ = os.Remove(s.Socket) }()
	_ = os.Chmod(s.Socket, 0600)
	go func() { <-ctx.Done(); _ = l.Close() }()
	if s.Client != nil {
		go s.consumeProvider(ctx)
	}
	s.scheduleGoalDeadline()
	s.kick()
	for {
		c, e := l.Accept()
		if e != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return e
			}
		}
		go s.handle(c)
	}
}

func (s *Supervisor) consumeProvider(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-s.Client.Requests:
			if !ok {
				return
			}
			s.recordApproval(req)
		case ev, ok := <-s.Client.Events:
			if !ok {
				return
			}
			s.recordEvent(ev)
		}
	}
}

func (s *Supervisor) recordApproval(req Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pending := range s.j.PendingApprovals {
		if string(pending.ID) == string(req.ID) {
			return
		}
	}
	s.j.PendingApprovals = append(s.j.PendingApprovals, PendingApproval{ID: req.ID, Method: req.Method, Params: req.Params, CreatedAt: time.Now().UTC()})
	b, _ := json.Marshal(PendingApproval{ID: req.ID, Method: req.Method, Params: req.Params, CreatedAt: time.Now().UTC()})
	s.j.addEvent("approval_required", req.Method, b)
	_ = s.saveLocked()
}

func (s *Supervisor) recordEvent(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := eventThreadID(ev.Params); id != "" && id != s.ThreadID {
		return
	}
	if ev.Method == "turn/completed" {
		providerStatus := completionStatus(ev.Params)
		s.j.Execution.InterruptInProgress = false
		s.j.Execution.ActiveTurnID = ""
		handled := false
		for i := range s.j.Instructions {
			if s.j.Instructions[i].State == "starting" || s.j.Instructions[i].State == "interrupting" {
				handled = true
				wasInterrupting := s.j.Instructions[i].State == "interrupting"
				outcome := stateMarkerText(s.j.Instructions[i].Output)
				if wasInterrupting {
					// An interrupt RPC acknowledgement only means the request was
					// accepted.  This provider completion is the first authoritative
					// indication that the active turn has actually stopped.
					if providerStatus == "completed" {
						s.j.Instructions[i].State = "completed"
						s.j.Checkpoint = fmt.Sprintf("turn_completed_after_interrupt:instruction-%d", s.j.Instructions[i].ID)
					} else {
						s.j.Instructions[i].State = "interrupted"
						s.j.Checkpoint = fmt.Sprintf("turn_%s_after_interrupt:instruction-%d", providerStatus, s.j.Instructions[i].ID)
					}
					s.j.State = SupervisorPaused
					s.j.addEvent("turn_interrupted", s.j.Checkpoint, ev.Params)
				} else if providerStatus != "completed" {
					s.j.Instructions[i].State = "failed"
					s.j.State = SupervisorPaused
					s.j.Checkpoint = fmt.Sprintf("turn_%s:instruction-%d", providerStatus, s.j.Instructions[i].ID)
					s.j.addEvent("turn_not_completed", s.j.Checkpoint, ev.Params)
				} else if outcome == "checkpoint" {
					s.j.Instructions[i].State = "checkpoint"
					s.j.Checkpoint = fmt.Sprintf("turn_checkpoint:instruction-%d", s.j.Instructions[i].ID)
					s.j.NextInstruction++
					now := time.Now().UTC()
					s.j.Instructions = append(s.j.Instructions, Instruction{ID: s.j.NextInstruction, Text: "Continue the prior task from its durable checkpoint. Do not repeat completed or destructive actions. Complete all remaining work, then output exactly one marker on its own line: RUN_AI_GOAL_STATE: completed. Checkpoint: " + s.j.Checkpoint, State: "queued", Autonomous: true, CreatedAt: now, UpdatedAt: now})
				} else if outcome == "blocked" {
					s.j.Instructions[i].State = "blocked"
					s.j.State = SupervisorBlocked
					s.j.Checkpoint = fmt.Sprintf("turn_blocked:instruction-%d", s.j.Instructions[i].ID)
				} else {
					s.j.Instructions[i].State = "completed"
					s.j.Checkpoint = fmt.Sprintf("turn_completed:instruction-%d", s.j.Instructions[i].ID)
				}
				s.j.Instructions[i].UpdatedAt = time.Now().UTC()
				if providerStatus == "completed" && outcome == "completed" {
					s.j.State = SupervisorCompleted
				}
				break
			}
		}
		if !handled {
			for i := range s.j.Instructions {
				if s.j.Instructions[i].State == "interrupting" {
					s.j.Instructions[i].State = "timed_out"
					s.j.Instructions[i].UpdatedAt = time.Now().UTC()
					break
				}
			}
		}
		if s.j.State != SupervisorPaused && s.j.State != SupervisorInterrupting && s.j.State != SupervisorTerminated && s.j.State != SupervisorCompleted && s.j.State != SupervisorBlocked {
			s.j.State = SupervisorIdle
		}
		s.j.addEvent("turn_completed", s.j.Checkpoint, ev.Params)
		_ = s.saveLocked()
		go s.kick()
		return
	}
	// Keep visible provider events available after a terminal reconnect.  Text
	// deltas are intentionally journalled rather than tied to one socket.
	if ev.Method == "item/agentMessage/delta" || ev.Method == "agentMessage/delta" {
		for i := range s.j.Instructions {
			if s.j.Instructions[i].State == "starting" {
				s.j.Instructions[i].Output = appendBounded(s.j.Instructions[i].Output, agentText(ev.Params))
				break
			}
		}
		s.j.addEvent("agent_message", ev.Method, ev.Params)
		_ = s.saveLocked()
		return
	}
	// Tool activity is useful progress evidence even when it has no text
	// delta. Retain a bounded event record for reconnecting terminals.
	if strings.Contains(strings.ToLower(ev.Method), "tool") || strings.Contains(strings.ToLower(ev.Method), "command") {
		s.j.addEvent("tool_event", ev.Method, ev.Params)
		_ = s.saveLocked()
	}
}

func completionStatus(params json.RawMessage) string {
	var p struct {
		Status string `json:"status"`
		Turn   struct {
			Status string `json:"status"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil {
		return "unknown"
	}
	if p.Turn.Status != "" {
		return p.Turn.Status
	}
	if p.Status != "" {
		return p.Status
	}
	return "unknown"
}

func eventThreadID(params json.RawMessage) string {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &p)
	return p.ThreadID
}

func appendBounded(existing, addition string) string {
	const limit = 32 << 10
	combined := existing + addition
	if len(combined) <= limit {
		return combined
	}
	return combined[len(combined)-limit:]
}

// goalStateMarker accepts only an explicit terminal marker emitted by the
// provider. A transport-level turn/completed event means merely that one
// provider turn ended, not that the supervised task completed.
func stateMarkerText(text string) string {
	for _, marker := range []string{"completed", "checkpoint", "blocked"} {
		if strings.Contains(text, "RUN_AI_GOAL_STATE: "+marker) {
			return marker
		}
	}
	return ""
}

func agentText(params json.RawMessage) string {
	var value any
	if json.Unmarshal(params, &value) != nil {
		return ""
	}
	var b strings.Builder
	var visit func(any)
	visit = func(v any) {
		switch typed := v.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "delta" || key == "text" {
					if text, ok := child.(string); ok {
						b.WriteString(text)
						continue
					}
				}
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return b.String()
}

func (s *Supervisor) kick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Client == nil || s.j.State != SupervisorIdle || s.j.active() {
		return
	}
	if s.goalDeadlineReachedLocked(time.Now().UTC()) {
		s.pauseForLimitLocked("total_timeout_reached", "goal total time limit reached")
		return
	}
	i := s.j.nextQueued()
	if i < 0 {
		return
	}
	if s.j.Instructions[i].Autonomous && s.j.Execution.AutonomousTurns >= s.j.Limits.MaxAutonomousTurns {
		s.pauseForLimitLocked("max_autonomous_turns_reached", "autonomous turn limit reached")
		return
	}
	s.j.Instructions[i].State = "starting"
	s.j.Instructions[i].UpdatedAt = time.Now().UTC()
	if s.j.Instructions[i].Autonomous {
		s.j.Execution.AutonomousTurns++
	}
	s.j.Execution.InterruptInProgress = false
	s.j.addEvent("turn_starting", fmt.Sprintf("instruction-%d", s.j.Instructions[i].ID), nil)
	if err := s.saveLocked(); err != nil {
		s.j.Instructions[i].State = "queued"
		s.j.addEvent("journal_error", err.Error(), nil)
		return
	}
	instruction := s.j.Instructions[i]
	go s.startTurn(instruction)
	go s.watchTurnDeadline(instruction.ID)
}

func (s *Supervisor) startTurn(instruction Instruction) {
	reply, err := s.Client.StartTurn(s.ctx, TurnStartParams{ThreadID: s.ThreadID, Input: []Input{{Type: "text", Text: instruction.Text}}})
	if err == nil {
		turnID := TurnID(reply)
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.j.Instructions {
			if s.j.Instructions[i].ID == instruction.ID && (s.j.Instructions[i].State == "starting" || s.j.Instructions[i].State == "interrupting") {
				if turnID == "" {
					s.j.Instructions[i].State = "uncertain"
					s.j.Instructions[i].UpdatedAt = time.Now().UTC()
					s.j.State = SupervisorPaused
					s.j.Checkpoint = fmt.Sprintf("turn_start_missing_id:instruction-%d", instruction.ID)
					s.j.addEvent("restart_reconciliation_required", "provider did not return an active turn ID", nil)
				} else {
					s.j.Execution.ActiveTurnID = turnID
					s.j.addEvent("turn_started", fmt.Sprintf("instruction-%d turn=%s", instruction.ID, turnID), nil)
					if s.j.Instructions[i].State == "interrupting" && s.j.Execution.InterruptInProgress {
						go s.interruptWithRetry("interrupt_requested")
					}
				}
				_ = s.saveLocked()
				break
			}
		}
		return // completion is handled by consumeProvider.
	}
	// A turn/start transport error is deliberately never retried: the provider
	// may have accepted the request before the connection failed.
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.j.Instructions {
		if s.j.Instructions[i].ID == instruction.ID && s.j.Instructions[i].State == "starting" {
			s.j.Instructions[i].State = "uncertain"
			s.j.Instructions[i].UpdatedAt = time.Now().UTC()
			s.j.State = SupervisorPaused
			s.j.Checkpoint = fmt.Sprintf("turn_start_uncertain:instruction-%d", instruction.ID)
			s.j.addEvent("restart_reconciliation_required", err.Error(), nil)
			_ = s.saveLocked()
		}
	}
}

func (s *Supervisor) goalDeadlineReachedLocked(now time.Time) bool {
	if s.j.Execution.StartedAt.IsZero() || s.j.Limits.TotalTimeout <= 0 {
		return false
	}
	return !now.Before(s.j.Execution.StartedAt.Add(s.j.Limits.TotalTimeout))
}

func (s *Supervisor) pauseForLimitLocked(reason, message string) {
	if s.j.State == SupervisorCompleted || s.j.State == SupervisorBlocked || s.j.State == SupervisorTerminated || s.j.State == SupervisorStopped {
		return
	}
	if s.j.active() {
		s.j.State = SupervisorInterrupting
	} else {
		s.j.State = SupervisorPaused
	}
	s.j.Checkpoint = reason
	s.j.Execution.LimitReason = reason
	s.j.addEvent("execution_limit_reached", message, nil)
	_ = s.saveLocked()
}

func (s *Supervisor) scheduleGoalDeadline() {
	s.mu.Lock()
	started := s.j.Execution.StartedAt
	limit := s.j.Limits.TotalTimeout
	s.mu.Unlock()
	if started.IsZero() || limit <= 0 {
		return
	}
	go func() {
		wait := time.Until(started.Add(limit))
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			ctx := s.context()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
		s.enforceTotalTimeout(started)
	}()
}

func (s *Supervisor) watchTurnDeadline(instructionID uint64) {
	s.mu.Lock()
	limit := s.j.Limits.TurnTimeout
	s.mu.Unlock()
	if limit <= 0 {
		return
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ctx := s.context()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	s.enforceTurnTimeout(instructionID, "turn_timeout_reached", "turn time limit reached")
}

func (s *Supervisor) enforceTotalTimeout(started time.Time) {
	var interrupt bool
	s.mu.Lock()
	if s.j.Execution.StartedAt != started || !s.goalDeadlineReachedLocked(time.Now().UTC()) {
		s.mu.Unlock()
		return
	}
	for i := range s.j.Instructions {
		if s.j.Instructions[i].State == "starting" {
			s.j.Instructions[i].State = "interrupting"
			s.j.Instructions[i].UpdatedAt = time.Now().UTC()
			interrupt = true
			break
		}
	}
	s.pauseForLimitLocked("total_timeout_reached", "goal total time limit reached")
	if interrupt && !s.j.Execution.InterruptInProgress {
		s.j.Execution.InterruptInProgress = true
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	if interrupt {
		go s.interruptWithRetry("total_timeout_reached")
	}
}

func (s *Supervisor) enforceTurnTimeout(instructionID uint64, reason, message string) {
	var interrupt bool
	s.mu.Lock()
	for i := range s.j.Instructions {
		if s.j.Instructions[i].ID == instructionID && s.j.Instructions[i].State == "starting" {
			s.j.Instructions[i].State = "interrupting"
			s.j.Instructions[i].UpdatedAt = time.Now().UTC()
			interrupt = true
			break
		}
	}
	if !interrupt {
		s.mu.Unlock()
		return
	}
	s.pauseForLimitLocked(reason, message)
	if !s.j.Execution.InterruptInProgress {
		s.j.Execution.InterruptInProgress = true
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	go s.interruptWithRetry(reason)
}

func (s *Supervisor) context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// interruptWithRetry retries only turn/interrupt, which is idempotent. It
// never retries turn/start because delivery of that operation can be unknown.
func (s *Supervisor) interruptWithRetry(reason string) {
	for {
		s.mu.Lock()
		client := s.Client
		threadID := s.ThreadID
		turnID := s.j.Execution.ActiveTurnID
		backoff := s.j.Limits.RetryBackoff
		maxRetries := s.j.Limits.MaxRetries
		s.mu.Unlock()
		if client == nil || turnID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(s.context(), 5*time.Second)
		err := client.InterruptTurn(ctx, threadID, turnID)
		cancel()
		if err == nil {
			s.mu.Lock()
			// RPC success only acknowledges the request.  The journal remains
			// interrupting until turn/completed confirms that the provider turn
			// ended, so /pause cannot claim a completed interruption early.
			s.j.addEvent("turn_interrupt_acknowledged", reason, nil)
			_ = s.saveLocked()
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		if s.j.Execution.RetryAttempts >= maxRetries {
			s.j.addEvent("interrupt_retry_exhausted", reason+": "+err.Error(), nil)
			_ = s.saveLocked()
			s.mu.Unlock()
			return
		}
		s.j.Execution.RetryAttempts++
		s.j.addEvent("interrupt_retry", fmt.Sprintf("%s: retry=%d error=%v", reason, s.j.Execution.RetryAttempts, err), nil)
		_ = s.saveLocked()
		s.mu.Unlock()
		if backoff <= 0 {
			backoff = 5 * time.Second
		}
		timer := time.NewTimer(backoff)
		select {
		case <-s.context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// requestInterruptLocked records intent before sending anything to Codex. The
// caller holds s.mu.  A successful RPC reply is not treated as completion;
// only the corresponding turn/completed event settles the paused state.
func (s *Supervisor) requestInterruptLocked(reason string) error {
	if s.Client == nil {
		return errors.New("app-server client is unavailable")
	}
	found := false
	for i := range s.j.Instructions {
		if s.j.Instructions[i].State == "starting" {
			s.j.Instructions[i].State = "interrupting"
			s.j.Instructions[i].UpdatedAt = time.Now().UTC()
			found = true
			break
		}
		if s.j.Instructions[i].State == "interrupting" {
			found = true
			break
		}
	}
	if !found {
		return errors.New("no active turn to interrupt")
	}
	s.j.State = SupervisorInterrupting
	s.j.Checkpoint = reason
	s.j.Execution.InterruptInProgress = true
	s.j.addEvent("turn_interrupt_requested", reason, nil)
	if err := s.saveLocked(); err != nil {
		return err
	}
	// A pause can race turn/start's reply. startTurn launches this request as
	// soon as it records the authoritative turn ID.
	if s.j.Execution.ActiveTurnID != "" {
		go s.interruptWithRetry(reason)
	}
	return nil
}

func (s *Supervisor) handle(c net.Conn) {
	defer c.Close()
	enc := json.NewEncoder(c)
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var cmd IPCCommand
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			_ = enc.Encode(Reply{Error: "invalid command"})
			continue
		}
		reply, err := s.command(cmd)
		if err != nil {
			reply.Error = err.Error()
		}
		_ = enc.Encode(reply)
		if err == nil && cmd.Op == "shutdown" && s.Stop != nil {
			s.Stop()
			return
		}
	}
}

func (s *Supervisor) command(cmd IPCCommand) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := func() Reply {
		executed, queued, active := s.j.Counts()
		return Reply{OK: true, ThreadID: s.ThreadID, State: s.j.State, Active: s.j.active(), Checkpoint: s.j.Checkpoint, ExecutedTurns: executed, QueuedInstructions: queued, ActiveInstructions: active, PendingApprovals: append([]PendingApproval(nil), s.j.PendingApprovals...), Limits: s.j.Limits, Execution: s.j.Execution, LimitReason: s.j.Execution.LimitReason}
	}
	switch cmd.Op {
	case "status":
		return status(), nil
	case "events":
		r := status()
		for _, event := range s.j.Events {
			if event.Sequence > cmd.After {
				r.Events = append(r.Events, event)
			}
		}
		return r, nil
	case "prompt":
		if cmd.Text == "" {
			return Reply{}, errors.New("empty prompt")
		}
		if s.j.State == SupervisorTerminated || s.j.State == SupervisorStopped {
			return Reply{}, errors.New("supervisor is terminated")
		}
		// A completed turn finishes the prior instruction, not the durable
		// session. A reconnect may legitimately submit another explicit prompt;
		// return to idle so kick can start that new queue entry.
		if s.j.State == SupervisorCompleted {
			s.j.State = SupervisorIdle
			s.j.addEvent("session_reactivated", "new instruction after completed turn", nil)
		}
		s.j.NextInstruction++
		now := time.Now().UTC()
		startDeadline := s.j.Execution.StartedAt.IsZero()
		if startDeadline {
			s.j.Execution.StartedAt = now
		}
		instruction := Instruction{ID: s.j.NextInstruction, Text: cmd.Text, State: "queued", CreatedAt: now, UpdatedAt: now}
		s.j.Instructions = append(s.j.Instructions, instruction)
		s.j.addEvent("prompt_queued", fmt.Sprintf("instruction-%d", instruction.ID), nil)
		if err := s.saveLocked(); err != nil {
			return Reply{}, err
		}
		if startDeadline {
			go s.scheduleGoalDeadline()
		}
		go s.kick()
		return Reply{OK: true, ThreadID: s.ThreadID, State: s.j.State, InstructionID: instruction.ID}, nil
	case "interrupt":
		if err := s.requestInterruptLocked("interrupt_requested_by_operator"); err != nil {
			return Reply{}, err
		}
		return status(), nil
	case "approve":
		if cmd.ID == nil || cmd.Approved == nil {
			return Reply{}, errors.New("approval id and decision required")
		}
		found := -1
		for i, pending := range s.j.PendingApprovals {
			if string(pending.ID) == string(cmd.ID) {
				found = i
				break
			}
		}
		if found < 0 {
			return Reply{}, errors.New("approval is not pending")
		}
		if s.Client == nil {
			return Reply{}, errors.New("app-server client is unavailable")
		}
		// Codex 0.147 app-server uses a decision union for its current
		// item/commandExecution/requestApproval request. Retain the older
		// boolean reply for generic/legacy approval methods so callers are not
		// silently broadened into an approval they did not request.
		result := any(map[string]any{"approved": *cmd.Approved})
		if s.j.PendingApprovals[found].Method == "item/commandExecution/requestApproval" {
			decision := "cancel"
			if *cmd.Approved {
				decision = "accept"
			}
			result = map[string]any{"decision": decision}
		}
		if err := s.Client.Reply(cmd.ID, result); err != nil {
			return Reply{}, err
		}
		s.j.PendingApprovals = append(s.j.PendingApprovals[:found], s.j.PendingApprovals[found+1:]...)
		s.j.addEvent("approval_replied", fmt.Sprintf("approved=%t", *cmd.Approved), cmd.ID)
		if err := s.saveLocked(); err != nil {
			return Reply{}, err
		}
		return status(), nil
	case "pause":
		if s.j.State == SupervisorTerminated {
			return Reply{}, errors.New("supervisor is terminated")
		}
		if s.j.active() {
			if err := s.requestInterruptLocked("paused_by_operator"); err != nil {
				return Reply{}, err
			}
		} else {
			s.j.State = SupervisorPaused
			s.j.Checkpoint = "paused_by_operator"
			s.j.addEvent("paused", s.j.Checkpoint, nil)
			if err := s.saveLocked(); err != nil {
				return Reply{}, err
			}
		}
		return status(), nil
	case "resume":
		if s.j.State == SupervisorTerminated {
			return Reply{}, errors.New("supervisor is terminated")
		}
		if s.goalDeadlineReachedLocked(time.Now().UTC()) {
			s.pauseForLimitLocked("total_timeout_reached", "goal total time limit reached")
			return status(), nil
		}
		needsContinuation := false
		for i := range s.j.Instructions {
			if s.j.Instructions[i].State == "uncertain" {
				s.j.Instructions[i].State = "reconciled"
				s.j.Instructions[i].UpdatedAt = time.Now().UTC()
				needsContinuation = true
			}
		}
		s.j.State = SupervisorIdle
		if needsContinuation {
			s.j.NextInstruction++
			now := time.Now().UTC()
			s.j.Instructions = append(s.j.Instructions, Instruction{ID: s.j.NextInstruction, Text: "Continue the prior interrupted turn from the durable checkpoint. Do not repeat completed or destructive actions; first reconcile current state. Checkpoint: " + s.j.Checkpoint, State: "queued", CreatedAt: now, UpdatedAt: now})
		}
		s.j.addEvent("resumed", s.j.Checkpoint, nil)
		if err := s.saveLocked(); err != nil {
			return Reply{}, err
		}
		go s.kick()
		return status(), nil
	case "shutdown":
		if s.j.active() {
			for i := range s.j.Instructions {
				if s.j.Instructions[i].State == "starting" || s.j.Instructions[i].State == "interrupting" {
					s.j.Instructions[i].State = "uncertain"
					s.j.Instructions[i].UpdatedAt = time.Now().UTC()
					s.j.Checkpoint = fmt.Sprintf("daemon_shutdown_reconciliation_required:instruction-%d", s.j.Instructions[i].ID)
					break
				}
			}
			s.j.State = SupervisorStopped
			s.j.addEvent("daemon_shutdown_reconciliation_required", s.j.Checkpoint, nil)
		} else {
			s.j.State = SupervisorStopped
			s.j.Checkpoint = "daemon_shutdown_by_operator"
			s.j.addEvent("daemon_shutdown", s.j.Checkpoint, nil)
		}
		if err := s.saveLocked(); err != nil {
			return Reply{}, err
		}
		return status(), nil
	case "terminate":
		if s.j.active() {
			if err := s.requestInterruptLocked("termination_requested_by_operator"); err != nil {
				return Reply{}, err
			}
			return status(), nil
		}
		s.j.State = SupervisorTerminated
		s.j.Checkpoint = "terminated_by_operator"
		for i := range s.j.Instructions {
			if s.j.Instructions[i].State == "queued" {
				s.j.Instructions[i].State = "cancelled"
				s.j.Instructions[i].UpdatedAt = time.Now().UTC()
			}
		}
		s.j.addEvent("terminated", s.j.Checkpoint, nil)
		if err := s.saveLocked(); err != nil {
			return Reply{}, err
		}
		return status(), nil
	default:
		return Reply{}, fmt.Errorf("unsupported operation %q", cmd.Op)
	}
}

// turn remains a synchronous primitive for callers that intentionally own an
// event stream (and for its focused regression test). Persistent serving uses
// the journalled queue above instead.
func (s *Supervisor) turn(ctx context.Context, text string) error {
	if s.Client == nil {
		return fmt.Errorf("app-server client is unavailable")
	}
	if _, err := s.Client.StartTurn(ctx, TurnStartParams{ThreadID: s.ThreadID, Input: []Input{{Type: "text", Text: text}}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-s.Client.Events:
			if !ok {
				return fmt.Errorf("app-server event stream closed")
			}
			if ev.Method == "turn/completed" {
				return nil
			}
		}
	}
}

func Connect(socket string) (net.Conn, error) {
	if socket == "" {
		socket = DefaultSocket()
	}
	return net.Dial("unix", socket)
}

// Send executes one IPC command. The daemon is deliberately the only holder
// of the provider transport; terminal clients use this small request/reply
// boundary and can disconnect without affecting a running turn.
func Send(ctx context.Context, socket string, command IPCCommand) (Reply, error) {
	conn, err := Connect(socket)
	if err != nil {
		return Reply{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Reply{}, err
		}
	}
	if err := json.NewEncoder(conn).Encode(command); err != nil {
		return Reply{}, err
	}
	var reply Reply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return Reply{}, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}
