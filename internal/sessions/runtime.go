package sessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Command is an already separated executable invocation. Args are never
// concatenated except when tmux itself requires one shell-command argument.
type Command struct {
	Binary           string
	Args             []string
	WorkingDirectory string
	Manifest         Manifest
}

type Manifest struct {
	Digest       string
	Provider     string
	AccessMode   string
	Workflow     string
	TargetDigest string
}

type Expectation struct {
	Manifest
	UID         int
	AllowLegacy bool
}

type RuntimeInfo struct {
	SessionID   string
	SessionName string
	UID         int
	PaneDead    bool
	// LastActivity is tmux's session activity timestamp. It is supplied by
	// tmux itself and is suitable for inventory/status displays only; it is
	// not an authorization input.
	LastActivity time.Time
	Current      string
	StartCommand string
	Windows      int
	Panes        int
	Manifest     Manifest
}

// LiveStatus is a read-only snapshot of a named tmux session. A session is
// live only when the exact name still resolves and its primary pane has not
// exited.  Callers that need to attach must still use Validate and revalidate
// the immutable tmux session ID immediately before attaching.
//
// Missing is deliberately distinct from an error: an absent tmux server and
// an exited session are normal stale-lineage conditions, while an inspection
// failure after has-session succeeded is left as an error rather than being
// misreported as stale.
type LiveStatus struct {
	Exists       bool
	Live         bool
	LastActivity time.Time
	Info         RuntimeInfo
}

type ProcessRunner interface {
	Run(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, binary string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return command.Run()
}

type TmuxClient struct {
	Binary string
	Runner ProcessRunner
}

func (client TmuxClient) normalized() (TmuxClient, error) {
	if client.Binary == "" {
		client.Binary = "tmux"
	}
	if client.Runner == nil {
		client.Runner = ExecRunner{}
	}
	if strings.ContainsRune(client.Binary, '\x00') {
		return TmuxClient{}, errors.New("tmux binary contains NUL")
	}
	return client, nil
}

func (client TmuxClient) Exists(ctx context.Context, name string) (bool, error) {
	if err := validateTmuxName(name); err != nil {
		return false, err
	}
	client, err := client.normalized()
	if err != nil {
		return false, err
	}
	err = client.Runner.Run(ctx, client.Binary, []string{"has-session", "-t", "=" + name}, nil, io.Discard, io.Discard)
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session: %w", err)
}

// CreateDetached creates exactly one named session. It never replaces or
// renames an existing session.
func (client TmuxClient) CreateDetached(ctx context.Context, name string, command Command) error {
	if err := validateTmuxName(name); err != nil {
		return err
	}
	if command.Binary == "" || strings.ContainsRune(command.Binary, '\x00') {
		return errors.New("provider binary is empty or contains NUL")
	}
	if command.WorkingDirectory == "" || !strings.HasPrefix(command.WorkingDirectory, "/") {
		return errors.New("working directory must be absolute")
	}
	client, err := client.normalized()
	if err != nil {
		return err
	}
	shellCommand, err := quoteCommand(command.Binary, command.Args)
	if err != nil {
		return err
	}
	if err := validateManifest(command.Manifest); err != nil {
		return fmt.Errorf("session manifest: %w", err)
	}
	args := []string{"new-session", "-d", "-s", name, "-c", command.WorkingDirectory, shellCommand}
	for _, option := range []struct{ name, value string }{
		{"@run_ai_manifest", command.Manifest.Digest},
		{"@run_ai_provider", command.Manifest.Provider},
		{"@run_ai_mode", command.Manifest.AccessMode},
		{"@run_ai_workflow", command.Manifest.Workflow},
		{"@run_ai_target_digest", command.Manifest.TargetDigest},
	} {
		// set-option expects a target-session, but tmux 3.5a does not resolve
		// the exact-match form "=name" in this command even though
		// has-session accepts it.  Appending ':' makes the target an exact
		// session-qualified target and keeps the manifest writes in the same
		// tmux command queue as new-session.
		args = append(args, ";", "set-option", "-t", "="+name+":", option.name, option.value)
	}
	if err := client.Runner.Run(ctx, client.Binary, args, nil, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}
	return nil
}

func (client TmuxClient) Inspect(ctx context.Context, name string) (RuntimeInfo, error) {
	if err := validateTmuxName(name); err != nil {
		return RuntimeInfo{}, err
	}
	client, err := client.normalized()
	if err != nil {
		return RuntimeInfo{}, err
	}
	const format = "#{session_id}\t#{session_name}\t#{uid}\t#{pane_dead}\t#{session_activity}\t#{pane_current_command}\t#{pane_start_command}\t#{session_windows}\t#{window_panes}\t#{@run_ai_manifest}\t#{@run_ai_provider}\t#{@run_ai_mode}\t#{@run_ai_workflow}\t#{@run_ai_target_digest}"
	var output bytes.Buffer
	if err := client.Runner.Run(ctx, client.Binary, []string{"display-message", "-p", "-t", "=" + name + ":0.0", format}, nil, &output, io.Discard); err != nil {
		return RuntimeInfo{}, fmt.Errorf("tmux inspect: %w", err)
	}
	parts := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\t")
	if len(parts) != 14 {
		return RuntimeInfo{}, fmt.Errorf("tmux inspect returned %d fields, want 14", len(parts))
	}
	if err := validateSessionID(parts[0]); err != nil || parts[1] != name {
		return RuntimeInfo{}, errors.New("tmux inspect returned mismatched session identity")
	}
	uid, err := strconv.Atoi(parts[2])
	if err != nil {
		return RuntimeInfo{}, errors.New("tmux inspect returned invalid uid")
	}
	dead, err := strconv.Atoi(parts[3])
	if err != nil || (dead != 0 && dead != 1) {
		return RuntimeInfo{}, errors.New("tmux inspect returned invalid pane state")
	}
	var lastActivity time.Time
	// session_activity is available on supported modern tmux releases. Treat
	// an empty expansion as unavailable instead of rejecting an otherwise
	// attachable legacy session; a non-empty malformed value remains unsafe.
	if parts[4] != "" {
		activity, parseErr := strconv.ParseInt(parts[4], 10, 64)
		if parseErr != nil || activity < 0 {
			return RuntimeInfo{}, errors.New("tmux inspect returned invalid activity time")
		}
		lastActivity = time.Unix(activity, 0).UTC()
	}
	windows, err := strconv.Atoi(parts[7])
	if err != nil {
		return RuntimeInfo{}, errors.New("tmux inspect returned invalid window count")
	}
	panes, err := strconv.Atoi(parts[8])
	if err != nil {
		return RuntimeInfo{}, errors.New("tmux inspect returned invalid pane count")
	}
	return RuntimeInfo{
		SessionID: parts[0], SessionName: parts[1], UID: uid, PaneDead: dead == 1,
		LastActivity: lastActivity, Current: parts[5], StartCommand: parts[6], Windows: windows, Panes: panes,
		Manifest: Manifest{Digest: parts[9], Provider: parts[10], AccessMode: parts[11], Workflow: parts[12], TargetDigest: parts[13]},
	}, nil
}

// Status obtains a read-only liveness snapshot for session inventory. It does
// not validate the run_ai manifest and must never be used as an attach
// authorization shortcut.
func (client TmuxClient) Status(ctx context.Context, name string) (LiveStatus, error) {
	exists, err := client.Exists(ctx, name)
	if err != nil {
		return LiveStatus{}, err
	}
	if !exists {
		return LiveStatus{}, nil
	}
	info, err := client.Inspect(ctx, name)
	if err != nil {
		return LiveStatus{}, err
	}
	return LiveStatus{
		Exists:       true,
		Live:         !info.PaneDead,
		LastActivity: info.LastActivity,
		Info:         info,
	}, nil
}

// Validate refuses stale, dead, cross-user, wrong-provider, or wrong-plan
// sessions. Legacy sessions are accepted only with an explicit mapping marker
// supplied by the caller and a provider-compatible pane command.
func (client TmuxClient) Validate(ctx context.Context, name string, expected Expectation) (RuntimeInfo, error) {
	if err := validateManifest(expected.Manifest); err != nil {
		return RuntimeInfo{}, fmt.Errorf("expected manifest: %w", err)
	}
	actual, err := client.Inspect(ctx, name)
	if err != nil {
		return RuntimeInfo{}, err
	}
	if actual.UID != expected.UID || actual.PaneDead || actual.Windows != 1 || actual.Panes != 1 {
		return RuntimeInfo{}, fmt.Errorf("tmux session identity/state mismatch")
	}
	if actual.Manifest.Digest == "" {
		if !expected.AllowLegacy || !legacyCommandAllowed(expected.Provider, actual.Current, actual.StartCommand) {
			return RuntimeInfo{}, errors.New("unmarked legacy tmux session is not authorized for this request")
		}
		return actual, nil
	}
	if actual.Manifest != expected.Manifest {
		return RuntimeInfo{}, errors.New("tmux session manifest mismatch")
	}
	if !providerCommandAllowed(expected.Provider, actual.Current) {
		return RuntimeInfo{}, fmt.Errorf("tmux pane command %q is not allowed for provider %q", actual.Current, expected.Provider)
	}
	return actual, nil
}

func (client TmuxClient) Attach(ctx context.Context, sessionID string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	client, err := client.normalized()
	if err != nil {
		return err
	}
	args := []string{"attach-session", "-t", sessionID}
	if os.Getenv("TMUX") != "" {
		args = []string{"switch-client", "-t", sessionID}
	}
	if err := client.Runner.Run(ctx, client.Binary, args, stdin, stdout, stderr); err != nil {
		return fmt.Errorf("tmux attach: %w", err)
	}
	return nil
}

// Terminate removes exactly one immutable tmux session ID. Callers must have
// inspected and bound that ID to durable metadata immediately beforehand;
// accepting a name here would permit a detach/reattach race to target a
// replacement session.
func (client TmuxClient) Terminate(ctx context.Context, sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	client, err := client.normalized()
	if err != nil {
		return err
	}
	if err := client.Runner.Run(ctx, client.Binary, []string{"kill-session", "-t", sessionID}, nil, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("tmux kill-session: %w", err)
	}
	return nil
}

func validateSessionID(value string) error {
	if len(value) < 2 || value[0] != '$' {
		return errors.New("invalid tmux session id")
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return errors.New("invalid tmux session id")
		}
	}
	return nil
}

func validateTmuxName(name string) error {
	if name == "" || len(name) > 512 || sanitizeTmuxName(name) != name {
		return errors.New("invalid tmux session name")
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	for label, value := range map[string]string{
		"digest": manifest.Digest, "provider": manifest.Provider, "access mode": manifest.AccessMode,
		"workflow": manifest.Workflow, "target digest": manifest.TargetDigest,
	} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\t\r\n") {
			return fmt.Errorf("invalid %s", label)
		}
	}
	if len(manifest.Digest) != 64 || len(manifest.TargetDigest) != 64 {
		return errors.New("manifest digests must be lowercase SHA-256")
	}
	for _, digest := range []string{manifest.Digest, manifest.TargetDigest} {
		for _, character := range digest {
			if !strings.ContainsRune("0123456789abcdef", character) {
				return errors.New("manifest digest is not lowercase hexadecimal")
			}
		}
	}
	return nil
}

func providerCommandAllowed(provider, current string) bool {
	allowed := map[string][]string{
		"codex":       {"codex", "node"},
		"claude":      {"claude"},
		"antigravity": {"agy"},
	}
	for _, value := range allowed[provider] {
		if current == value {
			return true
		}
	}
	return false
}

func legacyCommandAllowed(provider, current, startCommand string) bool {
	if providerCommandAllowed(provider, current) {
		return true
	}
	if current != "bash" && current != "node" {
		return false
	}
	if startCommand == "" {
		return provider == "codex"
	}
	switch provider {
	case "codex":
		return !strings.Contains(startCommand, "--provider claude") && !strings.Contains(startCommand, "--provider antigravity")
	case "claude":
		return strings.Contains(startCommand, "--provider claude") || strings.Contains(startCommand, "run_claude")
	case "antigravity":
		return strings.Contains(startCommand, "--provider antigravity") || strings.Contains(startCommand, "run_antigravity") || strings.Contains(startCommand, "run_gemini")
	default:
		return false
	}
}

func quoteCommand(binary string, args []string) (string, error) {
	values := append([]string{binary}, args...)
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		if strings.ContainsRune(value, '\x00') {
			return "", errors.New("command argument contains NUL")
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(value, "'", "'\"'\"'")+"'")
	}
	return strings.Join(quoted, " "), nil
}
