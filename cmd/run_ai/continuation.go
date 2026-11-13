package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// runContinuation is deliberately independent from app-server supervisor.
// It resumes one explicitly supplied Codex session, one bounded turn at a
// time, and stops on ambiguity, approval, timeout, or any non-terminal error.
func runContinuation(args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("continuation", flag.ContinueOnError)
	f.SetOutput(stderr)
	session := f.String("session-id", "", "explicit Codex session id")
	progress := f.String("progress", "", "progress file (required)")
	logPath := f.String("log", "", "append-only log file (required)")
	max := f.Int("max-turns", 12, "maximum turns (1..12)")
	turnTimeout := f.Duration("turn-timeout", 15*time.Minute, "per-turn timeout")
	total := f.Duration("total-timeout", 3*time.Hour, "overall timeout")
	stop := f.String("stop-file", "", "create this file to stop")
	if f.Parse(args) != nil {
		return exitUsage
	}
	if *session == "" || *progress == "" || *logPath == "" || *max < 1 || *max > 12 || *turnTimeout <= 0 || *total <= 0 {
		fmt.Fprintln(stderr, "continuation: invalid bounded arguments")
		return exitUsage
	}
	lockPath := "/tmp/run-ai-continuation-" + strings.NewReplacer("/", "_", ":", "_").Replace(*session) + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUnavailable
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(stderr, "continuation: session already has a runner")
		return exitUnavailable
	}
	if _, err := os.Stat(*progress); err != nil {
		fmt.Fprintf(stderr, "continuation: progress file: %v\n", err)
		return exitUnavailable
	}
	lf, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUnavailable
	}
	defer lf.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *total)
	defer cancel()
	stopCtx, stopCancel := context.WithCancel(ctx)
	defer stopCancel()
	if *stop != "" {
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if _, e := os.Stat(*stop); e == nil {
						stopCancel()
						return
					}
				case <-stopCtx.Done():
					return
				}
			}
		}()
	}
	for turn := 1; turn <= *max; turn++ {
		if _, err := os.Stat(*stop); err == nil {
			logContinuation(lf, *session, turn, "stopped", "stop-file")
			return exitOK
		}
		b, err := os.ReadFile(*progress)
		if err != nil {
			logContinuation(lf, *session, turn, "blocked", err.Error())
			return exitUnavailable
		}
		prompt := fmt.Sprintf("This is continuation turn %d of at most %d. Continue the implementation task. First read and update the progress file at %s. Work on the next unfinished item, run relevant tests, and do not claim the whole task complete for a partial change. At the end output exactly one marker on its own line: RUN_AI_CONTINUATION_STATE: checkpoint if more work remains, completed only if every requirement and E2E acceptance criterion is complete, or blocked if an approval/ambiguity/external decision is required.\nCurrent progress:\n%s", turn, *max, *progress, string(b))
		if err := stopCtx.Err(); err != nil {
			logContinuation(lf, *session, turn, "blocked", err.Error())
			return exitUnavailable
		}
		turnCtx, cancelTurn := context.WithTimeout(stopCtx, *turnTimeout)
		bin := os.Getenv("RUN_AI_CONTINUATION_CODEX_BIN")
		if bin == "" {
			bin = "/usr/local/bin/codex"
		}
		cmd := exec.CommandContext(turnCtx, bin, "exec", "resume", "--json", "--skip-git-repo-check", *session, prompt)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, runErr := cmd.Output()
		if turnCtx.Err() != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cancelTurn()
		state, detail := parseContinuationState(out)
		if runErr != nil || state == "" || state == "blocked" {
			if runErr != nil {
				detail = runErr.Error()
			}
			if detail == "" {
				detail = "ambiguous_or_missing_marker"
			}
			logContinuation(lf, *session, turn, "blocked", detail)
			return exitUnavailable
		}
		logContinuation(lf, *session, turn, state, detail)
		fmt.Fprintf(stdout, "session=%s turn=%d state=%s\n", *session, turn, state)
		if state == "completed" {
			return exitOK
		}
	}
	logContinuation(lf, *session, *max, "blocked", "max_turns_exceeded")
	return exitUnavailable
}

func parseContinuationState(out []byte) (string, string) {
	state, detail := "", ""
	s := bufio.NewScanner(strings.NewReader(string(out)))
	for s.Scan() {
		line := s.Text()
		if i := strings.Index(line, "RUN_AI_CONTINUATION_STATE:"); i >= 0 {
			v := strings.TrimSpace(strings.TrimPrefix(line[i:], "RUN_AI_CONTINUATION_STATE:"))
			if v == "completed" || v == "checkpoint" || v == "blocked" {
				state = v
				detail = line
			}
		}
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Item.Text != "" {
			if strings.Contains(ev.Item.Text, "RUN_AI_CONTINUATION_STATE:") {
				return parseContinuationState([]byte(ev.Item.Text))
			}
		}
	}
	return state, detail
}
func logContinuation(w io.Writer, session string, turn int, state, detail string) {
	fmt.Fprintf(w, "%s session=%s turn=%d state=%s detail=%q\n", time.Now().UTC().Format(time.RFC3339), session, turn, state, detail)
}
