package goals

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointSurvivesRestartAndTerminalStates(t *testing.T) {
	d := t.TempDir()
	s := Store{Root: filepath.Join(d, "goals"), MaxAttempts: 2}
	g := Goal{ID: "g1", Text: "continue", State: Checkpoint, Checkpoint: "step-1"}
	if e := s.Save(g); e != nil {
		t.Fatal(e)
	}
	s2 := Store{Root: s.Root, MaxAttempts: 2}
	got, e := s2.Load("g1")
	if e != nil || got.Checkpoint != "step-1" {
		t.Fatalf("%+v %v", got, e)
	}
	got.State = Completed
	got.NextRunAt = time.Now().UTC()
	if e = s2.Save(got); e != nil {
		t.Fatal(e)
	}
	again, e := s2.Claim("g1")
	if e != nil || again.State != Completed {
		t.Fatalf("terminal claim: %+v %v", again, e)
	}
}
func TestLockAndRetryLimit(t *testing.T) {
	s := Store{Root: t.TempDir(), MaxAttempts: 1}
	g := Goal{ID: "g", State: Running}
	if e := s.Save(g); e != nil {
		t.Fatal(e)
	}
	l, e := s.Lock()
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	if _, e = s.Lock(); e == nil {
		t.Fatal("second worker acquired lock")
	}
	g, e = s.Claim("g")
	if e != nil || g.Attempts != 1 {
		t.Fatalf("%+v %v", g, e)
	}
	g, e = s.Claim("g")
	if e != nil || g.State != Blocked {
		t.Fatalf("retry limit: %+v %v", g, e)
	}
}
