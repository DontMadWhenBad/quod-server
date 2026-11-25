// Package pipeline turns natural-language statements into verified Lean
// proofs: split → formalize → check statement → back-translate → (confirm)
// → prove with compiler feedback → explain. Statements are processed in
// order and each proved statement becomes available to the later ones.
package pipeline

import (
	"context"
	"errors"
	"time"

	"mathprover/internal/lean"
)

// Status of one statement.
type Status string

const (
	StatusPending     Status = "pending"
	StatusFormalizing Status = "formalizing"
	StatusProving     Status = "proving"
	StatusProved      Status = "proved"
	StatusFailed      Status = "failed"               // formalized, but no proof found
	StatusUnformal    Status = "formalization_failed" // no Lean statement accepted
	StatusCanceled    Status = "canceled"
)

// StatementResult is everything known about one statement.
type StatementResult struct {
	Index              int             `json:"index"` // 1-based
	Name               string          `json:"name"`  // Lean name, e.g. stmt_1
	Text               string          `json:"text"`  // natural language
	Signature          string          `json:"signature,omitempty"`
	FormalizationNotes string          `json:"formalization_notes,omitempty"`
	BackTranslation    string          `json:"back_translation,omitempty"`
	Status             Status          `json:"status"`
	Proof              string          `json:"proof,omitempty"`     // verified tactic script
	LeanCode           string          `json:"lean_code,omitempty"` // verified theorem + axiom check
	Axioms             string          `json:"axioms,omitempty"`
	Explanation        string          `json:"explanation,omitempty"`
	Attempts           []AttemptRecord `json:"attempts,omitempty"`
	Error              string          `json:"error,omitempty"`
}

// AttemptRecord is one proof attempt.
type AttemptRecord struct {
	Session    int           `json:"session"` // parallel session number, 1-based
	N          int           `json:"n"`       // attempt number within session
	Body       string        `json:"body"`
	Verified   bool          `json:"verified"`
	ErrorKinds []string      `json:"error_kinds,omitempty"`
	Feedback   string        `json:"feedback,omitempty"` // what the model was told
	LLMTime    time.Duration `json:"llm_time"`
	LeanTime   time.Duration `json:"lean_time"`
	CostUSD    float64       `json:"cost_usd"`
}

// Result of a whole job.
type Result struct {
	Statements []*StatementResult `json:"statements"`
	// LeanFile is a standalone file with every proved statement (and the
	// unproved ones commented out), checkable with `lake env lean`.
	LeanFile string        `json:"lean_file"`
	CostUSD  float64       `json:"cost_usd"`
	LLMCalls int           `json:"llm_calls"`
	Duration time.Duration `json:"duration"`
}

// EventType names a progress event.
type EventType string

const (
	EvSplit          EventType = "split"           // Message: count
	EvFormalizing    EventType = "formalizing"     // attempt n
	EvStatementError EventType = "statement_error" // Lean rejected the statement
	EvFormalized     EventType = "formalized"      // signature + back-translation ready
	EvRevising       EventType = "revising"        // user asked for a different formalization
	EvAttempt        EventType = "attempt"         // proof attempt finished (Attempt set)
	EvProved         EventType = "proved"
	EvFailed         EventType = "failed"
	EvExplained      EventType = "explained"
	EvDone           EventType = "done"
	EvError          EventType = "error"
)

// Event reports progress. Statement is a snapshot copy, safe to keep.
type Event struct {
	Time      time.Time        `json:"time"`
	Type      EventType        `json:"type"`
	Stmt      int              `json:"stmt,omitempty"` // 1-based; 0 = job level
	Message   string           `json:"message,omitempty"`
	Attempt   *AttemptRecord   `json:"attempt,omitempty"`
	Statement *StatementResult `json:"statement,omitempty"`
}

// Proposal is a formalization offered to the user for confirmation.
type Proposal struct {
	Index           int    `json:"index"`
	Text            string `json:"text"`
	Name            string `json:"name"`
	Signature       string `json:"signature"`
	Notes           string `json:"notes"`
	BackTranslation string `json:"back_translation"`
}

// Decision is the user's answer to a Proposal. Reject with Correction to
// have it re-formalized; reject without one to skip the statement.
type Decision struct {
	Accept     bool   `json:"accept"`
	Correction string `json:"correction,omitempty"`
}

// ConfirmFunc asks the user to approve a formalization. It may block (e.g.
// waiting for a client); honour ctx.
type ConfirmFunc func(ctx context.Context, p Proposal) (Decision, error)

// Checker runs Lean. PoolChecker is the real one; tests use fakes.
type Checker interface {
	CheckStatement(ctx context.Context, base *lean.Snapshot, s lean.Statement) (*lean.StatementCheck, error)
	CheckProof(ctx context.Context, base *lean.Snapshot, s lean.Statement, body string) (*lean.Outcome, error)
}

// CheckerSource hands out a Checker for one job; release ends it.
type CheckerSource interface {
	JobChecker(ctx context.Context) (c Checker, release func(), err error)
}

// PoolSource gives every job its own lease on fresh pool workers, which
// are discarded when the job ends (see lean.Lease). This is what isolates
// jobs from each other.
type PoolSource struct{ Pool *lean.Pool }

func (s PoolSource) JobChecker(context.Context) (Checker, func(), error) {
	l := s.Pool.Lease()
	return LeaseChecker{Lease: l}, l.Close, nil
}

// StaticSource returns the same Checker for every job (tests, trusted use).
type StaticSource struct{ C Checker }

func (s StaticSource) JobChecker(context.Context) (Checker, func(), error) {
	return s.C, func() {}, nil
}

// LeaseChecker runs checks on a job's leased worker.
type LeaseChecker struct{ Lease *lean.Lease }

func (c LeaseChecker) CheckStatement(ctx context.Context, base *lean.Snapshot, s lean.Statement) (*lean.StatementCheck, error) {
	return withRetry(ctx, c.Lease, func(r *lean.REPL) (*lean.StatementCheck, error) {
		return lean.CheckStatement(ctx, r, base, s)
	})
}

func (c LeaseChecker) CheckProof(ctx context.Context, base *lean.Snapshot, s lean.Statement, body string) (*lean.Outcome, error) {
	return withRetry(ctx, c.Lease, func(r *lean.REPL) (*lean.Outcome, error) {
		return lean.CheckProof(ctx, r, base, s, body)
	})
}

// withRetry runs fn once more on a fresh worker if the current one died
// before or while replaying (e.g. killed by another session's timeout).
func withRetry[T any](ctx context.Context, l *lean.Lease, fn func(*lean.REPL) (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		r, err := l.REPL(ctx)
		if err != nil {
			return zero, err
		}
		out, err := fn(r)
		if err != nil && errors.Is(err, lean.ErrDead) && attempt == 0 && ctx.Err() == nil {
			continue
		}
		return out, err
	}
}
