package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vaclavambroz/run-ai/internal/appserver"
	"github.com/vaclavambroz/run-ai/internal/cli"
	"github.com/vaclavambroz/run-ai/internal/config"
	"github.com/vaclavambroz/run-ai/internal/goals"
	"github.com/vaclavambroz/run-ai/internal/legacy"
	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/routing"
	"github.com/vaclavambroz/run-ai/internal/sessions"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

const (
	exitOK          = 0
	exitUsage       = 2
	exitUnavailable = 69
	exitSoftware    = 70

	inventoryLimit = 32 << 20
)

var (
	codexRoot    = os.Getenv("RUN_AI_CODEX_ROOT")
	codexHistory = os.Getenv("RUN_AI_CODEX_HISTORY")
)

type tmuxRuntime interface {
	Exists(context.Context, string) (bool, error)
	CreateDetached(context.Context, string, sessions.Command) error
	Validate(context.Context, string, sessions.Expectation) (sessions.RuntimeInfo, error)
	Attach(context.Context, string, io.Reader, io.Writer, io.Writer) error
}

// tmuxStatusRuntime is intentionally separate from tmuxRuntime so existing
// compatibility test doubles and older integrations remain valid. Production
// TmuxClient implements it and inventory then reports observed rather than
// inferred classic-session state.
type tmuxStatusRuntime interface {
	Status(context.Context, string) (sessions.LiveStatus, error)
}

type tmuxTerminator interface {
	Terminate(context.Context, string) error
}

type dependencies struct {
	hostname            func() (string, error)
	stat                func(string) (os.FileInfo, error)
	loadInventory       func(string, int64) ([]routing.DomainRecord, error)
	lookupBinary        func(string) (string, error)
	runProcess          sessions.ProcessRunner
	uid                 func() int
	workingDirectory    func() (string, error)
	now                 func() time.Time
	random              io.Reader
	store               sessions.Store
	lineage             sessions.LineageStore
	persistent          sessions.PersistentStore
	tmux                tmuxRuntime
	historySize         func() (int64, error)
	captureCodexSession func(int64, string) (string, error)
	remoteAliases       map[string]string
	inventoryPath       string
	sshBinary           string
	launcherDir         string
	ensureSupervisor    func(context.Context, string, string, appSupervisorLaunch) (appserver.Reply, error)
	setupError          error
}

func productionDependencies() dependencies {
	deps := dependencies{
		hostname:            os.Hostname,
		stat:                os.Stat,
		loadInventory:       routing.LoadInventory,
		lookupBinary:        exec.LookPath,
		runProcess:          sessions.ExecRunner{},
		uid:                 os.Getuid,
		workingDirectory:    os.Getwd,
		now:                 time.Now,
		random:              rand.Reader,
		store:               legacy.NewMapStore(legacy.CurrentMapPaths(codexRoot)),
		tmux:                sessions.TmuxClient{},
		historySize:         codexHistorySize,
		captureCodexSession: captureCodexHistorySession,
	}
	home, err := os.UserHomeDir()
	if err != nil {
		deps.setupError = fmt.Errorf("resolve user home: %w", err)
		return deps
	}
	paths, err := config.ResolvePaths(runtime.GOOS, home, map[string]string{
		"XDG_CONFIG_HOME": os.Getenv("XDG_CONFIG_HOME"),
		"XDG_STATE_HOME":  os.Getenv("XDG_STATE_HOME"),
		"XDG_CACHE_HOME":  os.Getenv("XDG_CACHE_HOME"),
	})
	if err != nil {
		deps.setupError = fmt.Errorf("resolve run_ai paths: %w", err)
		return deps
	}
	deps.lineage = sessions.FileLineageStore{Directory: filepath.Join(paths.UserState, "sessions")}
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(paths.UserState, "persistent")}
	deps.inventoryPath = os.Getenv("RUN_AI_INVENTORY_PATH")
	deps.sshBinary = os.Getenv("RUN_AI_SSH_PATH")
	if deps.sshBinary == "" {
		deps.sshBinary = "/usr/bin/ssh"
	}
	deps.launcherDir = os.Getenv("RUN_AI_LAUNCHER_DIR")
	if deps.launcherDir == "" {
		deps.launcherDir = "/usr/local/lib/run-ai"
	}
	if raw := os.Getenv("RUN_AI_REMOTE_ALIASES"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &deps.remoteAliases)
	}
	return deps
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "app-supervisor" {
		if len(args) > 2 {
			fmt.Fprintln(stderr, "run_ai app-supervisor: accepts at most one thread ID")
			return exitUsage
		}
		thread := os.Getenv("RUN_AI_APP_THREAD")
		if len(args) > 1 {
			thread = args[1]
		}
		if thread != "" {
			if err := sessions.ValidateID(thread); err != nil {
				fmt.Fprintf(stderr, "run_ai app-supervisor: invalid thread ID: %v\n", err)
				return exitUsage
			}
		}
		if err := appserver.RunDaemon(context.Background(), appserver.DefaultSocket(), thread); err != nil {
			fmt.Fprintln(stderr, err)
			return exitSoftware
		}
		return exitOK
	}
	if len(args) > 0 && args[0] == "continuation" {
		return runContinuation(args[1:], stdout, stderr)
	}
	return runWith(context.Background(), args, os.Stdin, stdout, stderr, productionDependencies())
}

func runWith(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) int {
	if len(args) > 0 && args[0] == "goal" {
		return runGoalCommand(args[1:], stdout, stderr)
	}
	request, err := cli.Parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "run_ai: %v\n\n%s", err, cli.Help)
		return exitUsage
	}
	if request.Help {
		fmt.Fprint(stdout, cli.Help)
		return exitOK
	}
	if request.ListSessions {
		if deps.setupError != nil {
			fmt.Fprintf(stderr, "run_ai: %v\n", deps.setupError)
			return exitUnavailable
		}
		if deps.lineage == nil {
			fmt.Fprintln(stderr, "run_ai: session lineage store is unavailable")
			return exitUnavailable
		}
		if err := writeUnifiedSessions(stdout, deps); err != nil {
			fmt.Fprintf(stderr, "run_ai: write session lineages: %v\n", err)
			return exitUnavailable
		}
		return exitOK
	}
	if request.TerminateSessionID != "" {
		if deps.setupError != nil {
			fmt.Fprintf(stderr, "run_ai: %v\n", deps.setupError)
			return exitUnavailable
		}
		if err := terminateExactSession(ctx, request.TerminateSessionID, deps); err != nil {
			fmt.Fprintf(stderr, "run_ai: terminate session: %v\n", err)
			return exitUnavailable
		}
		fmt.Fprintf(stdout, "terminated session %s; durable metadata retained\n", request.TerminateSessionID)
		return exitOK
	}
	if request.Explain {
		if err := cli.WriteExplanation(stdout, request); err != nil {
			fmt.Fprintf(stderr, "run_ai: %v\n", err)
			return exitUnavailable
		}
		return exitOK
	}
	// The historical environment-only app-server entry point is intentionally
	// disjoint from normal CLI requests.  It must not bypass workflow or access
	// mode guards by silently discarding flags.
	if os.Getenv("RUN_AI_APP_SERVER") == "1" {
		if request.ProviderExplicit || request.Session.NewSession || request.Session.ResumeID != "" || request.Session.Persistent || request.Workflow.Kind != workflows.KindNone || request.Workflow.Team != nil || request.Model.Track != providers.ModelConfigured || request.AccessMode != permissions.ModeFull {
			fmt.Fprintln(stderr, "run_ai: RUN_AI_APP_SERVER=1 is a low-level compatibility entry point and cannot be combined with launcher options; use --codex --persistent")
			return exitUsage
		}
		fmt.Fprintln(stderr, "run_ai: WARNING [FULL_ACCESS]: low-level app-server mode has full Codex permissions")
		if err := runAppServerMode(ctx, stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "run_ai app-server: %v\n", err)
			return exitSoftware
		}
		return exitOK
	}
	if request.Workflow.Kind == workflows.KindMonitoring || request.Workflow.Kind == workflows.KindSpam {
		fmt.Fprintf(stderr, "run_ai: %s workflow is plan-only in this build; inspect it with --explain\n", request.Workflow.Kind)
		return exitUnavailable
	}
	if request.AccessMode == permissions.ModeReadOnly {
		fmt.Fprintln(stderr, "run_ai: --readonly launch is blocked until the platform hard-readonly adapter is linked; --explain is available")
		return exitUnavailable
	}
	if request.AccessMode == permissions.ModeConfirm {
		fmt.Fprintln(stderr, "run_ai: --confirm launch is blocked until a hard per-action confirmation adapter is linked; --explain is available")
		return exitUnavailable
	}
	// Access mode is a security boundary, not merely a provider preference.
	// Present the canonical profile warning immediately before any executable
	// path (including legacy remote delegation).
	fmt.Fprintf(stderr, "run_ai: WARNING [%s]: %s\n", request.Workflow.WarningCode, request.Workflow.Warning)
	if err := execute(ctx, request, stdin, stdout, stderr, deps); err != nil {
		fmt.Fprintf(stderr, "run_ai: %v\n", err)
		return exitSoftware
	}
	return exitOK
}

// runAppServerMode is opt-in. The terminal is an IPC client only: it never
// starts or owns a Codex process, so its exit cannot terminate an active turn.
func runAppServerMode(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	socket := appserver.DefaultSocket()
	threadID := os.Getenv("RUN_AI_APP_THREAD")
	status, err := ensureAppSupervisor(ctx, socket, threadID, appSupervisorLaunch{CWD: os.Getenv("RUN_AI_APP_CWD"), Model: os.Getenv("RUN_AI_APP_MODEL")})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "run_ai: app-server daemon thread=%s (terminal is reconnectable)\n", status.ThreadID)
	return runAppServerTerminal(ctx, socket, stdin, stdout)
}

type appSupervisorLaunch struct {
	CWD            string
	Model          string
	StateDirectory string
}

// newAppSupervisorCommand is a seam for lifecycle tests.  The detached
// process is deliberately not tied to the terminal context once it has
// successfully published its socket: /quit must leave a healthy persistent
// supervisor running.
var newAppSupervisorCommand = func(executable, threadID string) *exec.Cmd {
	return exec.Command(executable, "app-supervisor", threadID)
}

func ensureAppSupervisor(ctx context.Context, socket, threadID string, launch appSupervisorLaunch) (appserver.Reply, error) {
	status, err := appserver.Send(ctx, socket, appserver.IPCCommand{Op: "status"})
	if err == nil {
		if threadID != "" && status.ThreadID != threadID {
			return appserver.Reply{}, fmt.Errorf("app-server daemon is bound to thread %s, not %s", status.ThreadID, threadID)
		}
		return status, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return appserver.Reply{}, fmt.Errorf("locate app-server supervisor executable: %w", err)
	}
	cmd := newAppSupervisorCommand(executable, threadID)
	cmd.Env = append(os.Environ(), "RUN_AI_GOAL_STATE="+filepath.Dir(socket), "RUN_AI_APP_SOCKET="+socket)
	if launch.CWD != "" {
		if !filepath.IsAbs(launch.CWD) {
			return appserver.Reply{}, errors.New("persistent working directory must be absolute")
		}
		cmd.Env = append(cmd.Env, "RUN_AI_APP_CWD="+launch.CWD)
	}
	if launch.Model != "" {
		cmd.Env = append(cmd.Env, "RUN_AI_APP_MODEL="+launch.Model)
	}
	if launch.StateDirectory != "" {
		if !filepath.IsAbs(launch.StateDirectory) {
			return appserver.Reply{}, errors.New("persistent state directory must be absolute")
		}
		cmd.Env = append(cmd.Env, "RUN_AI_APP_JOURNAL_STATE="+launch.StateDirectory)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logDirectory := filepath.Dir(socket)
	if launch.StateDirectory != "" {
		logDirectory = launch.StateDirectory
	}
	logPath := filepath.Join(logDirectory, "app-supervisor.log")
	if err := os.MkdirAll(logDirectory, 0700); err != nil {
		return appserver.Reply{}, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return appserver.Reply{}, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return appserver.Reply{}, fmt.Errorf("start app-server supervisor: %w", err)
	}
	// A supervisor is intentionally detached only after it is reachable.  If
	// initialization times out, the context is cancelled, or the daemon proves
	// to be bound to the wrong thread, this launcher still owns the child and
	// must reap its process group instead of leaving an unusable supervisor
	// behind.
	keepSupervisor := false
	defer func() {
		if !keepSupervisor {
			stopAppSupervisor(cmd)
		}
	}()
	if err := logFile.Close(); err != nil {
		return appserver.Reply{}, err
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		status, err = appserver.Send(ctx, socket, appserver.IPCCommand{Op: "status"})
		if err == nil {
			if threadID != "" && status.ThreadID != threadID {
				return appserver.Reply{}, fmt.Errorf("app-server daemon is bound to thread %s, not %s", status.ThreadID, threadID)
			}
			if err := cmd.Process.Release(); err != nil {
				return appserver.Reply{}, fmt.Errorf("detach app-server supervisor: %w", err)
			}
			keepSupervisor = true
			return status, nil
		}
		select {
		case <-ctx.Done():
			return appserver.Reply{}, ctx.Err()
		case <-deadline.C:
			return appserver.Reply{}, fmt.Errorf("app-server supervisor did not become ready: %w", err)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// stopAppSupervisor stops only the process group created by ensureAppSupervisor.
// The child is the session leader (Setsid), so a negative PID cannot reach the
// launcher or another session.  Waiting is important: killing without Wait
// leaves a zombie until the terminal process exits, which made failed startup
// tests accumulate orphaned app-supervisors.
func stopAppSupervisor(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func runAppServerTerminal(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	var outMu sync.Mutex
	go watchAppServerEvents(watchCtx, socket, out, &outMu)
	const maxPromptFileBytes = 1 << 20
	var pasted []string
	var pastedBytes int
	var pasteTooLarge bool
	pasteMode := false
	reportCommandError := func(err error) {
		outMu.Lock()
		fmt.Fprintf(out, "[command error: %v; client remains connected]\n", err)
		outMu.Unlock()
	}
	submit := func(text string) {
		if text == "" {
			reportCommandError(errors.New("empty prompt"))
			return
		}
		reply, sendErr := appserver.Send(ctx, socket, appserver.IPCCommand{Op: "prompt", Text: text})
		if sendErr != nil {
			reportCommandError(sendErr)
			return
		}
		outMu.Lock()
		fmt.Fprintf(out, "[prompt queued: instruction=%d]\n", reply.InstructionID)
		outMu.Unlock()
	}
	readPromptFile := func(path string) (string, error) {
		if !filepath.IsAbs(path) {
			return "", errors.New("prompt file path must be absolute")
		}
		// O_NOFOLLOW binds every validation below to the opened descriptor,
		// rather than to a pathname an attacker could swap after Lstat.
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return "", err
		}
		file := os.NewFile(uintptr(fd), path)
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("prompt file must be a regular file")
		}
		if info.Size() > maxPromptFileBytes {
			return "", fmt.Errorf("prompt file exceeds %d-byte limit", maxPromptFileBytes)
		}
		data, err := io.ReadAll(io.LimitReader(file, maxPromptFileBytes+1))
		if err != nil {
			return "", err
		}
		if len(data) > maxPromptFileBytes {
			return "", fmt.Errorf("prompt file exceeds %d-byte limit", maxPromptFileBytes)
		}
		return string(data), nil
	}
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if pasteMode {
			if line == "/endpaste" {
				pasteMode = false
				if pasteTooLarge {
					reportCommandError(fmt.Errorf("paste exceeds %d-byte limit", maxPromptFileBytes))
				} else {
					submit(strings.Join(pasted, "\n"))
				}
				pasted = nil
				pastedBytes = 0
				pasteTooLarge = false
			} else {
				pastedBytes += len(line) + 1
				if pastedBytes > maxPromptFileBytes {
					pasteTooLarge = true
					pasted = nil
				} else if !pasteTooLarge {
					pasted = append(pasted, line)
				}
			}
			if err == io.EOF {
				if pasteMode {
					reportCommandError(errors.New("paste discarded: terminate it with /endpaste"))
				}
				return nil
			}
			if err != nil {
				return err
			}
			continue
		}
		if line == "/quit" {
			return nil
		}
		if line == "/paste" {
			pasteMode = true
			pasted = nil
			pastedBytes = 0
			pasteTooLarge = false
			outMu.Lock()
			fmt.Fprintln(out, "[paste mode: enter /endpaste on its own line to submit one atomic prompt]")
			outMu.Unlock()
			continue
		}
		if strings.HasPrefix(line, "/prompt-file ") {
			text, fileErr := readPromptFile(strings.TrimSpace(strings.TrimPrefix(line, "/prompt-file ")))
			if fileErr != nil {
				reportCommandError(fileErr)
			} else {
				submit(text)
			}
			continue
		}
		if line == "/status" || line == "/interrupt" || line == "/pause" || line == "/resume" || line == "/terminate" || line == "/shutdown" {
			reply, sendErr := appserver.Send(ctx, socket, appserver.IPCCommand{Op: strings.TrimPrefix(line, "/")})
			if sendErr != nil {
				reportCommandError(sendErr)
				continue
			}
			if line == "/status" {
				outMu.Lock()
				fmt.Fprintf(out, "thread=%s state=%s active=%t active_turn=%s checkpoint=%s executed_turns=%d queued_instructions=%d active_instructions=%d approvals=%d autonomous_turns=%d/%d turn_timeout=%s total_timeout=%s retries=%d/%d limit_reason=%s\n", reply.ThreadID, reply.State, reply.Active, reply.Execution.ActiveTurnID, reply.Checkpoint, reply.ExecutedTurns, reply.QueuedInstructions, reply.ActiveInstructions, len(reply.PendingApprovals), reply.Execution.AutonomousTurns, reply.Limits.MaxAutonomousTurns, reply.Limits.TurnTimeout, reply.Limits.TotalTimeout, reply.Execution.RetryAttempts, reply.Limits.MaxRetries, reply.LimitReason)
				outMu.Unlock()
			}
			if line == "/shutdown" {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "/approve ") || strings.HasPrefix(line, "/deny ") {
			approved := strings.HasPrefix(line, "/approve ")
			id := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "/approve"), "/deny"))
			if id == "" {
				reportCommandError(errors.New("approval id is required"))
				continue
			}
			rawID := json.RawMessage(id)
			if !json.Valid(rawID) {
				encoded, _ := json.Marshal(id)
				rawID = encoded
			}
			if _, sendErr := appserver.Send(ctx, socket, appserver.IPCCommand{Op: "approve", ID: rawID, Approved: &approved}); sendErr != nil {
				reportCommandError(sendErr)
			}
			continue
		}
		if line != "" {
			submit(line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// watchAppServerEvents lets an attached terminal see provider messages and an
// approval request while stdin remains available for /approve or /deny.  A
// disconnect only stops this watcher; events and approval records remain in
// the supervisor journal for the next terminal.
func watchAppServerEvents(ctx context.Context, socket string, out io.Writer, outMu *sync.Mutex) {
	var after uint64
	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reply, err := appserver.Send(ctx, socket, appserver.IPCCommand{Op: "events", After: after})
			if err != nil {
				continue
			}
			for _, event := range reply.Events {
				if event.Sequence > after {
					after = event.Sequence
				}
				outMu.Lock()
				switch event.Type {
				case "agent_message":
					var p struct {
						Delta string `json:"delta"`
						Text  string `json:"text"`
					}
					_ = json.Unmarshal(event.Data, &p)
					if p.Delta != "" {
						fmt.Fprint(out, p.Delta)
					} else if p.Text != "" {
						fmt.Fprint(out, p.Text)
					}
				case "approval_required":
					var p appserver.PendingApproval
					_ = json.Unmarshal(event.Data, &p)
					fmt.Fprintf(out, "\n[approval required: %s id=%s; use /approve ID or /deny ID]\n", p.Method, string(p.ID))
				case "turn_completed":
					fmt.Fprintln(out, "\n[turn completed]")
				case "prompt_queued", "turn_starting", "turn_started", "turn_interrupt_requested", "turn_interrupt_acknowledged", "turn_interrupted":
					fmt.Fprintf(out, "\n[turn: %s]\n", event.Message)
				case "tool_event":
					fmt.Fprintf(out, "\n[tool event: %s]\n", event.Message)
				case "turn_not_completed", "journal_error", "interrupt_retry":
					fmt.Fprintf(out, "\n[turn error: %s]\n", event.Message)
				case "resumed":
					fmt.Fprintf(out, "\n[checkpoint: %s]\n", event.Message)
				case "restart_reconciliation_required":
					fmt.Fprintf(out, "\n[reconciliation required: %s; use /resume]\n", event.Message)
				case "execution_limit_reached", "interrupt_retry_exhausted":
					fmt.Fprintf(out, "\n[execution paused: %s]\n", event.Message)
				}
				outMu.Unlock()
			}
		}
	}
}

// goal commands are an opt-in durable control plane; normal run_ai behavior is unchanged.
func runGoalCommand(args []string, stdout, stderr io.Writer) int {
	root := os.Getenv("RUN_AI_GOAL_STATE")
	if root == "" {
		root = "/root/.local/state/run-ai/goals"
	}
	s := goals.Store{Root: root, MaxAttempts: envInt("RUN_AI_GOAL_MAX_ATTEMPTS", 8), RetryDelay: time.Minute}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: run_ai goal create|checkpoint|complete|blocked|failed|pause|resume|terminate|list|worker ...")
		return exitUsage
	}
	if args[0] == "list" {
		xs, e := s.List()
		if e != nil {
			fmt.Fprintln(stderr, e)
			return exitUnavailable
		}
		if e := json.NewEncoder(stdout).Encode(xs); e != nil {
			fmt.Fprintln(stderr, e)
			return exitUnavailable
		}
		return exitOK
	}
	if args[0] == "worker" {
		return runGoalWorker(s, stdout, stderr)
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "goal command requires id")
		return exitUsage
	}
	id := args[1]
	g, e := s.Load(id)
	if args[0] == "create" {
		if e == nil {
			fmt.Fprintln(stderr, "goal already exists")
			return exitUsage
		}
		text := ""
		if len(args) > 2 {
			text = strings.Join(args[2:], " ")
		}
		e = s.Save(goals.Goal{ID: id, Text: text, State: goals.Running})
		if e == nil {
			fmt.Fprintln(stdout, id)
		}
		if e != nil {
			fmt.Fprintln(stderr, e)
			return exitUnavailable
		}
		return exitOK
	}
	if args[0] == "bind" {
		if len(args) != 3 || args[2] == "" {
			fmt.Fprintln(stderr, "usage: run_ai goal bind ID CODEX_SESSION_ID")
			return exitUsage
		}
		if e != nil {
			fmt.Fprintln(stderr, e)
			return exitUnavailable
		}
		g.SessionID = args[2]
		g.Provider = "codex"
		if e = s.Save(g); e != nil {
			fmt.Fprintln(stderr, e)
			return exitUnavailable
		}
		return exitOK
	}
	if e != nil {
		fmt.Fprintln(stderr, e)
		return exitUnavailable
	}
	switch args[0] {
	case "checkpoint":
		g.State = goals.Checkpoint
		g.Checkpoint = strings.Join(args[2:], " ")
	case "complete":
		g.State = goals.Completed
	case "blocked":
		g.State = goals.Blocked
	case "failed":
		g.State = goals.Failed
	case "pause":
		g.State = goals.Blocked
		g.Checkpoint = "paused_by_operator"
	case "resume":
		g.State = goals.Checkpoint
		g.Checkpoint = "resume_requested"
		if len(args) > 2 {
			g.Checkpoint = strings.Join(args[2:], " ")
		}
	case "terminate":
		g.State = goals.Failed
		g.Checkpoint = "terminated_by_operator"
	default:
		fmt.Fprintln(stderr, "unknown goal command")
		return exitUsage
	}
	if e = s.Save(g); e != nil {
		fmt.Fprintln(stderr, e)
		return exitUnavailable
	}
	return exitOK
}

// worker resumes unfinished goals through a fixed external handler. The
// handler must be an absolute executable configured by RUN_AI_GOAL_WORKER and
// return JSON: {"state":"checkpoint|completed|blocked|failed","checkpoint":"..."}.
// No shell is used and the worker is bounded by RUN_AI_GOAL_WORKER_CYCLES.
func runGoalWorker(s goals.Store, stdout, stderr io.Writer) int {
	bin := os.Getenv("RUN_AI_GOAL_WORKER")
	if bin != "" && !filepath.IsAbs(bin) {
		fmt.Fprintln(stderr, "RUN_AI_GOAL_WORKER must be an absolute executable")
		return exitUnavailable
	}
	if bin == "" && os.Getenv("RUN_AI_GOAL_CODEX") != "1" {
		fmt.Fprintln(stderr, "set RUN_AI_GOAL_WORKER or RUN_AI_GOAL_CODEX=1")
		return exitUnavailable
	}
	lock, e := s.Lock()
	if e != nil {
		fmt.Fprintln(stderr, e)
		return exitUnavailable
	}
	defer lock.Close()
	cycles := envInt("RUN_AI_GOAL_WORKER_CYCLES", 1)
	if cycles > 32 {
		cycles = 32
	}
	for n := 0; n < cycles; n++ {
		xs, e := s.List()
		if e != nil {
			fmt.Fprintln(stderr, e)
			return exitUnavailable
		}
		progressed := false
		continueNeeded := false
		for _, candidate := range xs {
			if candidate.State == goals.Completed || candidate.State == goals.Blocked || candidate.State == goals.Failed {
				continue
			}
			g, e := s.Claim(candidate.ID)
			if e != nil {
				fmt.Fprintln(stderr, e)
				return exitUnavailable
			}
			progressed = true
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(envInt("RUN_AI_GOAL_WORKER_TIMEOUT", 900))*time.Second)
			var cmd *exec.Cmd
			if bin != "" {
				cmd = exec.CommandContext(ctx, bin, "--goal-id", g.ID, "--checkpoint", g.Checkpoint)
			} else {
				codexBin, codexLookupErr := exec.LookPath("codex")
				if codexLookupErr != nil {
					cancel()
					g.State = goals.Failed
					g.Checkpoint = "codex_binary_not_found"
					_ = s.Save(g)
					continue
				}
				prompt := "Continue the unfinished goal autonomously. Goal: " + g.Text + ". Preserve all prior context and agent work. At the end output exactly one marker on its own line: RUN_AI_GOAL_STATE: completed if the entire goal is finished, RUN_AI_GOAL_STATE: blocked if user input or approval is required, otherwise RUN_AI_GOAL_STATE: checkpoint. Checkpoint: " + g.Checkpoint
				if g.SessionID != "" && (g.TmuxName == "" || !tmuxIsActive(g.TmuxName)) {
					cmd = exec.CommandContext(ctx, codexBin, "exec", "resume", "--json", g.SessionID, prompt)
				} else {
					// Never resume the session currently owned by an interactive TUI.
					// A fresh exec session is the safe continuation boundary.
					cmd = exec.CommandContext(ctx, codexBin, "exec", "--json", "-C", codexRoot, prompt)
				}
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, runErr := cmd.Output()
			if ctx.Err() != nil && cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			cancel()
			if runErr != nil {
				g.State = goals.Failed
				g.Checkpoint = "worker_failed:" + runErr.Error()
			} else if bin == "" {
				g.State, g.Checkpoint = parseCodexOutcome(out)
				if id := parseCodexThreadID(out); id != "" {
					g.SessionID = id
				}
				if g.State == "" {
					g.State = goals.Failed
					g.Checkpoint = "codex_missing_terminal_state_marker"
				}
			} else {
				var result struct {
					State      goals.State `json:"state"`
					Checkpoint string      `json:"checkpoint"`
				}
				if json.Unmarshal(out, &result) != nil || result.State == "" {
					g.State = goals.Failed
					g.Checkpoint = "worker_invalid_result"
				} else {
					g.State = result.State
					g.Checkpoint = result.Checkpoint
				}
			}
			if e = s.Save(g); e != nil {
				fmt.Fprintln(stderr, e)
				return exitUnavailable
			}
			if g.State == goals.Checkpoint && g.Attempts >= g.MaxAttempts {
				g.State = goals.Blocked
				g.Checkpoint = "retry_limit_exceeded"
				_ = s.Save(g)
			}
			if g.State == goals.Checkpoint && g.Attempts < g.MaxAttempts {
				continueNeeded = true
			}
			fmt.Fprintf(stdout, "goal=%s session=%s turn=%d state=%s\n", g.ID, g.SessionID, g.Attempts, g.State)
		}
		if !progressed {
			return exitOK
		}
		if n+1 < cycles {
			time.Sleep(s.RetryDelay)
		}
		if continueNeeded && os.Getenv("RUN_AI_GOAL_AUTOCONTINUE") == "1" {
			_ = spawnGoalWorkerProcess(s.Root)
		}
	}
	return exitOK
}

// ensurePersistentExecGoal creates a controller-mode goal before tmux attach.
// It intentionally has no provider session ID: the worker will create its own
// Codex exec session and record the thread.started ID from structured JSON.
func ensurePersistentExecGoal(tmuxName, prompt string) error {
	root := os.Getenv("RUN_AI_GOAL_STATE")
	if root == "" {
		root = "/root/.local/state/run-ai/goals"
	}
	s := goals.Store{Root: root}
	id := "exec-" + strings.NewReplacer("/", "-", ":", "-").Replace(tmuxName)
	if _, err := s.Load(id); err == nil {
		return nil
	}
	return s.Save(goals.Goal{ID: id, Text: prompt, State: goals.Checkpoint, Provider: "codex", TmuxName: tmuxName})
}

func spawnGoalWorkerProcess(root string) error {
	cmd := exec.Command(os.Args[0], "goal", "worker")
	cmd.Env = append(os.Environ(), "RUN_AI_GOAL_CODEX=1", "RUN_AI_GOAL_AUTOCONTINUE=1", "RUN_AI_GOAL_STATE="+root)
	logFile, err := os.OpenFile(filepath.Join(root, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func tmuxIsActive(name string) bool {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return false
	}
	return exec.Command(tmux, "has-session", "-t", name).Run() == nil
}

func parseCodexOutcome(out []byte) (goals.State, string) {
	text := string(out)
	for _, line := range strings.Split(text, "\n") {
		for _, marker := range []string{"RUN_AI_GOAL_STATE: completed", "RUN_AI_GOAL_STATE: checkpoint", "RUN_AI_GOAL_STATE: blocked"} {
			if strings.Contains(line, marker) {
				switch {
				case strings.Contains(marker, "completed"):
					return goals.Completed, marker
				case strings.Contains(marker, "blocked"):
					return goals.Blocked, marker
				default:
					return goals.Checkpoint, marker
				}
			}
		}
	}
	return "", ""
}

func parseCodexThreadID(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "thread.started" && ev.ThreadID != "" {
			return ev.ThreadID
		}
	}
	return ""
}
func envInt(k string, fallback int) int {
	if v, e := strconv.Atoi(os.Getenv(k)); e == nil && v > 0 {
		return v
	}
	return fallback
}

type launchTarget struct {
	key         string
	cwd         string
	remoteAlias string
}

// sessionListEntry is deliberately summary-only: a supervisor journal can
// contain prompts, agent output and approval parameters, none of which belong
// in the normal session inventory.
type sessionListEntry struct {
	Type               string `json:"type"`
	Provider           string `json:"provider"`
	SessionID          string `json:"session_id"`
	TmuxSessionID      string `json:"tmux_session_id,omitempty"`
	TmuxName           string `json:"tmux_name,omitempty"`
	CodexThreadID      string `json:"codex_thread_id,omitempty"`
	WorkingDirectory   string `json:"working_directory,omitempty"`
	Model              string `json:"model,omitempty"`
	TeamMode           string `json:"team_mode"`
	State              string `json:"state"`
	Turns              int    `json:"turns"`
	ExecutedTurns      int    `json:"executed_turns"`
	QueuedInstructions int    `json:"queued_instructions"`
	LastActivity       string `json:"last_activity,omitempty"`
	SupervisorActive   bool   `json:"supervisor_active"`
	ResumeCapability   string `json:"resume_capability"`
	Blocked            bool   `json:"blocked"`
	Paused             bool   `json:"paused"`
}

type sessionListDocument struct {
	Schema   string             `json:"schema"`
	Sessions []sessionListEntry `json:"sessions"`
}

// terminateExactSession deliberately never removes a lineage or persistent
// record. It first binds the requested durable ID to a currently observed,
// provider-marked immutable runtime identity, then stops that identity only.
func terminateExactSession(ctx context.Context, id string, deps dependencies) error {
	// Persistent IDs are a distinct durable namespace.  Do not probe the
	// persistent store for a classic tmux lineage: apart from making classic
	// termination depend on unrelated persistent-store health, that would blur
	// the exact identity boundary between a tmux name and a supervisor ID.
	if strings.HasPrefix(id, "persistent-") {
		if deps.persistent.Directory == "" {
			return errors.New("no exact durable persistent session ID was found")
		}
		persistent, err := deps.persistent.Lookup(id)
		if err == nil {
			if persistent.ThreadID == "" {
				return errors.New("persistent session has no bound Codex thread")
			}
			socket := persistent.SocketPath
			if socket == "" {
				stateDir, stateErr := deps.persistent.StateDirectory(id)
				if stateErr != nil {
					return stateErr
				}
				socket = filepath.Join(stateDir, "app-supervisor.sock")
			}
			status, sendErr := appserver.Send(ctx, socket, appserver.IPCCommand{Op: "status"})
			if sendErr != nil {
				return fmt.Errorf("verify persistent supervisor: %w", sendErr)
			}
			if status.ThreadID != persistent.ThreadID {
				return errors.New("persistent supervisor thread does not match sealed metadata")
			}
			if _, sendErr = appserver.Send(ctx, socket, appserver.IPCCommand{Op: "shutdown"}); sendErr != nil {
				return fmt.Errorf("shutdown persistent supervisor: %w", sendErr)
			}
			return nil
		}
		if !errors.Is(err, sessions.ErrPersistentNotFound) {
			return err
		}
		return errors.New("no exact durable persistent session ID was found")
	}
	lineages, err := deps.lineage.List()
	if err != nil {
		return err
	}
	var lineage *sessions.Lineage
	for index := range lineages {
		if lineages[index].TmuxName == id {
			lineage = &lineages[index]
			break
		}
	}
	if lineage == nil {
		return errors.New("no exact durable session ID was found")
	}
	runtime, ok := deps.tmux.(tmuxStatusRuntime)
	if !ok {
		return errors.New("tmux status runtime is unavailable")
	}
	terminator, ok := deps.tmux.(tmuxTerminator)
	if !ok {
		return errors.New("tmux termination runtime is unavailable")
	}
	status, err := runtime.Status(ctx, lineage.TmuxName)
	if err != nil {
		return err
	}
	if !status.Exists || !status.Live || status.Info.SessionID == "" {
		return errors.New("classic tmux session is already absent")
	}
	if status.Info.SessionName != lineage.TmuxName || status.Info.UID != deps.uid() || status.Info.PaneDead || status.Info.Manifest.Digest == "" || status.Info.Manifest.Digest != lineage.ManifestDigest || status.Info.Manifest.Provider != lineage.Provider {
		return errors.New("live tmux session does not match durable lineage")
	}
	return terminator.Terminate(ctx, status.Info.SessionID)
}

func writeUnifiedSessions(out io.Writer, deps dependencies) error {
	classic, err := deps.lineage.List()
	if err != nil {
		return fmt.Errorf("list classic lineages: %w", err)
	}
	var persistent []sessions.PersistentSession
	var staleIDs []string
	if deps.persistent.Directory != "" {
		persistent, staleIDs, err = deps.persistent.ListWithStale()
		if err != nil {
			return fmt.Errorf("list persistent sessions: %w", err)
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	entries := make([]sessionListEntry, 0, len(classic)+len(persistent)+len(staleIDs))
	for _, value := range classic {
		// Do not reuse durable lineage State here: it denotes the durable
		// creation protocol, not current liveness. An old tmux server may no
		// longer exist while its protected lineage must remain available for
		// forensic history and exact resume.
		entry := sessionListEntry{Type: "classic", Provider: value.Provider, SessionID: value.TmuxName, TmuxName: value.TmuxName, Model: "unknown", TeamMode: "unknown", State: "unknown", LastActivity: value.CreatedAt, ResumeCapability: value.ResumeCapability}
		if value.Provider == string(providers.Codex) {
			entry.CodexThreadID = value.ProviderSessionID
		}
		if runtime, ok := deps.tmux.(tmuxStatusRuntime); ok {
			status, statusErr := runtime.Status(context.Background(), value.TmuxName)
			if statusErr != nil {
				entry.State = "unavailable"
			} else if !status.Exists {
				entry.State = "stale"
			} else {
				entry.TmuxSessionID = status.Info.SessionID
				entry.SupervisorActive = status.Live
				if status.Live {
					entry.State = "active"
				} else {
					entry.State = "exited"
				}
				if !status.LastActivity.IsZero() {
					entry.LastActivity = status.LastActivity.UTC().Format(time.RFC3339Nano)
				}
			}
		}
		entries = append(entries, entry)
	}
	for _, value := range persistent {
		entry := sessionListEntry{Type: "persistent", Provider: value.Provider, SessionID: value.ID, CodexThreadID: value.ThreadID, WorkingDirectory: value.WorkingDirectory, Model: value.Model, TeamMode: "disabled", State: "unknown", LastActivity: value.UpdatedAt, ResumeCapability: value.ResumeCapability}
		stateDir, dirErr := deps.persistent.StateDirectory(value.ID)
		if dirErr == nil {
			journal, journalErr := (appserver.JournalStore{Path: filepath.Join(stateDir, "app-supervisor-state.json")}).Load(value.ThreadID)
			if journalErr == nil && journal.ThreadID != "" {
				entry.State = journal.State
				entry.ExecutedTurns, entry.QueuedInstructions, _ = journal.Counts()
				// turns is retained for compatibility, but now means executed
				// provider turns rather than the local instruction queue.
				entry.Turns = entry.ExecutedTurns
				entry.Blocked = journal.State == appserver.SupervisorBlocked
				entry.Paused = journal.State == appserver.SupervisorPaused
				entry.SupervisorActive = journal.State != appserver.SupervisorStopped && journal.State != appserver.SupervisorTerminated && journal.State != appserver.SupervisorCompleted
				if len(journal.Instructions) > 0 {
					entry.LastActivity = journal.Instructions[len(journal.Instructions)-1].UpdatedAt.UTC().Format(time.RFC3339Nano)
				}
			}
		}
		entries = append(entries, entry)
	}
	for _, id := range staleIDs {
		entries = append(entries, sessionListEntry{
			Type:             "stale_digest_mismatch",
			Provider:         "unknown",
			SessionID:        id,
			TeamMode:         "unknown",
			State:            "stale_digest_mismatch",
			ResumeCapability: "none",
		})
	}
	return encoder.Encode(sessionListDocument{Schema: "run_ai_sessions/v3", Sessions: entries})
}

func persistentSessionID(now time.Time, random io.Reader) (string, error) {
	nonce, err := sessionNonce(now, random)
	if err != nil {
		return "", err
	}
	return "persistent-" + nonce, nil
}

// persistentSocketPath keeps the AF_UNIX name below its platform limit even
// when XDG_STATE_HOME is deliberately nested (for example in an isolated E2E
// stage). The runtime directory is private and the sealed session metadata is
// the authoritative mapping back to it.
func persistentSocketPath(id string) (string, error) {
	if err := sessions.ValidateID(id); err != nil {
		return "", err
	}
	runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("run-ai-%d", os.Getuid()))
	if !filepath.IsAbs(runtimeDir) {
		return "", errors.New("temporary directory must be absolute")
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(runtimeDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return "", errors.New("persistent runtime directory is not private")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return "", errors.New("persistent runtime directory has unexpected owner")
	}
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(runtimeDir, hex.EncodeToString(digest[:12])+".sock"), nil
}

func executePersistent(ctx context.Context, request cli.Request, target launchTarget, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) error {
	if request.Provider != providers.Codex {
		return errors.New("--persistent is supported only with Codex")
	}
	if request.Workflow.Team != nil {
		return errors.New("--persistent --team is not supported because the app-server supervisor has no independent team-agent runtime")
	}
	if target.remoteAlias != "" {
		return errors.New("--persistent is not supported for remote --dev delegation")
	}
	if deps.persistent.Directory == "" {
		return errors.New("persistent session store is unavailable")
	}
	// Generic persistent sessions must inherit the caller's actual directory.
	// The historical classic launcher deliberately uses / for this target, but
	// reusing it here would let an isolated terminal silently give Codex a
	// broader filesystem context than the caller selected.
	persistentCWD := target.cwd
	if request.Workflow.Kind == workflows.KindNone {
		if deps.workingDirectory == nil {
			return errors.New("persistent working directory resolver is unavailable")
		}
		cwd, cwdErr := deps.workingDirectory()
		if cwdErr != nil {
			return fmt.Errorf("resolve persistent working directory: %w", cwdErr)
		}
		if !filepath.IsAbs(cwd) {
			return errors.New("persistent working directory must be absolute")
		}
		persistentCWD = filepath.Clean(cwd)
	}
	model, err := providers.ResolveModel(request.Provider, request.Model.Track, request.Model.Model)
	if err != nil {
		return fmt.Errorf("resolve persistent model: %w", err)
	}
	var session sessions.PersistentSession
	if request.Session.ResumeID != "" {
		session, err = deps.persistent.Lookup(request.Session.ResumeID)
		if err != nil {
			return fmt.Errorf("resume persistent session %q: %w", request.Session.ResumeID, err)
		}
		if session.Provider != string(providers.Codex) || session.WorkingDirectory != persistentCWD {
			return errors.New("persistent session does not match the requested Codex working directory")
		}
		if session.Model != model || session.ModelTrack != string(request.Model.Track) {
			return errors.New("persistent session does not match the requested model")
		}
		if session.WorkflowDigest != request.Workflow.PlanDigest {
			return errors.New("persistent session does not match the requested workflow plan")
		}
	} else {
		id, idErr := persistentSessionID(deps.now(), deps.random)
		if idErr != nil {
			return idErr
		}
		socketPath, socketErr := persistentSocketPath(id)
		if socketErr != nil {
			return socketErr
		}
		session, err = sessions.NewPersistentSession(id, string(providers.Codex), persistentCWD, socketPath, model, string(request.Model.Track), request.Workflow.PlanDigest, deps.now())
		if err != nil {
			return err
		}
		if err := deps.persistent.Create(session); err != nil {
			return fmt.Errorf("create persistent session: %w", err)
		}
	}
	stateDir, err := deps.persistent.StateDirectory(session.ID)
	if err != nil {
		return err
	}
	socket := session.SocketPath
	if socket == "" {
		socket = filepath.Join(stateDir, "app-supervisor.sock")
	} // compatibility with pre-runtime-socket metadata
	starter := deps.ensureSupervisor
	if starter == nil {
		starter = ensureAppSupervisor
	}
	status, err := starter(ctx, socket, session.ThreadID, appSupervisorLaunch{CWD: session.WorkingDirectory, Model: session.Model, StateDirectory: stateDir})
	if err != nil {
		return fmt.Errorf("start persistent Codex supervisor: %w", err)
	}
	if session.ThreadID == "" {
		session, err = deps.persistent.BindThread(session.ID, status.ThreadID, deps.now())
		if err != nil {
			return fmt.Errorf("bind persistent Codex thread: %w", err)
		}
	} else if status.ThreadID != session.ThreadID {
		return errors.New("persistent supervisor thread does not match sealed session metadata")
	}
	fmt.Fprintf(stderr, "run_ai: persistent session=%s thread=%s state=%s; /quit detaches, /shutdown stops this supervisor\n", session.ID, session.ThreadID, status.State)
	return runAppServerTerminal(ctx, socket, stdin, stdout)
}

func execute(ctx context.Context, request cli.Request, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) error {
	if deps.setupError != nil {
		return deps.setupError
	}
	if deps.hostname == nil || deps.stat == nil || deps.loadInventory == nil || deps.lookupBinary == nil || deps.runProcess == nil || deps.uid == nil ||
		deps.now == nil || deps.random == nil || deps.store == nil || deps.lineage == nil || deps.tmux == nil || deps.historySize == nil || deps.captureCodexSession == nil {
		return fmt.Errorf("execution dependencies are incomplete")
	}
	host, err := deps.hostname()
	if err != nil {
		return fmt.Errorf("read hostname: %w", err)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	target, err := resolveTarget(host, request, deps)
	if err != nil {
		return err
	}
	if request.Session.Persistent {
		return executePersistent(ctx, request, target, stdin, stdout, stderr, deps)
	}
	if target.remoteAlias != "" {
		return executeRemote(ctx, request, target, stdin, stdout, stderr, deps)
	}
	selection, err := sessions.Select(ctx, deps.store, request.Provider, target.key, sessions.SelectOptions{
		NewSession: request.Session.NewSession, ResumeID: request.Session.ResumeID,
	})
	if err != nil {
		return fmt.Errorf("select session: %w", err)
	}
	manifest, err := buildManifest(request, target)
	if err != nil {
		return err
	}
	effectiveExplicitNew := request.Session.NewSession
	tmuxName := ""
	if request.Session.ResumeID != "" {
		found, findErr := deps.lineage.Find(string(request.Provider), target.key, request.Session.ResumeID, manifest.Digest)
		if findErr == nil {
			tmuxName = found.TmuxName
			effectiveExplicitNew = found.ExplicitNew
		} else if !errors.Is(findErr, sessions.ErrLineageNotFound) {
			return fmt.Errorf("find exact session lineage: %w", findErr)
		} else if request.Workflow.Team != nil {
			// Team prompts are privileged provenance, not decoration.  Never
			// manufacture a new team lineage around an arbitrary provider ID.
			return errors.New("refuse team resume without a matching exact durable team lineage")
		}
	}
	// A durable lineage is the authority for reconnecting an already-live
	// original TUI.  The old map contains at most one provider ID and cannot
	// represent multiple isolated --new-session launches, so consult it only
	// after checking exact, manifest-bound active lineages.  This is purposely
	// limited to local classic launches: remote launchers own their own state
	// and --persistent is a separate app-server transport.
	if request.Session.ResumeID == "" && !request.Session.NewSession {
		candidate, attachInput, selectErr := chooseLiveLineage(ctx, deps, request, target, manifest, stdin, stdout, stderr)
		if selectErr != nil {
			return selectErr
		}
		if candidate != nil {
			tmuxName = candidate.TmuxName
			effectiveExplicitNew = candidate.ExplicitNew
			stdin = attachInput
		} else {
			// Preserve a live unmarked legacy session only when the historical
			// mapping supplies its exact provider identity.  A missing legacy
			// tmux session is not silently resumed: the documented default is a
			// fresh isolated session unless the operator selects --resume-id.
			legacyName, nameErr := sessions.LegacyTmuxName("", request.Provider, target.key, false, "")
			if nameErr != nil {
				return nameErr
			}
			legacyExists, existsErr := deps.tmux.Exists(ctx, legacyName)
			if existsErr != nil {
				return existsErr
			}
			if !legacyExists {
				selection.Mode = sessions.StartFresh
				selection.SessionID = ""
				selection.Mapped = false
				// A durable but no-longer-live lineage must remain as history,
				// never become an implicit provider resume. Give the automatic
				// fresh launch a new isolated tmux identity so it cannot collide
				// with the retained exact-resume record.
				lineages, listErr := deps.lineage.List()
				if listErr != nil {
					return fmt.Errorf("list stale session lineages: %w", listErr)
				}
				for _, lineage := range lineages {
					if lineage.State == "active" && lineage.Provider == string(request.Provider) && lineage.SessionKey == target.key && lineage.ManifestDigest == manifest.Digest {
						effectiveExplicitNew = true
						break
					}
				}
			}
		}
	}
	if tmuxName == "" {
		nonce := ""
		if effectiveExplicitNew {
			nonce, err = sessionNonce(deps.now(), deps.random)
			if err != nil {
				return err
			}
		}
		tmuxName, err = sessions.LegacyTmuxName("", request.Provider, target.key, effectiveExplicitNew, nonce)
		if err != nil {
			return fmt.Errorf("build tmux identity: %w", err)
		}
	}
	expected := sessions.Expectation{
		Manifest: manifest, UID: deps.uid(),
		AllowLegacy: !effectiveExplicitNew && request.AccessMode == permissions.ModeFull &&
			request.Model.Track == providers.ModelConfigured && (selection.Mapped || request.Session.ResumeID != ""),
	}
	exists, err := deps.tmux.Exists(ctx, tmuxName)
	if err != nil {
		return err
	}
	storedLineage, lineageErr := deps.lineage.Lookup(tmuxName)
	if lineageErr != nil && !errors.Is(lineageErr, sessions.ErrLineageNotFound) {
		return fmt.Errorf("load session lineage: %w", lineageErr)
	}
	if exists {
		info, err := deps.tmux.Validate(ctx, tmuxName, expected)
		if err != nil {
			return fmt.Errorf("refuse unverified existing tmux session %s: %w", tmuxName, err)
		}
		if info.Manifest.Digest != "" {
			if lineageErr != nil || storedLineage.State != "active" || validateLineageBinding(storedLineage, request, target, tmuxName, manifest, effectiveExplicitNew) != nil {
				return errors.New("refuse marked tmux session without matching active durable lineage")
			}
		} else if lineageErr == nil {
			return errors.New("refuse unmarked legacy tmux session shadowed by a durable run_ai lineage")
		}
		if os.Getenv("RUN_AI_PERSISTENT_EXEC") == "1" && request.Provider == providers.Codex {
			if err := ensurePersistentExecGoal(tmuxName, request.Workflow.Prompt); err != nil {
				return err
			}
			if err := startPersistentWorker(stderr); err != nil {
				return err
			}
		}
		err = attachRevalidated(ctx, deps.tmux, tmuxName, expected, info.SessionID, stdin, stdout, stderr)
		return err
	}

	spec := providers.MustLookup(request.Provider)
	binary, err := deps.lookupBinary(spec.DefaultBinary)
	if err != nil {
		return fmt.Errorf("provider binary %s not found: %w", spec.DefaultBinary, err)
	}
	resumeID := selection.SessionID
	newSessionID := ""
	if lineageErr == nil {
		if err := validateLineageBinding(storedLineage, request, target, tmuxName, manifest, effectiveExplicitNew); err != nil {
			return fmt.Errorf("existing lineage binding: %w", err)
		}
		if resumeID != "" && storedLineage.ProviderSessionID != "" && resumeID != storedLineage.ProviderSessionID {
			return errors.New("legacy mapping and durable lineage disagree on provider session ID")
		}
		if storedLineage.State == "active" {
			if storedLineage.ProviderSessionID == "" {
				return errors.New("active lineage is tmux_lifetime_only because this provider ID was not captured; history was not deleted, inspect 'run_ai sessions list' and start a separate --new-session")
			}
			resumeID = storedLineage.ProviderSessionID
		} else if request.Provider == providers.Claude {
			newSessionID = storedLineage.ProviderSessionID
		}
	}
	if lineageErr != nil && request.Provider == providers.Claude && selection.Mode == sessions.StartFresh {
		newSessionID, err = newUUID(deps.random)
		if err != nil {
			return err
		}
	}
	candidateID := resumeID
	if candidateID == "" {
		candidateID = newSessionID
	}
	intent, err := sessions.NewLineageIntent(tmuxName, string(request.Provider), target.key, candidateID, manifest.Digest, effectiveExplicitNew, deps.now())
	if err != nil {
		return fmt.Errorf("build private session lineage intent: %w", err)
	}
	prepared, err := deps.lineage.Prepare(intent)
	if err != nil {
		return fmt.Errorf("persist private session lineage intent: %w", err)
	}
	if err := validateLineageBinding(prepared, request, target, tmuxName, manifest, effectiveExplicitNew); err != nil {
		return fmt.Errorf("prepared lineage binding: %w", err)
	}
	if prepared.ProviderSessionID != candidateID {
		if selection.Mode == sessions.StartResume && selection.SessionID != prepared.ProviderSessionID {
			return errors.New("concurrent lineage conflicts with requested resume ID")
		}
		if prepared.State == "active" {
			resumeID, newSessionID = prepared.ProviderSessionID, ""
		} else if request.Provider == providers.Claude {
			resumeID, newSessionID = "", prepared.ProviderSessionID
		}
	}
	providerPrompt := request.Workflow.Prompt
	historyOffset := int64(-1)
	captureTag := ""
	if request.Provider == providers.Codex && resumeID == "" {
		if offset, captureErr := deps.historySize(); captureErr == nil {
			historyOffset = offset
			captureTag, err = sessionNonce(deps.now(), deps.random)
			if err != nil {
				return err
			}
			providerPrompt += "\n\nrun_ai launch correlation: " + captureTag + ". This is launcher metadata; ignore it for task decisions."
		}
	}
	invocation, err := providers.BuildInvocation(request.Provider, providers.LaunchOptions{
		Binary: binary, WorkingDirectory: target.cwd, AccessMode: request.AccessMode,
		ModelTrack: request.Model.Track, Model: request.Model.Model,
		ResumeID: resumeID, NewSessionID: newSessionID,
		SessionName: target.key, Prompt: providerPrompt,
		ContextDirs: []string{codexRoot},
	})
	if err != nil {
		return fmt.Errorf("build provider invocation: %w", err)
	}
	if err := deps.tmux.CreateDetached(ctx, tmuxName, sessions.Command{
		Binary: invocation.Binary, Args: invocation.Args, WorkingDirectory: invocation.WorkingDirectory, Manifest: manifest,
	}); err != nil {
		// A competing launcher may have created the exact same compatible
		// session. Only attach after proving that exact name now exists.
		if raced, checkErr := deps.tmux.Exists(ctx, tmuxName); checkErr != nil || !raced {
			return err
		}
	}
	info, err := deps.tmux.Validate(ctx, tmuxName, expected)
	if err != nil {
		return fmt.Errorf("refuse newly created or raced unverified tmux session %s: %w", tmuxName, err)
	}
	active, err := deps.lineage.Activate(tmuxName, manifest.Digest)
	if err != nil {
		return fmt.Errorf("activate private session lineage after provider validation: %w", err)
	}
	if active.State != "active" {
		return errors.New("activate private session lineage returned non-active state")
	}
	// In controller mode the interactive TUI is deliberately only a monitor.
	// Start the independent exec worker before attaching, so completion of a
	// turn never depends on tmux exiting. The worker has its own Codex session
	// and therefore cannot race the TUI's stdin or turn lifecycle.
	if os.Getenv("RUN_AI_PERSISTENT_EXEC") == "1" && request.Provider == providers.Codex {
		if err := ensurePersistentExecGoal(tmuxName, request.Workflow.Prompt); err != nil {
			return fmt.Errorf("prepare persistent exec goal: %w", err)
		}
		if err := startPersistentWorker(stderr); err != nil {
			return fmt.Errorf("start persistent exec worker: %w", err)
		}
	}
	fmt.Fprintf(stderr, "run_ai: attaching tmux session %s\n", tmuxName)
	if err := attachRevalidated(ctx, deps.tmux, tmuxName, expected, info.SessionID, stdin, stdout, stderr); err != nil {
		return err
	}
	if historyOffset >= 0 && captureTag != "" {
		providerSessionID, captureErr := deps.captureCodexSession(historyOffset, captureTag)
		if captureErr != nil {
			fmt.Fprintf(stderr, "run_ai: WARNING: Codex session ID capture failed; durable lineage remains tmux_lifetime_only: %v\n", captureErr)
		} else if providerSessionID == "" {
			fmt.Fprintln(stderr, "run_ai: WARNING: Codex session ID was not captured; durable lineage remains tmux_lifetime_only")
		} else {
			if _, bindErr := deps.lineage.BindProviderSessionID(tmuxName, manifest.Digest, providerSessionID); bindErr != nil {
				return fmt.Errorf("bind captured Codex session lineage: %w", bindErr)
			}
			if os.Getenv("RUN_AI_PERSISTENT") == "1" && request.Provider == providers.Codex {
				goalID := "codex-" + providerSessionID
				gs := goals.Store{Root: os.Getenv("RUN_AI_GOAL_STATE")}
				if gs.Root == "" {
					gs.Root = "/root/.local/state/run-ai/goals"
				}
				if _, loadErr := gs.Load(goalID); errors.Is(loadErr, os.ErrNotExist) {
					_ = gs.Save(goals.Goal{ID: goalID, Text: "Continue the current Codex session until the task is explicitly completed or blocked", State: goals.Checkpoint, Provider: "codex", SessionID: providerSessionID, TmuxName: tmuxName})
				}
				_ = startPersistentWorker(stderr)
			}
		}
	}
	return nil
}

func startPersistentWorker(stderr io.Writer) error {
	if os.Getenv("RUN_AI_PERSISTENT") != "1" && os.Getenv("RUN_AI_PERSISTENT_EXEC") != "1" {
		return nil
	}
	root := os.Getenv("RUN_AI_GOAL_STATE")
	if root == "" {
		root = "/root/.local/state/run-ai/goals"
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(root, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(os.Args[0], "goal", "worker")
	cmd.Env = append(os.Environ(), "RUN_AI_GOAL_CODEX=1", "RUN_AI_GOAL_AUTOCONTINUE=1", "RUN_AI_GOAL_WORKER_CYCLES=1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	_ = logFile.Close()
	fmt.Fprintln(stderr, "run_ai: persistent Codex goal worker started")
	return nil
}

func codexHistorySize() (int64, error) {
	file, err := openPrivateCodexHistory(codexHistory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func captureCodexHistorySession(offset int64, marker string) (string, error) {
	if offset < 0 || marker == "" {
		return "", nil
	}
	file, err := openPrivateCodexHistory(codexHistory)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	return captureCodexHistorySessionFrom(file, marker)
}

// openPrivateCodexHistory refuses links and writable or foreign history files.
// Correlation data influences resumability, so a hostile path must disable
// capture instead of allowing a provider ID to be injected into lineage.
func openPrivateCodexHistory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Codex history path must be absolute")
	}
	owner := os.Getuid()
	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator))
	if len(components) < 2 {
		return nil, errors.New("Codex history path has no private parent")
	}
	for _, component := range components[:len(components)-1] {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || (int(stat.Uid) != owner && int(stat.Uid) != 0) {
			return nil, errors.New("Codex history parent is not private and owner-controlled")
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || int(stat.Uid) != owner {
		file.Close()
		return nil, errors.New("Codex history is not a private regular file")
	}
	return file, nil
}

func captureCodexHistorySessionFrom(reader io.Reader, marker string) (string, error) {
	var matched string
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var event struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || !strings.Contains(event.Text, marker) {
			continue
		}
		if err := sessions.ValidateID(event.SessionID); err == nil {
			matched = event.SessionID
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return matched, nil
}

func attachRevalidated(ctx context.Context, runtime tmuxRuntime, tmuxName string, expected sessions.Expectation, validatedID string, stdin io.Reader, stdout, stderr io.Writer) error {
	current, err := runtime.Validate(ctx, tmuxName, expected)
	if err != nil {
		return fmt.Errorf("refuse tmux attach after final revalidation: %w", err)
	}
	if current.SessionID != validatedID {
		return errors.New("refuse tmux attach because immutable session ID changed after validation")
	}
	return runtime.Attach(ctx, current.SessionID, stdin, stdout, stderr)
}

// chooseLiveLineage discovers only exact, active durable lineages compatible
// with this launch request.  Names are never treated as identity: every
// candidate is first checked against its saved provider/key/manifest binding
// and then cryptographically-bound tmux metadata is validated before it can
// be offered for attach.
func chooseLiveLineage(ctx context.Context, deps dependencies, request cli.Request, target launchTarget, manifest sessions.Manifest, stdin io.Reader, stdout, stderr io.Writer) (*sessions.Lineage, io.Reader, error) {
	lineages, err := deps.lineage.List()
	if err != nil {
		return nil, stdin, fmt.Errorf("list durable session lineages: %w", err)
	}
	candidates := make([]sessions.Lineage, 0, len(lineages))
	for _, lineage := range lineages {
		if lineage.State != "active" || lineage.Provider != string(request.Provider) || lineage.SessionKey != target.key || lineage.ManifestDigest != manifest.Digest {
			continue
		}
		if err := validateLineageBinding(lineage, request, target, lineage.TmuxName, manifest, lineage.ExplicitNew); err != nil {
			return nil, stdin, fmt.Errorf("validate durable session lineage %q: %w", lineage.TmuxName, err)
		}
		if runtime, ok := deps.tmux.(tmuxStatusRuntime); ok {
			status, statusErr := runtime.Status(ctx, lineage.TmuxName)
			if statusErr != nil {
				return nil, stdin, fmt.Errorf("inspect tmux candidate %q: %w", lineage.TmuxName, statusErr)
			}
			if !status.Live {
				continue // absent/dead panes are stale durable history, never attach candidates.
			}
		} else {
			exists, existsErr := deps.tmux.Exists(ctx, lineage.TmuxName)
			if existsErr != nil {
				return nil, stdin, fmt.Errorf("inspect tmux candidate %q: %w", lineage.TmuxName, existsErr)
			}
			if !exists {
				continue
			}
		}
		if _, err := deps.tmux.Validate(ctx, lineage.TmuxName, sessions.Expectation{Manifest: manifest, UID: deps.uid()}); err != nil {
			return nil, stdin, fmt.Errorf("refuse unverified live tmux candidate %q: %w", lineage.TmuxName, err)
		}
		candidates = append(candidates, lineage)
	}
	switch len(candidates) {
	case 0:
		return nil, stdin, nil
	case 1:
		return &candidates[0], stdin, nil
	}
	if stdin == nil {
		return nil, stdin, errors.New("multiple live Codex sessions match; select one with --resume-id ID")
	}
	fmt.Fprintln(stderr, "run_ai: multiple exact live sessions match this launch; choose one (no task is restarted):")
	for index, candidate := range candidates {
		identity := candidate.ProviderSessionID
		if identity == "" {
			identity = "tmux-lifetime-only"
		}
		fmt.Fprintf(stderr, "  %d) provider_id=%s tmux=%s created=%s\n", index+1, identity, candidate.TmuxName, candidate.CreatedAt)
	}
	fmt.Fprint(stderr, "Select session number (or press Enter to cancel): ")
	reader := bufio.NewReader(stdin)
	choice, readErr := reader.ReadString('\n')
	if readErr != nil && len(choice) == 0 {
		return nil, reader, fmt.Errorf("read session choice: %w", readErr)
	}
	index, parseErr := strconv.Atoi(strings.TrimSpace(choice))
	if parseErr != nil || index < 1 || index > len(candidates) {
		return nil, reader, errors.New("session selection cancelled; use --resume-id ID for an exact noninteractive resume")
	}
	return &candidates[index-1], reader, nil
}

func validateLineageBinding(lineage sessions.Lineage, request cli.Request, target launchTarget, tmuxName string, manifest sessions.Manifest, explicitNew bool) error {
	if err := lineage.Validate(); err != nil {
		return err
	}
	if lineage.TmuxName != tmuxName || lineage.Provider != string(request.Provider) || lineage.SessionKey != target.key ||
		lineage.ManifestDigest != manifest.Digest || lineage.ExplicitNew != explicitNew {
		return errors.New("lineage does not match provider/session/manifest request")
	}
	return nil
}

func resolveTarget(host string, request cli.Request, deps dependencies) (launchTarget, error) {
	if request.Workflow.Kind == workflows.KindNone {
		return launchTarget{key: "main_" + domainKey(host), cwd: "/"}, nil
	}
	if request.Workflow.Kind != workflows.KindDev {
		return launchTarget{}, fmt.Errorf("workflow %q is not executable", request.Workflow.Kind)
	}
	records, err := deps.loadInventory(deps.inventoryPath, inventoryLimit)
	if err != nil {
		return launchTarget{}, fmt.Errorf("load hosting inventory: %w", err)
	}
	resolution, err := routing.Resolve(records, request.Workflow.Target)
	if err != nil {
		return launchTarget{}, fmt.Errorf("resolve development domain: %w", err)
	}
	server := strings.TrimSuffix(strings.ToLower(resolution.Server), ".")
	if server != host {
		alias, ok := deps.remoteAliases[server]
		if !ok {
			return launchTarget{}, fmt.Errorf("domain %s belongs to unsupported remote server %s", resolution.CanonicalDomain, resolution.Server)
		}
		return launchTarget{key: remoteSessionKey(server, resolution.CanonicalDomain), remoteAlias: alias}, nil
	}
	info, err := deps.stat(resolution.DocumentRoot)
	if err != nil || !info.IsDir() {
		return launchTarget{}, fmt.Errorf("resolved document root is not an existing directory: %s", resolution.DocumentRoot)
	}
	prefix := "dev_"
	return launchTarget{key: prefix + domainKey(resolution.CanonicalDomain), cwd: filepath.Clean(resolution.DocumentRoot)}, nil
}

func remoteSessionKey(server, domain string) string {
	return "dev_" + domainKey(domain)
}

func executeRemote(ctx context.Context, request cli.Request, target launchTarget, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) error {
	if request.AccessMode != permissions.ModeFull {
		return errors.New("remote development delegation is supported only in full mode during migration")
	}
	providerBinary := map[providers.Name]string{
		providers.Codex: "run_codex", providers.Claude: "run_claude", providers.Antigravity: "run_antigravity",
	}[request.Provider]
	launcher := ""
	if providerBinary != "" {
		launcher = deps.launcherDir + "/" + providerBinary
	}
	if launcher == "" {
		return errors.New("remote provider launcher is unsupported")
	}
	args := make([]string, 0, 6)
	if request.Session.NewSession {
		// The legacy launcher must recognize this option.  An older launcher
		// rejects it during argument parsing, before it can rewrite its
		// canonical session map; silently relying on an environment variable
		// would allow an old launcher to promote a fresh remote session.
		args = append(args, "--new-session", "--no-promotion")
	}
	if request.Session.ResumeID != "" {
		args = append(args, "--resume-id", request.Session.ResumeID)
	}
	switch request.Model.Track {
	case providers.ModelDefault:
		args = append(args, "--default")
	case providers.ModelLatest:
		args = append(args, "--latest")
	case providers.ModelExplicit:
		args = append(args, "--model", request.Model.Model)
	}
	args = append(args, request.Workflow.Target)
	remoteCommand, err := shellCommand(launcher, args)
	if err != nil {
		return err
	}
	var expectedSessionName, beforeID string
	if !request.Session.NewSession {
		if name, nameErr := sessions.LegacyTmuxName("", request.Provider, target.key, false, ""); nameErr == nil {
			expectedSessionName = name
			fmt.Fprintf(stderr, "run_ai: attaching remote tmux session %s on %s\n", name, target.remoteAlias)
			// Read the session ID currently in the registry so we can detect changes after the session ends.
			beforeID = readRemoteSessionID(ctx, deps, target, request.Provider, target.key)
		}
	}
	if err := deps.runProcess.Run(ctx, deps.sshBinary, []string{"-t", target.remoteAlias, remoteCommand}, stdin, stdout, stderr); err != nil {
		return fmt.Errorf("remote legacy delegation to %s: %w", target.remoteAlias, err)
	}
	providerFlag := string(request.Provider)
	if request.Provider == providers.Antigravity {
		providerFlag = "gemini"
	}
	if request.Session.NewSession {
		fmt.Fprintf(stderr, "run_ai: remote session ended; session started fresh (--new-session) — to start another: run_ai --%s --new-session --dev %s\n", providerFlag, request.Workflow.Target)
		return nil
	}
	if expectedSessionName == "" {
		fmt.Fprintf(stderr, "run_ai: remote session ended; session state unknown — to reconnect: run_ai --%s --dev %s\n", providerFlag, request.Workflow.Target)
		return nil
	}
	info := probeRemoteSession(ctx, deps, target, request.Provider, expectedSessionName, target.key, beforeID)
	printRemoteResumeInfo(stderr, info, providerFlag, request.Workflow.Target)
	return nil
}

func shellCommand(binary string, args []string) (string, error) {
	values := append([]string{binary}, args...)
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		if strings.ContainsRune(value, '\x00') {
			return "", errors.New("remote argument contains NUL")
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(value, "'", "'\"'\"'")+"'")
	}
	return strings.Join(quoted, " "), nil
}

type remoteSessionState int

const (
	remoteStateUnknown       remoteSessionState = iota // probe did not run or returned unexpected output
	remoteStateAlive                                   // tmux session confirmed running
	remoteStateDeadCurrentID                           // tmux ended, ID changed during session window — unverified (concurrent runs indistinguishable)
	remoteStateDeadStaleID                             // tmux ended, ID unchanged — predates this run, definitely stale
	remoteStateDeadNoID                                // tmux ended, session ID absent or invalid
)

type remoteSessionInfo struct {
	State     remoteSessionState
	SessionID string
}

func isLegacyTmuxSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func remoteProviderSessionsFile(provider providers.Name) string {
	root := os.Getenv("RUN_AI_CODEX_ROOT")
	if root == "" {
		return ""
	}
	access := filepath.Join(root, "access")
	switch provider {
	case providers.Claude:
		return filepath.Join(access, "CLAUDE_SESSIONS.md")
	case providers.Antigravity:
		return filepath.Join(access, "ANTIGRAVITY_SESSIONS.md")
	default:
		return filepath.Join(access, "SESSIONS.md")
	}
}

// readRemoteSessionID performs a non-interactive pre-probe to read the session ID
// currently in the registry for the given key. The result is used as beforeID for
// before/after comparison in parseRemoteSessionProbe.
func readRemoteSessionID(ctx context.Context, deps dependencies, target launchTarget, provider providers.Name, sessionKey string) string {
	if !isLegacyTmuxSafe(sessionKey) {
		return ""
	}
	sessionsFile := remoteProviderSessionsFile(provider)
	readCmd := fmt.Sprintf(
		"awk -F= '$1==\"%s\" {print $2; exit}' '%s' 2>/dev/null | head -c 300",
		sessionKey, sessionsFile,
	)
	var out bytes.Buffer
	if err := deps.runProcess.Run(ctx, deps.sshBinary, []string{target.remoteAlias, readCmd}, nil, &out, io.Discard); err != nil {
		return ""
	}
	id := strings.TrimSpace(out.String())
	if sessions.ValidateID(id) != nil {
		return ""
	}
	return id
}

func probeRemoteSession(ctx context.Context, deps dependencies, target launchTarget, provider providers.Name, sessionName, sessionKey string, beforeID string) remoteSessionInfo {
	if !isLegacyTmuxSafe(sessionName) || !isLegacyTmuxSafe(sessionKey) {
		return remoteSessionInfo{}
	}
	sessionsFile := remoteProviderSessionsFile(provider)
	// Two output lines: "tmux:alive|tmux:dead", afterID-or-empty.
	// Identity is compared against beforeID (read before the session) to detect registry updates,
	// but concurrent runs of the same key are indistinguishable — all dead-session states are unverified.
	probeCmd := fmt.Sprintf(
		"{ tmux has-session -t '%s' 2>/dev/null && printf 'tmux:alive\\n' || printf 'tmux:dead\\n'; }; "+
			"_sid=$(awk -F= '$1==\"%s\" {print $2; exit}' '%s' 2>/dev/null | head -c 300); "+
			"printf '%%s\\n' \"$_sid\"",
		sessionName, sessionKey, sessionsFile,
	)
	var out bytes.Buffer
	if err := deps.runProcess.Run(ctx, deps.sshBinary, []string{target.remoteAlias, probeCmd}, nil, &out, io.Discard); err != nil {
		return remoteSessionInfo{}
	}
	return parseRemoteSessionProbe(out.String(), beforeID)
}

func parseRemoteSessionProbe(output, beforeID string) remoteSessionInfo {
	lines := strings.SplitN(output, "\n", 3)
	if len(lines) < 1 {
		return remoteSessionInfo{State: remoteStateUnknown}
	}
	switch lines[0] {
	case "tmux:alive":
		return remoteSessionInfo{State: remoteStateAlive}
	case "tmux:dead":
		afterID := ""
		if len(lines) >= 2 {
			afterID = strings.TrimSpace(lines[1])
		}
		if afterID == "" || sessions.ValidateID(afterID) != nil {
			return remoteSessionInfo{State: remoteStateDeadNoID}
		}
		// ID changed during the session window: registry was written, possibly by this run.
		// Concurrent runs of the same key are indistinguishable — unverified.
		if afterID != beforeID {
			return remoteSessionInfo{State: remoteStateDeadCurrentID, SessionID: afterID}
		}
		// ID unchanged: the registry was not updated during this run. The ID predates this session.
		return remoteSessionInfo{State: remoteStateDeadStaleID, SessionID: afterID}
	default:
		return remoteSessionInfo{State: remoteStateUnknown}
	}
}

func printRemoteResumeInfo(stderr io.Writer, info remoteSessionInfo, providerFlag, target string) {
	switch info.State {
	case remoteStateAlive:
		fmt.Fprintf(stderr, "run_ai: remote session ended; tmux session still running — reconnect: run_ai --%s --dev %s\n", providerFlag, target)
	case remoteStateDeadCurrentID:
		fmt.Fprintf(stderr, "run_ai: remote session ended; session ID unverified — to resume: run_ai --%s --resume-id %s --dev %s\n", providerFlag, info.SessionID, target)
	case remoteStateDeadStaleID:
		fmt.Fprintf(stderr, "run_ai: remote session ended; session ended — ID unchanged, identity unverified: run_ai --%s --resume-id %s --dev %s\n", providerFlag, info.SessionID, target)
	case remoteStateDeadNoID:
		fmt.Fprintf(stderr, "run_ai: remote session ended; session ended — reconnect starts fresh: run_ai --%s --dev %s\n", providerFlag, target)
	default:
		fmt.Fprintf(stderr, "run_ai: remote session ended; session state unknown — to reconnect: run_ai --%s --dev %s\n", providerFlag, target)
	}
}

func buildManifest(request cli.Request, target launchTarget) (sessions.Manifest, error) {
	targetPayload := struct {
		Key, CWD, PlanDigest, ModelTrack, Model string
	}{target.key, target.cwd, request.Workflow.PlanDigest, string(request.Model.Track), request.Model.Model}
	targetBytes, err := json.Marshal(targetPayload)
	if err != nil {
		return sessions.Manifest{}, err
	}
	targetHash := sha256.Sum256(targetBytes)
	manifest := sessions.Manifest{
		Provider: string(request.Provider), AccessMode: string(request.AccessMode), Workflow: string(request.Workflow.Kind),
		TargetDigest: hex.EncodeToString(targetHash[:]),
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return sessions.Manifest{}, err
	}
	manifestHash := sha256.Sum256(manifestBytes)
	manifest.Digest = hex.EncodeToString(manifestHash[:])
	return manifest, nil
}

func domainKey(domain string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(domain)
}

func sessionNonce(now time.Time, random io.Reader) (string, error) {
	// Keep the legacy-readable UTC prefix, but bind new tmux identities to 128
	// bits of cryptographic entropy. This avoids collisions between concurrent
	// launchers without relying on a PID or mutable global counter.
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return "", fmt.Errorf("generate session nonce: %w", err)
	}
	return fmt.Sprintf("%s-%x", now.UTC().Format("20060102150405"), bytes), nil
}

func newUUID(random io.Reader) (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return "", fmt.Errorf("generate Claude session ID: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
