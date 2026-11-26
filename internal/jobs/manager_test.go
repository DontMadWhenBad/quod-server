package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"mathprover/internal/llm"
	"mathprover/internal/pipeline"
)

// failingChat makes every job fail fast after spending cost.
type failingChat struct{ cost float64 }

func (f failingChat) Chat(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: "not json", CostUSD: f.cost}, nil
}

// noLean is never reached: failingChat makes formalization fail first.
var noLean = pipeline.StaticSource{}

func waitTerminal(t *testing.T, j *Job) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !j.State().Terminal() {
		if time.Now().After(deadline) {
			t.Fatalf("job %s stuck in %s", j.ID, j.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDailyBudget(t *testing.T) {
	// Each job: split (bad json → fallback) + formalize (no theorem) x3 → 4 calls at $0.30.
	m := NewManager(Deps{Chatter: failingChat{cost: 0.30}, Checkers: noLean, Models: llm.DefaultModels()},
		Config{DailyBudgetUSD: 1.0, MaxBudgetUSD: 5, DefaultBudgetUSD: 5})
	defer m.Shutdown(context.Background())

	j1, err := m.Submit("x", Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j1.BudgetUSD != 1.0 {
		t.Errorf("job budget should be capped by daily remainder: %g", j1.BudgetUSD)
	}
	waitTerminal(t, j1)
	if s := m.Stats(); s.DailySpentUSD < 1.0 {
		t.Fatalf("daily spent = %g", s.DailySpentUSD)
	}
	if _, err := m.Submit("y", Options{}, nil); !errors.Is(err, ErrDailyBudget) {
		t.Fatalf("err = %v", err)
	}
}

func TestPrune(t *testing.T) {
	m := NewManager(Deps{Chatter: failingChat{}, Checkers: noLean, Models: llm.DefaultModels()}, Config{MaxJobs: 2, Retention: time.Hour})
	defer m.Shutdown(context.Background())
	var js []*Job
	for range 4 {
		j, err := m.Submit("x", Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		waitTerminal(t, j)
		js = append(js, j)
		time.Sleep(2 * time.Millisecond) // distinct finish times
	}
	m.prune(time.Now())
	if n := len(m.List("")); n != 2 {
		t.Fatalf("kept %d jobs, want 2", n)
	}
	if _, err := m.Get(js[0].ID); !errors.Is(err, ErrNotFound) {
		t.Error("oldest job should be pruned first")
	}
	m.prune(time.Now().Add(2 * time.Hour))
	if n := len(m.List("")); n != 0 {
		t.Errorf("expired jobs kept: %d", n)
	}
}

func TestShutdownRejectsNewJobs(t *testing.T) {
	m := NewManager(Deps{Chatter: failingChat{}, Checkers: noLean, Models: llm.DefaultModels()}, Config{})
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit("x", Options{}, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
}
