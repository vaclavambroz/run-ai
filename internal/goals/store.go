package goals

// Durable goal state for the optional run_ai supervisor.  This package never
// interprets provider output as completion: only an explicit terminal state
// (completed/blocked/failed) closes a goal.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type State string

const (
	Running    State = "running"
	Checkpoint State = "checkpoint"
	Completed  State = "completed"
	Blocked    State = "blocked"
	Failed     State = "failed"
)

type Goal struct {
	ID          string    `json:"id"`
	Text        string    `json:"text"`
	Provider    string    `json:"provider,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	TmuxName    string    `json:"tmux_name,omitempty"`
	State       State     `json:"state"`
	Checkpoint  string    `json:"checkpoint,omitempty"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	NextRunAt   time.Time `json:"next_run_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	Digest      string    `json:"digest"`
}

type Store struct {
	Root        string
	MaxAttempts int
	RetryDelay  time.Duration
}

func (s Store) init() error {
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = 8
	}
	if s.RetryDelay <= 0 {
		s.RetryDelay = time.Minute
	}
	return os.MkdirAll(s.Root, 0700)
}
func (s Store) path(id string) string { return filepath.Join(s.Root, id+".json") }
func digest(g Goal) string {
	g.Digest = ""
	b, _ := json.Marshal(g)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func (s Store) Save(g Goal) error {
	if err := s.init(); err != nil {
		return err
	}
	if g.ID == "" || filepath.Base(g.ID) != g.ID {
		return errors.New("invalid goal id")
	}
	if g.MaxAttempts <= 0 {
		g.MaxAttempts = s.MaxAttempts
	}
	g.UpdatedAt = time.Now().UTC()
	g.Digest = digest(g)
	b, _ := json.MarshalIndent(g, "", "  ")
	tmp := s.path(g.ID) + fmt.Sprintf(".tmp.%d", os.Getpid())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(g.ID)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
func (s Store) Load(id string) (Goal, error) {
	b, e := os.ReadFile(s.path(id))
	if e != nil {
		return Goal{}, e
	}
	var g Goal
	if e = json.Unmarshal(b, &g); e != nil {
		return Goal{}, e
	}
	if g.Digest != digest(g) {
		return Goal{}, errors.New("goal digest mismatch")
	}
	return g, nil
}
func (s Store) Lock() (*os.File, error) {
	if e := s.init(); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(s.Root, ".worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("goal worker already running: %w", e)
	}
	return f, nil
}
func (s Store) List() ([]Goal, error) {
	if e := s.init(); e != nil {
		return nil, e
	}
	es, e := os.ReadDir(s.Root)
	if e != nil {
		return nil, e
	}
	out := []Goal{}
	for _, x := range es {
		if filepath.Ext(x.Name()) != ".json" {
			continue
		}
		g, e := s.Load(x.Name()[:len(x.Name())-5])
		if e != nil {
			return nil, e
		}
		out = append(out, g)
	}
	return out, nil
}
func (s Store) Claim(id string) (Goal, error) {
	g, e := s.Load(id)
	if e != nil {
		return Goal{}, e
	}
	if g.State == Completed || g.State == Blocked || g.State == Failed {
		return g, nil
	}
	if g.Attempts >= g.MaxAttempts {
		g.State = Blocked
		g.Checkpoint = "retry_limit_exceeded"
		return g, s.Save(g)
	}
	g.State = Running
	g.Attempts++
	return g, s.Save(g)
}
