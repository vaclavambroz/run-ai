package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaclavambroz/run-ai/internal/appserver"
	"github.com/vaclavambroz/run-ai/internal/cli"
	"github.com/vaclavambroz/run-ai/internal/goals"
	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/routing"
	"github.com/vaclavambroz/run-ai/internal/sessions"
)

func TestPersistentTerminalPasteFileAndCommandErrorsDoNotSplitOrExit(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "terminal.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mu sync.Mutex
	var prompts []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				var command appserver.IPCCommand
				if err := json.NewDecoder(conn).Decode(&command); err != nil {
					return
				}
				reply := appserver.Reply{OK: true}
				switch command.Op {
				case "prompt":
					mu.Lock()
					prompts = append(prompts, command.Text)
					reply.InstructionID = uint64(len(prompts))
					mu.Unlock()
				case "pause":
					reply.Error = "simulated interrupt rejection"
				case "events":
					// The reconnect watcher is intentionally idle in this test.
				}
				_ = json.NewEncoder(conn).Encode(reply)
			}()
		}
	}()
	filePrompt := "from file\n\nwith a blank paragraph\n"
	file := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(file, []byte(filePrompt), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "prompt-link.md")
	if err := os.Symlink(file, symlink); err != nil {
		t.Fatal(err)
	}
	markdown := "# Heading\n\nFirst paragraph.\n\n- one\n- two"
	input := "/paste\n" + markdown + "\n/endpaste\n/pause\nshort instruction\n/prompt-file " + file + "\n/prompt-file " + symlink + "\n/quit\n"
	var out bytes.Buffer
	if err := runAppServerTerminal(context.Background(), socket, strings.NewReader(input), &out); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), prompts...)
	mu.Unlock()
	if len(got) != 3 || got[0] != markdown || got[1] != "short instruction" || got[2] != filePrompt {
		t.Fatalf("submitted prompts = %#v", got)
	}
	if !strings.Contains(out.String(), "command error: simulated interrupt rejection") || strings.Count(out.String(), "[command error:") < 2 {
		t.Fatalf("command error did not remain visible: %s", out.String())
	}
	_ = listener.Close()
	<-done
}

func TestParseCodexOutcomeTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		state      goals.State
	}{
		{"completed", "{\"type\":\"item.completed\",\"text\":\"RUN_AI_GOAL_STATE: completed\"}", goals.Completed},
		{"checkpoint", "RUN_AI_GOAL_STATE: checkpoint", goals.Checkpoint},
		{"blocked", "RUN_AI_GOAL_STATE: blocked", goals.Blocked},
	} {
		got, _ := parseCodexOutcome([]byte(tc.text))
		if got != tc.state {
			t.Fatalf("%s: got %q", tc.name, got)
		}
	}
	if got, _ := parseCodexOutcome([]byte("turn.completed\n")); got != "" {
		t.Fatalf("unmarked turn became %q", got)
	}
}

func TestParseCodexThreadIDStructuredEvent(t *testing.T) {
	out := []byte(`{"type":"thread.started","thread_id":"01abc"}` + "\n" + `{"type":"turn.completed"}`)
	if got := parseCodexThreadID(out); got != "01abc" {
		t.Fatalf("thread id = %q", got)
	}
	if got := parseCodexThreadID([]byte(`{"type":"turn.completed"}`)); got != "" {
		t.Fatalf("unexpected thread id %q", got)
	}
}

type fakeTmux struct {
	exists       bool
	existsErr    error
	createErr    error
	created      []sessions.Command
	createdFor   []string
	attached     []string
	validated    []sessions.Expectation
	validateErr  error
	marked       bool
	sessionIDs   []string
	beforeCreate func()
}

func (fake *fakeTmux) Validate(_ context.Context, name string, expected sessions.Expectation) (sessions.RuntimeInfo, error) {
	fake.validated = append(fake.validated, expected)
	sessionID := "$42"
	if index := len(fake.validated) - 1; index >= 0 && index < len(fake.sessionIDs) {
		sessionID = fake.sessionIDs[index]
	}
	info := sessions.RuntimeInfo{SessionID: sessionID, SessionName: name}
	if len(fake.created) > 0 || fake.marked {
		info.Manifest = expected.Manifest
	}
	return info, fake.validateErr
}

func (fake *fakeTmux) Exists(context.Context, string) (bool, error) {
	return fake.exists, fake.existsErr
}

func (fake *fakeTmux) CreateDetached(_ context.Context, name string, command sessions.Command) error {
	if fake.beforeCreate != nil {
		fake.beforeCreate()
	}
	fake.createdFor = append(fake.createdFor, name)
	fake.created = append(fake.created, command)
	if fake.createErr == nil {
		fake.exists = true
	}
	return fake.createErr
}

func TestLineageIntentExistsBeforeProviderStart(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	tmux.beforeCreate = func() {
		lineage, err := deps.lineage.Lookup("codex_codex_main_testhost_example_com")
		if err != nil || lineage.State != "intent" {
			t.Fatalf("pre-create lineage = %#v, %v", lineage, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	lineage, err := deps.lineage.Lookup("codex_codex_main_testhost_example_com")
	if err != nil || lineage.State != "active" {
		t.Fatalf("post-create lineage = %#v, %v", lineage, err)
	}
}

func TestFreshCodexCaptureBindsExactResumeID(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.captureCodexSession = func(int64, string) (string, error) { return "01a14b7a-ccfa-76a2-90ae-83f6a6752d41", nil }
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	lineage, err := deps.lineage.Lookup("codex_codex_main_testhost_example_com")
	if err != nil || lineage.ProviderSessionID != "01a14b7a-ccfa-76a2-90ae-83f6a6752d41" || lineage.ResumeCapability != sessions.ResumeExactProviderID {
		t.Fatalf("lineage=%#v err=%v", lineage, err)
	}
	if len(tmux.created) != 1 || !strings.Contains(strings.Join(tmux.created[0].Args, "\n"), "run_ai launch correlation:") {
		t.Fatalf("provider invocation did not receive correlation marker: %#v", tmux.created)
	}
}

func TestDefaultLaunchReattachesOneExactLiveDurableLineage(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("create code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.created) != 1 {
		t.Fatalf("created=%#v", tmux.created)
	}
	tmux.attached = nil
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("reattach code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.created) != 1 || len(tmux.attached) != 1 {
		t.Fatalf("default launch duplicated instead of reattaching: %#v", tmux)
	}
}

func TestLiveLineageSelectionIsExplicitWhenSeveralExactCandidatesExist(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{exists: true, marked: true}
	deps := testDependencies(tmux)
	request, err := cli.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	target := launchTarget{key: "main_testhost_example_com", cwd: "/"}
	manifest, err := buildManifest(request, target)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{
		"codex_codex_main_testhost_example_com_new_20261014010203-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"codex_codex_main_testhost_example_com_new_20261014010203-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	for index, name := range names {
		intent, err := sessions.NewLineageIntent(name, string(providers.Codex), target.key, "01a14b7a-ccfa-76a2-90ae-83f6a6752d4"+string(rune('1'+index)), manifest.Digest, true, deps.now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := deps.lineage.Prepare(intent); err != nil {
			t.Fatal(err)
		}
		if _, err := deps.lineage.Activate(name, manifest.Digest); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	chosen, attachInput, err := chooseLiveLineage(context.Background(), deps, request, target, manifest, strings.NewReader("2\n"), &stdout, &stderr)
	if err != nil || chosen == nil || chosen.TmuxName != names[1] {
		t.Fatalf("chosen=%#v err=%v stderr=%s", chosen, err, stderr.String())
	}
	if attachInput == nil || !strings.Contains(stderr.String(), "multiple exact live sessions") || !strings.Contains(stderr.String(), "2) provider_id=") {
		t.Fatalf("selection was not presented safely: input=%T stderr=%s", attachInput, stderr.String())
	}
	if len(tmux.created) != 0 || len(tmux.attached) != 0 {
		t.Fatalf("selection unexpectedly mutated runtime: %#v", tmux)
	}

	_, _, err = chooseLiveLineage(context.Background(), deps, request, target, manifest, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "multiple live Codex sessions") {
		t.Fatalf("noninteractive ambiguity was accepted: %v", err)
	}
}

func TestTeamResumeUsesExactCodexThread(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	const sessionID = "01a14b7a-ccfa-76a2-90ae-83f6a6752d41"
	var stdout, stderr bytes.Buffer
	deps.captureCodexSession = func(int64, string) (string, error) { return sessionID, nil }
	if code := runWith(context.Background(), []string{"--team"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("fresh team code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.createdFor) != 1 {
		t.Fatalf("fresh team tmux creation = %#v", tmux.createdFor)
	}
	originalTmuxName := tmux.createdFor[0]
	tmux.exists = false
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--team", "--resume-id", sessionID}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("resumed team code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.created) != 2 || tmux.createdFor[1] != originalTmuxName || !contains(tmux.created[1].Args, "resume") || !contains(tmux.created[1].Args, sessionID) {
		t.Fatalf("team resume invocation = %#v", tmux.created)
	}
}

func TestTeamResumeRejectsUnboundOrNonTeamCodexThread(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	const sessionID = "01a14b7a-ccfa-76a2-90ae-83f6a6752d41"
	deps.captureCodexSession = func(int64, string) (string, error) { return sessionID, nil }
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("fresh non-team code=%d stderr=%s", code, stderr.String())
	}
	tmux.exists = false
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--team", "--resume-id", sessionID}, nil, &stdout, &stderr, deps); code != exitSoftware || len(tmux.created) != 1 || !strings.Contains(stderr.String(), "matching exact durable team lineage") {
		t.Fatalf("unbound team resume code=%d creations=%#v stderr=%s", code, tmux.created, stderr.String())
	}
}

func TestFreshCodexCaptureWarnsWhenHistoryIsUnavailable(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.captureCodexSession = func(int64, string) (string, error) { return "", errors.New("history unreadable") }
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK || !strings.Contains(stderr.String(), "tmux_lifetime_only") {
		t.Fatalf("capture warning code=%d stderr=%s", code, stderr.String())
	}
}

func TestOpenPrivateCodexHistoryRejectsLinksAndWritableParents(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".history-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	history := filepath.Join(root, "history.jsonl")
	if err := os.WriteFile(history, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openPrivateCodexHistory(history)
	if err != nil {
		t.Fatalf("private history rejected: %v", err)
	}
	file.Close()
	link := filepath.Join(root, "history-link.jsonl")
	if err := os.Symlink(history, link); err != nil {
		t.Fatal(err)
	}
	if file, err := openPrivateCodexHistory(link); err == nil {
		file.Close()
		t.Fatal("symlink history was accepted")
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if file, err := openPrivateCodexHistory(history); err == nil {
		file.Close()
		t.Fatal("group-writable history parent was accepted")
	}
}

func TestCaptureCodexHistorySessionFromMarker(t *testing.T) {
	t.Parallel()
	input := strings.NewReader("{\"session_id\":\"ignored\",\"text\":\"run_ai launch correlation: marker\"}\n{\"session_id\":\"01a14b7a-ccfa-76a2-90ae-83f6a6752d41\",\"text\":\"run_ai launch correlation: marker\"}\n")
	got, err := captureCodexHistorySessionFrom(input, "marker")
	if err != nil || got != "01a14b7a-ccfa-76a2-90ae-83f6a6752d41" {
		t.Fatalf("captured=%q err=%v", got, err)
	}
}

func (fake *fakeTmux) Attach(_ context.Context, name string, _ io.Reader, _, _ io.Writer) error {
	fake.attached = append(fake.attached, name)
	return nil
}

type statusFakeTmux struct {
	*fakeTmux
	status     sessions.LiveStatus
	statusErr  error
	terminated []string
}

func (fake *statusFakeTmux) Status(context.Context, string) (sessions.LiveStatus, error) {
	return fake.status, fake.statusErr
}

func (fake *statusFakeTmux) Terminate(_ context.Context, sessionID string) error {
	fake.terminated = append(fake.terminated, sessionID)
	return nil
}

type dirInfo struct{ path string }

func (info dirInfo) Name() string  { return info.path }
func (dirInfo) Size() int64        { return 0 }
func (dirInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (dirInfo) ModTime() time.Time { return time.Time{} }
func (dirInfo) IsDir() bool        { return true }
func (dirInfo) Sys() any           { return nil }

func testDependencies(tmux *fakeTmux) dependencies {
	return dependencies{
		hostname:            func() (string, error) { return "testhost.example.com", nil },
		stat:                func(path string) (os.FileInfo, error) { return dirInfo{path: path}, nil },
		loadInventory:       func(string, int64) ([]routing.DomainRecord, error) { return nil, errors.New("not configured") },
		lookupBinary:        func(name string) (string, error) { return "/test/bin/" + name, nil },
		runProcess:          &recordingProcess{},
		uid:                 func() int { return 1000 },
		workingDirectory:    func() (string, error) { return "/", nil },
		now:                 func() time.Time { return time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC) },
		random:              bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)),
		store:               sessions.NewMemoryStore(),
		lineage:             &sessions.MemoryLineageStore{},
		tmux:                tmux,
		historySize:         func() (int64, error) { return 0, nil },
		captureCodexSession: func(int64, string) (string, error) { return "", nil },
		remoteAliases:       map[string]string{"remote.example.com": "remote"},
		inventoryPath:       "/test/domains.json",
		sshBinary:           "/test/bin/ssh",
		launcherDir:         "/test/launchers",
	}
}

type recordingProcess struct {
	binary  string
	args    []string
	allArgs [][]string
	runs    int
}

func (runner *recordingProcess) Run(_ context.Context, binary string, args []string, _ io.Reader, _, _ io.Writer) error {
	copied := append([]string(nil), args...)
	runner.binary, runner.args, runner.allArgs, runner.runs = binary, copied, append(runner.allArgs, copied), runner.runs+1
	return nil
}

type scriptedProcess struct {
	outputs []string
	errs    []error
	allArgs [][]string
}

func (s *scriptedProcess) Run(_ context.Context, _ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	idx := len(s.allArgs)
	s.allArgs = append(s.allArgs, append([]string(nil), args...))
	if idx < len(s.outputs) && s.outputs[idx] != "" {
		io.WriteString(stdout, s.outputs[idx])
	}
	if idx < len(s.errs) && s.errs[idx] != nil {
		return s.errs[idx]
	}
	return nil
}

func TestRunHelpAndExplain(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("help exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "run_ai [run]") {
		t.Fatalf("help output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--claude", "--confirm", "--monitoring", "--explain"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("explain exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"provider": "claude"`) || !strings.Contains(stdout.String(), `"access_mode": "confirm"`) {
		t.Fatalf("explain output = %s", stdout.String())
	}
}

func TestSessionsListOnCleanStateIsReadOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "state", "sessions")
	deps := dependencies{lineage: sessions.FileLineageStore{Directory: directory}}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"sessions", "list"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var document sessionListDocument
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode output: %v: %s", err, stdout.String())
	}
	if document.Schema != "run_ai_sessions/v3" || len(document.Sessions) != 0 {
		t.Fatalf("output=%#v", document)
	}
	if _, err := os.Lstat(filepath.Join(root, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sessions list created state: %v", err)
	}
}

func TestSessionsListIncludesStaleDigestMismatchWithoutCrashing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	persistDir := filepath.Join(root, "persistent")
	if err := os.Mkdir(persistDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write a structurally valid session file whose digest field is wrong.
	// This simulates historic team-persistent sessions written by an older binary
	// that included additional fields (e.g. mode, tmux_name) in the digest input.
	staleID := "persistent-20261019090000-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	staleDir := filepath.Join(persistDir, staleID)
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleJSON := `{
  "schema": "run_ai_persistent_session/v1",
  "id": "persistent-20261019090000-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
  "provider": "codex",
  "working_directory": "/tmp/work",
  "model": "gpt-5.6-terra",
  "model_track": "configured",
  "workflow_digest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "created_at": "1970-01-01T00:00:01Z",
  "updated_at": "1970-01-01T00:00:01Z",
  "resume_capability": "exact_persistent_id",
  "digest": "0000000000000000000000000000000000000000000000000000000000000000"
}`
	if err := os.WriteFile(filepath.Join(staleDir, "session.json"), []byte(staleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := dependencies{
		lineage:    &sessions.MemoryLineageStore{},
		persistent: sessions.PersistentStore{Directory: persistDir},
	}
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"sessions", "list"}, nil, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("sessions list crashed: code=%d stderr=%s", code, stderr.String())
	}
	var document sessionListDocument
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode output: %v: %s", err, stdout.String())
	}
	if len(document.Sessions) != 1 {
		t.Fatalf("expected 1 session entry, got %d: %#v", len(document.Sessions), document.Sessions)
	}
	entry := document.Sessions[0]
	if entry.Type != "stale_digest_mismatch" {
		t.Fatalf("entry type = %q, want %q", entry.Type, "stale_digest_mismatch")
	}
	if entry.SessionID != staleID {
		t.Fatalf("entry session_id = %q, want %q", entry.SessionID, staleID)
	}
	if entry.State != "stale_digest_mismatch" {
		t.Fatalf("entry state = %q, want %q", entry.State, "stale_digest_mismatch")
	}
	if entry.ResumeCapability != "none" {
		t.Fatalf("entry resume_capability = %q, want %q", entry.ResumeCapability, "none")
	}
}

func TestSessionsListStaleEntryHasUnknownProviderAndTeamMode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	persistDir := filepath.Join(root, "persistent")
	if err := os.Mkdir(persistDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleID := "persistent-20261019090001-ffffffffffffffffffffffffffffffff"
	staleDir := filepath.Join(persistDir, staleID)
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleJSON := `{
  "schema": "run_ai_persistent_session/v1",
  "id": "persistent-20261019090001-ffffffffffffffffffffffffffffffff",
  "provider": "codex",
  "working_directory": "/tmp/work",
  "model": "gpt-5.6-terra",
  "model_track": "configured",
  "workflow_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "created_at": "1970-01-01T00:00:02Z",
  "updated_at": "1970-01-01T00:00:02Z",
  "resume_capability": "exact_persistent_id",
  "digest": "0000000000000000000000000000000000000000000000000000000000000000"
}`
	if err := os.WriteFile(filepath.Join(staleDir, "session.json"), []byte(staleJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := dependencies{
		lineage:    &sessions.MemoryLineageStore{},
		persistent: sessions.PersistentStore{Directory: persistDir},
	}
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"sessions", "list"}, nil, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("sessions list crashed: code=%d stderr=%s", code, stderr.String())
	}
	var document sessionListDocument
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode output: %v: %s", err, stdout.String())
	}
	if len(document.Sessions) != 1 {
		t.Fatalf("expected 1 session entry, got %d", len(document.Sessions))
	}
	entry := document.Sessions[0]
	if entry.Provider != "unknown" {
		t.Fatalf("stale entry provider = %q, want %q", entry.Provider, "unknown")
	}
	if entry.TeamMode != "unknown" {
		t.Fatalf("stale entry team_mode = %q, want %q", entry.TeamMode, "unknown")
	}
	if entry.ResumeCapability != "none" {
		t.Fatalf("stale entry resume_capability = %q, want %q", entry.ResumeCapability, "none")
	}
}

func TestSessionsListReportsObservedClassicTmuxStateWithoutDeletingLineage(t *testing.T) {
	t.Parallel()
	base := &fakeTmux{}
	deps := testDependencies(base)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("create code=%d stderr=%s", code, stderr.String())
	}
	lineages, err := deps.lineage.List()
	if err != nil || len(lineages) != 1 {
		t.Fatalf("lineages=%#v err=%v", lineages, err)
	}
	activity := time.Date(2026, 10, 20, 18, 7, 0, 0, time.UTC)
	deps.tmux = &statusFakeTmux{fakeTmux: base, status: sessions.LiveStatus{
		Exists: true, Live: true, LastActivity: activity,
		Info: sessions.RuntimeInfo{SessionID: "$42", SessionName: lineages[0].TmuxName},
	}}
	stdout.Reset()
	if code := runWith(context.Background(), []string{"sessions", "list"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("live list code=%d stderr=%s", code, stderr.String())
	}
	var live sessionListDocument
	if err := json.Unmarshal(stdout.Bytes(), &live); err != nil || len(live.Sessions) != 1 || live.Sessions[0].State != "active" || !live.Sessions[0].SupervisorActive || live.Sessions[0].TmuxSessionID != "$42" || live.Sessions[0].LastActivity != activity.Format(time.RFC3339Nano) {
		t.Fatalf("live inventory=%#v err=%v output=%s", live, err, stdout.String())
	}

	deps.tmux = &statusFakeTmux{fakeTmux: base, status: sessions.LiveStatus{Exists: false}}
	stdout.Reset()
	if code := runWith(context.Background(), []string{"sessions", "list"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("stale list code=%d stderr=%s", code, stderr.String())
	}
	var stale sessionListDocument
	if err := json.Unmarshal(stdout.Bytes(), &stale); err != nil || len(stale.Sessions) != 1 || stale.Sessions[0].State != "stale" || stale.Sessions[0].SupervisorActive {
		t.Fatalf("stale inventory=%#v err=%v output=%s", stale, err, stdout.String())
	}
	remaining, err := deps.lineage.List()
	if err != nil || len(remaining) != 1 || remaining[0].TmuxName != lineages[0].TmuxName {
		t.Fatalf("sessions list mutated durable lineage: %#v err=%v", remaining, err)
	}
}

func TestSessionsTerminateClassicAvoidsPersistentLookupAndRequiresExactLiveManifest(t *testing.T) {
	t.Parallel()
	base := &fakeTmux{}
	deps := testDependencies(base)
	// A classic ID must never be sent through PersistentStore.Lookup. A
	// relative store directory would make that accidental lookup fail.
	deps.persistent = sessions.PersistentStore{Directory: "relative-persistent-store"}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("create code=%d stderr=%s", code, stderr.String())
	}
	lineages, err := deps.lineage.List()
	if err != nil || len(lineages) != 1 || len(base.created) != 1 {
		t.Fatalf("lineages=%#v created=%#v err=%v", lineages, base.created, err)
	}
	runtime := &statusFakeTmux{fakeTmux: base, status: sessions.LiveStatus{
		Exists: true,
		Live:   true,
		Info:   sessions.RuntimeInfo{SessionID: "$42", SessionName: lineages[0].TmuxName, UID: 1000, Manifest: base.created[0].Manifest},
	}}
	deps.tmux = runtime
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "terminate", lineages[0].TmuxName}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("terminate code=%d stderr=%s lineage=%#v runtime=%#v", code, stderr.String(), lineages[0], runtime.status.Info.Manifest)
	}
	if len(runtime.terminated) != 1 || runtime.terminated[0] != "$42" || !strings.Contains(stdout.String(), "metadata retained") {
		t.Fatalf("termination=%#v stdout=%s", runtime.terminated, stdout.String())
	}
	if remaining, err := deps.lineage.List(); err != nil || len(remaining) != 1 || remaining[0].TmuxName != lineages[0].TmuxName {
		t.Fatalf("terminate removed durable lineage: %#v err=%v", remaining, err)
	}

	runtime.terminated = nil
	runtime.status.Info.Manifest.Digest = strings.Repeat("0", 64)
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "terminate", lineages[0].TmuxName}, nil, &stdout, &stderr, deps); code != exitUnavailable || len(runtime.terminated) != 0 {
		t.Fatalf("mismatched live runtime was terminated: code=%d terminated=%#v stderr=%s", code, runtime.terminated, stderr.String())
	}

	runtime.status.Info.Manifest = base.created[0].Manifest
	runtime.status.Info.SessionName = "codex_replacement"
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "terminate", lineages[0].TmuxName}, nil, &stdout, &stderr, deps); code != exitUnavailable || len(runtime.terminated) != 0 {
		t.Fatalf("replacement runtime was terminated: code=%d terminated=%#v stderr=%s", code, runtime.terminated, stderr.String())
	}

	runtime.status.Info.SessionName = lineages[0].TmuxName
	runtime.status.Live = false
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "terminate", lineages[0].TmuxName}, nil, &stdout, &stderr, deps); code != exitUnavailable || len(runtime.terminated) != 0 {
		t.Fatalf("stale runtime was terminated: code=%d terminated=%#v stderr=%s", code, runtime.terminated, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "terminate", "codex_unknown"}, nil, &stdout, &stderr, deps); code != exitUnavailable || len(runtime.terminated) != 0 {
		t.Fatalf("unknown classic ID was terminated: code=%d terminated=%#v stderr=%s", code, runtime.terminated, stderr.String())
	}
}

func TestSessionsTerminatePersistentUsesExactSupervisorID(t *testing.T) {
	id := "persistent-20261021110000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	thread := "01a15365-5be9-7661-a03e-df4519af73ec"
	deps := testDependencies(&fakeTmux{})
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(t.TempDir(), "persistent")}
	socket, err := persistentSocketPath(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socket) })
	value, err := sessions.NewPersistentSession(id, "codex", "/", socket, "gpt-5.6-terra", "configured", strings.Repeat("a", 64), deps.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.persistent.Create(value); err != nil {
		t.Fatal(err)
	}
	value, err = deps.persistent.BindThread(id, thread, deps.now())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	operations := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var command appserver.IPCCommand
			if decodeErr := json.NewDecoder(conn).Decode(&command); decodeErr == nil {
				operations <- command.Op
				_ = json.NewEncoder(conn).Encode(appserver.Reply{OK: true, ThreadID: thread})
			}
			_ = conn.Close()
		}
	}()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"sessions", "terminate", id}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("persistent terminate code=%d stderr=%s", code, stderr.String())
	}
	if first, second := <-operations, <-operations; first != "status" || second != "shutdown" {
		t.Fatalf("persistent operations = %q, %q", first, second)
	}
	_ = listener.Close()
	<-done

	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "terminate", "persistent-20261021110000-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, nil, &stdout, &stderr, deps); code != exitUnavailable {
		t.Fatalf("unknown persistent terminate code=%d stderr=%s", code, stderr.String())
	}
}

func TestPersistentSessionsListSeparatesExecutedTurnsFromQueue(t *testing.T) {
	root := t.TempDir()
	store := sessions.PersistentStore{Directory: filepath.Join(root, "persistent")}
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	value, err := sessions.NewPersistentSession("persistent-20261019100000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "codex", "/", "", "gpt-5.6-terra", "configured", strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(value); err != nil {
		t.Fatal(err)
	}
	value, err = store.BindThread(value.ID, "thread-for-list", now)
	if err != nil {
		t.Fatal(err)
	}
	stateDir, err := store.StateDirectory(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	instructions := []appserver.Instruction{{ID: 1, State: "starting", CreatedAt: now, UpdatedAt: now}}
	for id := uint64(2); id <= 49; id++ {
		instructions = append(instructions, appserver.Instruction{ID: id, State: "queued", CreatedAt: now, UpdatedAt: now})
	}
	if err := (appserver.JournalStore{Path: filepath.Join(stateDir, "app-supervisor-state.json")}).Save(appserver.Journal{Version: 1, ThreadID: value.ThreadID, State: appserver.SupervisorIdle, Instructions: instructions}); err != nil {
		t.Fatal(err)
	}
	deps := dependencies{lineage: &sessions.MemoryLineageStore{}, persistent: store}
	var out bytes.Buffer
	if err := writeUnifiedSessions(&out, deps); err != nil {
		t.Fatal(err)
	}
	var document sessionListDocument
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Sessions) != 1 || document.Sessions[0].Turns != 1 || document.Sessions[0].ExecutedTurns != 1 || document.Sessions[0].QueuedInstructions != 48 {
		t.Fatalf("session summary = %#v", document.Sessions)
	}
}

func TestPersistentLaunchCreatesSealedSessionAndPassesModelAndWorkingDirectory(t *testing.T) {
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(t.TempDir(), "persistent")}
	var gotLaunch appSupervisorLaunch
	var gotSocket, gotThread string
	deps.ensureSupervisor = func(_ context.Context, socket, thread string, launch appSupervisorLaunch) (appserver.Reply, error) {
		gotSocket, gotThread, gotLaunch = socket, thread, launch
		return appserver.Reply{OK: true, ThreadID: "01a15365-5be9-7661-a03e-df4519af73ec", State: appserver.SupervisorIdle}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--persistent", "--new-session", "--model", "gpt-5.6-sol"}, strings.NewReader("/quit\n"), &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if gotThread != "" || gotLaunch.Model != "gpt-5.6-sol" || gotLaunch.CWD != "/" || gotLaunch.StateDirectory == "" || !strings.HasSuffix(gotSocket, ".sock") {
		t.Fatalf("thread=%q launch=%#v socket=%q", gotThread, gotLaunch, gotSocket)
	}
	values, err := deps.persistent.List()
	if err != nil || len(values) != 1 || values[0].ThreadID != "01a15365-5be9-7661-a03e-df4519af73ec" || values[0].Model != "gpt-5.6-sol" {
		t.Fatalf("values=%#v err=%v", values, err)
	}
}

func TestPersistentGenericLaunchUsesCallerWorkingDirectory(t *testing.T) {
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(t.TempDir(), "persistent")}
	deps.workingDirectory = func() (string, error) { return "/tmp/run-ai-isolated-cwd", nil }
	var launch appSupervisorLaunch
	deps.ensureSupervisor = func(_ context.Context, _, _ string, got appSupervisorLaunch) (appserver.Reply, error) {
		launch = got
		return appserver.Reply{OK: true, ThreadID: "01a15365-5be9-7661-a03e-df4519af73ec", State: appserver.SupervisorIdle}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--persistent", "--new-session"}, strings.NewReader("/quit\n"), &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if launch.CWD != "/tmp/run-ai-isolated-cwd" {
		t.Fatalf("persistent CWD=%q", launch.CWD)
	}
}

func TestPersistentResumeRejectsUnknownIDWithoutLaunching(t *testing.T) {
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(t.TempDir(), "persistent")}
	called := false
	deps.ensureSupervisor = func(context.Context, string, string, appSupervisorLaunch) (appserver.Reply, error) {
		called = true
		return appserver.Reply{}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"--codex", "--persistent", "--resume-id", "persistent-missing"}, strings.NewReader("/quit\n"), &stdout, &stderr, deps)
	if code != exitSoftware || called || len(tmux.created) != 0 {
		t.Fatalf("code=%d called=%t tmux=%#v stderr=%s", code, called, tmux, stderr.String())
	}
}

func TestPersistentRemoteDevIsRejectedBeforeLegacyDelegation(t *testing.T) {
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(t.TempDir(), "persistent")}
	runner := &recordingProcess{}
	deps.runProcess = runner
	deps.loadInventory = func(string, int64) ([]routing.DomainRecord, error) {
		return []routing.DomainRecord{{Domain: "app.example.cz", Server: "remote.example.com", DocumentRoots: []string{"/var/www/app"}}}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"--codex", "--persistent", "--dev", "app.example.cz"}, strings.NewReader("/quit\n"), &stdout, &stderr, deps)
	if code != exitSoftware || runner.runs != 0 || len(tmux.created) != 0 {
		t.Fatalf("code=%d runner=%#v tmux=%#v stderr=%s", code, runner, tmux, stderr.String())
	}
}

func TestPersistentResumeRequiresExactSealedWorkflowAndModelTrack(t *testing.T) {
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.persistent = sessions.PersistentStore{Directory: filepath.Join(t.TempDir(), "persistent")}
	stored, err := sessions.NewPersistentSession("persistent-20261019090000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "codex", "/", "", "gpt-5.6-terra", "configured", strings.Repeat("a", 64), deps.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.persistent.Create(stored); err != nil {
		t.Fatal(err)
	}
	called := false
	deps.ensureSupervisor = func(context.Context, string, string, appSupervisorLaunch) (appserver.Reply, error) {
		called = true
		return appserver.Reply{}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"--codex", "--persistent", "--resume-id", stored.ID}, strings.NewReader("/quit\n"), &stdout, &stderr, deps)
	if code != exitSoftware || called || !strings.Contains(stderr.String(), "workflow plan") {
		t.Fatalf("code=%d called=%t stderr=%s", code, called, stderr.String())
	}
}

func TestRunRejectsConflictsAndPlanOnlyWorkflowsWithoutLaunching(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--codex", "--gemini"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("conflict exit = %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	tmux := &fakeTmux{}
	code := runWith(context.Background(), []string{"--monitoring"}, nil, &stdout, &stderr, testDependencies(tmux))
	if code != exitUnavailable || len(tmux.created) != 0 || len(tmux.attached) != 0 {
		t.Fatalf("plan-only code=%d tmux=%#v stderr=%q", code, tmux, stderr.String())
	}
}

func TestConfirmSubprocessFailsBeforeRuntime(t *testing.T) {
	t.Parallel()
	command := exec.Command(os.Args[0], "-test.run=^TestRunAIHelperProcess$", "--", "--confirm")
	command.Env = append(os.Environ(), "RUN_AI_HELPER_PROCESS=1")
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != exitUnavailable {
		t.Fatalf("subprocess err=%v output=%s", err, output)
	}
	if !strings.Contains(string(output), "hard per-action confirmation adapter") {
		t.Fatalf("subprocess output = %s", output)
	}
}

func TestRunAIHelperProcess(t *testing.T) {
	if os.Getenv("RUN_AI_HELPER_PROCESS") != "1" {
		return
	}
	separator := 0
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index + 1
			break
		}
	}
	os.Exit(run(os.Args[separator:], os.Stdout, os.Stderr))
}

func TestGenericLaunchCreatesAndAttachesLegacyTmuxName(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"--claude", "--default"}, nil, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARNING [full_access_unsafe]") {
		t.Fatalf("security warning not presented: %q", stderr.String())
	}
	wantName := "codex_claude_main_testhost_example_com"
	if len(tmux.created) != 1 || tmux.createdFor[0] != wantName || len(tmux.attached) != 1 || tmux.attached[0] != "$42" {
		t.Fatalf("tmux = %#v", tmux)
	}
	command := tmux.created[0]
	if command.Binary != "/test/bin/claude" || command.WorkingDirectory != "/" || !contains(command.Args, "sonnet") || !contains(command.Args, "--session-id") {
		t.Fatalf("provider command = %#v", command)
	}
	if _, err := deps.store.Lookup(context.Background(), providers.Claude, "main_testhost_example_com"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("new session changed canonical Claude mapping: %v", err)
	}
	if got := len(deps.lineage.(*sessions.MemoryLineageStore).Values); got != 1 {
		t.Fatalf("private lineage count = %d, want 1", got)
	}
}

func TestNewSessionNameUsesCryptographicNonce(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.createdFor) != 1 {
		t.Fatalf("created sessions = %#v", tmux.createdFor)
	}
	const prefix = "codex_codex_main_testhost_example_com_new_20261014010203-"
	nonce := strings.TrimPrefix(tmux.createdFor[0], prefix)
	if nonce == tmux.createdFor[0] || len(nonce) != 32 {
		t.Fatalf("session name %q does not contain a 128-bit nonce", tmux.createdFor[0])
	}
}

func TestTeamLaunchUsesIsolatedSessionAndExactRolePrompt(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--team"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.createdFor) != 1 || !strings.Contains(tmux.createdFor[0], "_new_") {
		t.Fatalf("team session is not isolated: %#v", tmux.createdFor)
	}
	if len(tmux.created) != 1 {
		t.Fatalf("created commands = %#v", tmux.created)
	}
	joined := strings.Join(tmux.created[0].Args, "\n")
	for _, required := range []string{
		"root=Turing", "candidate_cutover_packaging=Fermat", "candidate_evidence_broker=Dewey",
		"ikb_candidate_consistency=Faraday", "nepridavaji zadne opravneni",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("provider argv lacks %q: %#v", required, tmux.created[0].Args)
		}
	}
}

func TestNewSessionNeverPromotesLegacyMapping(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	if err := deps.store.Put(context.Background(), sessions.Mapping{Provider: providers.Claude, Key: "main_testhost_example_com", ID: "canonical-old"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude", "--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	mapping, err := deps.store.Lookup(context.Background(), providers.Claude, "main_testhost_example_com")
	if err != nil || mapping.ID != "canonical-old" {
		t.Fatalf("canonical mapping changed: %#v, %v", mapping, err)
	}
	lineages := deps.lineage.(*sessions.MemoryLineageStore).Values
	lineage := lineages[tmux.createdFor[0]]
	if len(lineages) != 1 || lineage.State != "active" || lineage.PromotionState != "unpromoted" || !lineage.ExplicitNew || lineage.ProviderSessionID == "" {
		t.Fatalf("lineage = %#v", lineages)
	}
}

func TestExplicitNewClaudeCanBeListedAndResumedByProviderID(t *testing.T) {
	t.Parallel()
	firstTmux := &fakeTmux{}
	deps := testDependencies(firstTmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude", "--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("create code=%d stderr=%s", code, stderr.String())
	}
	lineages, err := deps.lineage.List()
	if err != nil || len(lineages) != 1 || lineages[0].ProviderSessionID == "" || lineages[0].ResumeCapability != sessions.ResumeExactProviderID {
		t.Fatalf("lineages=%#v err=%v", lineages, err)
	}
	lineage := lineages[0]

	secondTmux := &fakeTmux{}
	deps.tmux = secondTmux
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--claude", "--resume-id", lineage.ProviderSessionID}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("resume code=%d stderr=%s", code, stderr.String())
	}
	if len(secondTmux.createdFor) != 1 || secondTmux.createdFor[0] != lineage.TmuxName || !contains(secondTmux.created[0].Args, "--resume") || !contains(secondTmux.created[0].Args, lineage.ProviderSessionID) {
		t.Fatalf("resumed tmux=%#v lineage=%#v", secondTmux, lineage)
	}

	secondTmux.created = nil
	secondTmux.attached = nil
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"sessions", "list"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("list code=%d stderr=%s", code, stderr.String())
	}
	var listed sessionListDocument
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil || listed.Schema != "run_ai_sessions/v3" || len(listed.Sessions) != 1 || listed.Sessions[0].CodexThreadID != "" || listed.Sessions[0].ResumeCapability != sessions.ResumeExactProviderID || listed.Sessions[0].TmuxName != lineage.TmuxName || listed.Sessions[0].State != "unknown" {
		t.Fatalf("listed=%#v err=%v output=%s", listed, err, stdout.String())
	}
	if len(secondTmux.created) != 0 || len(secondTmux.attached) != 0 {
		t.Fatalf("sessions list touched tmux: %#v", secondTmux)
	}
}

func TestStaleTmuxLifetimeOnlyLineageStartsFreshIsolatedDefaultSession(t *testing.T) {
	t.Parallel()
	firstTmux := &fakeTmux{}
	deps := testDependencies(firstTmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("create code=%d stderr=%s", code, stderr.String())
	}
	lineage := deps.lineage.(*sessions.MemoryLineageStore).Values["codex_codex_main_testhost_example_com"]
	if lineage.ResumeCapability != sessions.ResumeTmuxLifetimeOnly || lineage.ProviderSessionID != "" {
		t.Fatalf("lineage=%#v", lineage)
	}

	missingTmux := &fakeTmux{}
	deps.tmux = missingTmux
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK || len(missingTmux.created) != 1 || !strings.Contains(missingTmux.createdFor[0], "_new_") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--new-session"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("new-session escape code=%d stderr=%s", code, stderr.String())
	}
}

func TestExistingTmuxAttachesWithoutBuildingProviderOrChangingMap(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{exists: true}
	deps := testDependencies(tmux)
	if err := deps.store.Put(context.Background(), sessions.Mapping{Provider: providers.Codex, Key: "main_testhost_example_com", ID: "legacy-session"}); err != nil {
		t.Fatal(err)
	}
	deps.lookupBinary = func(string) (string, error) { t.Fatal("provider lookup must not run"); return "", nil }
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.created) != 0 || len(tmux.attached) != 1 || tmux.attached[0] != "$42" || len(tmux.validated) != 2 || !tmux.validated[0].AllowLegacy || !tmux.validated[1].AllowLegacy {
		t.Fatalf("tmux = %#v", tmux)
	}
}

func TestDevLaunchUsesExactLocalInventoryRootAndLegacyKey(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	deps.hostname = func() (string, error) { return "devhost.example.com", nil }
	deps.loadInventory = func(path string, limit int64) ([]routing.DomainRecord, error) {
		if path != deps.inventoryPath || limit != inventoryLimit {
			t.Fatalf("inventory args = %q %d", path, limit)
		}
		return []routing.DomainRecord{{Domain: "app.example.cz", Server: "devhost.example.com", DocumentRoots: []string{"/home/domains/app.example.cz/web/public"}}}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runWith(context.Background(), []string{"--dev", "app.example.cz"}, nil, &stdout, &stderr, deps)
	if code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.created) != 1 || tmux.createdFor[0] != "codex_codex_dev_app_example_cz" || tmux.created[0].WorkingDirectory != "/home/domains/app.example.cz/web/public" {
		t.Fatalf("tmux = %#v", tmux)
	}
}

func TestRemoteDevUsesFixedFullModeLegacyDelegation(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	runner := &recordingProcess{}
	deps.runProcess = runner
	deps.loadInventory = func(string, int64) ([]routing.DomainRecord, error) {
		return []routing.DomainRecord{{Domain: "app.example.cz", Server: "remote.example.com", DocumentRoots: []string{"/var/www/app"}}}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude", "--latest", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if runner.runs != 3 || runner.binary != "/test/bin/ssh" || len(runner.allArgs[1]) != 3 || runner.allArgs[1][0] != "-t" || runner.allArgs[1][1] != "remote" || !strings.Contains(runner.allArgs[1][2], "'/test/launchers/run_claude' '--latest' 'app.example.cz'") {
		t.Fatalf("remote invocation = %q %#v", runner.binary, runner.allArgs)
	}
}

func TestRemoteConfirmAndReadOnlyFailBeforeExternalExecution(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{}
	deps := testDependencies(tmux)
	runner := &recordingProcess{}
	deps.runProcess = runner
	deps.loadInventory = func(string, int64) ([]routing.DomainRecord, error) {
		return []routing.DomainRecord{{Domain: "app.example.cz", Server: "remote.example.com", DocumentRoots: []string{"/var/www/app"}}}, nil
	}
	for _, args := range [][]string{{"--confirm", "--dev", "app.example.cz"}, {"--readonly", "--dev", "app.example.cz"}} {
		var stdout, stderr bytes.Buffer
		if code := runWith(context.Background(), args, nil, &stdout, &stderr, deps); code == exitOK {
			t.Fatalf("args %v unexpectedly launched", args)
		}
	}
	if runner.runs != 0 || len(tmux.created) != 0 || len(tmux.attached) != 0 {
		t.Fatalf("external process/tmux mutated: runner=%#v tmux=%#v", runner, tmux)
	}
}

func TestRemoteNewSessionUsesNoPromotionProtocol(t *testing.T) {
	t.Parallel()
	for _, selector := range [][]string{{"--new-session"}, {"--team"}} {
		tmux := &fakeTmux{}
		deps := testDependencies(tmux)
		runner := &recordingProcess{}
		deps.runProcess = runner
		deps.loadInventory = func(string, int64) ([]routing.DomainRecord, error) {
			return []routing.DomainRecord{{Domain: "app.example.cz", Server: "remote.example.com", DocumentRoots: []string{"/var/www/app"}}}, nil
		}
		var stdout, stderr bytes.Buffer
		args := append(append([]string(nil), selector...), "--dev", "app.example.cz")
		if code := runWith(context.Background(), args, nil, &stdout, &stderr, deps); code != exitOK {
			t.Fatalf("remote isolated session %v failed: code=%d stderr=%s", selector, code, stderr.String())
		}
		if runner.runs != 1 || runner.binary != "/test/bin/ssh" || len(runner.args) != 3 ||
			runner.args[0] != "-t" || runner.args[1] != "remote" ||
			!strings.Contains(runner.args[2], "'--new-session' '--no-promotion' 'app.example.cz'") || len(tmux.created) != 0 {
			t.Fatalf("remote no-promotion invocation for %v = %q %#v; tmux=%#v", selector, runner.binary, runner.args, tmux)
		}
	}
}

func remoteInventory() func(string, int64) ([]routing.DomainRecord, error) {
	return func(string, int64) ([]routing.DomainRecord, error) {
		return []routing.DomainRecord{{Domain: "app.example.cz", Server: "remote.example.com", DocumentRoots: []string{"/var/www/app"}}}, nil
	}
}

func TestRemoteDevPrintsSessionInfoAndReconnectHint(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	deps.runProcess = &recordingProcess{}
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "run_ai: attaching remote tmux session codex_codex_dev_app_example_cz on remote") {
		t.Errorf("missing session name hint in stderr:\n%s", stderr.String())
	}
	// recordingProcess returns no probe output → state unknown → cautious message
	if !strings.Contains(stderr.String(), "run_ai: remote session ended; session state unknown — to reconnect: run_ai --codex --dev app.example.cz") {
		t.Errorf("missing state-unknown hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteNewSessionOmitsSessionNameHint(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	deps.runProcess = &recordingProcess{}
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--new-session", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "attaching remote tmux session") {
		t.Errorf("session name hint must not appear for --new-session in stderr:\n%s", stderr.String())
	}
	// --new-session gets its own message; must not claim reconnect leads to same session
	if !strings.Contains(stderr.String(), "run_ai: remote session ended; session started fresh (--new-session) — to start another: run_ai --codex --new-session --dev app.example.cz") {
		t.Errorf("missing new-session hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteDevNoReconnectHintOnSSHFailure(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	deps.runProcess = &failingProcess{err: errors.New("ssh exit status 1")}
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code == exitOK {
		t.Fatal("SSH failure unexpectedly succeeded")
	}
	if strings.Contains(stderr.String(), "remote session ended") {
		t.Errorf("reconnect hint must not appear after SSH failure in stderr:\n%s", stderr.String())
	}
}

type failingProcess struct{ err error }

func (f *failingProcess) Run(_ context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
	return f.err
}

func TestRemoteProbeAlivePrintsReconnect(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:alive\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(runner.allArgs) != 3 {
		t.Fatalf("expected 3 SSH calls, got %d", len(runner.allArgs))
	}
	if runner.allArgs[2][0] != "remote" {
		t.Errorf("probe first arg = %q, want remote", runner.allArgs[2][0])
	}
	if strings.Contains(stderr.String(), "resume:") {
		t.Errorf("resume hint must not appear for alive tmux in stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "run_ai: remote session ended; tmux session still running — reconnect: run_ai --codex --dev app.example.cz") {
		t.Errorf("missing confirmed-alive reconnect hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeDeadWithIDPrintsResume(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	// beforeID="" (pre-probe returns nothing), afterID changes → DeadCurrentID (unverified)
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\ntest-session-id-123\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "session ID unverified — to resume: run_ai --codex --resume-id test-session-id-123 --dev app.example.cz") {
		t.Errorf("missing unverified resume hint in stderr:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "reconnect:") || strings.Contains(stderr.String(), "unchanged") {
		t.Errorf("unexpected hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeDeadNoIDPrintsStartFresh(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "resume:") {
		t.Errorf("resume hint must not appear without session ID in stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "run_ai: remote session ended; session ended — reconnect starts fresh: run_ai --codex --dev app.example.cz") {
		t.Errorf("missing dead-no-id hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeSSHFailurePrintsUnknownState(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{errs: []error{nil, nil, errors.New("probe SSH failure")}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "resume:") {
		t.Errorf("resume hint must not appear after probe failure in stderr:\n%s", stderr.String())
	}
	// probe failure → state unknown — must not claim reconnect restores the same session
	if !strings.Contains(stderr.String(), "run_ai: remote session ended; session state unknown — to reconnect: run_ai --codex --dev app.example.cz") {
		t.Errorf("missing state-unknown hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeClaudeUsesClaudeSessionsFile(t *testing.T) {
	t.Setenv("RUN_AI_CODEX_ROOT", "/opt/codex")
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\nclaud-session-456\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(runner.allArgs) != 3 || !strings.Contains(runner.allArgs[2][1], "CLAUDE_SESSIONS.md") {
		t.Errorf("probe must reference CLAUDE_SESSIONS.md, allArgs=%#v", runner.allArgs)
	}
	if !strings.Contains(stderr.String(), "session ID unverified — to resume: run_ai --claude --resume-id claud-session-456 --dev app.example.cz") {
		t.Errorf("missing claude unverified resume hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeAntigravityUsesAntigravitySessionsFile(t *testing.T) {
	t.Setenv("RUN_AI_CODEX_ROOT", "/opt/codex")
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\nantigrav-session-789\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--gemini", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(runner.allArgs) != 3 || !strings.Contains(runner.allArgs[2][1], "ANTIGRAVITY_SESSIONS.md") {
		t.Errorf("probe must reference ANTIGRAVITY_SESSIONS.md, allArgs=%#v", runner.allArgs)
	}
	if !strings.Contains(stderr.String(), "session ID unverified — to resume: run_ai --gemini --resume-id antigrav-session-789 --dev app.example.cz") {
		t.Errorf("missing gemini unverified resume hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeInvalidSessionIDTreatedAsDeadNoID(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\nsession/id/with/slashes\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "resume:") {
		t.Errorf("resume hint must not appear for invalid session ID in stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "run_ai: remote session ended; session ended — reconnect starts fresh: run_ai --codex --dev app.example.cz") {
		t.Errorf("missing dead-no-id hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteNewSessionSkipsProbe(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--new-session", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(runner.allArgs) != 1 {
		t.Errorf("expected 1 SSH call for --new-session (no probe), got %d: %#v", len(runner.allArgs), runner.allArgs)
	}
	if !strings.Contains(stderr.String(), "session started fresh (--new-session) — to start another: run_ai --codex --new-session --dev app.example.cz") {
		t.Errorf("missing new-session hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteResumeIDForwardedToRemoteLauncher(t *testing.T) {
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &recordingProcess{}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--resume-id", "019abc-def-123", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(runner.allArgs) < 2 || !strings.Contains(runner.allArgs[1][2], "'--resume-id' '019abc-def-123'") {
		t.Errorf("--resume-id not forwarded in SSH command: %#v", runner.allArgs)
	}
}

func TestRemoteProbePicksCorrectKeyAmongMultiple(t *testing.T) {
	// Tests that parseRemoteSessionProbe correctly extracts the session ID when ID changed.
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\n019e5a5e-c535-7042-b4cb-197e79c3625d\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "session ID unverified — to resume: run_ai --codex --resume-id 019e5a5e-c535-7042-b4cb-197e79c3625d --dev app.example.cz") {
		t.Errorf("unverified resume hint not found in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeAntigravityDotDbSessionID(t *testing.T) {
	// Antigravity session IDs end in .db — ValidateID must accept the dot.
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", "tmux:dead\n57876a29-e282-4475-8635-946bcdafa09d.db\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--gemini", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "session ID unverified — to resume: run_ai --gemini --resume-id 57876a29-e282-4475-8635-946bcdafa09d.db --dev app.example.cz") {
		t.Errorf("missing antigravity .db unverified resume hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeEmptyOutputTreatedAsUnknown(t *testing.T) {
	// A probe that connects but returns nothing (e.g., tmux not installed on remote)
	// must result in state unknown, not a claimed reconnect.
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"", "", ""}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "session state unknown") {
		t.Errorf("empty probe output must yield state-unknown message in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeDeadStaleIDPrintsUncertainResume(t *testing.T) {
	// Before-SSH and after-SSH IDs match; registry mtime predates sessionStartTime.
	// The ID was not written during this run — identity unverified.
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	// Pre-probe and post-probe return the same ID → unchanged, DeadStaleID.
	runner := &scriptedProcess{outputs: []string{"old-session-id-999", "", "tmux:dead\nold-session-id-999\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ID unchanged, identity unverified") {
		t.Errorf("missing identity-unverified warning in stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--resume-id old-session-id-999") {
		t.Errorf("missing stale session ID hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeRegistryNotUpdatedDuringSession(t *testing.T) {
	// Pre-probe and post-probe return the same ID → DeadStaleID regardless of mtime.
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"prev-session-abc", "", "tmux:dead\nprev-session-abc\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ID unchanged, identity unverified") {
		t.Errorf("missing identity-unverified warning in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeUnchangedIDMtimeIgnored(t *testing.T) {
	// Same ID before and after regardless of mtime → always DeadStaleID (mtime removed in M16).
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"same-id", "", "tmux:dead\nsame-id\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ID unchanged, identity unverified") {
		t.Errorf("unchanged ID must be stale regardless of mtime in stderr:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "session ID unverified — to resume:") {
		t.Errorf("unchanged ID must not produce DeadCurrentID message in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeBeforeAfterIDChangedUnverifiedResume(t *testing.T) {
	// Registry updated with a new ID during the session window — unverified (concurrent runs indistinguishable).
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"old-id-aaa", "", "tmux:dead\nnew-id-bbb\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "session ID unverified — to resume: run_ai --codex --resume-id new-id-bbb --dev app.example.cz") {
		t.Errorf("changed ID must yield unverified resume hint in stderr:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "unchanged") {
		t.Errorf("unexpected 'unchanged' in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeChangedIDConcurrentSameKeyUnverified(t *testing.T) {
	// A concurrent run of the same domain key could have written the new ID.
	// The system cannot attribute the change to this invocation — always unverified.
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	// Simulate: before="session-A" (previous), after="session-B" (may be concurrent).
	runner := &scriptedProcess{outputs: []string{"session-A", "", "tmux:dead\nsession-B\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "session ID unverified") {
		t.Errorf("changed ID must remain unverified (concurrent attribution impossible) in stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--resume-id session-B") {
		t.Errorf("changed ID must still be offered as resume hint in stderr:\n%s", stderr.String())
	}
}

func TestRemoteProbeBeforeAfterIDSameMtimeStale(t *testing.T) {
	// Same ID before and after → DeadStaleID (mtime is ignored in M16).
	t.Parallel()
	deps := testDependencies(&fakeTmux{})
	runner := &scriptedProcess{outputs: []string{"id-xyz", "", "tmux:dead\nid-xyz\n"}}
	deps.runProcess = runner
	deps.loadInventory = remoteInventory()
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--codex", "--dev", "app.example.cz"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ID unchanged, identity unverified") {
		t.Errorf("same-ID must flag unverified identity in stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--resume-id id-xyz") {
		t.Errorf("stale ID must still appear in resume hint in stderr:\n%s", stderr.String())
	}
}

func TestStaleClaudeLineageStartsFreshSessionUnlessResumeIDIsExplicit(t *testing.T) {
	t.Parallel()
	firstTmux := &fakeTmux{}
	deps := testDependencies(firstTmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("first code=%d stderr=%s", code, stderr.String())
	}
	lineage := deps.lineage.(*sessions.MemoryLineageStore).Values["codex_claude_main_testhost_example_com"]
	if lineage.State != "active" || lineage.ProviderSessionID == "" {
		t.Fatalf("first lineage = %#v", lineage)
	}

	secondTmux := &fakeTmux{}
	deps.tmux = secondTmux
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--claude"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("fresh code=%d stderr=%s", code, stderr.String())
	}
	if len(secondTmux.created) != 1 || contains(secondTmux.created[0].Args, "--resume") || !contains(secondTmux.created[0].Args, "--session-id") {
		t.Fatalf("fresh argv = %#v", secondTmux.created)
	}
}

func TestClaudeIntentCrashRetryReusesSameSessionID(t *testing.T) {
	t.Parallel()
	firstTmux := &fakeTmux{createErr: errors.New("injected create crash")}
	deps := testDependencies(firstTmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude"}, nil, &stdout, &stderr, deps); code == exitOK {
		t.Fatal("injected create failure unexpectedly succeeded")
	}
	lineage := deps.lineage.(*sessions.MemoryLineageStore).Values["codex_claude_main_testhost_example_com"]
	if lineage.State != "intent" || lineage.ProviderSessionID == "" {
		t.Fatalf("crash lineage = %#v", lineage)
	}

	secondTmux := &fakeTmux{}
	deps.tmux = secondTmux
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--claude"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("retry code=%d stderr=%s", code, stderr.String())
	}
	if len(secondTmux.created) != 1 || !contains(secondTmux.created[0].Args, "--session-id") || !contains(secondTmux.created[0].Args, lineage.ProviderSessionID) {
		t.Fatalf("retry argv = %#v", secondTmux.created)
	}
}

func TestMarkedExistingSessionRequiresMatchingActiveLineage(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{exists: true, marked: true}
	deps := testDependencies(tmux)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code == exitOK {
		t.Fatal("marked session without lineage unexpectedly attached")
	}
	if len(tmux.attached) != 0 {
		t.Fatalf("attached = %#v", tmux.attached)
	}
}

func TestMarkedExistingSessionWithActiveLineageAttachesByID(t *testing.T) {
	t.Parallel()
	creator := &fakeTmux{}
	deps := testDependencies(creator)
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), []string{"--claude"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("create code=%d stderr=%s", code, stderr.String())
	}
	existing := &fakeTmux{exists: true, marked: true}
	deps.tmux = existing
	deps.lookupBinary = func(string) (string, error) { t.Fatal("provider lookup must not run"); return "", nil }
	stdout.Reset()
	stderr.Reset()
	if code := runWith(context.Background(), []string{"--claude"}, nil, &stdout, &stderr, deps); code != exitOK {
		t.Fatalf("attach code=%d stderr=%s", code, stderr.String())
	}
	if len(existing.attached) != 1 || existing.attached[0] != "$42" {
		t.Fatalf("attached = %#v", existing.attached)
	}
}

func TestWrongTmuxManifestBlocksAttach(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{exists: true, validateErr: errors.New("wrong provider")}
	deps := testDependencies(tmux)
	if err := deps.store.Put(context.Background(), sessions.Mapping{Provider: providers.Codex, Key: "main_testhost_example_com", ID: "mapped"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code == exitOK {
		t.Fatal("mismatched session unexpectedly attached")
	}
	if len(tmux.attached) != 0 || len(tmux.created) != 0 {
		t.Fatalf("tmux was mutated: %#v", tmux)
	}
}

func TestTmuxSessionIDRaceBlocksAttach(t *testing.T) {
	t.Parallel()
	tmux := &fakeTmux{exists: true, sessionIDs: []string{"$42", "$43"}}
	deps := testDependencies(tmux)
	if err := deps.store.Put(context.Background(), sessions.Mapping{Provider: providers.Codex, Key: "main_testhost_example_com", ID: "mapped"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runWith(context.Background(), nil, nil, &stdout, &stderr, deps); code == exitOK || !strings.Contains(stderr.String(), "session ID changed") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(tmux.attached) != 0 {
		t.Fatalf("raced session attached: %#v", tmux.attached)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestTmuxIsActiveReturnsFalseWithoutTmuxInPath verifies that tmuxIsActive
// returns false without panicking when tmux is not on PATH — the macOS case
// where the binary lives under Homebrew prefixes rather than /usr/bin/tmux.
func TestTmuxIsActiveReturnsFalseWithoutTmuxInPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if tmuxIsActive("run-ai-test-nonexistent-session") {
		t.Error("tmuxIsActive returned true with no tmux in PATH")
	}
}

// TestTmuxIsActiveReturnsFalseForNonexistentSession verifies that
// tmuxIsActive reports false for a session that cannot exist — using whatever
// tmux binary is on PATH (or returning false immediately if none is present).
func TestTmuxIsActiveReturnsFalseForNonexistentSession(t *testing.T) {
	if tmuxIsActive("run-ai-test-nonexistent-session-xyzzy-99999") {
		t.Error("tmuxIsActive returned true for a session that cannot exist")
	}
}

// TestGoalWorkerCodexNotInPathFailsGoal verifies that when codex is absent
// from PATH — as on macOS Apple Silicon before /opt/homebrew/bin is added —
// the goal worker marks the goal Failed with a clear checkpoint rather than
// panicking or leaving the goal stuck in Checkpoint.
func TestGoalWorkerCodexNotInPathFailsGoal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("RUN_AI_GOAL_CODEX", "1")
	t.Setenv("RUN_AI_GOAL_WORKER_CYCLES", "1")

	dir := t.TempDir()
	s := goals.Store{Root: dir, RetryDelay: time.Millisecond}
	if err := s.Save(goals.Goal{
		ID: "test-codex-lookup", Text: "test goal", State: goals.Checkpoint, Provider: "codex",
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runGoalWorker(s, &stdout, &stderr)
	if code != exitOK {
		t.Errorf("runGoalWorker exit code = %d, want %d", code, exitOK)
	}
	g, err := s.Load("test-codex-lookup")
	if err != nil {
		t.Fatal(err)
	}
	if g.State != goals.Failed {
		t.Errorf("goal state = %q, want %q", g.State, goals.Failed)
	}
	if g.Checkpoint != "codex_binary_not_found" {
		t.Errorf("goal checkpoint = %q, want %q", g.Checkpoint, "codex_binary_not_found")
	}
}
