package sessions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type recordedRun struct {
	binary string
	args   []string
}

type recordingRunner struct {
	runs   []recordedRun
	err    error
	output string
}

func (runner *recordingRunner) Run(_ context.Context, binary string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	runner.runs = append(runner.runs, recordedRun{binary: binary, args: append([]string(nil), args...)})
	if stdout != nil && runner.output != "" {
		_, _ = io.WriteString(stdout, runner.output)
	}
	return runner.err
}

func testManifest() Manifest {
	return Manifest{
		Digest: strings.Repeat("a", 64), Provider: "codex", AccessMode: "full", Workflow: "none",
		TargetDigest: strings.Repeat("b", 64),
	}
}

func TestTmuxCreateUsesOneSafelyQuotedShellCommand(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	client := TmuxClient{Binary: "/usr/bin/tmux", Runner: runner}
	err := client.CreateDetached(context.Background(), "codex_codex_hosting_api_main", Command{
		Binary: "/usr/local/bin/codex", WorkingDirectory: "/tmp/path with spaces",
		Args:     []string{"--model", "x; touch /tmp/no", "apostrophe's argument", "$(false)"},
		Manifest: testManifest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"new-session", "-d", "-s", "codex_codex_hosting_api_main", "-c", "/tmp/path with spaces"}
	if len(runner.runs) != 1 || !reflect.DeepEqual(runner.runs[0].args[:6], wantPrefix) {
		t.Fatalf("runs = %#v", runner.runs)
	}
	wantCommand := "'/usr/local/bin/codex' '--model' 'x; touch /tmp/no' 'apostrophe'\"'\"'s argument' '$(false)'"
	if runner.runs[0].args[6] != wantCommand {
		t.Fatalf("shell command = %q, want %q", runner.runs[0].args[6], wantCommand)
	}
	for index := 7; index < len(runner.runs[0].args); index += 6 {
		if got := runner.runs[0].args[index : index+6]; got[0] != ";" || got[1] != "set-option" || got[2] != "-t" || got[3] != "=codex_codex_hosting_api_main:" {
			t.Fatalf("session option command = %#v", got)
		}
	}
}

func TestTmuxCreateMarksSessionOnRealTmux(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed in the test environment")
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TERM", "xterm-256color")
	t.Cleanup(func() {
		_ = exec.Command(tmux, "kill-server").Run()
	})

	manifest := testManifest()
	client := TmuxClient{Binary: tmux}
	if err := client.CreateDetached(context.Background(), "run_ai_tmux_35_probe", Command{
		Binary: "/bin/sleep", Args: []string{"30"}, WorkingDirectory: "/", Manifest: manifest,
	}); err != nil {
		t.Fatalf("create marked session: %v", err)
	}
	info, err := client.Inspect(context.Background(), "run_ai_tmux_35_probe")
	if err != nil {
		t.Fatalf("inspect marked session: %v", err)
	}
	if info.Manifest != manifest {
		t.Fatalf("manifest = %#v, want %#v", info.Manifest, manifest)
	}
	if info.LastActivity.IsZero() {
		t.Fatal("inspect did not return tmux activity time")
	}
	status, err := client.Status(context.Background(), "run_ai_tmux_35_probe")
	if err != nil {
		t.Fatalf("status marked session: %v", err)
	}
	if !status.Exists || !status.Live || status.Info.SessionID != info.SessionID || status.LastActivity.IsZero() {
		t.Fatalf("status = %#v, inspect = %#v", status, info)
	}
	if output, err := exec.Command(tmux, "kill-session", "-t", "=run_ai_tmux_35_probe").CombinedOutput(); err != nil {
		t.Fatalf("kill session for stale status check: %v: %s", err, output)
	}
	status, err = client.Status(context.Background(), "run_ai_tmux_35_probe")
	if err != nil || status.Exists || status.Live {
		t.Fatalf("stale status = %#v, err=%v", status, err)
	}
}

// TestTmuxE2EPreservesDirectInteractiveCodexProcessAcrossAttachDetach is a
// real tmux-server regression test.  The executable is a deliberately tiny
// stand-in for Codex: it has the same process identity (`codex`), accepts
// terminal input, and remains alive after the client detaches.  This tests the
// launcher boundary, rather than trying to reimplement or scrape Codex's TUI.
// A real Codex binary is intentionally not used: that would require account
// credentials and a network turn in the ordinary unit-test suite.
func TestTmuxE2EPreservesDirectInteractiveCodexProcessAcrossAttachDetach(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed in the test environment")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not installed for PTY-backed tmux attach")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed for the Codex-compatible process stub")
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TERM", "xterm-256color")
	t.Cleanup(func() { _ = exec.Command(tmux, "kill-server").Run() })

	binDir := t.TempDir()
	codex := filepath.Join(binDir, "codex")
	node = "'" + strings.ReplaceAll(node, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexec "+node+" -e 'process.stdout.write(\"codex-original-tui-stub ready\\\\n\"); process.stdin.setRawMode(true); process.stdin.resume(); process.stdin.on(\"data\", d => process.stdout.write(\"codex-input:\" + d.toString())); setInterval(() => {}, 1000)'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	client := TmuxClient{Binary: tmux}
	const name = "run_ai_codex_tui_e2e"
	if err := client.CreateDetached(context.Background(), name, Command{
		Binary: codex, Args: []string{"--no-alt-screen", "prompt"}, WorkingDirectory: "/", Manifest: manifest,
	}); err != nil {
		t.Fatalf("create direct Codex TUI session: %v", err)
	}
	info, err := client.Validate(context.Background(), name, Expectation{Manifest: manifest, UID: os.Getuid()})
	if err != nil {
		t.Fatalf("validate direct Codex TUI session: %v", err)
	}
	if info.Current != "node" {
		t.Fatalf("pane current command = %q, want Codex-compatible node process", info.Current)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		output, captureErr := exec.Command(tmux, "capture-pane", "-p", "-t", "="+name+":0.0").CombinedOutput()
		if captureErr == nil && strings.Contains(string(output), "codex-original-tui-stub ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("direct Codex TUI stub did not become ready: %v: %s", captureErr, output)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Attach through a real PTY client, then detach it from a second tmux
	// control command.  The server must retain the exact immutable session ID.
	attachCtx, cancelAttach := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAttach()
	attach := exec.CommandContext(attachCtx, script, "-qefc", tmux+" attach-session -t ="+name, "/dev/null")
	// The CI PTY created by script may inherit TERM=dumb. Codex itself uses a
	// rich terminal, and tmux's normal attach path requires clear support.
	attach.Env = append(os.Environ(), "TERM=xterm")
	var attachOutput bytes.Buffer
	attach.Stdout, attach.Stderr = &attachOutput, &attachOutput
	if err := attach.Start(); err != nil {
		t.Fatalf("start PTY attach: %v", err)
	}
	defer func() {
		if attach.ProcessState == nil {
			_ = attach.Process.Kill()
			_ = attach.Wait()
		}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for {
		listed := exec.Command(tmux, "list-clients", "-t", "="+name, "-F", "#{client_pid}")
		if output, listErr := listed.Output(); listErr == nil && strings.TrimSpace(string(output)) != "" {
			break
		}
		if time.Now().After(deadline) {
			if err := attach.Wait(); err != nil {
				t.Fatalf("PTY-backed tmux client did not attach: %v: %s", err, attachOutput.String())
			}
			t.Fatalf("PTY-backed tmux client exited without attaching: %s", attachOutput.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if output, err := exec.Command(tmux, "detach-client", "-s", "="+name).CombinedOutput(); err != nil {
		t.Fatalf("detach attached tmux client: %v: %s", err, output)
	}
	if err := attach.Wait(); err != nil {
		t.Fatalf("PTY attach did not exit cleanly after detach: %v", err)
	}

	if err := exec.Command(tmux, "send-keys", "-t", "="+name+":0.0", "reconnect-proof").Run(); err != nil {
		t.Fatalf("send input after detach: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		var pane bytes.Buffer
		capture := exec.Command(tmux, "capture-pane", "-p", "-t", "="+name+":0.0")
		output, err := capture.CombinedOutput()
		if err != nil {
			t.Fatalf("capture direct Codex pane after detach: %v: %s", err, output)
		}
		pane.Write(output)
		if strings.Contains(pane.String(), "codex-input:reconnect-proof") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("direct Codex process did not receive input after detach; pane=%q", pane.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	after, err := client.Validate(context.Background(), name, Expectation{Manifest: manifest, UID: os.Getuid()})
	if err != nil {
		t.Fatalf("session did not survive detach: %v", err)
	}
	if after.SessionID != info.SessionID {
		t.Fatalf("tmux session identity changed across detach: got %s, want %s", after.SessionID, info.SessionID)
	}
}

func TestTmuxValidateRequiresExactManifestUIDAndProviderCommand(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	line := "$42\tsafe\t1000\t0\t1760918832\tcodex\t/usr/local/bin/codex\t1\t1\t" + manifest.Digest + "\t" + manifest.Provider + "\t" + manifest.AccessMode + "\t" + manifest.Workflow + "\t" + manifest.TargetDigest + "\n"
	runner := &recordingRunner{output: line}
	client := TmuxClient{Runner: runner}
	info, err := client.Validate(context.Background(), "safe", Expectation{Manifest: manifest, UID: 1000})
	if err != nil || info.SessionID != "$42" {
		t.Fatal(err)
	}

	wrong := manifest
	wrong.Provider = "claude"
	if _, err := client.Validate(context.Background(), "safe", Expectation{Manifest: wrong, UID: 1000}); err == nil {
		t.Fatal("wrong-provider manifest unexpectedly accepted")
	}
}

func TestTmuxValidateLegacyNeedsExplicitMarkerAndCompatibleCommand(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	runner := &recordingRunner{output: strings.Join([]string{"$42", "safe", "1000", "0", "1760918832", "bash", "/opt/codex/run_codex --provider claude", "1", "1", "", "", "", "", ""}, "\t") + "\n"}
	client := TmuxClient{Runner: runner}
	if _, err := client.Validate(context.Background(), "safe", Expectation{Manifest: manifest, UID: 1000}); err == nil {
		t.Fatal("unmarked session without compatibility marker accepted")
	}
	if _, err := client.Validate(context.Background(), "safe", Expectation{Manifest: manifest, UID: 1000, AllowLegacy: true}); err == nil {
		t.Fatal("wrong-provider legacy command accepted")
	}
	manifest.Provider = "claude"
	if _, err := client.Validate(context.Background(), "safe", Expectation{Manifest: manifest, UID: 1000, AllowLegacy: true}); err != nil {
		t.Fatalf("explicit compatible legacy session rejected: %v", err)
	}
}

func TestTmuxInspectAllowsUnavailableActivityForLegacyCompatibility(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	runner := &recordingRunner{output: "$42\tsafe\t1000\t0\t\tcodex\t/usr/local/bin/codex\t1\t1\t" + manifest.Digest + "\t" + manifest.Provider + "\t" + manifest.AccessMode + "\t" + manifest.Workflow + "\t" + manifest.TargetDigest + "\n"}
	info, err := (TmuxClient{Runner: runner}).Inspect(context.Background(), "safe")
	if err != nil {
		t.Fatal(err)
	}
	if !info.LastActivity.IsZero() {
		t.Fatalf("legacy unavailable activity = %s", info.LastActivity)
	}
}

func TestTmuxStatusTreatsDeadPaneAsNotLive(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	runner := &recordingRunner{output: "$42\tsafe\t1000\t1\t1760918832\tcodex\t/usr/local/bin/codex\t1\t1\t" + manifest.Digest + "\t" + manifest.Provider + "\t" + manifest.AccessMode + "\t" + manifest.Workflow + "\t" + manifest.TargetDigest + "\n"}
	status, err := (TmuxClient{Runner: runner}).Status(context.Background(), "safe")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Exists || status.Live || !status.Info.PaneDead || status.LastActivity.IsZero() {
		t.Fatalf("status=%#v", status)
	}
}

func TestAttachUsesOnlyImmutableSessionID(t *testing.T) {
	t.Setenv("TMUX", "")
	runner := &recordingRunner{}
	client := TmuxClient{Runner: runner}
	if err := client.Attach(context.Background(), "$42", nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 1 || !reflect.DeepEqual(runner.runs[0].args, []string{"attach-session", "-t", "$42"}) {
		t.Fatalf("attach argv = %#v", runner.runs)
	}
	if err := client.Attach(context.Background(), "safe-name", nil, io.Discard, io.Discard); err == nil {
		t.Fatal("mutable name unexpectedly accepted as attach identity")
	}
}

func TestTmuxMethodsRejectUnsafeNamesAndDoNotRun(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	client := TmuxClient{Runner: runner}
	if _, err := client.Exists(context.Background(), "name;kill-server"); err == nil {
		t.Fatal("unsafe name unexpectedly accepted")
	}
	if err := client.CreateDetached(context.Background(), "bad name", Command{Binary: "codex", WorkingDirectory: "/"}); err == nil {
		t.Fatal("unsafe name unexpectedly accepted")
	}
	if len(runner.runs) != 0 {
		t.Fatalf("runner invoked: %#v", runner.runs)
	}
}

func TestTmuxErrorsAreFailClosed(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{err: errors.New("runner failed")}
	client := TmuxClient{Runner: runner}
	if _, err := client.Exists(context.Background(), "safe"); err == nil {
		t.Fatal("unexpected success")
	}
}
